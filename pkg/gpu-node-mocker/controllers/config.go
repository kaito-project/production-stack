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

// Package controllers contains the Phase 1 and Phase 2 reconcilers that
// implement the Shadow Pod lifecycle:
//
//	Phase 1 — NodeClaimReconciler
//	  Watches Karpenter NodeClaim objects. For each new NodeClaim it creates a
//	  fake Node (providerID = "fake://<name>" so the Azure CCM skips it),
//	  patches the Node's status to Ready, and keeps a kube-node-lease Lease
//	  renewed every LeaseRenewIntervalSec seconds so the node-lifecycle-
//	  controller never marks the node Unknown. Once the fake node carries the
//	  workspace label required by the InferenceSet labelSelector, KAITO flips
//	  ResourceReady=True.
//
//	Phase 2 — ShadowPodReconciler
//	  Watches Pods. When a Pod is bound (spec.nodeName set) to a fake node and
//	  is still Pending, it creates a "shadow pod" on a real AKS worker node
//	  running the LLM Mocker container. Once the shadow pod is Running the
//	  reconciler patches the original pod's status (phase, podIP, conditions,
//	  containerStatuses) with the shadow pod's IP, making KAITO believe the
//	  inference pod is Running/Ready. Traffic forwarded to that IP hits the
//	  real shadow pod and the LLM Mocker.
package controllers

const (
	// LabelFakeNode is set on every Node created by Phase 1 so Phase 2 can
	// cheaply filter pods assigned to fake nodes without re-fetching nodes.
	LabelFakeNode = "kaito.sh/fake-node"

	// LabelManagedBy identifies all resources owned by this controller.
	LabelManagedBy = "kaito.sh/managed-by"
	ControllerName = "gpu-mocker"

	// LabelKaitoWorkspace is the workspace label required by KAITO's
	// InferenceSet selector.
	LabelKaitoWorkspace = "kaito.sh/workspace"

	// LabelExcludeLB prevents the Azure CCM from attempting LB reconciliation
	// for the fake node, which would fail and flood the event stream.
	LabelExcludeLB = "node.kubernetes.io/exclude-from-external-load-balancers"

	// AnnotationShadowPodRef is stored on the original (pending) pod to track
	// which shadow pod mirrors it, enabling idempotent reconciliation.
	AnnotationShadowPodRef = "kaito.sh/shadow-pod-ref"

	// FakeProviderIDPrefix is intentionally un-parseable by the Azure CCM so
	// InstanceExistsByProviderID returns an error and the CCM skips the node
	// rather than deleting it.
	FakeProviderIDPrefix = "fake://"

	// ShadowPodLabelKey marks shadow pods so we can watch only those.
	ShadowPodLabelKey = "kaito.sh/shadow-pod-for"

	// DefaultInferenceSimImage is the default llm-d inference simulator image.
	DefaultInferenceSimImage = "ghcr.io/llm-d/llm-d-inference-sim:v0.8.1"

	// DefaultUDSTokenizerImage is the default UDS tokenizer sidecar image.
	DefaultUDSTokenizerImage = "ghcr.io/llm-d/llm-d-uds-tokenizer:v0.6.0"

	// InferenceSimPort is the default port for the inference simulator.
	InferenceSimPort = 8001

	// UDSTokenizerProbePort is the health probe port for the UDS tokenizer.
	UDSTokenizerProbePort = 8082

	// DefaultModelName is used when model name cannot be extracted from the original pod.
	DefaultModelName = "default-model"
)

// Config holds operator-wide settings injected via CLI flags.
type Config struct {
	// ShadowPodImage is the inference simulator container image.
	ShadowPodImage string

	// UDSTokenizerImage is the UDS tokenizer sidecar image.
	UDSTokenizerImage string

	// LeaseDurationSec is the Lease.spec.leaseDurationSeconds written to the
	// kube-node-lease Lease for each fake node.
	LeaseDurationSec int32

	// LeaseRenewIntervalSec controls how often the background goroutine
	// refreshes each lease's renewTime.
	LeaseRenewIntervalSec int
}
