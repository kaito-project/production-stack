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
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

var _ = Describe("API Key Authentication", Ordered, utils.GinkgoLabelAuth, utils.GinkgoLabelSmoke, func() {
	// EnsureNamespace provisions the per-namespace AuthorizationPolicy and APIKey CR
	// (the cluster-wide MeshConfig provider is installed once by the
	// llm-gateway-apikey chart).
	authDeployment := CaseDeployments[CaseAuth][0]
	modelName := authDeployment.Name
	caseNamespace := CaseNamespace(CaseAuth)

	var (
		ctx         context.Context
		authHeaders []deploy.AuthHeader
		caseGateway deploy.GatewayEndpoint
	)

	BeforeAll(func() {
		ctx = context.Background()
		caseGateway = InstallCase(CaseAuth)

		var err error
		authHeaders, err = utils.NamespaceAuthHeaders(ctx, caseNamespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(authHeaders).NotTo(BeEmpty())
	})

	AfterAll(func() {
		By("Uninstalling auth model deployment (removes AuthorizationPolicy, APIKey CR)")
		UninstallCase(CaseAuth)
	})

	It("should reject requests without an Authorization header (401)", func() {
		Eventually(func() int {
			// Deliberately unauthenticated: attaching the valid key here would
			// return 200 and mask a policy that is not being enforced.
			resp, err := utils.SendChat(caseGateway, modelName, utils.WithoutAuth())
			if err != nil {
				return 0 // treat request errors as non-401 responses to keep retrying
			}

			defer resp.Body.Close()

			return resp.StatusCode

		}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusUnauthorized),
			"request without auth should be rejected with 401")
	})

	It("should reject requests with an invalid API key (401)", func() {
		resp, err := utils.SendChat(caseGateway, modelName,
			utils.WithAuth(deploy.AuthHeader{Name: authHeaders[0].Name, Value: "Bearer invalid-key-12345"}))
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized),
			fmt.Sprintf("invalid key should be rejected; got status %d", resp.StatusCode))
	})

	// Check each header the backend publishes: a gateway that accepts the
	// credential in several headers is asserted on all of them, and one that
	// pins a single transport is not failed for the others it never claimed.
	It("should accept requests carrying a valid API key (200)", func() {
		Expect(authHeaders).NotTo(BeEmpty())
		for _, header := range authHeaders {
			By("sending the key in the " + header.Name + " header")
			Eventually(func() error {
				resp, err := utils.SendChat(caseGateway, modelName,
					utils.WithAuth(header))
				if err != nil {
					return fmt.Errorf("request failed: %w", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					body, _ := utils.ReadResponseBody(resp)
					return fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(body))
				}
				return nil
			}, 2*time.Minute, 5*time.Second).Should(Succeed(),
				"request with valid API key in %s header should succeed with 200", header.Name)
		}
	})

	// Model discovery must sit BEHIND authentication: ext_authz is an HTTP
	// filter and therefore runs before route selection, so the /v1/models
	// routes are covered by the same policy as inference traffic. Without
	// this an unauthenticated caller could enumerate a tenant's models.
	Context("Model discovery is gated by ext_authz", utils.GinkgoLabelModelDiscovery, func() {
		modelsPaths := func() []string {
			return []string{utils.ModelsPath, utils.ModelRetrievePath(modelName)}
		}

		It("should reject discovery requests without an Authorization header (401)", func() {
			for _, path := range modelsPaths() {
				Eventually(func() int {
					resp, err := utils.SendModels(caseGateway, path, utils.WithoutAuth())
					if err != nil {
						return 0
					}
					defer resp.Body.Close()
					return resp.StatusCode
				}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusUnauthorized),
					"unauthenticated %s must not enumerate models", path)
			}
		})

		It("should reject discovery requests with an invalid API key (401)", func() {
			for _, path := range modelsPaths() {
				resp, err := utils.SendModels(caseGateway, path,
					utils.WithAuth(deploy.AuthHeader{Name: authHeaders[0].Name, Value: "Bearer invalid-key-12345"}))
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized),
					"invalid key should be rejected on %s", path)
			}
		})

		It("should list and retrieve the namespace's models with a valid API key (200)", func() {
			Eventually(func() error {
				resp, err := utils.SendModels(caseGateway, utils.ModelsPath)
				if err != nil {
					return fmt.Errorf("request failed: %w", err)
				}
				if resp.StatusCode != http.StatusOK {
					body, _ := utils.ReadResponseBody(resp)
					return fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(body))
				}
				list, err := utils.ParseModelList(resp)
				if err != nil {
					return err
				}
				if !slices.Contains(list.IDs(), modelName) {
					return fmt.Errorf("model %q missing from listing %v", modelName, list.IDs())
				}
				return nil
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			resp, err := utils.SendModels(caseGateway, utils.ModelRetrievePath(modelName))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			model, err := utils.ParseModel(resp)
			Expect(err).NotTo(HaveOccurred())
			Expect(model.ID).To(Equal(modelName))
		})
	})

	Context("Browser CORS", utils.GinkgoLabelCORS, func() {
		const disallowedOrigin = "https://disallowed.e2e.test"

		sendPreflight := func(origin, method, headers string) (*http.Response, error) {
			options := []utils.RequestOption{
				utils.WithoutAuth(),
				utils.WithHeader("Origin", origin),
				utils.WithHeader("Access-Control-Request-Method", method),
			}
			if headers != "" {
				options = append(options, utils.WithHeader("Access-Control-Request-Headers", headers))
			}
			return utils.SendGatewayRequest(ctx, caseGateway, http.MethodOptions, utils.ChatCompletionsPath, nil, options...)
		}

		expectActualCORSHeaders := func(response *http.Response, origin string) {
			Expect(response.Header.Get("Access-Control-Allow-Origin")).To(Equal(origin))
			Expect(response.Header.Get("Access-Control-Allow-Credentials")).To(Equal("true"))
			Expect(commaSeparatedHeaderTokens(response, "Access-Control-Expose-Headers")).To(ConsistOf(
				"x-kaito-error-source", "x-kaito-requested-model"))
			Expect(commaSeparatedHeaderTokens(response, "Vary")).To(ContainElement("origin"))
		}

		DescribeTable("answers an allowed credential-less preflight locally",
			func(origin string) {
				response, err := sendPreflight(origin, http.MethodPost, "authorization, Content-Type")
				Expect(err).NotTo(HaveOccurred())
				defer response.Body.Close()

				Expect(response.StatusCode).To(Equal(http.StatusNoContent))
				body, err := io.ReadAll(response.Body)
				Expect(err).NotTo(HaveOccurred())
				Expect(body).To(BeEmpty())
				Expect(response.Header.Get("Access-Control-Allow-Origin")).To(Equal(origin))
				Expect(response.Header.Get("Access-Control-Allow-Credentials")).To(Equal("true"))
				Expect(response.Header.Get("Access-Control-Max-Age")).To(Equal("600"))
				Expect(commaSeparatedHeaderTokens(response, "Access-Control-Allow-Methods")).To(ConsistOf("get", "post"))
				Expect(commaSeparatedHeaderTokens(response, "Access-Control-Allow-Headers")).To(ConsistOf(
					"authorization", "content-type", "api-key", "x-api-key"))
				Expect(commaSeparatedHeaderTokens(response, "Vary")).To(ConsistOf(
					"origin", "access-control-request-method", "access-control-request-headers"))
			},
			Entry("primary exact origin", utils.E2ECORSAllowedOrigin),
			Entry("second exact origin", utils.E2ECORSAllowedOriginAlternate),
		)

		DescribeTable("rejects disallowed preflights locally",
			func(origin, method, headers string) {
				response, err := sendPreflight(origin, method, headers)
				Expect(err).NotTo(HaveOccurred())
				defer response.Body.Close()
				Expect(response.StatusCode).To(Equal(http.StatusForbidden))
				Expect(response.Header.Get("Access-Control-Allow-Origin")).To(BeEmpty())
			},
			Entry("origin", disallowedOrigin, http.MethodPost, "authorization,content-type"),
			Entry("method", utils.E2ECORSAllowedOrigin, http.MethodDelete, "authorization,content-type"),
			Entry("request header", utils.E2ECORSAllowedOrigin, http.MethodPost, "authorization,x-not-allowed"),
		)

		It("does not treat a bare OPTIONS request as an auth bypass", func() {
			response, err := utils.SendGatewayRequest(ctx, caseGateway, http.MethodOptions, utils.ChatCompletionsPath, nil,
				utils.WithoutAuth())
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusUnauthorized))
		})

		It("does not treat an incomplete preflight as an auth bypass", func() {
			response, err := utils.SendGatewayRequest(ctx, caseGateway, http.MethodOptions, utils.ChatCompletionsPath, nil,
				utils.WithoutAuth(), utils.WithHeader("Origin", utils.E2ECORSAllowedOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusUnauthorized))
		})

		It("keeps an allowed-origin actual request behind authentication", func() {
			response, err := utils.SendChat(caseGateway, modelName,
				utils.WithoutAuth(), utils.WithHeader("Origin", utils.E2ECORSAllowedOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusUnauthorized))
			expectActualCORSHeaders(response, utils.E2ECORSAllowedOrigin)
		})

		It("returns CORS headers after authenticated model routing", func() {
			response, err := utils.SendChat(caseGateway, modelName,
				utils.WithHeader("Origin", utils.E2ECORSAllowedOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusOK))
			expectActualCORSHeaders(response, utils.E2ECORSAllowedOrigin)
		})

		It("rejects an actual request from a disallowed origin before routing", func() {
			response, err := utils.SendChat(caseGateway, modelName,
				utils.WithHeader("Origin", disallowedOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusForbidden))
			Expect(response.Header.Get("Access-Control-Allow-Origin")).To(BeEmpty())
		})

		It("rejects an actual request with a disallowed method before authentication", func() {
			response, err := utils.SendGatewayRequest(ctx, caseGateway, http.MethodDelete, utils.ChatCompletionsPath, nil,
				utils.WithoutAuth(), utils.WithHeader("Origin", utils.E2ECORSAllowedOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusForbidden))
			Expect(response.Header.Get("Access-Control-Allow-Origin")).To(BeEmpty())
		})

		It("adds CORS headers to the authenticated direct-response fallback", func() {
			response, err := utils.SendChat(caseGateway, "cors-unknown-model",
				utils.WithHeader("Origin", utils.E2ECORSAllowedOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusNotFound))
			expectActualCORSHeaders(response, utils.E2ECORSAllowedOrigin)
		})
	})

	Context("Wildcard browser CORS", utils.GinkgoLabelCORS, func() {
		const arbitraryOrigin = "https://wildcard.e2e.test"

		sendPreflight := func(origin, method, headers string) (*http.Response, error) {
			options := []utils.RequestOption{
				utils.WithoutAuth(),
				utils.WithHeader("Origin", origin),
				utils.WithHeader("Access-Control-Request-Method", method),
			}
			if headers != "" {
				options = append(options, utils.WithHeader("Access-Control-Request-Headers", headers))
			}
			return utils.SendGatewayRequest(ctx, caseGateway, http.MethodOptions, utils.ChatCompletionsPath, nil, options...)
		}

		expectActualCORSHeaders := func(response *http.Response) {
			Expect(response.Header.Values("Access-Control-Allow-Origin")).To(Equal([]string{"*"}))
			Expect(response.Header.Values("Access-Control-Allow-Credentials")).To(BeEmpty())
			Expect(commaSeparatedHeaderTokens(response, "Access-Control-Expose-Headers")).To(ConsistOf(
				"x-kaito-error-source", "x-kaito-requested-model"))
			Expect(commaSeparatedHeaderTokens(response, "Vary")).NotTo(ContainElement("origin"))
		}

		BeforeAll(func() {
			allowCredentials := false
			By("reconciling the existing modelharness into non-credentialed wildcard mode")
			Expect(utils.ReconcileModelHarnessCORS(ctx, caseNamespace, deploy.CORSValues{
				Enabled:          true,
				AllowedOrigins:   []string{"*"},
				AllowCredentials: &allowCredentials,
			})).To(Succeed())

			By("waiting for the wildcard Envoy configuration to reach the Gateway")
			Eventually(func() error {
				response, err := sendPreflight(arbitraryOrigin, http.MethodPost, "authorization,content-type")
				if err != nil {
					return err
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusNoContent {
					return fmt.Errorf("preflight status = %d, want 204", response.StatusCode)
				}
				if values := response.Header.Values("Access-Control-Allow-Origin"); !slices.Equal(values, []string{"*"}) {
					return fmt.Errorf("Access-Control-Allow-Origin = %v, want [*]", values)
				}
				if values := response.Header.Values("Access-Control-Allow-Credentials"); len(values) != 0 {
					return fmt.Errorf("Access-Control-Allow-Credentials = %v, want absent", values)
				}
				if vary := commaSeparatedHeaderTokens(response, "Vary"); slices.Contains(vary, "origin") {
					return fmt.Errorf("Vary = %v, must not contain Origin", vary)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		DescribeTable("answers wildcard preflights locally",
			func(origin string) {
				response, err := sendPreflight(origin, http.MethodPost, "authorization, Content-Type")
				Expect(err).NotTo(HaveOccurred())
				defer response.Body.Close()

				Expect(response.StatusCode).To(Equal(http.StatusNoContent))
				body, err := io.ReadAll(response.Body)
				Expect(err).NotTo(HaveOccurred())
				Expect(body).To(BeEmpty())
				Expect(response.Header.Values("Access-Control-Allow-Origin")).To(Equal([]string{"*"}))
				Expect(response.Header.Values("Access-Control-Allow-Credentials")).To(BeEmpty())
				Expect(response.Header.Get("Access-Control-Max-Age")).To(Equal("600"))
				Expect(commaSeparatedHeaderTokens(response, "Access-Control-Allow-Methods")).To(ConsistOf("get", "post"))
				Expect(commaSeparatedHeaderTokens(response, "Access-Control-Allow-Headers")).To(ConsistOf(
					"authorization", "content-type", "api-key", "x-api-key"))
				Expect(commaSeparatedHeaderTokens(response, "Vary")).To(ConsistOf(
					"access-control-request-method", "access-control-request-headers"))
			},
			Entry("for an arbitrary HTTPS origin", arbitraryOrigin),
			Entry("for an opaque origin", "null"),
		)

		DescribeTable("rejects wildcard preflights that violate another allowlist",
			func(origin, method, headers string) {
				response, err := sendPreflight(origin, method, headers)
				Expect(err).NotTo(HaveOccurred())
				defer response.Body.Close()
				Expect(response.StatusCode).To(Equal(http.StatusForbidden))
				Expect(response.Header.Values("Access-Control-Allow-Origin")).To(BeEmpty())
				Expect(response.Header.Values("Access-Control-Allow-Credentials")).To(BeEmpty())
				Expect(commaSeparatedHeaderTokens(response, "Vary")).To(ConsistOf(
					"access-control-request-method", "access-control-request-headers"))
			},
			Entry("empty origin", "", http.MethodPost, "authorization,content-type"),
			Entry("method", arbitraryOrigin, http.MethodDelete, "authorization,content-type"),
			Entry("request header", arbitraryOrigin, http.MethodPost, "authorization,x-not-allowed"),
		)

		It("does not treat a bare OPTIONS request as an auth bypass", func() {
			response, err := utils.SendGatewayRequest(ctx, caseGateway, http.MethodOptions, utils.ChatCompletionsPath, nil,
				utils.WithoutAuth())
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusUnauthorized))
		})

		It("does not treat an incomplete wildcard preflight as an auth bypass", func() {
			response, err := utils.SendGatewayRequest(ctx, caseGateway, http.MethodOptions, utils.ChatCompletionsPath, nil,
				utils.WithoutAuth(), utils.WithHeader("Origin", arbitraryOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusUnauthorized))
		})

		It("keeps a wildcard actual request behind authentication", func() {
			response, err := utils.SendChat(caseGateway, modelName,
				utils.WithoutAuth(), utils.WithHeader("Origin", arbitraryOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusUnauthorized))
			expectActualCORSHeaders(response)
		})

		DescribeTable("returns wildcard headers after authenticated model routing",
			func(origin string) {
				response, err := utils.SendChat(caseGateway, modelName, utils.WithHeader("Origin", origin))
				Expect(err).NotTo(HaveOccurred())
				defer response.Body.Close()
				Expect(response.StatusCode).To(Equal(http.StatusOK))
				expectActualCORSHeaders(response)
			},
			Entry("for an arbitrary HTTPS origin", arbitraryOrigin),
			Entry("for an opaque origin", "null"),
		)

		It("adds wildcard headers to the authenticated direct-response fallback", func() {
			response, err := utils.SendChat(caseGateway, "cors-wildcard-unknown-model",
				utils.WithHeader("Origin", arbitraryOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusNotFound))
			expectActualCORSHeaders(response)
		})

		It("rejects an actual request with a disallowed method without origin variance", func() {
			response, err := utils.SendGatewayRequest(ctx, caseGateway, http.MethodDelete, utils.ChatCompletionsPath, nil,
				utils.WithoutAuth(), utils.WithHeader("Origin", arbitraryOrigin))
			Expect(err).NotTo(HaveOccurred())
			defer response.Body.Close()
			Expect(response.StatusCode).To(Equal(http.StatusForbidden))
			Expect(response.Header.Values("Access-Control-Allow-Origin")).To(BeEmpty())
			Expect(response.Header.Values("Access-Control-Allow-Credentials")).To(BeEmpty())
			Expect(commaSeparatedHeaderTokens(response, "Vary")).NotTo(ContainElement("origin"))
		})
	})
})

func commaSeparatedHeaderTokens(response *http.Response, name string) []string {
	var tokens []string
	for _, value := range response.Header.Values(name) {
		for token := range strings.SplitSeq(value, ",") {
			if normalized := strings.ToLower(strings.TrimSpace(token)); normalized != "" {
				tokens = append(tokens, normalized)
			}
		}
	}
	return tokens
}
