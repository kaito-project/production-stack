# E2E Test Framework

End-to-end test suite for production-stack, built with [Ginkgo v2](https://onsi.github.io/ginkgo/) + [Gomega](https://onsi.github.io/gomega/). Tests are organised as **per-case `Ordered` Describes** that run in parallel against an AKS cluster.

## Test cases

Single source of truth: [`cases.go`](cases.go) → `CaseDeployments`. Each entry carries `Name`, `Namespace`, `Model` (preset), `Replicas`, `InstanceType`. The Gateway name is derived as `<namespace>-gw` by both `charts/modelharness` and `charts/modeldeployment`.

| Case key | Test file | Namespace | Gateway | Deployments | Lifecycle |
| --- | --- | --- | --- | --- | --- |
| `CaseGPUMocker` | `gpu_mocker_test.go` | `e2e-gpu-mocker` | `e2e-gpu-mocker-gw` | `gpu-mocker-phi` | `BeforeAll` / `AfterAll` |
| `CaseModelRouting` | `model_routing_test.go` | `e2e-model-routing` | `e2e-model-routing-gw` | `routing-phi`, `routing-ministral` | `BeforeAll` / `AfterAll` |
| `CasePrefixCache` | `prefix_cache_routing_test.go` | `e2e-prefix-cache` | `e2e-prefix-cache-gw` | `prefix-cache-phi` (replicas ≥ 2) | `BeforeAll` / `AfterAll` |
| `CasePrefixCachePerf` | `prefix_cache_perf_test.go` | `e2e-pc-perf` | `e2e-pc-perf-gw` | `pc-perf-phi` (replicas ≥ 2) | `BeforeAll` / `AfterAll` |
| `CaseModelDeploymentChart` | `modeldeployment_chart_test.go` | `e2e-inferenceset-<rand>` | `e2e-inferenceset-<rand>-gw` | `mdchart-phi` | Per-`It`; namespace recycled in `AfterEach` |

`Name` is unique cluster-wide and is the value matched by `X-Gateway-Model-Name` (i.e. the `model` field clients send in OpenAI-compatible requests). `Model` is the KAITO preset only — multiple deployments may share a preset under different `Name`s.

Inference tests target the case's **`caseGatewayURL`**. Each case namespace gets its own Gateway, catch-all `model-not-found-direct` EnvoyFilter (Envoy `direct_response` 404), and (when enabled) API-key auth artifacts via the [`charts/modelharness`](../../charts/modelharness) chart installed by `EnsureNamespace`.

## Helpers

`utils/`:

- [`setup.go`](utils/setup.go) — `EnsureNamespace` (installs the modelharness chart per namespace), `DeleteNamespace`, `SetupInferenceSetsWithRouting`, `TeardownInferenceSetsWithRouting`, `WaitForGatewayService`.
- [`http.go`](utils/http.go) — multi-gateway port-forward (`GetGatewayURLFor`), `SendChatCompletion`.
- [`helm.go`](utils/helm.go) — `InstallModelDeployment`, `UninstallModelDeployment`, `InstallModelHarness`, `UninstallModelHarness`.
- [`inference.go`](utils/inference.go) — `WaitForInferenceSetReady`, `EPPServiceName`, snapshot/diff helpers.
- [`metrics.go`](utils/metrics.go), [`cluster.go`](utils/cluster.go), [`dynamic.go`](utils/dynamic.go), [`ginkgo.go`](utils/ginkgo.go).

`cases.go`:

- `InstallCase(caseName) string` — calls `EnsureNamespace`, waits for the gateway service, returns the case's gateway URL, and installs every chart in the case. Use in `BeforeAll`.
- `UninstallCase(caseName)` — uninstalls Helm releases and deletes the namespace (cascading the per-case Gateway + HTTPRoute). Use in `AfterAll`.
- `CaseNamespace(caseName)`, `CaseGatewayName(caseName)`.

## Running tests

```bash
# Default: 2 parallel workers, all labels
make test-e2e

# Subset by Ginkgo label
E2E_LABEL=Routing make test-e2e
E2E_LABEL=PrefixCache make test-e2e
E2E_LABEL=Smoke make test-e2e

# Override parallelism
E2E_PARALLEL=4 make test-e2e
```

Labels live in [`utils/ginkgo.go`](utils/ginkgo.go) and fall into two groups:

- **Cadence** (when a spec runs): `Smoke` (every PR), `Nightly` (long-running, nightly only).
- **Feature area** (what a spec verifies): `Infra`, `Routing`, `PrefixCache`, `Perf` (prefix-cache load/perf, see [below](#prefix-cache-perf--load-test)), `Auth`, `NetworkPolicy`, `Scaling` (scale-up / scale-down / anti-flapping), `InferenceSet`, `FilterOrder`, `Karpenter`, `Outage` (fail-closed / HA resilience).


### Bring up a cluster from scratch

```bash
# One-shot: create AKS, build/push image, install components, validate.
make e2e-up

# Custom config:
RESOURCE_GROUP=my-rg CLUSTER_NAME=my-cluster LOCATION=westus2 \
NODE_COUNT=2 NODE_VM_SIZE=Standard_D8s_v5 make e2e-up

# Run tests, then tear down:
make test-e2e
make e2e-teardown
```

Step-by-step targets exist as well: `e2e-setup`, `docker-build`, `e2e-push-image`, `e2e-install`, `e2e-validate`, `e2e-dump`, `e2e-teardown`. Set `SKIP_TEARDOWN=true` to keep the cluster after `make e2e`.

| Variable | Purpose | Default |
| --- | --- | --- |
| `E2E_LABEL` | Ginkgo label filter | _(all)_ |
| `E2E_PARALLEL` | Ginkgo `--procs` | `2` |
| `NODE_COUNT` | AKS node count | `2` |
| `MODELDEPLOYMENT_CHART` | Override chart path | _(repo root)_ |

## Prefix-cache perf / load test

[`prefix_cache_perf_test.go`](prefix_cache_perf_test.go) (labels `Perf` + `PrefixCache`, case `CasePrefixCachePerf`) drives **sustained concurrent load** through the gateway → EPP → backend chain and asserts on the EPP prefix-cache-scorer signals (hit ratio ≥ 80%, zero 5xx, bounded 429/503, KV-cache / queue metrics exported). It runs on the **gpu-node-mocker** path (`llm-d-inference-sim` shadow pods, no real GPU), and the simulator is configured with `enable-kvcache` + `block-size 16` using the sim's built-in (dummy) tokenizer, which still yields deterministic per-block hashes so `vllm:prefix_cache_hits/_queries` and sticky routing are genuine — only throughput/latency are synthetic.

The spec has three `It`s:

1. **Load + cache effectiveness** — replays the fixture under concurrency and asserts hit ratio ≥ 80%, zero 5xx / transport errors, ≤ 10% 429/503, and that `vllm:kv_cache_usage_perc` / `vllm:num_requests_waiting` are exported and in-bounds.
2. **A/B benefit** — shared-prefix load must yield a higher cache-hit ratio than genuinely unique-prefix load (per-request nonce at block 0).
3. **Sticky routing concentration** — replays each prefix in isolation (after a concurrent priming pass) and asserts one pod serves ≥ 70% of that prefix's requests (`perfStickyConcentrationTarget`). The threshold is below 100% because the queue / kv-cache scorers can legitimately spill some requests under load.

```bash
# Convenience target (serial, 90m timeout):
make test-e2e-perf

# Equivalent explicit invocation:
E2E_LABEL='Perf' E2E_PARALLEL=1 make test-e2e
```

### How the load generator works

Load is a **replay of real multi-turn agentic sessions**, not a synthetic prompt loop. The driver lives in [`utils/traces.go`](utils/traces.go):

1. **Fixture → sessions.** `LoadTraceSessions` reads a JSONL fixture (one row per LLM iteration) and groups rows by `session_id` into ordered `ReplaySession`s. Each turn's `input` is the full cumulative OpenAI messages array, so turn *N* is a prefix-superset of turn *N-1* — exactly the shared-prefix pattern the prefix-cache-scorer exploits. Turns shorter than one 16-token block (`BlockSizeTokens`) are dropped at load time, since they produce no prefix hashes.
2. **Concurrent replay.** `ReplaySessionsConcurrent` distributes sessions across a worker pool (`perfConcurrency`, default 8). Turns **within** a session are sent sequentially by one worker (so the shared prefix accumulates in the KV cache); **sessions** run in parallel (to saturate serving and fill the queue). All requests target the deployment name (`X-Gateway-Model-Name`), overriding the model recorded in the trace.
3. **Warm-up + measurement.** A warm-up pass (`perfWarmUpRounds`) primes the cache; then the fixture is replayed `perfMeasuredRounds` times while prefix-cache / success / error counters are snapshotted before and after. `repeatSessions` concatenates the fixture N times so a small fixture still generates sustained load.
4. **A/B check.** Shared-prefix load (repeated sessions) is compared against genuinely unique-prefix load (a per-request nonce prepended at block 0, single-turn) to prove cache-hit growth is real.

Total requests ≈ `sessions × turns × (warmUp + measured) rounds`, spread across `perfConcurrency` workers. The load constants are in [`prefix_cache_perf_test.go`](prefix_cache_perf_test.go): `perfConcurrency`, `perfMeasuredRounds`, `perfWarmUpRounds`, and the `prefixCacheHitRatioTarget` threshold.

### The trace fixture (and pulling down more)

The committed fixture [`testdata/agentic-traces.jsonl`](testdata/agentic-traces.jsonl) is only a **small trimmed slice** of the HuggingFace dataset [`sammshen/lmcache-agentic-traces`](https://huggingface.co/datasets/sammshen/lmcache-agentic-traces) (~2.36 GB). The full dataset is **never fetched at test time** — the test reads a local file only, keeping CI hermetic and network-independent.

To pull down more, regenerate the fixture **offline** with [`hack/e2e/scripts/extract_agentic_traces.py`](../../hack/e2e/scripts/extract_agentic_traces.py). It uses `datasets` streaming, so it downloads only what it needs and stops early:

```bash
pip install datasets
python hack/e2e/scripts/extract_agentic_traces.py \
  --num-sessions 40 \
  --max-turns 12 \
  --sources swebench gaia wildclaw \
  --output /tmp/agentic-traces-big.jsonl
```

| Flag | Effect | Default |
| --- | --- | --- |
| `--num-sessions` | distinct sessions kept (⇒ more prefix groups / parallelism) | `6` |
| `--max-turns` | earliest N turns kept per session (⇒ deeper shared prefix) | `4` |
| `--sources` | `session_id` prefixes to include, balanced round-robin | `swebench gaia wildclaw` |
| `--output` | JSONL output path | `test/e2e/testdata/agentic-traces.jsonl` |
| `--dataset` / `--split` | source dataset / split | the HF dataset / `train` |

Point the test at any fixture via the **`E2E_TRACE_FIXTURE`** env override — no recompile:

```bash
E2E_TRACE_FIXTURE=/tmp/agentic-traces-big.jsonl make test-e2e-perf
```

A bigger fixture raises the load automatically (more distinct sessions and deeper prefixes); increase `perfConcurrency` / `perfMeasuredRounds` to drive it harder still.

> **Goal: run against the full ~2.36 GB corpus.** The two-stage design (offline extract → local replay) is intended to scale up to the entire dataset. Because real sessions have a median ~21K input tokens, committing a full-size fixture would bloat the repo, so the path to "all traces" is: generate a large (or complete) fixture to a scratch path, then drive the perf spec at it via `E2E_TRACE_FIXTURE` (e.g. in a dedicated nightly/manual job) rather than checking the corpus into git. Raise `--num-sessions` / `--max-turns` toward the dataset's full session/turn counts, and scale `perfConcurrency` to keep the backend saturated.

## Adding a new e2e test

The suite enforces **one `Ordered` Describe per test file**, with deployments declared centrally. Follow these steps when adding a new component or scenario.

### 1. Decide whether you need a new case

- **Reuse an existing case** if your assertions only need an already-deployed model (e.g. another routing scenario over `routing-phi` / `routing-ministral`). Add a new `It` to the matching test file and skip to step 4.
- **Add a new case** if you need an isolated namespace, distinct preset combinations, or different replica counts. Continue with step 2.

### 2. Register the case in [`cases.go`](cases.go)

```go
const CaseMyFeature = "my-feature"

var CaseDeployments = map[string][]utils.ModelDeploymentValues{
    // ...existing entries...
    CaseMyFeature: {
        {
            Name:         "myfeature-phi",        // unique cluster-wide
            Namespace:    "e2e-my-feature",       // dedicated per-case namespace
            Model:        presetPhi,
            Replicas:     2,
            InstanceType: "Standard_NV36ads_A10_v5",
        },
    },
}
```

`InstallCase` / `UninstallCase` automatically pick up new entries — no other changes needed in `cases.go`.

### 3. Create the test file

```go
// test/e2e/myfeature_test.go
package e2e

import (
    "context"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"

    "github.com/kaito-project/production-stack/test/e2e/utils"
)

var _ = Describe("My Feature", Ordered, utils.GinkgoLabelMyFeature, func() {
    caseDeployments := CaseDeployments[CaseMyFeature]
    caseNamespace := CaseNamespace(CaseMyFeature)
    modelName := caseDeployments[0].Name

    var ctx context.Context
    var caseGatewayURL string

    BeforeAll(func() {
        ctx = context.Background()
        caseGatewayURL = InstallCase(CaseMyFeature)
    })

    AfterAll(func() {
        UninstallCase(CaseMyFeature)
    })

    It("routes inference traffic to the case gateway", func() {
        resp, err := utils.SendChatCompletion(caseGatewayURL, modelName)
        Expect(err).NotTo(HaveOccurred())
        defer resp.Body.Close()
        Expect(resp.StatusCode).To(Equal(200))
        _ = ctx           // available for K8s API calls
        _ = caseNamespace // available for kubectl-equivalent assertions
    })
})
```

Use `caseGatewayURL` for traffic that targets your case's deployments. The case namespace's catch-all and Gateway are provisioned by the modelharness chart in `EnsureNamespace`.

### 4. Add a Ginkgo label (only if no existing label fits)

Edit [`utils/ginkgo.go`](utils/ginkgo.go):

```go
var GinkgoLabelMyFeature = ginkgo.Label("MyFeature")
```

### 5. Add per-namespace resources (rare)

If your case needs additional cluster-side resources beyond what the [`charts/modelharness`](../../charts/modelharness) chart already provisions (Gateway, catch-all `model-not-found-direct` EnvoyFilter, optional `AuthorizationPolicy` + `APIKey`), add them as templates in `charts/modelharness` so every workload namespace picks them up consistently.

### 6. Validate

```bash
go build ./... && go vet ./test/e2e/...
go test -c -o /dev/null ./test/e2e/...
E2E_LABEL=MyFeature make test-e2e
```

## Directory layout

```
test/e2e/
├── e2e_test.go                       # Suite entry point, BeforeSuite/AfterSuite
├── cases.go                          # CaseDeployments + Install/UninstallCase
├── gpu_mocker_test.go                # CaseGPUMocker
├── model_routing_test.go             # CaseModelRouting
├── prefix_cache_routing_test.go      # CasePrefixCache
├── prefix_cache_perf_test.go         # CasePrefixCachePerf (Perf load/replay)
├── modeldeployment_chart_test.go     # CaseModelDeploymentChart (per-It ns)
├── testdata/
│   └── agentic-traces.jsonl          # trimmed replay fixture (see extract script)
├── production-stack-E2E-test-scenarios.md
├── README.md
└── utils/
    ├── cluster.go                    # K8s + dynamic client init
    ├── dynamic.go                    # GVK constants
    ├── ginkgo.go                     # Label definitions
    ├── helm.go                       # modeldeployment install/uninstall
    ├── http.go                       # Gateway port-forward + chat helpers
    ├── inference.go                  # InferenceSet readiness + snapshot/diff
    ├── metrics.go                    # EPP metrics scraping
    ├── traces.go                     # agentic-trace load generator (perf test)
    ├── setup.go                      # Namespace + per-case resource lifecycle
    └── utils.go                      # Misc helpers (env, logs, polling)
```
