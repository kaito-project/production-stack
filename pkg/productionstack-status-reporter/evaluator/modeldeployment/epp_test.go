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
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
)

func TestEPPNotReady(t *testing.T) {
	ctx := context.Background()
	obj := evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: "team-a"}
	epp := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "qwen-inferencepool-epp"},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "qwen-epp"}},
		},
	}

	// The deploying party guarantees the EPP Deployment exists alongside its
	// InferenceSet, so a missing one is not reported.
	if got := eppNotReady(ctx, fake.NewSimpleClientset(), "team-a", "qwen", obj, "g"); len(got) != 0 {
		t.Errorf("a missing EPP Deployment must not be reported, got %+v", got)
	}
	got := eppNotReady(ctx, fake.NewSimpleClientset(epp), "team-a", "qwen", obj, "g")
	want := "InferenceSet team-a/qwen: EPP Deployment qwen-inferencepool-epp not ready: Deployment is scaled to zero replicas."
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("an EPP Deployment scaled to zero must be reported, got %+v", got)
	}
}
