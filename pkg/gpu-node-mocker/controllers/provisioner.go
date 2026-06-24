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
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Supported node provisioner identifiers. The values intentionally match
// KAITO's own `--node-provisioner` flag so the mocker can be configured with
// the exact same string KAITO is running with.
const (
	ProvisionerAzureGPU  = "azure-gpu-provisioner"
	ProvisionerKarpenter = "karpenter"
)

// RequiredCRD describes one API resource that must already be registered with
// the apiserver before a mocker's controllers can start. It is validated via
// discovery at startup; a missing resource causes the process to exit so the
// Deployment's restart policy backs off until KAITO installs the CRD.
type RequiredCRD struct {
	// GroupVersion is the discovery group/version string, e.g. "karpenter.sh/v1".
	GroupVersion string
	// Resource is the lowercase plural resource name, e.g. "nodeclaims".
	Resource string
}

// ProvisionerMocker abstracts a node-provisioner-specific mocking strategy.
//
// Each implementation wires up the Phase-1 controllers required to fake node
// provisioning for one node provisioner. It translates the object KAITO really
// creates (a NodeClaim for azure-gpu-provisioner, a NodePool for karpenter)
// into a fake Node + Lease with patched status so KAITO believes a GPU node is
// ready. The Phase-2 ShadowPodReconciler is provisioner-agnostic and is
// registered separately by the caller, so it is not part of this interface.
type ProvisionerMocker interface {
	// Type returns the provisioner identifier this mocker handles
	// (ProvisionerAzureGPU or ProvisionerKarpenter).
	Type() string

	// RequiredCRDs returns the API resources that must exist before this
	// mocker's controllers can start.
	RequiredCRDs() []RequiredCRD

	// SetupWithManager registers all controllers this provisioner needs.
	SetupWithManager(mgr ctrl.Manager) error
}

// NewProvisionerMocker constructs the mocker implementation for the given
// provisioner type. Construction is manager-free so callers can validate the
// type and inspect RequiredCRDs before a manager exists; the client is taken
// from the manager in SetupWithManager.
func NewProvisionerMocker(provisionerType string, cfg Config) (ProvisionerMocker, error) {
	switch provisionerType {
	case ProvisionerAzureGPU:
		return &GPUProvisionerMocker{Config: cfg}, nil
	case ProvisionerKarpenter:
		return &KarpenterMocker{Config: cfg}, nil
	default:
		return nil, fmt.Errorf("unsupported node provisioner %q (must be %q or %q)",
			provisionerType, ProvisionerAzureGPU, ProvisionerKarpenter)
	}
}

// GPUProvisionerMocker mocks the Azure gpu-provisioner. KAITO creates the
// NodeClaim objects directly, so this mocker only needs the NodeClaimReconciler
// to turn each NodeClaim into a fake Node.
type GPUProvisionerMocker struct {
	Config Config
}

func (m *GPUProvisionerMocker) Type() string { return ProvisionerAzureGPU }

func (m *GPUProvisionerMocker) RequiredCRDs() []RequiredCRD {
	return []RequiredCRD{
		{GroupVersion: "karpenter.sh/v1", Resource: "nodeclaims"},
	}
}

func (m *GPUProvisionerMocker) SetupWithManager(mgr ctrl.Manager) error {
	if err := (&NodeClaimReconciler{
		Client: mgr.GetClient(),
		Config: m.Config,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create NodeClaim controller: %w", err)
	}
	return nil
}

// KarpenterMocker mocks the karpenter provisioner. KAITO creates a NodePool
// (with Spec.Replicas) and relies on the karpenter engine to materialize
// NodeClaims from it. Since no real karpenter engine runs in the mock
// environment, three reconcilers stand in for it:
//
//   - NodeClassReconciler marks the NodeClass KAITO references Ready, so
//     KAITO stops blocking and creates the NodePool.
//   - NodePoolReconciler materializes Spec.Replicas NodeClaims from the pool.
//   - the shared NodeClaimReconciler turns each NodeClaim into a fake Node.
type KarpenterMocker struct {
	Config Config
}

func (m *KarpenterMocker) Type() string { return ProvisionerKarpenter }

func (m *KarpenterMocker) RequiredCRDs() []RequiredCRD {
	return []RequiredCRD{
		// nodeclaims is shipped by KAITO; nodepools and the NodeClass CRD are
		// shipped by this chart's crds/ directory (upstream KAITO installs only
		// nodeclaims, and no real karpenter provider runs in the mock to install
		// NodePool/NodeClass). KAITO references the NodeClass from the NodePool
		// template and blocks until it is Ready.
		{GroupVersion: "karpenter.sh/v1", Resource: "nodeclaims"},
		{GroupVersion: "karpenter.sh/v1", Resource: "nodepools"},
		{GroupVersion: m.Config.NodeClass.GroupVersion(), Resource: m.Config.NodeClass.Resource},
	}
}

func (m *KarpenterMocker) SetupWithManager(mgr ctrl.Manager) error {
	// Phase 1-pre: NodeClass -> Ready (mimics the karpenter provider). KAITO
	// blocks node provisioning until the referenced NodeClass is Ready, so this
	// must run for the NodePool to ever be created.
	if err := (&NodeClassReconciler{
		Client: mgr.GetClient(),
		Config: m.Config,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create NodeClass controller: %w", err)
	}
	// Phase 1a: NodePool -> NodeClaim materialization (mimics karpenter engine).
	if err := (&NodePoolReconciler{
		Client: mgr.GetClient(),
		Config: m.Config,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create NodePool controller: %w", err)
	}
	// Phase 1b: NodeClaim -> fake Node (shared with gpu-provisioner mode).
	if err := (&NodeClaimReconciler{
		Client: mgr.GetClient(),
		Config: m.Config,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create NodeClaim controller: %w", err)
	}
	return nil
}
