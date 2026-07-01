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

package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Weight-download windowed-evaluation tests (issue #87, proposal §1.2
// inferencesetWeightDownloadSlow). The reporter must only raise the slow
// reason for a SUSTAINED low-throughput window — a healthy or fast download
// must never trip it. The full ring-buffer window semantics (sustained-slow
// raises, transient-dip and partial-window do not) are exhaustively covered by
// the deterministic unit tests in pkg/.../scraper/window_test.go; here we
// assert the deterministic e2e negative: a healthy deployed case never emits a
// spurious inferencesetWeightDownloadSlow.
var _ = Describe("Weight Download Slow Reporter",
	Ordered, utils.GinkgoLabelStatusReporter, func() {

		const quietDwell = 2 * time.Minute

		var (
			ctx    context.Context
			caseNS string
		)

		BeforeAll(func() {
			ctx = context.Background()
			InstallCase(CaseWeightDownloadSlow)
			caseNS = CaseNamespace(CaseWeightDownloadSlow)
		})

		AfterAll(func() {
			UninstallCase(CaseWeightDownloadSlow)
		})

		It("does not emit inferencesetWeightDownloadSlow for a healthy deployment", func() {
			Expect(utils.EnsureNoReporterEvent(ctx, "inferencesetWeightDownloadSlow", caseNS, quietDwell)).
				To(Succeed(),
					"a healthy/fast weight download must never trip the sustained-slow window")
		})
	})
