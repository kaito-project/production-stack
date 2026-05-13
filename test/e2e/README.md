# E2E Test Framework

End-to-end test suite for production-stack, built with [Ginkgo v2](https://onsi.github.io/ginkgo/) + [Gomega](https://onsi.github.io/gomega/). Tests are organised as **per-case `Ordered` Describes** that run in parallel against an AKS cluster.

## Test cases

Single source of truth: [`cases.go`](cases.go) → `CaseDeployments`. Each entry carries `Name`, `Namespace`, `Model` (preset), `Replicas`, `InstanceType`. The Gateway name is derived as `<namespace>-gw` by both `charts/modelharness` and `charts/modeldeployment`.

| Case key | Test file | Namespace | Gateway | Deployments | Lifecycle |
| --- | --- | --- | --- | --- | --- |
| `CaseGPUMocker` | `gpu_mocker_test.go` | `e2e-gpu-mocker` | `e2e-gpu-mocker-gw` | `gpu-mocker-phi` | `BeforeAll` / `AfterAll` |
| `CaseModelRouting` | `model_routing_test.go` | `e2e-model-routing` | `e2e-model-routing-gw` | `routing-phi`, `routing-ministral` | `BeforeAll` / `AfterAll` |
| `CasePrefixCache` | `prefix_cache_routing_test.go` | `e2e-prefix-cache` | `e2e-prefix-cache-gw` | `prefix-cache-phi` (replicas ≥ 2) | `BeforeAll` / `AfterAll` |
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

Labels live in [`utils/ginkgo.go`](utils/ginkgo.go): `GinkgoLabelSmoke`, `GinkgoLabelRouting`, `GinkgoLabelPrefixCache`, `GinkgoLabelInfra`.

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
├── modeldeployment_chart_test.go     # CaseModelDeploymentChart (per-It ns)
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
    ├── setup.go                      # Namespace + per-case resource lifecycle
    └── utils.go                      # Misc helpers (env, logs, polling)
```
