# E2E Test Framework

This directory contains the end-to-end (e2e) test suite for the production-stack project, built with [Ginkgo v2](https://onsi.github.io/ginkgo/) and [Gomega](https://onsi.github.io/gomega/), following the same pattern as [kaito-project/kaito](https://github.com/kaito-project/kaito/tree/main/test/e2e).

## Resource Management Strategy

E2E resources are split into two tiers based on scope and lifecycle:

### Install script ([`hack/e2e/scripts/install-components.sh`](../../hack/e2e/scripts/install-components.sh)) — Platform-level components

These are **shared infrastructure** installed once before any test runs (see
the prerequisites table above for the role of each):

1. KAITO workspace operator (Helm; image pinned to `nightly-latest`)
2. GPU node mocker (`gpu-node-mocker`, Helm)
3. Gateway API CRDs
4. Istio (minimal profile, GAIE pilot env enabled)
5. GAIE CRDs (`InferencePool`, `InferenceObjective`)
6. BBR (Body-Based Router)
7. Inference Gateway
8. Catch-all `HTTPRoute` + `model-not-found` error service + debug filter
9. KEDA
10. KEDA Kaito Scaler

### Test cases (`test/e2e/`) — Per-case model deployments

Per-deployment GAIE artifacts (`InferenceSet`, `InferencePool`, EPP
`Deployment` + `Service` + RBAC + `ConfigMap`, `HTTPRoute`) are provisioned
by the [`modeldeployment`](../../charts/modeldeployment) Helm chart. The
chart's EPP runs with `--secure-serving=false`, so the Istio Gateway can
reach it over plaintext gRPC and **no `DestinationRule` is required**.

The suite invokes the chart from Go via the `helm` CLI. Helpers live in
[`utils/helm.go`](utils/helm.go) and [`utils/inference.go`](utils/inference.go):

- `utils.SetupInferenceSetsWithRouting(deployments, namespace, gatewayURL)` —
  takes a `[]utils.ModelDeploymentValues`, installs the chart for each entry,
  and waits for the EPP / inference pods to be Running and the Gateway to
  return 200.
- `utils.CreateInferenceSetWithRouting(ctx, cl, values)` — installs the chart
  for a single per-test InferenceSet (used by `modeldeployment_chart_test.go`).
- `utils.CleanupInferenceSetWithRouting(ctx, cl, name, namespace)` — runs
  `helm uninstall` and removes every chart-rendered artifact.

The set of deployments owned by each test case is centralised in
[`cases.go`](cases.go) as
`CaseDeployments map[string][]utils.ModelDeploymentValues`. **Each case has
its own dedicated entry — deployments are NOT shared across cases.** The
suite-level cases (those whose deployments are installed once by
`BeforeSuite`) are enumerated by `AllSuiteDeployments`; lifecycle cases
install their deployment per-test in a fresh random namespace.

Helpers exposed by `cases.go`:

- `CaseDeploymentsWithNamespace(caseName, ns)` — returns a copy of the
  case's table entry with `Namespace` stamped to `ns`.
- `AllSuiteDeployments(ns)` — concatenates every suite-level case's
  deployments (in deterministic order); used by `BeforeSuite` /
  `AfterSuite`.

#### Per-case modeldeployment values

The table below mirrors [`CaseDeployments`](cases.go) — the single source of
truth for what the modeldeployment Helm chart is invoked with per case.
**`name` (the deployment / Helm release identifier) is unique across the
entire table and is decoupled from `model` (the inference preset).** The
HTTPRoute matches `name` against `X-Gateway-Model-Name` (i.e. the value
users send in the `model` field of OpenAI-style requests), so a single
namespace can host multiple deployments of the same preset under distinct
names.

| Case key                  | Test entry point                                 | `name` (deployment / request key) | Namespace                            | `model` (preset)          | `replicas` | `instanceType`            | `gatewayName`        | Lifecycle                                    |
| ------------------------- | ------------------------------------------------ | --------------------------------- | ------------------------------------ | ------------------------- | ---------- | ------------------------- | -------------------- | -------------------------------------------- |
| `CaseGPUMocker`           | `gpu_mocker_test.go`                             | `gpu-mocker-phi`                  | `default`                            | `phi-4-mini-instruct`     | `2`        | `Standard_NV36ads_A10_v5` | `inference-gateway`  | Suite-level (installed by `BeforeSuite`).    |
| `CaseGPUMocker`           | `gpu_mocker_test.go`                             | `gpu-mocker-ministral`            | `default`                            | `ministral-3-3b-instruct` | `2`        | `Standard_NV36ads_A10_v5` | `inference-gateway`  | Suite-level (installed by `BeforeSuite`).    |
| `CaseModelRouting`        | `model_routing_test.go`                          | `routing-phi`                     | `default`                            | `phi-4-mini-instruct`     | `2`        | `Standard_NV36ads_A10_v5` | `inference-gateway`  | Suite-level (installed by `BeforeSuite`).    |
| `CaseModelRouting`        | `model_routing_test.go`                          | `routing-ministral`               | `default`                            | `ministral-3-3b-instruct` | `2`        | `Standard_NV36ads_A10_v5` | `inference-gateway`  | Suite-level (installed by `BeforeSuite`).    |
| `CasePrefixCache`         | `prefix_cache_routing_test.go`                   | `prefix-cache-phi`                | `default`                            | `phi-4-mini-instruct`     | `2`        | `Standard_NV36ads_A10_v5` | `inference-gateway`  | Suite-level (installed by `BeforeSuite`).    |
| `CaseModelDeploymentChart` | `modeldeployment_chart_test.go`                 | `mdchart-phi`                     | `e2e-inferenceset-<rand>`            | `phi-4-mini-instruct`     | `2`        | `Standard_NV36ads_A10_v5` | `inference-gateway`  | Per-test; namespace recycled in `AfterEach`. Drives both the install/render and uninstall/delete `It` blocks. |

`name` ends up as the InferenceSet name, the prefix for the InferencePool /
EPP / HTTPRoute object names, the value of the
`inferenceset.kaito.sh/created-by` selector used by the InferencePool to
find shadow pods, AND the value of the `X-Gateway-Model-Name` header
matched by the HTTPRoute (i.e. the `model` field that user requests carry).
`model` ends up as `spec.template.inference.preset.name` only — it
identifies the underlying preset and is independent of the deployment
identity. Override the chart location with `MODELDEPLOYMENT_CHART=<path>`
if running tests outside the repository root.

#### Namespacing

The two tiers use different namespacing strategies:

- **Suite-level cases** (`CaseGPUMocker`, `CaseModelRouting`,
  `CasePrefixCache`) **share a single namespace** — `testNamespace`
  (currently `default`) — installed once by `BeforeSuite` and torn down by
  `AfterSuite`. Helm release names (`name` column above) are unique across
  the whole table so multiple deployments coexist there without collision.
- **Per-test cases** (`CaseModelDeploymentChart`) get a fresh
  random-suffixed namespace per `It`, created in `BeforeEach` and deleted
  in `AfterEach`. This is required because they exercise the chart's
  install/uninstall lifecycle and must not leave residue in the shared
  suite namespace.

## Directory Structure

```
test/e2e/
├── e2e_test.go                    # Suite entry point (TestE2E, Ginkgo bootstrap)
├── gpu_mocker_test.go             # Framework smoke tests
├── model_routing_test.go          # Model-based request routing tests
├── prefix_cache_routing_test.go   # Prefix/KV-cache aware routing tests
├── <component>_test.go            # Add new files per component
├── README.md                      # This file
└── utils/
    ├── cluster.go                 # Kubernetes client initialisation
    ├── utils.go                   # Shared helpers (wait, logs, config)
    └── ginkgo.go                  # Ginkgo label definitions
```

## Running Tests

### Smoke tests (no cluster required)

```bash
# Run all e2e tests (smoke tests work without a cluster)
make test-e2e

# Run only tests with a specific label
E2E_LABEL=Smoke make test-e2e
```

### Full e2e tests (requires a live cluster)

```bash
# Ensure KUBECONFIG is set or ~/.kube/config points to your cluster
export KUBECONFIG=/path/to/kubeconfig

# Run all e2e tests
make test-e2e

# Run only Routing tests
E2E_LABEL=Routing make test-e2e

# Run only PrefixCache tests
E2E_LABEL=PrefixCache make test-e2e

# Run with verbose Ginkgo output
go test -v -timeout 30m ./test/e2e/... --ginkgo.v
```

### Setting up a local E2E environment from scratch

This creates an AKS cluster, builds and pushes the gpu-node-mocker image, installs
all components (KAITO, Istio, BBR, Gateway, InferenceSets), and validates them.

**Prerequisites:**
- Azure CLI (`az`) logged in with a subscription that has quota for `Standard_D8s_v5` nodes (3 × 8 vCPU = 24 vCPU in the `standardDSv5Family`)
- Docker installed (for building the gpu-node-mocker image)
- `kubectl`, `helm`, `istioctl` available in PATH (or the setup script will install them)

**One-command setup:**

```bash
# Uses default names (kaito-e2e-local, swedencentral, 3 nodes)
make e2e-up
```

**With custom configuration:**

```bash
export RESOURCE_GROUP=my-e2e-rg
export CLUSTER_NAME=my-e2e-cluster
export LOCATION=westus2
export NODE_COUNT=3
export NODE_VM_SIZE=Standard_D8s_v5
make e2e-up
```

**After setup completes, run tests:**

```bash
make test-e2e

# Or run specific labels
make test-e2e E2E_LABEL=Smoke
make test-e2e E2E_LABEL=Infra
make test-e2e E2E_LABEL=Routing
make test-e2e E2E_LABEL=PrefixCache
```

**Tear down when done:**

```bash
make e2e-teardown
```

**Step-by-step (if you need more control):**

```bash
# 1. Build the gpu-node-mocker image
make docker-build

# 2. Create AKS cluster and ACR
make e2e-setup

# 3. Push image to ACR
make e2e-push-image

# 4. Install all components (KAITO, Istio, BBR, Gateway, InferenceSets)
SHADOW_CONTROLLER_IMAGE=<image-from-step-3> make e2e-install

# 5. Validate everything is healthy
make e2e-validate

# 6. Run tests
make test-e2e

# 7. (Optional) Dump cluster state for debugging
make e2e-dump

# 8. Tear down
make e2e-teardown
```

**Skip docker build (use default upstream image):**

If you don't need to test local gpu-node-mocker changes:

```bash
make e2e-setup
make e2e-install
make e2e-validate
make test-e2e
```

**Keep the cluster after tests (for debugging):**

```bash
SKIP_TEARDOWN=true make e2e
# Cluster stays running. Tear down later:
make e2e-teardown
```

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `E2E_LABEL` | Ginkgo label filter expression | _(all tests)_ |
| `GPU_MOCKER_NAMESPACE` | Namespace where gpu-node-mocker is deployed | `gpu-node-mocker-system` |
| `GPU_MOCKER_DEPLOYMENT` | Deployment name to check in BeforeSuite | _(skip check if empty)_ |

## Adding New Test Cases

### Step 1: Create a test file

Create a new file `test/e2e/<component>_test.go` in the `e2e` package:

```go
package e2e

import (
    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"

    "github.com/kaito-project/production-stack/test/e2e/utils"
)

var _ = Describe("My Component", utils.GinkgoLabelSmoke, func() {

    Context("when deployed", func() {
        It("should do something", func() {
            Expect(true).To(BeTrue())
        })
    })
})
```

### Step 2: Choose appropriate labels

Labels defined in `utils/ginkgo.go` control which tests run in different CI environments:

| Label | When to use |
|-------|-------------|
| `utils.GinkgoLabelSmoke` | Tests that run without a cluster (framework validation, unit-like checks) |
| `utils.GinkgoLabelRouting` | Tests that verify model-based request routing via BBR |
| `utils.GinkgoLabelPrefixCache` | Tests that verify prefix/KV-cache aware routing via EPP |

You can combine labels: `Describe("...", utils.GinkgoLabelRouting, utils.GinkgoLabelSmoke, func() {...})`

To add a new label, edit `utils/ginkgo.go`:

```go
var GinkgoLabelMyFeature = g.Label("MyFeature")
```

### Step 3: Use shared utilities

The `utils/` package provides common helpers:

```go
// Kubernetes client (initialised in BeforeSuite)
utils.TestingCluster.KubeClient

// Wait for a pod to be ready
utils.WaitForPodReady(ctx, clientset, namespace, podName, utils.PollTimeout)

// Print pod logs on failure (used in ReportAfterSuite)
utils.PrintPodLogsOnFailure(namespace, "app=my-app")

// Environment variables
utils.GetEnv("MY_VAR")
```

### Step 4: Test lifecycle pattern

For tests that create and clean up Kubernetes resources:

```go
var _ = Describe("My Feature", func() {
    var resourceName string

    BeforeEach(func() {
        resourceName = "test-" + utils.E2eNamespace
        // Create test resources
    })

    AfterEach(func() {
        // Clean up test resources
    })

    It("should work correctly", func() {
        // Test logic with Eventually/Consistently
        Eventually(func() error {
            // poll condition
            return nil
        }, utils.PollTimeout, utils.PollInterval).Should(Succeed())
    })
})
```

### Step 5: Verify locally

```bash
# Compile check
go build ./test/e2e/...

# Run your new tests
go test -v -count=1 ./test/e2e/... --ginkgo.label-filter="MyFeature" --ginkgo.v
```
