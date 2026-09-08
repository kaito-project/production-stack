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
// workload namespace. The listing is assembled from the KAITO InferenceSets in
// the caller's own namespace, so the `id` clients discover is exactly the value
// they send in the `model` field of an inference request.
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

// Model is one entry of an OpenAI-compatible model listing. ID is the
// InferenceSet name, which is also the `X-Gateway-Model-Name` value the
// per-deployment HTTPRoute matches — discovery and invocation therefore agree.
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
	for i := range sets.Items {
		models = append(models, modelFor(&sets.Items[i]))
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

// Get returns the model named id in namespace. The bool reports whether it
// exists; a lookup scoped to a different namespace can never observe it.
func (l *Lister) Get(ctx context.Context, namespace, id string) (Model, bool, error) {
	set := &unstructured.Unstructured{}
	set.SetGroupVersionKind(inferenceSetGVK)
	err := l.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: id}, set)
	if apierrors.IsNotFound(err) {
		return Model{}, false, nil
	}
	if err != nil {
		return Model{}, false, fmt.Errorf("get InferenceSet %s/%s: %w", namespace, id, err)
	}
	return modelFor(set), true, nil
}

// modelFor projects an InferenceSet onto the OpenAI model shape. Created is the
// InferenceSet's creation time, which — unlike vLLM's process start time —
// stays stable across restarts.
func modelFor(set *unstructured.Unstructured) Model {
	m := Model{
		ID:      set.GetName(),
		Object:  objectModel,
		OwnedBy: ownedBy,
	}
	if created := set.GetCreationTimestamp(); !created.IsZero() {
		m.Created = created.Unix()
	}
	return m
}
