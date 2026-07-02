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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestIsTerminatingOnFakeNode(t *testing.T) {
	del := metav1.NewTime(time.Now())
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"valid inferenceset", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Labels:            map[string]string{InferenceSetCreatedByLabelKey: "falcon"},
				DeletionTimestamp: &del,
			},
			Spec: corev1.PodSpec{NodeName: "fake-ws1"},
		}, true},
		{"valid workspace", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Labels:            map[string]string{LabelKaitoWorkspace: "workspace-phi"},
				DeletionTimestamp: &del,
			},
			Spec: corev1.PodSpec{NodeName: "fake-ws1"},
		}, true},
		{"not terminating", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{InferenceSetCreatedByLabelKey: "falcon"}},
			Spec:       corev1.PodSpec{NodeName: "fake-ws1"},
		}, false},
		{"real node", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Labels:            map[string]string{InferenceSetCreatedByLabelKey: "falcon"},
				DeletionTimestamp: &del,
			},
			Spec: corev1.PodSpec{NodeName: "aks-node1"},
		}, false},
		{"no kaito label", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Labels:            map[string]string{"app": "nginx"},
				DeletionTimestamp: &del,
			},
			Spec: corev1.PodSpec{NodeName: "fake-ws1"},
		}, false},
		{"no node", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Labels:            map[string]string{InferenceSetCreatedByLabelKey: "falcon"},
				DeletionTimestamp: &del,
			},
		}, false},
		{"nil labels", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &del},
			Spec:       corev1.PodSpec{NodeName: "fake-ws1"},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTerminatingOnFakeNode(tt.pod); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// newTerminatingPodOnFakeNode builds a KAITO inference pod that is bound to a
// fake node and marked for deletion. A finalizer is required because the fake
// client refuses to persist an object that has a deletionTimestamp but no
// finalizers.
func newTerminatingPodOnFakeNode(name, ns, node string, deletionTS time.Time) *corev1.Pod {
	dt := metav1.NewTime(deletionTS)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:                       name,
			Namespace:                  ns,
			Labels:                     map[string]string{InferenceSetCreatedByLabelKey: "falcon-7b-instruct"},
			DeletionTimestamp:          &dt,
			DeletionGracePeriodSeconds: ptrInt64(30),
			Finalizers:                 []string{"kaito.sh/test-hold"},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "model", Image: "kaito/falcon:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func ptrInt64(v int64) *int64 { return &v }

func TestFakeNodePodReaper_ForceDeletesAfterGrace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	// deletionTimestamp already in the past → grace period elapsed.
	pod := newTerminatingPodOnFakeNode("falcon-0", "default", "fake-ws1", time.Now().Add(-time.Minute))

	var deleteCalled bool
	var gotGrace *int64
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteCalled = true
				do := &client.DeleteOptions{}
				for _, o := range opts {
					o.ApplyToDelete(do)
				}
				gotGrace = do.GracePeriodSeconds
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &FakeNodePodReaper{Client: cl}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "falcon-0", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue after grace elapsed, got %v", res.RequeueAfter)
	}
	if !deleteCalled {
		t.Fatal("expected pod to be force-deleted")
	}
	if gotGrace == nil || *gotGrace != 0 {
		t.Errorf("expected force delete with grace period 0, got %v", gotGrace)
	}
}

func TestFakeNodePodReaper_RequeuesDuringGrace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	// deletionTimestamp in the future → still within the graceful window.
	pod := newTerminatingPodOnFakeNode("falcon-0", "default", "fake-ws1", time.Now().Add(30*time.Second))

	var deleteCalled bool
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteCalled = true
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &FakeNodePodReaper{Client: cl}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "falcon-0", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected requeue during grace window, got %v", res.RequeueAfter)
	}
	if deleteCalled {
		t.Error("pod must not be deleted before grace period elapses")
	}
}

func TestFakeNodePodReaper_IgnoresNonTerminating(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "falcon-0",
			Namespace: "default",
			Labels:    map[string]string{InferenceSetCreatedByLabelKey: "falcon"},
		},
		Spec:   corev1.PodSpec{NodeName: "fake-ws1", Containers: []corev1.Container{{Name: "c", Image: "i"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	var deleteCalled bool
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteCalled = true
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &FakeNodePodReaper{Client: cl}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "falcon-0", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 || deleteCalled {
		t.Errorf("non-terminating pod must be ignored, requeue=%v deleteCalled=%v", res.RequeueAfter, deleteCalled)
	}
}

func TestFakeNodePodReaper_NotFound(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &FakeNodePodReaper{Client: cl}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gone", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected empty result, got %v", res.RequeueAfter)
	}
}
