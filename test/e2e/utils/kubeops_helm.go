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
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/scenarios"
)

// KubeOpsHelm implements scenarios.KubeOps directly against a live
// cluster's Kubernetes clientset, reusing this package's existing
// clientset/metrics/pod-log helpers. It backs the filter-execution-order
// and BBR-cluster-filter-HA scenario groups when run through `make
// test-e2e`.
type KubeOpsHelm struct{}

var _ scenarios.KubeOps = KubeOpsHelm{}

// GetDeployment implements scenarios.KubeOps.
func (KubeOpsHelm) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	cs, err := GetK8sClientset()
	if err != nil {
		return nil, err
	}
	return cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
}

// ScaleDeployment implements scenarios.KubeOps.
func (KubeOpsHelm) ScaleDeployment(ctx context.Context, namespace, name string, replicas int32) error {
	return ScaleDeployment(ctx, namespace, name, replicas)
}

// ReadyReplicas implements scenarios.KubeOps.
func (KubeOpsHelm) ReadyReplicas(ctx context.Context, namespace, name string) (int32, error) {
	_, ready, err := GetDeploymentReplicas(ctx, namespace, name)
	return ready, err
}

// WaitForReadyReplicas implements scenarios.KubeOps.
func (KubeOpsHelm) WaitForReadyReplicas(ctx context.Context, namespace, name string, want int32, timeout time.Duration) error {
	return WaitForDeploymentReplicas(ctx, namespace, name, want, timeout)
}

// FirstRunningPod implements scenarios.KubeOps.
func (KubeOpsHelm) FirstRunningPod(ctx context.Context, namespace, labelSelector string) (string, error) {
	clientset, err := GetK8sClientset()
	if err != nil {
		return "", err
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return "", fmt.Errorf("list pods in %s with %q: %w", namespace, labelSelector, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no Running pods in %s match %q", namespace, labelSelector)
	}
	return pods.Items[0].Name, nil
}

// PodLogs implements scenarios.KubeOps.
func (KubeOpsHelm) PodLogs(_ context.Context, namespace, pod, container string) (string, error) {
	clientset, err := GetK8sClientset()
	if err != nil {
		return "", err
	}
	return GetPodLogs(clientset, namespace, pod, container)
}

// AggregatePodSetLogLen implements scenarios.KubeOps. BBR (and any other
// HA cluster-wide Deployment) load-balances traffic across every ready
// replica, so a single pod's log is not a reliable signal that traffic
// transited the Deployment at all — sum log length across every Running
// pod matching labelSelector instead.
func (KubeOpsHelm) AggregatePodSetLogLen(ctx context.Context, namespace, labelSelector, container string) (int, error) {
	clientset, err := GetK8sClientset()
	if err != nil {
		return 0, err
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return 0, err
	}
	if len(pods.Items) == 0 {
		return 0, fmt.Errorf("no Running pods in %s match %q", namespace, labelSelector)
	}
	total := 0
	for i := range pods.Items {
		logs, err := GetPodLogs(clientset, namespace, pods.Items[i].Name, container)
		if err != nil {
			return 0, err
		}
		total += len(logs)
	}
	return total, nil
}

// ExecInPod implements scenarios.KubeOps. Shells out to `kubectl exec`
// (rather than the K8s REST exec subresource) to stay consistent with
// the rest of this suite, which already shells out for port-forward /
// helm / kubectl operations.
func (KubeOpsHelm) ExecInPod(_ context.Context, namespace, pod string, command ...string) (string, error) {
	args := append([]string{"exec", "-n", namespace, pod, "--"}, command...)
	cmd := exec.Command("kubectl", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("kubectl %s: %w (output: %.1000s)",
			strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

// RequestSuccessTotalSnapshot implements scenarios.KubeOps.
func (KubeOpsHelm) RequestSuccessTotalSnapshot(ctx context.Context, namespace, model string) (scenarios.MetricSnapshot, error) {
	clientset, err := GetK8sClientset()
	if err != nil {
		return nil, err
	}
	snap, err := ScrapeRequestSuccessTotal(ctx, clientset, namespace, model)
	if err != nil {
		return nil, err
	}
	out := make(scenarios.MetricSnapshot, len(snap))
	for k, v := range snap {
		out[k] = v
	}
	return out, nil
}

// EPPPodNames implements scenarios.KubeOps.
func (KubeOpsHelm) EPPPodNames(ctx context.Context, namespace, deploymentName string) ([]string, error) {
	clientset, err := GetK8sClientset()
	if err != nil {
		return nil, err
	}
	pods, err := GetEPPPods(ctx, clientset, deploymentName, namespace)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(pods))
	for i, p := range pods {
		names[i] = p.Name
	}
	return names, nil
}
