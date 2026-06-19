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
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fixedNow is the deterministic clock installed on the scraper under test so a
// returned sample's Timestamp can be asserted.
var fixedNow = time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

// fakeCluster serves canned pod/metrics fixtures to a vllmPodScraper and
// records the selector and port it was queried with, so Scrape can be
// exercised without a real cluster.
type fakeCluster struct {
	// pods is keyed by namespace; metrics is keyed by "namespace/pod".
	pods    map[string][]corev1.Pod
	metrics map[string]string
	// listErr / scrapeErr, when set, are returned by the corresponding seam.
	listErr   error
	scrapeErr error

	// Captured inputs from the most recent calls.
	gotSelector string
	gotPort     int
}

// installFakes points the scraper's pod discovery and /metrics scraping at fc
// and pins its clock to fixedNow.
func installFakes(sc *vllmPodScraper, fc *fakeCluster) {
	sc.pods.NowFunc = func() time.Time { return fixedNow }
	sc.listRawFunc = func(_ context.Context, namespace, selector string) (*corev1.PodList, error) {
		fc.gotSelector = selector
		if fc.listErr != nil {
			return nil, fc.listErr
		}
		return &corev1.PodList{Items: fc.pods[namespace]}, nil
	}
	sc.pods.ScrapeRawFunc = func(_ context.Context, namespace, pod string, port int) (string, error) {
		fc.gotPort = port
		if fc.scrapeErr != nil {
			return "", fc.scrapeErr
		}
		return fc.metrics[namespace+"/"+pod], nil
	}
}

// downloadingPod is a Running model pod whose Ready condition is False — i.e.
// still downloading weights, the state the scraper samples.
func downloadingPod(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}
}

// readyPod is a Running model pod whose Ready condition is True — the download
// has finished and vLLM is serving.
func readyPod(name string) corev1.Pod {
	p := downloadingPod(name)
	p.Status.Conditions[0].Status = corev1.ConditionTrue
	return p
}

// qwenTarget is the target every test scrapes.
var qwenTarget = ScrapeTarget{Namespace: "team-a", InferenceSet: "qwen"}

func TestVLLMScraperReadsThroughputSample(t *testing.T) {
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	fc := &fakeCluster{
		pods: map[string][]corev1.Pod{
			"team-a": {downloadingPod("qwen-0")},
		},
		metrics: map[string]string{
			"team-a/qwen-0": `kaito_model_download_speed_bytes_per_second 8.0e6`,
		},
	}
	installFakes(sc, fc)

	sample, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if err != nil || !found {
		t.Fatalf("expected a sample, got found=%v err=%v", found, err)
	}
	if sample.MBps != 8.0 {
		t.Fatalf("MBps=%v, want 8.0 (8.0e6 bytes/s)", sample.MBps)
	}
	if sample.SourcePod != "qwen-0" {
		t.Fatalf("SourcePod=%q, want qwen-0", sample.SourcePod)
	}
	if !sample.Timestamp.Equal(fixedNow) {
		t.Fatalf("Timestamp=%v, want %v (scraper clock)", sample.Timestamp, fixedNow)
	}
	if fc.gotSelector != "inferenceset.kaito.sh/created-by=qwen" {
		t.Fatalf("selector=%q, want inferenceset.kaito.sh/created-by=qwen", fc.gotSelector)
	}
	if fc.gotPort != defaultMetricsPort {
		t.Fatalf("port=%d, want %d (default)", fc.gotPort, defaultMetricsPort)
	}
}

func TestVLLMScraperUsesFirstPodSample(t *testing.T) {
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	fc := &fakeCluster{
		pods: map[string][]corev1.Pod{
			"team-a": {downloadingPod("qwen-0"), downloadingPod("qwen-1")},
		},
		metrics: map[string]string{
			"team-a/qwen-0": `kaito_model_download_speed_bytes_per_second 8.0e6`,
			"team-a/qwen-1": `kaito_model_download_speed_bytes_per_second 12.0e6`,
		},
	}
	installFakes(sc, fc)

	sample, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if err != nil || !found {
		t.Fatalf("expected a sample, got found=%v err=%v", found, err)
	}
	if sample.SourcePod != "qwen-0" || sample.MBps != 8.0 {
		t.Fatalf("got SourcePod=%q MBps=%v, want qwen-0 / 8.0 (first pod)", sample.SourcePod, sample.MBps)
	}
}

func TestVLLMScraperNoPodsReturnsNotFound(t *testing.T) {
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	installFakes(sc, &fakeCluster{pods: map[string][]corev1.Pod{}})

	_, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false when no source pod exists")
	}
}

func TestVLLMScraperThroughputMetricAbsentReturnsNotFound(t *testing.T) {
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	installFakes(sc, &fakeCluster{
		pods: map[string][]corev1.Pod{
			"team-a": {downloadingPod("qwen-0")},
		},
		metrics: map[string]string{
			"team-a/qwen-0": `unrelated_metric 1`,
		},
	})

	_, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false when the throughput gauge is absent")
	}
}

func TestVLLMScraperDownloadFinishedReturnsNotFound(t *testing.T) {
	// Once vLLM's native metrics appear the download has finished, so the
	// scraper reports no sample even though the throughput gauge is still
	// present in the same scrape.
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	installFakes(sc, &fakeCluster{
		pods: map[string][]corev1.Pod{
			"team-a": {downloadingPod("qwen-0")},
		},
		metrics: map[string]string{
			"team-a/qwen-0": "kaito_model_download_speed_bytes_per_second 8.0e6\nvllm:num_requests_running 0",
		},
	})

	_, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false once vLLM native metrics appear (download finished)")
	}
}

func TestVLLMScraperSkipsReadyPod(t *testing.T) {
	// A Ready pod has finished downloading and is serving traffic, so it is not
	// scraped even though it still exposes the throughput gauge.
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	installFakes(sc, &fakeCluster{
		pods: map[string][]corev1.Pod{
			"team-a": {readyPod("qwen-0")},
		},
		metrics: map[string]string{
			"team-a/qwen-0": `kaito_model_download_speed_bytes_per_second 8.0e6`,
		},
	})

	_, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false when the only pod is Ready (download finished)")
	}
}

func TestVLLMScraperSkipsDeletingPod(t *testing.T) {
	// A pod being deleted during a rollout is not a download in progress, so it
	// is skipped even though it is Ready=False and exposes the gauge.
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	deleting := downloadingPod("qwen-0")
	now := metav1.NewTime(fixedNow)
	deleting.DeletionTimestamp = &now
	installFakes(sc, &fakeCluster{
		pods: map[string][]corev1.Pod{
			"team-a": {deleting},
		},
		metrics: map[string]string{
			"team-a/qwen-0": `kaito_model_download_speed_bytes_per_second 8.0e6`,
		},
	})

	_, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false when the only pod is being deleted")
	}
}

func TestVLLMScraperSkipsTerminalPod(t *testing.T) {
	// A pod with a terminal container failure (CrashLoopBackOff) is a real
	// failure surfaced elsewhere, not a healthy in-progress download, so it is
	// skipped even though it is Ready=False and exposes the gauge.
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	terminal := downloadingPod("qwen-0")
	terminal.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "vllm",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}
	installFakes(sc, &fakeCluster{
		pods: map[string][]corev1.Pod{
			"team-a": {terminal},
		},
		metrics: map[string]string{
			"team-a/qwen-0": `kaito_model_download_speed_bytes_per_second 8.0e6`,
		},
	})

	_, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false when the only pod has a terminal container failure")
	}
}

func TestVLLMScraperListErrorWrapped(t *testing.T) {
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	wantErr := errors.New("api server unreachable")
	installFakes(sc, &fakeCluster{listErr: wantErr})

	_, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if found {
		t.Fatalf("expected found=false on list error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error %v should wrap %v", err, wantErr)
	}
}

func TestVLLMScraperScrapeErrorWrapped(t *testing.T) {
	sc := NewVLLMScraper(nil, 0).(*vllmPodScraper)
	wantErr := errors.New("proxy timeout")
	installFakes(sc, &fakeCluster{
		pods: map[string][]corev1.Pod{
			"team-a": {downloadingPod("qwen-0")},
		},
		scrapeErr: wantErr,
	})

	_, found, err := sc.Scrape(context.Background(), qwenTarget, "")
	if found {
		t.Fatalf("expected found=false when scraping the pod fails")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error %v should wrap %v", err, wantErr)
	}
}

func TestVLLMScraperHonorsCustomMetricNameAndPort(t *testing.T) {
	sc := NewVLLMScraper(nil, 9000).(*vllmPodScraper)
	fc := &fakeCluster{
		pods: map[string][]corev1.Pod{
			"team-a": {downloadingPod("qwen-0")},
		},
		metrics: map[string]string{
			"team-a/qwen-0": `custom_download_bytes_per_second 5.0e6`,
		},
	}
	installFakes(sc, fc)

	sample, found, err := sc.Scrape(context.Background(), qwenTarget, "custom_download_bytes_per_second")
	if err != nil || !found {
		t.Fatalf("expected a sample, got found=%v err=%v", found, err)
	}
	if sample.MBps != 5.0 {
		t.Fatalf("MBps=%v, want 5.0 (5.0e6 bytes/s)", sample.MBps)
	}
	if fc.gotPort != 9000 {
		t.Fatalf("port=%d, want 9000 (custom)", fc.gotPort)
	}
}
