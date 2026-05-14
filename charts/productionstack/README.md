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

Each subchart exposes a `namespaceOverride` value that pins **all of
its namespaced resources** to a specific namespace, independent of the
Helm release namespace passed via `--namespace`. The defaults above
are baked into [values.yaml](./values.yaml); see
[Per-subchart install namespace](#per-subchart-install-namespace) for
details.

Additional cluster-level components (e.g. `llm-gateway-auth`) will be
added as new subcharts under [`charts/`](./charts/) with a matching
`<name>.enabled` toggle.

## Layout

```
charts/productionstack/
├── Chart.yaml                 # umbrella chart metadata + dependencies
├── values.yaml                # top-level toggles + subchart value pass-through
├── templates/
│   └── NOTES.txt              # post-install summary (no resources of its own)
└── charts/
    ├── body-based-routing/    # BBR ext_proc + Istio EnvoyFilter
    └── keda-kaito-scaler/     # KEDA external scaler
```

The umbrella chart itself ships **no Kubernetes resources** — every
manifest is rendered by a subchart. This keeps the umbrella thin and
avoids accidental coupling between components.

## Install standalone

```sh
# istio-system and keda must already exist (Helm only auto-creates the
# release namespace).
kubectl create namespace istio-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace keda          --dry-run=client -o yaml | kubectl apply -f -

helm dependency update charts/productionstack
helm upgrade --install productionstack charts/productionstack \
  --namespace kaito-system \
  --create-namespace \
  --wait
```

With the default values BBR lands in `istio-system` and the KEDA
scaler in `keda`, regardless of the `--namespace` value above; only
the Helm release metadata Secret lives in the release namespace.

## Per-subchart install namespace

Helm itself only accepts a single `--namespace` per release. To
install each subchart into its **own** namespace inside a single
`helm install`, every subchart in this umbrella exposes a
`namespaceOverride` value. When set, all of that subchart's
**namespaced** resources are rendered into the override namespace
instead of the release namespace; cluster-scoped resources
(`ClusterRole`, `ClusterRoleBinding`) are unaffected.

```yaml
# my-values.yaml
body-based-routing:
  namespaceOverride: istio-system     # required: BBR EnvoyFilter must live here
keda-kaito-scaler:
  namespaceOverride: keda
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
