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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func newNodePool(name string, replicas *int64) *karpenterv1.NodePool {
	return &karpenterv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				KarpenterManagedByLabel:             KarpenterManagedByValue,
				"karpenter.kaito.sh/workspace-name": "test-workspace",
			},
		},
		Spec: karpenterv1.NodePoolSpec{
			Replicas: replicas,
			Template: karpenterv1.NodeClaimTemplate{
				ObjectMeta: karpenterv1.ObjectMeta{
					Labels: map[string]string{
						"karpenter.kaito.sh/workspace-name": "test-workspace",
						"apps":                              "falcon-7b-instruct",
					},
				},
				Spec: karpenterv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpenterv1.NodeClassReference{
						Group: "karpenter.azure.com",
						Kind:  "AKSNodeClass",
						Name:  "default",
					},
					Requirements: []karpenterv1.NodeSelectorRequirementWithMinValues{
						{
							Key:      "node.kubernetes.io/instance-type",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"Standard_NC6s_v3"},
						},
					},
					Taints: []corev1.Taint{
						{Key: "sku", Value: "gpu", Effect: corev1.TaintEffectNoSchedule},
					},
				},
			},
		},
	}
}

func reconcileNodePool(t *testing.T, r *NodePoolReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func listOwnedNodeClaims(t *testing.T, cl client.Client, poolName string) []karpenterv1.NodeClaim {
	t.Helper()
	list := &karpenterv1.NodeClaimList{}
	if err := cl.List(context.Background(), list, client.MatchingLabels{LabelNodePool: poolName}); err != nil {
		t.Fatalf("list NodeClaims: %v", err)
	}
	return list.Items
}

func TestNodePoolReconcile_CreatesNodeClaims(t *testing.T) {
	scheme := testScheme()
	np := newNodePool("test-workspace", ptr.To(int64(2)))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(np).Build()
	r := &NodePoolReconciler{Client: cl, Config: testConfig()}

	reconcileNodePool(t, r, np.Name)

	ncs := listOwnedNodeClaims(t, cl, np.Name)
	if len(ncs) != 2 {
		t.Fatalf("got %d NodeClaims, want 2", len(ncs))
	}
	for i := range ncs {
		nc := &ncs[i]
		if nc.Labels[LabelNodePool] != np.Name {
			t.Errorf("NodeClaim %q missing nodepool label", nc.Name)
		}
		if nc.Labels["apps"] != "falcon-7b-instruct" {
			t.Errorf("NodeClaim %q missing template label", nc.Name)
		}
		if len(nc.OwnerReferences) == 0 || nc.OwnerReferences[0].Name != np.Name {
			t.Errorf("NodeClaim %q ownerRef = %v, want NodePool %q", nc.Name, nc.OwnerReferences, np.Name)
		}
		if nc.Spec.NodeClassRef == nil || nc.Spec.NodeClassRef.Kind != "AKSNodeClass" {
			t.Errorf("NodeClaim %q nodeClassRef = %v", nc.Name, nc.Spec.NodeClassRef)
		}
		if len(nc.Spec.Requirements) != 1 || nc.Spec.Requirements[0].Values[0] != "Standard_NC6s_v3" {
			t.Errorf("NodeClaim %q requirements = %v", nc.Name, nc.Spec.Requirements)
		}
		if len(nc.Spec.Taints) != 1 || nc.Spec.Taints[0].Key != "sku" {
			t.Errorf("NodeClaim %q taints = %v", nc.Name, nc.Spec.Taints)
		}
	}
}

func TestNodePoolReconcile_Idempotent(t *testing.T) {
	scheme := testScheme()
	np := newNodePool("test-workspace", ptr.To(int64(2)))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(np).Build()
	r := &NodePoolReconciler{Client: cl, Config: testConfig()}

	reconcileNodePool(t, r, np.Name)
	reconcileNodePool(t, r, np.Name)

	if ncs := listOwnedNodeClaims(t, cl, np.Name); len(ncs) != 2 {
		t.Fatalf("got %d NodeClaims after second reconcile, want 2 (idempotent)", len(ncs))
	}
}

func TestNodePoolReconcile_ScalesUp(t *testing.T) {
	scheme := testScheme()
	np := newNodePool("test-workspace", ptr.To(int64(1)))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(np).Build()
	r := &NodePoolReconciler{Client: cl, Config: testConfig()}

	reconcileNodePool(t, r, np.Name)
	if ncs := listOwnedNodeClaims(t, cl, np.Name); len(ncs) != 1 {
		t.Fatalf("got %d NodeClaims, want 1", len(ncs))
	}

	// Bump replicas to 3.
	cur := &karpenterv1.NodePool{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: np.Name}, cur); err != nil {
		t.Fatalf("get NodePool: %v", err)
	}
	cur.Spec.Replicas = ptr.To(int64(3))
	if err := cl.Update(context.Background(), cur); err != nil {
		t.Fatalf("update NodePool: %v", err)
	}

	reconcileNodePool(t, r, np.Name)
	if ncs := listOwnedNodeClaims(t, cl, np.Name); len(ncs) != 3 {
		t.Fatalf("got %d NodeClaims after scale up, want 3", len(ncs))
	}
}

func TestNodePoolReconcile_ScalesDown(t *testing.T) {
	scheme := testScheme()
	np := newNodePool("test-workspace", ptr.To(int64(3)))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(np).Build()
	r := &NodePoolReconciler{Client: cl, Config: testConfig()}

	reconcileNodePool(t, r, np.Name)
	if ncs := listOwnedNodeClaims(t, cl, np.Name); len(ncs) != 3 {
		t.Fatalf("got %d NodeClaims, want 3", len(ncs))
	}

	cur := &karpenterv1.NodePool{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: np.Name}, cur); err != nil {
		t.Fatalf("get NodePool: %v", err)
	}
	cur.Spec.Replicas = ptr.To(int64(1))
	if err := cl.Update(context.Background(), cur); err != nil {
		t.Fatalf("update NodePool: %v", err)
	}

	reconcileNodePool(t, r, np.Name)
	if ncs := listOwnedNodeClaims(t, cl, np.Name); len(ncs) != 1 {
		t.Fatalf("got %d NodeClaims after scale down, want 1", len(ncs))
	}
}

func TestNodePoolReconcile_NilReplicasCreatesNone(t *testing.T) {
	scheme := testScheme()
	np := newNodePool("test-workspace", nil)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(np).Build()
	r := &NodePoolReconciler{Client: cl, Config: testConfig()}

	reconcileNodePool(t, r, np.Name)
	if ncs := listOwnedNodeClaims(t, cl, np.Name); len(ncs) != 0 {
		t.Fatalf("got %d NodeClaims, want 0 for nil replicas", len(ncs))
	}
}
