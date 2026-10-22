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

// Package weightdownload implements the orthogonal
// inferencesetModelDownloadSlow Evaluator (§1.2, §1.4): it observes each
// InferenceSet's model download through the WeightDownloader of the download
// path KAITO uses, and evaluates the throughput against a sliding window.
package weightdownload

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/downloader"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
	"github.com/kaito-project/production-stack/pkg/util/window"
)

// bytesPerMB converts a bytes/second throughput into MB/s.
const bytesPerMB = 1_000_000.0

// Evaluator implements the sliding-window inferencesetModelDownloadSlow check.
// It keeps one window per InferenceSet and raises the warning only when every
// sample across a fully-populated window is below the threshold.
type Evaluator struct {
	clientset   kubernetes.Interface
	dynamic     dynamic.Interface
	cfg         config.Config
	windows     *window.Set
	downloaders map[downloader.Path]downloader.WeightDownloader
	clock       func() time.Time
}

// New constructs a weightdownload Evaluator with the vLLM model-pod and
// ModelMirror downloaders.
func New(cs kubernetes.Interface, dyn dynamic.Interface, cfg config.Config) *Evaluator {
	return &Evaluator{
		clientset: cs,
		dynamic:   dyn,
		cfg:       cfg,
		windows:   window.NewSet(cfg.WeightDownload),
		downloaders: map[downloader.Path]downloader.WeightDownloader{
			downloader.PathVLLM:        downloader.NewVLLMPodDownloader(cs, cfg.MetricName, cfg.MetricPort),
			downloader.PathModelMirror: downloader.NewModelMirrorDownloader(dyn),
		},
		clock: time.Now,
	}
}

// Name identifies the evaluator for logging.
func (e *Evaluator) Name() string { return "weightdownload" }

// Evaluate enumerates the InferenceSets across all managed namespaces and
// returns one Finding per InferenceSet whose model download is sustained-slow.
// It returns an error only when namespace discovery fails.
func (e *Evaluator) Evaluate(ctx context.Context) ([]evaluator.Finding, error) {
	namespaces, err := util.DiscoverNamespaces(ctx, e.clientset)
	if err != nil {
		return nil, err
	}
	var findings []evaluator.Finding
	seen := map[string]bool{}
	for _, ns := range namespaces {
		sets, err := e.dynamic.Resource(util.InferenceSetGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			// Unknown this pass: keep the namespace's windows.
			for k := range e.windowKeysIn(ns) {
				seen[k] = true
			}
			continue
		}
		for i := range sets.Items {
			is := &sets.Items[i]
			seen[windowKey(is)] = true
			if f, ok := e.evaluateInferenceSet(ctx, is); ok {
				findings = append(findings, f)
			}
		}
	}
	e.windows.Prune(seen)
	return findings, nil
}

// evaluateInferenceSet observes one InferenceSet through the downloader of its
// download path and feeds the result into its window (§1.4).
func (e *Evaluator) evaluateInferenceSet(ctx context.Context, is *unstructured.Unstructured) (evaluator.Finding, bool) {
	key := windowKey(is)
	d, ok := e.downloaders[downloader.Select(is, e.cfg.ModelStreamingEnabled)]
	if !ok {
		e.windows.Forget(key) // nothing is downloaded on this path
		return evaluator.Finding{}, false
	}
	status, err := d.Observe(ctx, is)
	if err != nil {
		log.FromContext(ctx).Error(err, "observe model download", "inferenceset", key)
		return evaluator.Finding{}, false // unknown this pass
	}
	switch status.Phase {
	case downloader.PhaseCompleted:
		e.windows.Forget(key)
		return evaluator.Finding{}, false
	case downloader.PhaseDownloading:
	default:
		return evaluator.Finding{}, false
	}

	verdict := e.windows.Add(key, window.Sample{
		Timestamp: e.clock(),
		MBps:      status.BytesPerSecond / bytesPerMB,
		Source:    status.Source,
	})
	if !verdict.Ready || !verdict.Slow {
		return evaluator.Finding{}, false
	}
	namespace, name := is.GetNamespace(), is.GetName()
	return evaluator.Finding{
		Reason:            reason.InferencesetModelDownloadSlow,
		Object:            evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: namespace},
		WorkloadNamespace: namespace,
		GroupKey:          fmt.Sprintf("inferenceset/%s/%s/modeldownload", namespace, name),
		Message: fmt.Sprintf(
			"InferenceSet %s/%s: model download below %g MB/s for %s (worst %.2f MB/s); source=%s.",
			namespace, name, e.cfg.WeightDownload.MinMBps, e.cfg.WeightDownload.WindowDuration,
			verdict.WorstMBps, verdict.Source),
	}, true
}

// windowKey is the per-InferenceSet sliding-window key.
func windowKey(is *unstructured.Unstructured) string {
	return is.GetNamespace() + "/" + is.GetName()
}

// windowKeysIn returns the window keys of the InferenceSets in namespace that
// were seen in earlier passes.
func (e *Evaluator) windowKeysIn(namespace string) map[string]bool {
	return e.windows.KeysWithPrefix(namespace + "/")
}
