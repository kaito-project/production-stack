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

// Package scraper scrapes the model-weights download throughput metric used
// by the inferencesetWeightDownloadSlow reason (§1.2). The throughput sample
// is read from the InferenceSet's vLLM workload pod, so the package defines a
// MetricScraper interface (so additional sources can be added later without
// changing callers). Each scraper locates the correct source pod for an
// InferenceSet itself, scrapes the metric, and reports its own
// download-finished condition.
package scraper

import (
	"context"

	"github.com/kaito-project/production-stack/pkg/util/window"
)

// ScrapeTarget identifies whose model-weights download throughput to scrape.
type ScrapeTarget struct {
	// Namespace is the workload namespace of the InferenceSet.
	Namespace string
	// InferenceSet is the owning InferenceSet name.
	InferenceSet string
}

// key is the per-InferenceSet sliding-window key for the target. The scraper
// selects the source pod itself, so the window is keyed by InferenceSet rather
// than a specific pod, keeping it stable across the download.
func (t ScrapeTarget) key() string {
	return t.Namespace + "/" + t.InferenceSet
}

// ThroughputSample is one observed model-weights download throughput reading.
// It is an alias for window.Sample so a sample read by a MetricScraper feeds
// directly into a sliding window without conversion.
type ThroughputSample = window.Sample

// MetricScraper discovers the source pod(s) that back a target InferenceSet
// (the vLLM pod) and reports the current model-weights download throughput.
// Each scraper encapsulates its own metric selection and its own
// download-finished condition, so callers never deal with raw metrics.
//
// Defining the source-pod discovery + scrape behind this interface lets the
// reporter swap scrape targets (or compose several) without callers caring
// where a sample came from.
type MetricScraper interface {
	// Name identifies the scraper (e.g. "vllm") for logging.
	Name() string

	// Scrape returns the current download throughput sample for target, reading
	// the throughput gauge named metricName. found=false (with a nil error) means
	// no sample is available — the source pod has not started, the download has
	// not begun, or it has already finished — and the caller MUST treat the result
	// as "no sample" (resolving any window) rather than as zero throughput.
	Scrape(ctx context.Context, target ScrapeTarget, metricName string) (sample ThroughputSample, found bool, err error)
}
