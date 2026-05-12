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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// ── Fake Node GC tests ─────────────────────────────────────────────────────

func newFakeNode(name, nodeClaimRef string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				LabelFakeNode:  "true",
				LabelManagedBy: ControllerName,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "karpenter.sh/v1",
				Kind:       "NodeClaim",
				Name:       nodeClaimRef,
			}},
		},
	}
}

func TestFakeNodeGC_DeletesOrphanedNode(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	node := newFakeNode("fake-ws-orphan", "nc-gone")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := &FakeNodeGCReconciler{Client: cl}

	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fake-ws-orphan"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", res.RequeueAfter)
	}

	got := &corev1.Node{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "fake-ws-orphan"}, got); err == nil {
		t.Error("orphaned node should have been deleted")
	}
}

func TestFakeNodeGC_KeepsNodeWithExistingNodeClaim(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	nc := &karpenterv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "nc-alive"},
	}
	node := newFakeNode("fake-ws-alive", "nc-alive")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc, node).Build()
	r := &FakeNodeGCReconciler{Client: cl}

	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fake-ws-alive"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != gcInterval {
		t.Errorf("expected requeue after %v, got %v", gcInterval, res.RequeueAfter)
	}

	got := &corev1.Node{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "fake-ws-alive"}, got); err != nil {
		t.Errorf("node should still exist: %v", err)
	}
}

func TestFakeNodeGC_IgnoresNonFakeNode(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "real-node",
			Labels: map[string]string{"kubernetes.io/os": "linux"},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := &FakeNodeGCReconciler{Client: cl}

	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "real-node"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("should not requeue for non-fake node")
	}

	got := &corev1.Node{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "real-node"}, got); err != nil {
		t.Errorf("real node should not be deleted: %v", err)
	}
}

func TestFakeNodeGC_IgnoresNodeWithoutAnnotation(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fake-no-ref",
			Labels: map[string]string{
				LabelFakeNode:  "true",
				LabelManagedBy: ControllerName,
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := &FakeNodeGCReconciler{Client: cl}

	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fake-no-ref"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &corev1.Node{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "fake-no-ref"}, got); err != nil {
		t.Errorf("node without annotation should not be deleted: %v", err)
	}
}

func TestFakeNodeGC_DeletedNodeIsNoop(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &FakeNodeGCReconciler{Client: cl}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gone"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("should not requeue for missing node")
	}
}

// ── Shadow Pod GC tests ────────────────────────────────────────────────────

func newOrphanedShadowPod(name, ns, originalNS, originalName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				LabelManagedBy:    ControllerName,
				ShadowPodLabelKey: originalNS + "." + originalName,
			},
			Annotations: map[string]string{
				"kaito.sh/original-pod": originalNS + "/" + originalName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "llm-mocker", Image: "test/llm-mocker:latest"}},
		},
	}
}

func TestShadowPodGC_DeletesOrphanedShadowPod(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	shadow := newOrphanedShadowPod("shadow-default-falcon-0", "kaito-shadow", "default", "falcon-0")
	shadowNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kaito-shadow"}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(shadowNS, shadow).Build()
	r := &ShadowPodGCReconciler{Client: cl}

	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "kaito-shadow", Name: "shadow-default-falcon-0"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", res.RequeueAfter)
	}

	got := &corev1.Pod{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "kaito-shadow", Name: "shadow-default-falcon-0"}, got); err == nil {
		t.Error("orphaned shadow pod should have been deleted")
	}
}

func TestShadowPodGC_KeepsShadowPodWithExistingOriginal(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	original := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "falcon-0", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "kaito/falcon:latest"}}},
	}
	shadow := newOrphanedShadowPod("shadow-default-falcon-0", "kaito-shadow", "default", "falcon-0")
	shadowNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kaito-shadow"}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(shadowNS, original, shadow).Build()
	r := &ShadowPodGCReconciler{Client: cl}

	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "kaito-shadow", Name: "shadow-default-falcon-0"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != gcInterval {
		t.Errorf("expected requeue after %v, got %v", gcInterval, res.RequeueAfter)
	}

	got := &corev1.Pod{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "kaito-shadow", Name: "shadow-default-falcon-0"}, got); err != nil {
		t.Errorf("shadow pod should still exist: %v", err)
	}
}

func TestShadowPodGC_IgnoresNonShadowPod(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "regular-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &ShadowPodGCReconciler{Client: cl}

	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "regular-pod"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("should not requeue for non-shadow pod")
	}

	got := &corev1.Pod{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "regular-pod"}, got); err != nil {
		t.Errorf("regular pod should not be deleted: %v", err)
	}
}

func TestShadowPodGC_DeletedShadowPodIsNoop(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ShadowPodGCReconciler{Client: cl}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kaito-shadow", Name: "gone"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("should not requeue for missing shadow pod")
	}
}
