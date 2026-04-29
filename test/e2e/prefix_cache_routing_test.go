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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Prefix-cache aware routing tests verify that the EPP (Endpoint Picker)
// correctly routes requests with the same prefix to the same backend pod,
// leveraging KV-cache locality for better performance.
//
// Validation approach:
//   - Determine which backend pod served a request by scraping per-pod
//     vllm:request_success_total deltas from shadow pods.
//   - Cross-check with vllm:prefix_cache_hits and vllm:prefix_cache_queries
//     to confirm the simulator's KV cache is active.
//
// Prerequisites (deployed on the test cluster):
//   - Istio Gateway with EPP configured
//   - KAITO InferenceSet with 2+ replicas (shadow pods running llm-d-inference-sim)
//   - llm-d-inference-sim configured with enable-kvcache: true

var _ = Describe("Prefix Cache Aware Routing", Ordered, utils.GinkgoLabelPrefixCache, func() {
	// Per-case deployment owned by prefix_cache_routing_test.go (see cases.go).
	// A single deployment with replicas≥2 is sufficient for prefix-cache tests.
	// Installed in a dedicated namespace by BeforeAll so this case can run in
	// parallel with other Ordered Describes.
	model := CaseDeployments[CasePrefixCache][0].Name
	caseNamespace := CaseNamespace(CasePrefixCache)

	var ctx context.Context
	var caseGatewayURL string

	BeforeAll(func() {
		caseGatewayURL = InstallCase(CasePrefixCache)
	})

	AfterAll(func() {
		UninstallCase(CasePrefixCache)
	})

	BeforeEach(func() {
		ctx = context.Background()
	})

	Context("Same prompt repeated requests", func() {
		const numRequests = 5
		const prompt = "Explain quantum computing in simple terms for a beginner audience"

		It("should route identical requests to the same backend pod", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			By("snapshotting per-pod metrics before sending requests")
			before, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(before)).To(BeNumerically(">=", 2),
				"need at least 2 shadow pods for prefix cache test")

			beforeCacheHits, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
			Expect(err).NotTo(HaveOccurred())
			beforeCacheQueries, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("sending the same prompt %d times", numRequests))
			for i := 0; i < numRequests; i++ {
				resp, err := utils.SendChatCompletionWithPrompt(caseGatewayURL, model, prompt)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK),
					"request %d should succeed", i)
				resp.Body.Close()
			}

			By("verifying all requests were routed to the same pod")
			after, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())

			diff := utils.DiffSnapshots(before, after)
			Expect(utils.TotalDelta(diff)).To(BeNumerically(">=", float64(numRequests)),
				"total requests served should be at least %d", numRequests)

			// Exactly one pod should have received all requests.
			var stickyPod string
			for pod, delta := range diff {
				if delta > 0 {
					Expect(delta).To(BeNumerically("==", float64(numRequests)),
						"pod %s received %.0f requests, expected all %d (prefix cache should make EPP sticky)", pod, delta, numRequests)
					stickyPod = pod
				}
			}
			Expect(stickyPod).NotTo(BeEmpty(), "should have identified the sticky pod")

			By("verifying prefix cache metrics incremented on the sticky pod")
			afterCacheHits, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
			Expect(err).NotTo(HaveOccurred())
			afterCacheQueries, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
			Expect(err).NotTo(HaveOccurred())

			cacheHitsDiff := utils.DiffSnapshots(beforeCacheHits, afterCacheHits)
			cacheQueriesDiff := utils.DiffSnapshots(beforeCacheQueries, afterCacheQueries)

			Expect(cacheQueriesDiff[stickyPod]).To(BeNumerically(">", 0),
				"vllm:prefix_cache_queries should increment on the sticky pod")
			// prefix_cache_hits may stay 0 if the simulator doesn't implement
			// real prefix caching (mode=random). Log for visibility.
			if cacheHitsDiff[stickyPod] == 0 {
				GinkgoWriter.Printf("[INFO] vllm:prefix_cache_hits is 0 on %s \u2014 simulator may not implement real prefix caching\n", stickyPod)
			}
		})
	})

	Context("Different prompt categories sticky routing", func() {
		const numPerCategory = 3
		const promptA = "Explain quantum computing and the principles of superposition and entanglement in detail"
		const promptB = "Write a Python function to sort a list using the merge sort algorithm with detailed comments"

		It("should route each category to a consistent pod", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			By("snapshotting per-pod metrics before category A")
			beforeA, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())

			beforeCacheHits, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("sending category A prompt %d times", numPerCategory))
			for i := 0; i < numPerCategory; i++ {
				resp, err := utils.SendChatCompletionWithPrompt(caseGatewayURL, model, promptA)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK))
				resp.Body.Close()
			}

			afterA, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())
			diffA := utils.DiffSnapshots(beforeA, afterA)

			// Identify which pod got category A.
			var podA string
			for pod, delta := range diffA {
				if delta == float64(numPerCategory) {
					podA = pod
					break
				}
			}
			Expect(podA).NotTo(BeEmpty(),
				"category A should be sticky — one pod should have received all %d requests", numPerCategory)

			By(fmt.Sprintf("sending category B prompt %d times", numPerCategory))
			beforeB, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())

			for i := 0; i < numPerCategory; i++ {
				resp, err := utils.SendChatCompletionWithPrompt(caseGatewayURL, model, promptB)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK))
				resp.Body.Close()
			}

			afterB, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())
			diffB := utils.DiffSnapshots(beforeB, afterB)

			// Identify which pod got category B.
			var podB string
			for pod, delta := range diffB {
				if delta == float64(numPerCategory) {
					podB = pod
					break
				}
			}
			Expect(podB).NotTo(BeEmpty(),
				"category B should be sticky — one pod should have received all %d requests", numPerCategory)

			By("checking prefix cache hits on each sticky pod")
			afterCacheHits, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
			Expect(err).NotTo(HaveOccurred())
			cacheHitsDiff := utils.DiffSnapshots(beforeCacheHits, afterCacheHits)

			// prefix_cache_hits may stay 0 if the simulator doesn't implement
			// real prefix caching. Log for visibility.
			if cacheHitsDiff[podA] == 0 || cacheHitsDiff[podB] == 0 {
				GinkgoWriter.Printf("[INFO] vllm:prefix_cache_hits is 0 \u2014 simulator may not implement real prefix caching\n")
			}
		})
	})

	Context("Pod deletion fallback", utils.GinkgoLabelNightly, func() {
		const prompt = "Explain the theory of relativity and its implications for modern physics"

		It("should re-route to another pod when the sticky pod is deleted", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			By("sending requests to establish a sticky pod")
			before, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())

			for i := 0; i < 3; i++ {
				resp, err := utils.SendChatCompletionWithPrompt(caseGatewayURL, model, prompt)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK))
				resp.Body.Close()
			}

			after, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())
			diff := utils.DiffSnapshots(before, after)

			var stickyPod string
			for pod, delta := range diff {
				if delta >= 3 {
					stickyPod = pod
					break
				}
			}
			Expect(stickyPod).NotTo(BeEmpty(), "should have identified a sticky pod")

			By(fmt.Sprintf("deleting sticky pod %s", stickyPod))
			err = clientset.CoreV1().Pods(caseNamespace).Delete(ctx, stickyPod, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("sending the same prompt again and verifying it succeeds on a different pod")
			// Scrape metrics from the remaining (non-deleted) pods before sending.
			// The deleted pod will not appear in this snapshot.
			before2, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			// ScrapeRequestSuccessTotal may transiently fail while the pod list
			// refreshes after deletion — retry until it succeeds.
			if err != nil {
				Eventually(func() error {
					before2, err = utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
					return err
				}, "30s", "2s").Should(Succeed(),
					"should be able to scrape remaining pods after deletion")
			}

			Eventually(func() error {
				resp, err := utils.SendChatCompletionWithPrompt(caseGatewayURL, model, prompt)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("expected 200, got %d", resp.StatusCode)
				}
				return nil
			}, "3m", "5s").Should(Succeed(),
				"request should succeed after sticky pod deletion")

			after2, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			if err != nil {
				Eventually(func() error {
					after2, err = utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
					return err
				}, "30s", "2s").Should(Succeed())
			}
			diff2 := utils.DiffSnapshots(before2, after2)

			// The deleted pod should not appear in the new snapshot; a different pod served it.
			var servingPod string
			for pod, delta := range diff2 {
				if delta > 0 {
					servingPod = pod
				}
			}
			Expect(servingPod).NotTo(BeEmpty(), "a pod should have served the request")
			Expect(servingPod).NotTo(Equal(stickyPod),
				"request should have been served by a different pod after deletion")
		})
	})
})
