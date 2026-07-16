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
(`model` non-empty, `replicas` / `maxReplicas` positive, and — when
`enableScaling=true` — at least one `scaling.metrics` entry). The
cross-field guard `maxReplicas >= replicas` and the non-empty
`scaling.metrics` requirement (neither expressible in JSON Schema alone)
are additionally enforced by fail-fast template guards when
`enableScaling=true`.

The EPP runs the [`llm-d-inference-scheduler`](https://github.com/llm-d/llm-d-inference-scheduler/tree/v0.7.1)
distribution with `--secure-serving=false`, so the Istio Gateway can reach it
over plaintext gRPC and **no `DestinationRule` is required**.

## Inputs

| Key                       | Required | Default                                                              | Description                                                                                |
| ------------------------- | -------- | -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| `name`                    | optional | `.Release.Name`                                                      | Deployment name. Used as InferenceSet name and as the `X-Gateway-Model-Name` header value. |
| `namespace`               | optional | `.Release.Namespace`                                                 | Target namespace.                                                                          |
| `model`                   | required | `""`                                                                 | Preset model name. Used only as `spec.template.inference.preset.name`.                     |
| `instanceType`            | required | `Standard_NV36ads_A10_v5`                                            | VM instance type for the underlying nodes.                                                 |
| `replicas`                | required | `1`                                                                  | InferenceSet replicas. Also wired to `scaledobject.kaito.sh/min-replicas`.                 |
| `enableScaling`           | optional | `false`                                                              | Wired to `scaledobject.kaito.sh/auto-provision`. Gates the entire `scaling` block.         |
| `maxReplicas`             | optional | `3`                                                                  | Wired to `scaledobject.kaito.sh/max-replicas` (only when `enableScaling=true`).            |
| `scaling.metrics`         | optional | `vllm:num_requests_waiting` gauge + `vllm:request_queue_time_seconds` histogram | Ordered list of scaling signals combined under the AND policy; rendered as a single `scaledobject.kaito.sh/metrics` annotation (a YAML list). At least one entry is required when `enableScaling=true`. |
| `scaling.metrics[].name`  | required | `vllm:num_requests_waiting`                                         | Prometheus metric family name. Rendered as the `name` field of a `scaledobject.kaito.sh/metrics` entry.            |
| `scaling.metrics[].type`  | optional | `gauge`                                                             | `gauge` (per-replica average) or `histogram` (per-pod quantile). Rendered as the `type` field of a `scaledobject.kaito.sh/metrics` entry. |
| `scaling.metrics[].upThreshold`   | required | _empty_                                                    | Per-replica scale-up threshold. Empty by default; a template guard rejects the install when unset (`enableScaling=true`). Rendered as the `upthreshold` field of a `scaledobject.kaito.sh/metrics` entry. |
| `scaling.metrics[].downThreshold` | required | _empty_                                                    | Per-replica scale-down threshold (MUST be `< upThreshold`). Empty by default; a template guard rejects the install when unset or `>= upThreshold` (`enableScaling=true`). Rendered as the `downthreshold` field of a `scaledobject.kaito.sh/metrics` entry. |
| `scaling.metrics[].quantile`      | optional | _empty_ → `0.95`                                            | Target quantile for `histogram` metrics; ignored for `gauge`. Rendered as the `quantile` field of a `scaledobject.kaito.sh/metrics` entry. |
| `scaling.evaluationWindow`| optional | `60`                                                                | Scale-up stabilization window (seconds). Wired to `scaledobject.kaito.sh/evaluationwindow`. |
| `scaling.scaleUpCooldown` | optional | `300`                                                               | Minimum seconds between scale-up steps. Wired to `scaledobject.kaito.sh/scaleupcooldown`.  |
| `scaling.scaleDownCooldown` | optional | `300`                                                             | Minimum seconds between scale-down steps. Wired to `scaledobject.kaito.sh/scaledowncooldown`. |
| `gatewayName`             | optional | _empty_ → `<namespace>-gw`                                          | Gateway the HTTPRoute attaches to. Defaults to the per-namespace Gateway provisioned by `charts/modelharness`. |
| `epp.image.repository`    | optional | `mcr.microsoft.com/oss/v2/llm-d/llm-d-inference-scheduler`           | EPP container image.                                                                       |
| `epp.image.tag`           | optional | `v0.7.1`                                                             | EPP image tag.                                                                             |
| `epp.image.pullPolicy`    | optional | `IfNotPresent`                                                       | EPP image pull policy.                                                                     |
| `epp.replicas`            | optional | `1`                                                                  | Number of EPP pods.                                                                        |
| `epp.modelServerPort`     | optional | `5000`                                                               | Port exposed by inference pods (`KAITO PortInferenceServer`).                              |
| `epp.extProcPort`         | optional | `9002`                                                               | EPP gRPC ext_proc port.                                                                    |
| `epp.healthPort`          | optional | `9003`                                                               | EPP gRPC health-check port.                                                                |
| `epp.metricsPort`         | optional | `9090`                                                               | EPP HTTP metrics port.                                                                     |
| `epp.logVerbosity`        | optional | `1`                                                                  | EPP `--v=<n>` log verbosity.                                                               |
| `epp.resources`           | optional | `requests: 100m / 256Mi, limits: 500m / 512Mi`                       | EPP container resources.                                                                   |
| `epp.modelServerSelector` | optional | `inferenceset.kaito.sh/created-by: ""`, `apps.kubernetes.io/pod-index: "0"` | InferencePool selector. Empty `created-by` value is auto-filled with `name`. |

## Naming conventions

- InferencePool: `<name>-inferencepool`
- EPP Deployment / Service / SA / Role / ConfigMap: `<name>-inferencepool-epp`
- HTTPRoute: `<name>-route`

## Example

Install into a workload namespace whose `Gateway` was provisioned by
[`charts/modelharness`](../modelharness) (Gateway name follows the
`<namespace>-gw` convention shared by both charts):

```sh
helm install qwen ./charts/modeldeployment \
  --namespace my-models \
  --set name=qwen \
  --set model=qwen2-5-coder-7b-instruct \
  --set replicas=2 \
  --set maxReplicas=5 \
  --set enableScaling=true \
  --set scalingThreshold=10
```

The rendered `HTTPRoute` parents into the `my-models-gw` `Gateway`
because `gatewayName` is left empty. Override `--set gatewayName=...`
only when attaching to a Gateway with a business-specific name.

## Compatibility note

When this chart is used together with KAITO's controller, the controller's
`FeatureFlagGatewayAPIInferenceExtension` should be **disabled** — otherwise
KAITO will create a Flux `OCIRepository`/`HelmRelease` that renders a second
InferencePool/EPP set with the same name and conflict with the resources
rendered here.

