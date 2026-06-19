# modelharness

Per-namespace shared resources for production-stack workloads. One `modelharness` release per workload namespace provisions everything every model deployment in that namespace shares:

- the Istio `Gateway` that fronts the namespace,
- the catch-all `EnvoyFilter` (`model-not-found-direct`) that returns an OpenAI-compatible `404 model_not_found` directly from Envoy for any path not matched by a deployment-specific `HTTPRoute`,
- the per-namespace `EnvoyFilter` that injects BBR's ext_proc into the Gateway HCM,
- the unified-error `local_reply` filter that maps fail-closed BBR / ext_authz outages and any other `>= 500` reply onto a consistent OpenAI-compatible error envelope,
- (when `auth.enabled`) the `AuthorizationPolicy` and `APIKey` CR that wire the Gateway into the cluster-wide `apikey-ext-authz` CUSTOM provider,
- (when `networkPolicy.enabled`) a `CiliumNetworkPolicy` that locks down East-West ingress to inference workloads in the namespace.

A namespace may host one or more `modeldeployment` releases, all of which share its Gateway.

## Rendered resources

| Resource (Kind)                                | Group / Version                  | Toggle                       | Purpose                                                                                                                                                                                |
| ---------------------------------------------- | -------------------------------- | ---------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Gateway`                                      | `gateway.networking.k8s.io/v1`   | always                       | Public entry point for the namespace; `gatewayClassName: istio`, HTTP/80 by default.                                                                                                   |
| `EnvoyFilter` `bbr-ext-proc`                   | `networking.istio.io/v1alpha3`   | always                       | Injects the cluster-wide BBR ext_proc into the Gateway HCM, scoped via `workloadSelector`. BBR itself ships in `productionstack` / `body-based-routing`.                               |
| `EnvoyFilter` `model-not-found-direct`         | `networking.istio.io/v1alpha3`   | always                       | Patches a `direct_response` onto the Gateway HCM; returns an OpenAI 404 for any unknown-model path. Required to keep API-key ext_authz running on unknown-model requests.              |
| `EnvoyFilter` `gateway-filter-outage-local-reply` | `networking.istio.io/v1alpha3` | always                       | Single namespace-scoped `local_reply` that maps fail-closed BBR / ext_authz outages, gateway data-plane health failures, and any remaining 5xx onto the unified error envelope.        |
| `EnvoyFilter` `apikey-ext-authz`               | `networking.istio.io/v1alpha3`   | `auth.enabled`               | Splices `envoy.filters.http.ext_authz` into the Gateway pod and points it at the cluster-wide `apikey-authz` gRPC Service installed by `productionstack/llm-gateway-apikey`.           |
| `AuthorizationPolicy` `apikey-gateway-ext-authz` | `security.istio.io/v1`         | `auth.enabled`               | Per-namespace CUSTOM policy that gates traffic through the cluster-wide `apikey-ext-authz` extension provider on the Gateway pod.                                                       |
| `APIKey` `default`                             | `apikeys.kaito.sh/v1alpha1`      | `auth.enabled`               | Triggers the `apikey-operator` to reconcile a Secret (`llm-api-key`) holding the bearer token clients send.                                                                            |
| `CiliumNetworkPolicy` `inference-pods-ingress` | `cilium.io/v2`                   | `networkPolicy.enabled`      | Selects pods labelled `kaito.sh/owned-by: modeldeployment` and admits ingress from the same namespace plus `networkPolicy.allowedIngressNamespaces`; all other ingress is dropped.     |
| `Namespace`                                    | `v1`                             | `manageNamespace` (default `true`) | Stamped with `productionstack.kaito.sh/managed-by: modelharness` so the status reporter can discover it. Skipped when the release namespace is the workload namespace.            |

## Cilium requirement for the network-policy path

The chart renders `cilium.io/v2` `CiliumNetworkPolicy` (CNP) rather than `networking.k8s.io/v1` `NetworkPolicy`, because on the AKS Azure-CNI overlay dataplane a vanilla K8s `NetworkPolicy` that selects a pod created **before** the policy is installed is intermittently never compiled into the per-endpoint BPF map (the `CiliumEndpoint` reports `status.policy.ingress.enforcing=false` and traffic flows unrestricted). Native CNPs bypass that translation step and reconcile through Cilium's own path, becoming enforcing within a single tick.

Production deployments should therefore run on a cluster with the Cilium dataplane enabled — for AKS this means `--network-dataplane cilium --network-policy cilium` (see [`hack/e2e/scripts/setup-cluster.sh`](../../hack/e2e/scripts/setup-cluster.sh)). If you cannot run Cilium, set `networkPolicy.enabled=false` and apply equivalent ingress isolation out-of-band.

## Install

OCI (recommended):

```sh
helm install modelharness oci://ghcr.io/kaito-project/modelharness \
  --version <X.Y.Z> \
  --namespace my-models \
  --create-namespace
```

In-tree:

```sh
helm install modelharness ./charts/modelharness \
  --namespace my-models \
  --create-namespace
```

Enable API-key auth and customize the allowed ingress namespaces:

```sh
helm install modelharness oci://ghcr.io/kaito-project/modelharness \
  --version <X.Y.Z> \
  --namespace my-models \
  --create-namespace \
  --set auth.enabled=true \
  --set 'networkPolicy.allowedIngressNamespaces={kube-system,monitoring}'
```

## Inputs

Top-level values (see [`values.yaml`](./values.yaml) for the full schema, defaults, and inline documentation; cross-field constraints that JSON Schema cannot express are enforced by [`values.schema.json`](./values.schema.json) at install time).

| Key                                       | Default                          | Description                                                                                                                                                                                                |
| ----------------------------------------- | -------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `namespace`                               | `.Release.Namespace`             | Workload namespace that owns the rendered resources.                                                                                                                                                       |
| `manageNamespace`                         | `true`                           | Render the workload `Namespace` object (stamped with the discovery label). Skipped when the release namespace is the workload namespace.                                                                   |
| `gatewayName`                             | `<namespace>-gw`                 | Per-namespace Istio Gateway name. The Service follows Istio's `<gatewayName>-istio` convention.                                                                                                            |
| `gatewayClassName`                        | `istio`                          | `GatewayClass` the Gateway binds to (AKS Istio add-on with App Routing: `approuting-istio`).                                                                                                               |
| `gatewayPort`                             | `80`                             | HTTP listener port on the Gateway.                                                                                                                                                                         |
| `bbr.name` / `bbr.namespace` / `bbr.port` | `body-based-router` / `kube-system` / `9004` | Cluster FQDN coordinates of the BBR Service installed by `productionstack/body-based-routing`. Must match wherever BBR was installed.                                                              |
| `bbr.envoyFilter.operation` / `anchorSubFilter` | `INSERT_BEFORE` / `envoy.filters.http.ext_proc` | Filter-chain placement so BBR injects `X-Gateway-Model-Name` before the InferencePool / EPP ext_proc and the `HTTPRoute` match run.                                                |
| `bbr.outlierDetection.*`                  | see `values.yaml`                | Passive outlier detection on the BBR ext_proc upstream cluster so an erroring replica is ejected before tripping the fail-closed `502 bbr_unavailable` path.                                               |
| `auth.enabled`                            | `false`                          | Render the per-namespace API-key auth artifacts (`AuthorizationPolicy`, `APIKey`, `apikey-ext-authz` `EnvoyFilter`).                                                                                       |
| `auth.apiKeyName`                         | `default`                        | Name of the rendered `APIKey` CR.                                                                                                                                                                          |
| `auth.extAuthz.*`                         | `apikey-authz` / `kube-system` / `9001` / `5s` | Cluster-wide `apikey-authz` gRPC Service coordinates the per-namespace `EnvoyFilter` targets. Defaults match the `llm-gateway-apikey` subchart.                                              |
| `networkPolicy.enabled`                   | `true`                           | Render the per-namespace `CiliumNetworkPolicy`. **Requires the Cilium dataplane** (see above). Disable when running on a non-Cilium cluster.                                                               |
| `networkPolicy.allowedIngressNamespaces`  | `[kube-system, monitoring]` | Cross-namespace ingress allowlist for inference pods. Each entry renders a `fromEndpoints` clause keyed off `k8s:io.kubernetes.pod.namespace`. Empty = strict per-namespace isolation. |

## Companion charts

- [`charts/productionstack`](../productionstack/README.md) — cluster-level components (BBR data plane, KEDA Kaito scaler, llm-gateway-apikey control plane) that `modelharness` references.
- [`charts/modeldeployment`](../modeldeployment/README.md) — installed per model into a namespace whose Gateway / harness this chart provisioned.
