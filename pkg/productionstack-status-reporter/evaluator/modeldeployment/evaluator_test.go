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

package modeldeployment

import (
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

var testNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// cond is a status condition fixture; its lastTransitionTime is omitted when
// age is negative.
type cond struct {
	condType, status, reason, message string
	age                               time.Duration
}

func kaitoObject(kind, name string, conds ...cond) *unstructured.Unstructured {
	list := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		m := map[string]interface{}{"type": c.condType, "status": c.status, "reason": c.reason, "message": c.message}
		if c.age >= 0 {
			m["lastTransitionTime"] = testNow.Add(-c.age).Format(time.RFC3339)
		}
		list = append(list, m)
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kaito.sh/v1beta1",
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": "ns"},
		"status":     map[string]interface{}{"conditions": list},
	}}
}

func workspace(name string, conds ...cond) *unstructured.Unstructured {
	return kaitoObject("Workspace", name, conds...)
}

func inferenceSet(conds ...cond) *unstructured.Unstructured {
	return kaitoObject("InferenceSet", "iset", conds...)
}

func nodeClaim(r string, age time.Duration) cond {
	return cond{condNodeClaimReady, "False", r, "detail", age}
}

func TestRootCause(t *testing.T) {
	nodeClaimsReady := cond{condNodeClaimReady, "True", "NodeClaimsReady", "", time.Hour}
	nodesReady := cond{condNodesReady, "True", "NodesReady", "", time.Hour}
	cases := []struct {
		name           string
		conds          []cond
		wantReason     reason.Reason
		wantStalled    bool
		wantReportable bool
	}{
		{"provisioning failure within the failure delay is withheld",
			[]cond{nodeClaim("SubscriptionQuotaReached", 2*time.Minute)}, reason.InferencesetInfraProvisioningFailed, false, false},
		{"provisioning failure past the failure delay is reported",
			[]cond{nodeClaim("SubscriptionQuotaReached", 3*time.Minute)}, reason.InferencesetInfraProvisioningFailed, false, true},
		{"any reason other than NodeClaimNotReady is a failure",
			[]cond{nodeClaim("NodeClassNotReady", 5*time.Minute)}, reason.InferencesetInfraProvisioningFailed, false, true},
		{"failure without a transition time cannot be aged",
			[]cond{nodeClaim("SubscriptionQuotaReached", -1)}, reason.InferencesetInfraProvisioningFailed, false, false},
		{"provisioning in progress within the stall timeout is withheld",
			[]cond{nodeClaim("NodeClaimNotReady", 14*time.Minute)}, reason.InferencesetInfraProvisioningFailed, true, false},
		{"provisioning in progress past the stall timeout is reported",
			[]cond{nodeClaim("NodeClaimNotReady", 15*time.Minute)}, reason.InferencesetInfraProvisioningFailed, true, true},
		{"ready NodeClaims waiting for the GPU device plugin stall",
			[]cond{nodeClaimsReady, {condNodesReady, "False", "NodePluginsNotReady", "", 20 * time.Minute}},
			reason.InferencesetInfraProvisioningFailed, true, true},
		{"BYO nodes not matching stall",
			[]cond{{condNodesReady, "False", "NoAvailableGPUNodes", "", 20 * time.Minute}},
			reason.InferencesetInfraProvisioningFailed, true, true},
		{"NodeClaim stage is the root cause over NodesReady",
			[]cond{nodeClaim("NodeClaimNotReady", time.Minute), {condNodesReady, "False", "NodeNotReady", "", time.Hour}},
			reason.InferencesetInfraProvisioningFailed, true, false},
		{"withheld node cause does not fall through to the inference workload",
			[]cond{nodeClaim("SubscriptionQuotaReached", time.Minute), {condInferenceReady, "False", "Unschedulable", "", time.Minute}},
			reason.InferencesetInfraProvisioningFailed, false, false},
		{"failing model mirror wins over progressing node provisioning",
			[]cond{nodeClaim("NodeClaimNotReady", time.Minute), {condModelMirrorReady, "False", "DownloadFailed", "", 0}},
			reason.InferencesetModelMirrorFailed, false, true},
		{"failing model configuration wins over progressing nodes",
			[]cond{{condNodesReady, "False", "NodeNotReady", "", time.Minute}, {condModelConfigReady, "False", "ModelConfigInvalid", "", 0}},
			reason.InferencesetModelConfigInvalid, false, true},
		{"first failing prerequisite is the root cause even while withheld",
			[]cond{nodeClaim("SubscriptionQuotaReached", time.Minute), {condModelMirrorReady, "False", "DownloadFailed", "", 0}},
			reason.InferencesetInfraProvisioningFailed, false, false},
		{"progressing model mirror does not hide a node stall",
			[]cond{{condModelMirrorReady, "False", "ModelMirrorPending", "", time.Hour}, nodeClaim("NodeClaimNotReady", 20*time.Minute)},
			reason.InferencesetInfraProvisioningFailed, true, true},
		{"inference failure waits for progressing prerequisites",
			[]cond{{condNodesReady, "False", "NodePluginsNotReady", "", time.Minute}, {condInferenceReady, "False", "Unschedulable", "", 0}},
			reason.InferencesetInfraProvisioningFailed, true, false},
		{"condition without a reason is skipped",
			[]cond{nodeClaim("", time.Hour), nodesReady, {condInferenceReady, "False", "ImagePullError", "", 0}},
			reason.InferencesetModelPodsNotReady, false, true},
		{"model mirror being recreated is progress",
			[]cond{nodeClaimsReady, nodesReady, {condModelMirrorReady, "False", "ModelMirrorDeleting", "", time.Hour}},
			reason.InferencesetModelMirrorFailed, true, false},
		{"model configuration failure is reported",
			[]cond{nodeClaimsReady, nodesReady, {condModelConfigReady, "False", "ModelConfigInvalid", "", 0}},
			reason.InferencesetModelConfigInvalid, false, true},
		{"model download in progress is withheld",
			[]cond{nodeClaimsReady, nodesReady, {condModelMirrorReady, "False", "ModelMirrorPending", "", time.Hour}},
			reason.InferencesetModelMirrorFailed, true, false},
		{"model mirror failure is reported",
			[]cond{nodeClaimsReady, nodesReady, {condModelMirrorReady, "False", "DownloadFailed", "", 0}},
			reason.InferencesetModelMirrorFailed, false, true},
		{"model mirror is the root cause over the inference workload",
			[]cond{nodeClaimsReady, nodesReady, {condModelMirrorReady, "False", "DownloadOOMKilled", "", 0},
				{condInferenceReady, "False", "ContainerCrashLoopBackOff", "", 0}},
			reason.InferencesetModelMirrorFailed, false, true},
		{"classified inference failure is reported",
			[]cond{nodeClaimsReady, nodesReady, {condInferenceReady, "False", "ImagePullError", "", 0}},
			reason.InferencesetModelPodsNotReady, false, true},
		{"inference workload starting is withheld",
			[]cond{nodeClaimsReady, nodesReady, {condInferenceReady, "False", "WorkspaceInferenceStatusPending", "", time.Hour}},
			reason.InferencesetModelPodsNotReady, true, false},
		{"empty inference reason is skipped",
			[]cond{{condInferenceReady, "False", "", "", time.Hour}}, "", false, false},
		{"ready workspace has no root cause",
			[]cond{nodeClaimsReady, nodesReady, {condInferenceReady, "True", "WorkspaceInferenceStatusSuccess", "", time.Hour}}, "", false, false},
	}
	for _, tc := range cases {
		cause, reportable := rootCause(workspace("ws0", tc.conds...), testNow)
		if cause.reason != tc.wantReason || cause.stalled != tc.wantStalled || reportable != tc.wantReportable {
			t.Errorf("%s: got reason=%q stalled=%v reportable=%v, want reason=%q stalled=%v reportable=%v",
				tc.name, cause.reason, cause.stalled, reportable, tc.wantReason, tc.wantStalled, tc.wantReportable)
		}
	}
}

func TestWorkspaceFindingsAggregatesPerReason(t *testing.T) {
	workspaces := []*unstructured.Unstructured{
		workspace("ws-d", cond{condInferenceReady, "False", "ContainerOOMKilled", "oom", 0}),
		workspace("ws-c", nodeClaim("NodeClaimNotReady", 20*time.Minute)),
		workspace("ws-b", nodeClaim("SubscriptionQuotaReached", 4*time.Minute)),
		workspace("ws-a", nodeClaim("InsufficientCapacityError", time.Minute)),
		workspace("ws-e", cond{condInferenceReady, "False", "ImagePullError", "pull", 0}),
	}
	findings := workspaceFindings(inferenceSet(), workspaces, testNow)
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}

	infra := findings[0]
	wantInfra := "InferenceSet ns/iset: GPU nodes are not ready: " +
		"workspace ws-b NodeClaimReady=False (SubscriptionQuotaReached): detail; " +
		"workspace ws-c not ready for over 15 minutes, NodeClaimReady=False (NodeClaimNotReady): detail."
	if infra.Reason != reason.InferencesetInfraProvisioningFailed || infra.Message != wantInfra {
		t.Errorf("infra finding = %q %q, want %q", infra.Reason, infra.Message, wantInfra)
	}

	pods := findings[1]
	wantPods := "InferenceSet ns/iset: model pod not ready: " +
		"workspace ws-d InferenceReady=False (ContainerOOMKilled): oom; " +
		"workspace ws-e InferenceReady=False (ImagePullError): pull."
	if pods.Reason != reason.InferencesetModelPodsNotReady || pods.Message != wantPods {
		t.Errorf("model pod finding = %q %q, want %q", pods.Reason, pods.Message, wantPods)
	}

	for _, f := range findings {
		if f.GroupKey != "inferenceset/ns/iset" || f.WorkloadNamespace != "ns" || f.Object.Name != "ns" {
			t.Errorf("%s: unexpected subject GroupKey=%q WorkloadNamespace=%q Object=%v", f.Reason, f.GroupKey, f.WorkloadNamespace, f.Object)
		}
		if !f.StartupGraceExempt {
			t.Errorf("%s: timing is decided per Workspace, so the finding must bypass the startup grace gate", f.Reason)
		}
	}
}

func TestWorkspaceFindingsNothingReportable(t *testing.T) {
	workspaces := []*unstructured.Unstructured{
		workspace("ws-a", nodeClaim("InsufficientCapacityError", time.Minute)),
		workspace("ws-b", nodeClaim("NodeClaimNotReady", 5*time.Minute)),
	}
	if findings := workspaceFindings(inferenceSet(), workspaces, testNow); len(findings) != 0 {
		t.Errorf("got %+v, want no findings", findings)
	}
}

func TestWorkspaceFindingsBoundsMessage(t *testing.T) {
	var workspaces []*unstructured.Unstructured
	for i := 0; i < maxReportedWorkspaces+2; i++ {
		workspaces = append(workspaces, workspace(fmt.Sprintf("ws-%d", i),
			cond{condInferenceReady, "False", "ContainerCrashLoopBackOff", strings.Repeat("x", 300), 0}))
	}
	findings := workspaceFindings(inferenceSet(), workspaces, testNow)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	msg := findings[0].Message
	if strings.Contains(msg, "ws-5") || !strings.Contains(msg, "ws-4") || !strings.HasSuffix(msg, "; and 2 more.") {
		t.Errorf("message must list %d workspaces and count the rest: %q", maxReportedWorkspaces, msg)
	}
	if !strings.Contains(msg, "): "+strings.Repeat("x", maxConditionMessageLen)+"...;") || strings.Contains(msg, strings.Repeat("x", maxConditionMessageLen+1)) {
		t.Errorf("condition messages must be truncated to %d characters: %q", maxConditionMessageLen, msg)
	}
}

func TestWorkspaceFindingsModelConfig(t *testing.T) {
	ready := []cond{
		{condNodeClaimReady, "True", "NodeClaimsReady", "", time.Hour},
		{condNodesReady, "True", "NodesReady", "", time.Hour},
	}
	wsInvalid := workspace("ws-a", append(ready, cond{condModelConfigReady, "False", "ModelConfigReplaced", "replaced", 0})...)

	findings := workspaceFindings(inferenceSet(), []*unstructured.Unstructured{wsInvalid}, testNow)
	want := "InferenceSet ns/iset: model configuration is invalid: workspace ws-a ModelConfigReady=False (ModelConfigReplaced): replaced."
	if len(findings) != 1 || findings[0].Reason != reason.InferencesetModelConfigInvalid || findings[0].Message != want {
		t.Errorf("workspace model configuration: got %+v, want message %q", findings, want)
	}

	// The InferenceSet's own condition, set before any Workspace is created,
	// replaces the Workspaces' copies.
	is := inferenceSet(cond{condModelConfigReady, "False", "ModelConfigInvalid", "bad config", 0})
	findings = workspaceFindings(is, []*unstructured.Unstructured{wsInvalid}, testNow)
	want = "InferenceSet ns/iset: model configuration is invalid: InferenceSet ModelConfigReady=False (ModelConfigInvalid): bad config."
	if len(findings) != 1 || findings[0].Message != want {
		t.Errorf("InferenceSet model configuration: got %+v, want message %q", findings, want)
	}
	if findings = workspaceFindings(is, nil, testNow); len(findings) != 1 || findings[0].Message != want {
		t.Errorf("InferenceSet model configuration without Workspaces: got %+v, want message %q", findings, want)
	}
}

// Workspaces being deleted are skipped so their stale conditions are not
// reported, as KAITO does when it explains why an InferenceSet is not ready.
func TestLiveWorkspaces(t *testing.T) {
	live := workspace("live", nodeClaim("NodeClaimNotReady", -1))
	deleting := workspace("deleting", nodeClaim("SubscriptionQuotaReached", -1))
	now := metav1.Now()
	deleting.SetDeletionTimestamp(&now)

	got := liveWorkspaces([]unstructured.Unstructured{*live, *deleting})
	if len(got) != 1 || got[0].GetName() != "live" {
		names := make([]string, 0, len(got))
		for _, ws := range got {
			names = append(names, ws.GetName())
		}
		t.Errorf("liveWorkspaces = %v, want [live]", names)
	}
}
