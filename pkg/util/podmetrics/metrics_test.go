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

import "testing"

const sampleMetrics = `# HELP kaito_model_download_speed_bytes_per_second throughput
# TYPE kaito_model_download_speed_bytes_per_second gauge
kaito_model_download_speed_bytes_per_second{pod="qwen-0",ns="team-a"} 1.2e7
kaito_model_download_speed_bytes_per_second{pod="qwen-1",ns="team-a"} 3.4e7
# TYPE other_metric counter
other_metric 42
`

// sampleByLabel returns the first parsed sample of metric whose labelKey equals
// labelVal (or the first sample when labelKey is empty).
func sampleByLabel(t *testing.T, families map[string][]Sample, metric, labelKey, labelVal string) (Sample, bool) {
	t.Helper()
	for _, sm := range families[metric] {
		if labelKey == "" || sm.Labels[labelKey] == labelVal {
			return sm, true
		}
	}
	return Sample{}, false
}

func TestParseMetricFamiliesByLabel(t *testing.T) {
	families, err := parseMetricFamilies(sampleMetrics)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sm, ok := sampleByLabel(t, families, "kaito_model_download_speed_bytes_per_second", "pod", "qwen-1")
	if !ok {
		t.Fatalf("expected to find sample for pod qwen-1")
	}
	if sm.Value != 3.4e7 {
		t.Fatalf("value=%v, want 3.4e7", sm.Value)
	}
	if sm.Labels["ns"] != "team-a" {
		t.Fatalf("ns label=%q, want team-a", sm.Labels["ns"])
	}
}

func TestParseMetricFamiliesUnlabelled(t *testing.T) {
	families, err := parseMetricFamilies(sampleMetrics)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sm, ok := sampleByLabel(t, families, "other_metric", "", "")
	if !ok || sm.Value != 42 {
		t.Fatalf("expected other_metric=42, got %v ok=%v", sm.Value, ok)
	}
}

func TestParseMetricFamiliesMissing(t *testing.T) {
	families, err := parseMetricFamilies(sampleMetrics)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := sampleByLabel(t, families, "kaito_model_download_speed_bytes_per_second", "pod", "absent"); ok {
		t.Fatalf("expected no match for absent pod")
	}
	if _, ok := families["nope"]; ok {
		t.Fatalf("expected no entry for unknown metric")
	}
}

func TestSnapshotHasMetricWithPrefix(t *testing.T) {
	downloading := Snapshot{Metrics: map[string][]Sample{
		"kaito_model_download_speed_bytes_per_second": {{Value: 1}},
	}}
	if downloading.HasMetricWithPrefix("vllm:") {
		t.Fatalf("did not expect a vllm: metric while still downloading")
	}

	completed := Snapshot{Metrics: map[string][]Sample{
		"vllm:num_requests_running": {{Value: 0}},
	}}
	if !completed.HasMetricWithPrefix("vllm:") {
		t.Fatalf("expected a vllm: metric once the engine has loaded")
	}
}
