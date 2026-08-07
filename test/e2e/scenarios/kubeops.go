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

package scenarios

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
)

// MetricSnapshot holds a metric value per pod (keyed by pod name) at a
// point in time. DiffSnapshots/TotalMetricDelta below let assertions
// compute per-pod and aggregate deltas across two snapshots.
type MetricSnapshot map[string]float64

// DiffSnapshots returns after[k]-before[k] for every pod present in
// after (pods that disappeared between snapshots are dropped, not
// counted as a negative delta).
func DiffSnapshots(before, after MetricSnapshot) MetricSnapshot {
	diff := make(MetricSnapshot, len(after))
	for k, av := range after {
		diff[k] = av - before[k]
	}
	return diff
}

// TotalMetricDelta sums every value in a diff snapshot.
func TotalMetricDelta(diff MetricSnapshot) float64 {
	var total float64
	for _, v := range diff {
		total += v
	}
	return total
}

// KubeOps is the narrow set of direct Kubernetes operations the
// filter-execution-order and BBR-cluster-filter-HA scenario groups need
// beyond what Lifecycle already covers (pod/log inspection, in-pod exec,
// Deployment scaling, and vLLM/EPP metric scraping). Groups that don't
// need direct cluster access (API-key auth, GPU/framework smoke) accept a
// nil KubeOps.
//
// Implementations: a Kubernetes-clientset-backed adapter
// (test/e2e/utils/kubeops_helm.go) and a fake for unit tests.
type KubeOps interface {
	// GetDeployment returns the named Deployment.
	GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error)
	// ScaleDeployment sets spec.replicas via the scale subresource.
	// Does not block for the rollout to converge — see
	// WaitForReadyReplicas.
	ScaleDeployment(ctx context.Context, namespace, name string, replicas int32) error
	// ReadyReplicas returns the Deployment's current ready replica count.
	ReadyReplicas(ctx context.Context, namespace, name string) (int32, error)
	// WaitForReadyReplicas blocks until the Deployment reports >= want
	// ready replicas, or the timeout elapses.
	WaitForReadyReplicas(ctx context.Context, namespace, name string, want int32, timeout time.Duration) error

	// FirstRunningPod returns the name of the first Running pod matching
	// labelSelector in namespace.
	FirstRunningPod(ctx context.Context, namespace, labelSelector string) (string, error)
	// PodLogs returns the current logs of one container in one pod.
	PodLogs(ctx context.Context, namespace, pod, container string) (string, error)
	// AggregatePodSetLogLen sums the current log length across every
	// Running pod matching labelSelector (used where traffic may land on
	// any replica of an HA Deployment, so a single pod's log is not a
	// reliable signal).
	AggregatePodSetLogLen(ctx context.Context, namespace, labelSelector, container string) (int, error)
	// ExecInPod runs `command` inside pod/namespace and returns combined
	// output (used to curl the Envoy admin /config_dump endpoint).
	ExecInPod(ctx context.Context, namespace, pod string, command ...string) (string, error)

	// RequestSuccessTotalSnapshot scrapes vLLM's per-pod
	// `vllm:request_success_total{model_name=model}` counter across the
	// namespace's shadow pods.
	RequestSuccessTotalSnapshot(ctx context.Context, namespace, model string) (MetricSnapshot, error)
	// EPPPodNames returns the EPP pod name(s) for a deployment.
	EPPPodNames(ctx context.Context, namespace, deploymentName string) ([]string, error)
}
