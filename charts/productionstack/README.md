# productionstack

Umbrella Helm chart that installs all **cluster-level** components and
CRDs required by the [Kaito](https://github.com/kaito-project/kaito)
production stack. It is designed to be consumed as a Helm
**dependency** of the top-level `kaito` chart so a single
`helm install kaito ...` pulls in every shared piece of infrastructure
the inference data plane relies on.

## Bundled components

| Subchart             | Purpose                                                                                                  | Toggle                              | Default install namespace |
|----------------------|----------------------------------------------------------------------------------------------------------|-------------------------------------|---------------------------|
| `body-based-routing` | Cluster-wide singleton Body-Based Router (BBR) ext_proc service + Istio `EnvoyFilter` wiring it into every inference Gateway. | `body-based-routing.enabled` (default `true`) | `istio-system` |
| `keda-kaito-scaler`  | KEDA external scaler that aggregates vLLM / `InferenceSet` metrics for workload-aware autoscaling.       | `keda-kaito-scaler.enabled` (default `true`)  | `keda` |
| `llm-gateway-apikey` (upstream OCI dep, 0.0.11-alpha) | API-key auth control plane for the inference Gateway: installs the `APIKey` CRD, the `apikey-operator` (reconciles `APIKey` CRs into per-namespace `llm-api-key` Secrets), and the `apikey-authz` ext_authz gRPC dataplane (single cluster-wide Service). **ext_authz wiring on inference Gateway pods is rendered per-namespace by [`charts/modelharness`](../modelharness/) — not by this subchart.** The upstream chart's own cluster-wide `EnvoyFilter` is neutralised via a sentinel `gatewaySelector` so per-namespace EnvoyFilters in `modelharness` are the sole source of ext_authz wiring. The upstream chart's STRICT `PeerAuthentication` is disabled by default (`llm-gateway-apikey.istio.strictMTLS: false`) to avoid blocking control-plane probes during rollout on mixed-mTLS clusters; flip it back on once the cluster is fully meshed. | `llm-gateway-apikey.enabled` (default `true`) | `llm-gateway-auth` |

The three subcharts each expose a `namespaceOverride` value pinning
**all of their namespaced resources** to a specific namespace,
independent of the Helm release namespace passed via `--namespace`.
The first two are in-tree forks; the third (`llm-gateway-apikey`) is
pulled as a Helm dependency directly from
`oci://mcr.microsoft.com/aks/kaito/helm` and has supported
`namespaceOverride` since upstream chart version 0.0.8-alpha. See
[Per-subchart install namespace](#per-subchart-install-namespace) for
details.

Additional cluster-level components will be added as new subcharts
under [`charts/`](./charts/) with a matching `<name>.enabled` toggle.

## Layout

```
charts/productionstack/
├── Chart.yaml                 # umbrella chart metadata + dependencies
├── values.yaml                # top-level toggles + subchart value pass-through
├── templates/
│   └── NOTES.txt              # post-install summary (no resources of its own)
└── charts/
    ├── body-based-routing/    # BBR ext_proc + Istio EnvoyFilter (in-tree fork)
    ├── keda-kaito-scaler/     # KEDA external scaler (in-tree fork)
    └── llm-gateway-apikey-*.tgz  # vendored at `helm dependency update` time
                                   # from oci://mcr.microsoft.com/aks/kaito/helm
```

The umbrella chart itself ships **no Kubernetes resources** — every
manifest is rendered by a subchart. This keeps the umbrella thin and
avoids accidental coupling between components.

## Install standalone

```sh
# istio-system and keda must already exist (Helm only auto-creates the
# release namespace).
kubectl create namespace istio-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace keda         --dry-run=client -o yaml | kubectl apply -f -

# Vendor the upstream llm-gateway-apikey tarball into ./charts/ from
# the OCI registry declared in Chart.yaml. Required once after a fresh
# clone (and after bumping the dependency version).
helm dependency update charts/productionstack

helm upgrade --install productionstack charts/productionstack \
  --namespace kaito-system \
  --create-namespace \
  --wait
```

With the default values BBR lands in `istio-system`, the KEDA scaler
in `keda`, and the upstream llm-gateway-apikey control plane in
`llm-gateway-auth` (the `namespaceOverride` defaults). The umbrella
release itself can live in any namespace.

### API-key ext_authz is per-namespace

`productionstack` only installs the **control plane** for API-key auth
(`APIKey` CRD + apikey-operator + apikey-authz gRPC Service). The
actual `EnvoyFilter` that splices `envoy.filters.http.ext_authz` into
a Gateway pod is rendered **per workload namespace** by
[`charts/modelharness`](../modelharness/) under
`templates/envoyfilter-ext-authz.yaml`, gated by `auth.enabled: true`.
Each modelharness release attaches ext_authz to its own `<namespace>-gw`
Gateway pod and points it at the cluster-wide `apikey-authz` Service
installed by this umbrella. To enforce auth in a workload namespace:

```yaml
# my-modelharness-values.yaml
auth:
  enabled: true
```

The 0.0.11-alpha bump of the upstream `llm-gateway-apikey` chart
replaced its historic `MeshConfig.extensionProviders` + CUSTOM
`AuthorizationPolicy` flow (which required a post-install hook Job to
mutate the cluster-shared `istio` ConfigMap — impossible on the AKS
Istio add-on) with a templated cluster-wide `EnvoyFilter`. Because
auth scope is per workload namespace, we deliberately **neutralise**
that cluster-wide EnvoyFilter (sentinel `gatewaySelector` that no
Gateway pod carries, namespace pinned to the LGA control plane) and
own the wiring in `modelharness` instead.

## Per-subchart install namespace

Helm itself only accepts a single `--namespace` per release. To
install each subchart into its **own** namespace inside a single
`helm install`, every subchart in this umbrella exposes a
`namespaceOverride` value. When set, all of that subchart's
**namespaced** resources are rendered into the override namespace
instead of the release namespace; cluster-scoped resources
(`ClusterRole`, `ClusterRoleBinding`, `CustomResourceDefinition`,
`ValidatingWebhookConfiguration`) are unaffected.

The upstream `llm-gateway-apikey` dependency added the same
`namespaceOverride` knob in chart version 0.0.8-alpha; below that
version every namespaced resource it ships always rendered into the
umbrella's release namespace.

```yaml
# my-values.yaml
body-based-routing:
  namespaceOverride: istio-system     # required: BBR EnvoyFilter must live here
keda-kaito-scaler:
  namespaceOverride: keda
llm-gateway-apikey:
  namespaceOverride: llm-gateway-auth
```

Caveats:

1. Target namespaces **must already exist**. `helm install
   --create-namespace` only creates the release namespace, not the
   override targets.
2. The Helm release metadata Secret is stored in the release
   namespace, but `helm uninstall` still cleans up resources in every
   override namespace because Helm tracks the rendered manifest.
3. Set `namespaceOverride: ""` to opt out and inherit the release
   namespace (standard Helm behavior).

## Install only a subset of components

Disable any subchart by setting its `enabled` toggle to `false`:

```sh
helm upgrade --install productionstack charts/productionstack \
  --namespace istio-system \
  --set body-based-routing.enabled=true \
  --set keda-kaito-scaler.enabled=false
```

## Override subchart values

Nest the override under the subchart's name (which matches the
subchart's `Chart.yaml` `name:` field). For example, to run two BBR
replicas and bump the keda-kaito-scaler log level:

```yaml
# my-values.yaml
body-based-routing:
  enabled: true
  bbr:
    replicas: 2

keda-kaito-scaler:
  enabled: true
  log:
    level: 3

# Pass-through to the upstream OCI dependency. Key MUST match the
# dependency `name:` in Chart.yaml (`llm-gateway-apikey`).
llm-gateway-apikey:
  enabled: true
  authz:
    replicaCount: 3
```

```sh
helm upgrade --install productionstack charts/productionstack \
  --namespace istio-system \
  -f my-values.yaml
```

See each subchart's own README / values.yaml for the full set of
supported keys:

- [`charts/body-based-routing/README.md`](./charts/body-based-routing/README.md)
- [`charts/keda-kaito-scaler/values.yaml`](./charts/keda-kaito-scaler/values.yaml)
- Upstream `llm-gateway-apikey` chart sources:
  [`kaito-project/llm-gateway-auth/chart/llm-gateway-apikey/values.yaml`](https://github.com/kaito-project/llm-gateway-auth/blob/main/chart/llm-gateway-apikey/values.yaml)

## Consume as a dependency of the kaito chart

Add an entry to the parent chart's `Chart.yaml`:

```yaml
# charts/kaito/Chart.yaml
dependencies:
  - name: productionstack
    version: 0.1.0
    repository: "file://../productionstack"   # or an OCI / HTTP repo URL
    condition: productionstack.enabled
```

Then forward overrides in the parent's `values.yaml`:

```yaml
# charts/kaito/values.yaml
productionstack:
  enabled: true
  body-based-routing:
    enabled: true
  keda-kaito-scaler:
    enabled: true
  llm-gateway-apikey:
    enabled: true
```

Run `helm dependency update charts/kaito` to vendor this chart into
the parent's `charts/` directory.

## Adding a new cluster-level component

1. Drop the component's chart directory under
   [`charts/`](./charts/) (e.g. `charts/llm-gateway-auth/`).
2. Append a new entry to `dependencies:` in [`Chart.yaml`](./Chart.yaml)
   with `condition: <name>.enabled`.
3. Add a `<name>.enabled` toggle (and any documented overrides) to
   [`values.yaml`](./values.yaml).
4. Mention it in the table above and in `templates/NOTES.txt`.
