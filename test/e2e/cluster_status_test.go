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

// Cluster-layer status reporter tests (issue #87, proposal §1.2 cluster
// reasons). Each spec perturbs a shared cluster-wide control-plane Deployment
// and asserts the productionstack-status-reporter emits the matching Warning
// Event in kube-system within one reporter resync, and that the Warning ceases
// once the problem is fixed (the reporter publishes no positive recovery
// event).
//
// These perturb cluster-wide singletons every other namespace shares, so the
// suite is decorated Serial.
var _ = Describe("Cluster Status Reporter",
	Serial, utils.GinkgoLabelStatusReporter, func() {

		const (
			// Resync default is 30s; allow a couple of cycles plus rollout slack.
			emitTimeout  = 3 * time.Minute
			clearTimeout = 5 * time.Minute

			bbrNamespace  = "kaito-system"
			bbrDeployment = "body-based-router"

			gatewayAuthNamespace  = "llm-gateway-auth"
			gatewayAuthDeployment = "apikey-authz"

			kaitoNamespace  = "kaito-system"
			kaitoDeployment = "kaito-workspace"
		)

		var ctx context.Context

		BeforeEach(func() {
			ctx = context.Background()
		})

		// scaleToZeroAndAssert scales a Deployment to zero, registers a
		// safety-net restore (so a failed assertion cannot leave the shared
		// control plane degraded), then runs assert while the Deployment is
		// down. The restore returns the Deployment to its original replica
		// count and waits for that many ready replicas.
		scaleToZeroAndAssert := func(ns, name string, original int32, assert func()) {
			DeferCleanup(func() {
				_ = utils.ScaleDeployment(ctx, ns, name, original)
				_ = utils.WaitForDeploymentReplicas(ctx, ns, name, original, clearTimeout)
			})
			Expect(utils.ScaleDeployment(ctx, ns, name, 0)).To(Succeed())
			assert()
		}

		It("emits clusterBBRNotReady when BBR is scaled to zero and clears on recovery",
			utils.GinkgoLabelSmoke, func() {
				desired, _, err := utils.GetDeploymentReplicas(ctx, bbrNamespace, bbrDeployment)
				Expect(err).NotTo(HaveOccurred())

				By("scaling BBR to zero and waiting for the clusterBBRNotReady Warning")
				scaleToZeroAndAssert(bbrNamespace, bbrDeployment, desired, func() {
					_, err := utils.WaitForReporterEvent(ctx, "clusterBBRNotReady", "", emitTimeout)
					Expect(err).NotTo(HaveOccurred())
				})

				By("restoring BBR and waiting for it to become ready")
				Expect(utils.ScaleDeployment(ctx, bbrNamespace, bbrDeployment, desired)).To(Succeed())
				Expect(utils.WaitForDeploymentReplicas(ctx, bbrNamespace, bbrDeployment, desired, clearTimeout)).To(Succeed())

				// Recovery is signalled by the Warning ceasing, not by a
				// positive event: the reporter only publishes problems. Baseline
				// the last clusterBBRNotReady once BBR is ready, then require no
				// fresh emission over a quiet window.
				By("waiting for clusterBBRNotReady to stop firing once BBR is restored")
				recoverBaseline, err := utils.ReporterEventBaseline(ctx, "clusterBBRNotReady", "")
				Expect(err).NotTo(HaveOccurred())
				Expect(utils.EnsureNoReporterEventSince(ctx, "clusterBBRNotReady", "", recoverBaseline, emitTimeout)).
					To(Succeed())
			})

		It("emits clusterGatewayAuthNotReady when llm-gateway-auth is scaled to zero", func() {
			desired, _, err := utils.GetDeploymentReplicas(ctx, gatewayAuthNamespace, gatewayAuthDeployment)
			Expect(err).NotTo(HaveOccurred())

			baseline, err := utils.ReporterEventBaseline(ctx, "clusterGatewayAuthNotReady", "")
			Expect(err).NotTo(HaveOccurred())

			By("scaling llm-gateway-auth to zero and waiting for the clusterGatewayAuthNotReady Warning")
			scaleToZeroAndAssert(gatewayAuthNamespace, gatewayAuthDeployment, desired, func() {
				_, err := utils.WaitForReporterEventSince(ctx, "clusterGatewayAuthNotReady", "", baseline, emitTimeout)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		It("emits clusterKaitoControllerNotReady when the KAITO controller is scaled to zero", func() {
			desired, _, err := utils.GetDeploymentReplicas(ctx, kaitoNamespace, kaitoDeployment)
			Expect(err).NotTo(HaveOccurred())

			baseline, err := utils.ReporterEventBaseline(ctx, "clusterKaitoControllerNotReady", "")
			Expect(err).NotTo(HaveOccurred())

			By("scaling the KAITO controller to zero and waiting for the clusterKaitoControllerNotReady Warning")
			scaleToZeroAndAssert(kaitoNamespace, kaitoDeployment, desired, func() {
				_, err := utils.WaitForReporterEventSince(ctx, "clusterKaitoControllerNotReady", "", baseline, emitTimeout)
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})
