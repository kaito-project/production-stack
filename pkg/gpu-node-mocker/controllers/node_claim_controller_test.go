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
	"sync"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = coordinationv1.AddToScheme(s)
	gv := schema.GroupVersion{Group: "karpenter.sh", Version: "v1"}
	s.AddKnownTypes(gv,
		&karpenterv1.NodeClaim{},
		&karpenterv1.NodeClaimList{},
		&karpenterv1.NodePool{},
		&karpenterv1.NodePoolList{},
	)
	metav1.AddToGroupVersion(s, gv)
	return s
}

func testConfig() Config {
	return Config{
		ShadowPodImage:        DefaultInferenceSimImage,
		UDSTokenizerImage:     DefaultUDSTokenizerImage,
		LeaseDurationSec:      40,
		LeaseRenewIntervalSec: 10,
	}
}

func newNodeClaim(name string) *karpenterv1.NodeClaim {
	return &karpenterv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				LabelKaitoWorkspace: "test-workspace",
				"apps":              "falcon-7b-instruct",
			},
		},
		Spec: karpenterv1.NodeClaimSpec{
			Taints: []corev1.Taint{
				{Key: "sku", Value: "gpu", Effect: corev1.TaintEffectNoSchedule},
			},
		},
	}
}

func TestFakeNodeName(t *testing.T) {
	tests := []struct {
		ncName string
		want   string
	}{
		{"ws12345", "fake-ws12345"},
		{"abc", "fake-abc"},
		{"", "fake-"},
	}
	for _, tt := range tests {
		nc := &karpenterv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: tt.ncName}}
		if got := fakeNodeName(nc); got != tt.want {
			t.Errorf("fakeNodeName(%q) = %q, want %q", tt.ncName, got, tt.want)
		}
	}
}

func TestControllerName(t *testing.T) {
	if got := controllerName("fake-ws123"); got != "gpu-mocker/fake-ws123" {
		t.Errorf("controllerName = %q", got)
	}
}

func TestNodeCapacity(t *testing.T) {
	cap := nodeCapacity()
	if cap.Cpu().Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("CPU = %s, want 4", cap.Cpu().String())
	}
	if cap.Memory().Cmp(resource.MustParse("16Gi")) != 0 {
		t.Errorf("Mem = %s, want 16Gi", cap.Memory().String())
	}
	gpuVal := cap[corev1.ResourceName("nvidia.com/gpu")]
	if gpuVal.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("GPU = %s, want 1", gpuVal.String())
	}
	if cap.Pods().Cmp(resource.MustParse("110")) != 0 {
		t.Errorf("Pods = %s, want 110", cap.Pods().String())
	}
}

func TestMakeCondition(t *testing.T) {
	now := metav1.Now()
	c := makeCondition(corev1.NodeReady, corev1.ConditionTrue, "R", "m", now)
	if c.Type != corev1.NodeReady || c.Status != corev1.ConditionTrue || c.Reason != "R" {
		t.Errorf("unexpected condition: %+v", c)
	}
}

// ── Reconciler tests using fake client ──────────────────────────────────────

func TestEnsureFakeNode_CreatesNode(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-test1")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	node, err := r.ensureFakeNode(ctx, nc, "fake-ws-test1")
	if err != nil {
		t.Fatalf("ensureFakeNode: %v", err)
	}

	// Verify node properties
	if node.Name != "fake-ws-test1" {
		t.Errorf("node name = %q", node.Name)
	}
	if node.Spec.ProviderID != "fake://fake-ws-test1" {
		t.Errorf("providerID = %q", node.Spec.ProviderID)
	}
	if node.Labels[LabelFakeNode] != "true" {
		t.Error("missing fake-node label")
	}
	if node.Labels[LabelKaitoWorkspace] != "test-workspace" {
		t.Errorf("workspace label = %q", node.Labels[LabelKaitoWorkspace])
	}
	if node.Labels[LabelExcludeLB] != "true" {
		t.Error("missing exclude-LB label")
	}
	if len(node.OwnerReferences) == 0 || node.OwnerReferences[0].Name != "ws-test1" {
		t.Errorf("ownerReference = %v, want NodeClaim ws-test1", node.OwnerReferences)
	}
	// Taints should come from NodeClaim
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "sku" {
		t.Errorf("taints = %v, want [sku=gpu:NoSchedule]", node.Spec.Taints)
	}
}

func TestEnsureFakeNode_ReturnsExisting(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-test1")
	existingNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-test1", Labels: map[string]string{"existing": "true"}},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc, existingNode).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	node, err := r.ensureFakeNode(ctx, nc, "fake-ws-test1")
	if err != nil {
		t.Fatalf("ensureFakeNode: %v", err)
	}
	if node.Labels["existing"] != "true" {
		t.Error("should return existing node, not create new one")
	}
}

func TestEnsureNodeReady_PatchesStatus(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-test1")
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-test1"},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc, node).WithStatusSubresource(node).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	if err := r.ensureNodeReady(ctx, node, nc); err != nil {
		t.Fatalf("ensureNodeReady: %v", err)
	}

	// Re-fetch and verify
	updated := &corev1.Node{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "fake-ws-test1"}, updated); err != nil {
		t.Fatal(err)
	}
	var ready bool
	for _, c := range updated.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		t.Error("node should be Ready after ensureNodeReady")
	}
	if updated.Status.Capacity.Cpu().Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("capacity CPU = %s", updated.Status.Capacity.Cpu().String())
	}
	gpu := updated.Status.Capacity[corev1.ResourceName("nvidia.com/gpu")]
	if gpu.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("capacity GPU = %s, want 1", gpu.String())
	}
}

func TestEnsureNodeReady_SkipsIfAlreadyReady(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-test1")
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-test1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc, node).WithStatusSubresource(node).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	// Should return nil without patching (no status subresource call)
	if err := r.ensureNodeReady(ctx, node, nc); err != nil {
		t.Fatalf("ensureNodeReady: %v", err)
	}
}

func TestEnsureNodeClaimReady_PatchesStatus(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-test1")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).WithStatusSubresource(nc).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	if err := r.ensureNodeClaimReady(ctx, nc, "fake-ws-test1"); err != nil {
		t.Fatalf("ensureNodeClaimReady: %v", err)
	}

	updated := &karpenterv1.NodeClaim{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "ws-test1"}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.NodeName != "fake-ws-test1" {
		t.Errorf("nodeName = %q", updated.Status.NodeName)
	}
	if updated.Status.ProviderID != "fake://fake-ws-test1" {
		t.Errorf("providerID = %q", updated.Status.ProviderID)
	}
	if len(updated.Status.Conditions) != 4 {
		t.Errorf("conditions count = %d, want 4", len(updated.Status.Conditions))
	}
	// Verify all expected condition types are present.
	expectedTypes := map[string]bool{"Ready": false, "Launched": false, "Registered": false, "Initialized": false}
	for _, c := range updated.Status.Conditions {
		if _, ok := expectedTypes[string(c.Type)]; ok {
			expectedTypes[string(c.Type)] = true
		}
	}
	for typ, found := range expectedTypes {
		if !found {
			t.Errorf("missing condition type %q", typ)
		}
	}
}

func TestEnsureNodeClaimReady_SkipsIfAlreadyReady(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-test1")
	nc.Status.NodeName = "already-set"

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).WithStatusSubresource(nc).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	if err := r.ensureNodeClaimReady(ctx, nc, "fake-ws-test1"); err != nil {
		t.Fatalf("ensureNodeClaimReady: %v", err)
	}
	// Status should not change
	updated := &karpenterv1.NodeClaim{}
	_ = cl.Get(ctx, types.NamespacedName{Name: "ws-test1"}, updated)
	if updated.Status.NodeName != "already-set" {
		t.Error("should not overwrite existing nodeName")
	}
}

func TestEnsureLease_CreatesNew(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	if err := r.ensureLease(ctx, "fake-ws-test1"); err != nil {
		t.Fatalf("ensureLease: %v", err)
	}

	lease := &coordinationv1.Lease{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: "fake-ws-test1"}, lease); err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if *lease.Spec.LeaseDurationSeconds != 40 {
		t.Errorf("leaseDuration = %d", *lease.Spec.LeaseDurationSeconds)
	}
	if lease.Labels[LabelManagedBy] != ControllerName {
		t.Errorf("label = %q", lease.Labels[LabelManagedBy])
	}
}

func TestEnsureLease_UpdatesExisting(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	existingLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fake-ws-test1",
			Namespace: "kube-node-lease",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingLease).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	if err := r.ensureLease(ctx, "fake-ws-test1"); err != nil {
		t.Fatalf("ensureLease: %v", err)
	}

	updated := &coordinationv1.Lease{}
	_ = cl.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: "fake-ws-test1"}, updated)
	if updated.Spec.RenewTime == nil {
		t.Error("renewTime should be set")
	}
}

func TestRenewLease(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-test1", Namespace: "kube-node-lease"},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lease).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	if err := r.renewLease(ctx, "fake-ws-test1"); err != nil {
		t.Fatalf("renewLease: %v", err)
	}

	updated := &coordinationv1.Lease{}
	_ = cl.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: "fake-ws-test1"}, updated)
	if updated.Spec.RenewTime == nil {
		t.Error("renewTime should be updated")
	}
}

func TestRenewLease_NotFound(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	// Should not error when lease doesn't exist
	if err := r.renewLease(ctx, "nonexistent"); err != nil {
		t.Fatalf("renewLease should not fail for missing lease: %v", err)
	}
}

func TestStopLeaseRenewer(t *testing.T) {
	r := &NodeClaimReconciler{cancelFuncs: make(map[string]context.CancelFunc)}

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancelFuncs["fake-ws-test1"] = cancel
	r.mu.Unlock()

	r.stopLeaseRenewer("fake-ws-test1")

	// Context should be cancelled
	select {
	case <-ctx.Done():
	// good
	default:
		t.Error("stopLeaseRenewer should cancel the context")
	}

	r.mu.Lock()
	_, exists := r.cancelFuncs["fake-ws-test1"]
	r.mu.Unlock()
	if exists {
		t.Error("cancelFunc should be removed from map")
	}
}

func TestStopLeaseRenewer_Noop(t *testing.T) {
	r := &NodeClaimReconciler{cancelFuncs: make(map[string]context.CancelFunc)}
	// Should not panic for nonexistent node
	r.stopLeaseRenewer("nonexistent")
}

func TestEnsureLeaseRenewer_StartsOnce(t *testing.T) {
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &NodeClaimReconciler{
		Client:      cl,
		Config:      testConfig(),
		cancelFuncs: make(map[string]context.CancelFunc),
	}

	ctx := context.Background()
	r.ensureLeaseRenewer(ctx, "fake-ws-test1")

	r.mu.Lock()
	_, exists := r.cancelFuncs["fake-ws-test1"]
	r.mu.Unlock()
	if !exists {
		t.Error("should have a cancelFunc after ensureLeaseRenewer")
	}

	// Call again — should not create a second goroutine
	r.ensureLeaseRenewer(ctx, "fake-ws-test1")

	// Cleanup
	r.stopLeaseRenewer("fake-ws-test1")
}

func TestReconcileDelete(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	now := metav1.Now()
	nc := newNodeClaim("ws-test1")
	nc.Finalizers = []string{fakeNodeFinalizer}
	nc.DeletionTimestamp = &now

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-test1"}}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-test1", Namespace: "kube-node-lease"},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc, node, lease).Build()
	r := &NodeClaimReconciler{
		Client:      cl,
		Config:      testConfig(),
		cancelFuncs: make(map[string]context.CancelFunc),
		mu:          sync.Mutex{},
	}

	result, err := r.reconcileDelete(ctx, nc)
	if err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue after delete")
	}

	// Node should be deleted
	got := &corev1.Node{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "fake-ws-test1"}, got); err == nil {
		t.Error("node should be deleted")
	}

	// Lease should be deleted
	gotLease := &coordinationv1.Lease{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: "fake-ws-test1"}, gotLease); err == nil {
		t.Error("lease should be deleted")
	}

	// Finalizer should be removed
	updated := &karpenterv1.NodeClaim{}
	_ = cl.Get(ctx, types.NamespacedName{Name: "ws-test1"}, updated)
	for _, f := range updated.Finalizers {
		if f == fakeNodeFinalizer {
			t.Error("finalizer should be removed")
		}
	}
}

func TestReconcile_NotFound(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent"}})
	if err != nil {
		t.Fatalf("should not error for missing NodeClaim: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for missing NodeClaim")
	}
}

func TestReconcile_AddsFinalizer(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-test1")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).WithStatusSubresource(nc).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-test1"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.Requeue {
		t.Error("should requeue after adding finalizer")
	}

	updated := &karpenterv1.NodeClaim{}
	_ = cl.Get(ctx, types.NamespacedName{Name: "ws-test1"}, updated)
	found := false
	for _, f := range updated.Finalizers {
		if f == fakeNodeFinalizer {
			found = true
		}
	}
	if !found {
		t.Error("finalizer should be added")
	}
}

func TestEnsureFakeNode_InstanceTypeFromRequirements(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-itype")
	nc.Spec.Requirements = []karpenterv1.NodeSelectorRequirementWithMinValues{
		{
			Key:      "node.kubernetes.io/instance-type",
			Operator: "In",
			Values:   []string{"Standard_NC6s_v3"},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	node, err := r.ensureFakeNode(ctx, nc, "fake-ws-itype")
	if err != nil {
		t.Fatalf("ensureFakeNode: %v", err)
	}
	if node.Labels["node.kubernetes.io/instance-type"] != "Standard_NC6s_v3" {
		t.Errorf("instance-type = %q", node.Labels["node.kubernetes.io/instance-type"])
	}
	if node.Labels["beta.kubernetes.io/instance-type"] != "Standard_NC6s_v3" {
		t.Errorf("beta instance-type = %q", node.Labels["beta.kubernetes.io/instance-type"])
	}
}

func TestReconcile_FullHappyPath(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-happy")
	nc.Finalizers = []string{fakeNodeFinalizer} // already has finalizer

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).WithStatusSubresource(nc, &corev1.Node{}).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-happy"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue on happy path")
	}

	// Verify fake node was created
	node := &corev1.Node{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "fake-ws-happy"}, node); err != nil {
		t.Fatalf("node not created: %v", err)
	}
	if node.Spec.ProviderID != "fake://fake-ws-happy" {
		t.Errorf("providerID = %q", node.Spec.ProviderID)
	}

	// Verify node is Ready
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		t.Error("node should be Ready")
	}

	// Verify lease was created
	lease := &coordinationv1.Lease{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: "fake-ws-happy"}, lease); err != nil {
		t.Fatalf("lease not created: %v", err)
	}

	// Verify NodeClaim status was patched
	updated := &karpenterv1.NodeClaim{}
	_ = cl.Get(ctx, types.NamespacedName{Name: "ws-happy"}, updated)
	if updated.Status.NodeName != "fake-ws-happy" {
		t.Errorf("NodeClaim.Status.NodeName = %q", updated.Status.NodeName)
	}

	// Verify lease renewer is running
	r.mu.Lock()
	_, hasRenewer := r.cancelFuncs["fake-ws-happy"]
	r.mu.Unlock()
	if !hasRenewer {
		t.Error("lease renewer should be running")
	}

	// Cleanup
	r.stopLeaseRenewer("fake-ws-happy")
}

func TestReconcile_DeletionPath(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	now := metav1.Now()
	nc := newNodeClaim("ws-del")
	nc.Finalizers = []string{fakeNodeFinalizer}
	nc.DeletionTimestamp = &now

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-del"}}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-del", Namespace: "kube-node-lease"}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc, node, lease).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-del"}})
	if err != nil {
		t.Fatalf("Reconcile deletion: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue after deletion")
	}

	// Verify cleanup
	if err := cl.Get(ctx, types.NamespacedName{Name: "fake-ws-del"}, &corev1.Node{}); err == nil {
		t.Error("node should be deleted")
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: "fake-ws-del"}, &coordinationv1.Lease{}); err == nil {
		t.Error("lease should be deleted")
	}
}

func TestEnsureFakeNode_PropagatesAllNodeClaimLabels(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	nc := newNodeClaim("ws-labels")
	nc.Labels["custom-label"] = "custom-value"

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).Build()
	r := &NodeClaimReconciler{Client: cl, Config: testConfig(), cancelFuncs: make(map[string]context.CancelFunc)}

	node, err := r.ensureFakeNode(ctx, nc, "fake-ws-labels")
	if err != nil {
		t.Fatalf("ensureFakeNode: %v", err)
	}
	if node.Labels["custom-label"] != "custom-value" {
		t.Errorf("custom label not propagated: %v", node.Labels)
	}
	if node.Labels["apps"] != "falcon-7b-instruct" {
		t.Errorf("apps label not propagated: %v", node.Labels)
	}
}

func TestReconcile_DeletionPath_StopsLeaseRenewer(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	now := metav1.Now()
	nc := newNodeClaim("ws-del-renewer")
	nc.Finalizers = []string{fakeNodeFinalizer}
	nc.DeletionTimestamp = &now

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-del-renewer"}}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "fake-ws-del-renewer", Namespace: "kube-node-lease"}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc, node, lease).Build()
	renewCtx, cancel := context.WithCancel(context.Background())

	r := &NodeClaimReconciler{
		Client:      cl,
		Config:      testConfig(),
		cancelFuncs: map[string]context.CancelFunc{"fake-ws-del-renewer": cancel},
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-del-renewer"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Verify the renewer context was cancelled
	select {
	case <-renewCtx.Done():
	default:
		t.Error("lease renewer should be stopped on deletion")
	}

	r.mu.Lock()
	_, exists := r.cancelFuncs["fake-ws-del-renewer"]
	r.mu.Unlock()
	if exists {
		t.Error("cancelFunc should be removed from map")
	}
}
