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
5. **Unmatched models.** The namespace's `model-not-found-direct` `EnvoyFilter` (rendered by `charts/modelharness`) patches a catch-all `direct_response` onto the Gateway's HCM, returning an OpenAI-compatible `404 model_not_found` directly from Envoy with no backend Pod / Service / `ReferenceGrant`. The catch-all is also required to keep API-key ext_authz running on unknown-model requests (Istio's CUSTOM `AuthorizationPolicy` is gated on metadata written during route matching).

#### Scaling flow

1. **vLLM pods → metrics.** Each pod exposes `vllm:*` Prometheus metrics (queue depth, KV-cache utilisation, request rate).
2. **keda-kaito-scaler → KEDA.** The external scaler aggregates per-`InferenceSet` pod metrics and returns a single summed metric value.
3. **KEDA → HPA → InferenceSet.** KEDA exposes that value through the external metrics API; the HPA computes the desired replica count from it and patches the `InferenceSet`, and the KAITO controller adds or removes vLLM pods.

## Installation

Install the stack in three steps. Step 1 is one-time per cluster; steps 2 and 3 are repeated per workload namespace and per model.

All three releasable charts are published as OCI artifacts on every release; pick whichever installation source matches your environment:

| Source       | Reference                                                                          |
| ------------ | ---------------------------------------------------------------------------------- |
| OCI (GHCR)   | `oci://ghcr.io/kaito-project/{productionstack,modelharness,modeldeployment}`       |
| OCI (MCR)    | `oci://mcr.microsoft.com/aks/kaito/helm/{productionstack,modelharness,modeldeployment}` |
| Helm repo    | `helm repo add production-stack https://kaito-project.github.io/production-stack/charts/kaito-project` |
| In-tree      | `./charts/{productionstack,modelharness,modeldeployment}`                          |

The OCI commands below assume `ghcr.io`; swap the registry to suit. Pin `--version <X.Y.Z>` to a specific release tag — see the latest release on [GitHub Releases](https://github.com/kaito-project/production-stack/releases).

### Step 1. Cluster + addons (one-time, cluster-wide)

Install the `productionstack` umbrella chart, plus any external prerequisites your environment does not already provide (Gateway API CRDs, Istio, GAIE CRDs, KEDA, KAITO). The umbrella chart bundles the cluster-level singletons every model deployment shares: BBR data plane, KEDA Kaito scaler, and the `llm-gateway-auth` control plane.

```sh
helm install productionstack oci://ghcr.io/kaito-project/productionstack \
  --version <X.Y.Z> \
  --namespace kube-system \
  --create-namespace
```

With the default values the umbrella release and all the cluster-level
singletons it bundles — BBR data plane, KEDA Kaito scaler, and the
`llm-gateway-auth` control plane — install into `kube-system`.

The full list of prerequisites the E2E suite installs alongside the umbrella chart is in [`hack/e2e/scripts/install-components.sh`](hack/e2e/scripts/install-components.sh). For the bundled subcharts, toggles, and per-subchart values, see [`charts/productionstack/README.md`](charts/productionstack/README.md).

### Step 2. modelharness (one-time per workload namespace)

One Helm release per workload namespace provisions every per-namespace shared resource: the Istio `Gateway`, the BBR / catch-all 404 / unified-error `EnvoyFilter`s, and — when enabled — the API-key auth artifacts and a Cilium-based East-West network policy.

```sh
helm install modelharness oci://ghcr.io/kaito-project/modelharness \
  --version <X.Y.Z> \
  --namespace my-models \
  --create-namespace
```

For the rendered resource list, the API-key auth / network-policy toggles, the Cilium dataplane requirement, and the full value reference, see [`charts/modelharness/README.md`](charts/modelharness/README.md).

### Step 3. modeldeployment (per model deployment)

One Helm release per model deployment, parented to the namespace's `Gateway`. Renders the `InferenceSet`, `InferencePool`, EPP (`llm-d-inference-scheduler`) Deployment / Service / RBAC / ConfigMap, and the `HTTPRoute` that matches `X-Gateway-Model-Name: <name>`.

```sh
helm install qwen oci://ghcr.io/kaito-project/modeldeployment \
  --version <X.Y.Z> \
  --namespace my-models \
  --set name=qwen \
  --set model=qwen2-5-coder-7b-instruct \
  --set replicas=2 \
  --set maxReplicas=5 \
  --set enableScaling=true \
  --set scalingThreshold=10
```

For the full value schema (EPP image / ports / resources, scaling knobs, naming conventions, KAITO `FeatureFlagGatewayAPIInferenceExtension` compatibility note), see [`charts/modeldeployment/README.md`](charts/modeldeployment/README.md).

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
| `APIKey` | `apikeys.kaito.sh/v1alpha1` | [`kaito-project/llm-gateway-auth`](https://github.com/kaito-project/llm-gateway-auth) | Declares an API key for a gateway namespace; the `apikey-operator` reconciles it into a `Secret` (`llm-api-key` by default) consumed by the `apikey-authz` ext_authz filter. |
| `Gateway` | `gateway.networking.k8s.io/v1` | Kubernetes Gateway API | Per-namespace public entry point; `gatewayClassName: istio`, HTTP/80. |
| `HTTPRoute` | `gateway.networking.k8s.io/v1` | Kubernetes Gateway API | Per-deployment routes match `X-Gateway-Model-Name == <name>` and forward to the deployment's `InferencePool`. |
| `EnvoyFilter` | `networking.istio.io/v1alpha3` | Istio | `charts/modelharness` renders the per-namespace `bbr-ext-proc` filter (injects BBR's ext_proc into the namespace Gateway's HCM, scoped via `workloadSelector`), the `model-not-found-direct` filter (OpenAI 404 `direct_response` catch-all for unknown models), and the `gateway-filter-outage-local-reply` filter (the single namespace-scoped `local_reply` that intercepts every gateway-generated `>= 500` local reply and maps fail-closed BBR/ext_authz outages, gateway data-plane health failures, and any remaining 5xx onto the unified error envelope). |
| `AuthorizationPolicy` | `security.istio.io/v1` | Istio (rendered by `llm-gateway-auth` + `charts/modelharness`) | `llm-gateway-auth` targets the cluster-wide `inference-gateway`; `charts/modelharness` renders a per-namespace `apikey-gateway-ext-authz` policy that wires each workload namespace's Gateway pod into the `apikey-ext-authz` CUSTOM provider. |
| `CiliumNetworkPolicy` | `cilium.io/v2` | Cilium (rendered by `charts/modelharness`) | Per-namespace `inference-pods-ingress` CNP that locks down East-West ingress to pods labelled `kaito.sh/owned-by: modeldeployment`. Requires the cluster to run the Cilium dataplane (AKS: `--network-dataplane cilium --network-policy cilium`). Native `cilium.io/v2` resources are used instead of `networking.k8s.io/v1` `NetworkPolicy` to avoid an AKS-Cilium translation race that leaves policies un-enforcing on pre-existing endpoints. |

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

## Release Process

Releases are driven by **a single manual GitHub Actions workflow**,
[`Create release (manually)`](.github/workflows/create-release.yaml), which
chains two reusable workflows
([`publish-image.yaml`](.github/workflows/publish-image.yaml) and
[`publish-helm-chart.yaml`](.github/workflows/publish-helm-chart.yaml))
into one synchronous run. A release publishes:

- a multi-arch container image at `ghcr.io/kaito-project/gpu-node-mocker:<X.Y.Z>` (no leading `v`);
- the four Helm charts under [`charts/`](charts/) — `gpu-node-mocker`, `modeldeployment`, `modelharness`, and the `productionstack` umbrella chart — published to **all three** chart distribution channels:
  - the gh-pages Helm repo `https://kaito-project.github.io/production-stack/charts/kaito-project`,
  - OCI artifacts under `oci://ghcr.io/kaito-project/<chart>`,
  - OCI artifacts under `oci://mcr.microsoft.com/aks/kaito/helm/<chart>`;
  the `productionstack` chart has its OCI subchart dependency vendored via `helm dependency update` before packaging.
- a GitHub Release at the same `vX.Y.Z` tag with auto-generated changelog notes.

To publish `vX.Y.Z`:

1. **Open a PR against `main`** that bumps the chart versions for any chart
   whose contents changed in this release. For each touched chart in
   [`charts/`](charts/), update its `Chart.yaml` (`version` and `appVersion`).
   When `charts/gpu-node-mocker` ships a new mocker image, also update its
   `values.yaml` `image.tag`. A typical bump touches:
   - [`charts/gpu-node-mocker/Chart.yaml`](charts/gpu-node-mocker/Chart.yaml) and
     [`charts/gpu-node-mocker/values.yaml`](charts/gpu-node-mocker/values.yaml)
   - [`charts/modeldeployment/Chart.yaml`](charts/modeldeployment/Chart.yaml)
   - [`charts/modelharness/Chart.yaml`](charts/modelharness/Chart.yaml)
   - [`charts/productionstack/Chart.yaml`](charts/productionstack/Chart.yaml)

2. **After the PR is merged**, run **Actions → "Create release (manually)"**
   with `release_version=vX.Y.Z`.

Notes:

- Use the same `vX.Y.Z` value across all jobs. Git tags / Release names
  carry the leading `v`; container image tags do not (`X.Y.Z`).
- Each Helm chart is published with the version declared in its own
  `Chart.yaml`; the workflow does not rewrite chart versions. Always bump
  charts you intend to ship in step 1.
- If a job fails part-way through (e.g. Trivy finds a new CVE), fix the
  underlying issue and rerun the workflow — `publish-image` and
  `create-gh-release` are idempotent (they skip tag/release creation when
  they already exist).
- When publishing patch releases on an older minor while `main` has moved
  on, cut a `release-vX.Y` branch (e.g. `release-v0.3`) and run
  "Create release (manually)" against that branch's tag.

## License

Production Stack is licensed under the [Apache License 2.0](LICENSE).


