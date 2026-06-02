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

// ext_authz (llm-gateway-auth) outage tests.
//
// What this verifies (and why):
//
//	The cluster-wide ext_authz filter contributed by llm-gateway-auth
//	(the `apikey-authz` dataplane) sits FIRST in the Gateway HTTP filter
//	chain. The chart configures it fail-closed so an outage cannot let
//	unauthenticated traffic through. When `apikey-authz` is unreachable,
//	Envoy raises the UAEX (UnauthorizedExternalService) response flag and
//	generates a LOCAL reply. Without the per-namespace outage local_reply
//	(charts/modelharness/templates/envoyfilter-outage-reply.yaml) that
//	reply would be an opaque error — or, had ext_authz failed open, the
//	request would proceed unauthenticated and could fall through the
//	per-namespace catch-all as a misleading `404 model_not_found`
//	(proposal Story 5).
//
//	Primary assertion (always in-scope, fully under this repo's control):
//	an ext_authz outage must NOT surface as `404 model_not_found`.
//
//	Secondary assertion (best-effort): when the outage reply is a 5xx, the
//	per-namespace local_reply maps it to a `502 ext_authz_unavailable`
//	envelope with `x-kaito-error-source: authz`. This depends on
//	llm-gateway-auth emitting a 5xx `status_on_error` under fail-closed,
//	which is owned by the separate kaito-project/llm-gateway-auth repo
//	(proposal §3). The assertion is therefore only enforced when a 5xx is
//	actually returned, so this suite does not flake on that upstream's
//	rollout state.
//
// Why Serial: `apikey-authz` is a cluster-wide singleton shared by every
// auth-enabled Gateway. Scaling it to zero breaks every other in-flight
// authenticated request, so this suite must not run concurrently with any
// other spec.
var _ = Describe("ext_authz outage (fail-closed cluster filter)",
	Ordered, Serial, utils.GinkgoLabelClusterOutage, utils.GinkgoLabelAuth, func() {

		const (
			authNamespace      = "llm-gateway-auth"
			authDeploymentName = "apikey-authz"
		)

		var (
			ctx          context.Context
			caseURL      string
			caseNS       string
			modelName    string
			hostHeader   string
			apiKey       string
			origReplicas int32
		)

		BeforeAll(func() {
			ctx = context.Background()

			caseURL = InstallCase(CaseExtAuthzOutage)
			caseNS = CaseNamespace(CaseExtAuthzOutage)
			modelName = CaseDeployments[CaseExtAuthzOutage][0].Name
			hostHeader = caseNS + ".gw.example.com"

			Eventually(func() (string, error) {
				return utils.GetAPIKeyFromSecret(ctx, caseNS)
			}, 60*time.Second, 2*time.Second).ShouldNot(BeEmpty(),
				"API key Secret should be created in %s", caseNS)
			var err error
			apiKey, err = utils.GetAPIKeyFromSecret(ctx, caseNS)
			Expect(err).NotTo(HaveOccurred())

			// Sanity: an authenticated request must succeed BEFORE we
			// induce the outage.
			Eventually(func() int {
				resp, sErr := utils.SendChatCompletionWithAuth(caseURL, modelName, "hello", apiKey, hostHeader)
				if sErr != nil {
					return 0
				}
				defer resp.Body.Close()
				return resp.StatusCode
			}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusOK),
				"baseline authenticated request should succeed before inducing the ext_authz outage")
		})

		AfterAll(func() {
			// Always restore apikey-authz so we never leave the shared
			// cluster filter broken for subsequent (Serial) suites.
			if origReplicas > 0 {
				Expect(utils.ScaleDeployment(ctx, authNamespace, authDeploymentName, origReplicas)).
					To(Succeed(), "failed to restore apikey-authz replicas")
				Expect(utils.WaitForDeploymentReplicas(ctx, authNamespace, authDeploymentName, origReplicas, 3*time.Minute)).
					To(Succeed(), "apikey-authz did not return to %d ready replicas", origReplicas)
			}
			UninstallCase(CaseExtAuthzOutage)
		})

		It("does not map an ext_authz outage to 404 model_not_found", func() {
			By("scaling the apikey-authz Deployment to zero")
			var err error
			origReplicas, _, err = utils.GetDeploymentReplicas(ctx, authNamespace, authDeploymentName)
			Expect(err).NotTo(HaveOccurred(), "failed to read apikey-authz replica count")
			Expect(origReplicas).To(BeNumerically(">", 0), "apikey-authz should have had >0 replicas before the outage")
			Expect(utils.ScaleDeployment(ctx, authNamespace, authDeploymentName, 0)).
				To(Succeed(), "failed to scale apikey-authz to zero")

			By("sending an authenticated request and asserting it is not a 404 model_not_found")
			Eventually(func(g Gomega) {
				resp, sErr := utils.SendChatCompletionWithAuth(caseURL, modelName, "hello", apiKey, hostHeader)
				g.Expect(sErr).NotTo(HaveOccurred(), "request to gateway failed")
				defer resp.Body.Close()

				status := resp.StatusCode
				errSource := resp.Header.Get("x-kaito-error-source")
				parsed, pErr := utils.ParseErrorResponse(resp)
				// A non-JSON body is acceptable for the primary guard; only
				// the status / code must not be a model_not_found 404.
				code := ""
				if pErr == nil {
					code = parsed.ErrorCode()
				}

				// Core Story-5 regression guard (always enforced): an
				// ext_authz outage must NEVER be misreported as an unknown
				// model.
				g.Expect(status).NotTo(Equal(http.StatusNotFound),
					"ext_authz outage must not surface as 404")
				g.Expect(code).NotTo(Equal("model_not_found"),
					"ext_authz outage must not surface as model_not_found")

				// Best-effort unified-envelope assertion: only enforced when
				// the upstream returns a 5xx (i.e. llm-gateway-auth's
				// fail-closed 5xx status_on_error is in effect). See the
				// suite doc comment for why this is conditional.
				if status >= http.StatusInternalServerError {
					g.Expect(status).To(Equal(http.StatusBadGateway),
						"a 5xx ext_authz outage should be mapped to 502")
					g.Expect(code).To(Equal("ext_authz_unavailable"),
						"a 5xx ext_authz outage should carry code ext_authz_unavailable")
					g.Expect(errSource).To(Equal("authz"),
						"a 5xx ext_authz outage should carry x-kaito-error-source: authz")
				}
			}, 90*time.Second, 5*time.Second).Should(Succeed())
		})

		It("recovers once ext_authz is restored", func() {
			By("scaling the apikey-authz Deployment back to its original replica count")
			Expect(origReplicas).To(BeNumerically(">", 0),
				"previous spec must have captured the original replica count")
			Expect(utils.ScaleDeployment(ctx, authNamespace, authDeploymentName, origReplicas)).
				To(Succeed(), "failed to restore apikey-authz replicas")
			Expect(utils.WaitForDeploymentReplicas(ctx, authNamespace, authDeploymentName, origReplicas, 3*time.Minute)).
				To(Succeed(), "apikey-authz did not return to %d ready replicas", origReplicas)

			By("sending an authenticated request and asserting it succeeds again")
			Eventually(func() int {
				resp, sErr := utils.SendChatCompletionWithAuth(caseURL, modelName, "hello", apiKey, hostHeader)
				if sErr != nil {
					return 0
				}
				defer resp.Body.Close()
				return resp.StatusCode
			}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusOK),
				"authenticated requests should succeed again once ext_authz is healthy")
		})
	})
