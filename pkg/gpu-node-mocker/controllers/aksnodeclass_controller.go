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
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// AKSNodeClassGroup / AKSNodeClassVersion / AKSNodeClassKind identify the
	// Azure karpenter provider's NodeClass resource. In karpenter mode KAITO's
	// NodePool template references an AKSNodeClass by name and, before it will
	// provision nodes, polls that NodeClass for a Ready=True status condition.
	AKSNodeClassGroup   = "karpenter.azure.com"
	AKSNodeClassVersion = "v1beta1"
	AKSNodeClassKind    = "AKSNodeClass"

	// AKSNodeClassReadyCondition is the rollup condition type KAITO waits on.
	// In a real cluster the Azure karpenter provider sets it once it has
	// resolved images, subnets and the Kubernetes version; in the mock
	// environment no provider runs, so this reconciler sets it instead.
	AKSNodeClassReadyCondition = "Ready"
)

// AKSNodeClassReconciler mocks the Azure karpenter provider's NodeClass
// readiness. KAITO creates one or more AKSNodeClass objects on startup (e.g.
// "image-family-ubuntu") and blocks ProvisionNodes() until the default class
// reports Ready=True. Since no real provider runs in the mock environment, this
// reconciler watches AKSNodeClass objects and patches their status to Ready so
// KAITO proceeds to create the NodePool that NodePoolReconciler then mocks.
//
// AKSNodeClass is handled as an unstructured object so the mocker does not have
// to vendor the karpenter-provider-azure API types; the CRD is shipped by this
// chart and the only field this reconciler touches is status.conditions.
type AKSNodeClassReconciler struct {
	client.Client
	Config Config
}

func aksNodeClassGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   AKSNodeClassGroup,
		Version: AKSNodeClassVersion,
		Kind:    AKSNodeClassKind,
	}
}

// SetupWithManager registers the controller. It watches every AKSNodeClass;
// there is no managed-by filter because KAITO owns the only AKSNodeClass
// objects in the mock cluster and they all need to be marked Ready.
func (r *AKSNodeClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	nc := &unstructured.Unstructured{}
	nc.SetGroupVersionKind(aksNodeClassGVK())
	return ctrl.NewControllerManagedBy(mgr).
		For(nc).
		Named("aksnodeclass").
		Complete(r)
}

// +kubebuilder:rbac:groups=karpenter.azure.com,resources=aksnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.azure.com,resources=aksnodeclasses/status,verbs=get;update;patch

func (r *AKSNodeClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("aksnodeclass", req.Name)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(aksNodeClassGVK())
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get AKSNodeClass: %w", err)
	}

	// Nothing to do for objects on their way out.
	if !obj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	// Already Ready for the current generation — avoid a no-op status write.
	if aksNodeClassReady(obj) {
		return ctrl.Result{}, nil
	}

	if err := r.markReady(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("patched AKSNodeClass status to Ready=True")
	return ctrl.Result{}, nil
}

// markReady patches the AKSNodeClass status subresource so it carries a single
// Ready=True condition, which is all KAITO's checkNodeClassReady inspects.
func (r *AKSNodeClassReconciler) markReady(ctx context.Context, obj *unstructured.Unstructured) error {
	cond := map[string]interface{}{
		"type":               AKSNodeClassReadyCondition,
		"status":             "True",
		"reason":             "Mocked",
		"message":            "AKSNodeClass marked Ready by gpu-node-mocker",
		"lastTransitionTime": metav1.Now().Format(time.RFC3339),
		"observedGeneration": obj.GetGeneration(),
	}

	patched := obj.DeepCopy()
	if err := unstructured.SetNestedSlice(patched.Object, []interface{}{cond}, "status", "conditions"); err != nil {
		return fmt.Errorf("set status.conditions: %w", err)
	}
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(obj)); err != nil {
		return fmt.Errorf("patch AKSNodeClass status: %w", err)
	}
	return nil
}

// aksNodeClassReady reports whether the object already has a Ready=True
// status condition.
func aksNodeClassReady(obj *unstructured.Unstructured) bool {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == AKSNodeClassReadyCondition && m["status"] == "True" {
			return true
		}
	}
	return false
}
