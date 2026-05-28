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
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// Matches: GinkgoLabelFoo = g.Label("Foo")
	labelConstRe = regexp.MustCompile(`(\w+)\s*=\s*\w+\.Label\("([^"]+)"\)`)
	// Matches: Describe("title" | Context("title" | It("title"
	blockRe = regexp.MustCompile(`(Describe|Context|It)\("([^"]+)"`)
	// Matches: utils.GinkgoLabelFoo
	utilsLabelRe = regexp.MustCompile(`utils\.(GinkgoLabel\w+)`)
	// Matches: Label("Foo")
	inlineLabelRe = regexp.MustCompile(`Label\("([^"]+)"\)`)
)

// parseLabelConstants reads utils/ginkgo.go and returns a map from
// Go constant name (e.g. "GinkgoLabelSmoke") to label string (e.g. "Smoke").
func parseLabelConstants(path string) map[string]string {
	m := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if matches := labelConstRe.FindStringSubmatch(sc.Text()); matches != nil {
			m[matches[1]] = matches[2]
		}
	}
	return m
}

// extractLabels pulls all Ginkgo labels from a single source line.
// It handles both utils.GinkgoLabelFoo references and inline Label("Foo") calls.
func extractLabels(line string, labelMap map[string]string) []string {
	var labels []string
	seen := make(map[string]bool)

	for _, m := range utilsLabelRe.FindAllStringSubmatch(line, -1) {
		varName := m[1]
		display := labelMap[varName]
		if display == "" {
			display = strings.TrimPrefix(varName, "GinkgoLabel")
		}
		if !seen[display] {
			labels = append(labels, display)
			seen[display] = true
		}
	}

	for _, m := range inlineLabelRe.FindAllStringSubmatch(line, -1) {
		lbl := m[1]
		if !seen[lbl] {
			labels = append(labels, lbl)
			seen[lbl] = true
		}
	}

	return labels
}

// labelFrame is one entry in the label-inheritance stack maintained while
// parsing a single file. Each Describe or Context block pushes a frame;
// the frame is popped when we encounter a new block at an equal or shallower
// indentation level.
type labelFrame struct {
	indent int
	labels []string
}

// stackLabels returns the flattened, deduplicated union of all labels
// currently on the inheritance stack.
func stackLabels(stack []labelFrame) []string {
	seen := make(map[string]bool)
	var result []string
	for _, frame := range stack {
		for _, lbl := range frame.labels {
			if !seen[lbl] {
				seen[lbl] = true
				result = append(result, lbl)
			}
		}
	}
	return result
}

// mergeLabels returns a deduplicated union of base and extra.
func mergeLabels(base, extra []string) []string {
	seen := make(map[string]bool, len(base))
	result := make([]string, len(base))
	copy(result, base)
	for _, l := range base {
		seen[l] = true
	}
	for _, l := range extra {
		if !seen[l] {
			seen[l] = true
			result = append(result, l)
		}
	}
	return result
}

// parseTestFiles scans every *_test.go file in e2eDir (skipping e2e_test.go)
// and returns the parsed blocks grouped by file.
func parseTestFiles(e2eDir string, labelMap map[string]string) []TestFile {
	matches, _ := filepath.Glob(filepath.Join(e2eDir, "*_test.go"))
	sort.Strings(matches)

	var files []TestFile
	for _, path := range matches {
		name := filepath.Base(path)
		if name == "e2e_test.go" {
			continue
		}

		f, err := os.Open(path)
		if err != nil {
			continue
		}

		var blocks []Block
		tests := 0
		// labelStack tracks label inheritance: each Describe/Context pushes its
		// own labels; a frame is popped when we see a block at the same or
		// shallower indentation level.
		var labelStack []labelFrame
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			// Measure leading whitespace to determine nesting depth.
			indent := len(line) - len(strings.TrimLeft(line, "\t "))
			// Pop any frames whose block has ended (current indent ≤ frame indent).
			for len(labelStack) > 0 && indent <= labelStack[len(labelStack)-1].indent {
				labelStack = labelStack[:len(labelStack)-1]
			}
			m := blockRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			// When the block call spans multiple lines (e.g. labels on the
			// next line), concatenate continuation lines until we see func()
			// or hit a reasonable limit.
			full := line
			for i := 0; i < 5 && !strings.Contains(full, "func()"); i++ {
				if !sc.Scan() {
					break
				}
				full += " " + strings.TrimSpace(sc.Text())
			}
			ownLabels := extractLabels(full, labelMap)
			b := Block{
				Type:            m[1],
				Title:           m[2],
				Labels:          ownLabels,
				EffectiveLabels: mergeLabels(stackLabels(labelStack), ownLabels),
			}
			// Describe/Context blocks push their own labels onto the stack so
			// descendant It blocks can inherit them.
			if b.Type == "Describe" || b.Type == "Context" {
				labelStack = append(labelStack, labelFrame{indent: indent, labels: ownLabels})
			}
			blocks = append(blocks, b)
			if b.Type == "It" {
				tests++
			}
		}
		f.Close()

		files = append(files, TestFile{
			Name:      name,
			Blocks:    blocks,
			TestCount: tests,
		})
	}
	return files
}
