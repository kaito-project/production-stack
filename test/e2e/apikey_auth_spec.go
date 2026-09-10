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
	"net/http"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

var _ = Describe("API Key Authentication", Ordered, utils.GinkgoLabelAuth, utils.GinkgoLabelSmoke, func() {
	// CaseAuth deployment — AuthAPIKeyEnabled=true causes EnsureNamespace
	// to provision the per-namespace AuthorizationPolicy and APIKey CR
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

		Eventually(func() ([]deploy.AuthHeader, error) {
			utils.ForgetNamespaceAuthHeaders(caseNamespace)
			return utils.NamespaceAuthHeaders(ctx, caseNamespace)
		}, 60*time.Second, 2*time.Second).ShouldNot(BeEmpty(),
			"backend should publish an API key for %s", caseNamespace)
		// NOTE: assign to the outer authHeaders, do NOT use `:=` here — a
		// shadowed variable leaves the outer one empty and every "valid key"
		// spec then sends an unauthenticated request and fails with 401.
		var err error
		authHeaders, err = utils.NamespaceAuthHeaders(ctx, caseNamespace)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("Uninstalling auth model deployment (removes AuthorizationPolicy, APIKey CR)")
		UninstallCase(CaseAuth)
	})

	It("should reject requests without an Authorization header (401)", func() {
		Eventually(func() int {
			// Deliberately unauthenticated: attaching the valid key here would
			// return 200 and mask a policy that is not being enforced.
			resp, err := utils.SendChat(caseGateway, modelName)
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
			utils.WithHeader("Authorization", "Bearer invalid-key-12345"))
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized),
			fmt.Sprintf("invalid key should be rejected; got status %d", resp.StatusCode))
	})

	// One spec per header the backend publishes: a gateway that accepts the
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
	Context("Model discovery is gated by ext_authz", func() {
		modelsPaths := func() []string {
			return []string{utils.ModelsPath, utils.ModelRetrievePath(modelName)}
		}

		It("should reject discovery requests without an Authorization header (401)", func() {
			for _, path := range modelsPaths() {
				Eventually(func() int {
					resp, err := utils.SendModels(caseGateway, path)
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
					utils.WithHeader("Authorization", "Bearer invalid-key-12345"))
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized),
					"invalid key should be rejected on %s", path)
			}
		})

		It("should list and retrieve the namespace's models with a valid API key (200)", func() {
			Eventually(func() error {
				resp, err := utils.SendModels(caseGateway, utils.ModelsPath,
					utils.WithAuth(authHeaders[0]))
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

			resp, err := utils.SendModels(caseGateway, utils.ModelRetrievePath(modelName),
				utils.WithAuth(authHeaders[0]))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			model, err := utils.ParseModel(resp)
			Expect(err).NotTo(HaveOccurred())
			Expect(model.ID).To(Equal(modelName))
		})
	})
})
