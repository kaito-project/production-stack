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

// Upstream-gating / suppression tests (issue #87, proposal §1.4). When an
// upstream (cluster-layer) reason is active, the reporter must SUPPRESS the
// downstream per-InferenceSet/per-namespace reasons it would otherwise emit,
// and append a transparency suffix to the surviving upstream Event naming the
// suppressed downstream reasons and the namespace count.
//
// This suite scales the cluster-wide KAITO controller to zero (an upstream
// clusterKaitoControllerNotReady condition), so it is Serial.
var _ = Describe("Upstream Gating Reporter",
	Ordered, Serial, utils.GinkgoLabelStatusReporter, func() {

		const (
			emitTimeout  = 3 * time.Minute
			clearTimeout = 5 * time.Minute
			quietDwell   = 90 * time.Second

			kaitoNamespace  = "kaito-system"
			kaitoDeployment = "kaito-workspace"
		)

		var ctx context.Context

		BeforeEach(func() {
			ctx = context.Background()
		})

		It("suppresses downstream inferenceset reasons while the KAITO controller is down", func() {
			desired, _, err := utils.GetDeploymentReplicas(ctx, kaitoNamespace, kaitoDeployment)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = utils.ScaleDeployment(ctx, kaitoNamespace, kaitoDeployment, desired)
				_ = utils.WaitForDeploymentReplicas(ctx, kaitoNamespace, kaitoDeployment, 1, clearTimeout)
			})

			By("scaling the KAITO controller to zero")
			Expect(utils.ScaleDeployment(ctx, kaitoNamespace, kaitoDeployment, 0)).To(Succeed())

			By("waiting for the upstream clusterKaitoControllerNotReady Warning")
			_, err = utils.WaitForReporterEvent(ctx, "clusterKaitoControllerNotReady", "", emitTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("asserting downstream inferenceset reasons are suppressed while the upstream is open")
			Expect(utils.EnsureNoReporterEvent(ctx, "inferencesetInfraProvisioningFailed", "", quietDwell)).
				To(Succeed())
			Expect(utils.EnsureNoReporterEvent(ctx, "inferencesetModelPodsNotReady", "", quietDwell)).
				To(Succeed())
		})
	})
