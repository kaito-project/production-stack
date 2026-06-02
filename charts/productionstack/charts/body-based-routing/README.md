# body-based-routing

Helm chart that installs a **cluster-wide workload singleton** Body-Based
Router (BBR) for the Gateway API Inference Extension. BBR runs as a
single shared `ext_proc` service (a Deployment + Service + cluster-scoped
RBAC) that extracts the OpenAI-style `model` field from the request body
and surfaces it as the `X-Gateway-Model-Name` HTTP header.

This chart renders **only the BBR workload** — it does **not** render any
`EnvoyFilter`. The dataplane wiring that injects BBR's `ext_proc` into a
Gateway's HCM is rendered **per workload namespace** by the
[`modelharness`](../../../modelharness) chart
(`templates/envoyfilter-bbr.yaml`) and scoped to that namespace's Gateway
pod via a `workloadSelector`. Because BBR is reached purely by its
cluster FQDN, it no longer has to live in Istio's root namespace and is
co-located with the umbrella release (`kaito-system` by default).

BBR always serves **plaintext HTTP/2** (`--secure-serving=false`): the
gateway↔BBR hop never leaves the pod network, so there is no upstream
TLS and no `DestinationRule`.

This chart is shipped as a **subchart of the umbrella
[`productionstack`](../../README.md) chart** and is meant to be
installed either via the umbrella (preferred for production stacks)
or directly for ad-hoc / development use. As a mandatory umbrella
component it is always installed with the stack.

The chart itself is forked from
[`sigs.k8s.io/gateway-api-inference-extension/config/charts/body-based-routing`](https://github.com/kubernetes-sigs/gateway-api-inference-extension/tree/main/config/charts/body-based-routing).
The diffs vs upstream are deliberate, documented in the template
comments, and recapped here:

| Change | Why |
|---|---|
| EnvoyFilter rendering moved OUT of this chart into `charts/modelharness` | BBR's ext_proc EnvoyFilter is now rendered per workload namespace and scoped to that namespace's Gateway pod via `workloadSelector`, so production-stack's BBR never patches unrelated Istio Gateways elsewhere in the mesh. This chart renders only the BBR workload (Deployment + Service + RBAC). |
| `EnvoyFilter` has **no** `spec.targetRefs` and the `inferenceGateway.name` value is removed | Upstream pins the filter to a single Gateway named `inference-gateway` in the EnvoyFilter's own namespace. The per-namespace EnvoyFilter rendered by `modelharness` instead selects each namespace's Gateway pod directly via `workloadSelector` + `match.context: GATEWAY`. |
| Filter anchored INSERT_BEFORE the InferencePool `envoy.filters.http.ext_proc` (configured in `modelharness`) | Enforces the gateway HTTP filter-chain order required by this repo: `ext_authz (if any) → bbr → ext_proc/epp → router`. BBR must inject `X-Gateway-Model-Name` before the InferencePool ext_proc resolves its per-route override. |
| BBR always serves plaintext HTTP/2; the `bbr.secureServing` toggle and the upstream `DestinationRule` are removed | The gateway↔BBR hop never leaves the pod network, so upstream TLS adds a per-request self-signed-cert handshake for no benefit. `--secure-serving=false` is hardcoded and no `DestinationRule` is rendered. |
| `bbr.multiNamespace` value removed; RBAC is **always** cluster-wide | Upstream defaults `multiNamespace: false` and renders a namespace-scoped `Role`/`RoleBinding`, which would leave BBR blind to the LoRA-adapter → base-model ConfigMaps living in workload namespaces and silently break adapter-aware routing. This fork ships a single `ClusterRole` + `ClusterRoleBinding` unconditionally. |
| `namespaceOverride` value retained | Lets the umbrella `productionstack` chart place BBR in a namespace independent of the parent release namespace if needed. Empty string falls back to the release namespace (`kaito-system`). BBR no longer needs to live in Istio's root namespace. |
| GKE provider template dropped | Upstream offers a `provider.name=gke` rendering path; this repo only ships the Istio data plane today. Reintroduce when a GKE-backed E2E lane lands. |
| Image tag pinned (`v1.5.0`) instead of upstream `main` | Reproducible installs; `main` is a moving floating tag in the upstream staging registry. |
| `app.kubernetes.io/*` labels added via `bbr.labels` / `bbr.selectorLabels` helpers; ConfigMap-reader `ClusterRole` name is release-scoped (`<bbr.name>-<release>-configmap-reader`) | Lets multiple unrelated releases coexist on the same cluster and makes `kubectl ... -l app.kubernetes.io/name=body-based-routing` work for ops. |

## Install via the umbrella chart (recommended)

When consumed through [`productionstack`](../../README.md) a single
`helm install` of the parent is enough; BBR is co-located with the
umbrella release namespace:

```sh
helm upgrade --install productionstack charts/productionstack \
  --namespace kaito-system --create-namespace --wait
```

Override BBR-specific values by nesting them under `body-based-routing:`
in the parent values (see [`charts/productionstack/values.yaml`](../../values.yaml)).
The per-namespace ext_proc EnvoyFilter that wires BBR into each Gateway
is configured separately, under `bbr:` in the
[`modelharness`](../../../modelharness/values.yaml) chart — keep its
`bbr.namespace` in sync with wherever BBR is installed.

## Install standalone (ad-hoc / dev)

BBR can be installed into any namespace (it is reached by its cluster
FQDN); co-locating with the rest of the stack is typical:

```sh
helm upgrade --install body-based-router \
  charts/productionstack/charts/body-based-routing \
  --namespace kaito-system \
  --create-namespace \
  --wait
```

Then point the `modelharness` chart at it via `--set bbr.namespace=<ns>`
so the per-namespace EnvoyFilter targets the right Service FQDN.

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
* The InferencePool `ext_proc` (EPP) then uses that header to pick an
  endpoint, and the router matches a per-deployment HTTPRoute or falls
  through to the catch-all 404 EnvoyFilter rendered by the
  `modelharness` chart.

## Configuration

| Parameter                                        | Default                                                                       | Description                                                                                                                    |
|--------------------------------------------------|-------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------|
| `namespaceOverride`                              | `""`                                                                          | Pin the BBR Deployment / Service / RBAC (and the upstream FQDN modelharness uses) to this namespace. Empty ⇒ inherit the Helm release namespace (`kaito-system`). |
| `bbr.name`                                       | `body-based-router`                                                           | Name of the Deployment / Service.                                                                                              |
| `bbr.replicas`                                   | `2`                                                                           | Replica count. BBR is a cluster-scope singleton on the request hot path, so it runs HA by default. `values.schema.json` pins the minimum to **2**; lowering it below 2 is rejected at render time. |
| `bbr.podAntiAffinity.type`                       | `soft`                                                                        | `soft` ⇒ `preferredDuringScheduling…` (best-effort spread, still schedules on a single-node cluster); `hard` ⇒ `requiredDuringScheduling…` (a replica stays Pending until a distinct node is free). The anti-affinity rule itself is load-bearing for HA and is always rendered. |
| `bbr.podAntiAffinity.topologyKey`                | `kubernetes.io/hostname`                                                      | Topology domain across which replicas are spread (per-node by default; set to `topology.kubernetes.io/zone` for per-zone).      |
| `bbr.healthCheck.{liveness,readiness}.*`         | `initialDelaySeconds:5, periodSeconds:10, timeoutSeconds:5/3, failureThreshold:3` | Standard probe timing knobs. The gRPC liveness/readiness probes against BBR's `grpc.health.v1.Health` service on `healthCheckPort` are load-bearing for HA (their readiness result drives the pod Ready condition → Istio EDS endpoint set → ext_proc cluster) and are always rendered. |
| `bbr.image.{registry,repository,tag,pullPolicy}` | upstream staging registry, tag `v1.5.0`                                       | Container image coordinates. Pinned tag (upstream defaults to `main`).                                                          |
| `bbr.port`                                       | `9004`                                                                        | ext_proc gRPC port.                                                                                                            |
| `bbr.healthCheckPort`                            | `9005`                                                                        | gRPC health-check port.                                                                                                        |
| `bbr.flags`                                      | `{ v: 3 }`                                                                    | Extra `--key=value` cmd-line flags. `secure-serving` is hardcoded to `false` (plaintext HTTP/2) and MUST NOT be set here.       |
| `bbr.plugins`                                    | upstream defaults                                                             | Plugin pipeline. Empty list ⇒ runner auto-loads its built-in defaults.                                                          |
| `bbr.tracing.*`                                  | disabled                                                                      | OpenTelemetry exporter configuration.                                                                                          |

> The ext_proc EnvoyFilter placement (`operation`, `anchorSubFilter`),
> the `supportedEvents` processing modes, and the passive
> `outlierDetection` CLUSTER patch are configured in the
> [`modelharness`](../../../modelharness/values.yaml) chart under `bbr:`,
> not here — this chart no longer renders an EnvoyFilter.

## High availability & unhealthy-replica ejection

BBR ext_proc is a **cluster-scope component on the hot path of every
inference request**, so a single replica is a single point of failure
and a single unhealthy replica must not trigger the fail-closed
`502 bbr_unavailable` path (that path is reserved for when **all**
replicas are down). This chart ships HA out of the box:

* **≥ 2 replicas + pod anti-affinity** (`bbr.replicas`, default `2`,
  schema minimum `2`; `bbr.podAntiAffinity`) spread BBR across distinct
  nodes so the loss of one node cannot take down every replica.
* **Active gRPC health checking** is wired via Kubernetes
  liveness/readiness probes (`bbr.healthCheck`) against BBR's
  `grpc.health.v1.Health` service on `healthCheckPort`. BBR serves the
  health protocol **only** on `healthCheckPort` (the ext_proc port
  answers a probe with `UNIMPLEMENTED`), so the active check is wired at
  the platform layer: the readiness result drives the pod Ready
  condition → Istio EDS endpoint set → the ext_proc upstream cluster, so
  only Ready replicas are dialed by the ext_proc filter.
* **Passive outlier detection** is rendered by
  [`charts/modelharness`](../../../modelharness/values.yaml) (under
  `bbr.outlierDetection`) as a CLUSTER `EnvoyFilter` patch on each
  namespace's Gateway, ejecting a replica that starts erroring on the
  request path faster than the readiness probe can flip it NotReady.
  `maxEjectionPercent` is capped below 100 so outlier detection can
  never eject every replica at once.

Net effect: losing a single BBR replica is a transparent failover to
the healthy replicas, and `502 bbr_unavailable` only fires when every
replica is unhealthy.

## Uninstall

```sh
helm uninstall body-based-router --namespace kaito-system
```
