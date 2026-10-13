# modeldeployment

Helm chart that deploys a complete set of GAIE (Gateway API Inference
Extension) artifacts for a single model:

- `kaito.sh/v1alpha1` `InferenceSet`
- `inference.networking.k8s.io/v1` `InferencePool` (normally provisioned by KAITO via Flux; rendered inline here)
- Endpoint Picker (EPP) `Deployment` + `Service` + `ServiceAccount` + `Role` + `RoleBinding` + `ConfigMap`
- `gateway.networking.k8s.io/v1` `HTTPRoute` matching `X-Gateway-Model-Name: <name>`
- `networking.istio.io/v1alpha3` `EnvoyFilter` mapping EPP / upstream response flags onto the unified OpenAI-compatible error envelope

Every chart-owned object (EPP `Deployment` / `Service`, `HTTPRoute`,
`InferencePool`, `ConfigMap`, `EnvoyFilter`) is stamped with the
identifying labels `kaito.sh/inferenceset: <name>` and
`kaito.sh/owned-by: modeldeployment` so the
`productionstack-status-reporter` can correlate them back to the owning
deployment.

Chart values are validated by `values.schema.json` at install time
(`model` non-empty, `replicas` / `minReplicas` / `maxReplicas` empty or
non-negative integers, and — when `enableScaling=true` — at least one
`scaling.metrics` entry). Rules that JSON Schema cannot express are
additionally enforced by fail-fast template guards when
`enableScaling=true`: `minReplicas` and `maxReplicas` are required,
`maxReplicas >= 1`, `minReplicas <= maxReplicas` (and
`minReplicas <= replicas <= maxReplicas` when `replicas` is set), paired
thresholds, and the [scale to zero](#scale-to-zero) activation/deactivation
rules of keda-kaito-scaler v0.7.0. With `enableScaling=false`, `minReplicas`,
`maxReplicas` and `scaling` are ignored.

The EPP runs the [`llm-d-router-endpoint-picker`](https://github.com/llm-d/llm-d-router/tree/v0.9.0)
distribution with `--secure-serving=false`, so the Istio Gateway can reach it
over plaintext gRPC and **no `DestinationRule` is required**.

## Inputs

| Key                       | Required | Default                                                              | Description                                                                                |
| ------------------------- | -------- | -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| `name`                    | optional | `.Release.Name`                                                      | Deployment name. Used as InferenceSet name and as the `X-Gateway-Model-Name` header value. |
| `namespace`               | optional | `.Release.Namespace`                                                 | Target namespace.                                                                          |
| `model`                   | required | `""`                                                                 | Preset model name. Used only as `spec.template.inference.preset.name`.                     |
| `config`                  | optional | `""`                                                                 | Name of a ConfigMap in the deployment namespace containing custom inference arguments. Rendered as `spec.template.inference.config`. |
| `modelAccessSecret`       | optional | `""`                                                                 | Name of a Secret containing Hugging Face model access credentials. Rendered as `spec.template.inference.preset.presetOptions.modelAccessSecret`. |
| `instanceType`            | required | `Standard_NV36ads_A10_v5`                                            | VM instance type for the underlying nodes.                                                 |
| `replicas`                | optional | _empty_                                                              | InferenceSet replicas (`spec.replicas`). Empty leaves `spec.replicas` unset (CRD default `1`; with `enableScaling=true`, KEDA moves it into `[minReplicas, maxReplicas]`). When set with `enableScaling=true` it must lie within `[minReplicas, maxReplicas]` and is re-applied only when its value changes (see [Replica ownership](#replica-ownership)). |
| `minReplicas`             | required when `enableScaling=true` | _empty_                                    | Autoscaling lower bound, wired to `scaledobject.kaito.sh/min-replicas`. Ignored when `enableScaling=false`. `0` enables [scale to zero](#scale-to-zero). |
| `enableScaling`           | optional | `false`                                                              | Wired to `scaledobject.kaito.sh/auto-provision`. Gates `minReplicas`, `maxReplicas` and the entire `scaling` block. |
| `maxReplicas`             | required when `enableScaling=true` | _empty_                                    | Autoscaling upper bound, wired to `scaledobject.kaito.sh/max-replicas`. Ignored when `enableScaling=false`. Must be `>= 1`, `>= minReplicas` and, when set, `>= replicas`. |
| `scaling.metrics`         | optional | `vllm:num_requests_waiting` gauge + `vllm:request_queue_time_seconds` histogram (1 ↔ N), plus `llm_d_epp_flow_control_queue_size` (epp, activation `10` / deactivation `0`) and `vllm:num_requests_running` (deactivation `0`) for 0 ↔ 1; `vllm:num_requests_waiting` also carries deactivation `0` | Ordered list of scaling signals; rendered as a single `scaledobject.kaito.sh/metrics` annotation (a YAML list). Up/down bands combine under the AND policy. With `minReplicas >= 1` the activation/deactivation fields are ignored and entries carrying only those fields are skipped, so the defaults work for both modes. |
| `scaling.metrics[].name`  | required | `vllm:num_requests_waiting`                                         | Prometheus metric family name. Rendered as the `name` field of a `scaledobject.kaito.sh/metrics` entry.            |
| `scaling.metrics[].type`  | optional | `gauge`                                                             | `gauge` (per-replica average; summed across EPP pods for `source: epp`) or `histogram` (per-pod windowed average). Rendered as the `type` field. |
| `scaling.metrics[].source` | optional | _empty_ → `modelpod`                                               | `modelpod` (model-serving pods) or `epp` (Endpoint Picker pods, which keep reporting at zero replicas). Rendered as the `source` field when set. |
| `scaling.metrics[].aggregation` | optional | _empty_ → derived                                            | Overrides the derived reduction: `sum` / `service-avg` (gauge) or `windowed-avg` (histogram). Rendered as the `aggregation` field when set. |
| `scaling.metrics[].upThreshold`   | required¹ | `10` (queue depth) / `30` (queue time)                    | Per-replica scale-up threshold for 1 ↔ N. Set together with `downThreshold`. Rendered as the `upthreshold` field. |
| `scaling.metrics[].downThreshold` | required¹ | `1`                                                       | Per-replica scale-down threshold for 1 ↔ N (MUST be `< upThreshold`). Rendered as the `downthreshold` field. |
| `scaling.metrics[].activationThreshold` | optional² | _empty_                                             | 0 → 1 wake threshold (metric `>` threshold, ORed across metrics). Requires `source: epp` and a `deactivationThreshold` on the same metric. Rendered as the `activationthreshold` field. |
| `scaling.metrics[].deactivationThreshold` | optional² | _empty_                                           | 1 → 0 park threshold (metric `<=` threshold, ANDed across metrics). MUST NOT exceed `activationThreshold`; at least one `modelpod` metric must carry one. Rendered as the `deactivationthreshold` field. |
| `scaling.metrics[].metricCacheWindow` | optional | _empty_ → `300`                                    | Rolling cache window (seconds) over which `histogram` metrics are averaged (windowed-avg); ignored for `gauge`. Must be a positive number of seconds. Rendered as the `metriccachewindow` field of a `scaledobject.kaito.sh/metrics` entry. |
| `scaling.evaluationWindow`| optional | `60`                                                                | Scale-up stabilization window (seconds). Wired to `scaledobject.kaito.sh/evaluationwindow`. |
| `scaling.scaleUpCooldown` | optional | `300`                                                               | Minimum seconds between scale-up steps. Wired to `scaledobject.kaito.sh/scaleupcooldown`.  |
| `scaling.scaleDownCooldown` | optional | `300`                                                             | Minimum seconds between scale-down steps. Wired to `scaledobject.kaito.sh/scaledowncooldown`. |
| `scaling.cooldownPeriod`  | optional | `600`                                                               | Seconds KEDA waits after the last active trigger reading before parking at zero. `0` parks on the first inactive poll (~15s); empty or `null` falls back to keda-kaito-scaler's `300`. Must fit in int32. Only rendered when `minReplicas=0` and silently ignored otherwise. Wired to `scaledobject.kaito.sh/cooldownperiod`. |
| `autoUpgrade.enabled`     | optional | `false`                                                             | Opts this InferenceSet into KAITO automatic base image upgrades. Renders `spec.autoUpgrade.enabled: true`. Also requires the `enableBaseImageAutoUpgrade` feature gate on the KAITO controller. |
| `autoUpgrade.maintenanceWindow.schedule` | optional | _empty_                                             | 5-field cron (UTC) marking when rollouts may begin, e.g. `"0 2 * * 6"`. Empty lets upgrades start at any time (the `maintenanceWindow` block is omitted). Consumed only when `autoUpgrade.enabled=true`. |
| `autoUpgrade.maintenanceWindow.duration` | optional | _empty_ → `4h`                                      | How long the window stays open once it opens, e.g. `"4h"`. Empty inherits the KAITO controller's `4h` default. Ignored when `schedule` is empty. |
| `gatewayName`             | optional | _empty_ → `<namespace>`                                             | Gateway the HTTPRoute attaches to. Defaults to the per-namespace Gateway provisioned by `charts/modelharness`. |
| `epp.image.repository`    | optional | `mcr.microsoft.com/oss/v2/llm-d/llm-d-router-endpoint-picker`       | EPP container image.                                                                       |
| `epp.image.tag`           | optional | `v0.9.0`                                                             | EPP image tag.                                                                             |
| `epp.image.pullPolicy`    | optional | `IfNotPresent`                                                       | EPP image pull policy.                                                                     |
| `epp.replicas`            | optional | `1`                                                                  | Number of EPP pods.                                                                        |
| `epp.modelServerPort`     | optional | `5000`                                                               | Port exposed by inference pods (`KAITO PortInferenceServer`).                              |
| `epp.extProcPort`         | optional | `9002`                                                               | EPP gRPC ext_proc port.                                                                    |
| `epp.healthPort`          | optional | `9003`                                                               | EPP gRPC health-check port.                                                                |
| `epp.metricsPort`         | optional | `9090`                                                               | EPP HTTP metrics port.                                                                     |
| `epp.logVerbosity`        | optional | `1`                                                                  | EPP `--v=<n>` log verbosity.                                                               |
| `epp.resources`           | optional | `requests: 100m / 256Mi` (no limits)                                 | EPP container resources. Steady-state usage is ~10–25m CPU / ~40–50Mi memory; `epp.flowControl.maxBytes` keeps a full flow-control queue within the memory request. |
| `epp.modelServerSelector` | optional | `inferenceset.kaito.sh/created-by: ""`, `apps.kubernetes.io/pod-index: "0"` | InferencePool selector. Empty `created-by` value is auto-filled with `name`. |
| `epp.flowControl.enabled` | optional | `false`                                                              | Forces the EPP `flowControl` feature gate on. It is turned on automatically for [scale to zero](#scale-to-zero). |
| `epp.flowControl.maxConcurrency` | optional | `1000000`                                                     | Per-endpoint in-flight capacity of the `concurrency-detector` saturation detector. The EPP queues once in-flight requests reach `maxConcurrency × endpoints`; an empty pool always counts as saturated. |
| `epp.flowControl.maxBytes` | optional | `128Mi`                                                            | Cap on the total size of request bodies buffered in the flow-control queue (held in EPP memory). Requests beyond it get an immediate 429. Empty falls back to the EPP's 1GB-per-priority-band limit. |
| `epp.flowControl.defaultRequestTTL` | optional | `600s`                                                    | Maximum time a request may wait in the flow-control queue, e.g. `10m`. Empty waits until the client cancels. |

¹ Required (as a pair) unless `minReplicas=0`, where an entry may carry only an
activation/deactivation band.
² Only used when `minReplicas=0`; ignored otherwise.

## Naming conventions

- InferencePool: `<name>-inferencepool`
- EPP Deployment / Service / SA / Role / ConfigMap: `<name>-inferencepool-epp`
- HTTPRoute: `<name>-route`

## Example

Install into a workload namespace whose `Gateway` was provisioned by
[`charts/modelharness`](../modelharness) (Gateway name follows the
`<namespace>` convention shared by both charts):

```sh
helm install qwen ./charts/modeldeployment \
  --namespace my-models \
  --set name=qwen \
  --set model=qwen2-5-coder-7b-instruct \
  --set enableScaling=true \
  --set minReplicas=2 \
  --set maxReplicas=5
```

The rendered `HTTPRoute` parents into the `my-models-gw` `Gateway`
because `gatewayName` is left empty. Override `--set gatewayName=...`
only when attaching to a Gateway with a business-specific name.

## Scale to zero

> Requires keda-kaito-scaler **v0.7.0** or later (shipped by the
> `productionstack` chart).

Setting `minReplicas=0` (with `enableScaling=true`) lets the deployment
release its GPU capacity entirely while idle and reacquire it on the next
request:

```sh
helm install qwen ./charts/modeldeployment \
  --namespace my-models \
  --set name=qwen \
  --set model=qwen2-5-coder-7b-instruct \
  --set enableScaling=true \
  --set minReplicas=0 \
  --set maxReplicas=5
```

With `replicas` left empty the deployment starts with one replica (the CRD
default) and parks once idle; set `replicas=0` to install it parked. The
1 ↔ N range keeps working exactly as before; scale to zero adds the 0 ↔ 1
edge, driven by the default `scaling.metrics` entries:

- **Wake (0 → 1)** — `llm_d_epp_flow_control_queue_size` (`source: epp`,
  `activationThreshold: 10`). At zero replicas there are no model pods to
  scrape, so the signal comes from this deployment's Endpoint Picker. The
  chart turns on the EPP `flowControl` feature gate whenever `minReplicas=0`:
  the EPP then **queues** requests that arrive while the pool is empty instead
  of failing them immediately, and the queued requests raise the queue gauge
  that wakes the deployment. Serving those requests is best effort, not
  guaranteed: see [Cold-start requests](#cold-start-requests).
- **Park (1 → 0)** — after `scaling.cooldownPeriod` seconds during which the
  EPP queue is empty **and** both `vllm:num_requests_waiting` and
  `vllm:num_requests_running` are `0` on the model pods, so in-flight
  generation is never cut off (`deactivationThreshold: 0` on all three).

The defaults follow the keda-kaito-scaler scale-to-zero design
([kaito-project/keda-kaito-scaler#150](https://github.com/kaito-project/keda-kaito-scaler/issues/150#issuecomment-5404566270)).
While parked nothing drains the queue, so it holds every request that arrived
within the wait window (`epp.flowControl.defaultRequestTTL`, `600s` by default,
or the client timeout if shorter), and the wake condition is roughly
`QPS > activationThreshold / window`: with the defaults, `10` wakes at about
1 request per minute. Requests below that rate are buffered rather than
rejected and fail once they time out, so a deployment whose traffic stays
below the floor never wakes. Set the activation threshold to `0` to wake on
the first request.

Flow control uses the `concurrency-detector` with a very large
`epp.flowControl.maxConcurrency`, so it only queues while no replica exists
and leaves 1 ↔ N queueing to vLLM, whose metrics drive scale-up. Queued
requests are held in EPP memory; `epp.flowControl.maxBytes` (default `128Mi`)
caps the queue so it stays within the EPP's memory request, and requests
beyond it are rejected immediately with 429.

### Cold-start requests

Scale to zero does **not** guarantee that the requests which arrive while the
deployment is parked succeed. Queuing them exists to produce the wake-up
signal; serving them is best effort:

- A cold start (GPU node provisioning + model load) can take many minutes and
  may outlast the queue. Once `epp.flowControl.defaultRequestTTL` (`600s`)
  expires the EPP rejects the request with `503`, which the modelharness
  gateway reports as `502 epp_unavailable` (`x-kaito-error-source: epp`); it
  fails earlier if the client times out or disconnects. Istio also bounds
  each EPP exchange to 1000s.
- When the first replica becomes Ready, the EPP (which watches Pods directly)
  can release the queued requests before the gateway has received the new
  endpoint (EndpointSlice → istiod → EDS). Requests dispatched in that window
  fail with `no healthy upstream`, which the modelharness gateway maps to
  `503 model_unavailable` with `Retry-After: 10`.

Clients should therefore treat `503 model_unavailable` and
`502 epp_unavailable` from a parked or waking deployment as retryable and
back off (honouring `Retry-After` when present); once the replica is Ready
and propagated, retries are served normally.

Things to keep in mind:

- keda-kaito-scaler scrapes the EPP pods on `epp.metricsPort` (`9090`) from
  its own namespace, which must be allowed by the modelharness
  `networkPolicy.allowedIngressNamespaces` (`keda` by default).
- Changing `minReplicas` from or to `0` restarts the EPP pod (its config is
  read at startup).
- Raising `minReplicas` from `0` on a parked deployment does not wake it: with
  no model pods the regenerated triggers error out and KEDA holds the count.
  Wake it first (send a request), or change `replicas` in the same upgrade,
  or run `kubectl scale inferenceset <name> --replicas=<n>`.

## Replica ownership

`replicas` is optional. When it is empty the chart does not render
`spec.replicas`: the InferenceSet starts at the CRD default of `1` and Helm
never touches the live count afterwards. With `enableScaling=true`, KEDA owns
that count (via the InferenceSet's scale subresource) and keeps it within
`[minReplicas, maxReplicas]`.

When `replicas` is set the chart renders `spec.replicas: <replicas>`. Helm 3
upgrades custom resources with a merge patch computed from the previous and
the new release manifest (live state is not consulted), so with
`enableScaling=true`:

- an upgrade that does not change `replicas` leaves the live count untouched;
- changing `replicas` sets the live count to the new value once, after which
  KEDA keeps adjusting it within `[minReplicas, maxReplicas]`;
- clearing `replicas` removes the field, so the CRD default (`1`) applies
  once before KEDA scales it back.

GitOps tools that diff against live state (e.g. Argo CD) report
`spec.replicas` as drifted when it is set and KEDA has scaled; leave
`replicas` empty, or ignore it the same way as for a Deployment managed by an
HPA (an Argo CD `ignoreDifferences` entry for `/spec/replicas` on
`kaito.sh/InferenceSet`).

> **Upgrading from chart versions without `minReplicas`:** with
> `enableScaling=true`, `minReplicas` and `maxReplicas` are now required.
> Previously `replicas` doubled as the lower bound and `maxReplicas`
> defaulted to `3`; pass them explicitly, e.g.
> `--set minReplicas=<previous replicas> --set maxReplicas=3`. Those versions
> did not render `spec.replicas` under autoscaling, so leaving `replicas`
> empty keeps the live count untouched on upgrade.

## Automatic base image upgrades

KAITO embeds the version of its base serving image in its controller.
When the controller is upgraded to a release bundling a newer base image, 
existing `InferenceSet` replicas keep running the old image until
they are recreated. Setting `autoUpgrade.enabled=true` renders
`spec.autoUpgrade.enabled: true` on the InferenceSet so the KAITO
controller detects the version drift and rolls the replicas onto the new
image one at a time — waiting for each replica to become Ready before
moving to the next — without recreating the InferenceSet. See the
[KAITO docs](https://kaito-project.github.io/kaito/docs/inference#automatic-base-image-upgrades).

Optionally restrict when rollouts may begin with a maintenance window
(`schedule` is a 5-field cron in UTC; `duration` defaults to `4h`):

```sh
helm install qwen ./charts/modeldeployment \
  --namespace my-models \
  --set name=qwen \
  --set model=qwen2-5-coder-7b-instruct \
  --set autoUpgrade.enabled=true \
  --set autoUpgrade.maintenanceWindow.schedule="0 2 * * 6" \
  --set autoUpgrade.maintenanceWindow.duration="4h"
```

> **Note:** auto-upgrade ALSO requires the `enableBaseImageAutoUpgrade`
> feature gate to be enabled on the KAITO controller (off by default).
> This chart value only opts the individual InferenceSet in; without the
> feature gate the controller ignores `spec.autoUpgrade`.

## Compatibility note

When this chart is used together with KAITO's controller, the controller's
`FeatureFlagGatewayAPIInferenceExtension` should be **disabled** — otherwise
KAITO will create a Flux `OCIRepository`/`HelmRelease` that renders a second
InferencePool/EPP set with the same name and conflict with the resources
rendered here.
