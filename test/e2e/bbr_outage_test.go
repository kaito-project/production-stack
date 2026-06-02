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

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// BBR (body-based-routing) outage tests.
//
// What this verifies (and why):
//
//	The cluster-wide BBR ext_proc filter is configured fail-closed
//	(`failure_mode_allow: false`, see
//	charts/modelharness/templates/envoyfilter-bbr.yaml).
//	When the BBR Deployment is unreachable, Envoy generates a LOCAL reply
//	for the request rather than skipping the filter. Without the
//	per-namespace outage local_reply
//	(charts/modelharness/templates/envoyfilter-outage-reply.yaml), that
//	local reply would either be an unbodied 500 OR — if BBR failed open —
//	the request would lose its `X-Gateway-Model-Name` header and fall
//	through the per-namespace catch-all EnvoyFilter as a misleading
//	`404 model_not_found` (proposal Story 5).
//
//	This suite scales BBR to zero and asserts the request is mapped to a
//	structured `502 bbr_unavailable` envelope carrying
//	`x-kaito-error-source: bbr`, and explicitly NOT a `404 model_not_found`.
//
// Why Serial: BBR is a cluster-wide singleton shared by every Gateway in
// the mesh. Scaling it to zero breaks every other in-flight inference
// request, so this suite must not run concurrently with any other spec.
var _ = Describe("BBR outage (fail-closed cluster filter)",
	Ordered, Serial, utils.GinkgoLabelClusterOutage, func() {

		const (
			bbrNamespace      = "kaito-system"
			bbrDeploymentName = "body-based-router"
		)

		var (
			ctx          context.Context
			caseURL      string
			modelName    string
			origReplicas int32
		)

		BeforeAll(func() {
			ctx = context.Background()

			caseURL = InstallCase(CaseBBROutage)
			modelName = CaseDeployments[CaseBBROutage][0].Name

			// Sanity: a valid request must succeed BEFORE we induce the
			// outage, otherwise a 502 below would be meaningless.
			Eventually(func() int {
				resp, sErr := utils.SendChatCompletion(caseURL, modelName)
				if sErr != nil {
					return 0
				}
				defer resp.Body.Close()
				return resp.StatusCode
			}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusOK),
				"baseline request should succeed before inducing the BBR outage")
		})

		AfterAll(func() {
			// Always restore BBR so we never leave the shared cluster
			// filter broken for subsequent (Serial) suites, even if an
			// assertion above failed.
			if origReplicas > 0 {
				Expect(utils.ScaleDeployment(ctx, bbrNamespace, bbrDeploymentName, origReplicas)).
					To(Succeed(), "failed to restore BBR replicas")
				Expect(utils.WaitForDeploymentReplicas(ctx, bbrNamespace, bbrDeploymentName, origReplicas, 3*time.Minute)).
					To(Succeed(), "BBR did not return to %d ready replicas", origReplicas)
			}
			UninstallCase(CaseBBROutage)
		})

		It("maps a BBR outage to 502 bbr_unavailable (not 404 model_not_found)", func() {
			By("scaling the BBR Deployment to zero")
			var err error
			origReplicas, _, err = utils.GetDeploymentReplicas(ctx, bbrNamespace, bbrDeploymentName)
			Expect(err).NotTo(HaveOccurred(), "failed to read BBR replica count")
			Expect(origReplicas).To(BeNumerically(">", 0), "BBR should have had >0 replicas before the outage")
			Expect(utils.ScaleDeployment(ctx, bbrNamespace, bbrDeploymentName, 0)).
				To(Succeed(), "failed to scale BBR to zero")

			By("sending a valid chat completion and asserting the outage envelope")
			Eventually(func(g Gomega) {
				resp, sErr := utils.SendChatCompletion(caseURL, modelName)
				g.Expect(sErr).NotTo(HaveOccurred(), "request to gateway failed")
				defer resp.Body.Close()

				status := resp.StatusCode
				errSource := resp.Header.Get("x-kaito-error-source")
				parsed, pErr := utils.ParseErrorResponse(resp)
				g.Expect(pErr).NotTo(HaveOccurred(), "response body should be a JSON error envelope")
				code := parsed.ErrorCode()

				// Core Story-5 regression guard: a BBR outage must NEVER be
				// misreported as an unknown model.
				g.Expect(status).NotTo(Equal(http.StatusNotFound),
					"BBR outage must not surface as 404")
				g.Expect(code).NotTo(Equal("model_not_found"),
					"BBR outage must not surface as model_not_found")

				// Unified outage envelope produced by the per-namespace
				// local_reply (charts/modelharness/templates/envoyfilter-outage-reply.yaml).
				g.Expect(status).To(Equal(http.StatusBadGateway),
					"BBR outage should surface as 502")
				g.Expect(code).To(Equal("bbr_unavailable"),
					"BBR outage should carry code bbr_unavailable")
				g.Expect(errSource).To(Equal("bbr"),
					"BBR outage should carry x-kaito-error-source: bbr")
			}, 90*time.Second, 5*time.Second).Should(Succeed())
		})

		It("recovers once BBR is restored", func() {
			By("scaling the BBR Deployment back to its original replica count")
			Expect(origReplicas).To(BeNumerically(">", 0),
				"previous spec must have captured the original replica count")
			Expect(utils.ScaleDeployment(ctx, bbrNamespace, bbrDeploymentName, origReplicas)).
				To(Succeed(), "failed to restore BBR replicas")
			Expect(utils.WaitForDeploymentReplicas(ctx, bbrNamespace, bbrDeploymentName, origReplicas, 3*time.Minute)).
				To(Succeed(), "BBR did not return to %d ready replicas", origReplicas)

			By("sending a valid chat completion and asserting it succeeds again")
			Eventually(func() int {
				resp, sErr := utils.SendChatCompletion(caseURL, modelName)
				if sErr != nil {
					return 0
				}
				defer resp.Body.Close()
				return resp.StatusCode
			}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusOK),
				"requests should succeed again once BBR is healthy")
		})
	})
