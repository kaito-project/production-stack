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

package downloader

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/util/kube"
	"github.com/kaito-project/production-stack/pkg/util/podmetrics"
)

const (
	// labelCreatedBy identifies the InferenceSet that owns a pod, e.g.
	// `inferenceset.kaito.sh/created-by=<inferenceset-name>`.
	labelCreatedBy = "inferenceset.kaito.sh/created-by"

	// vllmNativeMetricPrefix is the prefix of vLLM's own Prometheus metrics
	// (e.g. vllm:num_requests_running). During weight download the pod's
	// /metrics endpoint is served by KAITO's pre-download server and exposes
	// only the kaito_* download gauges; once the engine has loaded, /metrics
	// emits vllm:* metrics, so their presence means the download finished.
	vllmNativeMetricPrefix = "vllm:"
)

// VLLMPodDownloader observes the vLLM self-download: each not-yet-Ready model
// pod of the InferenceSet downloads its own weights and exposes the throughput
// gauge on its /metrics endpoint.
type VLLMPodDownloader struct {
	clientset  kubernetes.Interface
	pods       *podmetrics.Client
	metricName string

	// listRawFunc overrides the clientset-backed pod List. It is a test seam:
	// nil in production, in which case the clientset is used.
	listRawFunc func(ctx context.Context, namespace, selector string) (*corev1.PodList, error)
}

// NewVLLMPodDownloader returns a downloader that scrapes metricName on
// metricPort from the InferenceSet's model pods.
func NewVLLMPodDownloader(clientset kubernetes.Interface, metricName string, metricPort int) *VLLMPodDownloader {
	return &VLLMPodDownloader{
		clientset:  clientset,
		pods:       podmetrics.NewClient(clientset, metricPort),
		metricName: metricName,
	}
}

// Observe scrapes every not-yet-Ready model pod of the InferenceSet.
//   - Downloading: at least one pod reports the throughput gauge; the slowest
//     such pod is reported.
//   - Completed: pods exist and every one is Ready or already exposes vLLM's
//     native metrics, i.e. its weights are loaded.
//   - NotStarted: anything else (no pods yet, pods not serving metrics yet, or
//     pods in a terminal container failure that is surfaced elsewhere).
//
// A scrape error is returned only when no pod could be classified as
// downloading.
func (d *VLLMPodDownloader) Observe(ctx context.Context, is *unstructured.Unstructured) (DownloadStatus, error) {
	namespace, name := is.GetNamespace(), is.GetName()
	list, err := d.listRaw(ctx, namespace, fmt.Sprintf("%s=%s", labelCreatedBy, name))
	if err != nil {
		return DownloadStatus{}, fmt.Errorf("list model pods of InferenceSet %s/%s: %w", namespace, name, err)
	}

	var (
		pods     int
		loaded   int
		slowest  *DownloadStatus
		firstErr error
	)
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil {
			continue // being deleted during a rollout
		}
		pods++
		ready := kube.PodReadyCondition(p)
		if ready != nil && ready.Status == corev1.ConditionTrue {
			loaded++
			continue
		}
		if ready == nil {
			continue // not started by the kubelet yet
		}
		if _, terminal := kube.PodTerminalContainerCause(p); terminal {
			continue // a real failure, surfaced through InferenceSetReady
		}
		snap, found, err := d.pods.Snapshot(ctx, namespace, []string{p.Name})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !found {
			continue
		}
		if snap.HasMetricWithPrefix(vllmNativeMetricPrefix) {
			loaded++
			continue
		}
		m, ok := snap.Value(d.metricName)
		if !ok {
			continue
		}
		if slowest == nil || m.Value < slowest.BytesPerSecond {
			slowest = &DownloadStatus{Phase: PhaseDownloading, BytesPerSecond: m.Value, Source: "model-pod " + p.Name}
		}
	}

	switch {
	case slowest != nil:
		return *slowest, nil
	case pods > 0 && loaded == pods:
		return DownloadStatus{Phase: PhaseCompleted}, nil
	default:
		return DownloadStatus{Phase: PhaseNotStarted}, firstErr
	}
}

// listRaw lists the pods matching selector, via the clientset unless a test
// seam is installed.
func (d *VLLMPodDownloader) listRaw(ctx context.Context, namespace, selector string) (*corev1.PodList, error) {
	if d.listRawFunc != nil {
		return d.listRawFunc(ctx, namespace, selector)
	}
	return d.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
}
