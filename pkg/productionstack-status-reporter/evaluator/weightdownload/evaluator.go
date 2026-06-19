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
// inferencesetWeightDownloadSlow Evaluator (§1.2) via a sliding-window
// throughput monitor.
package weightdownload

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/scraper"
)

// Evaluator implements the sliding-window inferencesetWeightDownloadSlow check
// (§1.2). It owns the throughput monitor, which delegates the scrape + per-pod
// window bookkeeping; it raises the warning only when every sample across a
// fully-populated window is below the threshold. The monitor forgets the
// window once the pod is Ready or the download is complete (no sample
// available, or vLLM's native metrics have appeared), so the warning resolves
// and a later pod start re-evaluates from scratch.
type Evaluator struct {
	clientset  kubernetes.Interface
	dynamic    dynamic.Interface
	throughput *scraper.ThroughputMonitor
	cfg        config.Config
}

// New constructs a weightdownload Evaluator, building the throughput monitor it
// owns from the supplied config.
func New(cs kubernetes.Interface, dyn dynamic.Interface, cfg config.Config) *Evaluator {
	return &Evaluator{
		clientset:  cs,
		dynamic:    dyn,
		throughput: scraper.NewThroughputMonitor(cs, cfg.WeightDownload, cfg.MetricName, cfg.MetricPort),
		cfg:        cfg,
	}
}

// Name identifies the evaluator for logging.
func (e *Evaluator) Name() string { return "weightdownload" }

// Evaluate enumerates the InferenceSets across all managed namespaces, scrapes
// the model pod throughput for each, and returns one Finding per InferenceSet
// whose model-weights download is sustained-slow. It returns an error only
// when namespace discovery fails.
func (e *Evaluator) Evaluate(ctx context.Context) ([]evaluator.Finding, error) {
	namespaces, err := util.DiscoverNamespaces(ctx, e.clientset)
	if err != nil {
		return nil, err
	}
	logger := log.FromContext(ctx)
	var findings []evaluator.Finding
	for _, ns := range namespaces {
		sets, err := e.dynamic.Resource(util.InferenceSetGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range sets.Items {
			name := sets.Items[i].GetName()
			if f, ok := e.evaluateInferenceSet(ctx, ns, name, logger); ok {
				findings = append(findings, f)
			}
		}
	}
	return findings, nil
}

func (e *Evaluator) evaluateInferenceSet(ctx context.Context, namespace, name string, logger logr.Logger) (evaluator.Finding, bool) {
	// Pod selection lives in the scraper: it samples only a still-downloading
	// (not-Ready) model pod and reports no sample once the download has finished
	// or no such pod exists, in which case Evaluate forgets the window. So every
	// InferenceSet is simply fed to the monitor here.
	target := scraper.ScrapeTarget{Namespace: namespace, InferenceSet: name}
	verdict, err := e.throughput.Evaluate(ctx, target)
	if err != nil {
		logger.Error(err, "scrape weight-download throughput", "inferenceset", fmt.Sprintf("%s/%s", namespace, name))
	}
	if !verdict.Ready || !verdict.Slow {
		return evaluator.Finding{}, false
	}

	return evaluator.Finding{
		Reason:            reason.InferencesetWeightDownloadSlow,
		Object:            evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: namespace},
		WorkloadNamespace: namespace,
		GroupKey:          fmt.Sprintf("inferenceset/%s/%s/weightdownload", namespace, name),
		Message: fmt.Sprintf(
			"InferenceSet %s/%s: model-weights download is slow — every sample in the %s window was below %.0f MB/s (worst %.2f MB/s); source pod %s.",
			namespace, name, e.cfg.WeightDownload.WindowDuration, e.cfg.WeightDownload.MinMBps,
			verdict.WorstMBps, verdict.SourcePod),
	}, true
}
