# E2E Test Framework

This directory contains the end-to-end (e2e) test suite for the production-stack project, built with [Ginkgo v2](https://onsi.github.io/ginkgo/) and [Gomega](https://onsi.github.io/gomega/), following the same pattern as [kaito-project/kaito](https://github.com/kaito-project/kaito/tree/main/test/e2e).

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
