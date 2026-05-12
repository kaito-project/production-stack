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
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const gcInterval = 30 * time.Second

// ---------- Fake Node GC ----------

// FakeNodeGCReconciler watches fake Nodes and deletes any whose linked
// NodeClaim no longer exists. This prevents node leaks when the normal
// finalizer-based cleanup in NodeClaimReconciler is bypassed (e.g. the
// NodeClaim was force-deleted).
type FakeNodeGCReconciler struct {
	client.Client
	StopLeaseRenewer func(nodeName string)
}

func (r *FakeNodeGCReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only act on fake nodes created by us.
	if node.Labels[LabelFakeNode] != "true" || node.Labels[LabelManagedBy] != ControllerName {
		return ctrl.Result{}, nil
	}

	ncName := nodeClaimOwner(node)
	if ncName == "" {
		return ctrl.Result{}, nil
	}

	// Check if the linked NodeClaim still exists.
	nc := &karpenterv1.NodeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: ncName}, nc)
	if err == nil {
		// NodeClaim exists — nothing to do, requeue after interval.
		return ctrl.Result{RequeueAfter: gcInterval}, nil
	}
	if !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("check NodeClaim %s: %w", ncName, err)
	}

	// NodeClaim is gone — delete the orphaned fake node and its lease.
	log.Info("garbage collecting orphaned fake node", "node", node.Name, "missingNodeClaim", ncName)

	// Stop the lease renewal goroutine if one is running.
	if r.StopLeaseRenewer != nil {
		r.StopLeaseRenewer(node.Name)
	}

	// Delete the associated Lease from kube-node-lease.
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: node.Name}, lease); err == nil {
		if delErr := r.Delete(ctx, lease); delErr != nil && !errors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("delete orphaned lease %s: %w", node.Name, delErr)
		}
	}

	if err := r.Delete(ctx, node); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete orphaned node %s: %w", node.Name, err)
	}

	return ctrl.Result{}, nil
}

func (r *FakeNodeGCReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Named("FakeNodeGC").
		Complete(r)
}

// nodeClaimOwner returns the name of the owning NodeClaim from the Node's
// OwnerReferences, or "" if none is found.
func nodeClaimOwner(node *corev1.Node) string {
	for _, ref := range node.OwnerReferences {
		if ref.Kind == "NodeClaim" {
			return ref.Name
		}
	}
	return ""
}

// ---------- Shadow Pod GC ----------

// ShadowPodGCReconciler watches shadow Pods and deletes any whose linked
// original (inference) pod no longer exists. This prevents shadow pod leaks
// when the original pod is deleted before the ShadowPodReconciler can react.
type ShadowPodGCReconciler struct {
	client.Client
}

func (r *ShadowPodGCReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	shadow := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, shadow); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only act on shadow pods created by us.
	if shadow.Labels[LabelManagedBy] != ControllerName {
		return ctrl.Result{}, nil
	}

	// Resolve the original pod reference. Prefer the annotation (no length
	// limit, format "namespace/name") over the label (may be truncated to
	// 63 chars, format "namespace.name").
	var originalNS, originalName string
	if ref, ok := shadow.Annotations["kaito.sh/original-pod"]; ok {
		parts := strings.SplitN(ref, "/", 2)
		if len(parts) == 2 {
			originalNS, originalName = parts[0], parts[1]
		}
	}
	if originalNS == "" || originalName == "" {
		ref, ok := shadow.Labels[ShadowPodLabelKey]
		if !ok {
			return ctrl.Result{}, nil
		}
		parts := strings.SplitN(ref, ".", 2)
		if len(parts) != 2 {
			return ctrl.Result{}, nil
		}
		originalNS, originalName = parts[0], parts[1]
	}

	// Check if the linked original pod still exists.
	original := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: originalNS, Name: originalName}, original)
	if err == nil {
		// Original pod exists — nothing to do, requeue after interval.
		return ctrl.Result{RequeueAfter: gcInterval}, nil
	}
	if !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("check original pod %s/%s: %w", originalNS, originalName, err)
	}

	// Original pod is gone — delete the orphaned shadow pod.
	log.Info("garbage collecting orphaned shadow pod", "shadowPod", shadow.Name, "missingOriginal", originalNS+"/"+originalName)
	if err := r.Delete(ctx, shadow); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete orphaned shadow pod %s: %w", shadow.Name, err)
	}

	return ctrl.Result{}, nil
}

func (r *ShadowPodGCReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Named("ShadowPodGC").
		Complete(r)
}
