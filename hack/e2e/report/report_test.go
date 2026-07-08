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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleFiles() []TestFile {
	return []TestFile{
		{
			Name: "alpha_test.go",
			Blocks: []Block{
				{Type: "Describe", Title: "Alpha Suite", Labels: []string{"Smoke", "Infra"}, EffectiveLabels: []string{"Smoke", "Infra"}},
				{Type: "Context", Title: "when running", Labels: nil, EffectiveLabels: []string{"Smoke", "Infra"}},
				{Type: "It", Title: "test 1", Labels: []string{"Smoke"}, EffectiveLabels: []string{"Smoke", "Infra"}},
				{Type: "It", Title: "test 2", Labels: nil, EffectiveLabels: []string{"Smoke", "Infra"}},
			},
			TestCount: 2,
		},
		{
			Name: "beta_test.go",
			Blocks: []Block{
				{Type: "Describe", Title: "Beta Suite", Labels: []string{"Auth"}, EffectiveLabels: []string{"Auth"}},
				{Type: "It", Title: "test A", Labels: []string{"Auth"}, EffectiveLabels: []string{"Auth"}},
			},
			TestCount: 1,
		},
	}
}

func TestBuildReportData_Totals(t *testing.T) {
	data := buildReportData(sampleFiles(), "CI", "!Nightly")

	if data.Workflow != "CI" {
		t.Errorf("expected Workflow=CI, got %q", data.Workflow)
	}
	if data.LabelFilter != "!Nightly" {
		t.Errorf("expected LabelFilter=!Nightly, got %q", data.LabelFilter)
	}
	if data.TotalFiles != 2 {
		t.Errorf("expected 2 files, got %d", data.TotalFiles)
	}
	if data.TotalIts != 3 {
		t.Errorf("expected 3 Its, got %d", data.TotalIts)
	}
}

func TestBuildReportData_BarChart(t *testing.T) {
	data := buildReportData(sampleFiles(), "", "")

	// Bars are grouped by label (ordered per orderedLabels): Smoke=2, Infra=2,
	// Auth=1. Both alpha Its carry Smoke and Infra; the single beta It carries
	// Auth.
	if len(data.BarChart) != 3 {
		t.Fatalf("expected 3 bar entries, got %d", len(data.BarChart))
	}

	// Smoke has 2 tests (max), so its percent should be 100.
	if data.BarChart[0].Label != "Smoke" {
		t.Errorf("expected bar[0].Label=Smoke, got %q", data.BarChart[0].Label)
	}
	if data.BarChart[0].Value != 2 {
		t.Errorf("expected bar[0].Value=2, got %d", data.BarChart[0].Value)
	}
	if data.BarChart[0].Percent != 100 {
		t.Errorf("expected bar[0].Percent=100, got %d", data.BarChart[0].Percent)
	}

	// Infra also has 2 tests → 100%.
	if data.BarChart[1].Label != "Infra" {
		t.Errorf("expected bar[1].Label=Infra, got %q", data.BarChart[1].Label)
	}
	if data.BarChart[1].Value != 2 {
		t.Errorf("expected bar[1].Value=2, got %d", data.BarChart[1].Value)
	}

	// Auth has 1 test → 50%.
	if data.BarChart[2].Label != "Auth" {
		t.Errorf("expected bar[2].Label=Auth, got %q", data.BarChart[2].Label)
	}
	if data.BarChart[2].Percent != 50 {
		t.Errorf("expected bar[2].Percent=50, got %d", data.BarChart[2].Percent)
	}
}

func TestBuildReportData_DonutGradient(t *testing.T) {
	data := buildReportData(sampleFiles(), "", "")

	if data.ConicGradient == "" {
		t.Fatal("expected non-empty ConicGradient")
	}
	if !strings.Contains(data.ConicGradient, "deg") {
		t.Errorf("expected degree values in gradient, got %q", data.ConicGradient)
	}
	// One legend entry per label with matching tests: Smoke, Infra, Auth.
	if len(data.DonutLegend) != 3 {
		t.Errorf("expected 3 legend entries, got %d", len(data.DonutLegend))
	}
}

func TestBuildReportData_DonutGradient_NoFiles(t *testing.T) {
	data := buildReportData(nil, "", "")

	if data.ConicGradient != "var(--bd) 0deg 360deg" {
		t.Errorf("expected fallback gradient, got %q", data.ConicGradient)
	}
}

func TestBuildReportData_LabelCards(t *testing.T) {
	data := buildReportData(sampleFiles(), "", "")

	cardMap := make(map[string]int)
	for _, c := range data.LabelCards {
		cardMap[c.Name] = c.Count
	}

	// Smoke appears on Describe + It = 2 blocks.
	if cardMap["Smoke"] != 2 {
		t.Errorf("expected Smoke count=2, got %d", cardMap["Smoke"])
	}
	// Auth appears on Describe + It = 2 blocks.
	if cardMap["Auth"] != 2 {
		t.Errorf("expected Auth count=2, got %d", cardMap["Auth"])
	}
	// Infra appears only on 1 Describe.
	if cardMap["Infra"] != 1 {
		t.Errorf("expected Infra count=1, got %d", cardMap["Infra"])
	}
	// Routing should be 0.
	if cardMap["Routing"] != 0 {
		t.Errorf("expected Routing count=0, got %d", cardMap["Routing"])
	}
}

func TestBuildReportData_Timestamp(t *testing.T) {
	data := buildReportData(nil, "", "")

	if !strings.HasSuffix(data.Timestamp, "UTC") {
		t.Errorf("expected timestamp ending in UTC, got %q", data.Timestamp)
	}
}

func TestWriteMarkdown(t *testing.T) {
	data := buildReportData(sampleFiles(), "E2E tests", "!Nightly")
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")

	if err := writeMarkdown(data, path); err != nil {
		t.Fatalf("writeMarkdown: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md := string(content)

	checks := []string{
		"# ✅ E2E Test Coverage Report",
		"**Workflow:** E2E tests",
		"**Label filter:** `!Nightly`",
		"📄 Test Files | **2**",
		"🧪 Test Cases | **3**",
		"alpha_test.go",
		"beta_test.go",
		"🟢 test 1",
		"🟢 test A",
		// Mermaid pie chart.
		"```mermaid",
		"pie title Tests by Label",
		`"Smoke" : 2`,
		`"Auth" : 1`,
		// Mermaid bar chart.
		"xychart-beta horizontal",
		// Label coverage table.
		"Coverage by Label",
		"| `Smoke` | **2**",
		"| `Auth` | **2**",
	}

	for _, want := range checks {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestWriteMarkdown_NoWorkflow(t *testing.T) {
	data := buildReportData(sampleFiles(), "", "(all)")
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")

	if err := writeMarkdown(data, path); err != nil {
		t.Fatalf("writeMarkdown: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(content), "**Workflow:**") {
		t.Error("expected no Workflow line when workflow is empty")
	}
}

func TestWriteHTML(t *testing.T) {
	data := buildReportData(sampleFiles(), "Nightly", "(all)")
	dir := t.TempDir()
	path := filepath.Join(dir, "report.html")

	if err := writeHTML(data, path); err != nil {
		t.Fatalf("writeHTML: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)

	checks := []string{
		"<!DOCTYPE html>",
		"Nightly",
		"Alpha Suite",
		"Beta Suite",
		"conic-gradient",
	}
	for _, want := range checks {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
}

func TestWriteHTML_EmptyFiles(t *testing.T) {
	data := buildReportData(nil, "", "(all)")
	dir := t.TempDir()
	path := filepath.Join(dir, "report.html")

	if err := writeHTML(data, path); err != nil {
		t.Fatalf("writeHTML should not error with empty files: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Results annotation
// ---------------------------------------------------------------------------

func TestApplyResults_AnnotatesAndCounts(t *testing.T) {
	data := buildReportData(sampleFiles(), "", "(all)")
	// sampleFiles has Its: "test 1", "test 2" (alpha), "test A" (beta).
	applyResults(data, map[string]string{
		"test 1": StatusPassed,
		"test 2": StatusFailed,
		// "test A" has no result → should be counted as skipped.
	})

	if !data.HasResults {
		t.Fatal("expected HasResults=true")
	}
	if data.TotalPassed != 1 {
		t.Errorf("expected 1 passed, got %d", data.TotalPassed)
	}
	if data.TotalFailed != 1 {
		t.Errorf("expected 1 failed, got %d", data.TotalFailed)
	}
	if data.TotalSkipped != 1 {
		t.Errorf("expected 1 skipped, got %d", data.TotalSkipped)
	}

	// Verify per-spec statuses were assigned on the block copies.
	statuses := map[string]string{}
	for _, f := range data.Files {
		for _, b := range f.Blocks {
			if b.Type == "It" {
				statuses[b.Title] = b.Status
			}
		}
	}
	if statuses["test 1"] != StatusPassed {
		t.Errorf("expected test 1 passed, got %q", statuses["test 1"])
	}
	if statuses["test 2"] != StatusFailed {
		t.Errorf("expected test 2 failed, got %q", statuses["test 2"])
	}
	if statuses["test A"] != StatusSkipped {
		t.Errorf("expected test A skipped, got %q", statuses["test A"])
	}
}

func TestApplyResults_EmptyIsNoOp(t *testing.T) {
	data := buildReportData(sampleFiles(), "", "(all)")
	applyResults(data, nil)

	if data.HasResults {
		t.Error("expected HasResults=false when no results supplied")
	}
	if data.TotalPassed+data.TotalFailed+data.TotalSkipped != 0 {
		t.Error("expected zero status tallies when no results supplied")
	}
}

func TestWriteMarkdown_WithResults(t *testing.T) {
	data := buildReportData(sampleFiles(), "E2E tests", "(all)")
	applyResults(data, map[string]string{
		"test 1": StatusPassed,
		"test 2": StatusFailed,
		"test A": StatusPassed,
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	if err := writeMarkdown(data, path); err != nil {
		t.Fatalf("writeMarkdown: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md := string(content)

	checks := []string{
		"🟢 Passed | **2**",
		"🔴 Failed | **1**",
		"🔴 test 2", // failing spec rendered with red icon
		"🟢 test 1",
	}
	for _, want := range checks {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestParseResults_MissingFileIsEmpty(t *testing.T) {
	results, err := parseResults(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %v", results)
	}
}

func TestParseResults_EmptyPathIsEmpty(t *testing.T) {
	results, err := parseResults("")
	if err != nil {
		t.Fatalf("expected no error for empty path, got %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %v", results)
	}
}

func TestParseResults_ParsesGinkgoJSON(t *testing.T) {
	// Minimal Ginkgo JSON report. SpecState marshals as a lowercase string
	// (see github.com/onsi/ginkgo/v2/types.SpecState.MarshalJSON). LeafNodeText
	// is the matching key; for a duplicate leaf the worst status wins.
	jsonReport := `[
      {
        "SpecReports": [
          {"LeafNodeText": "spec passes", "State": "passed"},
          {"LeafNodeText": "spec fails", "State": "failed"},
          {"LeafNodeText": "spec skipped", "State": "skipped"},
          {"LeafNodeText": "spec timed out", "State": "timedout"},
          {"LeafNodeText": "dup", "State": "passed"},
          {"LeafNodeText": "dup", "State": "failed"},
          {"LeafNodeText": "", "State": "passed"}
        ]
      }
    ]`
	dir := t.TempDir()
	path := filepath.Join(dir, "ginkgo-report.json")
	if err := os.WriteFile(path, []byte(jsonReport), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := parseResults(path)
	if err != nil {
		t.Fatalf("parseResults: %v", err)
	}
	if results["spec passes"] != StatusPassed {
		t.Errorf("expected spec passes=passed, got %q", results["spec passes"])
	}
	if results["spec fails"] != StatusFailed {
		t.Errorf("expected spec fails=failed, got %q", results["spec fails"])
	}
	if results["spec skipped"] != StatusSkipped {
		t.Errorf("expected spec skipped=skipped, got %q", results["spec skipped"])
	}
	if results["spec timed out"] != StatusFailed {
		t.Errorf("expected spec timed out=failed, got %q", results["spec timed out"])
	}
	// Worst status wins for duplicate leaf text.
	if results["dup"] != StatusFailed {
		t.Errorf("expected dup=failed (worst wins), got %q", results["dup"])
	}
	// Empty leaf text (setup/teardown nodes) must be ignored.
	if _, ok := results[""]; ok {
		t.Error("expected empty leaf text to be ignored")
	}
}

// ---------------------------------------------------------------------------
// Label filter evaluation
// ---------------------------------------------------------------------------

func TestMatchesLabelFilter_AllCases(t *testing.T) {
	tests := []struct {
		filter string
		labels []string
		want   bool
	}{
		// Empty / (all) always matches.
		{"", []string{"Smoke"}, true},
		{"(all)", nil, true},
		// Simple label presence.
		{"Smoke", []string{"Smoke"}, true},
		{"Smoke", []string{"Auth"}, false},
		{"Smoke", nil, false},
		// NOT operator.
		{"!Nightly", nil, true},
		{"!Nightly", []string{"Smoke"}, true},
		{"!Nightly", []string{"Nightly"}, false},
		// AND operator.
		{"Nightly && !NetworkPolicy", []string{"Nightly"}, true},
		{"Nightly && !NetworkPolicy", []string{"Nightly", "NetworkPolicy"}, false},
		{"Nightly && !NetworkPolicy", []string{"Smoke"}, false},
		// OR operator.
		{"Smoke || Auth", []string{"Smoke"}, true},
		{"Smoke || Auth", []string{"Auth"}, true},
		{"Smoke || Auth", []string{"Nightly"}, false},
		// Parentheses.
		{"(Smoke || Auth) && !Nightly", []string{"Smoke"}, true},
		{"(Smoke || Auth) && !Nightly", []string{"Smoke", "Nightly"}, false},
	}
	for _, tc := range tests {
		got := matchesLabelFilter(tc.filter, tc.labels)
		if got != tc.want {
			t.Errorf("matchesLabelFilter(%q, %v) = %v, want %v", tc.filter, tc.labels, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// buildReportData with label filter
// ---------------------------------------------------------------------------

// nightlyFiles returns two synthetic files: one with Nightly tests and one
// with only non-Nightly tests, to verify that the label filter is actually
// applied when computing TotalFiles and TotalIts.
func nightlyFiles() []TestFile {
	return []TestFile{
		{
			Name: "scaling_test.go",
			Blocks: []Block{
				{Type: "Describe", Title: "Scaling", Labels: []string{"Scaling", "Nightly"}, EffectiveLabels: []string{"Scaling", "Nightly"}},
				{Type: "It", Title: "scale up", Labels: nil, EffectiveLabels: []string{"Scaling", "Nightly"}},
				{Type: "It", Title: "scale down", Labels: nil, EffectiveLabels: []string{"Scaling", "Nightly"}},
			},
			TestCount: 2,
		},
		{
			Name: "model_routing_test.go",
			Blocks: []Block{
				{Type: "Describe", Title: "Routing", Labels: []string{"Routing"}, EffectiveLabels: []string{"Routing"}},
				{Type: "It", Title: "routes correctly", Labels: nil, EffectiveLabels: []string{"Routing"}},
			},
			TestCount: 1,
		},
	}
}

func TestBuildReportData_NightlyFilter(t *testing.T) {
	data := buildReportData(nightlyFiles(), "E2E (nightly)", "Nightly && !NetworkPolicy")

	// Only scaling_test.go has Nightly tests.
	if data.TotalFiles != 1 {
		t.Errorf("expected 1 file for Nightly filter, got %d", data.TotalFiles)
	}
	if data.TotalIts != 2 {
		t.Errorf("expected 2 Its for Nightly filter, got %d", data.TotalIts)
	}
	if len(data.Files) != 1 || data.Files[0].Name != "scaling_test.go" {
		t.Errorf("expected data.Files=[scaling_test.go], got %v", data.Files)
	}
}

func TestBuildReportData_NonNightlyFilter(t *testing.T) {
	data := buildReportData(nightlyFiles(), "E2E", "!Nightly")

	// Only model_routing_test.go passes !Nightly.
	if data.TotalFiles != 1 {
		t.Errorf("expected 1 file for !Nightly filter, got %d", data.TotalFiles)
	}
	if data.TotalIts != 1 {
		t.Errorf("expected 1 It for !Nightly filter, got %d", data.TotalIts)
	}
	if len(data.Files) != 1 || data.Files[0].Name != "model_routing_test.go" {
		t.Errorf("expected data.Files=[model_routing_test.go], got %v", data.Files)
	}
}

func TestBuildReportData_AllFilter(t *testing.T) {
	data := buildReportData(nightlyFiles(), "", "(all)")

	if data.TotalFiles != 2 {
		t.Errorf("expected 2 files for (all) filter, got %d", data.TotalFiles)
	}
	if data.TotalIts != 3 {
		t.Errorf("expected 3 Its for (all) filter, got %d", data.TotalIts)
	}
}

// TestBuildReportData_DetailFiltering verifies that data.Files contains only
// the blocks that match the active label filter, and that TestCount reflects
// the filtered count (not the raw file total).
func TestBuildReportData_DetailFiltering(t *testing.T) {
	// scaling_test.go: 2 Its tagged Nightly
	// model_routing_test.go: 1 It tagged Routing (not Nightly)
	data := buildReportData(nightlyFiles(), "W", "!Nightly")

	if len(data.Files) != 1 {
		t.Fatalf("expected 1 file in data.Files, got %d", len(data.Files))
	}
	tf := data.Files[0]
	if tf.Name != "model_routing_test.go" {
		t.Errorf("expected model_routing_test.go, got %q", tf.Name)
	}
	// TestCount should be the filtered count (1), not the raw file count.
	if tf.TestCount != 1 {
		t.Errorf("expected filtered TestCount=1, got %d", tf.TestCount)
	}
	// Only the matching It block should remain.
	var itBlocks []Block
	for _, b := range tf.Blocks {
		if b.Type == "It" {
			itBlocks = append(itBlocks, b)
		}
	}
	if len(itBlocks) != 1 {
		t.Errorf("expected 1 It block in filtered file, got %d", len(itBlocks))
	}

	// Verify the opposite: Nightly filter should have scaling_test.go with 2 Its.
	data2 := buildReportData(nightlyFiles(), "W", "Nightly")
	if len(data2.Files) != 1 || data2.Files[0].Name != "scaling_test.go" {
		t.Fatalf("expected scaling_test.go for Nightly filter, got %v", data2.Files)
	}
	if data2.Files[0].TestCount != 2 {
		t.Errorf("expected filtered TestCount=2 for scaling, got %d", data2.Files[0].TestCount)
	}
	var itBlocks2 []Block
	for _, b := range data2.Files[0].Blocks {
		if b.Type == "It" {
			itBlocks2 = append(itBlocks2, b)
		}
	}
	if len(itBlocks2) != 2 {
		t.Errorf("expected 2 It blocks for Nightly scaling file, got %d", len(itBlocks2))
	}
}
