# GPU Node Mocker

## Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Karpenter creates                           │
│                          NodeClaim                                 │
│                             │                                      │
│                             ▼                                      │
│               ┌─────────────────────────┐                          │
│               │  Phase 1: NodeClaim     │                          │
│               │     Reconciler          │                          │
│               └────────────┬────────────┘                          │
│                            │                                       │
│              ┌─────────────┼─────────────┐                         │
│              ▼             ▼             ▼                          │
│        ┌──────────┐ ┌──────────┐ ┌────────────┐                   │
│        │Fake Node │ │NodeClaim │ │   Lease    │                    │
│        │          │ │  Status  │ │ Heartbeat  │                    │
│        │fake://.. │ │Ready=True│ │ (10s loop) │                    │
│        │gpu labels│ │Registered│ │            │                    │
│        │gpu taint │ │Initialized│ │           │                    │
│        └──────────┘ └──────────┘ └────────────┘                   │
│                            │                                       │
│               KAITO sees GPU node ready                            │
│               → creates inference Pod                              │
│                            │                                       │
│                            ▼                                       │
│               ┌─────────────────────────┐                          │
│               │  Phase 2: ShadowPod     │                          │
│               │     Reconciler          │                          │
│               └────────────┬────────────┘                          │
│                            │                                       │
│              ┌─────────────┼─────────────┐                         │
│              ▼             ▼             ▼                          │
│        ┌──────────┐ ┌──────────┐ ┌────────────┐                   │
│        │Shadow Pod│ │Inference │ │ Annotation │                    │
│        │          │ │Pod Status│ │            │                    │
│        │llm-mocker│ │podIP=real│ │shadow-pod  │                    │
│        │on real   │ │Running   │ │  -ref      │                    │
│        │AKS node  │ │Ready=True│ │            │                    │
│        └──────────┘ └──────────┘ └────────────┘                   │
│                            │                                       │
│               KAITO sees inference pod running                     │
│               → traffic hits llm-mocker via real IP                │
└─────────────────────────────────────────────────────────────────────┘
```

## Phase 1 — Fake the infrastructure (NodeClaimReconciler)

- **Creates a fake Node** for each Karpenter NodeClaim — with `providerID: fake://...`, workspace labels, instance-type labels, `sku=gpu` taint, and `nvidia.com/gpu` in capacity. This makes KAITO think a GPU VM exists.
- **Patches the NodeClaim status** — sets `nodeName`, `providerID`, `Ready=True`, `Registered=True`, `Initialized=True`. This tells KAITO the NodeClaim is fulfilled so it proceeds to create inference pods.
- **Maintains a Lease heartbeat** — creates a Lease in `kube-node-lease` and renews it every 10 seconds in a background goroutine. This prevents the node-lifecycle-controller from marking the fake node as Unknown.

## Phase 2 — Fake the workload (ShadowPodReconciler)

- **Creates a shadow pod** for each inference pod that's Pending on a fake node — the shadow pod runs the `llm-mocker` image on a real AKS node and gets a real CNI IP.
- **Patches the inference pod's status** — copies the shadow pod's IP into the inference pod's `status.podIP`, sets `phase=Running`, `conditions[Ready]=True`, and builds fake `containerStatuses`. This makes KAITO think the inference pod is running.
- **Annotates the inference pod** with `kaito.sh/shadow-pod-ref` pointing to the shadow pod, so future reconciles can correlate them.

## Streaming probe

KAITO streaming Workspaces load weights from object storage via the Run:ai model
streamer. Weight loading is CPU-safe, so the shadow pod can validate that path
without a GPU — and because the shadow pod gates the mocked inference pod's
readiness, **"mock serving is Ready" also means "streaming is configured
correctly"**.

When the original pod is a streaming pod, the shadow pod clones its streaming
identity (ServiceAccount, workload-identity label, and the `fetch-sas`
credential-bootstrap init container plus the volumes it references) and runs a
probe init container that lists the model's `*.safetensors`, streams one shard
into CPU memory, and asserts at least one tensor is read.

The original pod sits on a fake node, so its init containers never execute and
the SAS env file they would have written does not exist — nor could it be
reused, since that volume is an `emptyDir` scoped to a single Pod. The shadow
pod therefore runs its **own** copy of `fetch-sas`, which mints a fresh SAS into
the shadow pod's own volume:

```
initContainers[0]  fetch-sas        (cloned spec; runs here for the first time)
initContainers[1]  streamer-probe   (sources the SAS env file, lists, streams)
containers[0]      llm-d-inference-sim
```

Detection reads the pod spec only — KAITO stamps streaming annotations on the
Workspace and never copies them onto the Pod. Modes:

| Mode | Trigger | Behaviour |
| --- | --- | --- |
| `sas` | a `fetch-sas` init container | clone identity + bootstrap, then probe |
| `direct-azure` | `--model az://…` | clone identity, then probe |
| `unsupported-scheme` | `--model s3://…` or `gs://…` | no probe (not deployable from these charts yet) |
| `none` | anything else | unchanged behaviour |

`--load-format` deliberately plays no part in detection: KAITO also sets
`--load-format=runai_streamer` for weights already on local disk, which is not
streaming.

**A failing probe blocks readiness indefinitely** — the shadow pod stays
Pending, so the mocked inference pod stays Pending too. That is the intended
trade-off, and there is deliberately no switch to skip the probe: a pod that
streams is a pod whose streaming should be verified, and mocking it as
download-at-runtime would report success for a path that was never exercised.
To opt out, disable streaming on the model itself (`streaming.disabled` in the
modeldeployment chart, which stamps `kaito.sh/model-streaming: "disabled"`), so
KAITO renders a non-streaming pod and there is nothing to probe.

The blocking container's reason and message are included in the controller's
retry log line.

### Requirements and caveats

- **Azure Workload Identity must be enabled** on the cluster
  (`az aks ... --enable-oidc-issuer --enable-workload-identity`). The probe
  relies on the mutating webhook to inject the AAD env vars and projected token.
- Shadow-pod nodes need egress to PyPI, `download.pytorch.org`, AAD/ARM and blob
  storage. The Run:ai streamer hard-depends on torch, so the probe installs the
  **CPU-only** wheel first to avoid pulling ~2.5 GB of CUDA packages.
- Shadow pods are built once and never mutated, so changing the probe
  configuration only affects shadow pods created afterwards. To refresh existing
  ones, delete them — the controller recreates them from current config:
  `kubectl delete pod -l kaito.sh/managed-by=gpu-mocker`.
- The probe container runs as root (`python:3.12-slim`) and the shadow pod gains
  a non-default ServiceAccount, init containers and a projected token. Under a
  `restricted` PodSecurity label the shadow pod would be rejected.
- **This controller is test-only.** It can create pods under any ServiceAccount,
  including one federated to a real cloud identity. Never run it in production.

## Inference latency profile

The shadow pod runs `llm-d-inference-sim`, configured with a latency calculator
so mocked endpoints behave closer to real vLLM serving instead of responding
instantly.

### Profiles (selected per InferenceSet)

Each shadow pod's baseline latency comes from a **latency profile** that mirrors
one of the three upstream profiles in
[manifests/latency-profiles](https://github.com/llm-d/llm-d-inference-sim/tree/main/manifests/latency-profiles)
(each ships as a separate constant-calculator and per-token-calculator manifest):

| Profile | Mirrors | Selected for (auto) | TTFT / ITL |
| --- | --- | --- | --- |
| `small-l40s` | Small model (1–3B) on L40S, low-latency edge | size `< 5B` | 110ms / 15ms |
| `8b-h100` | 8B-class model on H100, balanced load | `5B ≤ size < 25B` (and fallback when size can't be parsed) | 100ms / 12ms |
| `70b-tp8` | 70B model on 8×H100 (TP=8), throughput-optimized | size `≥ 25B` | 200ms / 25ms |

By default the profile is chosen **automatically from the served model size**
parsed out of the model name (e.g. `...-8B...` ⇒ 8 billion parameters). An
InferenceSet can override the selection through pod-template annotations:

| Annotation | Values | Default |
| --- | --- | --- |
| `kaito.sh/latency-profile` | `auto`, `small-l40s`, `8b-h100`, `70b-tp8` | `auto` (pick by model size) |
| `kaito.sh/latency-calculator` | `per-token`, `constant` | `per-token` |

```yaml
# InferenceSet / modeldeployment pod template
metadata:
  annotations:
    kaito.sh/latency-profile: 70b-tp8
    kaito.sh/latency-calculator: per-token
```

Unknown annotation values fall back to the default (`auto` /
`per-token`) and an unrecognized `kaito.sh/latency-calculator` value is logged as
a warning. `kaito.sh/latency-calculator` selects the model — `per-token`
(default, TTFT scales with prompt length) or `constant` (TTFT is a fixed value).
Only the fields for the selected calculator are written into the simulator
config.

### Operator-wide overrides (Helm values / flags)

The Helm values and CLI flags below are **operator-wide overrides**: each is
empty by default so the selected profile drives the value. Set one to force that
knob for every shadow pod regardless of profile. `latencyCalculator` /
`--latency-calculator` sets the default calculator used when a pod has no
`kaito.sh/latency-calculator` annotation (empty ⇒ `per-token`).

### Common settings (both calculators)

| Setting | Helm value (`shadowPod.*`) | Flag | Default |
| --- | --- | --- | --- |
| Latency calculator | `latencyCalculator` | `--latency-calculator` | profile / `per-token` |
| Inter-token latency | `interTokenLatency` | `--inter-token-latency` | profile value |
| Inter-token std-dev | `interTokenLatencyStdDev` | `--inter-token-latency-std-dev` | profile value |
| Time factor under load | `timeFactorUnderLoad` | `--time-factor-under-load` | profile value |

### `constant` calculator (TTFT is a fixed value)

| Setting | Helm value (`shadowPod.*`) | Flag | Default |
| --- | --- | --- | --- |
| Time to first token | `timeToFirstToken` | `--time-to-first-token` | profile value |
| TTFT std-dev | `timeToFirstTokenStdDev` | `--time-to-first-token-std-dev` | profile value |
| KV-cache transfer latency | `kvCacheTransferLatency` | `--kv-cache-transfer-latency` | profile value |
| KV-cache transfer std-dev | `kvCacheTransferLatencyStdDev` | `--kv-cache-transfer-latency-std-dev` | profile value |

### `per-token` calculator (TTFT scales with prompt length)

| Setting | Helm value (`shadowPod.*`) | Flag | Default |
| --- | --- | --- | --- |
| Prefill overhead | `prefillOverhead` | `--prefill-overhead` | profile value |
| Prefill time per token | `prefillTimePerToken` | `--prefill-time-per-token` | profile value |
| Prefill time std-dev | `prefillTimeStdDev` | `--prefill-time-std-dev` | profile value |
| KV-cache transfer time per token | `kvCacheTransferTimePerToken` | `--kv-cache-transfer-time-per-token` | profile value |
| KV-cache transfer time std-dev | `kvCacheTransferTimeStdDev` | `--kv-cache-transfer-time-std-dev` | profile value |

Override per-deployment to mimic other model/hardware combinations, e.g. force a
70B TP=8 throughput profile via annotation:

```yaml
# InferenceSet pod template
metadata:
  annotations:
    kaito.sh/latency-profile: 70b-tp8
```

Or pin individual knobs operator-wide (applies to every shadow pod):

```yaml
shadowPod:
  latencyCalculator: constant
  timeToFirstToken: 200ms
  interTokenLatency: 25ms
  timeFactorUnderLoad: "3.0"
```
