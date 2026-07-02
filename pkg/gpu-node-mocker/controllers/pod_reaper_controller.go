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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// FakeNodePodReaper acts as the missing kubelet for pod *deletion* on fake nodes.
//
// Fake nodes created by Phase 1 have no real kubelet. Kubernetes pod deletion is
// two-phase: the API server stamps metadata.deletionTimestamp, then the node's
// kubelet stops the containers and issues the final removal. On a fake node the
// second phase never happens, so a KAITO inference pod that is deleted (e.g.
// during a StatefulSet/Workspace rollout) hangs in Terminating forever. Because
// it still holds the fake node's single nvidia.com/gpu, the replacement pod can
// never schedule → Pending deadlock.
//
// This reconciler completes the deletion the fake kubelet would have performed:
// once the pod's graceful-termination deadline has elapsed it force-deletes the
// pod (grace period 0). The shadow pod and its ConfigMap are garbage-collected
// via their OwnerReference to the original pod, so no extra cleanup is required.
//
// It is the deletion-side counterpart of ShadowPodReconciler (which mirrors the
// kubelet's start-up job of driving Pending pods on fake nodes to Running).
type FakeNodePodReaper struct {
	client.Client
}

// SetupWithManager registers the reaper with a single Pod watch filtered to
// KAITO inference pods that are bound to a fake node and marked for deletion.
func (r *FakeNodePodReaper) SetupWithManager(mgr ctrl.Manager) error {
	terminatingOnFakeNode := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isTerminatingOnFakeNode(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return isTerminatingOnFakeNode(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("fake-node-pod-reaper").
		For(&corev1.Pod{}, builder.WithPredicates(terminatingOnFakeNode)).
		Complete(r)
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete

func (r *FakeNodePodReaper) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get pod: %w", err)
	}

	if !isTerminatingOnFakeNode(pod) {
		return ctrl.Result{}, nil
	}

	// Respect the graceful-termination window. metadata.deletionTimestamp is the
	// deadline the API server already computed as
	// (deletion-request time + terminationGracePeriodSeconds). If it has not yet
	// passed, requeue for the remaining time instead of force-deleting early.
	deadline := pod.DeletionTimestamp.Time
	if remaining := time.Until(deadline); remaining > 0 {
		log.Info("pod terminating on fake node; waiting for grace period to elapse",
			"node", pod.Spec.NodeName, "remaining", remaining.String())
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	// Grace period elapsed — complete the deletion the fake node's (absent)
	// kubelet would have performed.
	if err := r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))}); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("force delete terminating pod: %w", err)
	}

	log.Info("force-deleted pod stuck Terminating on fake node", "node", pod.Spec.NodeName)
	return ctrl.Result{}, nil
}

// isTerminatingOnFakeNode reports whether the pod is bound to a fake node
// (spec.nodeName starts with "fake-") and has been marked for deletion. Any
// such pod — a KAITO inference pod or an unrelated DaemonSet/Job pod that
// happened to be scheduled onto a fake node — hangs in Terminating forever
// because the fake node has no kubelet to finalize deletion. Force-deleting it
// is exactly the job the absent kubelet would have performed, so no KAITO-label
// gate is applied here.
func isTerminatingOnFakeNode(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}
	return pod.DeletionTimestamp != nil &&
		pod.Spec.NodeName != "" &&
		strings.HasPrefix(pod.Spec.NodeName, "fake-")
}
