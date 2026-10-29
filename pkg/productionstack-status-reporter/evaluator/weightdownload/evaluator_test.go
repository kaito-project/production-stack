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

package weightdownload

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/downloader"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

// scripted is a WeightDownloader that returns one canned status per call.
type scripted struct {
	statuses []downloader.DownloadStatus
	calls    int
}

func (s *scripted) Observe(context.Context, *unstructured.Unstructured) (downloader.DownloadStatus, error) {
	st := s.statuses[min(s.calls, len(s.statuses)-1)]
	s.calls++
	return st, nil
}

func downloading(mbps float64) downloader.DownloadStatus {
	return downloader.DownloadStatus{Phase: downloader.PhaseDownloading, BytesPerSecond: mbps * bytesPerMB, Source: "modelmirror team-a/3fa2c1"}
}

var (
	notStarted = downloader.DownloadStatus{Phase: downloader.PhaseNotStarted}
	completed  = downloader.DownloadStatus{Phase: downloader.PhaseCompleted}
)

func presetInferenceSet() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kaito.sh/v1beta1",
		"kind":       "InferenceSet",
		"metadata":   map[string]interface{}{"name": "qwen", "namespace": "team-a"},
		"spec": map[string]interface{}{"template": map[string]interface{}{
			"inference": map[string]interface{}{"preset": map[string]interface{}{"name": "Qwen/Qwen3-8B"}},
		}},
	}}
}

// newTestEvaluator returns an Evaluator over one managed namespace holding the
// given InferenceSets, whose ModelMirror path is served by d, and a clock that
// the test advances by one 30s resync per pass.
func newTestEvaluator(d downloader.WeightDownloader, sets ...runtime.Object) (*Evaluator, *time.Time) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{util.ManagedByLabel: util.ManagedByValue}}}
	cs := fake.NewSimpleClientset(ns)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{util.InferenceSetGVR: "InferenceSetList"}, sets...)
	cfg := config.DefaultConfig()
	e := New(cs, dyn, cfg)
	e.downloaders[downloader.PathModelMirror] = d
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	e.clock = func() time.Time { return now }
	return e, &now
}

// run evaluates passes resyncs 30s apart and returns the findings of each.
func run(t *testing.T, e *Evaluator, now *time.Time, passes int) [][]string {
	t.Helper()
	return runEvery(t, e, now, passes, 30*time.Second)
}

// runEvery evaluates passes resyncs step apart and returns the findings of
// each.
func runEvery(t *testing.T, e *Evaluator, now *time.Time, passes int, step time.Duration) [][]string {
	t.Helper()
	out := make([][]string, passes)
	for i := 0; i < passes; i++ {
		findings, err := e.Evaluate(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		for _, f := range findings {
			if f.Reason != reason.InferencesetModelDownloadSlow {
				t.Fatalf("pass %d: unexpected reason %q", i, f.Reason)
			}
			out[i] = append(out[i], f.Message)
		}
		*now = now.Add(step)
	}
	return out
}

func TestSustainedSlowRaisesOnceTheWindowIsFull(t *testing.T) {
	d := &scripted{statuses: []downloader.DownloadStatus{downloading(6), downloading(5), downloading(7), downloading(6)}}
	e, now := newTestEvaluator(d, presetInferenceSet())
	got := run(t, e, now, 4)
	// Samples at 0s and 30s span less than the 60s window: no decision yet.
	if len(got[0]) != 0 || len(got[1]) != 0 {
		t.Fatalf("partial window must not raise, got %v", got[:2])
	}
	if len(got[2]) != 1 || len(got[3]) != 1 {
		t.Fatalf("full slow window must raise, got %v", got[2:])
	}
	msg := got[2][0]
	for _, want := range []string{"InferenceSet team-a/qwen", "below 20 MB/s for 1m0s", "worst 5.00 MB/s", "source=modelmirror team-a/3fa2c1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}

// With the default 60s resync against the default 60s window, a sustained slow
// download raises on the second sample.
func TestSustainedSlowRaisesAtTheDefaultResync(t *testing.T) {
	d := &scripted{statuses: []downloader.DownloadStatus{downloading(6)}}
	e, now := newTestEvaluator(d, presetInferenceSet())
	got := runEvery(t, e, now, 3, 60*time.Second+100*time.Millisecond)
	if len(got[0]) != 0 || len(got[1]) != 1 || len(got[2]) != 1 {
		t.Fatalf("want no finding on the first sample and one per pass after, got %v", got)
	}
}

// Passes of a loaded reporter jitter around the default 60s resync. Once the
// window is full, a sustained-slow download must raise on every pass: a pass
// without the finding would reset the startup gate's first-observation timer
// and withhold the Warning again.
func TestSustainedSlowRaisesOnEveryJitteredPass(t *testing.T) {
	d := &scripted{statuses: []downloader.DownloadStatus{downloading(6)}}
	e, now := newTestEvaluator(d, presetInferenceSet())
	steps := []time.Duration{
		60500 * time.Millisecond, 59800 * time.Millisecond, 60300 * time.Millisecond,
		59600 * time.Millisecond, 60900 * time.Millisecond, 0,
	}
	for i, step := range steps {
		got := runEvery(t, e, now, 1, step)
		if i >= 2 && len(got[0]) != 1 {
			t.Fatalf("pass %d: a sustained-slow download must raise on every pass once the window is full, got %v", i+1, got[0])
		}
	}
}

func TestTransientDipDoesNotRaise(t *testing.T) {
	d := &scripted{statuses: []downloader.DownloadStatus{downloading(6), downloading(50), downloading(6), downloading(50), downloading(6)}}
	e, now := newTestEvaluator(d, presetInferenceSet())
	for i, f := range run(t, e, now, 5) {
		if len(f) != 0 {
			t.Fatalf("pass %d: a sample above the threshold inside the window must suppress the warning, got %v", i, f)
		}
	}
}

func TestNotStartedKeepsAndCompletedClearsTheWindow(t *testing.T) {
	d := &scripted{statuses: []downloader.DownloadStatus{
		downloading(6), notStarted, downloading(6), completed, downloading(6), downloading(6),
	}}
	e, now := newTestEvaluator(d, presetInferenceSet())
	got := run(t, e, now, 6)
	// Pass 2: samples at 0s and 60s (the NotStarted pass added none) fill the
	// window.
	if len(got[2]) != 1 {
		t.Fatalf("NotStarted must keep the window, got %v", got)
	}
	// Completed clears the window, so the next download starts from scratch.
	if len(got[3]) != 0 || len(got[4]) != 0 || len(got[5]) != 0 {
		t.Fatalf("Completed must clear the window, got %v", got[3:])
	}
}

func TestPodTemplateInferenceSetIsSkipped(t *testing.T) {
	is := presetInferenceSet()
	unstructured.RemoveNestedField(is.Object, "spec", "template", "inference", "preset")
	d := &scripted{statuses: []downloader.DownloadStatus{downloading(1)}}
	e, now := newTestEvaluator(d, is)
	run(t, e, now, 4)
	if d.calls != 0 {
		t.Fatalf("a pod-template InferenceSet downloads nothing, but the downloader was called %d times", d.calls)
	}
}

func TestDeletedInferenceSetWindowIsPruned(t *testing.T) {
	d := &scripted{statuses: []downloader.DownloadStatus{downloading(6)}}
	e, now := newTestEvaluator(d, presetInferenceSet())
	run(t, e, now, 1)
	if len(e.windows.KeysWithPrefix("team-a/")) != 1 {
		t.Fatalf("expected one window after the first pass")
	}
	if err := e.dynamic.Resource(util.InferenceSetGVR).Namespace("team-a").Delete(context.Background(), "qwen", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	run(t, e, now, 1)
	if n := len(e.windows.KeysWithPrefix("team-a/")); n != 0 {
		t.Fatalf("window of a deleted InferenceSet must be pruned, %d left", n)
	}
}
