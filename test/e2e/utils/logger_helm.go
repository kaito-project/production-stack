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

package utils

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo DSL

	"github.com/kaito-project/production-stack/test/e2e/scenarios"
)

// GinkgoLogger implements scenarios.Logger by writing to GinkgoWriter,
// so scenario diagnostics/cleanup-warning output is captured and
// attributed by Ginkgo the same way By() output already is.
type GinkgoLogger struct{}

var _ scenarios.Logger = GinkgoLogger{}

// Logf implements scenarios.Logger.
func (GinkgoLogger) Logf(format string, args ...any) {
	GinkgoWriter.Printf(format+"\n", args...)
}
