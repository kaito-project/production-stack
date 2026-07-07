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

## Inference latency profile

The shadow pod runs `llm-d-inference-sim`, configured with a latency calculator
so mocked endpoints behave closer to real vLLM serving instead of responding
instantly.

### Profiles (selected per InferenceSet)

Each shadow pod's baseline latency comes from a **latency profile** that mirrors
one of the "Suggested Default Profiles" / reference-table rows in the upstream
[latency-profiles.md](https://github.com/llm-d/llm-d-inference-sim/blob/main/docs/latency-profiles.md):

| Profile | Mirrors | Selected for (auto) | TTFT / ITL |
| --- | --- | --- | --- |
| `small-l40s` | Small model (1–3B) on L40S, low-latency edge | size `< 5B` | 110ms / 15ms |
| `8b-h100` | 8B-class model on H100, balanced load | `5B ≤ size < 10B` (and fallback when size can't be parsed) | 100ms / 12ms |
| `13b` | 13B model, H100-class balanced load | `10B ≤ size < 20B` | 180ms / 22ms |
| `30b-tp2` | 30–34B model on 2×H100 (TP=2) | `20B ≤ size < 50B` | 250ms / 30ms |
| `70b-tp8` | 70B model on 8×H100 (TP=8), throughput-optimized | `50B ≤ size < 150B` | 200ms / 25ms |
| `405b-tp8` | 405B model on 8×H100 (TP=8) | size `≥ 150B` | 900ms / 80ms |

By default the profile is chosen **automatically from the served model size**
parsed out of the model name (e.g. `...-8B...` ⇒ 8 billion parameters). An
InferenceSet can override the selection through pod-template annotations:

| Annotation | Values | Default |
| --- | --- | --- |
| `kaito.sh/latency-profile` | `auto`, `small-l40s`, `8b-h100`, `13b`, `30b-tp2`, `70b-tp8`, `405b-tp8` | `auto` (pick by model size) |
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
