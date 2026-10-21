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

package utils

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

func TestScalingTimingParams(t *testing.T) {
	so := map[string]interface{}{}
	for _, field := range []struct {
		path  []string
		value int64
	}{
		{[]string{"spec", "pollingInterval"}, 15},
		{[]string{"spec", "cooldownPeriod"}, 60},
		{[]string{"spec", "initialCooldownPeriod"}, 120},
		{[]string{"spec", "advanced", "horizontalPodAutoscalerConfig", "behavior", "scaleUp", "stabilizationWindowSeconds"}, 60},
		{[]string{"spec", "advanced", "horizontalPodAutoscalerConfig", "behavior", "scaleDown", "stabilizationWindowSeconds"}, 300},
	} {
		if err := unstructured.SetNestedField(so, field.value, field.path...); err != nil {
			t.Fatal(err)
		}
	}
	for direction, seconds := range map[string]int64{"scaleUp": 900, "scaleDown": 1800} {
		if err := unstructured.SetNestedSlice(so, []interface{}{map[string]interface{}{"periodSeconds": seconds}},
			"spec", "advanced", "horizontalPodAutoscalerConfig", "behavior", direction, "policies"); err != nil {
			t.Fatal(err)
		}
	}
	p := scalingTimingParams(so)
	if p.InitialCooldownPeriod != 120*time.Second || p.CooldownPeriod != time.Minute {
		t.Fatalf("initial and ordinary cooldowns must remain independent: %+v", p)
	}
	if p.ScaleUpTotalWait != 930*time.Second || p.ScaleDownTotalWait != 1830*time.Second {
		t.Fatalf("wait budgets must include the longer rate-limit period: %+v", p)
	}
	// Explicit zero must not fall back to a nonzero default.
	if err := unstructured.SetNestedField(so, int64(0), "spec", "initialCooldownPeriod"); err != nil {
		t.Fatal(err)
	}
	if scalingTimingParams(so).InitialCooldownPeriod != 0 {
		t.Fatal("explicit initial cooldown zero was lost")
	}
}

func modelPod(name, node string, phase corev1.PodPhase, ready corev1.ConditionStatus, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:      phase,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}},
		},
	}
}

func TestListModelPodsSelectsTheInferenceSetAndSkipsTerminatingPods(t *testing.T) {
	values := deploy.ModelDeploymentValues{Name: "scaling-phi", Namespace: "ns"}
	own := map[string]string{"inferenceset.kaito.sh/created-by": "scaling-phi"}
	terminating := modelPod("scaling-phi-c-0", "n3", corev1.PodRunning, corev1.ConditionTrue, own)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"test"}
	clientset := fake.NewSimpleClientset(
		modelPod("scaling-phi-b-0", "n2", corev1.PodRunning, corev1.ConditionTrue, own),
		modelPod("scaling-phi-a-0", "n1", corev1.PodPending, corev1.ConditionFalse, own),
		terminating,
		modelPod("other-0", "n1", corev1.PodRunning, corev1.ConditionTrue,
			map[string]string{"inferenceset.kaito.sh/created-by": "other"}),
		modelPod("shadow", "n9", corev1.PodRunning, corev1.ConditionTrue,
			map[string]string{"kaito.sh/shadow-pod-inferenceset": "scaling-phi"}),
	)

	pods, err := ListModelPods(context.Background(), clientset, values)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range pods {
		names = append(names, p.Name)
	}
	if want := []string{"scaling-phi-a-0", "scaling-phi-b-0"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("pods=%v, want %v", names, want)
	}
	if got := PodPlacements(pods); !reflect.DeepEqual(got, map[string]string{"scaling-phi-a-0": "n1", "scaling-phi-b-0": "n2"}) {
		t.Fatalf("placements=%v", got)
	}
	ready := ReadyPods(pods)
	if len(ready) != 1 || ready[0].Name != "scaling-phi-b-0" {
		t.Fatalf("ready=%v", ready)
	}
}

func TestIsPodReady(t *testing.T) {
	for _, test := range []struct {
		name  string
		pod   *corev1.Pod
		ready bool
	}{
		{"running and ready", modelPod("p", "", corev1.PodRunning, corev1.ConditionTrue, nil), true},
		{"running not ready", modelPod("p", "", corev1.PodRunning, corev1.ConditionFalse, nil), false},
		{"ready condition but pending", modelPod("p", "", corev1.PodPending, corev1.ConditionTrue, nil), false},
		{"no conditions", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, false},
	} {
		if got := IsPodReady(*test.pod); got != test.ready {
			t.Errorf("%s: IsPodReady=%v, want %v", test.name, got, test.ready)
		}
	}
}

func TestModelServerPort(t *testing.T) {
	withPorts := func(containers ...corev1.Container) corev1.Pod {
		return corev1.Pod{Spec: corev1.PodSpec{Containers: containers}}
	}
	for _, test := range []struct {
		name string
		pod  corev1.Pod
		want int
	}{
		{"no ports", withPorts(corev1.Container{Name: "a"}), DefaultModelServerPort},
		{"first declared port", withPorts(corev1.Container{Ports: []corev1.ContainerPort{{ContainerPort: 5000}, {ContainerPort: 5557}}}), 5000},
		{"port on a later container", withPorts(corev1.Container{Name: "a"}, corev1.Container{Ports: []corev1.ContainerPort{{ContainerPort: 8001}}}), 8001},
	} {
		if got := ModelServerPort(test.pod); got != test.want {
			t.Errorf("%s: port=%d, want %d", test.name, got, test.want)
		}
	}
}

func TestSumMetricValuesAddsEverySeriesForTheModel(t *testing.T) {
	text := `# HELP vllm:request_success_total Count of successfully processed requests.
vllm:request_success_total{engine="0",finished_reason="stop",model_name="scaling-phi"} 3.0
vllm:request_success_total{engine="0",finished_reason="length",model_name="scaling-phi"} 7.0
vllm:request_success_total{engine="0",finished_reason="stop",model_name="other"} 100.0
vllm:num_requests_waiting{engine="0",model_name="scaling-phi"} 0.0
`
	labels := map[string]string{"model_name": "scaling-phi"}
	if got, found := SumMetricValues(text, "vllm:request_success_total", labels); !found || got != 10 {
		t.Fatalf("success=%v found=%v, want 10 true", got, found)
	}
	if got, found := SumMetricValues(text, "vllm:num_requests_waiting", labels); !found || got != 0 {
		t.Fatalf("waiting=%v found=%v, want present zero", got, found)
	}
	if _, found := SumMetricValues(text, "vllm:num_requests_running", labels); found {
		t.Fatal("absent metric reported as found")
	}
	if _, found := SumMetricValues(text, "vllm:request_success_total", map[string]string{"model_name": "missing"}); found {
		t.Fatal("metric for another model reported as found")
	}
}

func nodeClaim(name, nodeName string) unstructured.Unstructured {
	nc := unstructured.Unstructured{Object: map[string]interface{}{}}
	nc.SetName(name)
	if nodeName != "" {
		_ = unstructured.SetNestedField(nc.Object, nodeName, "status", "nodeName")
	}
	return nc
}

func TestNodeClaimHelpers(t *testing.T) {
	claims := []unstructured.Unstructured{
		nodeClaim("e2e-scaling-ws-b-xyz", "node-b"),
		nodeClaim("system-surge-1", "node-sys"),
		nodeClaim("e2e-scaling-ws-a-abc", ""),
	}
	if got, want := NodeClaimNames(claims), []string{"e2e-scaling-ws-a-abc", "e2e-scaling-ws-b-xyz", "system-surge-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names=%v, want %v", got, want)
	}
	if got, want := NodeClaimsByNode(claims), map[string]string{"node-b": "e2e-scaling-ws-b-xyz", "node-sys": "system-surge-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("byNode=%v, want %v (claims without a node must be skipped)", got, want)
	}
	// Claims that existed before the case are not attributed to it; a
	// still-launching claim without a node is.
	if got, want := NewNodeClaimNames([]string{"system-surge-1"}, claims), []string{"e2e-scaling-ws-a-abc", "e2e-scaling-ws-b-xyz"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("new=%v, want %v", got, want)
	}
	if got := NewNodeClaimNames(NodeClaimNames(claims), claims); len(got) != 0 {
		t.Fatalf("no new claims expected, got %v", got)
	}
}

func TestEnsureConfigMapCreatesThenReplacesData(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	if err := EnsureConfigMap(ctx, clientset, "ns", "cfg", map[string]string{"a": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureConfigMap(ctx, clientset, "ns", "cfg", map[string]string{"b": "2"}); err != nil {
		t.Fatal(err)
	}
	cm, err := clientset.CoreV1().ConfigMaps("ns").Get(ctx, "cfg", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cm.Data, map[string]string{"b": "2"}) {
		t.Fatalf("data=%v, want replaced", cm.Data)
	}
}
