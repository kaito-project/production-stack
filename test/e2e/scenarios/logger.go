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

// Logger is the minimal logging surface scenario code uses for
// diagnostics and cleanup-warning output. It intentionally has a single
// method so it is trivial to adapt (Ginkgo's GinkgoWriter.Printf,
// AIManager's structured logger, or a fake that records calls for unit
// tests).
//
// Logger itself does not redact anything — callers MUST route any
// secret-shaped value (API keys, bearer tokens) through Redact before
// including it in a Logf call. This keeps the redaction policy in one
// place (this file) rather than duplicated at every call site.
type Logger interface {
	Logf(format string, args ...any)
}

// NoopLogger discards every message. Useful as a safe default when a
// caller does not care about scenario diagnostics/cleanup-warning output.
type NoopLogger struct{}

// Logf implements Logger.
func (NoopLogger) Logf(string, ...any) {}

// redactedTailLen is the number of trailing characters of a secret that
// Redact keeps visible (enough to eyeball "same value across calls"
// without reconstructing the secret from logs).
const redactedTailLen = 4

// Redact masks a secret-shaped value (API key, bearer token) for safe
// inclusion in log output: it keeps only the last few characters and
// replaces everything else with a fixed-width mask, so the mask itself
// never leaks the secret's true length. Empty input returns "" (nothing
// to redact); a non-empty input shorter than the visible tail is masked
// in full.
func Redact(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= redactedTailLen {
		return "****"
	}
	return "****" + secret[len(secret)-redactedTailLen:]
}
