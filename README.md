# Production Stack

This project evaluates a production inference stack built on top of existing OSS projects. The stack is designed based on the [**llm-d**](https://github.com/llm-d/llm-d) reference stack. Credits go to llm-d contributors for the reference architecture and the contribution of several core components, such as the EPP. In this stack, KAITO is the inference engine, and we focus on evaluating the request routing and autoscaling performance. We run the [vLLM simulator](https://github.com/llm-d/llm-d-inference-sim) so that the entire stack can be evaluated using CPUs only.

## Architecture

![Production Stack Architecture](docs/imgs/production-stack-arch.png)

### Components

- **[Istio Gateway](https://istio.io/latest/docs/tasks/traffic-management/ingress/gateway-api/)** — Entry point for all inference requests. Routes client requests (e.g., `POST /v1/chat/completions`) through the stack.
- **[llm-gateway-auth](https://github.com/kaito-project/llm-gateway-auth)** — ext_authz API-key authorization filter. Validates the `Authorization: Bearer <token>` header against an `APIKey` custom resource resolved from the request's `Host` subdomain (`<namespace>.gw.example.com`) before any routing or model dispatch happens. Ships two components — `apikey-operator` (reconciles `APIKey` CRs into per-namespace Secrets) and `apikey-authz` (the ext_authz dataplane).
- **[body-based routing (BBR)](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/pkg/bbr/README.md)** — Parses request body to extract the model name and injects the `X-Gateway-Model-Name` header, enabling model-level routing.
- **[llm-d-inference-scheduler (EPP)](https://github.com/llm-d/llm-d-inference-scheduler)** — Per-model Endpoint Picker (image `mcr.microsoft.com/oss/v2/llm-d/llm-d-inference-scheduler`). Performs KV-cache aware routing by injecting the `x-gateway-destination-endpoint` header, directing requests to the optimal inference pod.
- **[Kaito InferenceSet](https://github.com/kaito-project/kaito)** — Manages groups of vLLM inference pods. Multiple InferenceSets (e.g., Model-A, Model-B) can run different models simultaneously.
- **[vLLM Inference Pods(llm-d-inference-sim)](https://github.com/llm-d/llm-d-inference-sim)** — Serve model inference requests. On CPU-only E2E clusters, the real vLLM container is replaced by a **shadow pod** running llm-d-inference-sim (image `ghcr.io/llm-d/llm-d-inference-sim`), a lightweight vLLM-compatible simulator that exposes the same OpenAI API and `vllm:*` Prometheus metrics. See [`pkg/gpu-node-mocker/README.md`](pkg/gpu-node-mocker/README.md) for the original-pod ↔ shadow-pod mechanism.
- **[keda-kaito-scaler](https://github.com/kaito-project/keda-kaito-scaler)** — Metric-based autoscaler built on [KEDA](https://keda.sh/) that scales vLLM inference pods up and down based on workload metrics.
- **[Mocked GPU Nodes](https://github.com/kaito-project/production-stack/blob/main/pkg/gpu-node-mocker/README.md) / CPU Nodes** — Infrastructure layer providing compute resources for inference workloads. The `gpu-node-mocker` controller (E2E-only) fakes GPU nodes on CPU-only clusters and runs the `llm-d-inference-sim` shadow pods on real CPU nodes.

### Request and Scaling Flows

#### Prompt flow

1. **Client → Istio Gateway.** Client sends `POST /v1/chat/completions` to `<namespace>.gw.example.com` with a bearer token.
2. **Gateway → ext-proc filters.** `llm-gateway-auth` validates the token; BBR parses the body and injects `X-Gateway-Model-Name`.
3. **Gateway → EPP.** The per-deployment `HTTPRoute` matches the model name and calls `llm-d-inference-scheduler`, which returns the target pod via `x-gateway-destination-endpoint`.
4. **Gateway → vLLM Pod.** Envoy forwards the request directly to the chosen inference pod; the response streams back along the reverse path.
5. **Unmatched models.** The namespace's catch-all `HTTPRoute` returns an OpenAI-compatible `404 model_not_found` from the cluster-shared `default/model-not-found` Service.

#### Scaling flow

1. **vLLM pods → metrics.** Each pod exposes `vllm:*` Prometheus metrics (queue depth, KV-cache utilisation, request rate).
2. **keda-kaito-scaler → KEDA.** The external scaler aggregates per-`InferenceSet` pod metrics and returns a single summed metric value.
3. **KEDA → HPA → InferenceSet.** KEDA exposes that value through the external metrics API; the HPA computes the desired replica count from it and patches the `InferenceSet`, and the KAITO controller adds or removes vLLM pods.

## Installation

Install the stack in three steps. Step 1 is one-time per cluster;
steps 2 and 3 are repeated per workload namespace and per model.

### Step 1. Cluster + addons (one-time, cluster-wide)

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
| `llm-gateway-auth` ([`kaito-project/llm-gateway-auth`](https://github.com/kaito-project/llm-gateway-auth)) | `llm-gateway-auth` | `LLM_GATEWAY_AUTH_VERSION` | helm           | API-key ext_authz for the `inference-gateway`. Installs the `APIKey` CRD, the `apikey-operator` (reconciles `APIKey` → per-namespace Secret), and the `apikey-authz` ext_authz dataplane wired into Istio via `MeshConfig` + `AuthorizationPolicy`. |
| KEDA + KEDA Kaito Scaler ([`kaito-project/keda-kaito-scaler`](https://github.com/kaito-project/keda-kaito-scaler), optional)  | `keda` | `KEDA_VERSION` (v2.19.0), `KEDA_KAITO_SCALER_VERSION` (v0.4.1) | helm | Workload-metric autoscaling.                                                    |
| `model-not-found` (Deployment + ConfigMap + Service) | `default` | repo `HEAD` ([`hack/e2e/manifests/model-not-found.yaml`](hack/e2e/manifests/model-not-found.yaml)) | kubectl | Cluster-shared nginx-backed Service that returns OpenAI-compatible `404 model_not_found` JSON. Referenced cross-namespace by every workload namespace's catch-all `HTTPRoute` (authorised via a `ReferenceGrant` rendered by `charts/modelharness`). |

### Step 2. modelharness (one-time per workload namespace)

Provisioned by the [`charts/modelharness`](charts/modelharness) Helm
chart. One Helm release per workload namespace owns every per-namespace
shared resource — the Istio `Gateway` that fronts the namespace, the
catch-all `HTTPRoute` (forwards unknown-model requests to the
cluster-shared `default/model-not-found` Service), the
`ReferenceGrant` authorising that cross-namespace `backendRef`, and —
when API-key auth is enabled — the per-namespace `AuthorizationPolicy`
+ `APIKey` CR that wire that Gateway into the cluster-wide
`apikey-ext-authz` CUSTOM provider. A namespace may host one or more
model deployments, all of which share its Gateway:

| Resource                                                       | Where                     | Version                | Source                                | Role                                                                                                                                       |
| -------------------------------------------------------------- | ------------------------- | ---------------------- | ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `Gateway` (`gateway.networking.k8s.io/v1`)                     | Per namespace             | API `v1`               | `charts/modelharness`                 | Public entry point; `gatewayClassName: istio`, HTTP/80.                                                                                    |
| Catch-all `HTTPRoute` `model-not-found-route`                  | Per namespace             | API `v1`               | `charts/modelharness`                 | Forwards unmatched paths on the namespace's Gateway to the cluster-shared `default/model-not-found` Service via a cross-namespace `backendRef`. |
| `ReferenceGrant` `allow-model-not-found-from-<ns>`             | `default`                 | API `v1beta1`          | `charts/modelharness`                 | Authorises the per-namespace catch-all `HTTPRoute` to reference `default/model-not-found` across namespaces.                                |
| `AuthorizationPolicy` `apikey-gateway-ext-authz` (auth-enabled) | Per namespace             | `security.istio.io/v1` | `charts/modelharness` (`auth.enabled`) | Wires the per-namespace Gateway pod into the cluster-wide `apikey-ext-authz` CUSTOM provider (registered in `MeshConfig` by `llm-gateway-auth`). |
| `APIKey` `default` (auth-enabled)                              | Per namespace             | `apikeys.kaito.sh/v1alpha1` | `charts/modelharness` (`auth.enabled`) | Triggers the `apikey-operator` to reconcile a Secret (`llm-api-key`) holding the bearer token clients send.                                 |

In the E2E suite the chart is installed and uninstalled by
[`EnsureNamespace` / `DeleteNamespace`](test/e2e/utils/setup.go) (called
from `InstallCase` / `UninstallCase` in
[`cases.go`](test/e2e/cases.go)). `helm uninstall modelharness` cleans
up the cross-namespace `ReferenceGrant` automatically. The two
auth-related resources are skipped when `auth.enabled=false`.

### Step 3. modeldeployment (per model deployment)

Provisioned by the [`charts/modeldeployment`](charts/modeldeployment) Helm chart. One Helm release
per model deployment, parented to the namespace's `Gateway`:

| Resource                                         | Version (chart-rendered) | Install method | Role                                                                  |
| ------------------------------------------------ | ------------------------ | -------------- | --------------------------------------------------------------------- |
| `InferenceSet` (`kaito.sh/v1alpha1`)             | `v1alpha1`               | helm           | Reconciled by KAITO; renders inference pods running vLLM.             |
| `InferencePool` (`inference.networking.k8s.io/v1`) | `v1`                   | helm           | Selects the inference pods backing this deployment.                   |
| EPP `Deployment` + `Service` + RBAC + `ConfigMap` | `apps/v1`, `v1`, `rbac/v1` | helm         | Endpoint Picker (`llm-d-inference-scheduler`) for KV-cache aware routing. |
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
| `InferenceSet` | `kaito.sh/v1alpha1` | KAITO | Declares one model deployment; KAITO renders inference pods. |cluster-shared `default/model-not-found` for an OpenAI 404. |
| `ReferenceGrant` | `gateway.networking.k8s.io/v1beta1` | Kubernetes Gateway API | Authorises each workload namespace's catch-all `HTTPRoute` to reference the cluster-shared `default/model-not-found` Service
| `InferencePool` | `inference.networking.k8s.io/v1` | Gateway API Inference Extension (GAIE) | GAIE pool selecting the inference pods backing a deployment. |
| `InferenceObjective` | `inference.networking.k8s.io/v1` | Gateway API Inference Extension (GAIE) | API object defining objective contracts; CRD only — not authored by this stack. |
| `APIKey` | `apikeys.kaito.sh/v1alpha1` | [`kaito-project/llm-gateway-auth`](https://github.com/kaito-project/llm-gateway-auth) | Declares an API key for a gateway namespace; the `apikey-operator` reconciles it into a `Secret` (`llm-api-key` by default) consumed by the `apikey-authz` ext_authz filter. |
| `Gateway` | `gateway.networking.k8s.io/v1` | Kubernetes Gateway API | Per-namespace public entry point; `gatewayClassName: istio`, HTTP/80. |
| `HTTPRoute` | `gateway.networking.k8s.io/v1` | Kubernetes Gateway API | Model-specific routes match `X-Gateway-Model-Name == <name>` → InferencePool; per-namespace catch-all routes unmatched paths to the namespace-local `model-not-found` for an OpenAI 404. |
| `EnvoyFilter` | `networking.istio.io/v1alpha3` | Istio | BBR injects ext_proc into every Istio Gateway via rootNamespace. |
| `AuthorizationPolicy` | `security.istio.io/v1` | Istio (rendered by `llm-gateway-auth`) | Targets the `inference-gateway` Pod and routes ext_authz to the `apikey-authz` provider so every request must carry a valid `APIKey`-derived bearer token. |

## Testing

The E2E suite under [`test/e2e/`](test/e2e) exercises the full stack
(Gateway → llm-gateway-auth (ext_authz) → BBR → EPP
(`llm-d-inference-scheduler`) → vLLM shadow pod
(`llm-d-inference-sim`)) against a live AKS cluster. Tests run as
parallel `Ordered` Ginkgo Describes, one per case namespace.

See [`test/e2e/README.md`](test/e2e/README.md) for the full framework
guide, helper API, and the
[**Adding a new e2e test**](test/e2e/README.md#adding-a-new-e2e-test)
workflow.

## License

Production Stack is licensed under the [Apache License 2.0](LICENSE).


