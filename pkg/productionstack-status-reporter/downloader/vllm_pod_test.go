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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeCluster serves canned pods and /metrics bodies to a VLLMPodDownloader
// and records the selector and port it was queried with.
type fakeCluster struct {
	pods      []corev1.Pod
	metrics   map[string]string // keyed by pod name
	scrapeErr map[string]error  // keyed by pod name

	gotSelector string
	gotPort     int
}

func newTestVLLMDownloader(fc *fakeCluster) *VLLMPodDownloader {
	d := NewVLLMPodDownloader(nil, "kaito_model_download_speed_bytes_per_second", 5000)
	d.listRawFunc = func(_ context.Context, _, selector string) (*corev1.PodList, error) {
		fc.gotSelector = selector
		return &corev1.PodList{Items: fc.pods}, nil
	}
	d.pods.ScrapeRawFunc = func(_ context.Context, _, pod string, port int) (string, error) {
		fc.gotPort = port
		if err := fc.scrapeErr[pod]; err != nil {
			return "", err
		}
		return fc.metrics[pod], nil
	}
	return d
}

func modelPod(name string, ready corev1.ConditionStatus) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}},
		},
	}
}

func imagePullPod(name string) corev1.Pod {
	p := modelPod(name, corev1.ConditionFalse)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "vllm",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}
	return p
}

const (
	slowMetric = `kaito_model_download_speed_bytes_per_second 6.0e6`
	fastMetric = `kaito_model_download_speed_bytes_per_second 80.0e6`
	vllmMetric = `vllm:num_requests_running 0`
)

func TestVLLMPodDownloaderObserve(t *testing.T) {
	cases := []struct {
		name       string
		fc         fakeCluster
		wantPhase  Phase
		wantBps    float64
		wantSource string
		wantErr    bool
	}{
		{name: "no pods yet", wantPhase: PhaseNotStarted},
		{
			name:      "downloading pod",
			fc:        fakeCluster{pods: []corev1.Pod{modelPod("qwen-0", corev1.ConditionFalse)}, metrics: map[string]string{"qwen-0": slowMetric}},
			wantPhase: PhaseDownloading, wantBps: 6e6, wantSource: "model-pod qwen-0",
		},
		{
			name: "the slowest downloading pod is reported",
			fc: fakeCluster{
				pods:    []corev1.Pod{modelPod("qwen-0", corev1.ConditionFalse), modelPod("qwen-1", corev1.ConditionFalse)},
				metrics: map[string]string{"qwen-0": fastMetric, "qwen-1": slowMetric},
			},
			wantPhase: PhaseDownloading, wantBps: 6e6, wantSource: "model-pod qwen-1",
		},
		{
			name: "one loaded pod does not hide another still downloading",
			fc: fakeCluster{
				pods:    []corev1.Pod{modelPod("qwen-0", corev1.ConditionFalse), modelPod("qwen-1", corev1.ConditionFalse)},
				metrics: map[string]string{"qwen-0": vllmMetric, "qwen-1": slowMetric},
			},
			wantPhase: PhaseDownloading, wantBps: 6e6, wantSource: "model-pod qwen-1",
		},
		{
			name: "every pod ready or loaded is completed",
			fc: fakeCluster{
				pods:    []corev1.Pod{modelPod("qwen-0", corev1.ConditionTrue), modelPod("qwen-1", corev1.ConditionFalse)},
				metrics: map[string]string{"qwen-1": vllmMetric},
			},
			wantPhase: PhaseCompleted,
		},
		{
			name:      "pod serving no download gauge yet",
			fc:        fakeCluster{pods: []corev1.Pod{modelPod("qwen-0", corev1.ConditionFalse)}, metrics: map[string]string{"qwen-0": `other_metric 1`}},
			wantPhase: PhaseNotStarted,
		},
		{
			name:      "terminal container failure is not a download",
			fc:        fakeCluster{pods: []corev1.Pod{imagePullPod("qwen-0")}, metrics: map[string]string{"qwen-0": slowMetric}},
			wantPhase: PhaseNotStarted,
		},
		{
			name:      "scrape error with nothing downloading is reported",
			fc:        fakeCluster{pods: []corev1.Pod{modelPod("qwen-0", corev1.ConditionFalse)}, scrapeErr: map[string]error{"qwen-0": errors.New("proxy unavailable")}},
			wantPhase: PhaseNotStarted, wantErr: true,
		},
		{
			name: "scrape error is ignored when another pod downloads",
			fc: fakeCluster{
				pods:      []corev1.Pod{modelPod("qwen-0", corev1.ConditionFalse), modelPod("qwen-1", corev1.ConditionFalse)},
				metrics:   map[string]string{"qwen-1": slowMetric},
				scrapeErr: map[string]error{"qwen-0": errors.New("proxy unavailable")},
			},
			wantPhase: PhaseDownloading, wantBps: 6e6, wantSource: "model-pod qwen-1",
		},
	}
	for _, tc := range cases {
		fc := tc.fc
		got, err := newTestVLLMDownloader(&fc).Observe(context.Background(), newInferenceSet("qwen", "Qwen/Qwen3-8B", "", nil))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v, wantErr %v", tc.name, err, tc.wantErr)
		}
		if got.Phase != tc.wantPhase || got.BytesPerSecond != tc.wantBps || got.Source != tc.wantSource {
			t.Errorf("%s: got %+v, want %s / %v / %q", tc.name, got, tc.wantPhase, tc.wantBps, tc.wantSource)
		}
		if fc.gotSelector != "inferenceset.kaito.sh/created-by=qwen" {
			t.Errorf("%s: selector=%q", tc.name, fc.gotSelector)
		}
		if len(fc.pods) > 0 && fc.gotPort != 0 && fc.gotPort != 5000 {
			t.Errorf("%s: port=%d, want 5000", tc.name, fc.gotPort)
		}
	}
}
