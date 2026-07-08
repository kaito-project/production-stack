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

// Package reason defines the closed control-plane reason vocabulary
// published by the productionstack-status-reporter as Kubernetes Events in
// the kube-system namespace, together with the cross-layer upstream-gating
// suppression table (§1.4) of the end-to-end error-handling proposal.
//
// The reporter is the SINGLE producer of these reasons; emitting any of
// them from another component is forbidden by the proposal.
package reason

import (
	corev1 "k8s.io/api/core/v1"
)

// Layer identifies which of the three production-stack layers owns a reason.
type Layer string

const (
	// LayerCluster covers umbrella-chart subchart and shared controller
	// readiness (BBR, KEDA, Istio, KAITO, gateway-auth, node provisioner, CRDs).
	LayerCluster Layer = "cluster"
	// LayerModelharness covers per-workload-namespace harness objects.
	LayerModelharness Layer = "modelharness"
	// LayerModeldeployment covers per-InferenceSet model deployment objects.
	LayerModeldeployment Layer = "modeldeployment"
)

// Reason is one stable string from the §1.2 reason catalogue. The string
// itself is layer-prefixed so each value is globally unique and maps
// unambiguously to a TSG-1 anchor.
type Reason string

// Cluster-layer reasons (§1.2).
const (
	ClusterCRDMissing                Reason = "clusterCRDMissing"
	ClusterIstioControlPlaneNotReady Reason = "clusterIstioControlPlaneNotReady"
	ClusterGatewayAuthNotReady       Reason = "clusterGatewayAuthNotReady"
	ClusterBBRNotReady               Reason = "clusterBBRNotReady"
	ClusterKaitoControllerNotReady   Reason = "clusterKaitoControllerNotReady"
	ClusterNodeProvisionerNotReady   Reason = "clusterNodeProvisionerNotReady"
	ClusterKedaNotReady              Reason = "clusterKedaNotReady"
	ClusterKedaKaitoScalerNotReady   Reason = "clusterKedaKaitoScalerNotReady"
)

// Modelharness-layer reasons (§1.2).
const (
	ModelharnessGatewayClassMissing      Reason = "modelharnessGatewayClassMissing"
	ModelharnessGatewayProgrammingFailed Reason = "modelharnessGatewayProgrammingFailed"
	ModelharnessGatewayDataPlaneNotReady Reason = "modelharnessGatewayDataPlaneNotReady"
	ModelharnessExtAuthzProviderMissing  Reason = "modelharnessExtAuthzProviderMissing"
	ModelharnessAPIKeyReconcileFailed    Reason = "modelharnessAPIKeyReconcileFailed"
	ModelharnessEnvoyFilterMissing       Reason = "modelharnessEnvoyFilterMissing"
	ModelharnessNetworkPolicyMissing     Reason = "modelharnessNetworkPolicyMissing"
)

// Modeldeployment-layer reasons (§1.2).
const (
	InferencesetInfraProvisioningFailed Reason = "inferencesetInfraProvisioningFailed"
	InferencesetModelPodsNotReady       Reason = "inferencesetModelPodsNotReady"
	InferencesetEPPNotReady             Reason = "inferencesetEPPNotReady"
	InferencesetRouteNotReady           Reason = "inferencesetRouteNotReady"
	InferencesetWeightDownloadSlow      Reason = "inferencesetWeightDownloadSlow"
)

// EventType returns the Kubernetes event type for a reason. The reporter only
// publishes problems, so every reason is a Warning (§1.1); a healthy state is
// the absence of a Warning, not a positive event.
func EventType(Reason) string {
	return corev1.EventTypeWarning
}

// LayerOf returns the layer that owns the reason.
func LayerOf(r Reason) Layer {
	switch r {
	case ClusterCRDMissing, ClusterIstioControlPlaneNotReady, ClusterGatewayAuthNotReady,
		ClusterBBRNotReady, ClusterKaitoControllerNotReady, ClusterNodeProvisionerNotReady,
		ClusterKedaNotReady, ClusterKedaKaitoScalerNotReady:
		return LayerCluster
	case ModelharnessGatewayClassMissing, ModelharnessGatewayProgrammingFailed,
		ModelharnessGatewayDataPlaneNotReady,
		ModelharnessExtAuthzProviderMissing, ModelharnessAPIKeyReconcileFailed,
		ModelharnessEnvoyFilterMissing, ModelharnessNetworkPolicyMissing:
		return LayerModelharness
	default:
		return LayerModeldeployment
	}
}
