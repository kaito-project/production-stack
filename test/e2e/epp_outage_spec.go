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
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// EPP (InferencePool ext_proc) outage tests.
//
// What this verifies (and why):
//
//	Each modeldeployment renders an EPP (endpoint picker) wired into the
//	Gateway as the InferencePool ext_proc filter. The InferencePool is
//	configured failureMode: FailClose
//	(charts/modeldeployment/templates/inferencepool.yaml), so when the EPP
//	Deployment is unreachable Envoy generates a LOCAL reply (the
//	ext_proc default 500) rather than routing blindly.
//
//	Because BBR has already injected X-Gateway-Model-Name by the time the
//	EPP ext_proc runs, this local 500 carries the model header PRESENT and
//	NO router/upstream flag (the EPP failed before a backend was picked).
//	That combination is mapper #4 of the consolidated per-namespace outage
//	local_reply (charts/modelharness/templates/envoyfilter-outage-reply.yaml),
//	which rewrites it to a structured `502 epp_unavailable` envelope
//	carrying `x-kaito-error-source: epp`.
//
//	This suite scales the EPP Deployment to zero and asserts that mapping,
//	and explicitly that the outage is NOT misreported as a
//	`404 model_not_found` (proposal §2.2 / §2.3). It also confirms the
//	deleted per-modeldeployment EnvoyFilter is no longer required: the
//	consolidated modelharness filter owns this attribution.
//
// Not Serial: scaling THIS case's EPP Deployment perturbs only its own
// namespace's request path, not any cluster-wide singleton, so the suite
// is safe to run alongside other (non-overlapping) specs.
var _ = Describe("EPP outage (fail-closed InferencePool ext_proc)",
	Ordered, utils.GinkgoLabelOutage, utils.GinkgoLabelNightly,
	utils.GinkgoLabelStandardK8sOnly, func() {

		var (
			ctx         context.Context
			caseGateway deploy.GatewayEndpoint
			modelName   string
			replicas    *utils.DeploymentReplicaGuard
		)

		BeforeAll(func() {
			ctx = context.Background()
			DeferCleanup(UninstallCase, CaseEPPOutage)

			caseGateway = InstallCase(CaseEPPOutage)
			modelName = CaseDeployments[CaseEPPOutage][0].Name
			// The EPP Deployment is named "<name>-inferencepool-epp" by the
			// modeldeployment chart (modeldeployment.eppServiceName).
			replicas = prepareDeploymentOutage(ctx, CaseNamespace(CaseEPPOutage), utils.EPPServiceName(modelName))

			// Sanity: a valid request must succeed BEFORE we induce the
			// outage, otherwise a 502 below would be meaningless.
			Eventually(func() error {
				return utils.CheckChatSuccess(ctx, caseGateway, modelName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed(),
				"baseline request should succeed before inducing the EPP outage")
		})

		It("maps an EPP outage to 502 epp_unavailable (not 404 model_not_found)", func() {
			By("scaling the EPP Deployment to zero")
			Expect(replicas.ScaleAndWait(ctx, 0, 2*time.Minute)).
				To(Succeed(), "failed to scale EPP to zero")

			By("sending a valid chat completion and asserting the outage envelope")
			Eventually(func(g Gomega) {
				resp, sErr := utils.SendChat(caseGateway, modelName)
				g.Expect(sErr).NotTo(HaveOccurred(), "request to gateway failed")
				status := resp.StatusCode
				errSource := resp.Header.Get("x-kaito-error-source")
				parsed, pErr := utils.ParseErrorResponse(resp)
				g.Expect(pErr).NotTo(HaveOccurred(), "response body should be a JSON error envelope")
				code := parsed.ErrorCode()

				// Regression guard: an EPP outage must NEVER be misreported
				// as an unknown model.
				g.Expect(status).NotTo(Equal(http.StatusNotFound),
					"EPP outage must not surface as 404")
				g.Expect(code).NotTo(Equal("model_not_found"),
					"EPP outage must not surface as model_not_found")

				// Unified outage envelope produced by mapper #4 of the
				// consolidated per-namespace local_reply
				// (charts/modelharness/templates/envoyfilter-outage-reply.yaml).
				g.Expect(status).To(Equal(http.StatusBadGateway),
					"EPP outage should surface as 502")
				g.Expect(code).To(Equal("epp_unavailable"),
					"EPP outage should carry code epp_unavailable")
				g.Expect(errSource).To(Equal("epp"),
					"EPP outage should carry x-kaito-error-source: epp")
			}, 90*time.Second, 5*time.Second).Should(Succeed())
		})

		It("recovers once the EPP is restored", func() {
			By("scaling the EPP Deployment back to its original replica count")
			Expect(replicas.Restore(ctx, 3*time.Minute)).
				To(Succeed(), "failed to restore EPP replicas")

			By("sending a valid chat completion and asserting it succeeds again")
			Eventually(func() error {
				return utils.CheckChatSuccess(ctx, caseGateway, modelName)
			}, 2*time.Minute, 5*time.Second).Should(Succeed(),
				"requests should succeed again once the EPP is healthy")
		})
	})
