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

func deploymentPodsUnavailable(t *testing.T, pods ...runtime.Object) []PodUnavailability {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "epp"},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
	return DeploymentPodsUnavailable(context.Background(), fake.NewSimpleClientset(append(pods, dep)...), "ns", "epp")
}

// Every unavailable pod is returned with its own anchor, so a pod still
// waiting for a node cannot hide one that is failing on its node.
func TestDeploymentPodsUnavailableReturnsEveryPod(t *testing.T) {
	got := deploymentPodsUnavailable(t,
		testPod("serving", "n0", corev1.PodRunning, condition(corev1.PodReady, corev1.ConditionTrue, "", "", created)),
		testPod("waiting", "", corev1.PodPending,
			condition(corev1.PodScheduled, corev1.ConditionFalse, "Unschedulable", "0/3 nodes are available: 3 Insufficient cpu.", created.Add(time.Second))),
		testPod("crashing", "n1", corev1.PodRunning,
			condition(corev1.PodReady, corev1.ConditionFalse, "ContainersNotReady", "", created.Add(time.Minute))),
	)
	if len(got) != 2 {
		t.Fatalf("want the waiting and the crashing pod, got %+v", got)
	}
	for _, u := range got {
		want := PodUnavailability{Cause: "ContainersNotReady", NodeName: "n1", Since: created.Add(time.Minute)}
		if u.AwaitingNode {
			want = PodUnavailability{
				Cause:        "Unschedulable: 0/3 nodes are available: 3 Insufficient cpu.",
				Since:        created.Add(time.Second),
				AwaitingNode: true,
			}
		}
		if u != want {
			t.Errorf("got %+v, want %+v", u, want)
		}
	}
}

// A pod that got its node but has no kubelet status yet is timed from its node
// assignment, not from its creation, which may predate a long wait for a node.
func TestDeploymentPodsUnavailableTimesAssignedPodFromItsNode(t *testing.T) {
	assigned := created.Add(109 * time.Second)
	got := deploymentPodsUnavailable(t,
		testPod("assigned", "n2", corev1.PodPending, condition(corev1.PodScheduled, corev1.ConditionTrue, "", "", assigned)))
	want := PodUnavailability{Cause: "pod pending", NodeName: "n2", Since: assigned}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want [%+v]", got, want)
	}
}

func TestDaemonSetPodsUnavailableMarksPodAwaitingNode(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "csi-node"},
		Spec:       appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
	cs := fake.NewSimpleClientset(ds,
		testPod("waiting", "", corev1.PodPending, condition(corev1.PodScheduled, corev1.ConditionFalse, "Unschedulable", "", created)),
		testPod("simulated", "fake-gpu", corev1.PodRunning, condition(corev1.PodReady, corev1.ConditionFalse, "", "", created)))
	got := DaemonSetPodsUnavailable(context.Background(), cs, "ns", "csi-node", func(node string) bool { return node == "fake-gpu" })
	if len(got) != 1 || !got[0].AwaitingNode || got[0].Cause != "Unschedulable" {
		t.Fatalf("want only the pod awaiting a node, got %+v", got)
	}
}
