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

## Resource Layering

Production Stack resources are organised into three tiers by scope and
lifecycle. Operators provision the lower tiers once per cluster; users
provision a model deployment per workload.

### 1. Cluster tier (one-time, cluster-wide)

Installed by [`hack/e2e/scripts/install-components.sh`](hack/e2e/scripts/install-components.sh)
(or its production equivalent). These components live across multiple
namespaces and are shared by every model deployment:

| Component                            | Namespace        | Version (`versions.env`)                | Install method | Role                                                                                  |
| ------------------------------------ | ---------------- | --------------------------------------- | -------------- | ------------------------------------------------------------------------------------- |
| KAITO workspace controller           | `kaito-system`   | latest chart, image `nightly-latest`    | helm           | Reconciles `InferenceSet` and provisions inference pods.                              |
| `gpu-node-mocker` (E2E-only)         | `kaito-system`   | repo `HEAD` (`SHADOW_CONTROLLER_IMAGE`) | helm           | Creates fake GPU nodes + shadow pods on CPU-only clusters.                            |
| Gateway API CRDs                     | _cluster-scoped_ | `GATEWAY_API_VERSION` (v1.2.0)          | kubectl        | Required for `Gateway`, `HTTPRoute`, `ReferenceGrant`.                                |
| Istio control plane (`istiod`)       | `istio-system`   | `ISTIO_VERSION` (1.29.2)                | istioctl       | Implements the Gateway dataplane (Envoy) and ext_proc filter chain.                   |
| GAIE CRDs                            | _cluster-scoped_ | latest                                  | kubectl        | `InferencePool`, `InferenceObjective`.                                                |
| BBR (Body-Based Router)              | `istio-system`   | `BBR_VERSION` (v1.3.1)                  | helm           | Installed in Istio's rootNamespace so its EnvoyFilter applies cluster-wide; injects `X-Gateway-Model-Name`. |
| KEDA + KEDA Kaito Scaler (optional)  | `keda`, `kaito-system` | `KEDA_VERSION` (v2.19.0), `KEDA_KAITO_SCALER_VERSION` (v0.4.1) | helm | Workload-metric autoscaling.                                                    |

### 2. Namespace tier (one-time per environment / tenant)

These resources scope traffic into a Gateway dataplane. A namespace may
host one or more model deployments that all share its Gateway:

| Resource                              | Where                | Version       | Install method                       | Role                                                                          |
| ------------------------------------- | -------------------- | ------------- | ------------------------------------ | ----------------------------------------------------------------------------- |
| `Gateway` (`gateway.networking.k8s.io/v1`) | Per namespace   | API `v1`      | `provisionNamespaceResources` (E2E)  | Public entry point; `gatewayClassName: istio`, HTTP/80.                       |
| Catch-all `HTTPRoute` + `ReferenceGrant`   | Per namespace + `default` | API `v1` / `v1beta1` | `provisionNamespaceResources` (E2E)  | Routes unmatched paths to the shared `default/model-not-found` Service so unknown models receive an OpenAI-compatible 404. |

In the E2E suite these are provisioned by a single function,
[`provisionNamespaceResources`](test/e2e/utils/setup.go) — extend it
when a new per-namespace artifact is introduced.

### 3. Workload tier (per model deployment)

Provisioned by the `charts/modeldeployment` Helm chart. One Helm release
per model deployment, parented to the namespace's `Gateway`:

| Resource                                         | Version (chart-rendered) | Install method | Role                                                                  |
| ------------------------------------------------ | ------------------------ | -------------- | --------------------------------------------------------------------- |
| `InferenceSet` (`kaito.sh/v1alpha1`)             | `v1alpha1`               | helm           | Reconciled by KAITO; renders inference pods running vLLM.             |
| `InferencePool` (`inference.networking.k8s.io/v1`) | `v1`                   | helm           | Selects the inference pods backing this deployment.                   |
| EPP `Deployment` + `Service` + RBAC + `ConfigMap` | `apps/v1`, `v1`, `rbac/v1` | helm         | Endpoint Picker for KV-cache aware routing.                           |
| `HTTPRoute` (`gateway.networking.k8s.io/v1`)     | `v1`                     | helm           | Matches `X-Gateway-Model-Name == <name>` on the namespace's Gateway and forwards to the InferencePool. |

The chart's `name` value is the per-deployment routing key; `model` is
the underlying KAITO preset. See the
[`charts/modeldeployment` chart README](charts/modeldeployment/README.md)
for the full value schema and install examples.

## Resource Reference

A flat index of the **CRD-backed** resources Production Stack creates,
grouped by the controller / chart that owns it. Kubernetes native objects
(`Deployment`, `Service`, `ConfigMap`, `ServiceAccount`, `Role` /
`RoleBinding`, `Pod`, `Node`, …) are intentionally omitted — they are
implementation details of the charts above and are not listed here.

| Resource (Kind) | Group / Version | Source | Purpose |
| --- | --- | --- | --- |
| `Workspace` | `kaito.sh/v1alpha1` | KAITO | Aggregates inference workloads (used indirectly via `InferenceSet`). |
| `InferenceSet` | `kaito.sh/v1alpha1` | KAITO | Declares one model deployment; KAITO renders inference pods. |
| `InferencePool` | `inference.networking.k8s.io/v1` | Gateway API Inference Extension (GAIE) | GAIE pool selecting the inference pods backing a deployment. |
| `InferenceObjective` | `inference.networking.k8s.io/v1` | Gateway API Inference Extension (GAIE) | API object defining objective contracts; CRD only — not authored by this stack. |
| `Gateway` | `gateway.networking.k8s.io/v1` | Kubernetes Gateway API | Per-namespace public entry point; `gatewayClassName: istio`, HTTP/80. |
| `HTTPRoute` | `gateway.networking.k8s.io/v1` | Kubernetes Gateway API | Model-specific routes match `X-Gateway-Model-Name == <name>` → InferencePool; catch-all routes unmatched paths to `default/model-not-found` for an OpenAI 404. |
| `ReferenceGrant` | `gateway.networking.k8s.io/v1beta1` | Kubernetes Gateway API | Authorises the per-namespace catch-all `HTTPRoute` to reference `default/model-not-found`. |
| `EnvoyFilter` | `networking.istio.io/v1alpha3` | Istio | BBR injects ext_proc into every Istio Gateway via rootNamespace; debug filter adds a Lua `PRE-BBR` / `POST-EPP` / `RESPONSE` log chain for E2E. |
| `DestinationRule` | `networking.istio.io/v1` | Istio | Configures mTLS / load balancing for the BBR ext_proc Service. |

## Testing

The E2E suite under [`test/e2e/`](test/e2e) exercises the full stack
(Gateway → BBR → EPP → vLLM) against a live cluster. Run it via
`make test-e2e` after a cluster is up.

To add a new test case, follow
[**Adding a new e2e test**](test/e2e/README.md#adding-a-new-e2e-test) in
the E2E framework README.


