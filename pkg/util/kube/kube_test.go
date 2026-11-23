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

package kube

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

var (
	created = time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	labels  = map[string]string{"app": "epp"}

	csiNodeDaemonSet = &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "csi-node"},
		Spec:       appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
)

func testPod(name, node string, phase corev1.PodPhase, conds ...corev1.PodCondition) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, Labels: labels, CreationTimestamp: metav1.NewTime(created)},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: phase, Conditions: conds},
	}
}

func condition(t corev1.PodConditionType, s corev1.ConditionStatus, reason, msg string, at time.Time) corev1.PodCondition {
	return corev1.PodCondition{Type: t, Status: s, Reason: reason, Message: msg, LastTransitionTime: metav1.NewTime(at)}
}

func deploymentPodUnavailable(t *testing.T, pods ...runtime.Object) (PodUnavailability, bool) {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "epp"},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
	return DeploymentPodUnavailable(context.Background(), fake.NewSimpleClientset(append(pods, dep)...), "ns", "epp")
}

// A pod failing on its node is reported before a pod still waiting for a node,
// even one listed first and waiting for longer: the wait has the longer
// startup grace, so it must not hide the failure.
func TestDeploymentPodUnavailablePrefersPodOnNode(t *testing.T) {
	got, ok := deploymentPodUnavailable(t,
		testPod("a-waiting", "", corev1.PodPending,
			condition(corev1.PodScheduled, corev1.ConditionFalse, "Unschedulable", "0/3 nodes are available: 3 Insufficient cpu.", created)),
		testPod("b-crashing", "n1", corev1.PodRunning,
			condition(corev1.PodReady, corev1.ConditionFalse, "ContainersNotReady", "", created.Add(time.Minute))),
		testPod("c-serving", "n0", corev1.PodRunning, condition(corev1.PodReady, corev1.ConditionTrue, "", "", created)))
	want := PodUnavailability{Cause: "ContainersNotReady", NodeName: "n1", Since: created.Add(time.Minute)}
	if !ok || got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A pod that got its node but has no kubelet status yet is timed from its node
// assignment, not from its creation, which may predate a long wait for a node.
func TestDeploymentPodUnavailableTimesAssignedPodFromItsNode(t *testing.T) {
	assigned := created.Add(109 * time.Second)
	got, ok := deploymentPodUnavailable(t,
		testPod("assigned", "n2", corev1.PodPending, condition(corev1.PodScheduled, corev1.ConditionTrue, "", "", assigned)))
	want := PodUnavailability{Cause: "pod pending", NodeName: "n2", Since: assigned}
	if !ok || got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDaemonSetPodUnavailableMarksPodAwaitingNode(t *testing.T) {
	cs := fake.NewSimpleClientset(csiNodeDaemonSet,
		testPod("waiting", "", corev1.PodPending, condition(corev1.PodScheduled, corev1.ConditionFalse, "Unschedulable", "", created)),
		testPod("simulated", "fake-gpu", corev1.PodRunning, condition(corev1.PodReady, corev1.ConditionFalse, "", "", created)))
	got, ok := DaemonSetPodUnavailable(context.Background(), cs, "ns", "csi-node", func(node string) bool { return node == "fake-gpu" }, nil)
	if !ok || !got.AwaitingNode || got.Cause != "Unschedulable" {
		t.Fatalf("want only the pod awaiting a node, got %+v", got)
	}
}

// A DaemonSet pod is bound to a node as soon as the node registers, so on a new
// node it is still waiting for its node, while a pod failing on an older node
// is reported first.
func TestDaemonSetPodUnavailableWaitsForNewNode(t *testing.T) {
	starting := testPod("a-starting", "new", corev1.PodPending,
		condition(corev1.PodReady, corev1.ConditionFalse, "ContainersNotReady", "", created))
	failing := testPod("b-failing", "old", corev1.PodRunning,
		condition(corev1.PodReady, corev1.ConditionFalse, "ContainersNotReady", "", created))
	newNode := func(node string) bool { return node == "new" }

	got, ok := DaemonSetPodUnavailable(context.Background(), fake.NewSimpleClientset(csiNodeDaemonSet, starting), "ns", "csi-node", nil, newNode)
	if !ok || !got.AwaitingNode || got.NodeName != "new" {
		t.Fatalf("a pod on a new node must wait for its node, got %+v", got)
	}
	got, ok = DaemonSetPodUnavailable(context.Background(), fake.NewSimpleClientset(csiNodeDaemonSet, starting, failing), "ns", "csi-node", nil, newNode)
	if !ok || got.AwaitingNode || got.NodeName != "old" {
		t.Fatalf("a pod failing on an older node must be reported first, got %+v", got)
	}
}

// A pod the kubelet evicted stays Failed until it is deleted, while a
// replacement serves: it must not flag the Deployment. With only the evicted
// pod left, nothing runs for the Deployment.
func TestDeploymentPodUnavailableIgnoresEvictedPod(t *testing.T) {
	evicted := testPod("a-evicted", "n1", corev1.PodFailed,
		condition(corev1.PodReady, corev1.ConditionFalse, "PodFailed", "", created))
	evicted.Status.Reason = "Evicted"
	serving := testPod("b-serving", "n0", corev1.PodRunning, condition(corev1.PodReady, corev1.ConditionTrue, "", "", created))
	if got, ok := deploymentPodUnavailable(t, evicted, serving); ok {
		t.Fatalf("an evicted pod must not flag the Deployment, got %+v", got)
	}
	got, ok := deploymentPodUnavailable(t, evicted)
	if want := (PodUnavailability{Cause: "no pods are running for the Deployment"}); !ok || got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
