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
)

// workspaceWithCondition builds a minimal Workspace unstructured carrying a
// single status condition.
func workspaceWithCondition(name, condType, status, reason, message string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kaito.sh/v1beta1",
		"kind":       "Workspace",
		"metadata":   map[string]interface{}{"name": name},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":               condType,
					"status":             status,
					"reason":             reason,
					"message":            message,
					"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
				},
			},
		},
	}}
}

func TestInfraProvisioningFinding(t *testing.T) {
	obj := evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: "ns"}
	cases := []struct {
		name   string
		status string
		reason string
		want   bool
	}{
		{"quota failure surfaces", "False", "SubscriptionQuotaReached", true},
		{"sku unavailable surfaces", "False", "SKUNotAvailable", true},
		{"generic in-progress benign", "False", "NodeClaimNotReady", false},
		{"awaiting reconciliation benign", "False", "AwaitingReconciliation", false},
		{"ready is not a failure", "True", "NodeClaimsReady", false},
	}
	for _, tc := range cases {
		ws := workspaceWithCondition("ws0", workspaceCondNodeClaimReady, tc.status, tc.reason, "detail")
		_, got := infraProvisioningFinding([]*unstructured.Unstructured{ws}, "ns", "iset", obj, "gk")
		if got != tc.want {
			t.Errorf("%s: infraProvisioningFinding got=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestModelPodFinding(t *testing.T) {
	obj := evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: "ns"}
	cases := []struct {
		name   string
		status string
		reason string
		want   bool
	}{
		{"image pull error surfaces", "False", "ImagePullError", true},
		{"crash loop surfaces", "False", "ContainerCrashLoopBackOff", true},
		{"oom surfaces", "False", "ContainerOOMKilled", true},
		{"generic pending benign", "False", "WorkspaceInferenceStatusPending", false},
		{"empty reason benign", "False", "", false},
		{"ready is not a failure", "True", "WorkspaceInferenceStatusSuccess", false},
	}
	for _, tc := range cases {
		ws := workspaceWithCondition("ws0", workspaceCondInferenceReady, tc.status, tc.reason, "detail")
		_, got := modelPodFinding([]*unstructured.Unstructured{ws}, "ns", "iset", obj, "gk")
		if got != tc.want {
			t.Errorf("%s: modelPodFinding got=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNodePressureFinding(t *testing.T) {
	obj := evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: "ns"}
	cases := []struct {
		name    string
		status  string
		message string
		want    bool
	}{
		{"pressure marker surfaces (nodes ready)", "True", "Enough Nodes are ready with GPU resources; warning: worker node(s) under resource pressure: aks-0 (DiskPressure)", true},
		{"pressure marker surfaces (nodes not ready)", "False", "Not enough Nodes are ready; warning: worker node(s) under resource pressure: aks-0 (MemoryPressure)", true},
		{"healthy message benign", "True", "Enough Nodes are ready with GPU resources", false},
		{"empty message benign", "True", "", false},
	}
	for _, tc := range cases {
		ws := workspaceWithCondition("ws0", workspaceCondNodesReady, tc.status, "NodesReady", tc.message)
		f, got := nodePressureFinding([]*unstructured.Unstructured{ws}, "ns", "iset", obj, "gk")
		if got != tc.want {
			t.Errorf("%s: nodePressureFinding got=%v, want %v", tc.name, got, tc.want)
			continue
		}
		if got && f.GracePeriodOverride != nodePressureGracePeriod {
			t.Errorf("%s: expected short GracePeriodOverride, got %v", tc.name, f.GracePeriodOverride)
		}
	}
}
