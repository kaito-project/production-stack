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
	_ "embed"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"
)

//go:embed template.html
var htmlTemplateStr string

// ---------------------------------------------------------------------------
// Data structures for report rendering
// ---------------------------------------------------------------------------

// ReportData holds all data needed to render both Markdown and HTML reports.
type ReportData struct {
	Workflow    string
	LabelFilter string
	Timestamp   string
	TotalFiles  int
	TotalIts    int

	// HasResults is true when a Ginkgo JSON report was supplied, enabling the
	// pass/fail/skip summary and per-spec status annotations.
	HasResults   bool
	TotalPassed  int
	TotalFailed  int
	TotalSkipped int
	TotalPending int

	Files         []TestFile
	BarChart      []BarEntry
	ConicGradient string
	DonutLegend   []LegendEntry
	LabelCards    []LabelCard
	AllLabels     []string
}

// BarEntry is one row in the horizontal bar chart.
type BarEntry struct {
	Label   string
	Value   int
	Percent int
	Color   string
}

// LegendEntry is one swatch in the donut chart legend.
type LegendEntry struct {
	Label string
	Color string
}

// LabelCard is one card in the "Coverage by Label" section.
type LabelCard struct {
	Name  string
	Count int
	Color string
}

// ---------------------------------------------------------------------------
// Label configuration — edit here when adding new Ginkgo labels
// ---------------------------------------------------------------------------

var orderedLabels = []string{
	"Smoke", "Nightly",
	"Infra", "Routing", "PrefixCache", "Auth", "NetworkPolicy",
	"Scaling", "InferenceSet", "FilterOrder", "Karpenter", "Outage",
}

var labelDescriptions = map[string]string{
	"Smoke":         "Basic sanity checks — every PR",
	"Nightly":       "Long-running tests (nightly only)",
	"Infra":         "Infrastructure lifecycle (nodes, pods, GC)",
	"Routing":       "Gateway / model routing correctness",
	"PrefixCache":   "Prefix / KV-cache aware routing",
	"Auth":          "API key authentication",
	"NetworkPolicy": "Kubernetes NetworkPolicy enforcement",
	"Scaling":       "KEDA-driven scale-up / scale-down / anti-flapping",
	"InferenceSet":  "InferenceSet chart lifecycle",
	"FilterOrder":   "Envoy HTTP filter chain execution order",
	"Karpenter":     "GPU node provisioning from zero",
	"Outage":        "Fail-closed / HA outage resilience",
}

var labelColors = map[string]string{
	"Smoke":         "#3fb950",
	"Nightly":       "#d29922",
	"Infra":         "#bc8cff",
	"Routing":       "#f0883e",
	"PrefixCache":   "#f0883e",
	"Auth":          "#f85149",
	"NetworkPolicy": "#bc8cff",
	"Scaling":       "#39d353",
	"InferenceSet":  "#bc8cff",
	"FilterOrder":   "#58a6ff",
	"Karpenter":     "#56d364",
	"Outage":        "#f85149",
}

var chartColors = []string{
	"#58a6ff", "#3fb950", "#f85149", "#bc8cff",
	"#f0883e", "#d29922", "#39d353", "#8b949e",
	"#79c0ff", "#56d364",
}

// ---------------------------------------------------------------------------
// Label filter evaluation
// ---------------------------------------------------------------------------

// matchesLabelFilter evaluates a Ginkgo-style label filter expression against
// a set of labels. Supported operators: !, &&, ||, and parentheses.
// An empty filter or "(all)" matches every spec.
func matchesLabelFilter(filter string, labels []string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" || filter == "(all)" {
		return true
	}
	set := make(map[string]bool, len(labels))
	for _, l := range labels {
		set[l] = true
	}
	result, _ := evalOrExpr(filter, set)
	return result
}

func evalOrExpr(s string, labels map[string]bool) (bool, string) {
	left, rest := evalAndExpr(s, labels)
	for {
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "||") {
			break
		}
		rest = strings.TrimSpace(rest[2:])
		var right bool
		right, rest = evalAndExpr(rest, labels)
		left = left || right
	}
	return left, rest
}

func evalAndExpr(s string, labels map[string]bool) (bool, string) {
	left, rest := evalUnary(s, labels)
	for {
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "&&") {
			break
		}
		rest = strings.TrimSpace(rest[2:])
		var right bool
		right, rest = evalUnary(rest, labels)
		left = left && right
	}
	return left, rest
}

func evalUnary(s string, labels map[string]bool) (bool, string) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "!") {
		val, rest := evalPrimary(strings.TrimSpace(s[1:]), labels)
		return !val, rest
	}
	return evalPrimary(s, labels)
}

func evalPrimary(s string, labels map[string]bool) (bool, string) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "(") {
		val, rest := evalOrExpr(strings.TrimSpace(s[1:]), labels)
		rest = strings.TrimSpace(rest)
		if strings.HasPrefix(rest, ")") {
			rest = rest[1:]
		}
		return val, rest
	}
	// Parse an identifier (label name).
	end := 0
	for end < len(s) {
		c := rune(s[end])
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			end++
		} else {
			break
		}
	}
	if end == 0 {
		return false, s
	}
	return labels[s[:end]], s[end:]
}

// ---------------------------------------------------------------------------
// Report data construction
// ---------------------------------------------------------------------------

func buildReportData(files []TestFile, workflow, labelFilter string) *ReportData {
	data := &ReportData{
		Workflow:    workflow,
		LabelFilter: labelFilter,
		Timestamp:   time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		AllLabels:   orderedLabels,
	}

	// Compute per-file filtered test counts for the bar / donut charts.
	// Files with zero matching tests are excluded from the visual charts and
	// from TotalFiles / TotalIts.
	type fileCount struct {
		file  TestFile
		count int
	}
	var filteredFiles []fileCount
	for _, f := range files {
		n := 0
		for _, b := range f.Blocks {
			if b.Type == "It" && matchesLabelFilter(labelFilter, b.EffectiveLabels) {
				n++
			}
		}
		if n > 0 {
			filteredFiles = append(filteredFiles, fileCount{f, n})
			data.TotalFiles++
			data.TotalIts += n
		}
	}

	// Populate data.Files with filtered copies of each file: non-matching It
	// blocks are omitted so that the detail sections in Markdown/HTML only
	// show tests that actually run under this label filter.  TestCount on the
	// copy reflects the filtered count so that "N tests" headers are correct.
	for _, fc := range filteredFiles {
		f := fc.file
		filtered := TestFile{
			Name:      f.Name,
			TestCount: fc.count, // filtered count, not f.TestCount
		}
		for _, b := range f.Blocks {
			if b.Type == "It" && !matchesLabelFilter(labelFilter, b.EffectiveLabels) {
				continue // omit non-matching test cases
			}
			filtered.Blocks = append(filtered.Blocks, b)
		}
		data.Files = append(data.Files, filtered)
	}

	// Bar chart / donut data — grouped by label rather than by file. Each
	// running It block contributes to the count of every label it carries, so
	// a spec with multiple labels is counted once per label.
	barLabelCounts := make(map[string]int)
	for _, f := range files {
		for _, b := range f.Blocks {
			if b.Type != "It" || !matchesLabelFilter(labelFilter, b.EffectiveLabels) {
				continue
			}
			seen := make(map[string]bool, len(b.EffectiveLabels))
			for _, lbl := range b.EffectiveLabels {
				if seen[lbl] {
					continue
				}
				seen[lbl] = true
				barLabelCounts[lbl]++
			}
		}
	}

	// Preserve the canonical label ordering, keeping only labels that have at
	// least one matching test under the active filter.
	var chartLabels []string
	for _, lbl := range orderedLabels {
		if barLabelCounts[lbl] > 0 {
			chartLabels = append(chartLabels, lbl)
		}
	}

	maxTests := 0
	labelTotal := 0
	for _, lbl := range chartLabels {
		labelTotal += barLabelCounts[lbl]
		if barLabelCounts[lbl] > maxTests {
			maxTests = barLabelCounts[lbl]
		}
	}

	cumulative := 0
	var gradientParts []string
	for i, lbl := range chartLabels {
		count := barLabelCounts[lbl]
		color := labelColors[lbl]
		if color == "" {
			color = chartColors[i%len(chartColors)]
		}
		pct := 0
		if maxTests > 0 {
			pct = count * 100 / maxTests
		}

		data.BarChart = append(data.BarChart, BarEntry{
			Label:   lbl,
			Value:   count,
			Percent: pct,
			Color:   color,
		})

		// Donut segment — proportional to each label's share of the summed
		// per-label counts (which may exceed TotalIts when specs are
		// multi-labelled).
		startDeg := 0
		if labelTotal > 0 {
			startDeg = cumulative * 360 / labelTotal
		}
		cumulative += count
		endDeg := 0
		if labelTotal > 0 {
			endDeg = cumulative * 360 / labelTotal
		}
		gradientParts = append(gradientParts, fmt.Sprintf("%s %ddeg %ddeg", color, startDeg, endDeg))
		data.DonutLegend = append(data.DonutLegend, LegendEntry{Label: lbl, Color: color})
	}
	if len(gradientParts) > 0 {
		data.ConicGradient = strings.Join(gradientParts, ", ")
	} else {
		data.ConicGradient = "var(--bd) 0deg 360deg"
	}

	// Label card counts.
	labelCounts := make(map[string]int)
	for _, f := range files {
		for _, b := range f.Blocks {
			for _, lbl := range b.Labels {
				labelCounts[lbl]++
			}
		}
	}
	for _, lbl := range orderedLabels {
		data.LabelCards = append(data.LabelCards, LabelCard{
			Name:  lbl,
			Count: labelCounts[lbl],
			Color: labelColors[lbl],
		})
	}

	return data
}

// applyResults annotates each It block in data.Files with its recorded
// outcome and tallies the pass/fail/skip totals. results maps a spec's leaf
// text (the It title) to its status. When results is empty, data is left
// unchanged and the report renders as a source-only coverage report.
func applyResults(data *ReportData, results map[string]string) {
	if len(results) == 0 {
		return
	}
	data.HasResults = true
	for fi := range data.Files {
		for bi := range data.Files[fi].Blocks {
			b := &data.Files[fi].Blocks[bi]
			if b.Type != "It" {
				continue
			}
			// Specs that ran but produced no matching result are treated as
			// skipped rather than silently counted as passing.
			status, ok := results[b.Title]
			if !ok {
				status = StatusSkipped
			}
			b.Status = status
			switch status {
			case StatusPassed:
				data.TotalPassed++
			case StatusFailed:
				data.TotalFailed++
			case StatusPending:
				data.TotalPending++
			default:
				data.TotalSkipped++
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Markdown generation
// ---------------------------------------------------------------------------

// statusIcon maps a spec outcome to its report emoji. An empty status (no
// results supplied) falls back to 🟢 so source-only coverage reports render as
// before.
func statusIcon(status string) string {
	switch status {
	case StatusFailed:
		return "🔴"
	case StatusSkipped:
		return "⚪"
	case StatusPending:
		return "🟡"
	default:
		return "🟢"
	}
}

func writeMarkdown(data *ReportData, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	p := func(format string, a ...interface{}) { fmt.Fprintf(f, format+"\n", a...) }

	p("# ✅ E2E Test Coverage Report")
	p("")
	if data.Workflow != "" {
		p("**Workflow:** %s  ", data.Workflow)
	}
	p("**Label filter:** `%s`  ", data.LabelFilter)
	p("**Generated:** %s", data.Timestamp)
	p("")
	p("---")
	p("")
	p("| Metric | Count |")
	p("|--------|------:|")
	p("| 📄 Test Files | **%d** |", data.TotalFiles)
	p("| 🧪 Test Cases | **%d** |", data.TotalIts)
	if data.HasResults {
		p("| 🟢 Passed | **%d** |", data.TotalPassed)
		p("| 🔴 Failed | **%d** |", data.TotalFailed)
		p("| ⚪ Skipped | **%d** |", data.TotalSkipped)
		if data.TotalPending > 0 {
			p("| 🟡 Pending | **%d** |", data.TotalPending)
		}
	}
	p("")
	p("---")
	p("")

	// Mermaid pie chart — tests per label.
	p("### 📈 Test Distribution")
	p("")
	p("```mermaid")
	p("pie title Tests by Label")
	for _, entry := range data.BarChart {
		p("    %q : %d", entry.Label, entry.Value)
	}
	p("```")
	p("")

	// Mermaid bar chart — tests per label.
	p("### 📊 Tests per Label")
	p("")
	p("```mermaid")
	p("xychart-beta horizontal")
	p("    title \"Tests per Label\"")
	barLabels := make([]string, len(data.BarChart))
	barValues := make([]string, len(data.BarChart))
	for i, entry := range data.BarChart {
		barLabels[i] = fmt.Sprintf("%q", entry.Label)
		barValues[i] = fmt.Sprintf("%d", entry.Value)
	}
	p("    x-axis [%s]", strings.Join(barLabels, ", "))
	p("    bar [%s]", strings.Join(barValues, ", "))
	p("```")
	p("")

	// Label coverage table with counts.
	p("### 🏷️ Coverage by Label")
	p("")
	p("| Label | Blocks | Description |")
	p("|-------|-------:|-------------|")
	for _, card := range data.LabelCards {
		p("| `%s` | **%d** | %s |", card.Name, card.Count, labelDescriptions[card.Name])
	}
	p("")
	p("---")
	p("")

	for _, tf := range data.Files {
		p("<details>")
		p("<summary><strong>📄 %s</strong> &mdash; %d tests</summary>", tf.Name, tf.TestCount)
		p("")
		for _, b := range tf.Blocks {
			badges := ""
			for _, lbl := range b.Labels {
				badges += " `" + lbl + "`"
			}
			switch b.Type {
			case "Describe":
				p("")
				p("#### ▸ %s%s", b.Title, badges)
				p("")
			case "Context":
				p("**◦ %s**%s", b.Title, badges)
				p("")
			case "It":
				p("- %s %s%s", statusIcon(b.Status), b.Title, badges)
			}
		}
		p("")
		p("</details>")
		p("")
	}

	p("---")
	p("")
	p("_Generated by `go run ./hack/e2e/report` · KAITO Production Stack_")

	return nil
}

// ---------------------------------------------------------------------------
// HTML generation
// ---------------------------------------------------------------------------

func writeHTML(data *ReportData, path string) error {
	funcMap := template.FuncMap{
		"lower": strings.ToLower,
		"statusColor": func(status string) string {
			switch status {
			case StatusFailed:
				return "var(--rd)"
			case StatusSkipped:
				return "var(--mt)"
			case StatusPending:
				return "var(--yl)"
			default:
				return "var(--gn)"
			}
		},
	}
	tmpl, err := template.New("report").Funcs(funcMap).Parse(htmlTemplateStr)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, data)
}
