// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// nodeClassScheme extends the shared test scheme with the unstructured mock
// NodeClass GVK so the fake client can serve get/list/patch for it.
func nodeClassScheme() *runtime.Scheme {
	s := testScheme()
	gvk := DefaultNodeClassRef().GVK()
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	listGVK := gvk
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

func newNodeClass(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(DefaultNodeClassRef().GVK())
	obj.SetName(name)
	return obj
}

func getNodeClass(t *testing.T, r *NodeClassReconciler, name string) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(DefaultNodeClassRef().GVK())
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("get NodeClass %q: %v", name, err)
	}
	return got
}

func reconcileNodeClass(t *testing.T, r *NodeClassReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile NodeClass %q: %v", name, err)
	}
}

func TestNodeClassReconciler_MarksReady(t *testing.T) {
	scheme := nodeClassScheme()
	nc := newNodeClass("image-family-ubuntu")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).WithStatusSubresource(nc).Build()
	r := &NodeClassReconciler{Client: cl, Config: testConfig()}

	reconcileNodeClass(t, r, "image-family-ubuntu")

	got := getNodeClass(t, r, "image-family-ubuntu")
	if !nodeClassReady(got) {
		t.Fatalf("expected NodeClass to be Ready=True, got conditions: %v", got.Object["status"])
	}
}

func TestNodeClassReconciler_Idempotent(t *testing.T) {
	scheme := nodeClassScheme()
	nc := newNodeClass("image-family-ubuntu")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).WithStatusSubresource(nc).Build()
	r := &NodeClassReconciler{Client: cl, Config: testConfig()}

	reconcileNodeClass(t, r, "image-family-ubuntu")
	reconcileNodeClass(t, r, "image-family-ubuntu")

	got := getNodeClass(t, r, "image-family-ubuntu")
	conds, found, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	if err != nil || !found {
		t.Fatalf("expected status.conditions, found=%v err=%v", found, err)
	}
	if len(conds) != 1 {
		t.Fatalf("expected exactly one Ready condition after repeated reconciles, got %d", len(conds))
	}
}

func TestNodeClassReconciler_NotFound(t *testing.T) {
	scheme := nodeClassScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &NodeClassReconciler{Client: cl, Config: testConfig()}

	// Reconciling a non-existent object must be a no-op, not an error.
	reconcileNodeClass(t, r, "does-not-exist")
}
