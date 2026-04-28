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
	// Use the first model from CaseGPUMocker — it's deployed by BeforeSuite.
	modelName := CaseDeployments[CaseGPUMocker][0].Name

	var (
		ctx       context.Context
		apiKey    string
		namespace string
	)

	BeforeAll(func() {
		ctx = context.Background()
		utils.GetClusterClient(utils.TestingCluster)

		namespace = generateNamespace("e2e-apikey")
		By(fmt.Sprintf("Creating test namespace %s", namespace))
		createNamespace(ctx, namespace)

		By(fmt.Sprintf("Creating APIKey CR in namespace %s", namespace))
		err := utils.CreateAPIKeyResource(ctx, utils.TestingCluster.KubeClient, namespace)
		Expect(err).NotTo(HaveOccurred(), "failed to create APIKey CR")

		By("Waiting for apikey-operator to generate the API key Secret")
		Eventually(func() (string, error) {
			return utils.GetAPIKeyFromSecret(ctx, namespace)
		}, 60*time.Second, 2*time.Second).ShouldNot(BeEmpty(), "API key Secret should be created")

		key, err := utils.GetAPIKeyFromSecret(ctx, namespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(key).NotTo(BeEmpty())
		apiKey = key
		GinkgoWriter.Printf("API key obtained (length=%d)\n", len(apiKey))

		By("Creating AuthorizationPolicy to enforce ext_authz on the gateway")
		err = utils.CreateAuthorizationPolicy(ctx, utils.TestingCluster.KubeClient, utils.GatewayNamespace)
		Expect(err).NotTo(HaveOccurred(), "failed to create AuthorizationPolicy")

		// Give Envoy a moment to pick up the new policy.
		time.Sleep(5 * time.Second)
	})

	AfterAll(func() {
		By("Cleaning up AuthorizationPolicy")
		_ = utils.DeleteAuthorizationPolicy(ctx, utils.TestingCluster.KubeClient, utils.GatewayNamespace)

		By("Cleaning up APIKey CR and namespace")
		_ = utils.DeleteAPIKeyResource(ctx, utils.TestingCluster.KubeClient, namespace)
		deleteNamespace(ctx, namespace)
	})

	// hostHeader returns the Host header value that maps to the test namespace
	// for the apikey-authz namespace resolution (subdomain = namespace).
	hostHeader := func() string {
		return namespace + ".gw.example.com"
	}

	It("should reject requests without an Authorization header (401)", func() {
		Eventually(func() int {
			resp, err := utils.SendChatCompletionWithAuth(
				gatewayURL, modelName, "hello", "", hostHeader())
			if err != nil {
				GinkgoWriter.Printf("request error: %v\n", err)
				return 0
			}
			defer resp.Body.Close()
			return resp.StatusCode
		}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusUnauthorized),
			"request without auth should be rejected with 401")
	})

	It("should reject requests with an invalid API key (401)", func() {
		resp, err := utils.SendChatCompletionWithAuth(
			gatewayURL, modelName, "hello", "invalid-key-12345", hostHeader())
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized),
			fmt.Sprintf("invalid key should be rejected; got status %d", resp.StatusCode))
	})

	It("should accept requests with a valid API key (200)", func() {
		Eventually(func() error {
			resp, err := utils.SendChatCompletionWithAuth(
				gatewayURL, modelName, "hello", apiKey, hostHeader())
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
