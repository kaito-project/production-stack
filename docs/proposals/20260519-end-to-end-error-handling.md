---
title: End-to-End Error Handling Across Cluster, Modelharness, and Modeldeployment Levels
authors:
  - "@rambohe-ch"
reviewers:
  - "@Fei-Guo"
  - "@zhuangqh"
  - "@tnsimon"
  - "@techworldhello"
creation-date: 2026-05-19
last-updated: 2026-05-28
status: provisional
see-also:
  - "https://github.com/kaito-project/production-stack/issues/71"
replaces: []
superseded-by: []
---

# End-to-End Error Handling Across Cluster, Modelharness, and Modeldeployment Levels

## Table of Contents

- [End-to-End Error Handling Across Cluster, Modelharness, and Modeldeployment Levels](#end-to-end-error-handling-across-cluster-modelharness-and-modeldeployment-levels)
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
        - [1.3 Priority order](#13-priority-order)
        - [1.4 Upstream gating (cross-layer suppression)](#14-upstream-gating-cross-layer-suppression)
      - [2. Data-plane errors](#2-data-plane-errors)
        - [2.1 Unified OpenAI-compatible error envelope](#21-unified-openai-compatible-error-envelope)
        - [2.2 Error catalogue](#22-error-catalogue)
        - [2.3 Notable behaviors](#23-notable-behaviors)
      - [3. Requirements](#3-requirements)
    - [User Stories](#user-stories)
      - [Story 1 — Operator diagnoses a stuck modeldeployment](#story-1--operator-diagnoses-a-stuck-modeldeployment)
      - [Story 2 — Operator diagnoses a harness-local misconfiguration](#story-2--operator-diagnoses-a-harness-local-misconfiguration)
      - [Story 3 — Operator diagnoses a broken cluster install](#story-3--operator-diagnoses-a-broken-cluster-install)
      - [Story 4 — Client gets actionable HTTP error](#story-4--client-gets-actionable-http-error)
      - [Story 5 — BBR outage no longer disguised as 404](#story-5--bbr-outage-no-longer-disguised-as-404)
  - [Alternatives](#alternatives)
  - [Test Plan](#test-plan)
  - [Implementation History](#implementation-history)

## Glossary

- **modelharness**: The Helm release rendered by [`charts/modelharness`](../../charts/modelharness) — one per workload namespace. Provisions the namespace `Gateway`, the catch-all `EnvoyFilter` (`model-not-found-direct`), the `AuthorizationPolicy`, the `APIKey` CR, and the `NetworkPolicy` resources.
- **modeldeployment**: The Helm release rendered by [`charts/modeldeployment`](../../charts/modeldeployment) — one `InferenceSet`, one `InferencePool`, one EPP `Deployment`/`Service`/RBAC/`ConfigMap`, and one `HTTPRoute`, parented to the per-namespace `Gateway`.
- **EPP**: Endpoint Picker — per-model `llm-d-inference-scheduler` ext_proc pod that performs KV-cache aware routing.
- **BBR**: Body-Based Router — the cluster-wide ext_proc filter (in `istio-system`, shipped by the `productionstack` umbrella chart) that parses request bodies and injects the `X-Gateway-Model-Name` header.
- **InferenceSet**: `kaito.sh/v1alpha1` CR owned by the KAITO controller; the canonical declaration of one model deployment.

## Summary

Production-stack today has no coherent end-to-end error story. Failures occur at three distinct layers — cluster bootstrap (KAITO / Istio / CRDs / bbr/ keda-kaito-scaler / `llm-gateway-auth`), per-namespace harness setup (`Gateway`, catch-all `EnvoyFilter`, `AuthorizationPolicy`, `APIKey`, `NetworkPolicy`), and per-model deployment (`InferenceSet`, `InferencePool`, EPP, `HTTPRoute`) — and each surface emits errors in its own format on its own object. On the request path, distinct failures (cluster-wide ext_authz outage, BBR outage, missing namespace Gateway, EPP outage, model still warming up, model name truly unknown) all collapse onto indistinguishable `404`s or unbodied `503`s, and response bodies do not follow a stable schema.

This proposal addresses all three levels and organises every error into one of two top-level categories:

1. **Control-plane errors** — failures observable inside the cluster, covering install-time misconfiguration **and** post-install drift (a subchart Deployment crashing, a CRD being deleted, an `APIKey` Secret being rotated, an EPP pod entering `CrashLoopBackOff`, model-weights downloading too slowly, etc.). Surfaced **exclusively as Kubernetes `Event`s published to the `kube-system` namespace** by a single `productionstack-status-reporter` Deployment shipped with the umbrella chart. Component-local status fields (`InferenceSet.status`, `Gateway.status`, `Deployment.status`) are preserved for per-component diagnosis, but the production-stack-level cross-layer view lives entirely in the event stream.
2. **Data-plane errors** — failures observable on the HTTP request path against an installed stack. Standardised onto a single OpenAI-compatible JSON envelope, with a stable `code` and a new `x-kaito-error-source` header that pinpoints which hop (cluster filter, namespace gateway, modeldeployment EPP, upstream pod) produced the error.

## Motivation

Production-stack is built from independent OSS components (Istio Gateway, `llm-gateway-auth`, BBR, EPP, KAITO `InferenceSet`, KEDA, keda-kaito-scaler). Each component does its own error reporting in its own format and on its own resource, which means:

- **For operators.** Diagnosing "why is my model not ready?" requires walking objects across three layers and four-plus namespaces (`kaito-system`, `istio-system`, `llm-gateway-auth`, the workload namespace) and correlating events by timestamp. There is no single, cross-layer event stream operators can `kubectl get` to see the current state.
- **For end users.** The HTTP response a client receives for a broken stack is non-deterministic: `404` from the catch-all, `503` with no body from Envoy when EPP or BBR is unreachable, or `000` (connection reset) when ext_authz / ext_proc fails open. The response body shape changes by component.
- **For documentation.** Without stable `Reason` strings (status side) and stable `code` values (response side), TSG-1 (control-plane errors, all three layers) and TSG-2 (data-plane errors, all three layers) — the two deliverables called out in #71, organised along the same two-category axis as this proposal — cannot be deep-linked.

### Goals

- Define a control-plane-error taxonomy that covers cluster, modelharness, and modeldeployment levels, with every failure surfaced as a Kubernetes `Event` in the `kube-system` namespace.
- Define a data-plane-error taxonomy that covers cluster-level filters (ext_authz, BBR), modelharness-level routing (namespace Gateway, catch-all), and modeldeployment-level dispatch (EPP, upstream pod), all standardised onto one OpenAI-compatible JSON envelope.
- Eliminate the BBR/ext_authz-outage-looks-like-404 ambiguity.
- Distinguish `model_not_found` (route does not exist) from `model_unavailable` (route exists but `InferencePool` has zero ready endpoints — covers warming up, crash, OOM, eviction; root cause exposed via control-plane `Event`s in `kube-system`).
- Publish two TSGs aligned with the two top-level categories — TSG-1 (control-plane errors, covering cluster + modelharness + modeldeployment) and TSG-2 (data-plane errors, covering cluster + modelharness + modeldeployment) — both **internal-only**, both keyed off the `Reason` / `code` strings defined here.

### Non-Goals/Future Work

- Per-level aggregator controllers. We deliberately reuse Kubernetes `Event`s (rolled up into `kube-system` by the `productionstack-status-reporter` Deployment shipped with the umbrella chart) plus existing per-component status fields (`InferenceSet.status`, `Gateway.status`, `Deployment.status`).
- Centralised logging / alerting infrastructure; we reuse Kubernetes events, CR status, and Prometheus metrics, per #71 non-goals.
- Redesigning the request/response protocol beyond what's needed to carry actionable error information (per #71 non-goals).
- Rate-limit / quota errors (`429`). Whatever vLLM produces today is passed through unchanged.
- Errors that originate strictly outside the stack (e.g. cloud-provider AKS cluster-creation failure) are surfaced only as preconditions — production-stack does not own their root-cause remediation, only their detection and TSG cross-link.

## Proposal

### Implementation Details/Notes/Constraints

#### Error category overview

Every error in production-stack belongs to exactly one of two categories, and within each category is owned by exactly one of the three layers:

| Category | Cluster level | Modelharness level | Modeldeployment level |
| --- | --- | --- | --- |
| **Control-plane errors** (Kubernetes `Event`s in `kube-system`; install-time misconfig **and** post-install drift) | - umbrella chart subchart startup or runtime crash<br>- CRD installation or post-install deletion<br>- KAITO/Istio/KEDA controller readiness transitions | - namespace `Gateway` provisioning or runtime regression<br>- `AuthorizationPolicy` / `APIKey` / `EnvoyFilter` / `NetworkPolicy` provisioning or post-install drift | - `InferenceSet` / `InferencePool` / EPP / `HTTPRoute` startup or runtime regression<br>- infra (GPU node) provisioning or reclaim<br>- scaling misconfig<br>- slow model-weights download (`< 20 MB/s` from prefetch pod) |
| **Data-plane errors** (request path → OpenAI-compatible HTTP responses) | - BBR ext_proc outage<br>- `llm-gateway-auth` ext_authz outage | - namespace gateway dataplane outage<br>- missing/invalid `APIKey` secret<br>- `NetworkPolicy` blackhole<br>- catch-all `model_not_found` | - EPP outage<br>- no ready model endpoints (warming up / crash / OOM / eviction)<br>- upstream pod timeout<br>- EPP internal error |

The section below enumerates the unified taxonomy for all three layers in a single table, then describes the priority order and the per-component changes required.

#### 1. Control-plane errors

Control-plane errors are surfaced **exclusively through Kubernetes `Event`s published to the `kube-system` namespace**, by a single `productionstack-status-reporter` Deployment shipped with the umbrella chart. The rolled-up cross-layer view lives entirely in the event stream. Component-local status (`InferenceSet.status.conditions[]`, `Gateway.status.conditions[]`, `Deployment.status.conditions[]`) is preserved unchanged for component-local diagnosis, but the production-stack-level taxonomy in §1.2 is emitted only as events. Operators consume the entire taxonomy with one query:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter \
  --sort-by=.lastTimestamp
```

Each event carries the closed `reason` vocabulary defined in §1.2 (FR2). The event stream covers all three layers (cluster, modelharness, modeldeployment), install-time misconfiguration, post-install drift, and the new pre-Ready warning path (`inferencesetWeightDownloadSlow`).

##### 1.1 Event schema

Every control-plane event MUST follow the schema below.

| Field | Value |
| --- | --- |
| `metadata.namespace` | `kube-system` (always — regardless of which layer or which workload namespace produced the underlying condition) |
| `type` | `Warning` for any `*NotReady` / `*Failed` / `*Missing` / `*Misconfigured` / `*Rejected` / `*Slow` reason; `Normal` for the positive `*Ready` reasons |
| `reason` | One of the stable strings in §1.2 |
| `source.component` | `productionstack-status-reporter` |
| `involvedObject` | The resource carrying root cause: a `Deployment`, the namespace `Gateway`, the `InferenceSet`, the inference `Pod`, a `CustomResourceDefinition`, etc. Workload-namespace objects are referenced cross-namespace so operators can still pivot to `kubectl describe` from the event. |
| `message` | Human-readable description of the failure. The message MUST identify the affected workload namespace and (for modeldeployment-layer reasons) the `InferenceSet` name, so operators can pivot directly without re-reading `involvedObject`. The message MUST NOT carry internal-only links (e.g. TSG URLs). |

Events are emitted on every state transition (Ready→NotReady or vice versa). Repeats while the state is unchanged are suppressed by the standard client-go `EventRecorder` aggregation behaviour; the `count` and `lastTimestamp` fields on the existing event are bumped instead.

##### 1.2 Reason catalogue

The single table below replaces the previous per-layer Reason tables. The `Layer` column is informational; the `reason` string itself is layer-prefixed (`cluster*` / `modelharness*` / `inferenceset*`) so each value is globally unique and maps unambiguously to a TSG-1 anchor.

| Layer | `reason` | `type` | Triggered by | Detection source | `involvedObject` |
| --- | --- | --- | --- | --- | --- |
| Cluster | `clusterCRDMissing` | Warning | Required CRD absent. The CRD set is derived from what `charts/productionstack`, `charts/modelharness`, and `charts/modeldeployment` actually render or reference (including RBAC grants for runtime informers): Gateway API (`Gateway`, `HTTPRoute`); GAIE (`InferencePool`, `InferenceObjective`, `InferenceModelRewrite`); KAITO (`InferenceSet`, `Workspace`, `APIKey`); Istio (`EnvoyFilter`, `DestinationRule`, `AuthorizationPolicy`); KEDA (`ScaledObject`, `ClusterTriggerAuthentication`) | API discovery by the reporter (periodic); each affected component additionally poll-then-startup-timeout-exits and is restarted by Kubernetes until the CRD appears | the umbrella chart's release `Namespace` (default `kaito-system`) |
| Cluster | `clusterBBRNotReady` | Warning | `body-based-routing` subchart Deployment NotReady: `ImagePullBackOff`, missing `EnvoyFilter` injection point, RBAC errors, runtime crash, scale-to-zero | `Deployment.status` of BBR in `istio-system`; events on the `EnvoyFilter` | BBR `Deployment` |
| Cluster | `clusterKedaKaitoScalerNotReady` | Warning | `keda-kaito-scaler` subchart Deployment NotReady, or its `ScaledObject` / `TriggerAuthentication` rejected by KEDA (install-time or runtime) | `Deployment.status` + KEDA `ScaledObject.status.conditions` | `keda-kaito-scaler` `Deployment` |
| Cluster | `clusterGatewayAuthNotReady` | Warning | `llm-gateway-auth` components (`apikey-operator`, `apikey-authz`) NotReady, or `MeshConfig` patch missing the `apikey-ext-authz` extension provider (install-time or runtime) | `Deployment.status` + Istio `MeshConfig` lookup | `apikey-authz` `Deployment` |
| Cluster | `clusterIstioControlPlaneNotReady` | Warning | `istiod` not running / `IstioOperator` unhealthy | `Deployment.status` of `istiod` | `istiod` `Deployment` |
| Cluster | `clusterKaitoControllerNotReady` | Warning | KAITO workspace controller `Deployment` NotReady | `Deployment.status` of KAITO controller | KAITO controller `Deployment` |
| Cluster | `clusterKedaNotReady` | Warning | KEDA control plane components NotReady: `keda-operator` and `keda-operator-metrics-apiserver` (in the `keda` namespace, regardless of whether KEDA is installed as a managed add-on or via upstream Helm) | `Deployment.status` of `keda-operator` and `keda-operator-metrics-apiserver` | `keda-operator` `Deployment` |
| Cluster | `clusterNodeProvisionerNotReady` | Warning | Node-provisioner Deployment NotReady. The reporter probes whichever provisioner is registered:<br>- upstream Karpenter (`karpenter` Deployment in the `karpenter` namespace)<br>- `gpu-node-mocker` (`gpu-node-mocker` Deployment, see `charts/gpu-node-mocker`) used for E2E<br>- any other Deployment registered via `clusterStatus.nodeProvisioner.{name,namespace}` chart values<br>If none is registered, the check is skipped (treated as Ready), so clusters that pre-provision GPU nodes are not penalised. | `Deployment.status` of the configured node-provisioner Deployment | provisioner `Deployment` |
| Cluster | `clusterReady` | Normal | All `cluster*` reasons clear | aggregator | the umbrella chart's release `Namespace` |
| Modelharness | `modelharnessGatewayClassMissing` | Warning | `gatewayClassName: istio` not registered (local misconfiguration of `Gateway.spec.gatewayClassName`). Cluster-wide Istio absence is **not** re-classified here — it is already surfaced by `clusterIstioControlPlaneNotReady` in `kube-system`, and operators consult cluster-layer events first. | watch `Gateway.status.conditions[Accepted]=False` (`Reason=NoMatchingParent` / `InvalidParameters`) | the namespace `Gateway` |
| Modelharness | `modelharnessGatewayProgrammingFailed` | Warning | Harness-local Gateway programming failure: listener port collision, TLS secret missing, harness-local Envoy proxy startup failure. Cluster-wide Istio control plane outage is **not** re-classified here — it is already surfaced by `clusterIstioControlPlaneNotReady`. | watch `Gateway.status.conditions[Programmed]=False` and listener-level conditions | the namespace `Gateway` |
| Modelharness | `modelharnessExtAuthzProviderMissing` | Warning | namespace `AuthorizationPolicy` references a provider name that does **not** match the registered cluster-level provider (local chart misconfiguration — admin hand-edited or supplied wrong `values.yaml`). Cluster-wide absence of the provider is **not** re-classified here — it is already surfaced by `clusterGatewayAuthNotReady`. | reconcile-time comparison of `AuthorizationPolicy.spec.action.provider.name` against `MeshConfig.extensionProviders[*].name` | the namespace `AuthorizationPolicy` |
| Modelharness | `modelharnessAPIKeyReconcileFailed` | Warning | Local `APIKey` CR is invalid or conflicts with an existing Secret — the `apikey-operator` is up but rejected this specific CR. Cluster-wide `apikey-operator` outage is **not** re-classified here — it is already surfaced by `clusterGatewayAuthNotReady`. | watch `APIKey.status.conditions`; only surfaced when `clusterGatewayAuthNotReady` is clear | the `APIKey` CR |
| Modelharness | `modelharnessCatchAllFilterRejected` | Warning | namespace `EnvoyFilter` `model-not-found-direct` rejected by Istio (workload selector mismatch, schema error) | Istio `EnvoyFilter.status`; fallback heuristic when status is empty: assert the filter's `workloadSelector` matches the namespace `Gateway` pod labels | the namespace `EnvoyFilter` |
| Modelharness | `modelharnessNetworkPolicyMisconfigured` | Warning | `networkPolicy.allowedIngressNamespaces` references nonexistent namespaces, or KEDA namespace mismatch leaves the keda-kaito-scaler unable to reach inference pods | reconcile-time lookup by the reporter (Namespace existence + KEDA scaler namespace) | the namespace `NetworkPolicy` |
| Modelharness | `modelharnessReady` | Normal | All `modelharness*` reasons clear for the namespace | aggregator | the workload `Namespace` |
| Modeldeployment | `inferencesetInfraProvisioningFailed` | Warning | GPU node cannot be provisioned — quota exceeded, instance type unavailable, zone capacity, subscription not registered — covers both initial provisioning and runtime re-provisioning after a previously Ready node is reclaimed or fails | NodeClaim / Karpenter events; KAITO `Workspace` conditions (`NodeClaimReady=False`) | the owning `InferenceSet` |
| Modeldeployment | `inferencesetModelPodsNotReady` | Warning | Same reason regardless of whether the pod has ever reached Ready. Install-time: `ImagePullBackOff` on base image, model-weights pull failure, `InsufficientGPU`, PVC bind failure. Runtime: `OOMKilled`, `CrashLoopBackOff`, readiness-probe regression, eviction. | Pod `status.containerStatuses[*].state` + `restartCount`; KAITO-owned `Deployment.status`; `Workspace.status` (`ResourceReady=False` or `InferenceReady=False` while `NodeClaimReady=True`) | the owning `InferenceSet` |
| Modeldeployment | `inferencesetEPPNotReady` | Warning | Install-time: EPP image pull failure, malformed `ConfigMap`, RBAC missing for list pods, `--pool-name` mismatch. Runtime: EPP crash / restart loop / readiness-probe regression after the pod was previously Ready. | EPP `Deployment.status.conditions` + Pod state + events | EPP `Deployment` |
| Modeldeployment | `inferencesetRouteNotReady` | Warning | Install-time or runtime: `HTTPRoute` parent `Gateway` missing, `ResolvedRefs=False`, `InferencePool` selector matches zero pods, parent `Gateway` deleted post-install, or `InferencePool` selector drifts | `HTTPRoute.status.parents`, `InferencePool.status` | the `HTTPRoute` |
| Modeldeployment | `inferencesetScalingMisconfigured` | Warning | `enableScaling=true` with `maxReplicas < replicas`, threshold ≤ 0, or keda-kaito-scaler absent | Helm `values.schema.json` + reconcile-time validation | the owning `InferenceSet` |
| Modeldeployment | `inferencesetWeightDownloadSlow` | Warning | While the LLM workload pod is initialising, the model-weights download throughput reported by the prefetch pod's Prometheus metric is below the threshold (default **20 MB/s**, exposed as `controlPlane.weightDownload.minMBps` in the umbrella chart values). Indicates registry/storage backplane bottleneck that will materially delay `Ready`. Emitted at most once per pod-start and resolved (no further repeats) when the inference pod becomes Ready or the download completes. The event `message` MUST include the workload namespace and the owning `InferenceSet` name so operators can pivot directly (e.g. `"weight download throughput 8 MB/s below threshold 20 MB/s for inferenceset my-models/qwen, pod qwen-7d9f..."`). | the reporter scrapes the prefetch pod's exposed metric (e.g. `prefetch_model_weights_download_bytes_per_second{pod}`) every reconcile while the inference pod is in `ContainerCreating` / `PodInitializing`, and compares against the threshold | the owning `InferenceSet` (with `related` pointing at the LLM workload `Pod` and the prefetch `Pod`) |
| Modeldeployment | `inferencesetReady` | Normal | All `inferenceset*` non-warning reasons clear (`inferencesetWeightDownloadSlow` is orthogonal — see §1.3) | aggregator | the owning `InferenceSet` |

Each `reason` corresponds to a stable anchor in **TSG-1**. The reporter is the single producer; emitting the same reason from any other component is forbidden.

##### 1.3 Priority order

When more than one reason fires for the same `involvedObject` (or for an object that rolls up into the same higher layer), the reporter selects the **surfaced primary** event deterministically using the chain below. The pattern is uniformly: fail-fast config validation → install-order root cause → request-path order. Cross-layer dependencies are **not** re-classified into the harness or modeldeployment layer; a cluster-layer outage stays a cluster-layer event in `kube-system` and operators consult cluster-layer events first (see Story 2 / Story 3). §1.4 below adds a narrow cross-layer **suppression** rule for the small set of downstream reasons that are definitionally dependent on an upstream cluster reason.

| Layer | Priority chain (highest first) |
| --- | --- |
| Cluster | `clusterCRDMissing` > `clusterIstioControlPlaneNotReady` > `clusterGatewayAuthNotReady` > `clusterBBRNotReady` > `clusterKaitoControllerNotReady` > `clusterNodeProvisionerNotReady` > `clusterKedaNotReady` > `clusterKedaKaitoScalerNotReady` |
| Modelharness | `modelharnessGatewayClassMissing` > `modelharnessGatewayProgrammingFailed` > `modelharnessExtAuthzProviderMissing` > `modelharnessAPIKeyReconcileFailed` > `modelharnessCatchAllFilterRejected` > `modelharnessNetworkPolicyMisconfigured` |
| Modeldeployment | `inferencesetScalingMisconfigured` > `inferencesetInfraProvisioningFailed` > `inferencesetModelPodsNotReady` > `inferencesetRouteNotReady` > `inferencesetEPPNotReady` |

Notes:

- Within the modeldeployment chain, `inferencesetInfraProvisioningFailed` precedes `inferencesetModelPodsNotReady` because no pod schedules until a NodeClaim succeeds; `inferencesetRouteNotReady` precedes `inferencesetEPPNotReady` because `HTTPRoute` matching runs upstream of the EPP ext_proc on the request path.
- `inferencesetWeightDownloadSlow` is intentionally **outside** the chain: it is emitted in addition to whichever primary reason is current (typically `inferencesetModelPodsNotReady` while the pod is still pulling weights), because its remediation (improve registry/cache throughput) is independent of the primary failure mode.
- When **no** unhealthy reason is selectable for a layer, the corresponding positive `*Ready` event is emitted exactly once on the transition.
- Harness and modeldeployment reasons are **only** emitted for failures whose root cause sits in that layer. Cluster-layer outages that prevent a harness or modeldeployment from reaching Ready are surfaced as their own `cluster*` events in `kube-system`; the reporter does not re-emit a higher-layer placeholder for them. This avoids duplicate events for the same root cause and keeps the `kube-system` event stream a flat, layer-prefixed catalogue. A narrow cross-layer suppression rule applies to the small set of downstream reasons that are definitionally dependent on an upstream cluster reason — see §1.4.

##### 1.4 Upstream gating (cross-layer suppression)

§1.3 selects a single primary reason within one layer. §1.4 adds the cross-layer rule: when an upstream `cluster*` reason is active in `kube-system`, the reporter suppresses harness/modeldeployment reasons that have a **strict definitional dependency** on it — i.e. cases where the downstream check has no meaningful input until the upstream is healthy (typically because the upstream owns the API resource or the `status.conditions` the downstream check reads). Reasons **not** listed in the suppression table below are emitted independently of any active cluster reason, because they represent local state the reporter can evaluate without consulting the cluster layer (e.g. an `AuthorizationPolicy` provider typo, a `maxReplicas < replicas` value, a missing `NetworkPolicy` namespace, an EPP `Deployment` `ImagePullBackOff`). This way, genuinely independent local issues are never hidden behind a cluster outage.

Suppression table:

| Active upstream `cluster*` reason | Suppressed downstream reasons | Why this is a definitional dependency |
| --- | --- | --- |
| `clusterCRDMissing` (per specific CRD) | Any downstream reason whose detection requires the missing CRD (e.g. without the `APIKey` CRD, `modelharnessAPIKeyReconcileFailed` is suppressed; without the `InferenceSet` CRD, all `inferenceset*` reasons are suppressed) | Without the CRD, the API server cannot serve the resource; the downstream check has no input. |
| `clusterIstioControlPlaneNotReady` | `modelharnessGatewayClassMissing`, `modelharnessGatewayProgrammingFailed` | `Gateway.status.conditions[Accepted]` and `[Programmed]` are written by `istiod`; without it, those conditions cannot transition and the downstream check has no signal to evaluate. |
| `clusterGatewayAuthNotReady` | `modelharnessExtAuthzProviderMissing`, `modelharnessAPIKeyReconcileFailed` | `MeshConfig.extensionProviders` is owned by `llm-gateway-auth`; `APIKey.status` is written by `apikey-operator`. Without either, the downstream checks have no upstream state to validate against. |
| `clusterKaitoControllerNotReady` | `inferencesetInfraProvisioningFailed`, `inferencesetModelPodsNotReady` | `Workspace.status.conditions[NodeClaimReady]` / `[ResourceReady]` / `[InferenceReady]` are written by the KAITO controller; without it the reporter cannot distinguish "infra failed" from "infra not yet attempted" or "pod not scheduled" from "pod not yet observed". `inferencesetEPPNotReady`, `inferencesetRouteNotReady`, and `inferencesetScalingMisconfigured` are **not** gated here — EPP `Deployment` status is reconciled by kube-controller-manager; `HTTPRoute` / `InferencePool` status is written by Gateway API controllers (Istio); scaling validation is a static check on chart values — all independent of KAITO. |
| `clusterNodeProvisionerNotReady` | `inferencesetInfraProvisioningFailed` | `NodeClaim` transitions are produced by the node provisioner; if it is down, lack of progress is not evidence of provisioning failure. |

Behavior:

1. On every reconcile, the reporter evaluates upstream cluster reasons first (per the cluster priority chain in §1.3). For each currently-active upstream reason, the reporter consults the suppression table.
2. Downstream reasons in an active upstream's row are **not emitted** on this reconcile. Any pre-existing event for that downstream reason is left to age out via the standard Kubernetes event TTL (default 1h); the reporter does not actively delete it.
3. When the upstream resolves, suppression lifts on the next reconcile and the downstream reason is re-evaluated and emitted if still applicable. The corresponding `*Ready` event for the upstream (e.g. `clusterReady`) is emitted before suppression lifts so operators see the recovery first.
4. **Transparency suffix on the upstream event**: when an upstream cluster Warning is being emitted AND it is currently suppressing at least one downstream reason in at least one namespace, its `message` MUST include a deterministic suffix of the form `(suppressing downstream reasons: <reason1>, <reason2>, ... in N namespace(s))`. The suffix is included **only** in genuine cross-layer-dependency cases (i.e. when something is actually suppressed); cluster reasons not present in the suppression table (e.g. `clusterBBRNotReady`, `clusterKedaNotReady`) never carry this suffix. Downstream reason names are sorted lexicographically so the suffix is stable across reconciles.

Example. When `clusterKaitoControllerNotReady` is active and 3 workload namespaces have InferenceSets that would otherwise be reporting infra/pod-not-ready, the cluster Warning reads:

```
LAST SEEN   TYPE      REASON                            OBJECT                         MESSAGE
12s         Warning   clusterKaitoControllerNotReady    Deployment/kaito-controller    KAITO workspace controller has 0/2 ready replicas: CrashLoopBackOff on workspace-controller container. (suppressing downstream reasons: inferencesetInfraProvisioningFailed, inferencesetModelPodsNotReady in 3 namespace(s))
```

Adding a new entry to the suppression table requires evidence that the downstream check is **definitionally** meaningless without the upstream being healthy (e.g. the upstream owns the watched conditions or the watched API resource). Symptomatic correlation alone is not sufficient — if the downstream reason can be evaluated by reading local state, it stays out of the table and is emitted independently.

#### 2. Data-plane errors

Data-plane errors are everything an HTTP client can observe. They are standardised onto one OpenAI-compatible envelope regardless of which layer produced them.

##### 2.1 Unified OpenAI-compatible error envelope

```json
{
  "error": {
    "type":    "invalid_request_error" | "authentication_error" | "service_unavailable" | "internal_error",
    "code":    "<stable string from §2.2>",
    "message": "<human-readable>",
    "param":   "<json-path or null>"
  }
}
```

Headers on every error response include `x-kaito-error-source: gateway | authz | bbr | epp | inferenceset` — the value names the **at-fault component** (the thing the operator should look at first); the **layer** is implied by the `code`'s namespace per the tables below. Emission per source:

| Source value | Emitted by | Why |
| --- | --- | --- |
| `gateway`, `bbr`, `epp`, `inferenceset` | chart-rendered Envoy `local_reply_config` via `response_headers_to_add` | `body-based-routing` and `llm-d-inference-scheduler` are consumed as unmodified upstream binaries. |
| `authz` (deny path: `401 invalid_api_key`, `403 api_key_disabled`) | `llm-gateway-auth` in-process — a same-org `kaito-project/*` sibling repo | Envoy `local_reply` cannot match on the per-deny gRPC code or body text needed to differentiate 401 vs 403. |
| `authz` (outage path: `502 ext_authz_unavailable`) | chart-rendered cluster-level `local_reply` matching the `ext_authz_error` response flag | The in-process emitter is by definition unreachable when the authz Deployment is down. |

Request path (per `README.md`): `Client → Istio Gateway → ext_authz (llm-gateway-auth) → BBR → HTTPRoute → EPP → vLLM Pod`.

##### 2.2 Error catalogue

The table below lists every data-plane error `code`, the HTTP status it surfaces on, the at-fault component named by `x-kaito-error-source`, what triggers it, and the chart that owns rendering it. Codes are grouped by layer: cluster-level codes affect every namespace; modelharness-level codes are per-namespace; modeldeployment-level codes are per-InferenceSet.

| Layer | HTTP | `code` | `x-kaito-error-source` | Trigger | Owner |
| --- | --- | --- | --- | --- | --- |
| Cluster | 502 | `ext_authz_unavailable` | `authz` | `llm-gateway-auth` ext_authz Deployment unreachable or returning 5xx; cluster-wide `local_reply` mapped from the `ext_authz_error` response flag | `charts/productionstack` |
| Cluster | 502 | `bbr_unavailable` | `bbr` | BBR ext_proc filter unreachable / errored; cluster-wide `local_reply` mapped from the `ext_proc_error` response flag | `charts/productionstack` |
| Cluster | 500 | `mesh_config_invalid` | `gateway` | `MeshConfig.extensionProviders` references an unknown ext_authz / ext_proc cluster; Envoy aborts filter chain build | `charts/productionstack` |
| Modelharness | 401 | `invalid_api_key` | `authz` | `Authorization` missing, token does not match any `APIKey` Secret resolvable from the host subdomain, or token is syntactically malformed. Emitted in-process by `llm-gateway-auth`. | `llm-gateway-auth` (in-process) |
| Modelharness | 403 | `api_key_disabled` | `authz` | Valid `APIKey` resolved but not authorised for this gateway namespace, or the `APIKey` CR is explicitly marked disabled. Same in-process emitter as `invalid_api_key`, HTTP `403`. Requires the `llm-gateway-auth` deny-path change in §3 to actually surface `403` (today `apikey-authz` collapses every deny to `401`). | `llm-gateway-auth` (in-process) |
| Modelharness | 400 | `invalid_request_body` | `bbr` | Body fails BBR parsing (not JSON, not OpenAI chat-completions schema, missing `model`); chart-rendered cluster-level `local_reply` renders the envelope | `charts/modelharness` (+ `charts/productionstack` `local_reply`) |
| Modelharness | 404 | `model_not_found` | `gateway` | `X-Gateway-Model-Name` is present but no `HTTPRoute` in this namespace matches | `charts/modelharness` |
| Modelharness | 502 | `gateway_dataplane_unhealthy` | `gateway` | Namespace `Gateway` pod has zero ready replicas; reported by upstream HC and mapped through a `local_reply` on the parent listener | `charts/modelharness` |
| Modelharness | 503 | `gateway_not_programmed` | `gateway` | Namespace `Gateway` exists but `status.conditions[Programmed]=False`; emitted by a short-lived direct-response while the harness is still being installed | `charts/modelharness` |
| Modeldeployment | 502 | `epp_unavailable` | `epp` | EPP ext_proc unreachable / errored; chart-rendered `local_reply` on the EPP-targeted cluster mapped from the ext_proc filter error response flag | `charts/modeldeployment` |
| Modeldeployment | 500 | `epp_internal_error` | `epp` | EPP returned a non-routing error / 5xx to Envoy (panic, scheduler bug); the same chart-rendered `local_reply` maps `UpstreamProtocolError` / ext_proc upstream 5xx | `charts/modeldeployment` |
| Modeldeployment | 503 | `model_unavailable` | `epp` | `HTTPRoute` matched but `InferencePool` has zero ready endpoints (warm-up / crash / OOM / eviction). The `code` is deliberately root-cause-neutral because all sub-causes share the same client behaviour (back off on `Retry-After` and retry). The operator-facing root cause is surfaced as a control-plane `Warning` `Event` in `kube-system` — one of `inferencesetInfraProvisioningFailed`, `inferencesetModelPodsNotReady`, or `inferencesetEPPNotReady` (§1.2). | `charts/modeldeployment` |
| Modeldeployment | 504 | `upstream_timeout` | `inferenceset` | Inference pod did not respond within the route timeout; the chart-rendered `local_reply` names the inference pod (not EPP) as the at-fault component | `charts/modeldeployment` |
| Modeldeployment | pass-through | (preserved) | `inferenceset` | Any non-error or vLLM-native error (e.g. `429` rate-limit) is passed through unchanged; only the `x-kaito-error-source: inferenceset` header is added by a chart-rendered Envoy response-header filter on the upstream cluster | `charts/modeldeployment` |

##### 2.3 Notable behaviors

Two behaviours of the merged catalogue are worth calling out explicitly because they motivate concrete requirements in §3:

1. **Cluster-filter outages must not silently surface as `404`.** BBR ext_proc and `llm-gateway-auth` ext_authz both default to `failure_mode_allow: true`. Left at the default, a BBR outage would silently skip `X-Gateway-Model-Name` insertion and the request would fall through the namespace's catch-all `EnvoyFilter` as `404 model_not_found` (the same trap exists for ext_authz failing open). The catalogue closes this in two places: (a) both filters MUST be configured fail-closed and a cluster-wide `local_reply` MUST map `ext_proc_error` / `ext_authz_error` to `bbr_unavailable` / `ext_authz_unavailable` (see the `charts/productionstack` row in §3); (b) the modelharness catch-all `EnvoyFilter` MUST distinguish `X-Gateway-Model-Name` **absent** (→ `502 bbr_unavailable`, defence-in-depth) from **present but no `HTTPRoute` matched** (→ `404 model_not_found`) (see the `charts/modelharness` row in §3).
2. **`model_unavailable` vs. `model_not_found` are deterministically separable on the request path.** `charts/modeldeployment` always renders an `HTTPRoute` for the model name regardless of whether the `InferencePool` currently has ready endpoints. Therefore: matched route + empty `InferencePool` → `503 model_unavailable` (root-cause-neutral; see Trigger column); no matching route → `404 model_not_found`. The operator-facing root cause for `model_unavailable` is intentionally **not** carried on the response — it is published as one of the modeldeployment-layer Warning events per §1.2, and TSG-2's `model_unavailable` entry directs the operator to inspect that event stream. Alternatives that would discriminate the root cause on the request path (EPP patches, control-plane-state-reading sidecars) are rejected — see Alternatives.

#### 3. Requirements

This section enumerates the requirements that any implementing PR MUST satisfy, grouped by the component that owns the change. Concrete code shape (file paths, struct definitions, template names, RBAC verb lists) is left to the component owners.

| Component | Requirements |
| --- | --- |
| `productionstack-status-reporter` (new Deployment, owned by `charts/productionstack`; HA, leader-elected, read-only API access, no new CRD) | - **Single producer**: MUST be the sole producer of the §1.2 reason catalogue as Kubernetes `Event`s in `kube-system`. No other component MAY emit those reasons.<br>- **Namespace discovery**: MUST discover managed workload namespaces via a label selector (e.g. `productionstack.kaito.sh/managed-by: modelharness`) and watch their resources cross-namespace. No static namespace list MAY be required.<br>- **Continuous evaluation**: MUST evaluate every §1.2 reason on every reconcile via informer watches — not just at install time — so install-time misconfig, post-install drift, and runtime warnings (incl. `inferencesetWeightDownloadSlow`) are detected uniformly.<br>- **Cross-layer suppression**: MUST implement the upstream gating defined in §1.4 — suppress listed downstream reasons while their upstream cluster reason is active, append the transparency suffix to the upstream event `message`, and re-evaluate downstream on the next reconcile after the upstream resolves.<br>- **No TSG URLs in event messages**: control-plane event `message`s MUST NOT embed TSG URLs; TSG anchoring MUST be keyed off the stable `reason` string from §1.1.<br>- **Read-only KAITO coupling**: MUST consume upstream `Workspace` / `InferenceSet` conditions read-only; no new condition Types on `InferenceSet.status` MAY be required by this proposal. |
| `charts/productionstack` (umbrella chart, incl. `charts/body-based-routing` sub-chart) | - **Cluster-filter fail-closed**: BBR ext_proc MUST be configured with `failure_mode_allow: false`. The chart MUST render a cluster-wide `local_reply` (`EnvoyFilter`) mapping the `ext_proc_error` / `ext_authz_error` response flags to `bbr_unavailable` / `ext_authz_unavailable` per §2.3 item 1. |
| `charts/modelharness` | - **Labelling**: MUST stamp `productionstack.kaito.sh/managed-by: modelharness` on the workload `Namespace` (so the reporter can discover it via label selector) and `kaito.sh/owned-by: modelharness` on every harness-owned object.<br>- **Schema validation**: MUST ship `values.schema.json` covering at least the validations whose failures are surfaced by harness-level schema reasons in §1.2 (e.g. non-empty `gatewayClassName`, `networkPolicy.allowedIngressNamespaces`).<br>- **Catch-all + namespace `local_reply` overrides**: MUST split the catch-all `EnvoyFilter` so that `X-Gateway-Model-Name` **absent** → `502 bbr_unavailable` (defence-in-depth) and **present but unmatched** → `404 model_not_found` per §2.3 item 1. Namespace-scoped `local_reply` overrides for `gateway_dataplane_unhealthy` and `gateway_not_programmed` MUST also be rendered. |
| `charts/modeldeployment` | - **Labelling**: MUST stamp `kaito.sh/inferenceset: <name>` and `kaito.sh/owned-by: modeldeployment` on every chart-owned object (EPP `Deployment` / `Service`, `HTTPRoute`, `InferencePool`, `ConfigMap`).<br>- **Schema validation**: MUST ship `values.schema.json` covering at least the validations whose failures are surfaced by modeldeployment-level schema reasons in §1.2 (e.g. `maxReplicas >= replicas`, non-empty `model`, positive `scalingThreshold`, positive `controlPlane.weightDownload.minMBps`).<br>- **EPP error mapping**: MUST render an `EnvoyFilter` on the EPP-targeted cluster mapping Envoy response flags to the corresponding `(code, x-kaito-error-source)` pairs in §2.2, including the deliberately root-cause-neutral `503 model_unavailable` for `no_healthy_upstream` (§2.3 item 2). No upstream patches to `llm-d-inference-scheduler` MAY be required by this work. |
| `llm-gateway-auth` | - **Deny path**: both deny builders (`apikey`, `azure`) MUST emit the OpenAI envelope and `x-kaito-error-source: authz`. gRPC `PermissionDenied` MUST map to HTTP `403 api_key_disabled`; other denies remain `401 invalid_api_key`. <br>- Its own chart MUST set `failure_mode_allow: false` on the ext_authz filter. |

### User Stories

#### Story 1 — Operator diagnoses a stuck modeldeployment

An operator installs a modeldeployment in a region where the requested `instanceType` has zero quota. They run:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter,involvedObject.name=qwen \
  --sort-by=.lastTimestamp
```

and see:

```
LAST SEEN   TYPE      REASON                                  OBJECT                    MESSAGE
12s         Warning   inferencesetInfraProvisioningFailed     InferenceSet/qwen         InferenceSet my-models/qwen: GPU node provisioning failed: quota exceeded for Standard_NV36ads_A10_v5 in eastus.
```

No other resource needs to be inspected to locate root cause. The `reason` (`inferencesetInfraProvisioningFailed`) is the stable key tooling uses to deep-link into TSG-1 outside the event payload.

#### Story 2 — Operator diagnoses a harness-local misconfiguration

An operator hand-edited the rendered `AuthorizationPolicy` in workload namespace `my-models` and changed the `extensionProviders` reference to a typo that no longer matches the cluster-registered provider. `llm-gateway-auth` itself is healthy. They run:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter,reason=modelharnessExtAuthzProviderMissing \
  --sort-by=.lastTimestamp
```

and see one harness-local event scoped to the affected workload namespace:

```
LAST SEEN   TYPE      REASON                                OBJECT                                  MESSAGE
8s          Warning   modelharnessExtAuthzProviderMissing   AuthorizationPolicy/my-models/apikey    Namespace my-models: AuthorizationPolicy 'apikey' references extension provider 'apikey-ext-authz-typo' which is not registered in MeshConfig.extensionProviders; re-apply charts/modelharness with the correct providerName.
```

The operator knows this is harness-local (no `cluster*` event is open for the provider) and fixes it by re-applying the chart. Conversely, had `llm-gateway-auth` itself been uninstalled cluster-wide, the operator would have seen `reason=clusterGatewayAuthNotReady` in `kube-system` and remediated at the cluster layer — the reporter does **not** re-classify cluster-layer outages as harness-local reasons, so there is exactly one event per root cause.

#### Story 3 — Operator diagnoses a broken cluster install

An operator installs the `productionstack` umbrella chart but BBR cannot start (image pull failure on the air-gapped cluster). The `productionstack-status-reporter` emits a `Warning` event in `kube-system`:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter,reason=clusterBBRNotReady
```

```
LAST SEEN   TYPE      REASON                OBJECT                                           MESSAGE
5s          Warning   clusterBBRNotReady    Deployment/body-based-routing (istio-system)     body-based-routing deployment has 0/2 ready replicas: ImagePullBackOff on bbr container.
```

#### Story 4 — Client gets actionable HTTP error

A client calls `POST /v1/chat/completions` with `model: "qwen-typo"`. Production-stack returns:

```http
HTTP/1.1 404 Not Found
Content-Type: application/json
x-kaito-error-source: gateway

{ "error": { "type": "invalid_request_error", "code": "model_not_found",
             "message": "model(qwen-typo) doesn't exist", "param": "model" } }
```

If the operator has just scaled the deployment from zero, the same path returns instead:

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 10
x-kaito-error-source: epp

{ "error": { "type": "service_unavailable", "code": "model_unavailable",
             "message": "model(qwen) has no ready replicas; see Events in kube-system for root cause" } }
```

#### Story 5 — BBR outage no longer disguised as 404

When BBR's ext_proc pod is unavailable (cluster-level filter outage), the Istio Gateway returns a structured `502` with `code: bbr_unavailable` and `x-kaito-error-source: bbr` instead of falling through to the namespace catch-all `model_not_found`. Both the operator (via metrics + cluster-level condition) and the client (via response body) can tell BBR is at fault. The same disambiguation applies when `llm-gateway-auth` ext_authz is unavailable (`502 ext_authz_unavailable`, `x-kaito-error-source: authz`). In both cases the envelope is rendered by the cluster-wide chart `local_reply_config`; neither component is patched.

## Alternatives

- **Mirror sub-conditions as new `InferenceSet` condition Types (`InferenceSetInfraReady`, `InferenceSetModelPodsReady`).** Rejected: the owned `Workspace` CR already exposes `NodeClaimReady`, `ResourceReady`, and `InferenceReady`, which the reporter reads directly (e.g. `NodeClaimReady=False` → `inferencesetInfraProvisioningFailed`). New `InferenceSet` Types would duplicate that signal and add a CR-status contract no consumer reads — this proposal aggregates only on the `kube-system` event stream, not on `InferenceSet.status`. EPP, `HTTPRoute`, and cross-layer dependencies sit outside `Workspace` and are detected by the reporter's own watches.
- **Aggregate control-plane state into a new Kubernetes resource** — either per-layer ConfigMaps (`productionstack-status` / `modelharness-status`) or new `ModelDeployment` / `ProductionStack` CRDs with their own aggregator controllers. Rejected: introduces new resources, Helm ownership annotations, race-free reporter writes, and a separate upgrade story — all to give operators something they already get from `kubectl get events -n kube-system`. The unified event stream is queryable by every existing tool (kubectl, dashboards, log shippers) and benefits from standard `EventRecorder` aggregation without adding a new control-plane component or CR-status contract on the upgrade path.
- **Discriminate `model_unavailable` root cause on the request path** (patch `llm-d-inference-scheduler` to emit a more specific `code`, or add a sidecar that reads control-plane state and rewrites the response). Rejected: production-stack consumes `llm-d-inference-scheduler` as an unmodified upstream binary, and a status-reading sidecar adds a failure domain on the hot path. Every "zero ready endpoints" sub-cause (warm-up / crash / OOM / eviction) demands the same client behaviour (back off on `Retry-After` and retry), so a discriminated `code` would not change client behaviour. Operator-facing root cause is preserved as a `Warning` `Event` in `kube-system` per §1.2.

## Test Plan

**E2E tests** (under `test/e2e/`)

All control-plane assertions use `kubectl get events -n kube-system --field-selector source=productionstack-status-reporter,reason=<reason>` (or the Go equivalent in the test harness) instead of reading any ConfigMap or `InferenceSet.status` field. Each row exercises a specific `reason` from §1.2 (control-plane) or `code` from §2.2 (data-plane), or a cross-cutting requirement from §3.

| Layer | Test file | Scenario | Asserted outcome |
| --- | --- | --- | --- |
| Cluster | `cluster_status_test.go` (new) | scale `body-based-routing` Deployment to zero, then back | `Warning` `clusterBBRNotReady` appears in `kube-system` within one reporter resync; clears (followed by `Normal` `clusterReady`) when scaled back |
| Cluster | `cluster_status_test.go` (new) | scale `llm-gateway-auth` Deployment to zero | `Warning` `clusterGatewayAuthNotReady` |
| Cluster | `cluster_status_test.go` (new) | scale the KAITO workspace controller Deployment to zero | `Warning` `clusterKaitoControllerNotReady` |
| Cluster | `cluster_status_test.go` (new) | delete a required CRD | (a) reporter emits `Warning` `clusterCRDMissing`; (b) CRD-dependent components exit on startup timeout and are restarted by Kubernetes until the CRD returns |
| Modelharness | `harness_status_test.go` (new) | hand-edit `AuthorizationPolicy` provider name while `llm-gateway-auth` is healthy | `Warning` `modelharnessExtAuthzProviderMissing` whose `message` names the affected namespace; clears (followed by `modelharnessReady`) when the chart is re-applied |
| Modelharness | `harness_status_test.go` (new) | apply an `APIKey` CR that conflicts with an existing Secret while `apikey-operator` is healthy | `Warning` `modelharnessAPIKeyReconcileFailed` on the `APIKey` CR; clears when the CR is fixed |
| Modelharness | `harness_status_test.go` (new) | install a workload `Namespace` **without** the `productionstack.kaito.sh/managed-by: modelharness` label and introduce a known harness misconfiguration | no `modelharness*` events are emitted (reporter ignores unlabelled namespaces); when the label is added, the corresponding `Warning` appears on the next reconcile (verifies the §3 namespace-discovery requirement) |
| Modelharness | extend `network_policy_test.go` | `allowedIngressNamespaces` references a nonexistent namespace | `Warning` `modelharnessNetworkPolicyMisconfigured` emitted; clears (followed by `modelharnessReady`) when the namespace is created |
| Modeldeployment | `control_plane_error_test.go` (new) | install with invalid `instanceType` | `Warning` `inferencesetInfraProvisioningFailed` on the owning `InferenceSet`; `message` includes namespace and InferenceSet name |
| Modeldeployment | `control_plane_error_test.go` (new) | install while a `clusterBBRNotReady` event is open | only the existing `clusterBBRNotReady` event remains in `kube-system`; reporter does **not** emit a per-InferenceSet placeholder (verifies the no-re-classification rule in §1.3; `clusterBBRNotReady` is **not** in the §1.4 suppression table, so unrelated downstream reasons continue to be emitted independently) |
| Modeldeployment | `control_plane_error_test.go` (new) | after `inferencesetReady` is emitted, scale the EPP `Deployment` to zero | transitions to `Warning` `inferencesetEPPNotReady` on the EPP `Deployment`; clears (followed by `inferencesetReady`) when scaled back (verifies post-install drift detection) |
| Modeldeployment | `control_plane_error_test.go` (new) | after `inferencesetReady` is emitted, delete the parent namespace `Gateway` | `Warning` `inferencesetRouteNotReady` on the `HTTPRoute` |
| Modeldeployment | extend `scaling_test.go` | install with `enableScaling=true` and `maxReplicas < replicas` | `Warning` `inferencesetScalingMisconfigured` is emitted at reconcile time, independent of any pod or NodeClaim state (verifies the §1.3 modeldeployment priority head) |
| Modeldeployment | `weight_download_slow_test.go` (new) | inject a prefetch-metric stub that reports throughput `< 20 MB/s` while the inference pod is in `ContainerCreating` | a single `Warning` `inferencesetWeightDownloadSlow` on the owning `InferenceSet`, whose `message` names the workload namespace and the InferenceSet name, with `related` pointing at the LLM workload `Pod` and the prefetch `Pod`; not re-emitted while throughput stays below threshold; does **not** suppress `inferencesetReady` once the pod becomes Ready |
| Cross-layer (§1.4) | `upstream_gating_test.go` (new) | scale the KAITO workspace controller to zero while two InferenceSets exist with `ImagePullBackOff` pods | `clusterKaitoControllerNotReady` Warning carries `(suppressing downstream reasons: inferencesetInfraProvisioningFailed, inferencesetModelPodsNotReady in 2 namespace(s))` suffix; the listed downstream reasons are **not** emitted; `inferencesetEPPNotReady` (if any) is **still** emitted because it is not gated; once KAITO is scaled back, `clusterReady` fires first, then the per-InferenceSet reasons re-evaluate on the next reconcile |
| Cross-layer (§1.4) | `upstream_gating_test.go` (new) | scale `llm-gateway-auth` to zero in a namespace whose `AuthorizationPolicy` already has a deliberate provider-name typo | only `clusterGatewayAuthNotReady` is emitted with `(suppressing downstream reasons: modelharnessAPIKeyReconcileFailed, modelharnessExtAuthzProviderMissing in 1 namespace(s))` suffix; the local typo stays silent; once `llm-gateway-auth` recovers, `modelharnessExtAuthzProviderMissing` emerges on the next reconcile (verifies the `clusterGatewayAuthNotReady` row of the §1.4 suppression table) |
| Cross-cutting | `event_message_hygiene_test.go` (new) | after the full E2E suite has run, list every event emitted by `source=productionstack-status-reporter` in `kube-system` | assert no `message` contains `http://` or `https://` (verifies the §3 reporter "No TSG URLs in event messages" requirement) |
| Request path | extend `apikey_auth_test.go` | normal `401` deny (missing or unknown `Authorization`) | `401 invalid_api_key` envelope + `x-kaito-error-source: authz` |
| Request path | extend `apikey_auth_test.go` | valid token whose `APIKey` CR is explicitly disabled | `403 api_key_disabled` envelope + `x-kaito-error-source: authz` (verifies the §3 `llm-gateway-auth` deny-path 403 mapping) |
| Request path | extend `model_routing_test.go` | unknown model name | `404 model_not_found` envelope + `x-kaito-error-source: gateway` |
| Request path | `invalid_request_body_test.go` (new) | POST a body that BBR cannot parse (not JSON, missing `model`) | `400 invalid_request_body` envelope + `x-kaito-error-source: bbr` |
| Request path | `bbr_outage_test.go` (new) | scale BBR Deployment to zero | `502 bbr_unavailable` envelope + `x-kaito-error-source: bbr` (not `404`; verifies fail-closed BBR + cluster-wide `local_reply`) |
| Request path | `ext_authz_outage_test.go` (new) | scale `apikey-authz` Deployment to zero | `502 ext_authz_unavailable` envelope + `x-kaito-error-source: authz` (verifies fail-closed ext_authz + cluster-wide `local_reply`) |
| Request path | `gateway_dataplane_test.go` (new) | scale the namespace `Gateway` pod to zero | `502 gateway_dataplane_unhealthy` envelope + `x-kaito-error-source: gateway` |
| Request path | `epp_outage_test.go` (new) | scale the EPP Deployment to zero | `502 epp_unavailable` envelope + `x-kaito-error-source: epp` |
| Request path | `upstream_timeout_test.go` (new) | inject an inference pod that sleeps past the route timeout | `504 upstream_timeout` envelope + `x-kaito-error-source: inferenceset` |
| Request path | `model_unavailable_test.go` (new), warm-up | `replicas=0`, send a request | `503 model_unavailable` with `Retry-After` + `x-kaito-error-source: epp`; a concurrent `Warning` `inferencesetModelPodsNotReady` (or `inferencesetInfraProvisioningFailed` if no node yet) is present in `kube-system` |
| Request path | `model_unavailable_test.go` (new), crash | wait for Ready, then inject a crash-loop (`exit 1`) | same `503 model_unavailable` response shape — proves request-path code is root-cause-agnostic — while `Warning` `inferencesetModelPodsNotReady` is emitted on the owning `InferenceSet` |

**Manual verification.** Each TSG-1 control-plane `reason` and TSG-2 data-plane `code` is reachable via internal tooling from its corresponding event `reason` (control-plane) or response-body `code` (data-plane). Both TSGs are internal-only: control-plane event `message`s and data-plane response bodies alike MUST NOT carry TSG URLs.

## Implementation History

- [x] 2026-05-19: Proposed in [issue #71](https://github.com/kaito-project/production-stack/issues/71); initial proposal PR opened (modeldeployment-only scope)
- [x] 2026-05-21: Expanded scope to cluster + modelharness + modeldeployment; restructured under two top-level categories (control-plane / data-plane)
- [x] 2026-05-26: Removed all control-plane aggregator ConfigMaps; control-plane errors are now surfaced exclusively as Kubernetes `Event`s in `kube-system`. Merged §1.1–§1.4 into a single section with one unified reason catalogue. Added `inferencesetWeightDownloadSlow` warning (default threshold `< 20 MB/s`, sourced from prefetch pod metrics).
- [x] 2026-05-28: Required `inferencesetWeightDownloadSlow` to include the workload namespace and `InferenceSet` name in `message`, and switched its `involvedObject` to the owning `InferenceSet` (with `related` pointing at the LLM workload `Pod` and the prefetch `Pod`) so operators can pivot directly.
- [x] 2026-05-28: Consolidated §2: merged the per-layer §2.2 / §2.3 / §2.4 catalogues into one §2.2 table and demoted the standalone §2.5 / §2.6 sections into a shorter §2.3 "Notable behaviors" callout. Rewrote §3 as a per-component "Requirements" table (reporter, `charts/productionstack`, `charts/modelharness`, `charts/modeldeployment`, `llm-gateway-auth`).
- [x] 2026-05-28: Expanded the E2E Test Plan to cover the full §1.2 / §1.4 / §2.2 / §3 surface: new control-plane rows (`clusterKaitoControllerNotReady`, `inferencesetEPPNotReady`, `inferencesetRouteNotReady`, `inferencesetScalingMisconfigured`, `modelharnessAPIKeyReconcileFailed`), a second §1.4 suppression row, cross-cutting hygiene rows, and data-plane rows for the five previously untested `code`s.
- [ ] TBD: Upstream code `llm-gateway-auth` (envelope + 403 status mapping)
- [ ] TBD: `productionstack-status-reporter` controller implemented — single Deployment in `kube-system` that watches `Workspace` / `InferenceSet` / EPP / `HTTPRoute` / authz objects across discovered workload namespaces and emits §1.2 control-plane reasons as Kubernetes `Event`s (including the §1.4 transparency suffix when upstream gating fires)
- [ ] TBD: Charts merged — `charts/productionstack`, `charts/modelharness`, `charts/modeldeployment`
- [ ] TBD: TSGs merged — TSG-1 (control-plane errors) and TSG-2 (data-plane errors), both internal-only
