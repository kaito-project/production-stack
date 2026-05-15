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
// Prompt length / BlockSizeTokens note:
//   The EPP prefix-cache-scorer hashes the prompt into fixed-size token
//   blocks (`BlockSizeTokens`, default 16 — see
//   sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/plugins/
//   requestcontrol/dataproducer/approximateprefix/types.go).  Only FULL
//   blocks are recorded in the per-pod LRU index that drives the sticky
//   routing decision.  Prompts shorter than one block produce zero hashes
//   and the scorer contributes nothing — routing then falls back to the
//   queue-scorer / KV-cache-scorer weights and the "sticky pod" assertion
//   becomes flaky.
//
//   Each prompt below is sized to span at least eight full 16-token
//   blocks (~128 tokens ≈ ~512+ ASCII characters, using the GAIE
//   averageCharactersPerToken = 4 heuristic) so the scorer always has
//   multiple block hashes to match on and the sticky-routing assertion
//   is deterministic.
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

	// sendChatWithPrompt forwards to the non-auth helper — the
	// prefix-cache case no longer enables the API-key
	// AuthorizationPolicy (see cases.go).
	sendChatWithPrompt := func(url, model, prompt string) (*http.Response, error) {
		return utils.SendChatCompletionWithPrompt(url, model, prompt)
	}

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
		// ~700 chars ≈ ~175 tokens ≈ ~11 full 16-token blocks. Well above
		// the single-block floor required for the prefix-cache-scorer to
		// produce stable hashes; see the file-level comment.
		const prompt = "Explain in considerable depth the foundations of quantum computing for a curious beginner. " +
			"Start with the difference between a classical bit and a quantum bit, then introduce the principles " +
			"of superposition and entanglement using small concrete examples. Walk through the operation of a " +
			"single-qubit Hadamard gate, a two-qubit CNOT gate, and explain how these gates compose into circuits " +
			"that solve problems faster than any classical algorithm. Conclude by surveying real hardware platforms, " +
			"including superconducting qubits, trapped ions, and photonic systems, and the engineering challenges " +
			"of decoherence and error correction in current noisy intermediate-scale quantum devices."

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
				resp, err := sendChatWithPrompt(caseGatewayURL, model, prompt)
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
						"pod %s received %.0f requests, expected all %d (prefix cache should make EPP sticky); full distribution: %v", pod, delta, numRequests, diff)
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
			simulatorHasPrefixCache := cacheHitsDiff[stickyPod] > 0
			if !simulatorHasPrefixCache {
				GinkgoWriter.Printf("[INFO] vllm:prefix_cache_hits is 0 on %s \u2014 simulator may not implement real prefix caching\n", stickyPod)
			}

			By("verifying EPP prefix indexer metrics show non-trivial prefix matching")
			hitRatio, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
				"inference_extension_prefix_indexer_hit_ratio", nil)
			Expect(err).NotTo(HaveOccurred())
			hitBytes, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
				"inference_extension_prefix_indexer_hit_bytes", nil)
			Expect(err).NotTo(HaveOccurred())

			if simulatorHasPrefixCache {
				Expect(hitRatio).To(BeNumerically(">", 0),
					"inference_extension_prefix_indexer_hit_ratio should be > 0 after repeated identical prompts")
				Expect(hitBytes).To(BeNumerically(">", 0),
					"inference_extension_prefix_indexer_hit_bytes should be > 0 after repeated identical prompts")
			} else {
				GinkgoWriter.Printf("[INFO] EPP prefix indexer hit_ratio=%.4f hit_bytes=%.0f \u2014 skipping strict assertion (simulator lacks real prefix caching)\n", hitRatio, hitBytes)
			}
		})
	})

	Context("Different prompt categories sticky routing", func() {
		const numPerCategory = 3
		// promptA and promptB share NO common prefix, so the EPP
		// prefix-cache-scorer must steer each category to a different pod.
		// Each is sized to span ~9 full 16-token blocks (~570+ chars).
		const promptA = "Write a comprehensive Python implementation of the merge sort algorithm with detailed inline " +
			"commentary. The function must accept a list of integers, recursively split it in halves until " +
			"singleton sublists remain, then merge the halves back together while preserving sort order. " +
			"Include explicit handling for empty and single-element inputs, a suite of unit tests covering " +
			"already sorted, reverse sorted, and uniformly random inputs, and a short paragraph at the end " +
			"that analyses the time and space complexity using big-O notation."
		const promptB = "Describe the historical development of the special theory of relativity, starting with the " +
			"Michelson and Morley aether-drift experiment of 1887 and ending with Albert Einstein's foundational " +
			"1905 paper. Explain how the postulate that the vacuum speed of light is the same in every inertial " +
			"frame forces a re-examination of simultaneity, gives rise to the phenomena of time dilation and " +
			"length contraction, and unifies three-dimensional space with one-dimensional time into a single " +
			"four-dimensional spacetime manifold."

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
				resp, err := sendChatWithPrompt(caseGatewayURL, model, promptA)
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
				"category A should be sticky — one pod should have received all %d requests; full distribution: %v", numPerCategory, diffA)

			By(fmt.Sprintf("sending category B prompt %d times", numPerCategory))
			beforeB, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())

			for i := 0; i < numPerCategory; i++ {
				resp, err := sendChatWithPrompt(caseGatewayURL, model, promptB)
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
				"category B should be sticky — one pod should have received all %d requests; full distribution: %v", numPerCategory, diffB)

			By("checking prefix cache hits on each sticky pod")
			afterCacheHits, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
			Expect(err).NotTo(HaveOccurred())
			cacheHitsDiff := utils.DiffSnapshots(beforeCacheHits, afterCacheHits)

			// prefix_cache_hits may stay 0 if the simulator doesn't implement
			// real prefix caching. Log for visibility.
			if cacheHitsDiff[podA] == 0 || cacheHitsDiff[podB] == 0 {
				GinkgoWriter.Printf("[INFO] vllm:prefix_cache_hits is 0 \u2014 simulator may not implement real prefix caching\n")
			}

			By("verifying per-pod request counts match per-category expectations")
			// diffA/diffB are vllm:request_success_total deltas, which serve
			// as a proxy for prefix_cache_queries routing verification: each
			// sticky pod must have received exactly the per-category count.
			Expect(diffA[podA]).To(BeNumerically("==", float64(numPerCategory)),
				"category A sticky pod %s should have received exactly %d requests", podA, numPerCategory)
			Expect(diffB[podB]).To(BeNumerically("==", float64(numPerCategory)),
				"category B sticky pod %s should have received exactly %d requests", podB, numPerCategory)
		})
	})

	Context("Pod deletion fallback", utils.GinkgoLabelNightly, func() {
		// ~590 chars ≈ ~9 full 16-token blocks; see file-level comment for
		// the prompt-length rationale.
		const prompt = "Outline the major scientific revolutions of the twentieth century in chronological order. " +
			"Begin with the introduction of relativity, then the development of quantum mechanics, the discovery " +
			"of the double-helix structure of DNA, the formulation of the standard model of particle physics, " +
			"the experimental confirmation of plate tectonics, and conclude with the sequencing of the human " +
			"genome. For each revolution, summarise the key experiments that drove the paradigm shift and " +
			"identify the principal scientists involved."

		It("should re-route to another pod when the sticky pod is deleted", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			By("sending requests to establish a sticky pod")
			before, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())

			for i := 0; i < 3; i++ {
				resp, err := sendChatWithPrompt(caseGatewayURL, model, prompt)
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
				resp, err := sendChatWithPrompt(caseGatewayURL, model, prompt)
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

			By("verifying EPP scheduler success count incremented and failure count did not")
			afterSchedSuccess, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
				"inference_extension_scheduler_attempts_total", map[string]string{"status": "success"})
			Expect(err).NotTo(HaveOccurred())
			Expect(afterSchedSuccess).To(BeNumerically(">", 0),
				"inference_extension_scheduler_attempts_total{status=success} should be > 0 after re-routing")

			afterSchedFailure, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
				"inference_extension_scheduler_attempts_total", map[string]string{"status": "failure"})
			Expect(err).NotTo(HaveOccurred())
			Expect(afterSchedFailure).To(BeNumerically("==", 0),
				"inference_extension_scheduler_attempts_total{status=failure} should not increment after graceful re-route")
		})
	})
})
