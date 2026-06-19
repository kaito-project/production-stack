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

	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/util/window"
)

// ThroughputMonitor ties model-weights download throughput scrapers to per-pod
// sliding windows. It scrapes the current throughput for a target, feeds the
// sample into the matching window, and returns the verdict — so callers deal
// only with a ScrapeTarget in and a window.Verdict out, never with scrapers or
// windows directly.
type ThroughputMonitor struct {
	scrapers   []MetricScraper
	windows    *window.Set
	metricName string
}

// NewThroughputMonitor builds a monitor that scrapes the model-weights download
// throughput from the InferenceSet's vLLM workload pod (metricName/metricPort
// configure the gauge and port) and evaluates it against a per-pod window
// configured by cfg.
func NewThroughputMonitor(clientset kubernetes.Interface, cfg window.Config, metricName string, metricPort int) *ThroughputMonitor {
	return &ThroughputMonitor{
		scrapers:   []MetricScraper{NewVLLMScraper(clientset, metricPort)},
		windows:    window.NewSet(cfg),
		metricName: metricName,
	}
}

// Evaluate scrapes the current throughput for target, updates target's window,
// and returns the verdict. When no sample is available — the source pod is
// absent or the download has not started or has already finished — target's
// window is forgotten and a not-ready verdict is returned. A scrape error is
// returned for the caller to log; the window is forgotten in that case too.
func (m *ThroughputMonitor) Evaluate(ctx context.Context, target ScrapeTarget) (window.Verdict, error) {
	sample, found, err := m.scrape(ctx, target)
	if !found {
		m.windows.Forget(target.key())
		return window.Verdict{}, err
	}
	return m.windows.Add(target.key(), sample), nil
}

// Forget drops target's window (e.g. once the pod is Ready or no pod exists).
func (m *ThroughputMonitor) Forget(target ScrapeTarget) {
	m.windows.Forget(target.key())
}

// scrape returns the first available throughput sample across the scrapers. It
// returns found=false when no scraper has a sample; any scrape error is wrapped
// with the scraper name and returned only when no sample was found.
func (m *ThroughputMonitor) scrape(ctx context.Context, target ScrapeTarget) (ThroughputSample, bool, error) {
	var firstErr error
	for _, sc := range m.scrapers {
		sample, found, err := sc.Scrape(ctx, target, m.metricName)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s scraper: %w", sc.Name(), err)
			}
			continue
		}
		if found {
			return sample, true, nil
		}
	}
	return ThroughputSample{}, false, firstErr
}
