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

// model_unavailable (zero ready inference endpoints) tests.
//
// What this verifies (and why):
//
//	The modeldeployment chart always renders the HTTPRoute, InferencePool,
//	and EPP for a model regardless of whether the pool currently has ready
//	endpoints. When the InferenceSet is scaled to replicas=0, the route +
//	EPP stay healthy but the EPP can pick NO backend, so Envoy raises a
//	router/upstream flag (no healthy upstream). Because BBR has already
//	injected X-Gateway-Model-Name, the local reply carries the model header
//	PRESENT together with that router flag — mapper #2 of the consolidated
//	per-namespace outage local_reply
//	(charts/modelharness/templates/envoyfilter-outage-reply.yaml) — which
//	rewrites it to a `503 model_unavailable` envelope carrying
//	`x-kaito-error-source: inferenceset` and a `Retry-After` header.
//
//	The `code` is deliberately root-cause-neutral (proposal §2.3 item 3):
//	warm-up / crash / OOM / eviction all surface the SAME response shape,
//	and the operator-facing root cause is published as a control-plane
//	Warning Event in kube-system rather than carried on the response. This
//	suite proves the request-path code is root-cause-agnostic and is
//	distinct from `404 model_not_found` (route exists, pool is empty).
//
// Not Serial: scaling THIS case's InferenceSet to zero perturbs only its
// own namespace's inference pool, not any cluster-wide singleton, so the
// suite is safe to run alongside other (non-overlapping) specs.
var _ = Describe("model_unavailable (zero ready inference endpoints)",
	Ordered, utils.GinkgoLabelDataplaneOutage, utils.GinkgoLabelNightly, func() {

		var (
			ctx          context.Context
			caseURL      string
			caseNS       string
			modelName    string
			origReplicas int32
		)

		BeforeAll(func() {
			ctx = context.Background()

			caseURL = InstallCase(CaseModelUnavailable)
			caseNS = CaseNamespace(CaseModelUnavailable)
			// The InferenceSet is named after the deployment Name (the chart
			// `.Values.name`), matching the scaling helpers' convention.
			modelName = CaseDeployments[CaseModelUnavailable][0].Name

			// Sanity: a valid request must succeed BEFORE we induce the
			// empty-pool state, otherwise a 503 below would be meaningless.
			Eventually(func() int {
				resp, sErr := utils.SendChatCompletion(caseURL, modelName)
				if sErr != nil {
					return 0
				}
				defer resp.Body.Close()
				return resp.StatusCode
			}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusOK),
				"baseline request should succeed before scaling the InferenceSet to zero")
		})

		AfterAll(func() {
			// Always restore the InferenceSet so we never leave this
			// namespace's pool empty for subsequent specs, even if an
			// assertion above failed.
			if origReplicas > 0 {
				Expect(utils.SetInferenceSetReplicas(ctx, modelName, caseNS, origReplicas)).
					To(Succeed(), "failed to restore InferenceSet replicas")
			}
			UninstallCase(CaseModelUnavailable)
		})

		It("maps an empty inference pool to 503 model_unavailable (not 404 model_not_found)", func() {
			By("scaling the InferenceSet to replicas=0")
			var err error
			origReplicas, err = utils.GetInferenceSetReplicas(ctx, modelName, caseNS)
			Expect(err).NotTo(HaveOccurred(), "failed to read InferenceSet replica count")
			Expect(origReplicas).To(BeNumerically(">", 0), "InferenceSet should have had >0 replicas before the test")
			Expect(utils.SetInferenceSetReplicas(ctx, modelName, caseNS, 0)).
				To(Succeed(), "failed to scale InferenceSet to zero")

			By("sending a valid chat completion and asserting the model_unavailable envelope")
			Eventually(func(g Gomega) {
				resp, sErr := utils.SendChatCompletion(caseURL, modelName)
				g.Expect(sErr).NotTo(HaveOccurred(), "request to gateway failed")
				defer resp.Body.Close()

				status := resp.StatusCode
				errSource := resp.Header.Get("x-kaito-error-source")
				retryAfter := resp.Header.Get("retry-after")
				parsed, pErr := utils.ParseErrorResponse(resp)
				g.Expect(pErr).NotTo(HaveOccurred(), "response body should be a JSON error envelope")
				code := parsed.ErrorCode()

				// Regression guard: an EMPTY pool (route exists) must surface
				// as model_unavailable, never as the model-does-not-exist 404.
				g.Expect(status).NotTo(Equal(http.StatusNotFound),
					"empty pool must not surface as 404")
				g.Expect(code).NotTo(Equal("model_not_found"),
					"empty pool must not surface as model_not_found")

				// Unified outage envelope produced by mapper #2 of the
				// consolidated per-namespace local_reply.
				g.Expect(status).To(Equal(http.StatusServiceUnavailable),
					"empty pool should surface as 503")
				g.Expect(code).To(Equal("model_unavailable"),
					"empty pool should carry code model_unavailable")
				g.Expect(errSource).To(Equal("inferenceset"),
					"empty pool should carry x-kaito-error-source: inferenceset")
				g.Expect(retryAfter).NotTo(BeEmpty(),
					"model_unavailable should carry a Retry-After header")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("recovers once the InferenceSet is scaled back up", func() {
			By("restoring the InferenceSet to its original replica count")
			Expect(origReplicas).To(BeNumerically(">", 0),
				"previous spec must have captured the original replica count")
			Expect(utils.SetInferenceSetReplicas(ctx, modelName, caseNS, origReplicas)).
				To(Succeed(), "failed to restore InferenceSet replicas")

			By("sending a valid chat completion and asserting it succeeds again")
			Eventually(func() int {
				resp, sErr := utils.SendChatCompletion(caseURL, modelName)
				if sErr != nil {
					return 0
				}
				defer resp.Body.Close()
				return resp.StatusCode
			}, utils.InferenceSetReadyTimeout, 10*time.Second).Should(Equal(http.StatusOK),
				"requests should succeed again once the inference pool has ready endpoints")
		})
	})
