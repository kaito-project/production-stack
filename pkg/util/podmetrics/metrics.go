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

package podmetrics

import (
	"fmt"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Sample is a single parsed Prometheus sample: its scalar value, the labels it
// carries, and the source pod it was scraped from.
type Sample struct {
	// Value is the scalar value of the sample (e.g. bytes/second).
	Value float64
	// Labels are the sample's Prometheus labels (e.g. {"pod": "qwen-0"}).
	Labels map[string]string
	// SourcePod is the pod the sample was scraped from.
	SourcePod string
}

// Snapshot is a point-in-time collection of every metric exposed by the source
// pod(s) a Client discovered for a selector.
type Snapshot struct {
	// ScrapedAt is when the snapshot was taken.
	ScrapedAt time.Time
	// Metrics maps a metric family name to all of its samples, merged across
	// every source pod that was scraped.
	Metrics map[string][]Sample
}

// Value returns the first sample of metric in the snapshot. ok is false when
// the metric is not present.
func (s Snapshot) Value(metric string) (Sample, bool) {
	if samples := s.Metrics[metric]; len(samples) > 0 {
		return samples[0], true
	}
	return Sample{}, false
}

// HasMetricWithPrefix reports whether the snapshot contains any metric whose
// family name starts with prefix.
func (s Snapshot) HasMetricWithPrefix(prefix string) bool {
	for name := range s.Metrics {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// parseMetricFamilies parses Prometheus text-format metrics into a map of
// metric family name -> samples. Each sample carries its scalar value and
// labels. Gauge, counter and untyped metrics are supported; complex types
// (histograms, summaries) are skipped.
func parseMetricFamilies(metricsText string) (map[string][]Sample, error) {
	// The Prometheus text parser requires every sample line — including the
	// last — to be newline-terminated; append one when the source omitted it.
	if metricsText != "" && !strings.HasSuffix(metricsText, "\n") {
		metricsText += "\n"
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(metricsText))
	if err != nil {
		return nil, fmt.Errorf("parse prometheus text metrics: %w", err)
	}

	out := make(map[string][]Sample, len(families))
	for name, mf := range families {
		for _, m := range mf.GetMetric() {
			v, ok := extractScalarValue(m)
			if !ok {
				continue
			}
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			out[name] = append(out[name], Sample{Value: v, Labels: labels})
		}
	}
	return out, nil
}

// extractScalarValue extracts a single scalar value from a *dto.Metric for the
// metric types workload pods commonly expose (gauges, counters, untyped).
// Complex types such as histograms and summaries are skipped.
func extractScalarValue(m *dto.Metric) (float64, bool) {
	switch {
	case m.Gauge != nil:
		return m.GetGauge().GetValue(), true
	case m.Counter != nil:
		return m.GetCounter().GetValue(), true
	case m.Untyped != nil:
		return m.GetUntyped().GetValue(), true
	default:
		return 0, false
	}
}
