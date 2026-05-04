# modeldeployment

Helm chart that deploys a complete set of GAIE (Gateway API Inference
Extension) artifacts for a single model:

- `kaito.sh/v1alpha1` `InferenceSet`
- `inference.networking.k8s.io/v1` `InferencePool` (normally provisioned by KAITO via Flux; rendered inline here)
- Endpoint Picker (EPP) `Deployment` + `Service` + `ServiceAccount` + `Role` + `RoleBinding` + `ConfigMap`
- `gateway.networking.k8s.io/v1` `HTTPRoute` matching `X-Gateway-Model-Name: <name>`

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
| `replicas`                | required | `2`                                                                  | InferenceSet replicas. Also wired to `scaledobject.kaito.sh/min-replicas`.                 |
| `enableScaling`           | optional | `false`                                                              | Wired to `scaledobject.kaito.sh/auto-provision`.                                           |
| `maxReplicas`             | optional | `5`                                                                  | Wired to `scaledobject.kaito.sh/max-replicas` (only when `enableScaling=true`).            |
| `scalingThreshold`        | optional | `10`                                                                 | Wired to `scaledobject.kaito.sh/threshold` (only when `enableScaling=true`).               |
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

