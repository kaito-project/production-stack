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

// generate-e2e-report parses Ginkgo E2E test source files and produces
// Markdown + HTML coverage reports for GitHub Actions.
//
// Usage:
//
//	go run ./hack/e2e/report [flags]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// Block represents a single Ginkgo Describe, Context, or It block.
type Block struct {
	Type   string // "Describe", "Context", or "It"
	Title  string
	Labels []string
}

// TestFile collects all parsed blocks from a single *_test.go file.
type TestFile struct {
	Name         string
	Blocks       []Block
	TestCount    int // number of It blocks
	SuiteCount   int // number of Describe blocks
	ContextCount int // number of Context blocks
}

func main() {
	labelFilter := flag.String("label-filter", "(all)", "Ginkgo label filter shown in report header")
	outputMD := flag.String("output-md", "e2e-coverage-report.md", "path for Markdown output")
	outputHTML := flag.String("output-html", "e2e-coverage-report.html", "path for HTML output")
	workflow := flag.String("workflow", "", "GitHub Actions workflow name")
	flag.Parse()

	repoRoot, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	e2eDir := filepath.Join(repoRoot, "test", "e2e")

	labelMap := parseLabelConstants(filepath.Join(e2eDir, "utils", "ginkgo.go"))
	files := parseTestFiles(e2eDir, labelMap)

	data := buildReportData(files, *workflow, *labelFilter)

	if err := writeMarkdown(data, *outputMD); err != nil {
		fmt.Fprintf(os.Stderr, "markdown: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Markdown report → %s\n", *outputMD)

	if err := writeHTML(data, *outputHTML); err != nil {
		fmt.Fprintf(os.Stderr, "html: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ HTML report  → %s\n", *outputHTML)
}

// findRepoRoot walks up from the working directory until it finds go.mod.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod in any parent directory")
		}
		dir = parent
	}
}
