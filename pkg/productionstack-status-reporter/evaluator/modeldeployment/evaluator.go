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
	"sort"
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

// Workspace (and InferenceSet) status condition types and reasons surfaced by
// KAITO, consumed to derive the InferenceSet's health.
const (
	// condNodeClaimReady carries GPU node provisioning health: KAITO reports
	// provisioning progress as NodeClaimNotReady and a failure of the latest
	// attempt with the reason recorded by Karpenter, the cloud provider or
	// their Events (quota, capacity, SKU unavailable, asynchronous provisioning
	// errors), also after the failed NodeClaim was deleted. The failure reasons
	// are an open set.
	condNodeClaimReady = "NodeClaimReady"
	// condNodesReady carries GPU worker-node readiness: KAITO keeps it False
	// (NodeNotReady / NodePluginsNotReady / NoAvailableGPUNodes) until enough
	// nodes are Ready and advertise GPU capacity. It is the only signal for a
	// node whose NodeClaim is Ready but whose GPU device plugin never
	// registers, and the only node signal in BYO mode, which does not report
	// NodeClaimReady.
	condNodesReady = "NodesReady"
	// condModelConfigReady reports whether a bring-your-own model's
	// configuration resolved (ModelConfigInvalid / ModelConfigReplaced when
	// not). KAITO sets it on the InferenceSet, before creating any Workspace,
	// and on each Workspace.
	condModelConfigReady = "ModelConfigReady"
	// condModelMirrorReady reports whether the ModelMirror streaming the model
	// weights is ready: ModelMirrorPending while the download is in progress
	// and ModelMirrorDeleting while a previous ModelMirror is torn down before
	// it is recreated; any other reason is a failure (e.g. DownloadFailed,
	// JobCreateFailed, ModelMirrorCreateFailed).
	condModelMirrorReady = "ModelMirrorReady"
	// condInferenceReady carries model-server readiness: KAITO sets it False
	// with a classified pod/container failure reason once it detects an
	// actionable failure.
	condInferenceReady = "InferenceReady"
)

// infraProvisioningFailureDelay withholds a GPU node provisioning failure until
// the provisioning attempt has run for this long: Karpenter re-attempts with
// alternative SKUs/zones, so a failure early in the attempt may still clear
// within minutes. The attempt starts at the condition's lastTransitionTime,
// since NodeClaimReady is already False while provisioning and KAITO only
// changes its reason on a failure.
const infraProvisioningFailureDelay = 3 * time.Minute

// infraProvisioningStallTimeout bounds how long GPU nodes may stay in progress
// before they are surfaced as stalled: NodeClaimReady=False (NodeClaimNotReady)
// or, once the NodeClaims are Ready, NodesReady=False. Karpenter times out
// NodeClaims that fail to launch or register, which KAITO then reports as a
// failure, but nothing times out a node that never initializes or whose GPU
// device plugin never registers. The latter only shows on NodesReady: KAITO
// provisions through a static NodePool whose NodeClaims request no resources,
// so Karpenter does not wait for the GPU. Both conditions stay False from the
// start of the provisioning attempt, so their lastTransitionTime anchors the
// timeout.
const infraProvisioningStallTimeout = 15 * time.Minute

// maxReportedWorkspaces bounds the Workspaces listed in a finding's message.
const maxReportedWorkspaces = 5

// maxConditionMessageLen bounds each condition message quoted in a finding,
// matching the length KAITO truncates provisioning messages to.
const maxConditionMessageLen = 256

// Evaluator enumerates the InferenceSets in every managed namespace and
// evaluates every modeldeployment-layer chain reason (§1.2) for each. The
// orthogonal inferencesetWeightDownloadSlow reason is evaluated separately by
// the weightdownload Evaluator.
type Evaluator struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	now       func() time.Time
}

// New constructs a modeldeployment Evaluator.
func New(cs kubernetes.Interface, dyn dynamic.Interface) *Evaluator {
	return &Evaluator{clientset: cs, dynamic: dyn, now: time.Now}
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
	now := e.now()
	var findings []evaluator.Finding
	for _, ns := range namespaces {
		sets, err := e.dynamic.Resource(util.InferenceSetGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range sets.Items {
			findings = append(findings, e.evaluateInferenceSet(ctx, ns, &sets.Items[i], now)...)
		}
	}
	return findings, nil
}

func (e *Evaluator) evaluateInferenceSet(ctx context.Context, namespace string, is *unstructured.Unstructured, now time.Time) []evaluator.Finding {
	name := is.GetName()

	// The child Workspaces' status conditions are the authoritative source for
	// the GPU-node, model and model-pod health: the KAITO Workspace controller
	// already probes the underlying NodeClaims, ModelMirror and pods and
	// classifies their state on its conditions, so the reporter reads those
	// conditions instead of re-deriving them.
	findings := workspaceFindings(is, getWorkspaces(ctx, e.dynamic, namespace, name), now)

	// inferencesetEPPNotReady — EPP Deployment readiness.
	if eppName, eppCreatedAt, msg := eppNotReady(ctx, e.clientset, namespace, name); msg != "" {
		findings = append(findings, evaluator.Finding{
			Reason:            reason.InferencesetEPPNotReady,
			Object:            evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: namespace},
			Message:           fmt.Sprintf("InferenceSet %s/%s: EPP Deployment %s %s.", namespace, name, eppName, msg),
			WorkloadNamespace: namespace,
			GroupKey:          groupKey(namespace, name),
			ResourceCreatedAt: eppCreatedAt,
		})
	}
	return findings
}

func groupKey(namespace, name string) string {
	return fmt.Sprintf("inferenceset/%s/%s", namespace, name)
}

// statusCondition is a status condition flattened for the reporter, with
// lastTransitionTime parsed for time anchoring.
type statusCondition struct {
	condType       string
	status         string
	reason         string
	message        string
	lastTransition time.Time
}

// findCondition extracts status.conditions[type] from a KAITO object,
// returning the flattened condition and whether it was present.
func findCondition(obj *unstructured.Unstructured, condType string) (statusCondition, bool) {
	conds, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !ok {
		return statusCondition{}, false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != condType {
			continue
		}
		out := statusCondition{condType: condType}
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
	return statusCondition{}, false
}

// lastedAtLeast reports whether a condition that last transitioned at since has
// held for at least d. An unknown transition time cannot be aged, so it never
// qualifies.
func lastedAtLeast(since, now time.Time, d time.Duration) bool {
	return !since.IsZero() && now.Sub(since) >= d
}

// conditionRule maps a False Workspace condition onto a reporter reason and
// decides when it is reported.
type conditionRule struct {
	condType string
	reason   reason.Reason
	// progressReasons are the reasons that report progress rather than a
	// failure; nil means every reason is progress.
	progressReasons map[string]bool
	// failureDelay withholds a failure until the condition has been False for
	// this long; zero reports it immediately.
	failureDelay time.Duration
	// stallTimeout reports progress as stalled once the condition has been
	// False for this long; zero never reports progress.
	stallTimeout time.Duration
}

func (r conditionRule) inProgress(condReason string) bool {
	return r.progressReasons == nil || r.progressReasons[condReason]
}

// prerequisiteRules cover the Workspace conditions that must be True before
// the inference workload can become ready: the GPU nodes, the custom model
// configuration and the model weights. They progress independently, so a
// failure of one is not a consequence of another still progressing.
var prerequisiteRules = []conditionRule{
	{
		condType:        condNodeClaimReady,
		reason:          reason.InferencesetInfraProvisioningFailed,
		progressReasons: map[string]bool{"NodeClaimNotReady": true},
		failureDelay:    infraProvisioningFailureDelay,
		stallTimeout:    infraProvisioningStallTimeout,
	},
	{
		// Every NodesReady reason is progress, including the BYO
		// NoAvailableGPUNodes, which KAITO classifies as a failure: the
		// matching nodes may still be joining the cluster, so it is only
		// reported once it stalls.
		condType:     condNodesReady,
		reason:       reason.InferencesetInfraProvisioningFailed,
		stallTimeout: infraProvisioningStallTimeout,
	},
	{
		condType:        condModelConfigReady,
		reason:          reason.InferencesetModelConfigInvalid,
		progressReasons: map[string]bool{},
	},
	{
		// A slow download is reported by inferencesetWeightDownloadSlow.
		condType:        condModelMirrorReady,
		reason:          reason.InferencesetModelMirrorFailed,
		progressReasons: map[string]bool{"ModelMirrorPending": true, "ModelMirrorDeleting": true},
	},
}

// inferenceRule covers InferenceReady, the root cause only once every
// prerequisite is ready, as its failures (e.g. Unschedulable pods) can be a
// consequence of a prerequisite still progressing.
var inferenceRule = conditionRule{
	condType:        condInferenceReady,
	reason:          reason.InferencesetModelPodsNotReady,
	progressReasons: map[string]bool{"WorkspaceInferenceStatusPending": true},
}

// notReadyCondition returns the condition of the given type when it is False
// with a reason.
func notReadyCondition(obj *unstructured.Unstructured, condType string) (statusCondition, bool) {
	c, ok := findCondition(obj, condType)
	return c, ok && c.status == "False" && c.reason != ""
}

// workspaceCause is a Workspace's root cause mapped onto a reporter reason.
type workspaceCause struct {
	reason reason.Reason
	cond   statusCondition
	// stalled marks progress reported because it exceeded its stall timeout,
	// as opposed to a failure.
	stalled bool
}

// cause maps the rule's False condition onto a workspaceCause, and reports
// whether it is reportable at now.
func (r conditionRule) cause(c statusCondition, now time.Time) (workspaceCause, bool) {
	if r.inProgress(c.reason) {
		return workspaceCause{reason: r.reason, cond: c, stalled: true},
			r.stallTimeout > 0 && lastedAtLeast(c.lastTransition, now, r.stallTimeout)
	}
	return workspaceCause{reason: r.reason, cond: c},
		r.failureDelay == 0 || lastedAtLeast(c.lastTransition, now, r.failureDelay)
}

// rootCause returns why the Workspace is not ready, as KAITO does when it
// explains why an InferenceSet is not ready: the first failing prerequisite,
// otherwise the first progressing one, and InferenceReady only once every
// prerequisite is ready. reportable is false when the Workspace is ready, or
// when its root cause is progress that has not stalled or a failure that is
// still within its delay; the Workspace is then not reported under another
// condition.
func rootCause(ws *unstructured.Unstructured, now time.Time) (cause workspaceCause, reportable bool) {
	var progressing *conditionRule
	var progressingCond statusCondition
	for i := range prerequisiteRules {
		rule := &prerequisiteRules[i]
		c, ok := notReadyCondition(ws, rule.condType)
		if !ok {
			continue
		}
		if !rule.inProgress(c.reason) {
			return rule.cause(c, now)
		}
		if progressing == nil {
			progressing, progressingCond = rule, c
		}
	}
	if progressing != nil {
		return progressing.cause(progressingCond, now)
	}
	if c, ok := notReadyCondition(ws, inferenceRule.condType); ok {
		return inferenceRule.cause(c, now)
	}
	return workspaceCause{}, false
}

// reasonSummaries lead each finding's message, in the order findings are
// emitted.
var reasonSummaries = []struct {
	reason  reason.Reason
	summary string
}{
	{reason.InferencesetInfraProvisioningFailed, "GPU nodes are not ready"},
	{reason.InferencesetModelConfigInvalid, "model configuration is invalid"},
	{reason.InferencesetModelMirrorFailed, "model mirror is not ready"},
	{reason.InferencesetModelPodsNotReady, "model pod not ready"},
}

// reportedCauses collects the entries of one reporter reason; failures are
// listed before stalls, each in Workspace name order.
type reportedCauses struct {
	failures []string
	stalls   []string
}

// workspaceFindings derives one finding per reporter reason from the root
// causes of the InferenceSet's Workspaces, listing every reportable Workspace
// in its message. All timing is decided per Workspace from its conditions'
// lastTransitionTime, so the findings bypass the emitter's startup grace gate.
//
// The InferenceSet's own ModelConfigReady takes precedence over the
// Workspaces': KAITO resolves the model for the whole set before creating any
// Workspace, so it is the single source while it is False.
func workspaceFindings(is *unstructured.Unstructured, workspaces []*unstructured.Unstructured, now time.Time) []evaluator.Finding {
	namespace, name := is.GetNamespace(), is.GetName()
	sorted := append([]*unstructured.Unstructured(nil), workspaces...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetName() < sorted[j].GetName() })

	setModelConfig, setModelConfigInvalid := notReadyCondition(is, condModelConfigReady)

	byReason := map[reason.Reason]*reportedCauses{}
	causesOf := func(r reason.Reason) *reportedCauses {
		if byReason[r] == nil {
			byReason[r] = &reportedCauses{}
		}
		return byReason[r]
	}
	for _, ws := range sorted {
		cause, ok := rootCause(ws, now)
		if !ok || (setModelConfigInvalid && cause.reason == reason.InferencesetModelConfigInvalid) {
			continue
		}
		causes := causesOf(cause.reason)
		if cause.stalled {
			causes.stalls = append(causes.stalls, fmt.Sprintf("workspace %s not ready for over %d minutes, %s",
				ws.GetName(), int(infraProvisioningStallTimeout.Minutes()), describeCondition(cause.cond)))
			continue
		}
		causes.failures = append(causes.failures, "workspace "+ws.GetName()+" "+describeCondition(cause.cond))
	}
	if setModelConfigInvalid {
		causesOf(reason.InferencesetModelConfigInvalid).failures = []string{"InferenceSet " + describeCondition(setModelConfig)}
	}

	var findings []evaluator.Finding
	for _, rs := range reasonSummaries {
		causes := byReason[rs.reason]
		if causes == nil {
			continue
		}
		findings = append(findings, evaluator.Finding{
			Reason:             rs.reason,
			Object:             evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: namespace},
			Message:            fmt.Sprintf("InferenceSet %s/%s: %s: %s.", namespace, name, rs.summary, joinEntries(append(causes.failures, causes.stalls...))),
			WorkloadNamespace:  namespace,
			GroupKey:           groupKey(namespace, name),
			StartupGraceExempt: true,
		})
	}
	return findings
}

// describeCondition renders a False condition as "<Type>=False (<Reason>): <message>".
func describeCondition(c statusCondition) string {
	text := fmt.Sprintf("%s=False (%s)", c.condType, c.reason)
	if c.message != "" {
		text += ": " + truncate(c.message, maxConditionMessageLen)
	}
	return text
}

// joinEntries joins up to maxReportedWorkspaces entries and counts the rest.
func joinEntries(entries []string) string {
	if len(entries) <= maxReportedWorkspaces {
		return strings.Join(entries, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(entries[:maxReportedWorkspaces], "; "), len(entries)-maxReportedWorkspaces)
}

// truncate shortens s to at most maxLen runes, marking the cut with "...".
func truncate(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "..."
}

// getWorkspaces returns the KAITO Workspaces created by the InferenceSet,
// selected via the inferenceset.kaito.sh/created-by label (child Workspace
// names are assigned by the KAITO controller and do not match the
// InferenceSet name). Workspaces being deleted (e.g. by a scale-down or a
// rolling upgrade) are skipped, as KAITO does when it explains why an
// InferenceSet is not ready, so their stale conditions are not reported.
// Returns nil when unavailable.
func getWorkspaces(ctx context.Context, dyn dynamic.Interface, namespace, name string) []*unstructured.Unstructured {
	list, err := dyn.Resource(workspaceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", evaluator.LabelCreatedBy, name),
	})
	if err != nil {
		return nil
	}
	return liveWorkspaces(list.Items)
}

// liveWorkspaces returns the Workspaces that are not being deleted.
func liveWorkspaces(items []unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, 0, len(items))
	for i := range items {
		if items[i].GetDeletionTimestamp() != nil {
			continue
		}
		out = append(out, &items[i])
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
