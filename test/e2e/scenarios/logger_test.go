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

package scenarios

import (
	"strings"
	"testing"
)

func TestRedact_EmptyStringStaysEmpty(t *testing.T) {
	if got := Redact(""); got != "" {
		t.Errorf("Redact(\"\") = %q, want \"\"", got)
	}
}

func TestRedact_NeverContainsTheFullSecret(t *testing.T) {
	secrets := []string{
		"sk-1234567890abcdef",
		"short",
		"a",
		"exactly4",
	}
	for _, s := range secrets {
		got := Redact(s)
		if got == s {
			t.Errorf("Redact(%q) returned the secret unchanged", s)
		}
		if len(s) > redactedTailLen && strings.Contains(got, s[:len(s)-redactedTailLen]) {
			t.Errorf("Redact(%q) = %q leaks the secret's prefix", s, got)
		}
	}
}

func TestRedact_KeepsAShortRecognizableTail(t *testing.T) {
	got := Redact("sk-1234567890abcdef")
	if !strings.HasSuffix(got, "cdef") {
		t.Errorf("Redact(...) = %q, want it to end with the last %d chars for correlation", got, redactedTailLen)
	}
	if !strings.HasPrefix(got, "****") {
		t.Errorf("Redact(...) = %q, want a masked prefix", got)
	}
}

func TestFakeLogger_NeverReceivesRawSecret_WhenScenarioCodeRedacts(t *testing.T) {
	// This test documents the expected discipline: scenario code must
	// pass secrets through Redact before handing them to Logger. We
	// simulate that discipline here and assert the log line is
	// secret-safe end to end.
	log := &fakeLogger{}
	apiKey := "sk-super-secret-key-000111222"

	log.Logf("using api key %s", Redact(apiKey))

	for _, line := range log.all() {
		if strings.Contains(line, apiKey) {
			t.Fatalf("log line %q contains the raw secret", line)
		}
	}
}
