/*
Copyright 2026 The KAITO Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package modelsapi serves the OpenAI-compatible model discovery surface
// (`GET /v1/models` and `GET /v1/models/{id}`) for every modelharness-managed
// workload namespace. The listing is assembled from the KAITO InferenceSets
// and their LoRA adapter aliases in the caller's own namespace, so every `id`
// clients discover is a value they can send in the `model` field.
//
// The endpoint is a REGISTRY, not a health check: every InferenceSet in the
// namespace is listed regardless of whether its pods are currently serving.
// Runtime health is surfaced separately (inference requests return
// model_unavailable, and the reporter emits control-plane Events), so filtering
// on readiness here would only make discovery disagree with invocation.
package modelsapi

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
)

// KAITO types are not vendored, so they are read as unstructured. The version
// matches the one the rest of the reporter already consumes.
var (
	inferenceSetGVK     = schema.GroupVersionKind{Group: "kaito.sh", Version: "v1beta1", Kind: "InferenceSet"}
	inferenceSetListGVK = schema.GroupVersionKind{Group: "kaito.sh", Version: "v1beta1", Kind: "InferenceSetList"}
)

// OpenAI response constants. ownedBy deliberately names the platform rather
// than the serving engine: this is an aggregated view over a namespace, not a
// single model server's self-description.
const (
	objectList  = "list"
	objectModel = "model"
	ownedBy     = "kaito"
)

// Model is one entry of an OpenAI-compatible model listing. ID is either the
// InferenceSet name or one of its LoRA adapter source names. BBR maps adapter
// IDs to the owning InferenceSet before the per-deployment HTTPRoute matches.
//
// The field set is exactly the one OpenAI's own models API returns. vLLM's
// extra fields (root/parent/permission/max_model_len) are deliberately omitted:
// OpenAI dropped the first three years ago, and max_model_len is not derivable
// from any Kubernetes object the reporter watches.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the OpenAI-compatible `GET /v1/models` response envelope.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Lister answers model-discovery queries from a read-only cache.
type Lister struct {
	reader client.Reader
}

// NewLister builds a Lister over the given reader, which is expected to be an
// informer-backed cache so request handling never hits the API server.
func NewLister(reader client.Reader) *Lister {
	return &Lister{reader: reader}
}

// NamespaceManaged reports whether name is a workload namespace provisioned by
// charts/modelharness. Namespaces outside that set are not served, so the
// endpoint cannot be used to enumerate unrelated parts of the cluster.
func (l *Lister) NamespaceManaged(ctx context.Context, name string) (bool, error) {
	ns := &corev1.Namespace{}
	err := l.reader.Get(ctx, client.ObjectKey{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get namespace %s: %w", name, err)
	}
	return ns.Labels[util.ManagedByLabel] == util.ManagedByValue, nil
}

// List returns every model registered in namespace, ordered by ID so the
// response is stable across calls regardless of cache iteration order.
func (l *Lister) List(ctx context.Context, namespace string) ([]Model, error) {
	sets := &unstructured.UnstructuredList{}
	sets.SetGroupVersionKind(inferenceSetListGVK)
	if err := l.reader.List(ctx, sets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list InferenceSets in %s: %w", namespace, err)
	}

	models := make([]Model, 0, len(sets.Items))
	// LoRA adapter names are first-class OpenAI model IDs even though they
	// share an InferenceSet with their base model. BBR must resolve each ID to
	// exactly one base pool, so fail the registry closed when two InferenceSets
	// claim the same ID rather than publishing an ambiguous invocation target.
	seen := make(map[string]string)
	for i := range sets.Items {
		setModels, err := modelsFor(&sets.Items[i])
		if err != nil {
			return nil, err
		}
		for _, model := range setModels {
			if owner, exists := seen[model.ID]; exists {
				return nil, fmt.Errorf("model id %q is declared by both InferenceSet %s and %s",
					model.ID, owner, sets.Items[i].GetName())
			}
			seen[model.ID] = sets.Items[i].GetName()
			models = append(models, model)
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

// Get returns the base model or LoRA adapter alias named id in namespace. The
// bool reports whether it exists; a lookup scoped to a different namespace can
// never observe it.
func (l *Lister) Get(ctx context.Context, namespace, id string) (Model, bool, error) {
	models, err := l.List(ctx, namespace)
	if err != nil {
		return Model{}, false, err
	}
	i := sort.Search(len(models), func(i int) bool { return models[i].ID >= id })
	if i < len(models) && models[i].ID == id {
		return models[i], true, nil
	}
	return Model{}, false, nil
}

// modelsFor projects an InferenceSet and each of its LoRA adapter aliases onto
// the OpenAI model shape. Created is the InferenceSet's creation time, which —
// unlike vLLM's process start time — stays stable across restarts.
func modelsFor(set *unstructured.Unstructured) ([]Model, error) {
	base := Model{
		ID:      set.GetName(),
		Object:  objectModel,
		OwnedBy: ownedBy,
	}
	if created := set.GetCreationTimestamp(); !created.IsZero() {
		base.Created = created.Unix()
	}

	models := []Model{base}
	// Clients invoke a LoRA by its source.name, not by the owning InferenceSet
	// name, so discovery must project every configured adapter as a separate
	// model entry. Read the unstructured KAITO object defensively and reject a
	// malformed adapter rather than silently advertising an incomplete registry
	// that disagrees with BBR and vLLM routing.
	adapters, found, err := unstructured.NestedSlice(set.Object, "spec", "template", "inference", "adapters")
	if err != nil {
		return nil, fmt.Errorf("read adapters from InferenceSet %s/%s: %w", set.GetNamespace(), set.GetName(), err)
	}
	if !found {
		return models, nil
	}
	for i := range adapters {
		adapter, ok := adapters[i].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("InferenceSet %s/%s adapter %d is not an object",
				set.GetNamespace(), set.GetName(), i)
		}
		name, found, err := unstructured.NestedString(adapter, "source", "name")
		if err != nil {
			return nil, fmt.Errorf("read adapter %d name from InferenceSet %s/%s: %w",
				i, set.GetNamespace(), set.GetName(), err)
		}
		if !found || name == "" {
			return nil, fmt.Errorf("InferenceSet %s/%s adapter %d has no source.name",
				set.GetNamespace(), set.GetName(), i)
		}
		alias := base
		alias.ID = name
		models = append(models, alias)
	}
	return models, nil
}
