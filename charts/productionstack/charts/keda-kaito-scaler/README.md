# keda-kaito-scaler

Helm chart for the [`keda-kaito-scaler`](https://github.com/kaito-project/keda-kaito-scaler)
external scaler — a [KEDA](https://keda.sh/) gRPC scaler that aggregates
per-pod metrics from Kaito `InferenceSet` workloads (vLLM inference
pods) and exposes a single summed metric value to KEDA's
`ScaledObject` machinery so the underlying Deployment / `InferenceSet`
can be scaled in and out based on real workload pressure (e.g. running
requests, queued tokens) rather than CPU/memory only.

This chart is intended to be consumed either standalone or as part of
the umbrella [`productionstack`](../../README.md) chart.

## What gets installed

| Kind                 | Name                                  | Scope        | Purpose                                                                                |
|----------------------|---------------------------------------|--------------|----------------------------------------------------------------------------------------|
| `Deployment`         | `keda-kaito-scaler`                   | Namespaced   | The scaler controller pod (gRPC server + reconciler).                                  |
| `Service`            | `keda-kaito-scaler-svc`               | Namespaced   | Exposes the gRPC `ExternalScaler` API consumed by `keda-operator`.                     |
| `ServiceAccount`     | `keda-kaito-scaler-sa`                | Namespaced   | Identity used by the controller pod.                                                   |
| `Secret` (×2)        | `keda-kaito-scaler-{server,client}-certs` | Namespaced | mTLS material the controller mounts at `/tmp/keda-kaito-scaler-certs/{server,client}`. Generated/rotated by the controller itself (`--cert-duration`). |
| `ClusterRole`        | `keda-kaito-scaler-clusterrole`       | Cluster      | Read `kaito.sh/{inferencesets,workspaces}`; create/update `keda.sh/{scaledobjects,clustertriggerauthentications}`; manage leases, events, secrets. |
| `ClusterRoleBinding` | `keda-kaito-scaler-clusterrole-binding` | Cluster    | Binds the ClusterRole to the ServiceAccount.                                           |

The chart **does not** install KEDA itself. KEDA (`keda-operator`,
`keda-operator-metrics-apiserver`) must already be running in the
cluster, typically in the `keda` namespace.

## Install standalone

```sh
# Pre-create the target namespace; Helm --create-namespace only creates
# the release namespace, which may differ from namespaceOverride.
kubectl create namespace keda --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install keda-kaito-scaler \
  charts/productionstack/charts/keda-kaito-scaler \
  --namespace keda \
  --create-namespace \
  --wait
```

Verify the scaler is registered:

```sh
kubectl -n keda get pods -l app.kubernetes.io/name=keda-kaito-scaler
kubectl -n keda logs deploy/keda-kaito-scaler | head
```

## Install as a subchart of `productionstack`

The umbrella chart pins `keda-kaito-scaler.namespaceOverride: keda` by
default, so the scaler lands in the `keda` namespace regardless of the
Helm release namespace:

```sh
helm upgrade --install productionstack charts/productionstack \
  --namespace kaito-system \
  --create-namespace
```

To override individual values from the parent, nest them under the
subchart name:

```yaml
# parent values.yaml
keda-kaito-scaler:
  enabled: true
  replicaCount: 2
  log:
    level: 3
```

## Wiring it into KEDA

Reference the scaler from a KEDA `ScaledObject` with the
`external-push` (or `external`) trigger type, pointing at the service
FQDN rendered by this chart:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: my-inference-set
  namespace: my-workload-ns
spec:
  scaleTargetRef:
    apiVersion: kaito.sh/v1alpha1
    kind: InferenceSet
    name: my-inference-set
  triggers:
    - type: external
      metadata:
        scalerAddress: keda-kaito-scaler-svc.keda.svc.cluster.local:10450
        # Plus scaler-specific metadata; see the keda-kaito-scaler repo.
```

In practice the controller in this chart **creates the `ScaledObject`
itself** for each `InferenceSet` it watches (that's why the
`ClusterRole` grants `scaledobjects` create/delete), so end users
normally only author the `InferenceSet` and let the controller wire
KEDA up.

## Per-subchart install namespace

This chart exposes `namespaceOverride` so a single
`helm install` of the parent umbrella chart can place each subchart
into its own namespace. When set, every namespaced resource
(`Deployment`, `Service`, `ServiceAccount`, the two `Secrets`) and the
`ClusterRoleBinding`'s `subjects[].namespace` are pinned to the
override. The controller's `--working-namespace` flag is also wired to
the same value so its lease and self-managed secrets live alongside
the pod.

Set `namespaceOverride: ""` to fall back to the standard Helm
`--namespace` behavior.

## Configuration

| Parameter                  | Default                                  | Description                                                                                                  |
|----------------------------|------------------------------------------|--------------------------------------------------------------------------------------------------------------|
| `replicaCount`             | `1`                                      | Number of controller replicas. Leader election (via the lease grant in the ClusterRole) makes >1 safe.       |
| `certDuration`             | `43800h` (5 years)                       | Lifetime of the self-signed mTLS certs the controller generates and rotates.                                 |
| `nameOverrider`            | `""`                                     | Overrides the chart name used in labels (does **not** rename the workload resources).                        |
| `namespaceOverride`        | `""`                                     | Install namespace for all namespaced resources. Empty ⇒ inherit the release namespace. See above.            |
| `log.level`                | `3`                                      | klog verbosity (`--v`). Bump to `5` for debug logging.                                                        |
| `image.registry`           | `ghcr.io`                                | Container image registry.                                                                                    |
| `image.repository`         | `kaito-project/keda-kaito-scaler`        | Container image repository.                                                                                  |
| `image.tag`                | `0.5.1`                                  | Container image tag.                                                                                         |
| `image.pullSecrets`        | `[]`                                     | `imagePullSecrets` for the pod spec.                                                                         |
| `ports.grpc`               | `10450`                                  | gRPC `ExternalScaler` port exposed via the Service.                                                          |
| `ports.metrics`            | `10451`                                  | Prometheus metrics port (`--metrics-port`). Not currently fronted by a Service.                              |
| `ports.grpcHealth`         | `10452`                                  | gRPC health-check port consumed by liveness/readiness probes.                                                |
| `resources.requests.cpu`   | `200m`                                   | Controller container CPU request.                                                                            |
| `resources.requests.memory`| `256Mi`                                  | Controller container memory request.                                                                         |
| `resources.limits.cpu`     | `2000m`                                  | Controller container CPU limit.                                                                              |
| `resources.limits.memory`  | `512Mi`                                  | Controller container memory limit.                                                                           |

## Upgrade

The mTLS certs are managed by the controller itself, not by Helm, so
`helm upgrade` will not touch the two `*-certs` Secrets. The
controller will rotate them on its own schedule based on
`certDuration`.

## Uninstall

```sh
helm uninstall keda-kaito-scaler --namespace keda
```

Helm removes everything the chart created, including the
cluster-scoped `ClusterRole` and `ClusterRoleBinding`. Any
`ScaledObject` resources the controller has provisioned for
`InferenceSet`s are **not** cleaned up by Helm — delete the parent
`InferenceSet` first (or remove them with `kubectl delete
scaledobject`).

## Links

- Upstream repo: <https://github.com/kaito-project/keda-kaito-scaler>
- KEDA external scalers: <https://keda.sh/docs/latest/concepts/external-scalers/>
- Umbrella chart: [`charts/productionstack`](../../README.md)
