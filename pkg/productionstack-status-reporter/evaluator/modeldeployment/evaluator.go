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

// Package modeldeployment implements the modeldeployment-layer Evaluator: it
// enumerates the InferenceSets in every managed namespace and evaluates every
// §1.2 modeldeployment chain reason for each.
package modeldeployment

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
	"github.com/kaito-project/production-stack/pkg/util/kube"
)

// workspaceGVR is the KAITO Workspace type the modeldeployment evaluator reads
// read-only. It is not vendored, so it is consumed via the dynamic client as
// unstructured.
var workspaceGVR = schema.GroupVersionResource{Group: "kaito.sh", Version: "v1beta1", Resource: "workspaces"}

// Workspace status condition types and reasons surfaced by the KAITO Workspace
// controller, consumed to derive the InferenceSet's infra and model-pod health.
const (
	// workspaceCondNodeClaimReady carries GPU node provisioning health: KAITO
	// sets it False with the underlying cloud-provider error (quota, capacity,
	// SKU unavailable) copied from the backing NodeClaim.
	workspaceCondNodeClaimReady = "NodeClaimReady"
	// workspaceCondInferenceReady carries model-server readiness: KAITO sets it
	// False with a classified pod/container failure reason once it detects an
	// actionable failure.
	workspaceCondInferenceReady = "InferenceReady"

	// workspaceCondNodesReady carries GPU worker-node health. KAITO appends a
	// node-pressure warning to its message (leaving the status unchanged) when a
	// worker node reports kubelet resource pressure.
	workspaceCondNodesReady = "NodesReady"

	// workspaceInferencePendingReason is the generic InferenceReady=False reason
	// KAITO sets while the inference workload is still starting within its
	// StartupProbe budget; any other reason is a classified pod/container failure.
	workspaceInferencePendingReason = "WorkspaceInferenceStatusPending"

	// nodePressureMarker is the stable phrase KAITO appends to the NodesReady
	// condition message (via NodePressureWarning) when a worker node reports
	// kubelet resource pressure (Disk/Memory/PID). KAITO surfaces it only in the
	// message text, so this substring is the detection contract.
	nodePressureMarker = "under resource pressure"
)

// benignNodeClaimReasons are the NodeClaimReady=False reasons that mean the GPU
// node is still being provisioned (no actionable cloud-provider error yet), so
// they are NOT surfaced as inferencesetInfraProvisioningFailed. Any other
// NodeClaimReady=False reason carries a real provisioning error KAITO copied
// from the underlying NodeClaim.
var benignNodeClaimReasons = map[string]bool{
	"NodeClaimNotReady":      true, // generic "not enough NodeClaims ready yet"
	"AwaitingReconciliation": true, // karpenter has not acted on the NodeClaim yet
}

// infraProvisioningGracePeriod debounces a NodeClaimReady=False provisioning
// failure: karpenter re-attempts VM creation with alternative SKUs/zones, so a
// transient capacity/quota blip clears within minutes and must not alarm, while
// a genuine shortage (e.g. zero quota) persists past this window and is
// surfaced. The finding is anchored on the condition's lastTransitionTime.
const infraProvisioningGracePeriod = 3 * time.Minute

// nodePressureGracePeriod is a short debounce for inferencesetNodeUnderPressure:
// node pressure is a leading indicator that should surface quickly, and kubelet's
// eviction-pressure-transition-period already keeps the node condition sticky for
// minutes (so no long debounce is needed for anti-flap). Kept below the resync
// interval so a one-off reading is merely confirmed on the next reconcile.
const nodePressureGracePeriod = 30 * time.Second

// Evaluator enumerates the InferenceSets in every managed namespace and
// evaluates every modeldeployment-layer chain reason (§1.2) for each. The
// orthogonal inferencesetWeightDownloadSlow reason is evaluated separately by
// the weightdownload Evaluator.
type Evaluator struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

// New constructs a modeldeployment Evaluator.
func New(cs kubernetes.Interface, dyn dynamic.Interface) *Evaluator {
	return &Evaluator{clientset: cs, dynamic: dyn}
}

// Name identifies the evaluator for logging.
func (e *Evaluator) Name() string { return "modeldeployment" }

// Evaluate enumerates the InferenceSets across all managed namespaces and
// evaluates every modeldeployment-layer chain reason for each. It returns an
// error only when namespace discovery fails.
func (e *Evaluator) Evaluate(ctx context.Context) ([]evaluator.Finding, error) {
	namespaces, err := util.DiscoverNamespaces(ctx, e.clientset)
	if err != nil {
		return nil, err
	}
	var findings []evaluator.Finding
	for _, ns := range namespaces {
		sets, err := e.dynamic.Resource(util.InferenceSetGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range sets.Items {
			findings = append(findings, e.evaluateInferenceSet(ctx, ns, &sets.Items[i])...)
		}
	}
	return findings, nil
}

func (e *Evaluator) evaluateInferenceSet(ctx context.Context, namespace string, is *unstructured.Unstructured) []evaluator.Finding {
	cs := e.clientset
	dyn := e.dynamic
	name := is.GetName()
	obj := evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: namespace}
	groupKey := fmt.Sprintf("inferenceset/%s/%s", namespace, name)
	var findings []evaluator.Finding

	add := func(r reason.Reason, exempt bool, createdAt time.Time, msg string) {
		findings = append(findings, evaluator.Finding{
			Reason:             r,
			Object:             obj,
			Message:            msg,
			WorkloadNamespace:  namespace,
			GroupKey:           groupKey,
			StartupGraceExempt: exempt,
			ResourceCreatedAt:  createdAt,
		})
	}

	// The child Workspaces' status conditions are the authoritative source for
	// both the GPU-node (infra) and model-pod health: the KAITO Workspace
	// controller already probes the underlying NodeClaims and pods and copies the
	// classified failure onto its NodeClaimReady / InferenceReady conditions, so
	// the reporter reads those conditions instead of re-deriving them from the
	// NodeClaim and Pod objects directly.
	workspaces := getWorkspaces(ctx, dyn, namespace, name)

	// inferencesetInfraProvisioningFailed — GPU node provisioning failure,
	// read from the child Workspace's NodeClaimReady condition. KAITO sets it
	// False and copies the underlying cloud-provider provisioning error (quota,
	// capacity, SKU unavailable) from the backing NodeClaim; a still-provisioning
	// Workspace keeps a benign in-progress reason and is not surfaced. Debounced
	// via infraProvisioningGracePeriod (anchored on the condition transition) so
	// a transient blip that karpenter recovers from stays silent.
	if f, ok := infraProvisioningFinding(workspaces, namespace, name, obj, groupKey); ok {
		findings = append(findings, f)
	}

	// inferencesetModelPodsNotReady — model-server pod failure, read from the
	// child Workspace's InferenceReady condition. KAITO sets it False with a
	// classified pod/container failure reason (ImagePullError, CrashLoopBackOff,
	// OOMKilled, Unschedulable, ...) once it detects an actionable failure, and
	// leaves the generic pending reason while the pod is merely still starting
	// within its StartupProbe budget. Only a classified failure is surfaced, and
	// it is surfaced immediately since a settled classification is a real failure.
	if f, ok := modelPodFinding(workspaces, namespace, name, obj, groupKey); ok {
		findings = append(findings, f)
	}

	// inferencesetNodeUnderPressure — a worker GPU node reports kubelet resource
	// pressure (Disk/Memory/PID), read from the child Workspace's NodesReady
	// condition message (KAITO enriches it there without changing the status). A
	// leading indicator: sustained pressure precedes pod eviction, which then
	// surfaces separately as inferencesetModelPodsNotReady. Only a short debounce
	// is applied so it surfaces promptly; kubelet's transition-period already
	// keeps the node condition from flapping.
	if f, ok := nodePressureFinding(workspaces, namespace, name, obj, groupKey); ok {
		findings = append(findings, f)
	}

	// inferencesetEPPNotReady — EPP Deployment readiness.
	if eppName, eppCreatedAt, msg := eppNotReady(ctx, cs, namespace, name); msg != "" {
		add(reason.InferencesetEPPNotReady, false, eppCreatedAt, fmt.Sprintf(
			"InferenceSet %s/%s: EPP Deployment %s %s.", namespace, name, eppName, msg))
	}

	return findings
}

// wsCondition is a Workspace status condition flattened for the reporter, with
// lastTransitionTime parsed for debounce anchoring.
type wsCondition struct {
	status         string
	reason         string
	message        string
	lastTransition time.Time
}

// workspaceCondition extracts status.conditions[type] from a Workspace,
// returning the flattened condition and whether it was present.
func workspaceCondition(ws *unstructured.Unstructured, condType string) (wsCondition, bool) {
	conds, ok, _ := unstructured.NestedSlice(ws.Object, "status", "conditions")
	if !ok {
		return wsCondition{}, false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != condType {
			continue
		}
		out := wsCondition{}
		out.status, _ = m["status"].(string)
		out.reason, _ = m["reason"].(string)
		out.message, _ = m["message"].(string)
		if ts, _ := m["lastTransitionTime"].(string); ts != "" {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				out.lastTransition = parsed
			}
		}
		return out, true
	}
	return wsCondition{}, false
}

// infraProvisioningFinding derives an inferencesetInfraProvisioningFailed
// finding from the child Workspaces' NodeClaimReady condition, which KAITO sets
// False with the underlying cloud-provider provisioning error (quota, capacity,
// SKU unavailable) copied from the backing NodeClaim. A still-provisioning
// Workspace (a benign in-progress reason) is not surfaced. The finding is
// anchored on the condition's lastTransitionTime and debounced via
// infraProvisioningGracePeriod so a transient blip that karpenter recovers from
// stays silent while a genuine shortage that persists past the window surfaces.
func infraProvisioningFinding(workspaces []*unstructured.Unstructured, namespace, name string, obj evaluator.InvolvedObject, groupKey string) (evaluator.Finding, bool) {
	for _, ws := range workspaces {
		cond, ok := workspaceCondition(ws, workspaceCondNodeClaimReady)
		if !ok || cond.status != "False" || benignNodeClaimReasons[cond.reason] {
			continue
		}
		return evaluator.Finding{
			Reason:              reason.InferencesetInfraProvisioningFailed,
			Object:              obj,
			Message:             fmt.Sprintf("InferenceSet %s/%s: GPU node provisioning failed — Workspace %s NodeClaimReady=False (%s): %s.", namespace, name, ws.GetName(), cond.reason, cond.message),
			WorkloadNamespace:   namespace,
			GroupKey:            groupKey,
			ResourceCreatedAt:   cond.lastTransition,
			GracePeriodOverride: infraProvisioningGracePeriod,
		}, true
	}
	return evaluator.Finding{}, false
}

// modelPodFinding derives an inferencesetModelPodsNotReady finding from the
// child Workspaces' InferenceReady condition. KAITO sets it False with a
// classified pod/container failure reason (ImagePullError, CrashLoopBackOff,
// OOMKilled, Unschedulable, ...) once it detects an actionable failure, and
// leaves the generic pending reason while the pod is merely still starting
// within its StartupProbe budget. Only a classified failure is surfaced, and it
// is surfaced immediately since a settled classification is a real failure
// regardless of the startup budget.
func modelPodFinding(workspaces []*unstructured.Unstructured, namespace, name string, obj evaluator.InvolvedObject, groupKey string) (evaluator.Finding, bool) {
	for _, ws := range workspaces {
		cond, ok := workspaceCondition(ws, workspaceCondInferenceReady)
		if !ok || cond.status != "False" || cond.reason == "" || cond.reason == workspaceInferencePendingReason {
			continue
		}
		return evaluator.Finding{
			Reason:             reason.InferencesetModelPodsNotReady,
			Object:             obj,
			Message:            fmt.Sprintf("InferenceSet %s/%s: model pod not ready — Workspace %s InferenceReady=False (%s): %s.", namespace, name, ws.GetName(), cond.reason, cond.message),
			WorkloadNamespace:  namespace,
			GroupKey:           groupKey,
			StartupGraceExempt: true,
		}, true
	}
	return evaluator.Finding{}, false
}

// nodePressureFinding derives an inferencesetNodeUnderPressure finding from the
// child Workspaces' NodesReady condition message: KAITO appends a
// "...under resource pressure: <node> (DiskPressure, ...)" warning there (via
// NodePressureWarning) while leaving the condition status unchanged. It is a
// leading indicator — sustained pressure precedes pod eviction — so it is
// surfaced with only a short debounce (kubelet's transition-period keeps the
// node condition from flapping on its own).
func nodePressureFinding(workspaces []*unstructured.Unstructured, namespace, name string, obj evaluator.InvolvedObject, groupKey string) (evaluator.Finding, bool) {
	for _, ws := range workspaces {
		cond, ok := workspaceCondition(ws, workspaceCondNodesReady)
		if !ok || !strings.Contains(cond.message, nodePressureMarker) {
			continue
		}
		detail := cond.message
		if i := strings.Index(detail, "warning: "); i >= 0 {
			detail = detail[i+len("warning: "):]
		}
		return evaluator.Finding{
			Reason:              reason.InferencesetNodeUnderPressure,
			Object:              obj,
			Message:             fmt.Sprintf("InferenceSet %s/%s: %s; model pods may be evicted if the pressure persists.", namespace, name, detail),
			WorkloadNamespace:   namespace,
			GroupKey:            groupKey,
			GracePeriodOverride: nodePressureGracePeriod,
		}, true
	}
	return evaluator.Finding{}, false
}

// getWorkspaces returns the KAITO Workspaces created by the InferenceSet,
// selected via the inferenceset.kaito.sh/created-by label (child Workspace
// names are assigned by the KAITO controller and do not match the
// InferenceSet name). Returns nil when unavailable.
func getWorkspaces(ctx context.Context, dyn dynamic.Interface, namespace, name string) []*unstructured.Unstructured {
	list, err := dyn.Resource(workspaceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", evaluator.LabelCreatedBy, name),
	})
	if err != nil {
		return nil
	}
	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out
}

// eppNotReady checks the EPP Deployment for an InferenceSet by delegating to
// the shared kube.DeploymentPodUnavailable probe. The EPP Deployment name is
// deterministic: charts/modeldeployment names it `<inferenceset>-inferencepool-epp`
// (the modeldeployment.eppServiceName helper) and the InferenceSet name equals
// the chart's deployment name, so it is derived directly rather than via a label
// list (which could match more than one Deployment). It returns the Deployment
// name, a debounce anchor for the startup grace gate (the pod's NotReady
// transition; zero when the Deployment is missing, so the emit falls back to the
// reason-level debounce), and a message predicate when the Deployment is missing
// or its pod is not ready. A missing Deployment is surfaced too: without the EPP
// the InferencePool has no endpoint picker and routing is broken.
func eppNotReady(ctx context.Context, cs kubernetes.Interface, namespace, name string) (string, time.Time, string) {
	eppName := name + "-inferencepool-epp"
	missing, unavailable, cause, notReadySince := kube.DeploymentPodUnavailable(ctx, cs, namespace, eppName)
	switch {
	case missing:
		return eppName, time.Time{}, "is missing; re-apply charts/modeldeployment to restore the endpoint picker"
	case unavailable:
		return eppName, notReadySince, "not ready: " + cause
	}
	return eppName, time.Time{}, ""
}
