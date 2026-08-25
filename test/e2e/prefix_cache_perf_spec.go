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
// This extends prefix_cache_routing_spec.go (which verifies functional
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
//     when the EPP build exports it (an independent, router-side view of the
//     same property).
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

	// perfStickyMeasuredRequests is the number of identical requests used to
	// measure one prefix. Ten samples let the 70% target map exactly to 7/10;
	// reusing perfMeasuredRounds=6 would accidentally require 5/6 (83.3%).
	perfStickyMeasuredRequests = 10

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

func replayRequestCount(sessions []utils.ReplaySession) int64 {
	var total int64
	for _, session := range sessions {
		total += int64(len(session.Turns))
	}
	return total
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

func noncePrefixedTurn(base []utils.ChatMessage) ([]utils.ChatMessage, bool) {
	turn := make([]utils.ChatMessage, 0, len(base)+1)
	turn = append(turn, utils.ChatMessage{Role: "system", Content: uniqueNonce()})
	turn = append(turn, base...)
	return turn, utils.FitsModelContextWithCompletion(turn, 1)
}

// uniquePrefixSessions rewrites sessions into genuinely unique-prefix load: each
// session becomes a single-turn request carrying its full cumulative context
// (the last, largest turn) with a unique nonce prepended. This removes both
// cross-session and intra-session prefix sharing, so the backend cannot serve
// any block from cache — the counterfactual for the shared-prefix run.
func uniquePrefixSessions(sessions []utils.ReplaySession) []utils.ReplaySession {
	out := make([]utils.ReplaySession, 0, len(sessions))
	for i, s := range sessions {
		for turnIdx := len(s.Turns) - 1; turnIdx >= 0; turnIdx-- {
			turn, fits := noncePrefixedTurn(s.Turns[turnIdx])
			if !fits {
				continue
			}
			out = append(out, utils.ReplaySession{
				SessionID: fmt.Sprintf("%s-unique-%d", s.SessionID, i),
				Turns:     [][]utils.ChatMessage{turn},
				PreGaps:   []float64{0},
			})
			break
		}
	}
	return out
}

var _ = Describe("Prefix Cache Routing Perf",
	utils.GinkgoLabelPerf, utils.GinkgoLabelPrefixCache, Ordered, func() {

		model := CaseDeployments[CasePrefixCachePerf][0].Name
		caseNamespace := CaseNamespace(CasePrefixCachePerf)

		var (
			gatewayURL string
			fixture    string
		)

		// forEachUsableShard streams the fixture shard-by-shard (one shard
		// resident at a time, bounding memory to the largest shard) and calls
		// fn with each shard that has >=2 sessions — the minimum to exercise
		// cross-pod prefix routing. Shards with fewer are logged and skipped. A
		// hard load error fails the spec. BeforeAll has already guaranteed at
		// least one usable shard exists, so fn runs at least once.
		forEachUsableShard := func(fn func(shardName string, sessions []utils.ReplaySession)) {
			err := utils.StreamTraceShards(fixture, func(sh utils.TraceShard) error {
				shardName := filepath.Base(sh.Path)
				if len(sh.Sessions) < 2 {
					GinkgoWriter.Printf("[perf] skipping shard %s: %d session(s) < 2, cannot exercise cross-pod routing\n",
						shardName, len(sh.Sessions))
					return nil
				}
				By(fmt.Sprintf("shard %s: %d sessions", shardName, len(sh.Sessions)))
				Expect(utils.RefreshPortForward(gatewayURL)).To(Succeed(),
					"refreshing gateway port-forward before shard %s", shardName)
				fn(shardName, sh.Sessions)
				return nil
			})
			Expect(err).NotTo(HaveOccurred(), "streaming trace shards from %s", fixture)
		}

		BeforeAll(func() {
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

		It("replays agentic traces under load with stable errors, effective caching, and an A/B benefit", func(ctx SpecContext) {
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
				total503           int64
				totalTransport     int64
				totalLoadShed      int64
				hitsDeltaSum       float64
				queriesDeltaSum    float64
				maxIndexerHitRatio float64
				indexerMetricPods  int
			)

			forEachUsableShard(func(shardName string, sessions []utils.ReplaySession) {
				hitsBefore, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
				Expect(err).NotTo(HaveOccurred())
				queriesBefore, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
				Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("replaying %d sessions x %d rounds at concurrency %d", len(sessions), perfMeasuredRounds, perfConcurrency))
				runSessions := repeatSessions(sessions, perfMeasuredRounds)
				stats := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, runSessions, perfConcurrency, false)
				GinkgoWriter.Printf("[perf] shard replay stats: %+v\n", stats)
				Expect(stats.Total).To(Equal(replayRequestCount(runSessions)),
					"shared-prefix replay did not attempt every selected turn: %+v", stats)
				Expect(stats.OtherNon2xx-stats.StatusCounts[429]).To(BeNumerically("==", 0),
					"shared-prefix replay for shard %s produced unexpected 4xx responses: %+v", shardName, stats)
				Expect(stats.Errors5xx-stats.StatusCounts[503]).To(BeNumerically("==", 0),
					"shared-prefix replay for shard %s produced unexpected 5xx responses: %+v", shardName, stats)

				hitsAfter, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
				Expect(err).NotTo(HaveOccurred())
				queriesAfter, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
				Expect(err).NotTo(HaveOccurred())

				Expect(utils.ValidateCounterSnapshots(hitsBefore, hitsAfter)).To(Succeed(),
					"prefix-cache hit counters changed identity or reset during shard replay")
				Expect(utils.ValidateCounterSnapshots(queriesBefore, queriesAfter)).To(Succeed(),
					"prefix-cache query counters changed identity or reset during shard replay")
				shardHits := utils.SumSnapshot(utils.DiffSnapshots(hitsBefore, hitsAfter))
				shardQueries := utils.SumSnapshot(utils.DiffSnapshots(queriesBefore, queriesAfter))
				Expect(shardQueries).To(BeNumerically(">", 0),
					"shard %s replay did not advance vllm:prefix_cache_queries", shardName)
				shardRatio := shardHits / shardQueries
				Expect(shardRatio).To(BeNumerically(">=", prefixCacheHitRatioTarget),
					"shard %s prefix-cache hit ratio %.3f below target %.2f (hitsΔ=%.0f queriesΔ=%.0f)",
					shardName, shardRatio, prefixCacheHitRatioTarget, shardHits, shardQueries)
				hitsDeltaSum += shardHits
				queriesDeltaSum += shardQueries

				// EPP-side cross-check: the prefix indexer's own hit ratio. It is
				// computed independently of the sim's vllm counters — from the
				// router's longest-prefix-match decisions — so agreement between
				// the two confirms EPP believed it routed to a warm pod AND the
				// backend confirms the block was resident. Keep the best value
				// observed across shards (measured over each shard's warm rounds).
				indexerRatio, present, err := utils.ScrapeEPPMetricWithPresence(ctx, clientset, model, caseNamespace,
					"inference_extension_prefix_indexer_hit_ratio", nil)
				Expect(err).NotTo(HaveOccurred())
				indexerMetricPods += present
				if indexerRatio > maxIndexerHitRatio {
					maxIndexerHitRatio = indexerRatio
				}

				totalReqs += stats.Total
				total5xx += stats.Errors5xx
				total503 += stats.StatusCounts[503]
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
			Expect(total5xx).To(Equal(total503),
				"only 503 load-shed responses are allowed under load (aggregate 5xx=%d, 503=%d)", total5xx, total503)
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
			if indexerMetricPods == 0 {
				GinkgoWriter.Printf("[perf] EPP does not export inference_extension_prefix_indexer_hit_ratio; skipping router-side cross-check\n")
			} else {
				GinkgoWriter.Printf("[perf] inference_extension_prefix_indexer_hit_ratio (max across shards) = %.4f\n", maxIndexerHitRatio)
				Expect(maxIndexerHitRatio).To(BeNumerically(">", 0),
					"EPP's inference_extension_prefix_indexer_hit_ratio stayed 0 under shared-prefix load — the prefix-cache-scorer's "+
						"indexer registered no prefix matches; the vllm-side ratio was %.3f", ratio)
			}

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

			// Reuse the six-round load above as the shared side of the A/B
			// comparison. Only run the inexpensive one-request-per-session unique
			// control here; replaying the shared workload again would duplicate the
			// dominant cost without adding another measurement.
			var uniqueHits, uniqueQueries float64
			forEachUsableShard(func(_ string, sessions []utils.ReplaySession) {
				hitsBefore, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
				Expect(err).NotTo(HaveOccurred())
				queriesBefore, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
				Expect(err).NotTo(HaveOccurred())

				By("running unique-prefix load (per-request unique nonce, no shared prefix)")
				uniqueSessions := uniquePrefixSessions(sessions)
				Expect(uniqueSessions).NotTo(BeEmpty(), "no session in shard has room for the unique-prefix nonce")
				stats := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, uniqueSessions, perfConcurrency, false)
				Expect(stats.Total).To(Equal(replayRequestCount(uniqueSessions)),
					"A/B unique run did not attempt every selected request: %+v", stats)
				Expect(stats.Success).To(Equal(stats.Total), "A/B unique run must succeed completely: %+v", stats)

				hitsAfter, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_hits")
				Expect(err).NotTo(HaveOccurred())
				queriesAfter, err := utils.ScrapeModelMetric(ctx, clientset, caseNamespace, model, "vllm:prefix_cache_queries")
				Expect(err).NotTo(HaveOccurred())

				Expect(utils.ValidateCounterSnapshots(hitsBefore, hitsAfter)).To(Succeed(),
					"prefix-cache hit counters changed identity or reset during unique control")
				Expect(utils.ValidateCounterSnapshots(queriesBefore, queriesAfter)).To(Succeed(),
					"prefix-cache query counters changed identity or reset during unique control")
				uniqueHits += utils.SumSnapshot(utils.DiffSnapshots(hitsBefore, hitsAfter))
				uniqueQueries += utils.SumSnapshot(utils.DiffSnapshots(queriesBefore, queriesAfter))
			})

			Expect(uniqueQueries).To(BeNumerically(">", 0),
				"unique-prefix control did not advance vllm:prefix_cache_queries")
			uniqueRatio := uniqueHits / uniqueQueries

			GinkgoWriter.Printf("[perf] aggregate shared-prefix hit ratio=%.3f unique-prefix hit ratio=%.3f\n", ratio, uniqueRatio)
			Expect(ratio).To(BeNumerically(">", uniqueRatio),
				"shared-prefix load should yield a higher cache-hit ratio than unique-prefix load (shared=%.3f unique=%.3f)",
				ratio, uniqueRatio)
		})

		It("concentrates each prefix's requests on a single pod (sticky routing under load)", utils.GinkgoLabelPerf, func(ctx SpecContext) {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			forEachUsableShard(func(_ string, sessions []utils.ReplaySession) {
				// Measure one exact prompt per session. Give it a fresh block-0 nonce
				// so earlier perf specs cannot have warmed the same prefix on both
				// pods. Replaying a whole multi-turn session would aggregate several
				// distinct cumulative prompts and could look evenly distributed even
				// when each prompt is sticky. Use the first loader-approved turn to
				// avoid later turns near the context limit.
				prefixes := make([]utils.ReplaySession, 0, len(sessions))
				for _, session := range sessions {
					for _, base := range session.Turns {
						turn, fits := noncePrefixedTurn(base)
						if !fits {
							continue
						}
						prefixes = append(prefixes, utils.ReplaySession{
							SessionID: session.SessionID,
							Turns:     [][]utils.ChatMessage{turn},
							PreGaps:   []float64{0},
						})
						break
					}
				}
				Expect(prefixes).NotTo(BeEmpty(), "no session in shard has room for the sticky-routing nonce")

				// Prime every selected prefix once under concurrent load so the
				// sticky pod for each is established.
				warm := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, prefixes, perfConcurrency, false)
				Expect(warm.Total).To(Equal(replayRequestCount(prefixes)),
					"sticky priming did not attempt every selected request: %+v", warm)
				Expect(warm.Success).To(Equal(warm.Total), "sticky priming must succeed completely: %+v", warm)

				for _, s := range prefixes {
					By(fmt.Sprintf("measuring routing concentration for session %s", s.SessionID))

					before, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
					Expect(err).NotTo(HaveOccurred())

					// Replay this one prefix in isolation and sequentially, so
					// the per-pod request delta reflects the routing *decision*
					// for a single warm prefix rather than worker interleaving.
					single := []utils.ReplaySession{s}
					stats := utils.ReplaySessionsConcurrent(ctx, gatewayURL, model, repeatSessions(single, perfStickyMeasuredRequests), 1, false)
					Expect(stats.Total).To(Equal(int64(perfStickyMeasuredRequests)),
						"sticky run did not attempt every request: %+v", stats)
					Expect(stats.Success).To(Equal(stats.Total), "sticky run must succeed completely: %+v", stats)

					after, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNamespace, model)
					Expect(err).NotTo(HaveOccurred())
					Expect(utils.ValidateCounterSnapshots(before, after)).To(Succeed(),
						"request-success counters changed identity or reset during sticky measurement")

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
