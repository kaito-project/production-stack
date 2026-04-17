# E2E Test Framework

This directory contains the end-to-end (e2e) test suite for the production-stack project, built with [Ginkgo v2](https://onsi.github.io/ginkgo/) and [Gomega](https://onsi.github.io/gomega/), following the same pattern as [kaito-project/kaito](https://github.com/kaito-project/kaito/tree/main/test/e2e).

## Resource Management Strategy

E2E resources are split into two tiers based on scope and lifecycle:

### Install script (`hack/e2e/scripts/install-components.sh`) — Platform-level components

These are **shared infrastructure** installed once before all tests run:

- KAITO workspace operator
- GPU node mocker (gpu-node-mocker)
- Gateway API CRDs
- Istio (minimal profile)
- GWIE CRDs (InferencePool, InferenceModel)
- BBR (Body-Based Router)
- Inference Gateway
- Catch-all HTTPRoute + model-not-found error service + debug filter

### Test cases (`test/e2e/`) — Model-level resources

These are created and cleaned up **per test case** using helpers in `test/e2e/utils/`:

- **InferenceSet** — created via `utils.CreateInferenceSet()` or `utils.CreateInferenceSetWithRouting()`
- **Model-specific HTTPRoute** — created via `utils.CreateHTTPRouteForInferenceSet()`, routes model requests to the corresponding InferencePool via the shared Gateway
- **DestinationRule** — created via `utils.CreateDestinationRuleForInferenceSet()`, configures TLS for the EPP service

Each test context uses a **unique namespace** (with a random suffix) and cleans up all resources in `AfterEach` via `utils.CleanupInferenceSetWithRouting()`.

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
- Azure CLI (`az`) logged in with a subscription that has quota for `Standard_D4s_v3` nodes
- Docker installed (for building the gpu-node-mocker image)
- `kubectl`, `helm`, `istioctl` available in PATH (or the setup script will install them)

**One-command setup:**

```bash
# Uses default names (kaito-e2e-local, swedencentral, 2 nodes)
make e2e-up
```

**With custom configuration:**

```bash
export RESOURCE_GROUP=my-e2e-rg
export CLUSTER_NAME=my-e2e-cluster
export LOCATION=westus2
export NODE_COUNT=3
export NODE_VM_SIZE=Standard_D4s_v3
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
