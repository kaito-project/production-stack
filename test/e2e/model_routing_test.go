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
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Model-based routing tests verify the full BBR → HTTPRoute → EPP → inference
// pod request chain by sending requests and asserting the response proves
// correct model-level routing.
//
// Validation approach:
//   - Send POST /v1/chat/completions with {"model": "<model-name>", ...}
//   - Check the response JSON "model" field matches the requested model name
//   - Scrape per-pod vllm:request_success_total{model_name} from each shadow
//     pod to confirm only pods in the correct pool received the request
//   - Verify EPP scheduling metrics corroborate the routing decisions
//
// Prerequisites (deployed on the test cluster):
//   - Istio Gateway with Body-Based Routing (BBR) configured
//   - At least two KAITO InferenceSets serving different models
//   - GPU node mocker creating shadow pods with llm-d-inference-sim
//   - Catch-all `model-not-found-direct` EnvoyFilter (Envoy
//     direct_response, rendered per-namespace by charts/modelharness)

var _ = Describe("Model-Based Routing", Ordered, utils.GinkgoLabelRouting, func() {
	// Per-case deployments owned by model_routing_test.go (see cases.go).
	// Installed in a dedicated namespace by BeforeAll so this case can run
	// in parallel with other Ordered Describes.
	caseDeployments := CaseDeployments[CaseModelRouting]
	caseNamespace := CaseNamespace(CaseModelRouting)
	falconModel := caseDeployments[0].Name
	ministralModel := caseDeployments[1].Name
	modelNames := []string{falconModel, ministralModel}

	// otherModelName returns the model name in modelNames that is not `model`.
	otherModelName := func(model string) string {
		for _, m := range modelNames {
			if m != model {
				return m
			}
		}
		return ""
	}

	var ctx context.Context

	// caseGatewayURL routes to this case's dedicated Gateway. Resolved
	// in BeforeAll. Per-namespace Gateway + catch-all
	// `model-not-found-direct` EnvoyFilter are provisioned by the
	// modelharness chart.
	var caseGatewayURL string

	BeforeAll(func() {
		ctx = context.Background()
		caseGatewayURL = InstallCase(CaseModelRouting)
	})

	// sendChat / sendChatWithRetry / sendChatWithPrompt forward to the
	// non-auth helpers — the routing case no longer enables the API-key
	// AuthorizationPolicy (see cases.go).
	sendChat := func(url, model string) (*http.Response, error) {
		return utils.SendChatCompletion(url, model)
	}
	sendChatWithPrompt := func(url, model, prompt string) (*http.Response, error) {
		return utils.SendChatCompletionWithPrompt(url, model, prompt)
	}
	sendChatWithRetry := func(url, model string) (*http.Response, error) {
		return utils.SendChatCompletionWithRetry(url, model)
	}

	AfterAll(func() {
		UninstallCase(CaseModelRouting)
	})

	Context("Single model request", func() {
		It("should return the correct model name for falcon", func() {
			var parsed *utils.ChatCompletionResponse
			Eventually(func() error {
				resp, err := sendChat(caseGatewayURL, falconModel)
				if err != nil {
					return err
				}
				if resp.StatusCode != http.StatusOK {
					body, _ := utils.ReadResponseBody(resp)
					return fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(body))
				}
				p, err := utils.ParseChatCompletionResponse(resp)
				if err != nil {
					return err
				}
				parsed = p
				return nil
			}, 30*time.Second, 2*time.Second).Should(Succeed())
			Expect(parsed.Model).To(Equal(falconModel),
				"response model should match the requested falcon model")
		})

		It("should return the correct model name for ministral", func() {
			var parsed *utils.ChatCompletionResponse
			Eventually(func() error {
				resp, err := sendChat(caseGatewayURL, ministralModel)
				if err != nil {
					return err
				}
				if resp.StatusCode != http.StatusOK {
					body, _ := utils.ReadResponseBody(resp)
					return fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(body))
				}
				p, err := utils.ParseChatCompletionResponse(resp)
				if err != nil {
					return err
				}
				parsed = p
				return nil
			}, 30*time.Second, 2*time.Second).Should(Succeed())
			Expect(parsed.Model).To(Equal(ministralModel),
				"response model should match the requested ministral model")
		})
	})

	Context("Cross-model isolation (serial)", func() {
		const numRequests = 5

		It("should route requests to the correct model pool with no cross-contamination", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			for _, model := range modelNames {
				otherModel := otherModelName(model)

				By(fmt.Sprintf("snapshotting metrics before sending %d requests to %s", numRequests, model))
				beforeModel, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
				Expect(err).NotTo(HaveOccurred())
				beforeOther, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, otherModel)
				Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("sending %d requests to %s", numRequests, model))
				for i := 0; i < numRequests; i++ {
					resp, err := sendChatWithRetry(caseGatewayURL, model)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(http.StatusOK))

					parsed, err := utils.ParseChatCompletionResponse(resp)
					Expect(err).NotTo(HaveOccurred())
					Expect(parsed.Model).To(Equal(model))
				}

				By(fmt.Sprintf("verifying only %s pods received traffic", model))
				afterModel, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
				Expect(err).NotTo(HaveOccurred())
				afterOther, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, otherModel)
				Expect(err).NotTo(HaveOccurred())

				modelDiff := utils.DiffSnapshots(beforeModel, afterModel)
				otherDiff := utils.DiffSnapshots(beforeOther, afterOther)

				Expect(utils.TotalDelta(modelDiff)).To(BeNumerically(">=", float64(numRequests)),
					"%s pods should have received at least %d requests", model, numRequests)
				Expect(utils.TotalDelta(otherDiff)).To(BeNumerically("==", 0),
					"%s pods should have received 0 requests during %s traffic", otherModel, model)
			}
		})
	})

	Context("Cross-model isolation (concurrent)", func() {
		const numPerModel = 20

		It("should maintain isolation under interleaved concurrent traffic", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			By("snapshotting metrics before concurrent burst")
			beforeFalcon, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, falconModel)
			Expect(err).NotTo(HaveOccurred())
			beforeMinistral, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, ministralModel)
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("launching %d concurrent requests per model", numPerModel))
			type result struct {
				model      string
				statusCode int
				respModel  string
				err        error
			}
			results := make([]result, 2*numPerModel)
			var wg sync.WaitGroup
			wg.Add(2 * numPerModel)

			for i := 0; i < numPerModel; i++ {
				// Interleave falcon and ministral requests.
				for j, model := range modelNames {
					idx := i*2 + j
					model := model
					go func(idx int) {
						defer wg.Done()
						defer GinkgoRecover()
						resp, err := sendChatWithRetry(caseGatewayURL, model)
						if err != nil {
							results[idx] = result{model: model, err: err}
							return
						}
						parsed, parseErr := utils.ParseChatCompletionResponse(resp)
						if parseErr != nil {
							results[idx] = result{model: model, statusCode: resp.StatusCode, err: parseErr}
							return
						}
						results[idx] = result{model: model, statusCode: resp.StatusCode, respModel: parsed.Model}
					}(idx)
				}
			}
			wg.Wait()

			By("verifying all responses returned correct model names")
			for i, r := range results {
				Expect(r.err).NotTo(HaveOccurred(), "request %d for %s failed", i, r.model)
				Expect(r.statusCode).To(Equal(http.StatusOK), "request %d for %s", i, r.model)
				Expect(r.respModel).To(Equal(r.model),
					"request %d: response model %q should match requested %q", i, r.respModel, r.model)
			}

			By("verifying per-pod metrics show zero cross-contamination")
			afterFalcon, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, falconModel)
			Expect(err).NotTo(HaveOccurred())
			afterMinistral, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, ministralModel)
			Expect(err).NotTo(HaveOccurred())

			falconDiff := utils.DiffSnapshots(beforeFalcon, afterFalcon)
			ministralDiff := utils.DiffSnapshots(beforeMinistral, afterMinistral)

			Expect(utils.TotalDelta(falconDiff)).To(BeNumerically(">=", float64(numPerModel)),
				"falcon pods should have received at least %d requests", numPerModel)
			Expect(utils.TotalDelta(ministralDiff)).To(BeNumerically(">=", float64(numPerModel)),
				"ministral pods should have received at least %d requests", numPerModel)
		})
	})

	Context("Model-specific route wins over catch-all", func() {
		// The catch-all is now an Envoy `direct_response` (status 404)
		// patched onto the Gateway by the `model-not-found-direct`
		// EnvoyFilter (charts/modelharness/templates/envoyfilter-not-found.yaml).
		// If the catch-all ever hijacked a known-model request, the
		// response would be 404 instead of 200, and the model field in
		// the body would be missing. Assert both directly.
		It("should not absorb known model requests into the catch-all 404", func() {
			By("sending requests to known models")
			for _, model := range modelNames {
				for i := 0; i < 3; i++ {
					resp, err := sendChat(caseGatewayURL, model)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(http.StatusOK),
						"known-model request to %s should not fall through to catch-all 404", model)
					parsed, perr := utils.ParseChatCompletionResponse(resp)
					Expect(perr).NotTo(HaveOccurred())
					Expect(parsed.Model).To(Equal(model),
						"response for %s should echo the requested model name", model)
				}
			}
		})
	})

	Context("EPP routing success (metrics)", func() {
		const numRequests = 5

		It("should show EPP scheduler success counts matching requests sent", func() {
			// TODO: EPP pods have Istio sidecars that intercept traffic on port 9090,
			// causing 401 Unauthorized when accessing via K8s API pod proxy.
			// Re-enable once EPP metrics port is excluded from Istio interception
			// (e.g., via traffic.sidecar.istio.io/excludeInboundPorts annotation)
			// or when we switch to scraping via the EPP Service instead of pod proxy.
			Skip("EPP metrics port is intercepted by Istio sidecar (401 Unauthorized via pod proxy)")
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			By("recording EPP metrics before sending requests")
			// Use the first model's EPP — EPP is per-pool, so we check each.
			beforeSuccess := make(map[string]float64)
			beforeFailure := make(map[string]float64)
			beforeObjective := make(map[string]float64)
			for _, model := range modelNames {
				s, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
					"inference_extension_scheduler_attempts_total", map[string]string{"status": "success"})
				Expect(err).NotTo(HaveOccurred())
				beforeSuccess[model] = s

				f, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
					"inference_extension_scheduler_attempts_total", map[string]string{"status": "failure"})
				Expect(err).NotTo(HaveOccurred())
				beforeFailure[model] = f

				o, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
					"inference_objective_request_total", map[string]string{"model_name": model})
				Expect(err).NotTo(HaveOccurred())
				beforeObjective[model] = o
			}

			By(fmt.Sprintf("sending %d requests per model", numRequests))
			for _, model := range modelNames {
				for i := 0; i < numRequests; i++ {
					resp, err := sendChat(caseGatewayURL, model)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(http.StatusOK))
					resp.Body.Close()
				}
			}

			By("verifying EPP scheduler success metrics increased")
			for _, model := range modelNames {
				afterSuccess, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
					"inference_extension_scheduler_attempts_total", map[string]string{"status": "success"})
				Expect(err).NotTo(HaveOccurred())
				Expect(afterSuccess-beforeSuccess[model]).To(BeNumerically(">=", float64(numRequests)),
					"EPP scheduler success count for %s should have increased by at least %d", model, numRequests)

				afterFailure, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
					"inference_extension_scheduler_attempts_total", map[string]string{"status": "failure"})
				Expect(err).NotTo(HaveOccurred())
				Expect(afterFailure).To(Equal(beforeFailure[model]),
					"EPP scheduler failure count for %s should not have changed", model)

				afterObjective, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
					"inference_objective_request_total", map[string]string{"model_name": model})
				Expect(err).NotTo(HaveOccurred())
				Expect(afterObjective-beforeObjective[model]).To(BeNumerically(">=", float64(numRequests)),
					"EPP objective request count for %s should have increased by at least %d", model, numRequests)
			}
		})
	})

	Context("Load distribution", func() {
		const numRequests = 25
		const maxTrafficFraction = 0.80

		It("should distribute traffic across replicas with no pod receiving zero or >80% of requests", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			for _, model := range modelNames {
				By(fmt.Sprintf("snapshotting metrics before sending %d requests to %s", numRequests, model))
				before, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(before)).To(BeNumerically(">=", 2),
					"%s should have at least 2 shadow pods for load distribution test", model)

				By(fmt.Sprintf("sending %d requests to %s", numRequests, model))
				for i := 0; i < numRequests; i++ {
					// Use a unique prompt per request so the EPP's
					// prefix-cache-scorer does not route all requests to
					// the same pod. Load distribution is meaningful only
					// when requests carry distinct prefixes.
					prompt := fmt.Sprintf("load distribution request %d: what is the capital of France?", i)
					resp, err := sendChatWithPrompt(caseGatewayURL, model, prompt)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(http.StatusOK))
					resp.Body.Close()
				}

				By(fmt.Sprintf("verifying load distribution for %s", model))
				after, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
				Expect(err).NotTo(HaveOccurred())

				diff := utils.DiffSnapshots(before, after)
				total := utils.TotalDelta(diff)
				Expect(total).To(BeNumerically(">=", float64(numRequests)),
					"%s total requests served should be at least %d", model, numRequests)

				// Count how many pods received at least 1 request.
				var activePods int
				for _, delta := range diff {
					if delta > 0 {
						activePods++
					}
				}
				Expect(activePods).To(BeNumerically(">=", 2),
					"%s should have distributed traffic to at least 2 pods", model)

				for pod, delta := range diff {
					if delta == 0 {
						continue
					}
					fraction := delta / total
					Expect(fraction).To(BeNumerically("<=", maxTrafficFraction),
						"%s pod %s received %.0f%% of traffic (%.0f/%.0f), exceeding %d%% threshold",
						model, pod, fraction*100, delta, total, int(maxTrafficFraction*100))
				}
			}
		})
	})

	Context("Debug EnvoyFilter log chain", func() {
		It("should emit PRE-BBR, POST-EPP, and RESPONSE log lines for inference requests", func() {
			// The inference-debug-filter EnvoyFilter only applies to
			// workloads in its own namespace (`default`), so it does not
			// observe traffic on per-case Gateways. The case's
			// HTTPRoutes are not parented to the default Gateway either,
			// so this assertion can no longer be exercised end-to-end
			// with the per-case dataplane isolation introduced in this
			// suite. Re-enable once the debug filter is moved into
			// Istio's rootNamespace (or replicated per-namespace).
			Skip("debug filter is namespace-scoped to `default`; per-case gateways do not see it")

			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			// Identify the Gateway pod created by the Kubernetes Gateway API.
			gwPods, err := clientset.CoreV1().Pods(caseNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "gateway.networking.k8s.io/gateway-name=inference-gateway",
				FieldSelector: "status.phase=Running",
			})
			if err != nil || len(gwPods.Items) == 0 {
				// Fallback: try istio-system with classic label.
				gwPods, err = clientset.CoreV1().Pods("istio-system").List(ctx, metav1.ListOptions{
					LabelSelector: "istio=ingressgateway",
					FieldSelector: "status.phase=Running",
				})
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(gwPods.Items).NotTo(BeEmpty(), "istio-ingressgateway pods should be running")
			gwPod := gwPods.Items[0]

			for _, model := range modelNames {
				By(fmt.Sprintf("sending a request for %s and checking debug filter logs", model))

				resp, err := sendChat(caseGatewayURL, model)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK))
				resp.Body.Close()

				// Allow a brief window for logs to flush.
				time.Sleep(2 * time.Second)

				// Tail the ingressgateway logs.
				logs, err := utils.GetPodLogs(clientset, gwPod.Namespace, gwPod.Name, "istio-proxy")
				Expect(err).NotTo(HaveOccurred())

				// Find a request-id that has all three debug log lines.
				preBBR, postEPP, response := findDebugLogTriple(logs, model)
				Expect(preBBR).NotTo(BeEmpty(),
					"should find [PRE-BBR] log line for %s in ingressgateway logs", model)
				Expect(postEPP).NotTo(BeEmpty(),
					"should find [POST-EPP] log line for %s in ingressgateway logs", model)
				Expect(response).NotTo(BeEmpty(),
					"should find [RESPONSE] log line for %s in ingressgateway logs", model)

				// Verify POST-EPP line contains expected headers.
				Expect(postEPP).To(ContainSubstring("x-gateway-model-name"),
					"[POST-EPP] should log x-gateway-model-name for %s", model)
				Expect(postEPP).To(ContainSubstring(model),
					"[POST-EPP] x-gateway-model-name should contain %s", model)
				Expect(postEPP).To(ContainSubstring("x-gateway-destination-endpoint"),
					"[POST-EPP] should log x-gateway-destination-endpoint for %s", model)
			}
		})
	})

	Context("Unknown model / malformed request handling", func() {
		// Note: basic 404 for unknown model is already tested in gpu_mocker_test.go.
		// These tests cover additional negative-path scenarios.

		It("should return 404 for a request with missing model field", func() {
			// Refresh port-forwards before issuing a raw HTTP request so a
			// kubectl port-forward channel that died between specs gets
			// rebound; SendChatCompletion* does this internally.
			Expect(utils.EnsurePortForwards()).To(Succeed())
			gatewayURL := utils.ResolveGatewayURL(caseGatewayURL)

			client := &http.Client{Timeout: utils.HTTPTimeout}
			body := []byte(`{"messages": [{"role": "user", "content": "hello"}]}`)
			req, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/chat/completions",
				bytes.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			// Without a model field, BBR cannot inject x-gateway-model-name,
			// so the request should fall through to the catch-all 404.
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound),
				"missing model field should result in 404")
			errResp, err := utils.ParseErrorResponse(resp)
			Expect(err).NotTo(HaveOccurred())
			Expect(errResp.ErrorCode()).To(Equal("model_not_found"))
		})

		It("should return a well-formed error for non-string model field", func() {
			Expect(utils.EnsurePortForwards()).To(Succeed())
			gatewayURL := utils.ResolveGatewayURL(caseGatewayURL)

			client := &http.Client{Timeout: utils.HTTPTimeout}
			body := []byte(`{"model": 42, "messages": [{"role": "user", "content": "hello"}]}`)
			req, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/chat/completions",
				bytes.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			// BBR should reject or fall through to catch-all; ideally 4xx not 5xx.
			// TODO: BBR currently returns 500 for non-string model fields instead
			// of falling through to the catch-all 404. Tighten to < 500 once
			// BBR handles non-string model values gracefully.
			Expect(resp.StatusCode).To(BeNumerically(">=", 400))
			Expect(resp.StatusCode).To(BeNumerically("<=", 500),
				"non-string model field should not cause an unrecoverable error")

			// Verify subsequent valid requests still succeed (BBR didn't crash permanently).
			Eventually(func() int {
				r, err := sendChat(caseGatewayURL, falconModel)
				if err != nil {
					return 0
				}
				defer r.Body.Close()
				return r.StatusCode
			}, 30*time.Second, 2*time.Second).Should(Equal(http.StatusOK),
				"valid requests should succeed after non-string model field request")
		})

		It("should return a well-formed error for non-JSON body", func() {
			Expect(utils.EnsurePortForwards()).To(Succeed())
			gatewayURL := utils.ResolveGatewayURL(caseGatewayURL)

			client := &http.Client{Timeout: utils.HTTPTimeout}
			body := []byte(`this is not json`)
			req, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/chat/completions",
				bytes.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "text/plain")

			resp, err := client.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			// BBR should reject or fall through to catch-all; ideally 4xx not 5xx.
			Expect(resp.StatusCode).To(BeNumerically(">=", 400))
			Expect(resp.StatusCode).To(BeNumerically("<=", 500),
				"non-JSON body should not cause an unrecoverable error")

			// Verify subsequent valid requests still succeed (BBR didn't crash).
			Eventually(func() int {
				r, err := sendChat(caseGatewayURL, falconModel)
				if err != nil {
					return 0
				}
				defer r.Body.Close()
				return r.StatusCode
			}, 30*time.Second, 2*time.Second).Should(Equal(http.StatusOK),
				"valid requests should succeed after non-JSON body request")
		})

		It("should not inject x-gateway-model-name for non-/v1/ paths", func() {
			Expect(utils.EnsurePortForwards()).To(Succeed())
			gatewayURL := utils.ResolveGatewayURL(caseGatewayURL)

			client := &http.Client{Timeout: utils.HTTPTimeout}

			// GET /healthz — should bypass BBR entirely.
			req, err := http.NewRequest(http.MethodGet, gatewayURL+"/healthz", nil)
			Expect(err).NotTo(HaveOccurred())

			resp, err := client.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			// The response should not be treated as an inference request.
			// We just verify it doesn't crash and returns something.
			Expect(resp.StatusCode).To(BeNumerically("<", 500),
				"non-/v1/ path should not cause a 5xx error")
		})

		It("should passthrough backend 4xx when prompt exceeds max context length", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			model := falconModel

			// Snapshot vllm:request_success_total before the oversized request.
			beforeSuccess, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())

			// Build a prompt that exceeds the simulator's max-model-len (32768).
			// Each "word " is roughly 1 token, so 50000 repetitions should
			// comfortably exceed the limit.
			longPrompt := strings.Repeat("word ", 50000)

			// Send the oversized request and verify HTTP 400 with an
			// OpenAI-compatible JSON error body from vLLM.
			resp, err := sendChatWithPrompt(caseGatewayURL, model, longPrompt)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest),
				"prompt exceeding max context length should return 400")
			errResp, err := utils.ParseErrorResponse(resp)
			Expect(err).NotTo(HaveOccurred(),
				"response should be a valid OpenAI-compatible JSON error, not Envoy's error page")
			Expect(errResp.Error.Message).NotTo(BeEmpty(),
				"error response should contain a non-empty message from vLLM")

			// Verify vllm:request_success_total did NOT increment — the
			// request was rejected, not completed.
			afterSuccess, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())
			successDiff := utils.DiffSnapshots(beforeSuccess, afterSuccess)
			Expect(utils.TotalDelta(successDiff)).To(BeNumerically("==", 0),
				"vllm:request_success_total should not increment for a rejected request")

			// Verify subsequent valid requests still succeed — the rejection
			// did not wedge the connection or the ext_proc filter chain.
			Eventually(func() error {
				r, err := sendChat(caseGatewayURL, model)
				if err != nil {
					return err
				}
				defer r.Body.Close()
				if r.StatusCode != http.StatusOK {
					body, _ := utils.ReadResponseBody(r)
					return fmt.Errorf("expected 200, got %d: %s", r.StatusCode, string(body))
				}
				return nil
			}, 30*time.Second, 2*time.Second).Should(Succeed(),
				"valid requests should succeed after an oversized-prompt rejection")
		})
	})
})

// findDebugLogTriple searches istio-ingressgateway logs for a request-id that
// has [PRE-BBR], [POST-EPP], and [RESPONSE] lines, where the POST-EPP line
// contains the expected model name. Returns the three log lines.
func findDebugLogTriple(logs, model string) (preBBR, postEPP, response string) {
	type logEntry struct {
		preBBR       string
		postEPPLines []string
		response     string
	}
	entries := make(map[string]*logEntry)

	for _, line := range strings.Split(logs, "\n") {
		for _, tag := range []string{"[PRE-BBR]", "[POST-EPP]", "[RESPONSE]"} {
			idx := strings.Index(line, tag)
			if idx < 0 {
				continue
			}
			// Extract request-id: tag is followed by [<request-id>]
			rest := line[idx+len(tag):]
			if len(rest) < 2 || rest[0] != '[' {
				continue
			}
			endBracket := strings.Index(rest, "]")
			if endBracket < 0 {
				continue
			}
			reqID := rest[1:endBracket]
			if reqID == "" || reqID == "unknown" {
				continue
			}

			e, ok := entries[reqID]
			if !ok {
				e = &logEntry{}
				entries[reqID] = e
			}
			switch tag {
			case "[PRE-BBR]":
				e.preBBR = line
			case "[POST-EPP]":
				e.postEPPLines = append(e.postEPPLines, line)
			case "[RESPONSE]":
				e.response = line
			}
		}
	}

	// Find a complete triple where POST-EPP mentions the model.
	// The Lua filter emits multiple [POST-EPP] lines per request (one per header),
	// so join them for assertion.
	for _, e := range entries {
		if e.preBBR != "" && len(e.postEPPLines) > 0 && e.response != "" {
			joined := strings.Join(e.postEPPLines, "\n")
			if strings.Contains(joined, model) {
				return e.preBBR, joined, e.response
			}
		}
	}
	return "", "", ""
}
