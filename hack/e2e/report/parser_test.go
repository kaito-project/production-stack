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
	"testing"
)

func TestParseLabelConstants(t *testing.T) {
	content := `package utils

import g "github.com/onsi/ginkgo/v2"

var (
	GinkgoLabelSmoke = g.Label("Smoke")
	GinkgoLabelAuth  = g.Label("Auth")
	GinkgoLabelNightly = g.Label("Nightly")
)
`
	dir := t.TempDir()
	path := filepath.Join(dir, "ginkgo.go")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m := parseLabelConstants(path)

	if len(m) != 3 {
		t.Fatalf("expected 3 labels, got %d: %v", len(m), m)
	}
	for k, v := range map[string]string{
		"GinkgoLabelSmoke":   "Smoke",
		"GinkgoLabelAuth":    "Auth",
		"GinkgoLabelNightly": "Nightly",
	} {
		if m[k] != v {
			t.Errorf("expected %s=%q, got %q", k, v, m[k])
		}
	}
}

func TestParseLabelConstants_MissingFile(t *testing.T) {
	m := parseLabelConstants("/nonexistent/file.go")
	if len(m) != 0 {
		t.Fatalf("expected empty map for missing file, got %v", m)
	}
}

func TestExtractLabels_UtilsReferences(t *testing.T) {
	labelMap := map[string]string{
		"GinkgoLabelSmoke": "Smoke",
		"GinkgoLabelAuth":  "Auth",
	}

	labels := extractLabels(
		`var _ = Describe("Test", Ordered, utils.GinkgoLabelSmoke, utils.GinkgoLabelAuth, func() {`,
		labelMap,
	)

	if len(labels) != 2 {
		t.Fatalf("expected 2 labels, got %d: %v", len(labels), labels)
	}
	if labels[0] != "Smoke" || labels[1] != "Auth" {
		t.Errorf("expected [Smoke Auth], got %v", labels)
	}
}

func TestExtractLabels_InlineLabel(t *testing.T) {
	labels := extractLabels(
		`It("does stuff", Label("Fast"), func() {`,
		nil,
	)

	if len(labels) != 1 || labels[0] != "Fast" {
		t.Fatalf("expected [Fast], got %v", labels)
	}
}

func TestExtractLabels_MixedRefsAndInline(t *testing.T) {
	labelMap := map[string]string{
		"GinkgoLabelSmoke": "Smoke",
	}

	labels := extractLabels(
		`It("test", utils.GinkgoLabelSmoke, Label("Custom"), func() {`,
		labelMap,
	)

	if len(labels) != 2 {
		t.Fatalf("expected 2 labels, got %d: %v", len(labels), labels)
	}
	if labels[0] != "Smoke" || labels[1] != "Custom" {
		t.Errorf("expected [Smoke Custom], got %v", labels)
	}
}

func TestExtractLabels_NoLabels(t *testing.T) {
	labels := extractLabels(`It("plain test", func() {`, nil)
	if len(labels) != 0 {
		t.Fatalf("expected 0 labels, got %d: %v", len(labels), labels)
	}
}

func TestExtractLabels_UnknownUtilsRef(t *testing.T) {
	// When a utils.GinkgoLabelXyz isn't in the map, it should fall back
	// to stripping the "GinkgoLabel" prefix.
	labels := extractLabels(
		`It("test", utils.GinkgoLabelUnknown, func() {`,
		map[string]string{},
	)

	if len(labels) != 1 || labels[0] != "Unknown" {
		t.Fatalf("expected [Unknown], got %v", labels)
	}
}

func TestExtractLabels_Dedup(t *testing.T) {
	labelMap := map[string]string{"GinkgoLabelSmoke": "Smoke"}
	labels := extractLabels(
		`It("test", utils.GinkgoLabelSmoke, Label("Smoke"), func() {`,
		labelMap,
	)

	if len(labels) != 1 {
		t.Fatalf("expected 1 label (deduped), got %d: %v", len(labels), labels)
	}
}

func TestParseTestFiles_SingleLine(t *testing.T) {
	dir := t.TempDir()
	content := `package e2e

import . "github.com/onsi/ginkgo/v2"

var _ = Describe("Suite A", utils.GinkgoLabelSmoke, func() {
	Context("when ready", func() {
		It("does thing 1", func() {})
		It("does thing 2", func() {})
	})
})
`
	if err := os.WriteFile(filepath.Join(dir, "alpha_test.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	labelMap := map[string]string{"GinkgoLabelSmoke": "Smoke"}
	files := parseTestFiles(dir, labelMap)

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Name != "alpha_test.go" {
		t.Errorf("expected alpha_test.go, got %s", f.Name)
	}
	if f.TestCount != 2 {
		t.Errorf("expected 2 tests, got %d", f.TestCount)
	}
	if len(f.Blocks) != 4 {
		t.Errorf("expected 4 blocks, got %d", len(f.Blocks))
	}
	// First block should be the Describe with Smoke label.
	if len(f.Blocks[0].Labels) != 1 || f.Blocks[0].Labels[0] != "Smoke" {
		t.Errorf("expected Describe to have [Smoke], got %v", f.Blocks[0].Labels)
	}
}

func TestParseTestFiles_MultiLine(t *testing.T) {
	dir := t.TempDir()
	content := `package e2e

import . "github.com/onsi/ginkgo/v2"

var _ = Describe("Multi-line Suite",
	Ordered, utils.GinkgoLabelAuth, utils.GinkgoLabelSmoke, func() {
	It("test one", func() {})
})
`
	if err := os.WriteFile(filepath.Join(dir, "multi_test.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	labelMap := map[string]string{
		"GinkgoLabelSmoke": "Smoke",
		"GinkgoLabelAuth":  "Auth",
	}
	files := parseTestFiles(dir, labelMap)

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f := files[0]
	if f.TestCount != 1 {
		t.Fatalf("expected 1 test, got %d", f.TestCount)
	}
	// The Describe block should have both labels from the continuation line.
	desc := f.Blocks[0]
	if desc.Type != "Describe" {
		t.Fatalf("expected Describe, got %s", desc.Type)
	}
	if len(desc.Labels) != 2 {
		t.Fatalf("expected 2 labels on multi-line Describe, got %d: %v", len(desc.Labels), desc.Labels)
	}
	if desc.Labels[0] != "Auth" || desc.Labels[1] != "Smoke" {
		t.Errorf("expected [Auth Smoke], got %v", desc.Labels)
	}
}

func TestParseTestFiles_SkipsE2ETest(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"e2e_test.go", "real_test.go"} {
		content := `package e2e
import . "github.com/onsi/ginkgo/v2"
var _ = Describe("Suite", func() {
	It("test", func() {})
})
`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	files := parseTestFiles(dir, nil)

	if len(files) != 1 {
		t.Fatalf("expected 1 file (e2e_test.go skipped), got %d", len(files))
	}
	if files[0].Name != "real_test.go" {
		t.Errorf("expected real_test.go, got %s", files[0].Name)
	}
}

func TestParseTestFiles_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	files := parseTestFiles(dir, nil)
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

func TestParseTestFiles_LabelInheritance(t *testing.T) {
	// Verify that It blocks inside a Describe inherit the Describe's labels
	// via EffectiveLabels, even when the It itself has no labels.
	dir := t.TempDir()
	content := `package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

var _ = Describe("Scaling Suite",
	Ordered, utils.GinkgoLabelScaling, utils.GinkgoLabelNightly, func() {
	It("scale up", func() {})
	It("scale down", func() {})
})
`
	if err := os.WriteFile(filepath.Join(dir, "scaling_test.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	labelMap := map[string]string{
		"GinkgoLabelScaling": "Scaling",
		"GinkgoLabelNightly": "Nightly",
	}
	files := parseTestFiles(dir, labelMap)

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f := files[0]
	if f.TestCount != 2 {
		t.Fatalf("expected 2 tests, got %d", f.TestCount)
	}

	// Find the It blocks and check EffectiveLabels.
	var itBlocks []Block
	for _, b := range f.Blocks {
		if b.Type == "It" {
			itBlocks = append(itBlocks, b)
		}
	}
	if len(itBlocks) != 2 {
		t.Fatalf("expected 2 It blocks, got %d", len(itBlocks))
	}

	for _, it := range itBlocks {
		// Own labels should be empty (no Label() on the It line).
		if len(it.Labels) != 0 {
			t.Errorf("It %q: expected no own labels, got %v", it.Title, it.Labels)
		}
		// Effective labels should include the parent Describe's labels.
		effective := make(map[string]bool)
		for _, l := range it.EffectiveLabels {
			effective[l] = true
		}
		if !effective["Scaling"] {
			t.Errorf("It %q: expected EffectiveLabels to contain Scaling, got %v", it.Title, it.EffectiveLabels)
		}
		if !effective["Nightly"] {
			t.Errorf("It %q: expected EffectiveLabels to contain Nightly, got %v", it.Title, it.EffectiveLabels)
		}
	}
}

func TestParseTestFiles_TwoDescribesSeparateLabels(t *testing.T) {
	// Verify that two sibling Describe blocks don't leak labels into each other.
	dir := t.TempDir()
	content := `package e2e

import . "github.com/onsi/ginkgo/v2"

var _ = Describe("Nightly Suite", Label("Nightly"), func() {
	It("nightly test", func() {})
})

var _ = Describe("Smoke Suite", Label("Smoke"), func() {
	It("smoke test", func() {})
})
`
	if err := os.WriteFile(filepath.Join(dir, "two_test.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	files := parseTestFiles(dir, nil)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f := files[0]

	var itBlocks []Block
	for _, b := range f.Blocks {
		if b.Type == "It" {
			itBlocks = append(itBlocks, b)
		}
	}
	if len(itBlocks) != 2 {
		t.Fatalf("expected 2 It blocks, got %d", len(itBlocks))
	}

	// "nightly test" should only have Nightly in EffectiveLabels.
	eff0 := make(map[string]bool)
	for _, l := range itBlocks[0].EffectiveLabels {
		eff0[l] = true
	}
	if !eff0["Nightly"] || eff0["Smoke"] {
		t.Errorf("nightly test EffectiveLabels: want [Nightly], got %v", itBlocks[0].EffectiveLabels)
	}

	// "smoke test" should only have Smoke in EffectiveLabels.
	eff1 := make(map[string]bool)
	for _, l := range itBlocks[1].EffectiveLabels {
		eff1[l] = true
	}
	if eff1["Nightly"] || !eff1["Smoke"] {
		t.Errorf("smoke test EffectiveLabels: want [Smoke], got %v", itBlocks[1].EffectiveLabels)
	}
}
