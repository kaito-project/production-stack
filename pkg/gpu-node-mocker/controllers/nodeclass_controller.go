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

package controllers

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// NodeClassReadyCondition is the rollup condition type KAITO waits on. In a
// real cluster the karpenter provider sets it once it has resolved images,
// subnets and the Kubernetes version; in the mock environment no provider
// runs, so this reconciler sets it instead.
const NodeClassReadyCondition = "Ready"

// NodeClassReconciler mocks the karpenter provider's NodeClass readiness. In
// karpenter mode KAITO creates one or more NodeClass objects on startup (e.g.
// "image-family-ubuntu") and blocks ProvisionNodes() until the default class
// reports Ready=True. Since no real provider runs in the mock environment, this
// reconciler watches NodeClass objects and patches their status to Ready so
// KAITO proceeds to create the NodePool that NodePoolReconciler then mocks.
//
// The NodeClass GVK is taken from Config.NodeClass so the mocker can target its
// own mock node class (karpenter.kaito.sh/MockNodeClass — a kind only the
// mocker recognizes, so a real karpenter provider skips any NodePool that
// references it) or, when configured to, the real karpenter.azure.com
// AKSNodeClass. The object is handled as unstructured so the mocker does not
// have to vendor any provider API types; the only field this reconciler
// touches is status.conditions.
type NodeClassReconciler struct {
	client.Client
	Config Config
}

// SetupWithManager registers the controller. It watches every NodeClass of the
// configured GVK; there is no managed-by filter because KAITO owns the only
// NodeClass objects in the mock cluster and they all need to be marked Ready.
func (r *NodeClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	nc := &unstructured.Unstructured{}
	nc.SetGroupVersionKind(r.Config.NodeClass.GVK())
	return ctrl.NewControllerManagedBy(mgr).
		For(nc).
		Named("nodeclass").
		Complete(r)
}

// +kubebuilder:rbac:groups=karpenter.kaito.sh,resources=mocknodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.kaito.sh,resources=mocknodeclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=karpenter.azure.com,resources=aksnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.azure.com,resources=aksnodeclasses/status,verbs=get;update;patch

func (r *NodeClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("nodeclass", req.Name)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.Config.NodeClass.GVK())
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get NodeClass: %w", err)
	}

	// Nothing to do for objects on their way out.
	if !obj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	// Already Ready for the current generation — avoid a no-op status write.
	if nodeClassReady(obj) {
		return ctrl.Result{}, nil
	}

	if err := r.markReady(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("patched NodeClass status to Ready=True")
	return ctrl.Result{}, nil
}

// markReady patches the NodeClass status subresource so it carries a single
// Ready=True condition, which is all KAITO's checkNodeClassReady inspects.
func (r *NodeClassReconciler) markReady(ctx context.Context, obj *unstructured.Unstructured) error {
	cond := map[string]interface{}{
		"type":               NodeClassReadyCondition,
		"status":             "True",
		"reason":             "Mocked",
		"message":            "NodeClass marked Ready by gpu-node-mocker",
		"lastTransitionTime": metav1.Now().Format(time.RFC3339),
		"observedGeneration": obj.GetGeneration(),
	}

	patched := obj.DeepCopy()
	if err := unstructured.SetNestedSlice(patched.Object, []interface{}{cond}, "status", "conditions"); err != nil {
		return fmt.Errorf("set status.conditions: %w", err)
	}
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(obj)); err != nil {
		return fmt.Errorf("patch NodeClass status: %w", err)
	}
	return nil
}

// nodeClassReady reports whether the object already has a Ready=True status
// condition.
func nodeClassReady(obj *unstructured.Unstructured) bool {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == NodeClassReadyCondition && m["status"] == "True" {
			return true
		}
	}
	return false
}
