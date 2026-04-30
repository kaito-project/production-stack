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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
		apiKey      string
		caseAuthURL string
	)

	BeforeAll(func() {
		ctx = context.Background()
		caseAuthURL = InstallCase(CaseAuth)

		Eventually(func() (string, error) {
			return utils.GetAPIKeyFromSecret(ctx, caseNamespace)
		}, 60*time.Second, 2*time.Second).ShouldNot(BeEmpty(),
			"API key Secret should be created in %s", caseNamespace)
		// NOTE: assign to the outer apiKey, do NOT use `:=` here.
		// Previously `apiKey, err := ...` shadowed the closure variable
		// and left the outer `apiKey` empty, which caused the
		// "valid API key (200)" spec to send `Authorization: Bearer `
		// (no token) and fail with 401.
		var err error
		apiKey, err = utils.GetAPIKeyFromSecret(ctx, caseNamespace)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("Uninstalling auth model deployment (removes AuthorizationPolicy, APIKey CR)")
		UninstallCase(CaseAuth)
	})

	// hostHeader returns the Host header value that maps to the deployment
	// namespace for the apikey-authz namespace resolution (subdomain = namespace).
	hostHeader := func() string {
		return caseNamespace + ".gw.example.com"
	}

	It("should reject requests without an Authorization header (401)", func() {
		Eventually(func() int {
			// Pass empty bearer token so SendChatCompletionWithAuth omits
			// the Authorization header entirely (see http.go: bearerToken
			// is only set when non-empty). Using sendChat() here would
			// attach the valid API key and the request would succeed with
			// 200, masking the policy-not-enforcing failure mode.
			resp, err := utils.SendChatCompletionWithAuth(
				caseAuthURL, modelName, "hello", "", hostHeader())
			if err != nil {
				return 0 // treat request errors as non-401 responses to keep retrying
			}

			defer resp.Body.Close()

			return resp.StatusCode

		}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusUnauthorized),
			"request without auth should be rejected with 401")
	})

	It("should reject requests with an invalid API key (401)", func() {
		resp, err := utils.SendChatCompletionWithAuth(
			caseAuthURL, modelName, "hello", "invalid-key-12345", hostHeader())
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized),
			fmt.Sprintf("invalid key should be rejected; got status %d", resp.StatusCode))
	})

	It("should accept requests with a valid API key (200)", func() {
		Eventually(func() error {
			resp, err := utils.SendChatCompletionWithAuth(
				caseAuthURL, modelName, "hello", apiKey, hostHeader())
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
			"request with valid API key should succeed with 200")
	})
})
