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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// gateway_unavailable tests.
//
// What this verifies (and why):
//
//	Mapper #3 of the per-namespace outage local_reply
//	(charts/modelharness/templates/envoyfilter-outage-reply.yaml) rewrites a
//	local 5xx that carries a router/upstream response flag but NO
//	`X-Gateway-Model-Name` header — a failure before model routing — into a
//	`502 gateway_unavailable` envelope with `x-kaito-error-source: gateway`.
//
//	Scaling the namespace Gateway itself to zero cannot exercise it: no Envoy
//	is left to answer, so the client only sees a connection failure. The
//	model-discovery route is the request path that reaches an upstream
//	without model routing: a bodyless `GET /v1/models` gets no model header
//	from BBR and is routed to the cluster-wide productionstack-status-reporter
//	Service. Scaling the reporter to zero leaves that route with no healthy
//	upstream, so Envoy answers a local 503 with the `UH` flag, which mapper #3
//	turns into `502 gateway_unavailable`.
//
//	The harness is installed without any model deployment, so the spec costs
//	no GPU node.
//
// The reporter Deployment is discovered by its chart label rather than by a
// fixed namespace and name, which depend on the install (the umbrella release
// name and namespace, or a managed control plane's own add-on namespace).
//
// Why Serial: the status reporter is a cluster-wide singleton serving model
// discovery for every namespace and publishing every control-plane Event, so
// this suite must not run concurrently with any other spec.
var _ = Describe("gateway_unavailable (no ready upstream before model routing)",
	Ordered, Serial, utils.GinkgoLabelOutage, utils.GinkgoLabelModelDiscovery, utils.GinkgoLabelNoGPU, func() {

		const reporterSelector = "app.kubernetes.io/name=productionstack-status-reporter"

		var (
			ctx         context.Context
			caseGateway deploy.GatewayEndpoint
			replicas    *utils.DeploymentReplicaGuard
		)

		listModelsStatus := func() (int, error) {
			resp, err := utils.SendModels(caseGateway, utils.ModelsPath)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			return resp.StatusCode, nil
		}

		BeforeAll(func() {
			ctx = context.Background()
			namespace := generateNamespace("e2e-gateway-unavailable")
			Expect(utils.EnsureNamespace(ctx, namespace)).To(Succeed(),
				"failed to install the modelharness in %s", namespace)
			DeferCleanup(func(ctx SpecContext) {
				if err := utils.DeleteNamespace(ctx, namespace); err != nil {
					GinkgoWriter.Printf("Cleanup warning: %v\n", err)
				}
			}, NodeTimeout(5*time.Minute))

			var err error
			caseGateway, err = utils.OpenGateway(ctx, namespace, namespace)
			Expect(err).NotTo(HaveOccurred(), "failed to open the gateway for %s", namespace)

			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())
			reporters, err := clientset.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
				LabelSelector: reporterSelector,
			})
			Expect(err).NotTo(HaveOccurred(), "failed to list Deployments matching %q", reporterSelector)
			Expect(reporters.Items).To(HaveLen(1), "expected exactly one status reporter Deployment matching %q", reporterSelector)
			reporter := reporters.Items[0]
			replicas = prepareDeploymentOutage(ctx, reporter.Namespace, reporter.Name)

			// Sanity: model discovery must succeed BEFORE we induce the
			// outage, otherwise a 502 below would be meaningless.
			Eventually(listModelsStatus, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusOK),
				"baseline model listing should succeed before scaling the status reporter to zero")
		})

		It("maps a route with no ready upstream to 502 gateway_unavailable", func() {
			By("scaling the productionstack-status-reporter Deployment to zero")
			Expect(replicas.ScaleAndWait(ctx, 0, 2*time.Minute)).
				To(Succeed(), "failed to scale the status reporter to zero")

			By("listing models and asserting the gateway_unavailable envelope")
			Eventually(func(g Gomega) {
				resp, sErr := utils.SendModels(caseGateway, utils.ModelsPath)
				g.Expect(sErr).NotTo(HaveOccurred(), "request to gateway failed")
				status := resp.StatusCode
				errSource := resp.Header.Get("x-kaito-error-source")
				parsed, pErr := utils.ParseErrorResponse(resp)
				g.Expect(pErr).NotTo(HaveOccurred(), "response body should be a JSON error envelope")

				g.Expect(status).To(Equal(http.StatusBadGateway),
					"a route with no ready upstream should surface as 502")
				g.Expect(parsed.ErrorCode()).To(Equal("gateway_unavailable"),
					"a route with no ready upstream should carry code gateway_unavailable")
				g.Expect(errSource).To(Equal("gateway"),
					"gateway_unavailable should carry x-kaito-error-source: gateway")
			}, 90*time.Second, 5*time.Second).Should(Succeed())
		})

		It("recovers once the status reporter is restored", func() {
			By("scaling the productionstack-status-reporter Deployment back to its original replica count")
			Expect(replicas.Restore(ctx, 3*time.Minute)).
				To(Succeed(), "failed to restore the status reporter replicas")

			By("listing models and asserting it succeeds again")
			Eventually(listModelsStatus, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusOK),
				"model listing should succeed again once the status reporter is healthy")
		})
	})
