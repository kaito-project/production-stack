---
title: Control-Plane Error Reporting Across Cluster, Modelharness, and Modeldeployment Levels
authors:
  - "@rambohe-ch"
reviewers:
  - "@Fei-Guo"
  - "@zhuangqh"
  - "@tnsimon"
  - "@techworldhello"
creation-date: 2026-05-19
last-updated: 2026-10-21
status: implementable
see-also:
  - "https://github.com/kaito-project/production-stack/issues/71"
  - "/docs/proposals/20260519-data-plane-error-handling.md"
replaces: []
superseded-by: []
---

# Control-Plane Error Reporting Across Cluster, Modelharness, and Modeldeployment Levels

## Table of Contents

- [Control-Plane Error Reporting Across Cluster, Modelharness, and Modeldeployment Levels](#control-plane-error-reporting-across-cluster-modelharness-and-modeldeployment-levels)
  - [Table of Contents](#table-of-contents)
  - [Glossary](#glossary)
  - [Summary](#summary)
  - [Motivation](#motivation)
    - [Goals](#goals)
    - [Non-Goals/Future Work](#non-goalsfuture-work)
  - [Proposal](#proposal)
    - [Implementation Details/Notes/Constraints](#implementation-detailsnotesconstraints)
      - [Error category overview](#error-category-overview)
      - [1. Control-plane errors](#1-control-plane-errors)
        - [1.1 Event schema](#11-event-schema)
        - [1.2 Reason catalogue](#12-reason-catalogue)
        - [1.3 Emission model](#13-emission-model)
        - [1.4 Model download observation](#14-model-download-observation)
      - [3. Requirements](#3-requirements)
    - [User Stories](#user-stories)
      - [Story 1 — Operator diagnoses a stuck modeldeployment](#story-1--operator-diagnoses-a-stuck-modeldeployment)
      - [Story 2 — Operator diagnoses a harness-local misconfiguration](#story-2--operator-diagnoses-a-harness-local-misconfiguration)
      - [Story 3 — Operator diagnoses a broken cluster install](#story-3--operator-diagnoses-a-broken-cluster-install)
      - [Story 4 — Operator diagnoses a slow model download](#story-4--operator-diagnoses-a-slow-model-download)
  - [Alternatives](#alternatives)
  - [Test Plan](#test-plan)
  - [Implementation History](#implementation-history)

## Glossary

- **modelharness**: The Helm release rendered by [`charts/modelharness`](../../charts/modelharness) — one per workload namespace. Provisions the namespace `Gateway`, the per-namespace dataplane `EnvoyFilter`s (`bbr-ext-proc`, `model-not-found-direct`, `gateway-filter-outage-local-reply`, plus `apikey-ext-authz` when auth is enabled), the `APIKey` CR, and the `CiliumNetworkPolicy` resources (requires the cluster Cilium dataplane; see `charts/modelharness` for rationale).
- **modeldeployment**: The Helm release rendered by [`charts/modeldeployment`](../../charts/modeldeployment) — one `InferenceSet`, one `InferencePool`, one EPP `Deployment`/`Service`/RBAC/`ConfigMap`, and one `HTTPRoute`, parented to the per-namespace `Gateway`.
- **EPP**: Endpoint Picker — per-model `llm-d-inference-scheduler` ext_proc pod that performs KV-cache aware routing.
- **BBR**: Body-Based Router — the cluster-wide ext_proc filter (in the umbrella release namespace, `kaito-system` by default) that parses request bodies and injects the `X-Gateway-Model-Name` header.
- **InferenceSet**: `kaito.sh/v1beta1` CR owned by the KAITO controller; the canonical declaration of one model deployment. KAITO renders one child `Workspace` per replica and aggregates their root-cause conditions onto `InferenceSet.status.conditions[InferenceSetReady]`.
- **ModelMirror**: `kaito.sh/v1alpha1` namespaced CR owned by KAITO that downloads a model's weights once into a PVC for model streaming. It is created by the KAITO workspace controller and shared by all Workspaces in the same namespace that serve the same model.

## Summary

Production-stack today has no coherent end-to-end error story. Failures occur at three distinct layers — cluster bootstrap (KAITO / Istio / KEDA / BBR / keda-kaito-scaler / `llm-gateway-auth` / local CSI driver), per-namespace harness setup (`Gateway`), and per-model deployment (`InferenceSet`, EPP, model download) — and each surface emits errors in its own format on its own object.

Errors are organised into two top-level categories: **control-plane errors** (this proposal) and **data-plane errors** (covered by the companion [data-plane error handling proposal](./20260519-data-plane-error-handling.md)). This proposal addresses control-plane errors at all three levels:

- **Control-plane errors** — failures observable inside the cluster, covering install-time failures **and** post-install runtime regressions (a shared Deployment crashing, a Gateway failing to program, an EPP pod entering `CrashLoopBackOff`, an `InferenceSet` failing to provision or start, model weights downloading too slowly, etc.). Surfaced **exclusively as Kubernetes `Event`s published to the `kube-system` namespace** by a single leader-elected `productionstack-status-reporter` workload shipped with the umbrella chart. Component-local status fields (`InferenceSet.status`, `Gateway.status`, `Deployment.status`) are preserved for per-component diagnosis, but the production-stack-level cross-layer view lives entirely in the event stream.

## Motivation

Production-stack is built from independent OSS components (Istio Gateway, `llm-gateway-auth`, BBR, EPP, KAITO `InferenceSet`, KEDA, keda-kaito-scaler). Each component does its own error reporting in its own format and on its own resource, which means:

- **For operators.** Diagnosing "why is my model not ready?" requires walking objects across three layers and four-plus namespaces (`kaito-workspace`, `istio-system`, `llm-gateway-auth`, the workload namespace) and correlating events by timestamp. There is no single, cross-layer event stream operators can `kubectl get` to see the current state.
- **For documentation.** Without stable `Reason` strings, TSG-1 (control-plane errors, all three layers) — one of the two deliverables called out in #71 — cannot be deep-linked. (TSG-2, data-plane errors, is covered by the [data-plane proposal](./20260519-data-plane-error-handling.md).)

### Goals

- Define a control-plane-error taxonomy that covers cluster, modelharness, and modeldeployment levels, with every failure surfaced as a Kubernetes `Event` in the `kube-system` namespace.
- Periodically check the runtime health of every component production-stack installs or depends on, every deployed modelharness, and every deployed modeldeployment; report unhealthy components while they remain unhealthy, and let the Events age out after recovery.
- Detect slow model downloads on both download paths KAITO supports: vLLM self-download inside the model pod, and `ModelMirror`.
- Publish TSG-1 (control-plane errors, covering cluster + modelharness + modeldeployment) — **internal-only**, keyed off the `reason` strings defined here.

### Non-Goals/Future Work

- Per-level aggregator controllers. We deliberately reuse Kubernetes `Event`s (rolled up into `kube-system` by the `productionstack-status-reporter` Deployment shipped with the umbrella chart) plus existing per-component status fields (`InferenceSet.status`, `Gateway.status`, `Deployment.status`).
- Detecting configuration drift of installed resources (CRDs, workload specs, `EnvoyFilter`s, etc.). The deploying party periodically re-syncs these resources and overwrites any modification, so the reporter checks runtime health only.
- Checking modelharness objects that carry no runtime status of their own (`EnvoyFilter`, `APIKey`, `CiliumNetworkPolicy`, `ReferenceGrant`, `RequestAuthentication`, `PodMonitor`). They are kept in sync by the deploying party per the previous item.
- Node-level health (resource pressure, GPU faults). Other system components such as Node Problem Detector own it.
- Recovery Events. A healthy state is the absence of a freshly refreshed `Warning`; stale Events age out with the cluster's Event TTL.
- Support for `GET /v1/models` and `GET /v1/models/{id}`; out of scope for this proposal.
- Centralised logging / alerting infrastructure; we reuse Kubernetes events, CR status, and Prometheus metrics, per #71 non-goals.
- Errors that originate strictly outside the stack (e.g. cloud-provider AKS cluster-creation failure) are surfaced only as preconditions — production-stack does not own their root-cause remediation, only their detection and TSG cross-link.

## Proposal

### Implementation Details/Notes/Constraints

#### Error category overview

Every error in production-stack belongs to exactly one of two categories, and within each category is owned by exactly one of the three layers. This proposal covers the control-plane category; the data-plane category is covered by the [data-plane error handling proposal](./20260519-data-plane-error-handling.md#error-category-overview).

| Category | Cluster level | Modelharness level | Modeldeployment level |
| --- | --- | --- | --- |
| **Control-plane errors** (Kubernetes `Event`s in `kube-system`; install-time failures **and** post-install runtime regressions) | - installed component startup or runtime failure (BBR, keda-kaito-scaler, `llm-gateway-auth`, KAITO workspace controller, local CSI driver)<br>- required external dependency readiness (Istio, KEDA) | - namespace `Gateway` acceptance/programming regression<br>- programmed Gateway data-plane pod unavailable | - `InferenceSet` failure reported by KAITO on `InferenceSetReady`<br>- EPP startup or runtime regression<br>- slow model download (vLLM self-download or `ModelMirror`) |

The sections below enumerate the unified taxonomy for all three layers in a single table, then describe the emission model, the model-download observation, and the per-component changes required.

#### 1. Control-plane errors

Control-plane errors are surfaced **exclusively through Kubernetes `Event`s published to the `kube-system` namespace**, by a single `productionstack-status-reporter` Deployment shipped with the umbrella chart. The rolled-up cross-layer view lives entirely in the event stream. Component-local status (`InferenceSet.status.conditions[]`, `Gateway.status.conditions[]`, `Deployment.status.conditions[]`) is preserved unchanged for component-local diagnosis, but the production-stack-level taxonomy in §1.2 is emitted only as events. Operators consume the entire taxonomy with one query:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter \
  --sort-by=.lastTimestamp
```

Each event carries a `reason` defined in §1.2. The event stream covers all three layers (cluster, modelharness, modeldeployment), install-time misconfiguration, post-install runtime regressions, and the pre-Ready warning path (`inferencesetModelDownloadSlow`).

##### 1.1 Event schema

Every control-plane event MUST follow the schema below.

| Field | Value |
| --- | --- |
| `metadata.namespace` | `kube-system` (always — regardless of which layer or which workload namespace produced the underlying condition) |
| `type` | Always `Warning` — the reporter only publishes problems; a healthy state is the absence of a freshly refreshed `Warning`, not a positive event. |
| `reason` | One of the reasons in §1.2: a prefixed string owned by this proposal, or — for `InferenceSet` failures — the reason KAITO reports on `InferenceSetReady`, passed through verbatim. |
| `source.component` | `productionstack-status-reporter` |
| `involvedObject` | The cluster-scoped `Namespace` that contains the problematic resource: the workload `Namespace` for harness/modeldeployment reasons, or the component's install `Namespace` such as `istio-system`, `kaito-workspace`, or `keda` for cluster-layer reasons. Kubernetes requires the event's `metadata.namespace` to match the `involvedObject`'s namespace for namespaced resources, so a namespaced failing object cannot be referenced cross-namespace from `kube-system`. The specific failing resource name is carried in `message` so operators can still pivot to `kubectl describe`. |
| `message` | Human-readable description of the failure. The message MUST identify the affected workload namespace and (for modeldeployment-layer reasons) the `InferenceSet` name, so operators can pivot directly. For cluster-layer reasons the message MUST also identify the specific failing namespaced resource (e.g. the `Deployment` name and its install namespace), since `involvedObject` only names the containing `Namespace`. The message MUST NOT carry internal-only links (e.g. TSG URLs). |

**Event lifecycle.** The reporter derives a stable Event name from the logical subject (`GroupKey`) and reason.

- **Active.** The first observation of an active finding creates the Event. While the finding remains active, the reporter refreshes the same Event — incrementing `count` and updating `lastTimestamp` and `message` — at most once every `eventRefreshSeconds` (default **300 s**). The interval MUST be shorter than half the cluster's Event TTL (kube-apiserver `--event-ttl`, default 1 h), i.e. `< 1800 s`, so a long-lived problem never ages out while it is still active.
- **Recovered.** Once the finding is no longer active — including a model download that has completed — the reporter stops refreshing the Event. No recovery Event is emitted, and the retained Event ages out with the Event TTL.

An Event whose `lastTimestamp` is no older than roughly `eventRefreshSeconds` therefore denotes a problem that is still active.

##### 1.2 Reason catalogue

The single table below lists every reason the reporter emits. The `Layer` column is informational.

- **Cluster and modelharness layers.** The `reason` strings form a closed, layer-prefixed vocabulary (`cluster*` / `modelharness*`) owned by this proposal.
- **Modeldeployment layer.** Two prefixed reasons are owned by this proposal: `inferencesetEPPNotReady` and `inferencesetModelDownloadSlow`. Every other `InferenceSet` failure is delegated to KAITO: the reporter passes through the reason KAITO reports on `InferenceSetReady`.

Prefixed reasons map to TSG-1 anchors. Pass-through reasons map to the KAITO reason documentation, which TSG-1 links to.

| Layer | `reason` | `type` | Triggered by | Detection source | `involvedObject` |
| --- | --- | --- | --- | --- | --- |
| Cluster | `clusterBBRNotReady` | Warning | `body-based-routing` subchart Deployment NotReady: `ImagePullBackOff`, RBAC errors, runtime crash, scale-to-zero | `Deployment.status` of the `body-based-router` Deployment | the `body-based-routing` install `Namespace` (default `kaito-system`; the BBR `Deployment` is identified in `message`) |
| Cluster | `clusterKedaKaitoScalerNotReady` | Warning | `keda-kaito-scaler` subchart Deployment NotReady (install-time or runtime) | `Deployment.status` of the `keda-kaito-scaler` Deployment | the `keda-kaito-scaler` install `Namespace` (the `Deployment` name is identified in `message`) |
| Cluster | `clusterGatewayAuthNotReady` | Warning | Any configured `llm-gateway-auth` Deployment is missing or NotReady:<br>- `apikey-authz`, the ext_authz server<br>- `apikey-operator`, which reconciles `APIKey` CRs; while it is down, new or rotated keys stop taking effect<br>- `azure-authz`, when Azure auth is enabled | `Deployment.status` and backing Pod readiness | the `llm-gateway-auth` install `Namespace` (the failing `Deployment` is identified in `message`) |
| Cluster | `clusterIstioControlPlaneNotReady` | Warning | The configured `istiod` Deployment is missing or NotReady | `Deployment.status` and backing Pod readiness | `Namespace/istio-system` (the `istiod` `Deployment` is identified in `message`) |
| Cluster | `clusterKaitoControllerNotReady` | Warning | KAITO workspace controller `Deployment` NotReady | `Deployment.status` of KAITO controller | the KAITO controller install `Namespace` (default `kaito-workspace`; the controller `Deployment` is identified in `message`) |
| Cluster | `clusterKedaNotReady` | Warning | KEDA control plane components NotReady: `keda-operator` and `keda-operator-metrics-apiserver` (in the `keda` namespace, regardless of whether KEDA is installed as a managed add-on or via upstream Helm) | `Deployment.status` of `keda-operator` and `keda-operator-metrics-apiserver` | `Namespace/keda` (the failing `Deployment` is identified in `message`) |
| Cluster | `clusterLocalCSIDriverNotReady` | Warning | The local CSI driver installed by the KAITO workspace chart is unhealthy. This is the `local-csi-driver` subchart; it backs the `kaito-local-nvme-disk` StorageClass that KAITO uses to cache model weights on NVMe GPU nodes. The reason fires when either:<br>- the `csi-local-manager` Deployment is missing or NotReady, or<br>- any `csi-local-node` DaemonSet Pod is not Ready.<br>DaemonSet Pods on nodes created by `gpu-node-mocker` are excluded, because those nodes are simulated and cannot run the driver.<br>The check is enabled by default and configured via `controlPlane.localCSIDriver.{namespace,managerDeployment,nodeDaemonSet}` (defaults `kube-system` / `csi-local-manager` / `csi-local-node`). Clearing the values disables it. | `Deployment.status` of the manager and per-Pod readiness of the node DaemonSet | the local CSI driver install `Namespace` (default `kube-system`; the failing `Deployment`/`DaemonSet` and, for node Pods, the Node name are identified in `message`) |
| Modelharness | `modelharnessGatewayClassMissing` | Warning | `Gateway.status.conditions[Accepted]=False` with `Reason=NoMatchingParent`, `InvalidParameters`, or `UnsupportedValue`, indicating that the configured GatewayClass cannot accept the Gateway | periodic read of the top-level Gateway `Accepted` condition | the workload `Namespace` (the `Gateway` name is identified in `message`) |
| Modelharness | `modelharnessGatewayProgrammingFailed` | Warning | The top-level Gateway `Programmed` condition is `False` | periodic read of `Gateway.status.conditions[Programmed]`; debounced via a fixed 5-minute grace window | the workload `Namespace` (the `Gateway` name is identified in `message`) |
| Modelharness | `modelharnessGatewayDataPlaneNotReady` | Warning | The `Gateway` is `Programmed=True`, but a discovered modelharness-owned backing Deployment has no Ready pod | list Deployments labelled `gateway.networking.k8s.io/gateway-name=<gateway>` and `kaito.sh/owned-by=modelharness`, then inspect backing Pod readiness; debounced via a fixed 5-minute grace window | the workload `Namespace` (the `Gateway` name and scheduling/readiness cause are identified in `message`) |
| Modeldeployment | *pass-through* — the `InferenceSetReady` reason (e.g. `QuotaExceeded`, `CapacityUnavailable`, `ImagePullError`, `ContainerCrashLoopBackOff`, `ContainerOOMKilled`, `DownloadFailed`) | Warning | KAITO reports `InferenceSet.status.conditions[InferenceSetReady]=False` with a failure reason. KAITO already aggregates the root cause from the child Workspaces, covering node provisioning, inference workload, and `ModelMirror` failures.<br>**Not reported:** in-progress reasons, as determined by KAITO's `consts.IsProgressReason`.<br>**Timing:**<br>- reasons for which KAITO's `consts.IsTerminatedReason` is true are reported immediately;<br>- other failure reasons are reported after a fixed 3-minute debounce anchored on the condition's `lastTransitionTime`. | periodic read of `InferenceSet.status.conditions[InferenceSetReady]` | the workload `Namespace`. `message` is `InferenceSet <ns>/<name>: ` followed by the KAITO condition message, which already names up to three failing Workspaces with their condition type, reason, and message. |
| Modeldeployment | `inferencesetEPPNotReady` | Warning | Install-time: EPP image pull failure, malformed `ConfigMap`, RBAC missing for list pods, `--pool-name` mismatch. Runtime: EPP crash / restart loop / readiness-probe regression after the pod was previously Ready. | EPP `Deployment.status.conditions` + Pod state | the workload `Namespace` (the owning `InferenceSet` name and the EPP `Deployment` name are identified in `message`) |
| Modeldeployment | `inferencesetModelDownloadSlow` | Warning | Sustained slow model download while the model is not yet downloaded.<br>**Rule:** every throughput sample in a sliding window must be below the threshold. The window defaults to **60 s** (`controlPlane.weightDownload.windowSeconds`); the threshold defaults to **20 MB/s** (`controlPlane.weightDownload.minMBps`).<br>- One sample at or above the threshold inside the window suppresses the event, so transient dips never trigger it.<br>- The window MUST be fully populated (≥ `windowSeconds` and ≥ 2 samples) before deciding.<br>**Scope:** covers both KAITO download paths (§1.4). It reports slowness only; download failures surface through the pass-through `InferenceSetReady` reasons (e.g. `DownloadFailed`, `DownloadOOMKilled`). | the per-InferenceSet `WeightDownloader` selected per §1.4: the model-pod Prometheus metric (vLLM self-download) or `ModelMirror.status.download` (model streaming) | the workload `Namespace` (the owning `InferenceSet`, the download source — model `Pod` or `ModelMirror` —, window, and worst observed throughput are identified in `message`) |

The reporter is the single producer of these Events in `kube-system`; no other component MAY publish Events with `source.component=productionstack-status-reporter`.

##### 1.3 Emission model

The reporter evaluates the cluster, modelharness, modeldeployment, and model-download layers independently on a periodic full resync (default 60 seconds), then emits **every** active reason as its own Event according to the lifecycle in §1.1. There is no single-primary selection, priority collapse, or cross-layer suppression. Concurrent faults surface independently so one problem is never hidden behind another. Within a layer, findings are grouped by logical subject and de-duplicated so each reason is written at most once per subject per resync.

Notes:

- `inferencesetModelDownloadSlow` is emitted **in addition to** any other active modeldeployment reason, because its remediation (improve registry/network/cache throughput) is independent of the other failure modes.
- When **no** unhealthy reason is active for a layer, no event is emitted — a healthy state is represented by the absence of a freshly refreshed `Warning`, not by a positive event.
- Evaluators perform periodic Kubernetes API reads rather than registering informer-driven watches. A lookup that cannot be evaluated is treated as unknown and does not emit a warning for that pass.
- Startup transients are gated by the global startup grace (default 60 seconds), a reason-specific override, or a terminal-failure exemption. Findings with a known transition/creation timestamp use object-age gating; findings without one use an in-memory first-observation debounce.

##### 1.4 Model download observation

KAITO's workspace controller decides how each Workspace obtains its model weights (`modelstreaming.ModelStreamingEnabled` and `preset_inferences.go`). There are three paths:

| Path | When KAITO takes it | Where download progress is observable |
| --- | --- | --- |
| vLLM self-download | model streaming disabled and local weights not used | the `kaito_model_download_speed_bytes_per_second` gauge exposed by the not-yet-Ready model pod |
| `ModelMirror` | model streaming enabled | `ModelMirror.status.download.speedBytesPerSecond` |
| Local weights | `kaito.sh/use-local-weights: "true"` | none — weights are read from the node; nothing is downloaded |

Model streaming is enabled when all of the following hold:

- KAITO's `ModelStreaming` feature gate is on.
- The Workspace is not annotated `kaito.sh/model-streaming: "disabled"`.
- The runtime is vLLM, i.e. `kaito.sh/runtime` is not `transformers`.

The feature gate is a KAITO controller flag and is not visible on any Kubernetes object, so the reporter takes it as configuration: `controlPlane.weightDownload.modelStreamingEnabled`. It defaults to `true` and MUST match KAITO's `ModelStreaming` feature gate; for example, E2E sets it to `false`.

**Downloader selection.** For each `InferenceSet`, the reporter reads `spec.template.metadata.annotations`, which KAITO propagates to every child Workspace, and picks a downloader:

1. If `kaito.sh/use-local-weights` is `"true"`, skip the InferenceSet; nothing is downloaded.
2. If `modelStreamingEnabled` is `true`, `kaito.sh/model-streaming` is not `"disabled"`, and `kaito.sh/runtime` is not `transformers`, use the `ModelMirror` downloader.
3. Otherwise, use the vLLM model-pod downloader.

**Downloader interface.** Each download path implements one interface:

```go
type WeightDownloader interface {
    // Observe reports the current model-download state of one InferenceSet.
    Observe(ctx context.Context, is *unstructured.Unstructured) (DownloadStatus, error)
}

type DownloadStatus struct {
    Phase          DownloadPhase // NotStarted | Downloading | Completed — decided by the downloader
    BytesPerSecond float64       // valid only when Phase == Downloading; the slowest active source
    Source         string        // e.g. "model-pod <pod>" or "modelmirror <ns>/<name>"
}
```

Each downloader aggregates its sources into one status for the InferenceSet, using the slowest active source. As a result, the evaluator keeps exactly one sliding window per InferenceSet, and the Event subject is always the InferenceSet regardless of download path.

- **vLLM model-pod downloader.**
  - **Sources:** lists the InferenceSet's model pods (`inferenceset.kaito.sh/created-by=<name>`) and scrapes `/metrics` on the pods that are not yet Ready. The metric name and port are `controlPlane.weightDownload.metricName` and `controlPlane.weightDownload.metricPort` (defaults `kaito_model_download_speed_bytes_per_second` / `5000`).
  - **`Completed`:** pods exist and every pod is Ready or already exposes `vllm:*` metrics, meaning its weights are loaded.
  - **`Downloading`:** at least one pod reports the download metric.
  - **`NotStarted`:** any other case.
- **`ModelMirror` downloader.**
  - **Locating the mirror:** computes the mirror's key exactly as KAITO does (`modelstreaming.ModelMirrorKey`). The namespace is the InferenceSet's namespace. The name is the first 6 hex characters of the SHA-256 of the resolved model ID, which is `custom-<config>` for the `custom` preset, or otherwise the preset name mapped through KAITO's legacy short-name catalogue.
  - **Reading it:** reads the `kaito.sh/v1alpha1` `ModelMirror` through the dynamic client as unstructured, with no compile-time dependency on the KAITO `ModelMirror` Go type.
  - **`Completed`:** `status.phase=Ready`. This includes static mirrors, which become Ready immediately.
  - **`Downloading`:** `status.phase=Pending` and `status.failureMessage` is empty and `status.download` is present and `status.download.speedBytesPerSecond > 0` and `status.download.remainingSeconds != 0`.
  - **`NotStarted`:** any other case — the mirror is not found yet, the download Job is not running, or the last download failed. A failure is reported through the pass-through `InferenceSetReady` reason, not by this downloader.
  - **Speed semantics:** `speedBytesPerSecond` is KAITO's cumulative average since the download started, updated roughly every 10 s. It therefore reacts more slowly than an instantaneous rate.

**Evaluator.** Per InferenceSet and per resync, the evaluator calls `Observe` on the selected downloader and acts on the result:

| Result | Action |
| --- | --- |
| `Downloading` | Append a sample to the InferenceSet's window. |
| `NotStarted` | Add no sample. |
| `Completed` | Clear the window. The finding becomes inactive, so the Event stops being refreshed. |
| Error | Treat the InferenceSet as unknown for that pass. |

The window rule from §1.2 is applied identically for both downloaders.

#### 3. Requirements

This section enumerates the requirements that any implementing PR MUST satisfy, grouped by the component that owns the change. Concrete code shape (file paths, struct definitions, template names, RBAC verb lists) is left to the component owners. Data-plane requirements on the same components are listed in the [data-plane proposal](./20260519-data-plane-error-handling.md#3-requirements).

| Component | Requirements |
| --- | --- |
| `productionstack-status-reporter` (Deployment owned by `charts/productionstack`; HA, leader-elected, read-only API access except Event aggregation and leader-election Leases; no new CRD) | - **Single producer**: MUST be the sole producer of the §1.2 reason catalogue as Kubernetes `Event`s in `kube-system`. No other component MAY emit those Events.<br>- **Namespace discovery**: MUST discover managed workload namespaces via `productionstack.kaito.sh/managed-by=modelharness`. No static workload-namespace list MAY be required.<br>- **Periodic evaluation**: MUST evaluate each layer independently on a full resync (default 60 seconds) using read-only Kubernetes API lookups. The checks MUST cover both install-time failures and post-install runtime regressions. MUST NOT check the configuration of installed resources (see Non-Goals).<br>- **Independent findings**: MUST emit every active reason independently, with no priority collapse or cross-layer suppression.<br>- **Event lifecycle**: MUST derive a deterministic Event identity per logical subject and reason. While the finding remains active, MUST refresh the Event (`count`, `lastTimestamp`, `message`) at most once every `eventRefreshSeconds`, which defaults to 300 and MUST be `< 1800`. On recovery, MUST stop refreshing the Event rather than delete it, and MUST NOT emit a recovery Event.<br>- **Startup gating**: MUST support a configurable global startup grace, per-reason overrides, first-observation debounce for findings without a timestamp, and immediate emission for classified terminal failures.<br>- **No TSG URLs in event messages**: control-plane Event `message`s MUST NOT embed TSG URLs; TSG anchoring MUST be keyed off the `reason` string (§1.2).<br>- **Read-only KAITO coupling**: MUST consume `InferenceSet`, `Workspace`, and `ModelMirror` state read-only. MUST read `ModelMirror` through the dynamic client as unstructured. MUST classify `InferenceSetReady` reasons with KAITO's `consts.IsProgressReason` / `consts.IsTerminatedReason` rather than maintaining its own reason table. No new condition Types on `InferenceSet.status` MAY be required by this proposal.<br>- **Model download**: MUST implement the `WeightDownloader` interface for both the vLLM model-pod path and the `ModelMirror` path, and MUST select between them per InferenceSet as described in §1.4.<br>- **Configuration**: the reporter subchart MUST expose and validate `eventRefreshSeconds`, `controlPlane.localCSIDriver.{namespace,managerDeployment,nodeDaemonSet}`, and `controlPlane.weightDownload.{windowSeconds,minMBps,metricName,metricPort,modelStreamingEnabled}`. Validation MUST require positive `windowSeconds` and `minMBps`, and `eventRefreshSeconds < 1800`. |
| `charts/modelharness` | - **Labelling**: MUST stamp `productionstack.kaito.sh/managed-by: modelharness` on the workload `Namespace` (so the reporter can discover it via label selector) and `kaito.sh/owned-by: modelharness` on every harness-owned object. |
| `charts/modeldeployment` | - **Labelling**: MUST stamp `kaito.sh/inferenceset: <name>` and `kaito.sh/owned-by: modeldeployment` on every chart-owned object (EPP `Deployment` / `Service`, `HTTPRoute`, `InferencePool`, `ConfigMap`). |

### User Stories

#### Story 1 — Operator diagnoses a stuck modeldeployment

An operator installs a modeldeployment in workload namespace `my-models` in a region where the requested `instanceType` has zero quota. They run:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter,involvedObject.name=my-models \
  --sort-by=.lastTimestamp
```

and see:

```
LAST SEEN   TYPE      REASON          OBJECT                 MESSAGE
12s         Warning   QuotaExceeded   Namespace/my-models    InferenceSet my-models/qwen: inferenceset is not ready, 0/1 replicas are ready; workspace qwen-0 NodeClaimReady=False (QuotaExceeded): quota exceeded for Standard_NV36ads_A10_v5 in eastus.
```

**Why the Event looks like this:**
- **Reason.** `QuotaExceeded` is the reason KAITO reports on `InferenceSetReady`, passed through verbatim. Because KAITO classifies it as terminal, it is reported without waiting for the debounce.
- **Object.** The `involvedObject` is the cluster-scoped workload `Namespace`, per the §1.1 schema constraint that events in `kube-system` cannot reference namespaced resources cross-namespace.
- **Message.** It carries the `InferenceSet` and the failing Workspace, so the operator can pivot directly with `kubectl describe inferenceset -n my-models qwen`.

While the quota problem persists, the same Event keeps being refreshed every `eventRefreshSeconds`.

#### Story 2 — Operator diagnoses a harness-local misconfiguration

An operator supplies an invalid `gatewayClassName` for the workload namespace `my-models`, so the Gateway reports `Accepted=False` with `Reason=NoMatchingParent`. They run:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter,reason=modelharnessGatewayClassMissing \
  --sort-by=.lastTimestamp
```

and see one harness-local event scoped to the affected workload namespace:

```
LAST SEEN   TYPE      REASON                           OBJECT                 MESSAGE
8s          Warning   modelharnessGatewayClassMissing  Namespace/my-models    Namespace my-models: Gateway my-models not accepted (NoMatchingParent): no matching GatewayClass; check spec.gatewayClassName.
```

The operator fixes the harness-local Gateway configuration and re-applies the chart. The reporter evaluates cluster and harness findings independently, so operators should inspect the complete `kube-system` reporter Event stream when diagnosing potentially related failures.

#### Story 3 — Operator diagnoses a broken cluster install

An operator installs the `productionstack` umbrella chart but BBR cannot start (image pull failure on the air-gapped cluster). The `productionstack-status-reporter` emits a `Warning` event in `kube-system`:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter,reason=clusterBBRNotReady
```

```
LAST SEEN   TYPE      REASON                OBJECT                    MESSAGE
5s          Warning   clusterBBRNotReady    Namespace/kaito-system    body-based-routing pod in kaito-system/body-based-router is not ready: ImagePullBackOff on bbr container.
```

The `involvedObject` is the cluster-scoped `Namespace` `kaito-system` (the default BBR install namespace) — namespaced resources cannot be referenced cross-namespace from `kube-system`, so the failing `Deployment` name is identified in `message`.

#### Story 4 — Operator diagnoses a slow model download

An operator deploys a large model with model streaming enabled. The model's `ModelMirror` downloads at a sustained 6 MB/s, so the InferenceSet stays not-Ready for a long time without any failure. They run:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter,reason=inferencesetModelDownloadSlow
```

```
LAST SEEN   TYPE      REASON                          OBJECT                 MESSAGE
40s         Warning   inferencesetModelDownloadSlow   Namespace/my-models    InferenceSet my-models/qwen: model download below 20 MB/s for 60s (worst 6.1 MB/s); source=modelmirror my-models/3fa2c1.
```

The operator investigates registry and network throughput for the mirror's download Job. Once the mirror becomes `Ready`, the downloader reports `Completed` and the Event stops being refreshed. If the same InferenceSet used vLLM self-download instead, the Event would have the same reason and subject, with `source=model-pod <pod>`.

## Alternatives

- **Classify Workspace conditions in the reporter, or mirror sub-conditions as new `InferenceSet` condition Types (`InferenceSetInfraReady`, `InferenceSetModelPodsReady`).** Rejected. KAITO already aggregates the child-Workspace root cause onto `InferenceSetReady`: the reason, plus up to three Workspace causes in the message. It also exports `IsProgressReason` / `IsTerminatedReason` for classifying those reasons. Re-deriving the same signal in the reporter would duplicate KAITO's classification and drift as KAITO adds reasons. EPP readiness remains outside KAITO and is checked from the deterministic EPP Deployment.
- **Map KAITO reasons onto prefixed reporter reasons (e.g. `inferencesetInfraProvisioningFailed`, `inferencesetModelPodsNotReady`).** Rejected. The reporter would need a KAITO-reason→category table, updated every time KAITO adds a reason. Passing KAITO's reason through keeps a single source of truth.
- **Aggregate control-plane state into a new Kubernetes resource** — either per-layer ConfigMaps (`productionstack-status` / `modelharness-status`) or new `ModelDeployment` / `ProductionStack` CRDs with their own aggregator controllers. Rejected: introduces new resources, Helm ownership annotations, race-free reporter writes, and a separate upgrade story — all to give operators something they already get from `kubectl get events -n kube-system`. The unified event stream is queryable by every existing tool (kubectl, dashboards, log shippers); the reporter uses deterministic Event names and explicit create/update aggregation without adding a new CR-status contract.
- **Select the downloader from Workspace conditions (`ModelMirrorReady`).** Rejected. KAITO does not set `ModelMirrorReady` until the `ModelMirror` CR is observable by its status sync; before that it reports only `ResourceReady=ModelMirrorNotReady`. The signal is therefore absent during the first reconciles and when mirror creation fails. Annotations plus the configured feature-gate state are deterministic from the start.
- **Report slow `ModelMirror` downloads per `ModelMirror`.** Rejected. The subject would be inconsistent with the vLLM self-download path, which is per InferenceSet. Operators also start diagnosis from the InferenceSet they deployed. A mirror shared by several InferenceSets yields one Event per blocked InferenceSet, which matches the actual impact.

## Test Plan

**E2E tests** (under `test/e2e/`)

**How control-plane assertions are made:**
- **Query.** All assertions use `kubectl get events -n kube-system --field-selector source=productionstack-status-reporter,reason=<reason>`, or the Go equivalent in the test harness.
- **Baseline.** Reporter Events persist until the cluster Event TTL expires, so tests capture a per-reason baseline before perturbation and assert only Events created or refreshed after that baseline.
- **E2E settings.** Installs set `controlPlane.localCSIDriver.namespace=kaito-system` and `controlPlane.weightDownload.modelStreamingEnabled=false`.

| Layer | Test file | Scenario | Asserted outcome |
| --- | --- | --- | --- |
| Cluster | `cluster_status_test.go` | scale `body-based-routing` Deployment to zero, then restore it | a fresh `Warning` `clusterBBRNotReady` appears in `kube-system`; after recovery, the Event stops being refreshed and no recovery Event is emitted; the observed message contains no URL |
| Cluster | `cluster_status_test.go` | scale the configured `llm-gateway-auth` Deployment (`apikey-authz`, then `apikey-operator`) to zero | a fresh `Warning` `clusterGatewayAuthNotReady` naming the scaled Deployment appears and its message contains no URL |
| Cluster | `cluster_status_test.go` | scale the KAITO workspace controller Deployment to zero | a fresh `Warning` `clusterKaitoControllerNotReady` appears and its message contains no URL |
| Cluster | `cluster_status_test.go` | scale the `csi-local-manager` Deployment to zero | a fresh `Warning` `clusterLocalCSIDriverNotReady` appears naming the Deployment; `csi-local-node` Pods on `gpu-node-mocker` nodes never trigger the reason |
| Modeldeployment | `control_plane_error_test.go` | after the InferenceSet is healthy, scale its deterministic `<inferenceset>-inferencepool-epp` Deployment to zero | a fresh `Warning` `inferencesetEPPNotReady` appears on the workload `Namespace`; its message names the InferenceSet, states that the Deployment is scaled to zero, and contains no URL; the original replica count is restored after the test |
| Modeldeployment | `control_plane_error_test.go` | deploy an InferenceSet whose inference image cannot be pulled | a `Warning` whose `reason` equals the KAITO `InferenceSetReady` reason (`ImagePullError`) appears without waiting for the 3-minute debounce; its message names the InferenceSet and carries the KAITO condition message; no Event is emitted while `InferenceSetReady` carries only in-progress reasons |
| Modeldeployment | `model_download_slow_test.go` (new), sustained-slow | stub the model pod's download metric to report throughput `< 20 MB/s` for **every** sample across a full 60 s window while the pod is not Ready | a `Warning` `inferencesetModelDownloadSlow` on the workload `Namespace` whose `message` names the InferenceSet, `source=model-pod <pod>`, the window (`60s`), and the worst observed throughput; it keeps being refreshed while throughput stays below threshold, and stops being refreshed once the download completes |
| Modeldeployment | `model_download_slow_test.go` (new), transient-dip | stub the model pod's download metric to report throughput `< 20 MB/s` for most of the 60 s window but `≥ 20 MB/s` for at least one sample inside it | **no** `inferencesetModelDownloadSlow` event is emitted |
| Modeldeployment | `model_download_slow_test.go` (new), partial-window | stub the model pod's download metric to report throughput `< 20 MB/s` for less than 60 s of samples (e.g. the pod has just started) | **no** `inferencesetModelDownloadSlow` event is emitted until the window fills |

**Unit tests** (under `pkg/productionstack-status-reporter/`)

- **Event lifecycle:** an active finding creates the Event once; it is refreshed no more often than `eventRefreshSeconds`; nothing is written after recovery.
- **`InferenceSetReady` classification:**
  - reasons for which `IsProgressReason` is true are ignored;
  - `IsTerminatedReason` reasons are emitted immediately;
  - other reasons are emitted after the 3-minute debounce.
- **Downloader selection** across `use-local-weights`, `model-streaming: disabled`, `runtime: transformers`, and `modelStreamingEnabled=true/false`.
- **`ModelMirror` name resolution:** preset, legacy short name, and `custom` + config.
- **`ModelMirror` downloader** against a fake dynamic client:
  - `Pending` with slow `status.download` → `Downloading`, and a sustained-slow window yields `inferencesetModelDownloadSlow` with `source=modelmirror <ns>/<name>`;
  - `Ready` → `Completed`;
  - a missing mirror or a non-empty `failureMessage` → `NotStarted`.

**Manual verification.** Each TSG-1 control-plane `reason` is reachable via internal tooling from its corresponding event `reason`. Pass-through KAITO reasons are reachable via the KAITO reason documentation linked from TSG-1. TSG-1 is internal-only: control-plane event `message`s MUST NOT carry TSG URLs.

## Implementation History

- [x] 2026-05-19: Proposed in [issue #71](https://github.com/kaito-project/production-stack/issues/71); initial proposal PR opened (modeldeployment-only scope)
- [x] 2026-05-21: Expanded scope to cluster + modelharness + modeldeployment; restructured under two top-level categories (control-plane / data-plane)
- [x] 2026-05-26: Removed all control-plane aggregator ConfigMaps; control-plane errors are now surfaced exclusively as Kubernetes `Event`s in `kube-system`. Consolidated the control-plane sections around one unified reason catalogue. Added `inferencesetWeightDownloadSlow` warning (default threshold `< 20 MB/s`, sourced from prefetch pod metrics).
- [x] 2026-05-28: Required `inferencesetWeightDownloadSlow` to include the workload namespace, `InferenceSet`, LLM workload Pod, and prefetch Pod in `message`; its Event follows the common schema and references the cluster-scoped workload `Namespace`.
- [x] 2026-05-28: Consolidated §2: merged the per-layer §2.2 / §2.3 / §2.4 catalogues into one §2.2 table and demoted the standalone §2.5 / §2.6 sections into a shorter §2.3 "Notable behaviors" callout. Rewrote §3 as a per-component "Requirements" table (reporter, `charts/productionstack`, `charts/modelharness`, `charts/modeldeployment`, `llm-gateway-auth`).
- [x] 2026-05-28: Expanded the data-plane Test Plan and the initial control-plane test design.
- [x] 2026-05-29: Aligned every control-plane Event's `involvedObject` with Kubernetes' cross-namespace Event validation: Events in `kube-system` reference the cluster-scoped `Namespace` containing the problematic resource, while the specific failing namespaced resource is identified in `message`.
- [x] 2026-05-29: Reformulated `inferencesetWeightDownloadSlow` as a sliding-window check (default **60 s**, configurable via `controlPlane.weightDownload.windowSeconds`): the event fires only when every sample in the window is strictly below `controlPlane.weightDownload.minMBps`, a single in-threshold sample suppresses emission, and the reporter waits for the window to be fully populated before deciding. Eliminates spurious warnings from transient dips. Updated the §1.2 catalogue row, the §3 `charts/modeldeployment` schema-validation requirement, and split the Test Plan `weight_download_slow_test.go` row into sustained-slow / transient-dip / partial-window scenarios.
- [x] 2026-08-06: Implemented `productionstack-status-reporter` as an HA, leader-elected umbrella subchart. The reporter is installed in the umbrella release namespace by default and publishes Events to `kube-system`. It periodically evaluates seven cluster Deployment-readiness reasons, three modelharness Gateway reasons, and five modeldeployment reasons; reads KAITO `Workspace` conditions for infra, model-pod, and node-pressure health; checks deterministic EPP Deployment readiness; aggregates stable Events by subject and reason; and applies startup-grace/debounce rules. Cross-layer suppression and the previously proposed CRD, auth-policy/APIKey, EnvoyFilter, network-policy, and route checks are not part of the implemented catalogue.
- [x] 2026-10-21: Split from the combined `20260519-end-to-end-error-handling.md` proposal into this control-plane proposal and the companion [data-plane error handling proposal](./20260519-data-plane-error-handling.md); status moved to `implementable`.
- [x] 2026-10-21: Realigned the control-plane scope with the reporter's requirements:
  - **Scope.** The reporter checks runtime health only; configuration drift, node-level health, and `/v1/models` are out of scope.
  - **Event lifecycle.** Active Events are refreshed every `eventRefreshSeconds` (default 300 s); no recovery Events are emitted.
  - **Cluster layer.** Added `clusterLocalCSIDriverNotReady` and `apikey-operator` coverage. Removed `clusterNodeProvisionerNotReady`.
  - **Modeldeployment layer.** Removed `inferencesetNodeUnderPressure`. Replaced `inferencesetInfraProvisioningFailed` / `inferencesetModelPodsNotReady` with pass-through `InferenceSetReady` reasons, classified via KAITO's `IsProgressReason` / `IsTerminatedReason`.
  - **Model download.** Renamed `inferencesetWeightDownloadSlow` to `inferencesetModelDownloadSlow` and sourced it from the model pod instead of a prefetch pod. Added `ModelMirror` download observation behind a per-path `WeightDownloader` interface, selected from InferenceSet annotations and `controlPlane.weightDownload.modelStreamingEnabled`.
  - **Chart requirements.** Removed the `values.schema.json` requirements on `charts/modelharness` / `charts/modeldeployment`.
- [ ] TBD: `productionstack-status-reporter` updated per the 2026-10-21 realignment
- [ ] TBD: TSG-1 (control-plane errors) merged, internal-only
