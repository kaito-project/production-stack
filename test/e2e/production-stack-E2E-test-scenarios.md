# E2E test scenarios

## Test Coverage Overview

| Layer | Test Group | CI Tier | Description |
|-------|-----------|---------|-------------|
| **Smoke/Framework** | GPU Node Mocker (Smoke) | PR | Framework sanity and Gateway connectivity |
| **Infrastructure** | Fake Node / Shadow Pod Lifecycle | PR | gpu-node-mocker two-phase pipeline validation |
| | InferenceSet / InferencePool Lifecycle | PR | KAITO controller and routing infrastructure health |
| | NodeClaim / Fake Node Cleanup | PR | Reverse-path teardown and reconcile idempotency |
| | InferenceSet Scaling (E2E) | Nightly | End-to-end scale up/down driven by KEDA + traffic verification |
| **Routing** | Model-Based Routing | PR | BBR → HTTPRoute → EPP → Pod routing correctness |
| | Unknown Model / Malformed Request Handling | PR | Catch-all 404 JSON + BBR malformed-body behaviour |
| **Performance** | Prefix Cache Aware Routing | PR (core) + Nightly (full) | EPP prefix-cache scorer and KV-cache locality |
| **Shadow Pod** | llm-d-inference-sim Validation | PR | Simulator API, metrics, streaming, KV cache, concurrency, tokenizer |
| **Resilience** | Failure & Self-Healing | Nightly | Lease heartbeat, shadow-pod reconcile, NodeClaim cleanup, tokenizer degradation, KEDA anti-flapping |

### CI tier policy

Tests are tagged so CI can pick the right subset per trigger. The intent is: **PR tier must stay under
~15 min and never damage cluster state**; **Nightly tier is allowed to take longer, destroy pods, and
wait for KEDA cooldowns**.

| Tier | Ginkgo label | When it runs | Criteria |
|------|--------------|--------------|----------|
| **PR** | `Smoke`, `Infra`, `Routing`, `ShadowPod`, `PrefixCache` (core only) | Every pull request, required to merge | Deterministic, fast (< ~15 min total), no destructive actions on cluster state (no pod/NodeClaim deletion, no KEDA scale waits). Covers every user-facing API contract and happy-path regression. |
| **Nightly** | `Scaling`, `Resilience`, `PrefixCache-Full` | Scheduled nightly + pre-release, advisory on PR | Destructive (deletes pods, kills tokenizer, upgrades Helm chart), slow (minutes of KEDA polling + cooldown), or flaky-by-design (timing-sensitive anti-flapping). Catches silent-degradation bugs that PR tier cannot exercise in a bounded time budget. |

Per-case mapping is listed in the **CI Tier** column of each group below. A case marked *PR + Nightly*
runs in both; its PR variant uses a reduced iteration count / shorter window, and the Nightly variant
uses the full workload described in the scenario.

### Metrics sources used for validation

Routing and scaling correctness is verified by scraping Prometheus metrics from two sources (response
bodies contain only the standard OpenAI fields, so all behavioural assertions rely on metrics):

**llm-d-inference-sim metrics** — scraped from each shadow pod at `:8000/metrics`:

* `vllm:request_success_total{model_name, finish_reason}` — per-pod successful request counter (primary signal to identify which pod served a request)
* `vllm:num_requests_running{model_name}` — currently running requests
* `vllm:num_requests_waiting{model_name}` — waiting queue depth (**KEDA scaling trigger**)
* `vllm:kv_cache_usage_perc{model_name}` — KV cache utilisation 0–1
* `vllm:prefix_cache_hits{model_name}` — prefix cache hit tokens
* `vllm:prefix_cache_queries{model_name}` — prefix cache query tokens
* `vllm:prompt_tokens_total{model_name}` — total prompt tokens processed
* `vllm:generation_tokens_total{model_name}` — total generated tokens
* `vllm:e2e_request_latency_seconds{model_name}` — end-to-end latency histogram

**EPP metrics** — scraped from the EPP pod's metrics port:

* `inference_objective_request_total{model_name, target_model_name}` — total inference requests routed
* `inference_objective_request_error_total{model_name, target_model_name, error_code}` — routing errors
* `inference_extension_scheduler_attempts_total{status}` — scheduling success/failure counter
* `inference_extension_scheduler_e2e_duration_seconds` — scheduling decision latency
* `inference_extension_prefix_indexer_hit_ratio` — prefix cache hit ratio 0–1
* `inference_extension_prefix_indexer_hit_bytes` — matched prefix length in bytes
* `inference_pool_ready_pods{name}` — number of healthy pods in the pool
* `inference_pool_per_pod_queue_size{name, model_server_pod}` — per-pod queue depth
* `inference_pool_average_kv_cache_utilization{name}` — average KV cache across pool
* `inference_pool_average_queue_size{name}` — average pending requests across pool
* `inference_extension_flow_control_pool_saturation{inference_pool}` — pool saturation 0–1

---

## GPU Node Mocker — Smoke

**CI tier:** PR (required).

Basic sanity checks that the test framework is wired correctly and the Gateway is reachable before heavier tests run.

* Framework validation › properly initialised — Catches broken test wiring before anything else runs.
* Gateway connectivity › reachable and responds — Fails fast if the Gateway is down, avoiding misleading routing test failures.

> Removed: `Framework validation › utility constants defined` — unit-test level, no independent e2e value; moved to Go unit tests.

## Fake node and shadow pod lifecycle — Infra

**CI tier:** PR (required). Read-only observation of the initial provisioning path — no pod or NodeClaim is deleted, so the cluster is left in a clean state for downstream PR tests.

Verifies the gpu-node-mocker's two-phase pipeline: Phase 1 creates fake nodes with correct labels and Ready status; Phase 2 creates shadow pods with real CNI IPs and patches the original inference pods to Running.

* Fake nodes with correct labels — Wrong labels mean KAITO won't match the node to the InferenceSet, so pods never schedule.
* Fake nodes in Ready — A non-Ready node gets tainted; the scheduler won't place pods on it.
* Shadow pods running — If shadow pods aren't running, there's no real backend to serve inference traffic.
* Both containers present — Missing inference-sim means no `/v1/chat/completions`; missing tokenizer means EPP can't score the pod.
* Original pods patched to Running — If the status patch fails, KAITO sees the pod as Pending and `InferenceReady` stays False.

## InferenceSet and InferencePool lifecycle — Infra

**CI tier:** PR (required). CR creation + object-graph assertions only; bounded by a single Eventually poll.

Verifies KAITO's InferenceSet controller processed the CRs, auto-created InferencePool resources, and that the routing infrastructure (HTTPRoute, EPP, DestinationRules) is healthy.

* InferenceSet created → downstream resources appear — In one assertion block, verify the InferenceSet exists with the expected spec, `status.readyReplicas` matches `spec.replicas`, the `InferencePool` is auto-created (proves `gatewayAPIInferenceExtension` feature gate), and the matching HTTPRoute + DestinationRule exist. Consolidates the previously separated "InferenceSets deployed" / "Ready replicas match desired" / "Auto-created InferencePool" checks into one lifecycle assertion to avoid redundant polling.
* EPP pods running — Without a running EPP, the Gateway can't select backend pods; routing is dead.
* HTTPRoute Accepted=True — An unaccepted HTTPRoute silently drops all traffic; this catches misconfig early.
* DestinationRules `trafficPolicy.tls.mode=SIMPLE` — Missing or wrong-mode DestinationRules cause TLS errors between Istio sidecars and EPP; requests fail cryptically.

### NodeClaim / Fake Node cleanup — Infra

**CI tier:** PR (required). Operates on a dedicated test namespace / NodeClaim created for the case, so deletion does not affect other tests in the run.

Isolates the reverse path of Phase 1 so that failures in resource teardown can be localised without
running a full Scale-Down.

* NodeClaim deletion cascades — Delete a NodeClaim directly; verify the corresponding fake Node is removed and its `kube-node-lease` Lease is garbage-collected. Proves `NodeClaimReconciler`'s cleanup path and that the lease-renewal goroutine is stopped (no leaked goroutine keeps renewing a Lease for a deleted Node).
* NodeClaim reconcile idempotency — Trigger duplicate reconcile events (e.g., re-apply the same NodeClaim) and verify no duplicate fake Nodes or Leases are created. Protects against reconcile storms multiplying fake infrastructure.

## Shadow Pod (llm-d-inference-sim) Validation — ShadowPod

**CI tier:** PR (required). Direct per-pod API calls, no scaling or failure injection.

Verifies that the `llm-d-inference-sim` container running in each shadow pod correctly simulates an
OpenAI-compatible inference backend, exposing the metrics, streaming, KV-cache behaviour, and tokenizer
surface that the rest of the production-stack test suite depends on.

Tests in this group talk **directly to the shadow pod** (port-forward / in-cluster client),
not through the Gateway, so simulator-surface regressions are localised before Routing/PrefixCache
tests are run.

* Health and readiness endpoints — `GET /health` and `GET /ready` both return HTTP 200. If these fail, Kubernetes probes mark the pod unhealthy and EPP removes it from the endpoint list.
* OpenAI-compatible chat completion — `POST /v1/chat/completions` returns a valid response containing `id`, `model`, `choices[].message.content`, and `usage` (`prompt_tokens`, `completion_tokens`, `total_tokens`). The response `model` field equals the configured `served-model-name`, which is the same surface `GET /v1/models` exposes — so model listing is covered implicitly and no separate test is needed.
* Prometheus metrics endpoint — `GET /metrics` returns valid Prometheus text format containing at minimum `vllm:request_success_total`, `vllm:num_requests_running`, `vllm:num_requests_waiting`, and `vllm:kv_cache_usage_perc`. Without these metrics, EPP scoring and KEDA scaling cannot function.
* Streaming response — `POST /v1/chat/completions` with `"stream": true` returns Server-Sent Events with `chat.completion.chunk` objects and terminates with `data: [DONE]`. Validates the streaming path used by most real clients.
* KV cache metrics update — After sending requests, verify `vllm:kv_cache_usage_perc{model_name}` > 0 and `vllm:prefix_cache_queries{model_name}` increments. Proves the KV-cache simulation is active so EPP's prefix-cache scorer has real data to work with (requires `enable-kvcache: true` in the simulator config).
* Concurrent request handling — Send more concurrent requests than `max-num-seqs` (default 5). Verify `vllm:num_requests_running` saturates at the limit while excess requests appear in `vllm:num_requests_waiting`. Proves the simulator correctly models queue pressure — the signal that drives KEDA scaling.
* Tokenizer sidecar correctness — `POST /tokenize` (served by the UDS tokenizer sidecar) returns a token list for the given text. Assert (a) the response is a non-empty token list, (b) token count scales with input length (long prompt yields strictly more tokens than short prompt), and (c) two identical inputs produce identical token sequences (stability). EPP depends on token-boundary stability to compute prefix hashes for cache-aware routing; a flaky tokenizer silently degrades prefix scoring to random selection.

> Removed: `Model listing — GET /v1/models` — redundant with chat-completion's `model` field assertion; kept as part of chat-completion validation above.

## Model-Based Routing — Routing

**CI tier:** PR (required). Core request-routing contract; must stay green on every PR.

Verifies the full BBR → HTTPRoute → EPP → inference pod request chain by sending requests and asserting
the response proves correct model-level routing.

**Validation approach:** routing correctness is verified by (1) inspecting the response `model` field and
(2) scraping per-pod `vllm:request_success_total{model_name}` from each shadow pod to confirm only pods in
the correct pool received the request. EPP-side counters corroborate the scheduling decision.

* Correct model name for phi — Send `POST /v1/chat/completions` with `{"model": "routing-phi", ...}`. Verify `response.model == "routing-phi"`. Proves the full BBR → HTTPRoute → EPP → pod chain works for the first model.
* Correct model name for ministral — Same validation for the second model; catches per-pool misrouting.
* Cross-model isolation (serial) — Send N requests for each model sequentially. Scrape `vllm:request_success_total{model_name}` from every shadow pod before and after. Verify that phi pods' counters only incremented for phi requests, and ministral pods' counters only incremented for ministral requests. No cross-pool contamination.
* Cross-model isolation (concurrent) — Launch **interleaved concurrent** traffic: 20 in-flight phi + 20 in-flight ministral requests at the same time. Verify per-pod `vllm:request_success_total{model_name}` still shows zero cross-contamination. BBR and EPP are two chained ext_proc filters; serial tests cannot expose header-state leakage between concurrent requests within the same Envoy worker.
* Model-specific route wins over catch-all — While the catch-all `model-not-found` HTTPRoute is deployed (see *Unknown model handling*), requests with a known model name must never hit it. Verify by scraping the `model-not-found` Service's request counter / access log: it must stay at 0 during the above cross-model runs. Guards against HTTPRoute ordering regressions where the catch-all rule silently absorbs valid traffic.
* EPP routing success (metrics) — After the above runs, verify `inference_extension_scheduler_attempts_total{status="success"}` increased by the total requests sent and `{status="failure"}` did not change. Verify `inference_objective_request_total{model_name="routing-phi"}` and `{model_name="routing-ministral"}` match the per-model counts. Proves EPP actively scheduled each request rather than falling through to a default route.
* Load distribution — With 2 replicas per pool, send 20+ requests per model. Scrape `vllm:request_success_total` from each pod and verify no pod received 0 requests and none received more than 80% of its pool's traffic. Cross-check with `inference_pool_per_pod_queue_size{name, model_server_pod}` to confirm both pods were active. If one pod gets all traffic, EPP's scoring or endpoint list is broken.
* Debug EnvoyFilter log chain — For **one** representative request in each of the cases above (phi, ministral, concurrent cross-model), tail istio-ingressgateway logs and verify the `inference-debug-filter` Lua chain emitted exactly one `[PRE-BBR]`, one `[POST-EPP]`, and one `[RESPONSE]` line sharing the same `x-request-id`. In the `[POST-EPP]` line, `x-gateway-model-name` equals the request's model field (proves BBR ran) and `x-gateway-destination-endpoint` is a non-empty `IP:port` matching the pod that actually served the request per `vllm:request_success_total` (proves EPP ran and its decision was honoured). Health-check `GET /` traffic must not produce these log lines. This folds the debug/observability surface into the main routing assertions so filter-chain ordering regressions (e.g., Istio upgrade) cannot silently break on-cluster debugging.

## Prefix Cache Aware Routing — PrefixCache

**CI tier:** PR (core) + Nightly (full). The first two cases (*Same prompt → same pod*, *Different
categories → sticky routing*) run on PR as core correctness checks. *Pod deletion fallback* is
Nightly-only because it deletes a live shadow pod.

Verifies EPP's prefix-cache scorer routes requests with shared prefixes to the same backend pod,
maximising KV-cache reuse and minimising recomputation.

**Validation approach:** scrape per-pod `vllm:request_success_total` deltas to identify which pod served
each batch of requests. Cross-check with EPP's `inference_extension_prefix_indexer_hit_ratio` /
`inference_extension_prefix_indexer_hit_bytes` to confirm prefix matching actually occurred, and with
`vllm:prefix_cache_hits` / `vllm:prefix_cache_queries` on the sticky pod to confirm simulator-side cache
activity.

* Same prompt → same pod — Send the same prompt 5 times. Scrape `vllm:request_success_total` from each pod before and after; verify exactly one pod's counter incremented by 5 and all others by 0. After the second request onward, verify `inference_extension_prefix_indexer_hit_ratio` > 0 **and** `inference_extension_prefix_indexer_hit_bytes` > 0, proving EPP's prefix-cache scorer matched a non-trivial cached prefix.
* Different categories → sticky routing — Define two distinct prompt categories (e.g., "Explain quantum computing…" vs "Write a Python sort function…"). Send each category 3 times. Verify via per-pod `vllm:request_success_total` deltas that each category consistently hits the same pod across its 3 requests (they may be the same or different pods, but each category must be internally sticky). Verify `vllm:prefix_cache_hits{model_name}` increments on each sticky pod and `vllm:prefix_cache_queries` on each sticky pod matches the number of requests with shared prefixes — proves the full metrics pipeline from EPP prefix scoring to simulator cache tracking is consistent.
* Pod deletion fallback *(Nightly only)* — Delete the pod currently receiving sticky traffic. Send the same prompt again. Verify the request succeeds (HTTP 200 with a valid response body). Scrape `vllm:request_success_total` to confirm a different pod served the request, and verify `inference_extension_scheduler_attempts_total{status="success"}` incremented while `{status="failure"}` did not. Proves EPP gracefully re-selects another pod when the cached one disappears.

> Removed: `Prefix cache metrics consistency` — merged into the two cases above to avoid an extra e2e traffic run for assertions already implied by them.

## InferenceSet Scaling — Infra

**CI tier:** Nightly. KEDA polling interval + cooldown windows make the full Scale-Up/Scale-Down loop
too slow (several minutes each) and timing-sensitive for PR CI; Anti-Flapping in particular needs a
full cooldown window to be meaningful.

Verifies the system reacts correctly to **workload-driven** scaling by exercising the full end-to-end
chain: rising request queue → KEDA triggers InferenceSet replica change → gpu-node-mocker provisions or
removes fake nodes and shadow pods → EPP updates its endpoint list → traffic redistributes.

The KEDA scaling trigger is the annotation-configured metric `vllm:num_requests_waiting` with threshold
`10` (see `scaledobject.kaito.sh/*` annotations on the InferenceSet). Each phase of the flow is verified
individually so failures can be localised.

### Scale-Up — End-to-End

* Record baseline — Capture initial `InferenceSet.spec.replicas` (e.g. 2), per-pod `vllm:request_success_total`, `inference_pool_ready_pods{name}`, and fake-node/shadow-pod counts. Establishes a clean starting point.
* Generate queue pressure — Send concurrent requests at a rate exceeding serving capacity so that `vllm:num_requests_waiting` rises above the KEDA threshold (10) on at least one pod. Verify by scraping shadow pod metrics. This is the trigger condition for scale-up.
* KEDA triggers replica increase — Poll the InferenceSet and verify `.spec.replicas` increases (e.g. 2 → 3) within the KEDA polling interval. Proves the ScaledObject correctly read the vLLM metric and patched the InferenceSet.
* New fake node provisioned — Verify a new fake node appears with the correct labels and `Ready=True` status. Proves gpu-node-mocker Phase 1 (NodeClaimReconciler) handled the new NodeClaim produced by Karpenter.
* New shadow pod running — Verify a new shadow pod reaches `Running` in the model's namespace with both `inference-sim` and `tokenizer` containers ready. Proves gpu-node-mocker Phase 2 (ShadowPodReconciler) completed the shadow-pod creation path.
* Original pod patched to Running — Verify the newly scheduled original pod's status is patched to `Running` with the shadow pod's real CNI IP as `PodIP`. Proves the status-patch pipeline works for dynamically scaled pods, not just initially deployed ones.
* InferencePool endpoint list updated — Verify `inference_pool_ready_pods{name}` increases to match the new replica count. If InferencePool doesn't learn about the new pod, EPP cannot route to it and the new replica is wasted.
* New pod receives traffic — Continue sending requests. Scrape `vllm:request_success_total` from the new pod and verify it is > 0 within a reasonable window. The new replica must actually serve traffic, not just exist on paper. (General load-distribution fairness is covered by *Model-Based Routing → Load distribution*; not duplicated here.)

### Scale-Down — End-to-End

* Stop traffic and drain queue — Cease all inbound requests. Wait for `vllm:num_requests_waiting` to reach 0 on every pod and `vllm:num_requests_running` to reach 0. The queue must fully drain before KEDA begins scale-down.
* KEDA triggers replica decrease — Poll the InferenceSet and verify `.spec.replicas` decreases (e.g. 3 → 2) after the KEDA cooldown period elapses. Proves the scale-down path is not silently stuck.
* Excess shadow pod and fake node cleaned up — Verify the extra shadow pod is deleted from the model's namespace, the corresponding fake node is removed, and the NodeClaim is released. Proves the cleanup pipeline (ShadowPodReconciler → NodeClaim release, including the lease-renewal goroutine stop) works.
* InferencePool endpoint list shrinks — Verify `inference_pool_ready_pods{name}` decreases to match the new replica count. A stale endpoint would cause EPP to route to a non-existent pod and requests would fail.
* Remaining pods continue to serve traffic — Resume sending requests. Verify all requests succeed (HTTP 200). Scrape `vllm:request_success_total` on the remaining pods to confirm they handle the load. `inference_extension_scheduler_attempts_total{status="failure"}` must not increment.
* No request failures during transition — Send a low-rate stream of requests throughout the scale-down window. Verify `inference_objective_request_error_total` does not increment and no HTTP 5xx responses are observed. Proves the system handles pod removal gracefully without dropping in-flight requests.

### Anti-Flapping

* Below-threshold stability — Hold `vllm:num_requests_waiting` strictly below the KEDA threshold for the full polling + cooldown window. Verify `InferenceSet.spec.replicas` does **not** change and no new NodeClaim/fake-node/shadow-pod is created. Guards against false-positive scale-ups from noisy metrics.
* Cooldown respected — Immediately after a Scale-Down completes, re-apply queue pressure above the threshold. Verify the next Scale-Up does **not** fire until the ScaledObject's cooldown has elapsed and that no intermediate scale oscillations are observed. Flapping is the top production pain point for autoscalers and must be explicitly pinned.

## Unknown model / malformed request handling — Routing

**CI tier:** PR (required). Negative-path API contract, fast and deterministic.

Verifies the Gateway's client-compatibility contract for bad inputs: unknown models hit the catch-all
JSON 404, and malformed bodies do not crash BBR or leak Envoy's raw error pages.

* 404 for unknown model — Request with `{"model": "does-not-exist", ...}` returns HTTP 404 with the OpenAI-compatible JSON body `{"error":{"code":"model_not_found", ...}}` served by the `model-not-found` Service — not Envoy's raw 404 HTML.
* Missing `model` field — Request body `{"messages": [...]}` with no `model` key. BBR cannot inject `x-gateway-model-name`; verify the request is handled predictably (routed to the catch-all 404 JSON, not a 500 or hang).
* Non-string `model` field — Body with `{"model": 42, ...}`. Verify BBR rejects or falls through cleanly (catch-all 404 JSON), with no Envoy 5xx.
* Non-JSON body on `/v1/*` — Send `text/plain` or truncated JSON to `POST /v1/chat/completions`. Verify response is a well-formed error (4xx), the BBR ext_proc filter does not crash (Envoy stays up, subsequent valid requests succeed), and no goroutine leak appears in BBR logs.
* Non-`/v1/*` path — `GET /healthz` or an arbitrary path bypasses BBR entirely; verify it is not wrongly inspected as an inference request and does not inject `x-gateway-model-name`.

## Failure & Self-Healing — Resilience

**CI tier:** Nightly (all cases). Deliberately damages cluster state (kills containers, deletes pods,
upgrades Helm releases) and/or waits >1 min for self-healing to converge — unsafe and too slow for PR
CI.

Covers reverse paths, failure injection, and silent-degradation scenarios that are not exercised by the
happy-path suites. Intended to run in nightly / pre-release CI — not on every PR — because they
deliberately damage cluster state.

* Lease heartbeat keeps fake node Ready — Let an InferenceSet run steady-state for longer than
  `leaseDurationSeconds` (40s). Periodically read the fake Node's Lease in `kube-node-lease` and verify
  `renewTime` advances by roughly `leaseRenewIntervalSeconds` (10s) each period and the Node stays
  `Ready=True` without the `node.kubernetes.io/unreachable` taint. If the Phase-1 renewal goroutine
  stops, the node-lifecycle-controller marks the node `Unknown` within 40s and evicts all inference
  pods — the entire stack silently fails.
* Shadow pod deletion self-heals — Delete a specific shadow pod from the model's namespace while the original
  inference pod is running. Verify `ShadowPodReconciler` creates a replacement shadow pod (same
  `kaito.sh/shadow-pod-for` label, new CNI IP), the original pod's `status.podIP` is re-patched to the
  new IP, and the `kaito.sh/shadow-pod-ref` annotation is updated. Send a request afterwards and
  verify it succeeds against the new backend. Guards the reconciler's idempotency for second-order
  events.
* Tokenizer sidecar failure → random fallback — Stop the UDS tokenizer container in every shadow pod
  of a pool (e.g., via `kubectl exec kill` or by removing its socket). Send the same prompt 10 times.
  Expect: all requests still succeed (HTTP 200) and `inference_extension_scheduler_attempts_total{status="success"}` increases by 10, but `inference_extension_prefix_indexer_hit_ratio` drops to ~0 and
  per-pod `vllm:request_success_total` deltas are roughly uniform (random fallback, not sticky).
  Directly verifies the documented "prefix scoring silently degrades to random selection" contract.
* Rolling shadow-pod restart under load — While driving a low-rate stream of valid requests, delete
  every shadow pod in a pool one-by-one with a short delay. Verify the overall 5xx ratio stays below
  an agreed threshold (e.g., < 1%) and `inference_objective_request_error_total` does not spike
  sharply. Proves EPP's endpoint-list refresh is responsive to real churn.
* gpu-node-mocker Helm-chart upgrade — Upgrade the `charts/gpu-node-mocker` release with a new image
  tag or `values.yaml` change while an InferenceSet is serving traffic. Verify the new controller
  starts, leases keep renewing across the upgrade window, no existing shadow pods are deleted, and
  traffic continues to succeed. Guards the install path used by `install-components.sh`.
