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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

// inferenceSet builds a minimal InferenceSet at generation 2 carrying a single
// InferenceSetReady condition.
func inferenceSet(status, condReason, message string, observedGeneration int64, transition time.Time) *unstructured.Unstructured {
	cond := map[string]interface{}{
		"type":               inferenceSetReadyCondition,
		"status":             status,
		"reason":             condReason,
		"message":            message,
		"lastTransitionTime": transition.UTC().Format(time.RFC3339),
	}
	if observedGeneration != 0 {
		cond["observedGeneration"] = observedGeneration
	}
	is := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kaito.sh/v1beta1",
		"kind":       "InferenceSet",
		"metadata":   map[string]interface{}{"name": "qwen", "namespace": "team-a"},
		"status":     map[string]interface{}{"conditions": []interface{}{cond}},
	}}
	is.SetGeneration(2)
	return is
}

func TestInferenceSetReadyFinding(t *testing.T) {
	obj := evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: "team-a"}
	transition := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name               string
		status             string
		reason             string
		observedGeneration int64
		want               bool
		wantExempt         bool
	}{
		{"terminal quota failure surfaces immediately", "False", "QuotaExceeded", 2, true, true},
		{"terminal image pull failure surfaces immediately", "False", "ImagePullError", 2, true, true},
		{"terminal capacity failure surfaces immediately", "False", "CapacityUnavailable", 2, true, true},
		{"non-terminal failure is debounced", "False", "DownloadFailed", 2, true, false},
		{"unclassified generic reason is debounced", "False", "inferencesetNotReady", 2, true, false},
		{"in-progress node claim is ignored", "False", "NodeClaimNotReady", 2, false, false},
		{"in-progress model mirror is ignored", "False", "ModelMirrorNotReady", 2, false, false},
		{"in-progress inference is ignored", "False", "WorkspaceInferenceStatusPending", 2, false, false},
		{"ready is ignored", "True", "inferencesetReady", 2, false, false},
		{"empty reason is ignored", "False", "", 2, false, false},
		{"stale generation is ignored", "False", "QuotaExceeded", 1, false, false},
		{"missing observedGeneration is accepted", "False", "QuotaExceeded", 0, true, true},
	}
	for _, tc := range cases {
		is := inferenceSet(tc.status, tc.reason, "inferenceset is not ready, 0/1 replicas are ready", tc.observedGeneration, transition)
		f, got := inferenceSetReadyFinding(is, obj, "inferenceset/team-a/qwen")
		if got != tc.want {
			t.Errorf("%s: got=%v, want %v", tc.name, got, tc.want)
			continue
		}
		if !got {
			continue
		}
		if f.Reason != reason.Reason(tc.reason) {
			t.Errorf("%s: reason=%q, want pass-through %q", tc.name, f.Reason, tc.reason)
		}
		if f.StartupGraceExempt != tc.wantExempt {
			t.Errorf("%s: exempt=%v, want %v", tc.name, f.StartupGraceExempt, tc.wantExempt)
		}
		if f.GracePeriodOverride != FailureGracePeriod || !f.ResourceCreatedAt.Equal(transition) {
			t.Errorf("%s: debounce=%v anchored at %v, want %v at %v", tc.name, f.GracePeriodOverride, f.ResourceCreatedAt, FailureGracePeriod, transition)
		}
		if want := "InferenceSet team-a/qwen: inferenceset is not ready, 0/1 replicas are ready"; f.Message != want {
			t.Errorf("%s: message=%q, want %q", tc.name, f.Message, want)
		}
	}
}
