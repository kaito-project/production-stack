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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Prefix-cache routing PERFORMANCE / LOAD spec (issue #109).
//
// This extends prefix_cache_routing_test.go (which verifies functional
// stickiness with a few sequential requests) into a sustained CONCURRENT load
// test. Load is driven by replaying real multi-turn agent sessions from the
// sammshen/lmcache-agentic-traces dataset (a small committed fixture under
// test/e2e/testdata; see hack/e2e/scripts/extract_agentic_traces.py).
//
// It runs on the gpu-node-mocker path (llm-d-inference-sim shadow pods, no
// real GPU), so it needs no A100 capacity. The simulator is configured with
// enable-kvcache + a real tokenizer, so it computes real 16-token block hashes
// and tracks real prefix-cache hits/queries — the cache-hit ratio and
// sticky-routing behaviour are genuine. Only throughput/latency are synthetic
// (the sim sleeps per a latency profile instead of doing GPU compute), which
// issue #109 already scopes out. The spec asserts, under load:
//
//   - Error-rate stability: zero 5xx and bounded non-2xx while saturated.
//   - Prefix-cache effectiveness: aggregate hit ratio (Δvllm:prefix_cache_hits /
//     Δvllm:prefix_cache_queries) >= 0.80 once shared prefixes are warm.
//   - KV-cache / queue signal: vllm:kv_cache_usage_perc and
//     vllm:num_requests_waiting are asserted to be exported (so the
//     kv-cache-utilization-scorer and queue-scorer have signal) and within
//     valid bounds ([0,1] ratio / non-negative gauge).
//
// Labeled Perf + PrefixCache so it can be selected/skipped independently:
//
//	E2E_LABEL='Perf' make test-e2e

const (
	// prefixCacheHitRatioTarget is the minimum aggregate prefix-cache hit
	// ratio expected once the shared prefixes are warm (issue #109: "should
	// be over 80%").
	prefixCacheHitRatioTarget = 0.80

	// perfWarmUpRounds primes the KV cache so first-touch misses don't skew
	// the measured hit ratio.
	perfWarmUpRounds = 1
	// perfMeasuredRounds replays the warm sessions repeatedly under load; the
	// hit ratio and error counters are measured across these rounds.
	perfMeasuredRounds = 3
	// perfConcurrency is the number of sessions replayed in parallel.
	perfConcurrency = 8

	// perfStickyConcentrationTarget is the minimum share of a single prefix's
	// requests that must land on one backend pod. It is deliberately below
	// 1.0: under concurrency the queue-scorer and kv-cache-utilization-scorer
	// can legitimately spill some requests to other pods, so we assert
	// concentration ("mostly sticky"), not 100% stickiness (issue #109).
	perfStickyConcentrationTarget = 0.70
)

// resolveTraceFixture locates the committed agentic-trace fixture, honoring the
// E2E_TRACE_FIXTURE override and tolerating both the repo-root and test/e2e
// working directories.
func resolveTraceFixture() string {
	if p := os.Getenv("E2E_TRACE_FIXTURE"); p != "" {
		return p
	}
	candidates := []string{
		filepath.Join("test", "e2e", "testdata", "agentic-traces.jsonl"),
		filepath.Join("testdata", "agentic-traces.jsonl"),
		filepath.Join("..", "..", "test", "e2e", "testdata", "agentic-traces.jsonl"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

// repeatSessions returns the sessions concatenated `rounds` times, so a small
// fixture can generate sustained load and warm cache hits on later rounds.
func repeatSessions(sessions []utils.ReplaySession, rounds int) []utils.ReplaySession {
	out := make([]utils.ReplaySession, 0, len(sessions)*rounds)
	for r := 0; r < rounds; r++ {
		out = append(out, sessions...)
	}
	return out
}

// perfNonceSeed makes uniquePrefixSessions' nonces distinct across processes so
// a re-run against a still-warm backend can't accidentally reuse cached blocks.
var perfNonceSeed = time.Now().UnixNano()

// perfNonceCounter guarantees every nonce within a process is unique.
var perfNonceCounter atomic.Uint64

// uniqueNonce returns a block of >16 whitespace-separated unique tokens. It is
// prepended at the FRONT of a request so the simulator's 16-token prefix-hash
// chain diverges from block 0 — guaranteeing no cached block can be reused.
func uniqueNonce() string {
	n := perfNonceCounter.Add(1)
	var b strings.Builder
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, "u%d_%d_%d ", n, perfNonceSeed, i)
	}
	return b.String()
}

// uniquePrefixSessions rewrites sessions into genuinely unique-prefix load: each
// session becomes a single-turn request carrying its full cumulative context
// (the last, largest turn) with a unique nonce prepended. This removes both
// cross-session and intra-session prefix sharing, so the backend cannot serve
// any block from cache — the counterfactual for the shared-prefix run.
func uniquePrefixSessions(sessions []utils.ReplaySession) []utils.ReplaySession {
	out := make([]utils.ReplaySession, 0, len(sessions))
	for i, s := range sessions {
		if len(s.Turns) == 0 {
			continue
		}
		base := s.Turns[len(s.Turns)-1]
		turn := make([]utils.ChatMessage, 0, len(base)+1)
		turn = append(turn, utils.ChatMessage{Role: "system", Content: uniqueNonce()})
		turn = append(turn, base...)
		out = append(out, utils.ReplaySession{
			SessionID: fmt.Sprintf("%s-unique-%d", s.SessionID, i),
			Turns:     [][]utils.ChatMessage{turn},
			PreGaps:   []float64{0},
		})
	}
	return out
}

var _ = Describe("Prefix Cache Routing Perf",
	utils.GinkgoLabelPerf, utils.GinkgoLabelPrefixCache, Ordered, func() {

		model := CaseDeployments[CasePrefixCachePerf][0].Name
		caseNamespace := CaseNamespace(CasePrefixCachePerf)

		var (
			ctx        context.Context
			gatewayURL string
			sessions   []utils.ReplaySession
		)

		BeforeAll(func() {
			ctx = context.Background()

			fixture := resolveTraceFixture()
			var err error
			sessions, err = utils.LoadTraceSessions(fixture)
			Expect(err).NotTo(HaveOccurred(), "failed to load trace fixture %s", fixture)
			Expect(len(sessions)).To(BeNumerically(">=", 2),
				"need at least 2 trace sessions to exercise cross-pod prefix routing")

			gatewayURL = InstallCase(CasePrefixCachePerf)
		})

		AfterAll(func() {
			UninstallCase(CasePrefixCachePerf)
		})

		It("replays agentic traces under concurrent load with zero 5xx and >=80% prefix-cache hit ratio", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			By("confirming the mocked inference backend has >=2 shadow pods")
			baseline, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(baseline)).To(BeNumerically(">=", 2),
				"prefix-cache routing needs >=2 shadow pods")

			By("warming the KV cache with an initial pass of every session")
			warm := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, repeatSessions(sessions, perfWarmUpRounds), perfConcurrency, false)
			Expect(warm.Errors5xx).To(BeNumerically("==", 0),
				"warm-up produced 5xx responses: %+v", warm)

			By("snapshotting prefix-cache and success counters before the measured load")
			hitsBefore, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
			Expect(err).NotTo(HaveOccurred())
			queriesBefore, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
			Expect(err).NotTo(HaveOccurred())
			successBefore, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:request_success_total")
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("replaying %d sessions x %d rounds at concurrency %d", len(sessions), perfMeasuredRounds, perfConcurrency))
			stats := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, repeatSessions(sessions, perfMeasuredRounds), perfConcurrency, false)
			GinkgoWriter.Printf("[perf] replay stats: %+v\n", stats)

			By("asserting error-rate stability under saturation")
			Expect(stats.Total).To(BeNumerically(">", 0))
			Expect(stats.Errors5xx).To(BeNumerically("==", 0),
				"gateway->EPP->backend chain must stay 5xx-free under load: %+v", stats)
			Expect(stats.TransportErr).To(BeNumerically("==", 0),
				"replay hit transport errors: %+v", stats)
			// Bounded backpressure: at most 10% of requests may be 429/503.
			Expect(float64(stats.OtherNon2xx)).To(BeNumerically("<=", 0.10*float64(stats.Total)),
				"too many non-2xx (429/503) under load: %+v", stats)

			By("snapshotting prefix-cache and success counters after the measured load")
			hitsAfter, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
			Expect(err).NotTo(HaveOccurred())
			queriesAfter, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
			Expect(err).NotTo(HaveOccurred())
			successAfter, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:request_success_total")
			Expect(err).NotTo(HaveOccurred())

			hitsDelta := utils.SumSnapshot(utils.DiffSnapshots(hitsBefore, hitsAfter))
			queriesDelta := utils.SumSnapshot(utils.DiffSnapshots(queriesBefore, queriesAfter))
			successDelta := utils.SumSnapshot(utils.DiffSnapshots(successBefore, successAfter))
			GinkgoWriter.Printf("[perf] prefix_cache hits Δ=%.0f queries Δ=%.0f request_success Δ=%.0f\n",
				hitsDelta, queriesDelta, successDelta)

			Expect(queriesDelta).To(BeNumerically(">", 0),
				"vllm:prefix_cache_queries did not advance — backend did not exercise the prefix cache")

			ratio := hitsDelta / queriesDelta
			By(fmt.Sprintf("asserting prefix-cache hit ratio %.3f >= %.2f", ratio, prefixCacheHitRatioTarget))
			Expect(ratio).To(BeNumerically(">=", prefixCacheHitRatioTarget),
				"prefix-cache hit ratio %.3f below target %.2f (hitsΔ=%.0f queriesΔ=%.0f)",
				ratio, prefixCacheHitRatioTarget, hitsDelta, queriesDelta)

			By("asserting KV-cache utilization is exported and a valid ratio")
			kvPresent, kvTotal, err := utils.ScrapeModelMetricPresence(ctx, clientset, caseNamespace, model, "vllm:kv_cache_usage_perc")
			Expect(err).NotTo(HaveOccurred())
			Expect(kvPresent).To(BeNumerically(">=", 1),
				"vllm:kv_cache_usage_perc must be exported by >=1 pod so the kv-cache-utilization-scorer has signal (present=%d/%d)",
				kvPresent, kvTotal)
			kvUsage, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:kv_cache_usage_perc")
			Expect(err).NotTo(HaveOccurred())
			// The sim frees KV blocks as requests complete, so usage can settle
			// back to 0 once load drains; assert the invariant (a valid [0,1]
			// ratio) rather than a positive value, which would be flaky.
			Expect(utils.MinSnapshot(kvUsage)).To(BeNumerically(">=", 0),
				"kv_cache_usage_perc must be non-negative: %+v", kvUsage)
			Expect(utils.MaxSnapshot(kvUsage)).To(BeNumerically("<=", 1.0),
				"kv_cache_usage_perc must be a valid ratio <=1: %+v", kvUsage)
			GinkgoWriter.Printf("[perf] vllm:kv_cache_usage_perc max across pods = %.4f\n", utils.MaxSnapshot(kvUsage))

			By("asserting queue depth is exported and non-negative")
			waitPresent, waitTotal, err := utils.ScrapeModelMetricPresence(ctx, clientset, caseNamespace, model, "vllm:num_requests_waiting")
			Expect(err).NotTo(HaveOccurred())
			Expect(waitPresent).To(BeNumerically(">=", 1),
				"vllm:num_requests_waiting must be exported by >=1 pod so the queue-scorer has signal (present=%d/%d)",
				waitPresent, waitTotal)
			waiting, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:num_requests_waiting")
			Expect(err).NotTo(HaveOccurred())
			Expect(utils.MinSnapshot(waiting)).To(BeNumerically(">=", 0),
				"num_requests_waiting is a non-negative gauge: %+v", waiting)
			GinkgoWriter.Printf("[perf] vllm:num_requests_waiting max across pods = %.0f\n", utils.MaxSnapshot(waiting))
		})

		It("shows shared-prefix load yields a higher cache-hit ratio than unique-prefix load", utils.GinkgoLabelPerf, func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			measureRatio := func(runSessions []utils.ReplaySession) float64 {
				hb, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
				Expect(err).NotTo(HaveOccurred())
				qb, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
				Expect(err).NotTo(HaveOccurred())

				stats := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, runSessions, perfConcurrency, false)
				Expect(stats.Errors5xx).To(BeNumerically("==", 0), "A/B run produced 5xx: %+v", stats)

				ha, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
				Expect(err).NotTo(HaveOccurred())
				qa, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
				Expect(err).NotTo(HaveOccurred())

				qd := utils.SumSnapshot(utils.DiffSnapshots(qb, qa))
				if qd <= 0 {
					return 0
				}
				return utils.SumSnapshot(utils.DiffSnapshots(hb, ha)) / qd
			}

			By("running shared-prefix load (repeated identical sessions)")
			sharedRatio := measureRatio(repeatSessions(sessions, perfMeasuredRounds))

			By("running unique-prefix load (per-request unique nonce, no shared prefix)")
			uniqueRatio := measureRatio(uniquePrefixSessions(sessions))

			GinkgoWriter.Printf("[perf] shared-prefix hit ratio=%.3f unique-prefix hit ratio=%.3f\n", sharedRatio, uniqueRatio)
			Expect(sharedRatio).To(BeNumerically(">", uniqueRatio),
				"shared-prefix load should yield a higher cache-hit ratio than unique-prefix load (shared=%.3f unique=%.3f)",
				sharedRatio, uniqueRatio)
		})

		It("concentrates each prefix's requests on a single pod (sticky routing under load)", utils.GinkgoLabelPerf, func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			// Prime every prefix once under concurrent load so the sticky pod
			// for each is established (and first-touch cold misses don't count
			// against the concentration measurement below).
			warm := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, sessions, perfConcurrency, false)
			Expect(warm.Errors5xx).To(BeNumerically("==", 0), "priming produced 5xx: %+v", warm)

			for _, s := range sessions {
				By(fmt.Sprintf("measuring routing concentration for session %s", s.SessionID))

				before, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
				Expect(err).NotTo(HaveOccurred())

				// Replay this one prefix in isolation and sequentially, so the
				// per-pod request delta reflects the routing *decision* for a
				// single warm prefix rather than worker interleaving.
				single := []utils.ReplaySession{s}
				stats := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, repeatSessions(single, perfMeasuredRounds), 1, false)
				Expect(stats.Errors5xx).To(BeNumerically("==", 0), "sticky run produced 5xx: %+v", stats)

				after, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
				Expect(err).NotTo(HaveOccurred())

				delta := utils.DiffSnapshots(before, after)
				served := utils.SumSnapshot(delta)
				Expect(served).To(BeNumerically(">", 0),
					"no successful requests recorded for session %s (per-pod deltas=%+v)", s.SessionID, delta)

				concentration := utils.MaxSnapshot(delta) / served
				GinkgoWriter.Printf("[perf] session %s concentration=%.3f served=%.0f deltas=%+v\n",
					s.SessionID, concentration, served, delta)
				Expect(concentration).To(BeNumerically(">=", perfStickyConcentrationTarget),
					"prefix %s should concentrate >=%.0f%% of its requests on one pod, got %.1f%% (per-pod deltas=%+v)",
					s.SessionID, perfStickyConcentrationTarget*100, concentration*100, delta)
			}
		})
	})
