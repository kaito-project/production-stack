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
	Workflow       string
	LabelFilter    string
	Timestamp      string
	TotalFiles     int
	TotalDescribes int
	TotalContexts  int
	TotalIts       int

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
	"Smoke", "Infra", "Routing", "Auth",
	"Scaling", "ScaleUp", "ScaleDown", "AntiFlapping",
	"FilterOrder", "Nightly", "NetworkPolicy",
	"PrefixCache", "InferenceSet",
}

var labelDescriptions = map[string]string{
	"Smoke":         "Basic sanity checks — every PR",
	"Infra":         "Infrastructure lifecycle (nodes, pods, GC)",
	"Routing":       "Gateway / model routing correctness",
	"Auth":          "API key authentication",
	"Scaling":       "KEDA-driven scale-up / scale-down",
	"ScaleUp":       "Scale-up specific assertions",
	"ScaleDown":     "Scale-down specific assertions",
	"AntiFlapping":  "Prevents premature re-scaling",
	"FilterOrder":   "Envoy HTTP filter chain execution order",
	"Nightly":       "Long-running tests (nightly only)",
	"NetworkPolicy": "Kubernetes NetworkPolicy enforcement",
	"PrefixCache":   "Prefix / KV-cache aware routing",
	"InferenceSet":  "InferenceSet chart lifecycle",
}

var labelColors = map[string]string{
	"Smoke":         "#3fb950",
	"Infra":         "#bc8cff",
	"Routing":       "#f0883e",
	"Auth":          "#f85149",
	"Scaling":       "#39d353",
	"ScaleUp":       "#39d353",
	"ScaleDown":     "#39d353",
	"AntiFlapping":  "#39d353",
	"FilterOrder":   "#58a6ff",
	"Nightly":       "#d29922",
	"NetworkPolicy": "#bc8cff",
	"PrefixCache":   "#f0883e",
	"InferenceSet":  "#bc8cff",
}

var chartColors = []string{
	"#58a6ff", "#3fb950", "#f85149", "#bc8cff",
	"#f0883e", "#d29922", "#39d353", "#8b949e",
	"#79c0ff", "#56d364",
}

// ---------------------------------------------------------------------------
// Report data construction
// ---------------------------------------------------------------------------

func buildReportData(files []TestFile, workflow, labelFilter string) *ReportData {
	data := &ReportData{
		Workflow:    workflow,
		LabelFilter: labelFilter,
		Timestamp:   time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		Files:       files,
		TotalFiles:  len(files),
		AllLabels:   orderedLabels,
	}

	for _, f := range files {
		data.TotalIts += f.TestCount
		data.TotalDescribes += f.SuiteCount
		data.TotalContexts += f.ContextCount
	}

	// Bar chart data.
	maxTests := 0
	for _, f := range files {
		if f.TestCount > maxTests {
			maxTests = f.TestCount
		}
	}

	cumulative := 0
	var gradientParts []string
	for i, f := range files {
		color := chartColors[i%len(chartColors)]
		pct := 0
		if maxTests > 0 {
			pct = f.TestCount * 100 / maxTests
		}
		shortName := strings.TrimSuffix(f.Name, "_test.go")

		data.BarChart = append(data.BarChart, BarEntry{
			Label:   shortName,
			Value:   f.TestCount,
			Percent: pct,
			Color:   color,
		})

		// Donut segment.
		startDeg := 0
		if data.TotalIts > 0 {
			startDeg = cumulative * 360 / data.TotalIts
		}
		cumulative += f.TestCount
		endDeg := 0
		if data.TotalIts > 0 {
			endDeg = cumulative * 360 / data.TotalIts
		}
		gradientParts = append(gradientParts, fmt.Sprintf("%s %ddeg %ddeg", color, startDeg, endDeg))
		data.DonutLegend = append(data.DonutLegend, LegendEntry{Label: shortName, Color: color})
	}
	if len(gradientParts) > 0 {
		data.ConicGradient = strings.Join(gradientParts, ", ")
	} else {
		data.ConicGradient = "var(--bd) 0deg 360deg"
	}

	// Label card counts.
	labelCounts := make(map[string]int)
	for _, lbl := range orderedLabels {
		labelCounts[lbl] = 0
	}
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

// ---------------------------------------------------------------------------
// Markdown generation
// ---------------------------------------------------------------------------

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
	p("| 📋 Suites | **%d** |", data.TotalDescribes)
	p("| 📂 Contexts | **%d** |", data.TotalContexts)
	p("| 🧪 Test Cases | **%d** |", data.TotalIts)
	p("")
	p("---")
	p("")

	// Mermaid pie chart — tests per file.
	p("### 📈 Test Distribution")
	p("")
	p("```mermaid")
	p("pie title Tests by File")
	for _, entry := range data.BarChart {
		p("    %q : %d", entry.Label, entry.Value)
	}
	p("```")
	p("")

	// Mermaid bar chart — tests per file.
	p("### 📊 Tests per File")
	p("")
	p("```mermaid")
	p("xychart-beta horizontal")
	p("    title \"Tests per File\"")
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
		p("<summary><strong>📄 %s</strong> &mdash; %d tests, %d suite(s)</summary>", tf.Name, tf.TestCount, tf.SuiteCount)
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
				p("- 🟢 %s%s", b.Title, badges)
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
