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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/scenarios"
)

// diagnosticsMaxPodsPerNamespace/diagnosticsMaxLogBytes bound how much
// DiagnosticsHelm collects and logs on a failed assertion — a runaway
// dump is worse than none, and this output feeds a human debugging a CI
// failure, not a machine.
const (
	diagnosticsMaxPodsPerNamespace = 10
	diagnosticsMaxLogBytes         = 4000
)

// DiagnosticsHelm implements scenarios.Diagnostics by dumping a bounded
// summary of every pod's phase (and, for non-Running pods, a tail of
// their first container's logs) in each namespace the failed group
// owned. It never returns an error: collection failures are themselves
// logged via the injected Logger and otherwise ignored, so a diagnostics
// hiccup can never mask the original assertion failure.
type DiagnosticsHelm struct{}

var _ scenarios.Diagnostics = DiagnosticsHelm{}

// Collect implements scenarios.Diagnostics.
func (DiagnosticsHelm) Collect(ctx context.Context, group scenarios.GroupResources, log scenarios.Logger) {
	clientset, err := GetK8sClientset()
	if err != nil {
		log.Logf("diagnostics: failed to build clientset: %v", err)
		return
	}

	for _, ns := range group.Namespaces {
		pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Logf("diagnostics: list pods in %s: %v", ns, err)
			continue
		}
		log.Logf("diagnostics: namespace %s has %d pod(s)", ns, len(pods.Items))

		items := pods.Items
		if len(items) > diagnosticsMaxPodsPerNamespace {
			items = items[:diagnosticsMaxPodsPerNamespace]
		}
		for _, pod := range items {
			log.Logf("diagnostics: pod %s/%s phase=%s", ns, pod.Name, pod.Status.Phase)
			if pod.Status.Phase == "Running" || len(pod.Spec.Containers) == 0 {
				continue
			}
			logs, err := GetPodLogs(clientset, ns, pod.Name, pod.Spec.Containers[0].Name)
			if err != nil {
				log.Logf("diagnostics: get logs for %s/%s: %v", ns, pod.Name, err)
				continue
			}
			if len(logs) > diagnosticsMaxLogBytes {
				logs = logs[len(logs)-diagnosticsMaxLogBytes:]
			}
			log.Logf("diagnostics: %s/%s (%s) log tail:\n%s", ns, pod.Name, pod.Spec.Containers[0].Name, logs)
		}
	}
}
