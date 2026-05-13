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
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/status"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	fakeNodeFinalizer = "kaito.sh/fake-node-protection"
)

// NodeClaimReconciler implements Phase 1 of the Shadow Pod lifecycle.
//
// It watches Karpenter NodeClaim objects and for each one:
//  1. Creates a fake Node with an un-parseable providerID so the Azure CCM
//     skips it via InstanceExistsByProviderID returning an error.
//  2. Patches the Node's status to Ready=True with realistic allocatable/
//     capacity values derived from the NodeClaim's resource requests.
//  3. Creates a kube-node-lease Lease and spawns a goroutine that renews
//     renewTime every Config.LeaseRenewIntervalSec seconds, preventing the
//     node-lifecycle-controller from marking the node Unknown.
//  4. Cleans everything up (Node + Lease + goroutine) when the NodeClaim is
//     deleted.
type NodeClaimReconciler struct {
	client.Client
	Config Config

	// mu protects cancelFuncs.
	mu          sync.Mutex
	cancelFuncs map[string]context.CancelFunc // key = node name
}

// SetupWithManager registers the controller and initialises internal state.
func (r *NodeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.cancelFuncs = make(map[string]context.CancelFunc)
	return ctrl.NewControllerManagedBy(mgr).
		For(&karpenterv1.NodeClaim{}).
		Owns(&corev1.Node{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=karpenter.sh,resources=nodeclaims,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=karpenter.sh,resources=nodeclaims/status,verbs=patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;patch;delete

func (r *NodeClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("nodeclaim", req.NamespacedName)

	nc := &karpenterv1.NodeClaim{}
	if err := r.Get(ctx, req.NamespacedName, nc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get NodeClaim: %w", err)
	}

	if !nc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, nc)
	}

	// Ensures cleanup happens before the NodeClaim is deleted
	if !controllerutil.ContainsFinalizer(nc, fakeNodeFinalizer) {
		controllerutil.AddFinalizer(nc, fakeNodeFinalizer)
		if err := r.Update(ctx, nc); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// Re-queue so we pick up the updated object.
		return ctrl.Result{Requeue: true}, nil
	}

	// Ensure fake Node
	nodeName := fakeNodeName(nc)
	node, err := r.ensureFakeNode(ctx, nc, nodeName)
	if err != nil {
		log.Error(err, "failed to ensure fake node")
		return ctrl.Result{}, err
	}

	// Patch Node status → Ready
	if err := r.ensureNodeReady(ctx, node, nc); err != nil {
		log.Error(err, "failed to patch node status to Ready")
		return ctrl.Result{}, err
	}

	// Ensure Lease and background renewal
	if err := r.ensureLease(ctx, nc, nodeName); err != nil {
		log.Error(err, "failed to ensure node lease")
		return ctrl.Result{}, err
	}
	r.ensureLeaseRenewer(ctx, nodeName)

	// Patch NodeClaim status so KAITO knows the node is ready
	if err := r.ensureNodeClaimReady(ctx, nc, nodeName); err != nil {
		log.Error(err, "failed to patch NodeClaim status")
		return ctrl.Result{}, err
	}

	log.Info("fake node is ready", "node", nodeName)
	return ctrl.Result{}, nil
}

// reconcileDelete removes the fake Node and Lease and cancels the renewal
// goroutine, then strips the finalizer so Karpenter can proceed.
func (r *NodeClaimReconciler) reconcileDelete(ctx context.Context, nc *karpenterv1.NodeClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	nodeName := fakeNodeName(nc)

	// Stop the lease renewer goroutine.
	r.stopLeaseRenewer(nodeName)

	// Delete the Lease.
	lease := &coordinationv1.Lease{}
	err := r.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: nodeName}, lease)
	if err == nil {
		if delErr := r.Delete(ctx, lease); delErr != nil && !errors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("delete lease: %w", delErr)
		}
	}

	// Delete the fake Node.
	node := &corev1.Node{}
	err = r.Get(ctx, types.NamespacedName{Name: nodeName}, node)
	if err == nil {
		if delErr := r.Delete(ctx, node); delErr != nil && !errors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("delete node: %w", delErr)
		}
	}

	// Strip finalizer.
	controllerutil.RemoveFinalizer(nc, fakeNodeFinalizer)
	if err := r.Update(ctx, nc); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	log.Info("cleaned up fake node resources", "node", nodeName)
	return ctrl.Result{}, nil
}

// ensureFakeNode creates the fake Node if it does not exist, or returns the
// existing one. The Node carries:
//   - providerID: "fake://<node-name>"  — un-parseable by Azure CCM
//   - label LabelExcludeLB              — suppresses CCM LB reconciliation
//   - label LabelFakeNode               — used by Phase 2 pod filter
//   - workspace label                   — required by InferenceSet selector
func (r *NodeClaimReconciler) ensureFakeNode(ctx context.Context, nc *karpenterv1.NodeClaim, nodeName string) (*corev1.Node, error) {
	existing := &corev1.Node{}
	err := r.Get(ctx, types.NamespacedName{Name: nodeName}, existing)
	if err == nil {
		return existing, nil
	}
	if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("get node: %w", err)
	}

	// Get workspace name from NodeClaim labels
	workspaceName := nc.Labels[LabelKaitoWorkspace]

	labels := map[string]string{
		LabelFakeNode:  "true",
		LabelManagedBy: ControllerName,
		LabelExcludeLB: "true",
		// Standard K8s labels required by KAITO's node readiness check.
		"kubernetes.io/os":   "linux",
		"kubernetes.io/arch": "amd64",
		// Propagate all original NodeClaim labels so the InferenceSet
		// labelSelector is satisfied.
	}
	// Get instance type from NodeClaim requirements so KAITO's
	// node readiness check can match the workspace instanceType.
	for _, req := range nc.Spec.Requirements {
		if req.Key == "node.kubernetes.io/instance-type" && len(req.Values) > 0 {
			labels["node.kubernetes.io/instance-type"] = req.Values[0]
			labels["beta.kubernetes.io/instance-type"] = req.Values[0]
			break
		}
	}
	for k, v := range nc.Labels {
		labels[k] = v
	}
	if workspaceName != "" {
		labels[LabelKaitoWorkspace] = workspaceName
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{
			// "fake://" prefix is intentionally not a valid Azure provider ID
			// format, so the Azure CCM's InstanceExistsByProviderID call
			// returns a parse error and the CCM skips the node.
			ProviderID: FakeProviderIDPrefix + nodeName,
			// Propagate taints from the NodeClaim (e.g. sku=gpu:NoSchedule)
			// so KAITO considers the node valid for the workspace.
			// Do NOT add extra taints — KAITO's inference pods only tolerate
			// the taints defined in the workspace spec. Any additional taint
			// would block pod scheduling.
			Taints: nc.Spec.Taints,
		},
	}

	// Set OwnerReference so .Owns(&corev1.Node{}) works and the fake node gets garbage collected if the NodeClaim is deleted.
	if err := controllerutil.SetControllerReference(nc, node, r.Client.Scheme()); err != nil {
		return nil, fmt.Errorf("set owner reference: %w", err)
	}

	if err := r.Create(ctx, node); err != nil {
		return nil, fmt.Errorf("create fake node: %w", err)
	}
	return node, nil
}

// ensureNodeReady patches the Node's status so that:
//   - conditions: Ready=True, MemoryPressure=False, DiskPressure=False, PIDPressure=False
//   - allocatable / capacity values mirror the NodeClaim resource requests
func (r *NodeClaimReconciler) ensureNodeReady(ctx context.Context, node *corev1.Node, nc *karpenterv1.NodeClaim) error {
	// Check whether the node is already Ready to avoid unnecessary patches.
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return nil
		}
	}

	now := metav1.Now()
	capacity := nodeCapacity()

	patch := client.MergeFrom(node.DeepCopy())
	node.Status = corev1.NodeStatus{
		Phase:       corev1.NodeRunning,
		Capacity:    capacity,
		Allocatable: capacity,
		Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: node.Name},
			{Type: corev1.NodeInternalIP, Address: "192.0.2.1"}, // TEST-NET, non-routable
		},
		NodeInfo: corev1.NodeSystemInfo{
			Architecture:            "amd64",
			OperatingSystem:         "linux",
			KernelVersion:           "5.15.0-fake",
			OSImage:                 "Fake Ubuntu 22.04",
			ContainerRuntimeVersion: "containerd://1.7.0-fake",
			KubeletVersion:          "v1.30.0-fake",
			KubeProxyVersion:        "v1.30.0-fake",
		},
		Conditions: []corev1.NodeCondition{
			makeCondition(corev1.NodeReady, corev1.ConditionTrue, "KubeletReady", "fake kubelet is ready", now),
			makeCondition(corev1.NodeMemoryPressure, corev1.ConditionFalse, "KubeletHasSufficientMemory", "ok", now),
			makeCondition(corev1.NodeDiskPressure, corev1.ConditionFalse, "KubeletHasNoDiskPressure", "ok", now),
			makeCondition(corev1.NodePIDPressure, corev1.ConditionFalse, "KubeletHasSufficientPID", "ok", now),
			makeCondition(corev1.NodeNetworkUnavailable, corev1.ConditionFalse, "RouteCreated", "ok", now),
		},
		DaemonEndpoints: corev1.NodeDaemonEndpoints{
			KubeletEndpoint: corev1.DaemonEndpoint{Port: 10250},
		},
	}

	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("patch node status: %w", err)
	}

	return nil
}

// ensureNodeClaimReady patches the NodeClaim's status subresource so KAITO
// sees the node as provisioned and proceeds to create inference pods.
// Fields set: nodeName, providerID, conditions[Ready=True], capacity, allocatable.
func (r *NodeClaimReconciler) ensureNodeClaimReady(ctx context.Context, nc *karpenterv1.NodeClaim, nodeName string) error {
	// Skip if already marked ready.
	if nc.Status.NodeName != "" {
		return nil
	}

	now := metav1.Now()
	capacity := nodeCapacity()

	patch := client.MergeFrom(nc.DeepCopy())
	nc.Status.NodeName = nodeName
	nc.Status.ProviderID = FakeProviderIDPrefix + nodeName
	nc.Status.Capacity = capacity
	nc.Status.Allocatable = capacity
	nc.Status.Conditions = []status.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "FakeNodeReady",
			Message:            "fake node is ready",
		},
		{
			Type:               "Launched",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "FakeNodeLaunched",
			Message:            "fake node launched",
		},
		{
			Type:               "Registered",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "FakeNodeRegistered",
			Message:            "fake node registered",
		},
		{
			Type:               "Initialized",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "FakeNodeInitialized",
			Message:            "fake node initialized",
		},
	}

	if err := r.Status().Patch(ctx, nc, patch); err != nil {
		return fmt.Errorf("patch NodeClaim status: %w", err)
	}
	return nil
}

// ensureLease creates (or updates) a kube-node-lease Lease for nodeName.
// The Lease is what the node-lifecycle-controller inspects; without a
// sufficiently recent renewTime the node transitions to Unknown.
func (r *NodeClaimReconciler) ensureLease(ctx context.Context, nc *karpenterv1.NodeClaim, nodeName string) error {
	now := metav1.NewMicroTime(time.Now())
	holderID := controllerName(nodeName)

	lease := &coordinationv1.Lease{}
	err := r.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: nodeName}, lease)
	if errors.IsNotFound(err) {
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nodeName,
				Namespace: "kube-node-lease",
				Labels: map[string]string{
					LabelManagedBy: ControllerName,
					LabelFakeNode:  "true",
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "karpenter.sh/v1",
					Kind:       "NodeClaim",
					Name:       nc.Name,
					UID:        nc.UID,
				}},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holderID,
				LeaseDurationSeconds: ptr.To(r.Config.LeaseDurationSec),
				RenewTime:            &now,
				AcquireTime:          &now,
			},
		}
		if createErr := r.Create(ctx, lease); createErr != nil {
			return fmt.Errorf("create lease: %w", createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get lease: %w", err)
	}

	// Lease already exists — ensure our fields are current.
	patch := client.MergeFrom(lease.DeepCopy())
	lease.Spec.HolderIdentity = &holderID
	lease.Spec.LeaseDurationSeconds = ptr.To(r.Config.LeaseDurationSec)
	lease.Spec.RenewTime = &now
	// Backfill OwnerReference for leases created before this change.
	hasRef := false
	for _, ref := range lease.OwnerReferences {
		if ref.Kind == "NodeClaim" && ref.Name == nc.Name {
			hasRef = true
			break
		}
	}
	if !hasRef {
		lease.OwnerReferences = append(lease.OwnerReferences, metav1.OwnerReference{
			APIVersion: "karpenter.sh/v1",
			Kind:       "NodeClaim",
			Name:       nc.Name,
			UID:        nc.UID,
		})
	}
	if patchErr := r.Patch(ctx, lease, patch); patchErr != nil {
		return fmt.Errorf("patch lease: %w", patchErr)
	}
	return nil
}

// ensureLeaseRenewer starts a background goroutine for nodeName if one is not
// already running. The goroutine updates the Lease.spec.renewTime every
// Config.LeaseRenewIntervalSec seconds so the node-lifecycle-controller never
// transitions the node to Unknown due to a stale heartbeat.
func (r *NodeClaimReconciler) ensureLeaseRenewer(parentCtx context.Context, nodeName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cancelFuncs[nodeName]; ok {
		return // already running
	}
	renewCtx, cancel := context.WithCancel(context.Background())
	r.cancelFuncs[nodeName] = cancel

	go func() {
		log := log.FromContext(parentCtx).WithValues("node", nodeName)
		ticker := time.NewTicker(time.Duration(r.Config.LeaseRenewIntervalSec) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				log.Info("lease renewer stopped")
				return
			case <-ticker.C:
				gone, err := r.renewLease(renewCtx, nodeName)
				if err != nil {
					log.Error(err, "failed to renew lease — will retry next tick")
				}
				if gone {
					log.Info("lease not found, stopping renewer")
					r.stopLeaseRenewer(nodeName)
					return
				}
			}
		}
	}()
}

// stopLeaseRenewer cancels the background goroutine for nodeName.
func (r *NodeClaimReconciler) stopLeaseRenewer(nodeName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel, ok := r.cancelFuncs[nodeName]; ok {
		cancel()
		delete(r.cancelFuncs, nodeName)
	}
}

// renewLease patches the renewTime on the kube-node-lease Lease.
// It returns true if the Lease was not found, signalling the caller to stop.
func (r *NodeClaimReconciler) renewLease(ctx context.Context, nodeName string) (gone bool, err error) {
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "kube-node-lease", Name: nodeName}, lease); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("get lease for renew: %w", err)
	}

	patch := client.MergeFrom(lease.DeepCopy())
	now := metav1.NewMicroTime(time.Now())
	lease.Spec.RenewTime = &now
	if err := r.Patch(ctx, lease, patch); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("patch renewTime: %w", err)
	}
	return false, nil
}

// fakeNodeName derives the fake Node name from the NodeClaim.
// Using the NodeClaim name directly keeps tracing straightforward.
func fakeNodeName(nc *karpenterv1.NodeClaim) string {
	return "fake-" + nc.Name
}

// controllerName returns a holder identity string for the Lease that includes
// the node name, useful when debugging via kubectl get leases -n kube-node-lease.
func controllerName(nodeName string) string {
	return ControllerName + "/" + nodeName
}

// nodeCapacity returns a fixed ResourceList with enough capacity for the
// scheduler to place KAITO inference pods on the fake node.
// The exact values don't matter — no real workload runs here
// Phase 2 redirects pods to shadow pods on real nodes.
// KAITO only populates spec.resources.requests with storage so CPU/memory/GPU are hardcoded.
func nodeCapacity() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:                    resource.MustParse("4"),
		corev1.ResourceMemory:                 resource.MustParse("16Gi"),
		corev1.ResourcePods:                   resource.MustParse("110"),
		corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
	}
}

// makeCondition constructs a NodeCondition with the given parameters.
func makeCondition(condType corev1.NodeConditionType, status corev1.ConditionStatus, reason, msg string, now metav1.Time) corev1.NodeCondition {
	return corev1.NodeCondition{
		Type:               condType,
		Status:             status,
		LastHeartbeatTime:  now,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            msg,
	}
}
