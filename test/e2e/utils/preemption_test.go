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
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

func TestPreemptionWatcher(t *testing.T) {
	// newPod returns a pod whose DisruptionTarget condition has reason, or a
	// pod without one if reason is empty.
	newPod := func(name, priorityClass, reason string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "workload", Name: name, UID: types.UID(name)},
			Spec:       corev1.PodSpec{PriorityClassName: priorityClass, NodeName: "node-1"},
		}
		if reason != "" {
			pod.Status.Conditions = []corev1.PodCondition{{
				Type:   corev1.DisruptionTarget,
				Status: corev1.ConditionTrue,
				Reason: reason,
			}}
		}
		return pod
	}
	const preempted = corev1.PodReasonPreemptionByScheduler

	w := &preemptionWatcher{seen: map[types.UID]bool{}}
	// Preempted before the watch started: listed, then updated and deleted.
	w.record(newPod("phi-inferencepool-epp-6c8d9-old01", "", preempted), true)
	w.record(newPod("phi-inferencepool-epp-6c8d9-old01", "", preempted), false)
	// Updated without the condition, then with it, then again while it terminates.
	w.record(newPod("phi-inferencepool-epp-6c8d9-x1y2z", "", ""), false)
	w.record(newPod("phi-inferencepool-epp-6c8d9-x1y2z", "", preempted), false)
	w.record(newPod("phi-inferencepool-epp-6c8d9-x1y2z", "", preempted), false)
	// Evicted, e.g. by a consolidation drain: not a preemption.
	w.record(newPod("shadow-workload-falcon-0", "", "EvictionByEvictionAPI"), false)
	// Seen only in its deletion tombstone.
	w.record(cache.DeletedFinalStateUnknown{Obj: newPod("body-based-router-5b7f4-a1b2c", systemClusterCritical, preempted)}, false)
	w.record(newPod("apikey-authz-7c9d5-d3e4f", "", preempted), false)

	var got []string
	for _, p := range w.preemptions {
		got = append(got, fmt.Sprintf("%s fails=%t", p.pod.Name, p.toleratedBecause() == ""))
	}
	want := []string{
		"phi-inferencepool-epp-6c8d9-x1y2z fails=true",
		"body-based-router-5b7f4-a1b2c fails=false", // only a system-node-critical pod can preempt it
		"apikey-authz-7c9d5-d3e4f fails=false",      // known gap
	}
	if !slices.Equal(got, want) {
		t.Errorf("recorded preemptions = %q, want %q", got, want)
	}
}
