# Production Stack

This project provides a reference implementation on how to build an inference stack on top of [Kaito](https://github.com/kaito-project/kaito).

## Architecture

![Production Stack Architecture](docs/imgs/production-stack-arch.png)

### Components

- **Istio Gateway** — Entry point for all inference requests. Routes client requests (e.g., `GET /completions`) through the stack.
- **Body-based Routing** — Parses request body to extract the model name and injects the `x-gateway-model-name` header, enabling model-level routing.
- **GAIE EPP (Gateway API Inference Extension Endpoint Picker)** — Performs KV-cache aware routing by injecting the `x-gateway-destination-endpoint` header, directing requests to the optimal inference pod.
- **Kaito InferenceSet** — Manages groups of vLLM inference pods. Multiple InferenceSets (e.g., Model-A, Model-B) can run different models simultaneously.
- **vLLM Inference Pods** — Serve model inference requests using [vLLM](https://github.com/vllm-project/vllm).
- **Kaito-Keda-Scaler** — Metric-based autoscaler built on [KEDA](https://keda.sh/) that scales vLLM inference pods up and down based on workload metrics.
- **Mocked GPU Nodes / CPU Nodes** — Infrastructure layer providing compute resources for inference workloads.

### Component Versions

All component versions are centralized in [`versions.env`](versions.env). This file is the single source of truth used by both CI and local E2E scripts.

| Component | Version | Variable |
|---|---|---|
| Go toolchain | 1.26.1 | `GO_VERSION` |
| Istio | 1.29.2 | `ISTIO_VERSION` |
| Gateway API CRDs | v1.2.0 | `GATEWAY_API_VERSION` |
| BBR (Body-Based Router) | v1.3.1 | `BBR_VERSION` |
| KEDA | v2.19.0 | `KEDA_VERSION` |
| KEDA Kaito Scaler | v0.4.1 | `KEDA_KAITO_SCALER_VERSION` |

> The E2E workflow installs KAITO via the published Helm chart
> (latest release on https://kaito-project.github.io/kaito/charts/kaito,
> no `--version` pin) and overrides the controller **image tag** to
> `nightly-latest` so the binary tracks main-branch HEAD. This is the
> pattern documented verbatim in the
> [KAITO nightly install guide](https://kaito-project.github.io/kaito/docs/installation/#using-nightly-builds-for-testing-purpose).

## Model Deployment Helm Chart

Per-deployment GAIE artifacts (`InferenceSet`, `InferencePool`, EPP
`Deployment` + `Service` + RBAC + `ConfigMap`, `HTTPRoute`) are provisioned
by the [`charts/modeldeployment`](charts/modeldeployment) Helm chart. The
chart is the single rendering unit used by the E2E suite and is the
recommended way to deploy a model on top of this stack.

The chart's `name` value is the deployment / Helm release identifier AND
the `X-Gateway-Model-Name` header value matched by the rendered HTTPRoute
(i.e. the value users send in the `model` field of OpenAI-style requests).
The chart's `model` value is the underlying KAITO preset
(`spec.template.inference.preset.name`). They are deliberately decoupled,
so a single namespace can host multiple deployments of the same preset
under distinct request keys.

### Prerequisites

The chart depends on the following cluster-level controllers and CRDs.
They must already be installed and healthy before the chart is rendered.
The install script
[`hack/e2e/scripts/install-components.sh`](hack/e2e/scripts/install-components.sh)
provisions all of them in the correct order for E2E.

| Required component                                                | Why the chart needs it                                                                                          |
| ----------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| **KAITO workspace controller** (with `enableInferenceSetController=true` and `gatewayAPIInferenceExtension=false`) | Reconciles the `kaito.sh/v1alpha1` `InferenceSet` rendered by the chart. The GAIE feature gate must be off so KAITO does not render a duplicate set of GAIE artifacts that would conflict with the chart's. |
| **KAITO `InferenceSet` CRD**                                      | API for the chart's `inferenceset.yaml` template.                                                               |
| **GPU node mocker** (`gpu-node-mocker`, E2E-only)                 | Creates fake GPU nodes + shadow pods so InferenceSets can be scheduled on a CPU-only test cluster. Not required in production. |
| **Gateway API CRDs** (≥ v1.2.0)                                   | API for the chart's `httproute.yaml` template (`gateway.networking.k8s.io/v1` `HTTPRoute`).                     |
| **GAIE CRDs** (`InferencePool`, `InferenceObjective`)             | API for the chart's `inferencepool.yaml` template (`inference.networking.k8s.io/v1` `InferencePool`).           |
| **Istio** (with `ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true`)    | Implements the Gateway and the EPP `ext_proc` filter chain referenced by the chart-rendered HTTPRoute.          |
| **BBR (Body-Based Router)**                                       | Injects the `X-Gateway-Model-Name` header from the request body's `model` field — the header the chart's HTTPRoute matches against. |
| **Inference Gateway** (a `gateway.networking.k8s.io/v1` `Gateway`)| Parent gateway that the chart's HTTPRoute attaches to via `parentRefs`. The chart defaults to `inference-gateway` (override with `--set gatewayName=...`). |
| **KEDA** + **KEDA Kaito Scaler** (optional)                       | Required only when the chart is rendered with `--set enableScaling=true` for autoscaling.                       |
| **Cluster-wide catch-all `HTTPRoute` + `model-not-found` service** (E2E-only) | Returns the OpenAI-compatible 404 for unknown models so model-specific routes win over the catch-all. Required by routing tests but not by the chart itself. |

See the [E2E test framework README](test/e2e/README.md) for the per-case
deployment table and the test-suite-specific helpers.

