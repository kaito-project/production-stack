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

import "k8s.io/apimachinery/pkg/runtime/schema"

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

	// AnnotationLatencyProfile lets an InferenceSet (via its pod template)
	// select the llm-d-inference-sim latency profile for its shadow pods.
	// Recognized values are "auto" (default — pick a profile from the model
	// size parsed out of the served model name) or one of the named profiles
	// ("small-l40s", "8b-h100", "13b", "30b-tp2", "70b-tp8", "405b-tp8").
	// Unknown values fall back to "auto".
	AnnotationLatencyProfile = "kaito.sh/latency-profile"

	// AnnotationLatencyCalculator lets an InferenceSet select the simulator
	// latency model per deployment: "per-token" (default) or "constant".
	// Unknown values fall back to the operator-wide default and are logged.
	AnnotationLatencyCalculator = "kaito.sh/latency-calculator"

	// FakeProviderIDPrefix is intentionally un-parseable by the Azure CCM so
	// InstanceExistsByProviderID returns an error and the CCM skips the node
	// rather than deleting it.
	FakeProviderIDPrefix = "fake://"

	// ShadowPodLabelKey marks shadow pods so we can watch only those.
	ShadowPodLabelKey = "kaito.sh/shadow-pod-for"

	// InferenceSetCreatedByLabelKey is the upstream KAITO label that the
	// InferenceSet controller stamps on its child pods. The Phase-2 pod
	// predicate reads it to recognize InferenceSet (modeldeployment) pods.
	InferenceSetCreatedByLabelKey = "inferenceset.kaito.sh/created-by"

	// OwnedByLabelKey / OwnedByLabelValue is the stable, value-free label
	// production-stack stamps on every pod it owns — EPP, KAITO-controller-
	// rendered inference pods, and shadow pods. The modelharness chart's
	// NetworkPolicies positively select on this label so they isolate
	// inference workloads without sweeping in user-deployed pods that happen
	// to share the workload namespace (issue #83). Without it, shadow pods
	// would not match `allow-inference-traffic` and EPP would be unable to
	// reach them via the patched inference-pod IP.
	OwnedByLabelKey   = "kaito.sh/owned-by"
	OwnedByLabelValue = "modeldeployment"

	// DefaultInferenceSimImage is the default llm-d inference simulator image.
	DefaultInferenceSimImage = "ghcr.io/llm-d/llm-d-inference-sim:v0.8.1"

	// DefaultUDSTokenizerImage is the default UDS tokenizer sidecar image.
	DefaultUDSTokenizerImage = "ghcr.io/llm-d/llm-d-uds-tokenizer:v0.6.0"

	// LatencyCalculatorConstant / LatencyCalculatorPerToken are the two
	// llm-d-inference-sim latency models. "constant" derives TTFT from a fixed
	// value; "per-token" derives it from prompt length via prefill overhead +
	// per-token cost. See llm-d-inference-sim/docs/latency-profiles.md.
	//
	// Individual latency knob defaults are not defined here: when a Config knob
	// is empty the value comes from the model-size latency profile selected in
	// ensureSimConfigMap (see latency_profiles.go).
	LatencyCalculatorConstant = "constant"
	LatencyCalculatorPerToken = "per-token"
	DefaultLatencyCalculator  = LatencyCalculatorPerToken

	// InferenceSimPort is the default port for the inference simulator.
	InferenceSimPort = 8001

	// UDSTokenizerProbePort is the health probe port for the UDS tokenizer.
	UDSTokenizerProbePort = 8082

	// DefaultModelName is used when model name cannot be extracted from the original pod.
	DefaultModelName = "default-model"

	// MaxLabelValueLength is the maximum length of a Kubernetes label value.
	MaxLabelValueLength = 63

	// LabelNodePool is the standard karpenter label that links a NodeClaim to
	// the NodePool it was provisioned from.
	LabelNodePool = "karpenter.sh/nodepool"

	// KarpenterManagedByLabel / KarpenterManagedByValue mirror the labels KAITO
	// stamps on the NodePool it creates in karpenter mode.
	KarpenterManagedByLabel = "karpenter.kaito.sh/managed-by"
	KarpenterManagedByValue = "kaito"

	// MockNodeClass* identify the gpu-node-mocker's own karpenter NodeClass
	// kind. It lives in the karpenter.kaito.sh group — NOT karpenter.azure.com —
	// so a real Azure Karpenter provider (which only understands
	// karpenter.azure.com AKSNodeClass) does not recognize it and therefore
	// skips any KAITO NodePool whose nodeClassRef points at it. That lets the
	// mocker run alongside a real karpenter install: the mocker reconciles the
	// MockNodeClass (marks it Ready, materializes fake NodeClaims) while real
	// karpenter ignores the same NodePool. It is the default NodeClass the
	// mocker watches.
	MockNodeClassGroup    = "karpenter.kaito.sh"
	MockNodeClassVersion  = "v1alpha1"
	MockNodeClassKind     = "MockNodeClass"
	MockNodeClassResource = "mocknodeclasses"
)

// NodeClassRef identifies the cluster-scoped karpenter NodeClass resource that
// KAITO's NodePool template references and that the NodeClassReconciler marks
// Ready in karpenter mode. It is configurable so the mocker can target either
// its own mock node class (karpenter.kaito.sh/MockNodeClass — the default) or
// the real karpenter.azure.com AKSNodeClass.
type NodeClassRef struct {
	Group    string
	Version  string
	Kind     string
	Resource string
}

// DefaultNodeClassRef returns the gpu-node-mocker mock node class, the kind a
// real karpenter provider does not recognize.
func DefaultNodeClassRef() NodeClassRef {
	return NodeClassRef{
		Group:    MockNodeClassGroup,
		Version:  MockNodeClassVersion,
		Kind:     MockNodeClassKind,
		Resource: MockNodeClassResource,
	}
}

// GroupVersion returns the discovery group/version string, e.g.
// "karpenter.kaito.sh/v1alpha1".
func (n NodeClassRef) GroupVersion() string { return n.Group + "/" + n.Version }

// GVK returns the GroupVersionKind for the NodeClass.
func (n NodeClassRef) GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: n.Group, Version: n.Version, Kind: n.Kind}
}

// Config holds operator-wide settings injected via CLI flags.
type Config struct {
	// ShadowPodImage is the inference simulator container image.
	ShadowPodImage string

	// UDSTokenizerImage is the UDS tokenizer sidecar image.
	UDSTokenizerImage string

	// TimeToFirstToken / InterTokenLatency tune the llm-d-inference-sim
	// "constant" latency calculator (values include a unit suffix, e.g.
	// "100ms"). See llm-d-inference-sim/docs/latency-profiles.md.
	TimeToFirstToken  string
	InterTokenLatency string

	// Latency jitter and load-scaling knobs for the "constant" calculator.
	// StdDev values add jitter; TimeFactorUnderLoad scales latency as
	// concurrency approaches max-num-seqs; KVCacheTransfer* model prefill-cache
	// transfer overhead. See llm-d-inference-sim/docs/latency-profiles.md.
	TimeToFirstTokenStdDev  string
	InterTokenLatencyStdDev string
	KVCacheTransferLatency  string
	KVCacheTransferStdDev   string
	TimeFactorUnderLoad     string

	// LatencyCalculator selects the simulator latency model: "per-token"
	// (default) or "constant". The per-token model derives TTFT from prompt
	// length and uses the Prefill*/KVCacheTransferTime* knobs below instead of
	// TimeToFirstToken / KVCacheTransferLatency.
	LatencyCalculator string

	// Per-token calculator knobs. Used only when LatencyCalculator is
	// "per-token". See llm-d-inference-sim/docs/latency-profiles.md.
	PrefillOverhead             string
	PrefillTimePerToken         string
	PrefillTimeStdDev           string
	KVCacheTransferTimePerToken string
	KVCacheTransferTimeStdDev   string

	// LeaseDurationSec is the Lease.spec.leaseDurationSeconds written to the
	// kube-node-lease Lease for each fake node.
	LeaseDurationSec int32

	// LeaseRenewIntervalSec controls how often the background goroutine
	// refreshes each lease's renewTime.
	LeaseRenewIntervalSec int

	// NodeClass is the karpenter NodeClass GVK the mocker reconciles (and
	// discovery-checks) in karpenter mode. Defaults to the mock node class.
	NodeClass NodeClassRef
}
