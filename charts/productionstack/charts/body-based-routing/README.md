# body-based-routing

Helm chart that installs a **cluster-wide singleton** Body-Based Router
(BBR) for the Gateway API Inference Extension. BBR runs as a single
shared `ext_proc` service in `istio-system`; every Istio-managed
inference Gateway in the cluster — including per-namespace Gateways
provisioned by the `modelharness` chart — consults it via a single
`EnvoyFilter` to extract the OpenAI-style `model` field from the
request body and surface it as the `X-Gateway-Model-Name` HTTP header.

This chart is shipped as a **subchart of the umbrella
[`productionstack`](../../README.md) chart** and is meant to be
installed either via the umbrella (preferred for production stacks)
or directly for ad-hoc / development use. As a subchart it is
automatically toggleable via `body-based-routing.enabled` and pinned
to `istio-system` via `body-based-routing.namespaceOverride` in the
parent values.

The chart itself is forked from
[`sigs.k8s.io/gateway-api-inference-extension/config/charts/body-based-routing`](https://github.com/kubernetes-sigs/gateway-api-inference-extension/tree/main/config/charts/body-based-routing).
The diffs vs upstream are deliberate, documented in the template
comments, and recapped here:

| Change | Why |
|---|---|
| `provider.name` default flipped from `none` → `istio` | This repo only ships the Istio data plane today, so the EnvoyFilter is always wanted out of the box. |
| `EnvoyFilter` has **no** `spec.targetRefs` and the `inferenceGateway.name` value is removed | Upstream pins the filter to a single Gateway named `inference-gateway` in the EnvoyFilter's own namespace (`targetRefs` is namespace-local in Istio). This fork needs the filter to fan out to every per-namespace Gateway provisioned across the cluster, so `targetRefs` is omitted and selection happens via `match.context: GATEWAY` alone. |
| Default `provider.istio.envoyFilter.operation` flipped from `INSERT_FIRST` to `INSERT_BEFORE`, anchored on `envoy.filters.http.router` | Enforces the gateway HTTP filter-chain order required by this repo: `ext_authz (if any) → bbr → router (HTTPRoute → epp / model-not-found)`. The terminal `router` filter is always present, so this anchor works on both auth-enabled and auth-disabled gateways (anchoring on `ext_authz` would silently no-op on auth-disabled gateways). |
| New `bbr.secureServing` toggle (default `false`) | Upstream always serves over a self-signed cert and always renders a `DestinationRule` with `tls.mode=SIMPLE, insecureSkipVerify=true`. We default to plaintext HTTP/2 to avoid the per-request self-signed-cert TLS handshake; the request never leaves the pod network and Istio mTLS is sufficient defense in depth. The `--secure-serving=<bool>` flag is rendered automatically from this value and MUST NOT be duplicated in `bbr.flags`. |
| `DestinationRule` is only rendered when `bbr.secureServing=true` | When BBR runs plaintext HTTP/2 no upstream TLS config is needed; the unconditional upstream DestinationRule becomes dead config. |
| `bbr.multiNamespace` value removed; RBAC is **always** cluster-wide | Upstream defaults `multiNamespace: false` and renders a namespace-scoped `Role`/`RoleBinding`, which would leave BBR blind to the LoRA-adapter → base-model ConfigMaps living in workload namespaces and silently break adapter-aware routing. This fork ships a single `ClusterRole` + `ClusterRoleBinding` unconditionally. |
| New `namespaceOverride` value | Lets the umbrella `productionstack` chart install BBR into `istio-system` even when the parent Helm release lives in a different namespace (Helm itself supports only one `--namespace` per release). Empty string falls back to the release namespace. |
| GKE provider template dropped | Upstream offers a `provider.name=gke` rendering path; this repo only ships the Istio data plane today. Reintroduce when a GKE-backed E2E lane lands. |
| Image tag pinned (`v1.5.0`) instead of upstream `main` | Reproducible installs; `main` is a moving floating tag in the upstream staging registry. |
| `app.kubernetes.io/*` labels added via `bbr.labels` / `bbr.selectorLabels` helpers; ConfigMap-reader `ClusterRole` name is release-scoped (`<bbr.name>-<release>-configmap-reader`) | Lets multiple unrelated releases coexist on the same cluster and makes `kubectl ... -l app.kubernetes.io/name=body-based-routing` work for ops. |

## Install via the umbrella chart (recommended)

When consumed through [`productionstack`](../../README.md) the umbrella
chart already pins this subchart to `istio-system`, so a single
`helm install` of the parent is enough:

```sh
# istio-system must already exist (Helm only auto-creates the release namespace)
kubectl create namespace istio-system --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install productionstack charts/productionstack \
  --namespace kaito-system --create-namespace --wait
```

Override BBR-specific values by nesting them under `body-based-routing:`
in the parent values (see [`charts/productionstack/values.yaml`](../../values.yaml)).

## Install standalone (ad-hoc / dev)

The chart MUST be installed into Istio's root namespace
(`istio-system`) so the rendered cluster-wide EnvoyFilter is visible
to all gateways:

```sh
helm upgrade --install body-based-router \
  charts/productionstack/charts/body-based-routing \
  --namespace istio-system \
  --create-namespace \
  --wait
```

Override the filter anchor when a different placement is required:

```sh
helm upgrade --install body-based-router \
  charts/productionstack/charts/body-based-routing \
  --namespace istio-system \
  --set provider.istio.envoyFilter.operation=INSERT_FIRST \
  --set provider.istio.envoyFilter.anchorSubFilter="" \
  --wait
```

## Filter chain order (with auth enabled)

```
┌──────────┐   ┌─────┐   ┌────────────────────────────────────────────────────┐
│ ext_authz│──▶│ bbr │──▶│ router  ─┬─ HTTPRoute(model=X) ─▶ EPP ─▶ vLLM pod │
└──────────┘   └─────┘   │          └─ catch-all ─▶ direct_response 404      │
                         └────────────────────────────────────────────────────┘
```

* `ext_authz` runs first so unauthenticated requests are rejected
  before BBR ever sees the body — no wasted parse work, no chance of
  leaking the `model` field through telemetry.
* `bbr` parses the JSON body once per request and writes
  `X-Gateway-Model-Name`.
* The router uses that header to match a per-deployment HTTPRoute
  (which forwards to the InferencePool / EPP) or falls through to the
  catch-all 404 EnvoyFilter rendered by the `modelharness` chart.

## Configuration

| Parameter                                        | Default                                                                       | Description                                                                                                                    |
|--------------------------------------------------|-------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------|
| `namespaceOverride`                              | `""`                                                                          | Pin every namespaced resource (+ EnvoyFilter upstream FQDN) to this namespace. Empty ⇒ inherit the Helm release namespace. Used by the umbrella chart to force `istio-system`. |
| `bbr.name`                                       | `body-based-router`                                                           | Name of the Deployment / Service.                                                                                              |
| `bbr.replicas`                                   | `1`                                                                           | Replica count. BBR is stateless; bump for HA.                                                                                  |
| `bbr.image.{registry,repository,tag,pullPolicy}` | upstream staging registry, tag `v1.5.0`                                       | Container image coordinates. Pinned tag (upstream defaults to `main`).                                                          |
| `bbr.port`                                       | `9004`                                                                        | ext_proc gRPC port.                                                                                                            |
| `bbr.healthCheckPort`                            | `9005`                                                                        | gRPC health-check port.                                                                                                        |
| `bbr.secureServing`                              | `false`                                                                       | When `true` BBR serves over a self-signed cert and a DestinationRule with `insecureSkipVerify=true` is rendered. The `--secure-serving=<bool>` cmd-line flag is auto-generated from this value. |
| `bbr.flags`                                      | `{ v: 3 }`                                                                    | Extra `--key=value` cmd-line flags. `secure-serving` is rendered separately and MUST NOT be set here.                          |
| `bbr.plugins`                                    | upstream defaults                                                             | Plugin pipeline. Empty list ⇒ runner auto-loads its built-in defaults.                                                          |
| `bbr.tracing.*`                                  | disabled                                                                      | OpenTelemetry exporter configuration.                                                                                          |
| `provider.name`                                  | `istio`                                                                       | `istio` ⇒ render the EnvoyFilter; `none` ⇒ Deployment + Service + RBAC only. (Upstream defaults to `none`.)                    |
| `provider.supportedEvents.*`                     | all `true`                                                                    | HTTP lifecycle events Envoy forwards to BBR.                                                                                   |
| `provider.istio.envoyFilter.operation`           | `INSERT_BEFORE`                                                               | Envoy filter-chain placement op. (Upstream defaults to `INSERT_FIRST`.)                                                         |
| `provider.istio.envoyFilter.anchorSubFilter`     | `envoy.filters.http.router`                                                   | Anchor filter name for INSERT_BEFORE / INSERT_AFTER. The terminal `router` filter is always present on every Istio Gateway HCM chain, so this default works on both auth-enabled and auth-disabled gateways. (Upstream defaults to `""`.) |

## Uninstall

```sh
helm uninstall body-based-router --namespace istio-system
```
