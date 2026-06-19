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

// Package util holds the read-only Kubernetes lookups shared by the
// sub-evaluators: the managed-namespace discovery and the InferenceSet
// GroupVersionResource.
package util

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

const (
	// ManagedByLabel is the discovery label stamped by charts/modelharness on
	// every managed workload Namespace. Evaluators select on it to find the
	// namespaces to watch — no static namespace list (§3).
	ManagedByLabel = "productionstack.kaito.sh/managed-by"
	// ManagedByValue is the expected value of ManagedByLabel.
	ManagedByValue = "modelharness"
)

// InferenceSetGVR is the GroupVersionResource of the KAITO InferenceSet, the
// subject enumerated by both the modeldeployment and weightdownload
// evaluators. KAITO types are not vendored, so they are consumed via the
// dynamic client as unstructured objects.
var InferenceSetGVR = schema.GroupVersionResource{Group: "kaito.sh", Version: "v1beta1", Resource: "inferencesets"}

// DiscoverNamespaces returns the workload namespaces carrying the
// productionstack.kaito.sh/managed-by=modelharness discovery label (§3). No
// static namespace list is used. It is shared by the harness, modeldeployment
// and weightdownload evaluators.
func DiscoverNamespaces(ctx context.Context, cs kubernetes.Interface) ([]string, error) {
	list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", ManagedByLabel, ManagedByValue),
	})
	if err != nil {
		return nil, fmt.Errorf("list managed namespaces: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names, nil
}
