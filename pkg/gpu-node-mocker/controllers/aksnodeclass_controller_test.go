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

// aksScheme extends the shared test scheme with the unstructured AKSNodeClass
// GVK so the fake client can serve get/list/patch for it.
func aksScheme() *runtime.Scheme {
	s := testScheme()
	s.AddKnownTypeWithName(aksNodeClassGVK(), &unstructured.Unstructured{})
	listGVK := aksNodeClassGVK()
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

func newAKSNodeClass(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(aksNodeClassGVK())
	obj.SetName(name)
	return obj
}

func getAKSNodeClass(t *testing.T, r *AKSNodeClassReconciler, name string) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(aksNodeClassGVK())
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("get AKSNodeClass %q: %v", name, err)
	}
	return got
}

func reconcileAKSNodeClass(t *testing.T, r *AKSNodeClassReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile AKSNodeClass %q: %v", name, err)
	}
}

func TestAKSNodeClassReconciler_MarksReady(t *testing.T) {
	scheme := aksScheme()
	nc := newAKSNodeClass("image-family-ubuntu")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).WithStatusSubresource(nc).Build()
	r := &AKSNodeClassReconciler{Client: cl, Config: testConfig()}

	reconcileAKSNodeClass(t, r, "image-family-ubuntu")

	got := getAKSNodeClass(t, r, "image-family-ubuntu")
	if !aksNodeClassReady(got) {
		t.Fatalf("expected AKSNodeClass to be Ready=True, got conditions: %v", got.Object["status"])
	}
}

func TestAKSNodeClassReconciler_Idempotent(t *testing.T) {
	scheme := aksScheme()
	nc := newAKSNodeClass("image-family-ubuntu")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).WithStatusSubresource(nc).Build()
	r := &AKSNodeClassReconciler{Client: cl, Config: testConfig()}

	reconcileAKSNodeClass(t, r, "image-family-ubuntu")
	reconcileAKSNodeClass(t, r, "image-family-ubuntu")

	got := getAKSNodeClass(t, r, "image-family-ubuntu")
	conds, found, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	if err != nil || !found {
		t.Fatalf("expected status.conditions, found=%v err=%v", found, err)
	}
	if len(conds) != 1 {
		t.Fatalf("expected exactly one Ready condition after repeated reconciles, got %d", len(conds))
	}
}

func TestAKSNodeClassReconciler_NotFound(t *testing.T) {
	scheme := aksScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &AKSNodeClassReconciler{Client: cl, Config: testConfig()}

	// Reconciling a non-existent object must be a no-op, not an error.
	reconcileAKSNodeClass(t, r, "does-not-exist")
}
