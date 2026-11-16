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

// Gateway without a model tests.
//
// What this verifies (and why):
//
//	Istio adds the InferencePool (EPP) ext_proc filter only to a Gateway that
//	has an InferencePool attached, so a namespace whose modelharness is
//	installed but that has no modeldeployment yet (or no longer) runs
//	without it. BBR must still be in that Gateway's filter chain:
//	charts/modelharness/templates/envoyfilter-bbr.yaml inserts it after the
//	ext_authz filter rather than anchoring it on the EPP filter. Without BBR
//	no request carries X-Gateway-Model-Name, so a request that names a
//	model falls through to the header-absent catch-all as
//	400 invalid_request_body instead of 404 model_not_found.
//
//	The suite installs only the harness, asserts from the Gateway's
//	/config_dump that it really is in that state (BBR present, EPP ext_proc
//	absent), and then checks both catch-all responses.
//
// Upstream provider only: on an App Routing cluster the Gateway hostname is
// published to DNS from the namespace's HTTPRoutes, which only a
// modeldeployment renders, so a harness without a model has no resolvable
// hostname. The upstream provider reaches the Gateway through a port-forward.
//
// Not Serial: it only touches its own namespace.
var _ = Describe("Gateway without a model", Ordered, utils.GinkgoLabelRouting, utils.GinkgoLabelNoGPU, func() {
	var (
		ctx     context.Context
		ns      string
		gateway deploy.GatewayEndpoint
	)

	BeforeAll(func() {
		if utils.IsAzureProvider() {
			Skip("an App Routing Gateway hostname is published only from a modeldeployment's HTTPRoute, " +
				"so a namespace without a model is unreachable")
		}
		ctx = context.Background()
		ns = utils.UniqueName("e2e-no-model")
		DeferCleanup(func() {
			Expect(utils.DeleteNamespace(context.Background(), ns)).To(Succeed(),
				"failed to delete namespace %s", ns)
		})
		Expect(utils.EnsureNamespace(ctx, ns)).To(Succeed(), "failed to install the modelharness in %s", ns)

		var err error
		gateway, err = utils.OpenGateway(ctx, ns, ns)
		Expect(err).NotTo(HaveOccurred(), "failed to open the gateway in %s", ns)
	})

	It("keeps BBR after ext_authz although the Gateway has no InferencePool ext_proc", func() {
		Eventually(func(g Gomega) {
			pod, err := firstRunningPod(ctx, ns, "gateway.networking.k8s.io/gateway-name="+ns)
			g.Expect(err).NotTo(HaveOccurred(), "the Gateway pod should be Running")
			dump, err := kubectlExec(ns, pod, "pilot-agent", "request", "GET", "/config_dump")
			g.Expect(err).NotTo(HaveOccurred(), "failed to read Envoy admin /config_dump from %s/%s", ns, pod)

			filters := extractGatewayHTTPFilterNames(dump)
			g.Expect(filters).NotTo(BeEmpty(),
				"could not parse the Gateway HCM http_filters out of /config_dump (first 2k bytes: %s)",
				truncate(dump, 2000))
			// The precondition of the scenario: no InferencePool, so Istio
			// has not added the EPP ext_proc filter.
			g.Expect(filters).NotTo(ContainElement("envoy.filters.http.ext_proc"),
				"a Gateway without an InferencePool must not carry the EPP ext_proc filter; got %v", filters)

			authIdx, bbrIdx := -1, -1
			for i, f := range filters {
				if authIdx < 0 && isExtAuthzFilter(f) {
					authIdx = i
				}
				if f == "envoy.filters.http.ext_proc.bbr" {
					bbrIdx = i
				}
			}
			g.Expect(bbrIdx).To(BeNumerically(">=", 0), "BBR must be in the filter chain; got %v", filters)
			g.Expect(authIdx).To(BeNumerically(">=", 0), "ext_authz must be in the filter chain; got %v", filters)
			g.Expect(authIdx).To(BeNumerically("<", bbrIdx), "ext_authz must precede BBR; got %v", filters)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("answers a request that names a model with 404 model_not_found", func() {
		Eventually(func(g Gomega) {
			resp, err := utils.SendChat(gateway, "e2e-no-such-model")
			g.Expect(err).NotTo(HaveOccurred(), "request to gateway failed")
			g.Expect(resp.StatusCode).To(Equal(http.StatusNotFound),
				"BBR must inject the model header, so the request reaches the present-but-unmatched catch-all")
			g.Expect(resp.Header.Get("x-kaito-error-source")).To(Equal("gateway"))
			parsed, err := utils.ParseErrorResponse(resp)
			g.Expect(err).NotTo(HaveOccurred(), "response body should be a JSON error envelope")
			g.Expect(parsed.ErrorCode()).To(Equal("model_not_found"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("answers a request without a model with 400 invalid_request_body", func() {
		resp, err := utils.SendGatewayRequest(ctx, gateway, http.MethodPost, utils.ChatCompletionsPath,
			[]byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			utils.WithHeader("Content-Type", "application/json"))
		Expect(err).NotTo(HaveOccurred(), "request to gateway failed")
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(resp.Header.Get("x-kaito-error-source")).To(Equal("bbr"))
		parsed, err := utils.ParseErrorResponse(resp)
		Expect(err).NotTo(HaveOccurred(), "response body should be a JSON error envelope")
		Expect(parsed.ErrorCode()).To(Equal("invalid_request_body"))
	})
})
