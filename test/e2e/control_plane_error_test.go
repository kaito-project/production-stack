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

// Modeldeployment-layer (per-InferenceSet) status reporter tests (issue #87,
// proposal §1.2 inferenceset reasons). These perturb the request-path
// EPP owned by a single InferenceSet in a deployed case namespace and asserts
// the reporter surfaces the matching inferenceset* Warning on the
// workload Namespace (the reporter's involvedObject is always cluster-scoped).
//
// The perturbation is namespace-scoped to one EPP Deployment, so this suite
// does not need to be Serial.
var _ = Describe("Control-plane Error Reporter",
	Ordered, utils.GinkgoLabelStatusReporter, func() {

		const (
			emitTimeout  = 3 * time.Minute
			clearTimeout = 5 * time.Minute
		)

		var (
			ctx       context.Context
			caseNS    string
			modelName string
		)

		BeforeAll(func() {
			ctx = context.Background()
			InstallCase(CaseControlPlaneError)
			caseNS = CaseNamespace(CaseControlPlaneError)
			modelName = CaseDeployments[CaseControlPlaneError][0].Name
		})

		AfterAll(func() {
			UninstallCase(CaseControlPlaneError)
		})

		It("emits inferencesetEPPNotReady when the InferenceSet EPP is scaled to zero", func() {
			eppName := utils.EPPServiceName(modelName)
			desired, _, err := utils.GetDeploymentReplicas(ctx, caseNS, eppName)
			Expect(err).NotTo(HaveOccurred())
			baseline, err := utils.ReporterEventBaseline(ctx, "inferencesetEPPNotReady", caseNS)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = utils.ScaleDeployment(ctx, caseNS, eppName, desired)
				_ = utils.WaitForDeploymentReplicas(ctx, caseNS, eppName, desired, clearTimeout)
			})

			By("scaling the EPP Deployment to zero")
			Expect(utils.ScaleDeployment(ctx, caseNS, eppName, 0)).To(Succeed())

			ev, err := utils.WaitForReporterEventSince(ctx, "inferencesetEPPNotReady", caseNS, baseline, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
			Expect(ev.Message).To(ContainSubstring(modelName))
			Expect(ev.Message).To(ContainSubstring("scaled to zero"))
			Expect(ev.Message).NotTo(ContainSubstring("http://"))
			Expect(ev.Message).NotTo(ContainSubstring("https://"))
		})
	})
