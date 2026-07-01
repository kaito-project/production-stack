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

package scraper

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/util/kube"
	"github.com/kaito-project/production-stack/pkg/util/podmetrics"
)

const (
	// labelCreatedBy identifies the InferenceSet that owns a pod. The vLLM
	// workload pods are selected via this label, e.g.
	// `inferenceset.kaito.sh/created-by=<inferenceset-name>`.
	labelCreatedBy = "inferenceset.kaito.sh/created-by"

	// defaultMetricsPort is the port the vLLM pod exposes Prometheus metrics on.
	defaultMetricsPort = 5000

	// vllmNativeMetricPrefix is the prefix of vLLM's own Prometheus metrics
	// (e.g. vllm:num_requests_running). During weight download the pod's
	// /metrics endpoint is served by the pre-download server and exposes only
	// the kaito_* download gauges; once the engine has loaded and vLLM's HTTP
	// server takes over, /metrics starts emitting vllm:* metrics. Their presence
	// is therefore an unambiguous signal that the download already finished.
	vllmNativeMetricPrefix = "vllm:"

	// DefaultMetricName is the Prometheus gauge (bytes/s) the vLLM scraper reads
	// to derive model-weights download throughput. It is a real metric exposed by
	// the vLLM pod.
	//
	// kaito_model_download_speed_bytes_per_second: Current model download speed in
	// bytes per second.
	DefaultMetricName = "kaito_model_download_speed_bytes_per_second"

	// bytesPerMB converts a raw bytes/second gauge into MB/s.
	bytesPerMB = 1_000_000.0
)

// vllmPodScraper reads the model-weights download throughput from an
// InferenceSet's vLLM workload pod. It owns the vLLM-specific behaviour — the
// pod label selector and filtering and the download-finished condition — and
// delegates the generic /metrics scraping to a podmetrics.Client. The gauge
// name to read is supplied per call to Scrape.
type vllmPodScraper struct {
	clientset kubernetes.Interface
	pods      *podmetrics.Client
	// listRawFunc overrides the clientset-backed pod List. It is a test seam:
	// nil in production, in which case the clientset is used.
	listRawFunc func(ctx context.Context, namespace, selector string) (*corev1.PodList, error)
}

// NewVLLMScraper returns a MetricScraper that scrapes the model-weights
// download throughput directly from the InferenceSet's vLLM workload pod.
// metricPort defaults to defaultMetricsPort when zero; the throughput gauge
// name is supplied per call to Scrape.
func NewVLLMScraper(clientset kubernetes.Interface, metricPort int) MetricScraper {
	if metricPort == 0 {
		metricPort = defaultMetricsPort
	}
	return &vllmPodScraper{
		clientset: clientset,
		pods:      podmetrics.NewClient(clientset, metricPort),
	}
}

func (s *vllmPodScraper) Name() string { return "vllm" }

func (s *vllmPodScraper) Scrape(ctx context.Context, target ScrapeTarget, metricName string) (ThroughputSample, bool, error) {
	if metricName == "" {
		metricName = DefaultMetricName
	}
	pods, err := s.listPods(ctx, target.Namespace, target.InferenceSet)
	if err != nil {
		return ThroughputSample{}, false, fmt.Errorf("vllm scraper: %w", err)
	}
	snap, found, err := s.pods.Snapshot(ctx, target.Namespace, pods)
	if err != nil {
		return ThroughputSample{}, false, fmt.Errorf("vllm scraper: %w", err)
	}
	if !found {
		// No still-downloading pod, or none served parseable metrics (download
		// not started / already finished).
		return ThroughputSample{}, false, nil
	}

	// vLLM-specific download-finished condition: the engine's native vllm:*
	// metrics are exposed only after the model has loaded, so their presence
	// means the weight download already finished. Report no sample so the
	// caller resolves the window.
	if snap.HasMetricWithPrefix(vllmNativeMetricPrefix) {
		return ThroughputSample{}, false, nil
	}

	m, ok := snap.Value(metricName)
	if !ok {
		// The pod serves metrics but not the throughput gauge yet.
		return ThroughputSample{}, false, nil
	}
	return ThroughputSample{
		Timestamp: snap.ScrapedAt,
		MBps:      m.Value / bytesPerMB,
		SourcePod: m.SourcePod,
	}, true, nil
}

// listPods returns the InferenceSet's vLLM model pods that are still
// downloading weights and can therefore serve the kaito_* throughput gauge:
// those whose Ready condition is False (still starting up). KAITO's pre-download
// metrics server exposes the gauge on the vLLM port precisely while the pod is
// Ready=False, and hands the port to vLLM — flipping the pod to Ready — the
// moment the download completes, so a Ready pod has nothing to report and is
// skipped.
//
// Following the same terminal-container classification used for
// inferencesetModelPodsNotReady (kube.PodTerminalContainerCause), a pod being
// deleted, or one with a terminal container failure (CrashLoopBackOff /
// ImagePullBackOff / OOMKilled / ...), is skipped too: the former is a rollout
// and the latter is a real failure surfaced elsewhere, neither a healthy
// in-progress download.
func (s *vllmPodScraper) listPods(ctx context.Context, namespace, inferenceSetName string) ([]string, error) {
	selector := fmt.Sprintf("%s=%s", labelCreatedBy, inferenceSetName)
	list, err := s.listRaw(ctx, namespace, selector)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil {
			continue // being deleted during a rollout — not a download in progress
		}
		ready := kube.PodReadyCondition(p)
		if ready == nil || ready.Status != corev1.ConditionFalse {
			continue // no Ready condition yet, or already Ready (serving) — nothing to sample
		}
		if _, terminal := kube.PodTerminalContainerCause(p); terminal {
			continue // terminal container failure — not a healthy in-progress download
		}
		names = append(names, p.Name)
	}
	return names, nil
}

// listRaw lists the pods matching selector, via the clientset unless a test
// seam is installed.
func (s *vllmPodScraper) listRaw(ctx context.Context, namespace, selector string) (*corev1.PodList, error) {
	if s.listRawFunc != nil {
		return s.listRawFunc(ctx, namespace, selector)
	}
	return s.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
}
