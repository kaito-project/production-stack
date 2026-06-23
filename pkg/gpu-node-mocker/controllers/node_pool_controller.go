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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// NodePoolReconciler implements the karpenter-mode counterpart of the karpenter
// engine. In karpenter mode KAITO creates a NodePool (with Spec.Replicas) and
// expects the karpenter engine to materialize that many NodeClaims. Since no
// real karpenter engine runs in the mock environment, this reconciler does it:
//
//	For each KAITO-managed NodePool it materializes Spec.Replicas NodeClaims
//	from the pool's template (labels, requirements, taints, nodeClassRef). Each
//	NodeClaim is owned by the NodePool so deleting the pool cascades to the
//	NodeClaims, which the NodeClaimReconciler then tears down (Node + Lease).
//
// The NodeClaimReconciler picks the materialized NodeClaims up and turns each
// one into a fake Node, exactly as in azure-gpu-provisioner mode.
type NodePoolReconciler struct {
	client.Client
	Config Config
}

// SetupWithManager registers the controller. It only reacts to NodePools that
// carry KAITO's managed-by label so it never touches user- or real-karpenter-
// managed pools.
func (r *NodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	managedByKaito := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[KarpenterManagedByLabel] == KarpenterManagedByValue
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&karpenterv1.NodePool{}, builder.WithPredicates(managedByKaito)).
		Owns(&karpenterv1.NodeClaim{}, builder.WithPredicates(predicate.Funcs{
			// Re-materialize NodeClaims if one is deleted while the pool still
			// wants it (e.g. NodeClaim GC'd but replicas unchanged).
			DeleteFunc: func(e event.DeleteEvent) bool { return true },
		})).
		Complete(r)
}

// +kubebuilder:rbac:groups=karpenter.sh,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.sh,resources=nodeclaims,verbs=get;list;watch;create;delete

func (r *NodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("nodepool", req.NamespacedName)

	np := &karpenterv1.NodePool{}
	if err := r.Get(ctx, req.NamespacedName, np); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get NodePool: %w", err)
	}

	// On deletion, owner-reference GC cascades to the materialized NodeClaims,
	// whose deletion is handled by the NodeClaimReconciler. Nothing to do here.
	if !np.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	desired := int(0)
	if np.Spec.Replicas != nil && *np.Spec.Replicas > 0 {
		desired = int(*np.Spec.Replicas)
	}

	// List the NodeClaims this pool already owns.
	owned := &karpenterv1.NodeClaimList{}
	if err := r.List(ctx, owned, client.MatchingLabels{LabelNodePool: np.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list NodeClaims for pool: %w", err)
	}

	// Count only NodeClaims that are not being deleted; a NodeClaim with a
	// deletion timestamp is on its way out and should not satisfy the replica
	// target.
	var live []*karpenterv1.NodeClaim
	for i := range owned.Items {
		if owned.Items[i].DeletionTimestamp.IsZero() {
			live = append(live, &owned.Items[i])
		}
	}
	current := len(live)

	switch {
	case current < desired:
		for i := 0; i < desired-current; i++ {
			nc, err := r.materializeNodeClaim(ctx, np)
			if err != nil {
				return ctrl.Result{}, err
			}
			log.Info("materialized NodeClaim from NodePool", "nodeclaim", nc.Name, "replicas", desired)
		}
	case current > desired:
		for i := 0; i < current-desired; i++ {
			victim := live[current-1-i]
			if err := r.Delete(ctx, victim); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete surplus NodeClaim %q: %w", victim.Name, err)
			}
			log.Info("deleted surplus NodeClaim", "nodeclaim", victim.Name, "replicas", desired)
		}
	}

	return ctrl.Result{}, nil
}

// materializeNodeClaim creates a single NodeClaim from the NodePool template,
// mirroring what the karpenter engine would produce: template labels plus the
// karpenter.sh/nodepool link label, an owner reference to the pool for GC
// cascade, and the template's requirements/taints/nodeClassRef/resources.
func (r *NodePoolReconciler) materializeNodeClaim(ctx context.Context, np *karpenterv1.NodePool) (*karpenterv1.NodeClaim, error) {
	tmpl := np.Spec.Template

	labels := map[string]string{}
	for k, v := range tmpl.Labels {
		labels[k] = v
	}
	// karpenter stamps the owning pool on every NodeClaim it launches; the
	// reconciler relies on it to list/count NodeClaims per pool.
	labels[LabelNodePool] = np.Name

	annotations := map[string]string{}
	for k, v := range tmpl.Annotations {
		annotations[k] = v
	}

	nc := &karpenterv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        np.Name + "-" + rand.String(5),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: karpenterv1.NodeClaimSpec{
			Taints:                 tmpl.Spec.Taints,
			StartupTaints:          tmpl.Spec.StartupTaints,
			Requirements:           tmpl.Spec.Requirements,
			NodeClassRef:           tmpl.Spec.NodeClassRef,
			TerminationGracePeriod: tmpl.Spec.TerminationGracePeriod,
			ExpireAfter:            tmpl.Spec.ExpireAfter,
		},
	}

	// Owner reference so deleting the NodePool cascades to its NodeClaims.
	if err := controllerutil.SetControllerReference(np, nc, r.Client.Scheme()); err != nil {
		return nil, fmt.Errorf("set owner reference: %w", err)
	}

	if err := r.Create(ctx, nc); err != nil {
		return nil, fmt.Errorf("create NodeClaim: %w", err)
	}
	return nc, nil
}
