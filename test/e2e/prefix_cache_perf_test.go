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
	"errors"
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

// Prefix-cache routing PERFORMANCE / LOAD spec.
//
// This extends prefix_cache_routing_test.go (which verifies functional
// stickiness with a few sequential requests) into a sustained CONCURRENT load
// test. Load is driven by replaying real multi-turn agent sessions from the
// sammshen/lmcache-agentic-traces dataset (a small committed fixture under
// test/e2e/testdata; see hack/e2e/scripts/extract_agentic_traces.py).
//
// It runs on the gpu-node-mocker path (llm-d-inference-sim shadow pods, no
// real GPU), so it needs no A100 capacity. The simulator is configured with
// enable-kvcache + block-size 16 and the sim's built-in (dummy) tokenizer,
// which still produces deterministic per-block hashes, so it tracks real
// prefix-cache hits/queries — the cache-hit ratio and sticky-routing
// behaviour are genuine. Only throughput/latency are synthetic
// (the sim sleeps per a latency profile instead of doing GPU compute), which
// is out of scope for this spec. The spec asserts, under load:
//
//   - Error-rate stability: zero 5xx and bounded non-2xx while saturated.
//   - Prefix-cache effectiveness: aggregate hit ratio (Δvllm:prefix_cache_hits /
//     Δvllm:prefix_cache_queries) >= 0.80 once shared prefixes are warm,
//     cross-checked against EPP's own inference_extension_prefix_indexer_hit_ratio
//     (an independent, router-side view of the same property).
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
	// ratio expected across a shard's replay (target: "should be over 80%").
	// There is no separate warm-up pass, so the measured window includes each
	// shard's fixed first-touch cold misses; perfMeasuredRounds is set high
	// enough that those are diluted below this target.
	prefixCacheHitRatioTarget = 0.80

	// perfMeasuredRounds replays each shard's sessions repeatedly under load and
	// measures the hit ratio and error counters across ALL rounds. The first
	// round absorbs that shard's one-off first-touch cold misses; the remaining
	// rounds run hot. Enough rounds keeps the aggregate ratio above
	// prefixCacheHitRatioTarget without a dedicated warm-up phase (worst-case
	// floor (R-1)/R, plus intra-round prefix sharing lifts it further).
	perfMeasuredRounds = 6
	// perfConcurrency is the number of sessions replayed in parallel.
	perfConcurrency = 8

	// perfStickyConcentrationTarget is the minimum share of a single prefix's
	// requests that must land on one backend pod. It is deliberately below
	// 1.0: under concurrency the queue-scorer and kv-cache-utilization-scorer
	// can legitimately spill some requests to other pods, so we assert
	// concentration ("mostly sticky"), not 100% stickiness.
	perfStickyConcentrationTarget = 0.70
)

// resolveTraceFixture locates the committed agentic-trace fixture, honoring the
// E2E_TRACE_FIXTURE override and tolerating both the repo-root and test/e2e
// working directories. The override may be a single JSONL file (dev: one shard,
// replayed in isolation), or a directory of shard files (a real run:
// the whole corpus, streamed one shard at a time by StreamTraceShards so peak
// memory stays bounded to the largest shard — see traces.go). Either way each
// shard is measured on its own and the results are aggregated, so the same
// code path serves both single-shard dev and whole-corpus runs.
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
			fixture    string
		)

		// forEachUsableShard streams the fixture shard-by-shard (one shard
		// resident at a time, bounding memory to the largest shard) and calls
		// fn with each shard that has >=2 sessions — the minimum to exercise
		// cross-pod prefix routing. Shards with fewer are logged and skipped. A
		// hard load error fails the spec. BeforeAll has already guaranteed at
		// least one usable shard exists, so fn runs at least once.
		forEachUsableShard := func(fn func(sessions []utils.ReplaySession)) {
			err := utils.StreamTraceShards(fixture, func(sh utils.TraceShard) error {
				if len(sh.Sessions) < 2 {
					GinkgoWriter.Printf("[perf] skipping shard %s: %d session(s) < 2, cannot exercise cross-pod routing\n",
						filepath.Base(sh.Path), len(sh.Sessions))
					return nil
				}
				By(fmt.Sprintf("shard %s: %d sessions", filepath.Base(sh.Path), len(sh.Sessions)))
				fn(sh.Sessions)
				return nil
			})
			Expect(err).NotTo(HaveOccurred(), "streaming trace shards from %s", fixture)
		}

		BeforeAll(func() {
			ctx = context.Background()

			fixture = resolveTraceFixture()

			// Validate by streaming the shards once (one resident at a time, so
			// this stays memory-bounded even for a whole-corpus directory).
			// Skip — don't Fail — when no shard can exercise cross-pod routing:
			// an empty fixture (ErrNoUsableSessions) or one whose every shard
			// holds <2 distinct sessions is a fixture-availability limitation,
			// not a regression, so the suite stays green with actionable
			// guidance. A genuinely broken path (missing file, parse error) is
			// still a hard failure.
			var totalShards, usableShards, totalSessions int
			err := utils.StreamTraceShards(fixture, func(sh utils.TraceShard) error {
				totalShards++
				totalSessions += len(sh.Sessions)
				if len(sh.Sessions) >= 2 {
					usableShards++
				}
				return nil
			})
			if errors.Is(err, utils.ErrNoUsableSessions) {
				Skip(fmt.Sprintf("trace fixture %s yielded 0 usable sessions; the prefix-cache perf spec needs a shard with "+
					">=2 distinct sessions to exercise cross-pod prefix routing. Point E2E_TRACE_FIXTURE at a directory of "+
					"shards or a single richer fixture.", fixture))
			}
			Expect(err).NotTo(HaveOccurred(), "failed to load trace fixture %s", fixture)
			GinkgoWriter.Printf("[perf] fixture %s: %d shard(s), %d with >=2 sessions, %d sessions total\n",
				fixture, totalShards, usableShards, totalSessions)
			if usableShards == 0 {
				Skip(fmt.Sprintf("trace fixture %s: no shard has >=2 distinct sessions (across %d shard(s), %d session(s) "+
					"total); the prefix-cache perf spec needs >=2 sessions within a shard to exercise cross-pod routing. "+
					"Point E2E_TRACE_FIXTURE at a richer shard or a directory of shards.",
					fixture, totalShards, totalSessions))
			}

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

			// Per-shard measure, then aggregate. Each shard is measured on its
			// own — the first replay round absorbs that shard's first-touch cold
			// misses and the rest run hot — so the hit ratio reflects routing
			// quality rather than KV eviction across shards, which keeps the
			// >=0.80 target meaningful for a corpus far larger than the sim's KV
			// pool. Deltas are summed across shards; the final ratio is
			// Σhits/Σqueries.
			var (
				totalReqs          int64
				total5xx           int64
				totalTransport     int64
				totalLoadShed      int64
				hitsDeltaSum       float64
				queriesDeltaSum    float64
				maxIndexerHitRatio float64
			)

			forEachUsableShard(func(sessions []utils.ReplaySession) {
				hitsBefore, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
				Expect(err).NotTo(HaveOccurred())
				queriesBefore, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
				Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("replaying %d sessions x %d rounds at concurrency %d", len(sessions), perfMeasuredRounds, perfConcurrency))
				stats := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, repeatSessions(sessions, perfMeasuredRounds), perfConcurrency, false)
				GinkgoWriter.Printf("[perf] shard replay stats: %+v\n", stats)

				hitsAfter, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
				Expect(err).NotTo(HaveOccurred())
				queriesAfter, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
				Expect(err).NotTo(HaveOccurred())

				hitsDeltaSum += utils.SumSnapshot(utils.DiffSnapshots(hitsBefore, hitsAfter))
				queriesDeltaSum += utils.SumSnapshot(utils.DiffSnapshots(queriesBefore, queriesAfter))

				// EPP-side cross-check: the prefix indexer's own hit ratio. It is
				// computed independently of the sim's vllm counters — from the
				// router's longest-prefix-match decisions — so agreement between
				// the two confirms EPP believed it routed to a warm pod AND the
				// backend confirms the block was resident. Keep the best value
				// observed across shards (measured over each shard's warm rounds).
				indexerRatio, err := utils.ScrapeEPPMetric(ctx, clientset, model, caseNamespace,
					"inference_extension_prefix_indexer_hit_ratio", nil)
				Expect(err).NotTo(HaveOccurred())
				if indexerRatio > maxIndexerHitRatio {
					maxIndexerHitRatio = indexerRatio
				}

				totalReqs += stats.Total
				total5xx += stats.Errors5xx
				totalTransport += stats.TransportErr
				// Bounded backpressure: at most 10% of requests may be load-shed
				// (429 Too Many Requests / 503 Service Unavailable). 400s (e.g.
				// a turn exceeding max-model-len) are content errors, not
				// overload, and are excluded — the load generator drops
				// over-length turns at load time, so they should not occur here.
				totalLoadShed += stats.StatusCounts[429] + stats.StatusCounts[503]
			})

			By("asserting aggregate error-rate stability under saturation")
			Expect(totalReqs).To(BeNumerically(">", 0))
			Expect(total5xx).To(BeNumerically("==", 0),
				"gateway->EPP->backend chain must stay 5xx-free under load (aggregate 5xx=%d)", total5xx)
			Expect(totalTransport).To(BeNumerically("==", 0),
				"replay hit transport errors (aggregate=%d)", totalTransport)
			Expect(float64(totalLoadShed)).To(BeNumerically("<=", 0.10*float64(totalReqs)),
				"too many 429/503 (load-shed) under load: %d / %d", totalLoadShed, totalReqs)

			GinkgoWriter.Printf("[perf] aggregate prefix_cache hits Δ=%.0f queries Δ=%.0f over %d requests\n",
				hitsDeltaSum, queriesDeltaSum, totalReqs)
			Expect(queriesDeltaSum).To(BeNumerically(">", 0),
				"vllm:prefix_cache_queries did not advance — backend did not exercise the prefix cache")

			ratio := hitsDeltaSum / queriesDeltaSum
			By(fmt.Sprintf("asserting aggregate prefix-cache hit ratio %.3f >= %.2f", ratio, prefixCacheHitRatioTarget))
			Expect(ratio).To(BeNumerically(">=", prefixCacheHitRatioTarget),
				"prefix-cache hit ratio %.3f below target %.2f (hitsΔ=%.0f queriesΔ=%.0f)",
				ratio, prefixCacheHitRatioTarget, hitsDeltaSum, queriesDeltaSum)

			By("cross-checking EPP's own prefix-indexer hit ratio (independent of the vllm counters)")
			GinkgoWriter.Printf("[perf] inference_extension_prefix_indexer_hit_ratio (max across shards) = %.4f\n", maxIndexerHitRatio)
			Expect(maxIndexerHitRatio).To(BeNumerically(">", 0),
				"EPP's inference_extension_prefix_indexer_hit_ratio stayed 0 under shared-prefix load — the prefix-cache-scorer's "+
					"indexer registered no prefix matches (or the metric is not exported by this EPP build); the vllm-side ratio was %.3f",
				ratio)

			By("asserting KV-cache utilization is exported and a valid ratio")
			kvUsage, kvPresent, err := utils.ScrapeModelMetricWithPresence(ctx, clientset, caseNamespace, model, "vllm:kv_cache_usage_perc")
			Expect(err).NotTo(HaveOccurred())
			Expect(kvPresent).To(BeNumerically(">=", 1),
				"vllm:kv_cache_usage_perc must be exported by >=1 pod so the kv-cache-utilization-scorer has signal (present=%d/%d)",
				kvPresent, len(kvUsage))
			// The sim frees KV blocks as requests complete, so usage can settle
			// back to 0 once load drains; assert the invariant (a valid [0,1]
			// ratio) rather than a positive value, which would be flaky.
			Expect(utils.MinSnapshot(kvUsage)).To(BeNumerically(">=", 0),
				"kv_cache_usage_perc must be non-negative: %+v", kvUsage)
			Expect(utils.MaxSnapshot(kvUsage)).To(BeNumerically("<=", 1.0),
				"kv_cache_usage_perc must be a valid ratio <=1: %+v", kvUsage)
			GinkgoWriter.Printf("[perf] vllm:kv_cache_usage_perc max across pods = %.4f\n", utils.MaxSnapshot(kvUsage))

			By("asserting queue depth is exported and non-negative")
			waiting, waitPresent, err := utils.ScrapeModelMetricWithPresence(ctx, clientset, caseNamespace, model, "vllm:num_requests_waiting")
			Expect(err).NotTo(HaveOccurred())
			Expect(waitPresent).To(BeNumerically(">=", 1),
				"vllm:num_requests_waiting must be exported by >=1 pod so the queue-scorer has signal (present=%d/%d)",
				waitPresent, len(waiting))
			Expect(utils.MinSnapshot(waiting)).To(BeNumerically(">=", 0),
				"num_requests_waiting is a non-negative gauge: %+v", waiting)
			GinkgoWriter.Printf("[perf] vllm:num_requests_waiting max across pods = %.0f\n", utils.MaxSnapshot(waiting))
		})

		It("shows shared-prefix load yields a higher cache-hit ratio than unique-prefix load", utils.GinkgoLabelPerf, func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			// measure runs one load and returns the (hits, queries) deltas it
			// produced, so callers can accumulate across shards before taking a
			// ratio (Σhits/Σqueries) — the same shard-by-shard aggregation the
			// main spec uses, keeping peak memory at one shard.
			measure := func(runSessions []utils.ReplaySession) (hits, queries float64) {
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

				return utils.SumSnapshot(utils.DiffSnapshots(hb, ha)), utils.SumSnapshot(utils.DiffSnapshots(qb, qa))
			}

			var sharedHits, sharedQueries, uniqueHits, uniqueQueries float64
			forEachUsableShard(func(sessions []utils.ReplaySession) {
				By("running shared-prefix load (repeated identical sessions)")
				h, q := measure(repeatSessions(sessions, perfMeasuredRounds))
				sharedHits += h
				sharedQueries += q

				By("running unique-prefix load (per-request unique nonce, no shared prefix)")
				h, q = measure(uniquePrefixSessions(sessions))
				uniqueHits += h
				uniqueQueries += q
			})

			ratio := func(hits, queries float64) float64 {
				if queries <= 0 {
					return 0
				}
				return hits / queries
			}
			sharedRatio := ratio(sharedHits, sharedQueries)
			uniqueRatio := ratio(uniqueHits, uniqueQueries)

			GinkgoWriter.Printf("[perf] aggregate shared-prefix hit ratio=%.3f unique-prefix hit ratio=%.3f\n", sharedRatio, uniqueRatio)
			Expect(sharedRatio).To(BeNumerically(">", uniqueRatio),
				"shared-prefix load should yield a higher cache-hit ratio than unique-prefix load (shared=%.3f unique=%.3f)",
				sharedRatio, uniqueRatio)
		})

		It("concentrates each prefix's requests on a single pod (sticky routing under load)", utils.GinkgoLabelPerf, func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			forEachUsableShard(func(sessions []utils.ReplaySession) {
				// Prime every prefix in this shard once under concurrent load so
				// the sticky pod for each is established (and first-touch cold
				// misses don't count against the concentration measurement
				// below).
				warm := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, sessions, perfConcurrency, false)
				Expect(warm.Errors5xx).To(BeNumerically("==", 0), "priming produced 5xx: %+v", warm)

				for _, s := range sessions {
					By(fmt.Sprintf("measuring routing concentration for session %s", s.SessionID))

					before, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
					Expect(err).NotTo(HaveOccurred())

					// Replay this one prefix in isolation and sequentially, so
					// the per-pod request delta reflects the routing *decision*
					// for a single warm prefix rather than worker interleaving.
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
	})
