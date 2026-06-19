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
last-updated: 2026-08-06
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
        - [1.3 Emission model](#13-emission-model)
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

- **modelharness**: The Helm release rendered by [`charts/modelharness`](../../charts/modelharness) — one per workload namespace. Provisions the namespace `Gateway`, the per-namespace dataplane `EnvoyFilter`s (`bbr-ext-proc`, `model-not-found-direct`, `gateway-filter-outage-local-reply`, plus `apikey-ext-authz` when auth is enabled), the `APIKey` CR, and the `CiliumNetworkPolicy` resources (requires the cluster Cilium dataplane; see `charts/modelharness` for rationale).
- **modeldeployment**: The Helm release rendered by [`charts/modeldeployment`](../../charts/modeldeployment) — one `InferenceSet`, one `InferencePool`, one EPP `Deployment`/`Service`/RBAC/`ConfigMap`, and one `HTTPRoute`, parented to the per-namespace `Gateway`.
- **EPP**: Endpoint Picker — per-model `llm-d-inference-scheduler` ext_proc pod that performs KV-cache aware routing.
- **BBR**: Body-Based Router — the cluster-wide ext_proc filter (in the umbrella release namespace, `kaito-system` by default) that parses request bodies and injects the `X-Gateway-Model-Name` header.
- **InferenceSet**: `kaito.sh/v1beta1` CR owned by the KAITO controller; the canonical declaration of one model deployment.

## Summary

Production-stack today has no coherent end-to-end error story. Failures occur at three distinct layers — cluster bootstrap (KAITO / Istio / CRDs / bbr/ keda-kaito-scaler / `llm-gateway-auth`), per-namespace harness setup (`Gateway`, catch-all `EnvoyFilter`, `AuthorizationPolicy`, `APIKey`, `CiliumNetworkPolicy`), and per-model deployment (`InferenceSet`, `InferencePool`, EPP, `HTTPRoute`) — and each surface emits errors in its own format on its own object. On the request path, distinct failures (cluster-wide ext_authz outage, BBR outage, missing namespace Gateway, EPP outage, model still warming up, model name truly unknown) all collapse onto indistinguishable `404`s or unbodied `503`s, and response bodies do not follow a stable schema.

This proposal addresses all three levels and organises every error into one of two top-level categories:

1. **Control-plane errors** — failures observable inside the cluster, covering install-time failures **and** post-install drift (a shared Deployment crashing, a Gateway failing to program, an EPP pod entering `CrashLoopBackOff`, GPU node pressure, model-weights downloading too slowly, etc.). Surfaced **exclusively as Kubernetes `Event`s published to the `kube-system` namespace** by a single leader-elected `productionstack-status-reporter` workload shipped with the umbrella chart. Component-local status fields (`Workspace.status`, `Gateway.status`, `Deployment.status`) are preserved for per-component diagnosis, but the production-stack-level cross-layer view lives entirely in the event stream.
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
| **Control-plane errors** (Kubernetes `Event`s in `kube-system`; install-time failures **and** post-install drift) | - shared Deployment startup or runtime failure<br>- KAITO/Istio/KEDA/controller readiness transitions<br>- configured node-provisioner readiness | - namespace `Gateway` acceptance/programming regression<br>- programmed Gateway data-plane pod unavailable | - Workspace-reported infra or model-pod failure<br>- GPU node resource pressure<br>- EPP startup or runtime regression<br>- slow model-weights download (`< 20 MB/s` from prefetch pod) |
| **Data-plane errors** (request path → OpenAI-compatible HTTP responses) | - BBR ext_proc outage<br>- `llm-gateway-auth` ext_authz outage | - namespace gateway dataplane outage<br>- missing/invalid `APIKey` secret<br>- `CiliumNetworkPolicy` blackhole<br>- catch-all `model_not_found` | - EPP outage<br>- no ready model endpoints (warming up / crash / OOM / eviction)<br>- upstream pod timeout<br>- EPP internal error |

The section below enumerates the unified taxonomy for all three layers in a single table, then describes the emission model and the per-component changes required.

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
| `type` | Always `Warning` — the reporter only publishes problems; a healthy state is the absence of a `Warning`, not a positive event. |
| `reason` | One of the stable strings in §1.2 |
| `source.component` | `productionstack-status-reporter` |
| `involvedObject` | The cluster-scoped `Namespace` that contains the problematic resource: the workload `Namespace` for harness/modeldeployment reasons, or the component's install `Namespace` such as `istio-system`, `kaito-system`, or `keda` for cluster-layer reasons. Kubernetes requires the event's `metadata.namespace` to match the `involvedObject`'s namespace for namespaced resources, so a namespaced failing object cannot be referenced cross-namespace from `kube-system`. The specific failing resource name is carried in `message` so operators can still pivot to `kubectl describe`. |
| `message` | Human-readable description of the failure. The message MUST identify the affected workload namespace and (for modeldeployment-layer reasons) the `InferenceSet` name, so operators can pivot directly. For cluster-layer reasons the message MUST also identify the specific failing namespaced resource (e.g. the `Deployment` name and its install namespace), since `involvedObject` only names the containing `Namespace`. The message MUST NOT carry internal-only links (e.g. TSG URLs). |

A `Warning` event is emitted while its reason is active; a healthy state is the absence of a `Warning`, not a positive event. The reporter derives a stable Event name from the logical subject (`GroupKey`) and reason. On each resync, an active finding creates that Event or updates the existing Event by incrementing `count` and refreshing `lastTimestamp` and `message`. Recovery stops further updates; the retained Event ages out according to the cluster's Event TTL.

##### 1.2 Reason catalogue

The single table below replaces the previous per-layer Reason tables. The `Layer` column is informational; the `reason` string itself is layer-prefixed (`cluster*` / `modelharness*` / `inferenceset*`) so each value is globally unique and maps unambiguously to a TSG-1 anchor.

| Layer | `reason` | `type` | Triggered by | Detection source | `involvedObject` |
| --- | --- | --- | --- | --- | --- |
| Cluster | `clusterBBRNotReady` | Warning | `body-based-routing` subchart Deployment NotReady: `ImagePullBackOff`, RBAC errors, runtime crash, scale-to-zero | `Deployment.status` of the `body-based-router` Deployment | the `body-based-routing` install `Namespace` (default `kaito-system`; the BBR `Deployment` is identified in `message`) |
| Cluster | `clusterKedaKaitoScalerNotReady` | Warning | `keda-kaito-scaler` subchart Deployment NotReady (install-time or runtime) | `Deployment.status` of the `keda-kaito-scaler` Deployment | the `keda-kaito-scaler` install `Namespace` (the `Deployment` name is identified in `message`) |
| Cluster | `clusterGatewayAuthNotReady` | Warning | The configured `llm-gateway-auth` Deployment is missing or NotReady (default: `apikey-authz`) | `Deployment.status` and backing Pod readiness | the `llm-gateway-auth` install `Namespace` (the failing `Deployment` is identified in `message`) |
| Cluster | `clusterIstioControlPlaneNotReady` | Warning | The configured `istiod` Deployment is missing or NotReady | `Deployment.status` and backing Pod readiness | `Namespace/istio-system` (the `istiod` `Deployment` is identified in `message`) |
| Cluster | `clusterKaitoControllerNotReady` | Warning | KAITO workspace controller `Deployment` NotReady | `Deployment.status` of KAITO controller | the KAITO controller install `Namespace` (default `kaito-system`; the controller `Deployment` is identified in `message`) |
| Cluster | `clusterKedaNotReady` | Warning | KEDA control plane components NotReady: `keda-operator` and `keda-operator-metrics-apiserver` (in the `keda` namespace, regardless of whether KEDA is installed as a managed add-on or via upstream Helm) | `Deployment.status` of `keda-operator` and `keda-operator-metrics-apiserver` | `Namespace/keda` (the failing `Deployment` is identified in `message`) |
| Cluster | `clusterNodeProvisionerNotReady` | Warning | Node-provisioner Deployment NotReady. The reporter probes whichever provisioner is registered:<br>- upstream Karpenter (`karpenter` Deployment in the `karpenter` namespace)<br>- `gpu-node-mocker` (`gpu-node-mocker` Deployment, see `charts/gpu-node-mocker`) used for E2E<br>- any other Deployment registered via `clusterStatus.nodeProvisioner.{name,namespace}` chart values<br>If none is registered, the check is skipped (treated as Ready), so clusters that pre-provision GPU nodes are not penalised. | `Deployment.status` of the configured node-provisioner Deployment | the configured node-provisioner's install `Namespace` (the provisioner `Deployment` is identified in `message`) |
| Modelharness | `modelharnessGatewayClassMissing` | Warning | `Gateway.status.conditions[Accepted]=False` with `Reason=NoMatchingParent`, `InvalidParameters`, or `UnsupportedValue`, indicating that the configured GatewayClass cannot accept the Gateway | periodic read of the top-level Gateway `Accepted` condition | the workload `Namespace` (the `Gateway` name is identified in `message`) |
| Modelharness | `modelharnessGatewayProgrammingFailed` | Warning | The top-level Gateway `Programmed` condition is `False` | periodic read of `Gateway.status.conditions[Programmed]`; debounced via a fixed 5-minute grace window | the workload `Namespace` (the `Gateway` name is identified in `message`) |
| Modelharness | `modelharnessGatewayDataPlaneNotReady` | Warning | The `Gateway` is `Programmed=True`, but a discovered modelharness-owned backing Deployment has no Ready pod | list Deployments labelled `gateway.networking.k8s.io/gateway-name=<gateway>` and `kaito.sh/owned-by=modelharness`, then inspect backing Pod readiness; debounced via a fixed 5-minute grace window | the workload `Namespace` (the `Gateway` name and scheduling/readiness cause are identified in `message`) |
| Modeldeployment | `inferencesetInfraProvisioningFailed` | Warning | GPU node provisioning has reached an actionable failure such as quota, capacity, SKU, or subscription failure | child Workspace `NodeClaimReady=False`; `NodeClaimNotReady` and `AwaitingReconciliation` are treated as in-progress, while other reasons are surfaced after a fixed 3-minute debounce anchored on `lastTransitionTime` | the workload `Namespace` (the owning `InferenceSet`, Workspace, reason, and message are identified in `message`) |
| Modeldeployment | `inferencesetModelPodsNotReady` | Warning | KAITO has classified an actionable inference workload failure such as image pull failure, unschedulable pod, crash loop, OOM, or eviction | child Workspace `InferenceReady=False`; the generic `WorkspaceInferenceStatusPending` reason is ignored, while other non-empty reasons bypass startup grace and surface immediately | the workload `Namespace` (the owning `InferenceSet`, Workspace, reason, and message are identified in `message`) |
| Modeldeployment | `inferencesetNodeUnderPressure` | Warning | A GPU worker node reports sustained Disk, Memory, or PID pressure and model pods may be evicted | child Workspace `NodesReady` condition message contains the KAITO `under resource pressure` marker; debounced for 30 seconds | the workload `Namespace` (the owning `InferenceSet` and pressure detail are identified in `message`) |
| Modeldeployment | `inferencesetEPPNotReady` | Warning | Install-time: EPP image pull failure, malformed `ConfigMap`, RBAC missing for list pods, `--pool-name` mismatch. Runtime: EPP crash / restart loop / readiness-probe regression after the pod was previously Ready. | EPP `Deployment.status.conditions` + Pod state | the workload `Namespace` (the owning `InferenceSet` name and the EPP `Deployment` name are identified in `message`) |
| Modeldeployment | `inferencesetWeightDownloadSlow` | Warning | Sustained slow model-weights download while the LLM pod is initialising: **every** throughput sample in a sliding window (default **60 s**, `controlPlane.weightDownload.windowSeconds`) is below the threshold (default **20 MB/s**, `controlPlane.weightDownload.minMBps`). One sample ≥ threshold inside the window suppresses the event (no false alarms on transient dips). The window MUST be fully populated (≥ `windowSeconds`, ≥ 2 samples) before deciding. Emitted once per pod-start; resolved when the pod is Ready or download completes. `message` MUST name the workload namespace, `InferenceSet`, both pods, window, and worst observed throughput. | reporter scrapes the prefetch pod's Prometheus metric (e.g. `kaito_model_download_speed_bytes_per_second{pod}`) every reconcile into a per-pod ring buffer; samples older than `windowSeconds` are evicted | the workload `Namespace` (the owning `InferenceSet`, LLM `Pod`, prefetch `Pod`, window, and worst observed throughput are identified in `message`) |

Each `reason` corresponds to a stable anchor in **TSG-1**. The reporter is the single producer; emitting the same reason from any other component is forbidden.

##### 1.3 Emission model

The reporter evaluates the cluster, modelharness, modeldeployment, and weight-download layers independently on a periodic full resync (default 60 seconds), then emits **every** active reason as its own Event. There is no single-primary selection, priority collapse, or cross-layer suppression. Concurrent faults surface independently so one problem is never hidden behind another, and the absence of a freshly updated `Warning` is the only healthy signal. Within a layer, findings are grouped by logical subject and de-duplicated so each reason is written at most once per subject per resync.

Notes:

- `inferencesetWeightDownloadSlow` is emitted **in addition to** any other active modeldeployment reason (typically `inferencesetModelPodsNotReady` while the pod is still pulling weights), because its remediation (improve registry/cache throughput) is independent of the other failure modes.
- When **no** unhealthy reason is active for a layer, no event is emitted — a healthy state is represented by the absence of any `Warning`, not by a positive event.
- Evaluators perform periodic Kubernetes API reads rather than registering informer-driven watches. A lookup that cannot be evaluated is treated as unknown and does not emit a warning for that pass.
- Startup transients are gated by the global startup grace (default 60 seconds), a reason-specific override, or a terminal-failure exemption. Findings with a known transition/creation timestamp use object-age gating; findings without one use an in-memory first-observation debounce.

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

**Consolidated whole-path `local_reply` + component-first attribution.** All Envoy-generated `5xx` *local replies* on a namespace's request path — the cluster-filter hops (ext_authz / BBR), the namespace `Gateway` dataplane, AND the modeldeployment EPP / upstream-inference hops — are rewritten into the envelope by a **single** per-namespace `EnvoyFilter` (`envoyfilter-outage-reply`, owned by `charts/modelharness`). Consolidating into one mapper list removes the non-deterministic cross-`EnvoyFilter` merge order on the shared `Gateway` HCM that a split design would incur. Two orthogonal signals locate the at-fault component:

* **`X-Gateway-Model-Name` request header** — injected by BBR, which runs *before* route matching (EPP and the upstream run after). **Absent** ⇒ the failure is at/before BBR (infrastructure side: ext_authz, BBR, or the `Gateway` dataplane). **Present** ⇒ BBR ran and the request was routed to a model (model-serving side: EPP or the inference pods). The gateway strips any client-supplied value at ingress so it cannot be spoofed.
* **Envoy response flag** — a router/upstream flag (`UH`/`LH`/`UT`/`UPE`/`UF`/`UC`/`NC`) means the routed upstream or the dataplane failed; `UAEX` means ext_authz; **no** router flag on a local `5xx` means an ext_proc filter (BBR or EPP) failed closed.

Envoy's `local_reply` mapper filter is an `AccessLogFilter`, which has `and_filter` / `or_filter` / `response_flag_filter` / `header_filter` but **no** `not_filter`. "No router flag" is therefore expressed by **ordering**: every flag-bearing mapper precedes the two no-flag fallbacks, so a request that reaches the EPP/BBR fallbacks provably carries no router flag. The six mappers are evaluated top-to-bottom, first-match wins.

**Per-component codes are deliberately coarse** — one `code` per at-fault component (plus a stable `x-kaito-error-source`). The fine-grained root cause (zero-replica warm-up vs crash vs OOM vs eviction vs scheduler bug vs provisioning failure) is **not** carried on the response — it is published as a control-plane `Warning` `Event` in `kube-system` by the status reporter (§1.2). Returning more detail to the prompt client adds no actionable value: every sub-cause of a given component failure demands the same client behaviour (back off, retry).

##### 2.2 Error catalogue

The table below lists every data-plane error `code`, the HTTP status it surfaces on, the at-fault component named by `x-kaito-error-source`, what triggers it, and the chart that owns rendering it. Codes are grouped by layer: cluster-level codes affect every namespace; modelharness-level codes are per-namespace (incl. the consolidated `local_reply` above); modeldeployment-level codes are per-InferenceSet.

| Layer | HTTP | `code` | `x-kaito-error-source` | Trigger | Owner |
| --- | --- | --- | --- | --- | --- |
| Cluster | 502 | `ext_authz_unavailable` | `authz` | `llm-gateway-auth` ext_authz Deployment unreachable or returning 5xx; cluster-wide `local_reply` mapped from the `ext_authz_error` response flag | `charts/productionstack` |
| Cluster | 502 | `bbr_unavailable` | `bbr` | BBR ext_proc filter unreachable / errored; cluster-wide `local_reply` mapped from the `ext_proc_error` response flag | `charts/productionstack` |
| Cluster | 500 | `mesh_config_invalid` | `gateway` | `MeshConfig.extensionProviders` references an unknown ext_authz / ext_proc cluster; Envoy aborts filter chain build | `charts/productionstack` |
| Modelharness | 401 | `invalid_api_key` | `authz` | `Authorization` missing, token does not match any `APIKey` Secret resolvable from the host subdomain, or token is syntactically malformed. Emitted in-process by `llm-gateway-auth`. | `llm-gateway-auth` (in-process) |
| Modelharness | 403 | `api_key_disabled` | `authz` | Valid `APIKey` resolved but not authorised for this gateway namespace, or the `APIKey` CR is explicitly marked disabled. Same in-process emitter as `invalid_api_key`, HTTP `403`. Requires the `llm-gateway-auth` deny-path change in §3 to actually surface `403` (today `apikey-authz` collapses every deny to `401`). | `llm-gateway-auth` (in-process) |
| Modelharness | 400 | `invalid_request_body` | `bbr` | Body fails BBR parsing (not JSON, not OpenAI chat-completions schema, missing `model`); chart-rendered cluster-level `local_reply` renders the envelope | `charts/modelharness` (+ `charts/productionstack` `local_reply`) |
| Modelharness | 404 | `model_not_found` | `gateway` | `X-Gateway-Model-Name` is present but no `HTTPRoute` in this namespace matches | `charts/modelharness` |
| Modelharness | 502 | `gateway_unavailable` | `gateway` | `X-Gateway-Model-Name` **absent** (failure before model routing) AND a router/upstream flag fired: namespace `Gateway` dataplane has no ready upstream (`UH`/`LH`) or its listener/cluster is not yet programmed while the harness converges (`UF`/`UC`/`NC`). Consolidated `local_reply` mapper #3. | `charts/modelharness` |
| Modelharness | 502 | `bbr_unavailable` (per-namespace) | `bbr` | Local `5xx` with `X-Gateway-Model-Name` **absent** and **no** router flag: BBR ext_proc failed closed before injecting the header (pinned `status_on_error: 503`). Per-namespace defence-in-depth for the cluster-level `bbr_unavailable` row above — same `code` + source; consolidated `local_reply` mapper #5. | `charts/modelharness` |
| Modelharness | 502 | `epp_unavailable` | `epp` | Local `5xx` with `X-Gateway-Model-Name` **present** and **no** router flag: the EPP (`InferencePool.endpointPickerRef`, `failureMode: FailClose`) ext_proc filter failed closed when unreachable. EPP ext_proc is auto-injected by GAIE so it takes the ext_proc default `500`; the consolidated `local_reply` (mapper #4) rewrites it. | `charts/modelharness` |
| Modelharness | 503 | `model_unavailable` | `inferenceset` | `X-Gateway-Model-Name` **present** (routed to a model) AND a router/upstream flag fired (`UH`/`LH` zero ready endpoints — warm-up / crash / OOM / eviction; `UT` upstream timeout; `UPE` protocol error; `UF`/`UC`/`NC` connection failure). The `code` is deliberately root-cause-neutral because all sub-causes share the same client behaviour (back off on `Retry-After: 10` and retry). The operator-facing root cause is surfaced as a control-plane `Warning` `Event` in `kube-system` — one of `inferencesetInfraProvisioningFailed`, `inferencesetModelPodsNotReady`, or `inferencesetEPPNotReady` (§1.2). Consolidated `local_reply` mapper #2. | `charts/modelharness` |
| Modelharness | (preserved) | `gateway_internal_error` | `gateway` | Catch-all: any remaining local `5xx` not matched by a more specific mapper. `status_code` is preserved; only the body and `x-kaito-error-source` header are normalised. Consolidated `local_reply` mapper #6. | `charts/modelharness` |
| Modeldeployment | pass-through | (preserved) | `inferenceset` | Any non-error or vLLM-native error (e.g. `429` rate-limit) is proxied back unchanged; only `x-kaito-error-source: inferenceset` is stamped by the modeldeployment `HTTPRoute` `ResponseHeaderModifier`. It is **not** a local reply, so the consolidated `local_reply` never rewrites it. | `charts/modeldeployment` |

##### 2.3 Notable behaviors

Three behaviours of the merged catalogue are worth calling out explicitly because they motivate concrete requirements in §3:

1. **Cluster-filter outages must not silently surface as `404`.** BBR ext_proc and `llm-gateway-auth` ext_authz both default to `failure_mode_allow: true`. Left at the default, a BBR outage would silently skip `X-Gateway-Model-Name` insertion and the request would fall through the namespace's catch-all `EnvoyFilter` as `404 model_not_found` (the same trap exists for ext_authz failing open). The catalogue closes this in two places: (a) both filters MUST be configured fail-closed and a cluster-wide `local_reply` MUST map `ext_proc_error` / `ext_authz_error` to `bbr_unavailable` / `ext_authz_unavailable` (see the `charts/productionstack` row in §3); (b) the modelharness catch-all `EnvoyFilter` MUST distinguish `X-Gateway-Model-Name` **absent** (→ `502 bbr_unavailable`, defence-in-depth) from **present but no `HTTPRoute` matched** (→ `404 model_not_found`) (see the `charts/modelharness` row in §3).
2. **Cluster-level filters run highly-available and a single bad replica must not break the request path.** `llm-gateway-auth` (ext_authz) and BBR (ext_proc) are **cluster-scope** components on the hot path of *every* request in the cluster, so a single replica is a single point of failure. Both MUST run with **at least 2 replicas** (`replicas: 2`, anti-affinity across nodes). Because both are addressed by the Istio Gateway as ext_proc / ext_authz **upstream clusters**, the Gateway MUST automatically detect and eject an unhealthy replica so prompt requests are only forwarded to a healthy one. This is achieved with two Envoy mechanisms rendered by the chart on each filter's upstream cluster: (a) **active health checking** (a gRPC health check against the filter's serving port, so a replica that stops serving — `CrashLoopBackOff`, deadlock, failed readiness — is marked unhealthy and removed from the load-balancing set before it can take traffic), and (b) **passive outlier detection** (eject a replica from rotation after a configurable number of consecutive gRPC errors / 5xx, then probe it back in once healthy). With ≥ 2 replicas behind active + passive health checking, the loss of one replica is transparent to clients; the cluster-wide `502 bbr_unavailable` / `502 ext_authz_unavailable` fail-closed path of item 1 is therefore only reached when **all** replicas of a filter are simultaneously unhealthy, not when a single one is. The reporter still surfaces the degraded (but serving) state as a control-plane `Warning` — `clusterBBRNotReady` / `clusterGatewayAuthNotReady` — when a Deployment is below its desired ready-replica count, so operators see partial degradation before it becomes a full outage.
3. **`model_unavailable` vs. `model_not_found` are deterministically separable on the request path.** `charts/modeldeployment` always renders an `HTTPRoute` for the model name regardless of whether the `InferencePool` currently has ready endpoints. Therefore: matched route + empty `InferencePool` → `503 model_unavailable` (root-cause-neutral; see Trigger column); no matching route → `404 model_not_found`. The operator-facing root cause for `model_unavailable` is intentionally **not** carried on the response — it is published as one of the modeldeployment-layer Warning events per §1.2, and TSG-2's `model_unavailable` entry directs the operator to inspect that event stream. Alternatives that would discriminate the root cause on the request path (EPP patches, control-plane-state-reading sidecars) are rejected — see Alternatives.

#### 3. Requirements

This section enumerates the requirements that any implementing PR MUST satisfy, grouped by the component that owns the change. Concrete code shape (file paths, struct definitions, template names, RBAC verb lists) is left to the component owners.

| Component | Requirements |
| --- | --- |
| `productionstack-status-reporter` (Deployment owned by `charts/productionstack`; HA, leader-elected, read-only API access except Event aggregation and leader-election Leases; no new CRD) | - **Single producer**: MUST be the sole producer of the §1.2 reason catalogue as Kubernetes `Event`s in `kube-system`. No other component MAY emit those reasons.<br>- **Namespace discovery**: MUST discover managed workload namespaces via `productionstack.kaito.sh/managed-by=modelharness`. No static workload-namespace list MAY be required.<br>- **Periodic evaluation**: MUST evaluate each layer independently on a full resync (default 60 seconds) using read-only Kubernetes API lookups, covering both startup failures and post-install drift.<br>- **Independent findings**: MUST emit every active reason independently, with no priority collapse or cross-layer suppression.<br>- **Stable Event aggregation**: MUST derive a deterministic Event identity per logical subject and reason, increment `count`, and refresh `lastTimestamp`/`message` while the finding remains active. Recovery MUST stop refreshing the Event rather than delete it.<br>- **Startup gating**: MUST support a configurable global startup grace, per-reason overrides, first-observation debounce for findings without a timestamp, and immediate emission for classified terminal failures.<br>- **No TSG URLs in event messages**: control-plane Event `message`s MUST NOT embed TSG URLs; TSG anchoring MUST be keyed off the stable `reason` string from §1.1.<br>- **Read-only KAITO coupling**: MUST consume upstream `Workspace` / `InferenceSet` state read-only; no new condition Types on `InferenceSet.status` MAY be required by this proposal. |
| `charts/productionstack` (umbrella chart, incl. `charts/body-based-routing` sub-chart) | - **Cluster-filter fail-closed**: BBR ext_proc MUST be configured with `failure_mode_allow: false`. The chart MUST render a cluster-wide `local_reply` (`EnvoyFilter`) mapping the `ext_proc_error` / `ext_authz_error` response flags to `bbr_unavailable` / `ext_authz_unavailable` per §2.3 item 1.<br>- **BBR high availability**: the `body-based-routing` sub-chart MUST render the BBR Deployment with at least 2 replicas (default `replicas: 2`, configurable but with a schema minimum of 2) and pod anti-affinity spreading replicas across nodes per §2.3 item 2.<br>- **Automatic unhealthy-replica ejection**: the chart MUST configure the Gateway's BBR ext_proc upstream cluster with active gRPC health checking and passive outlier detection (per §2.3 item 2) so the Gateway forwards prompt requests only to healthy BBR replicas; a single unhealthy replica MUST NOT trigger the fail-closed `502 bbr_unavailable` path. |
| `charts/modelharness` | - **Labelling**: MUST stamp `productionstack.kaito.sh/managed-by: modelharness` on the workload `Namespace` (so the reporter can discover it via label selector) and `kaito.sh/owned-by: modelharness` on every harness-owned object.<br>- **Schema validation**: MUST ship `values.schema.json` covering at least the validations whose failures are surfaced by harness-level schema reasons in §1.2 (e.g. non-empty `gatewayClassName`, `networkPolicy.allowedIngressNamespaces`).<br>- **Consolidated whole-path `local_reply`**: MUST render a single per-namespace `EnvoyFilter` that rewrites every Envoy-generated `5xx` local reply on the request path into the §2.2 envelope, attributing the at-fault component from the `X-Gateway-Model-Name` header and the response flag per §2.2 (the six-mapper, ordering-based scheme that compensates for the absent `not_filter`). It MUST cover `gateway_unavailable`, `model_unavailable`, `epp_unavailable`, the per-namespace `bbr_unavailable` defence-in-depth, and the `gateway_internal_error` catch-all. The catch-all `EnvoyFilter` MUST additionally distinguish `X-Gateway-Model-Name` **absent** → `502 bbr_unavailable` and **present but unmatched** → `404 model_not_found` per §2.3 item 1. |
| `charts/modeldeployment` | - **Labelling**: MUST stamp `kaito.sh/inferenceset: <name>` and `kaito.sh/owned-by: modeldeployment` on every chart-owned object (EPP `Deployment` / `Service`, `HTTPRoute`, `InferencePool`, `ConfigMap`).<br>- **Schema validation**: MUST ship `values.schema.json` covering at least the validations whose failures are surfaced by modeldeployment-level schema reasons in §1.2 (e.g. `maxReplicas >= replicas`, non-empty `model`, positive `scalingThreshold`, positive `controlPlane.weightDownload.minMBps`, positive `controlPlane.weightDownload.windowSeconds`).<br>- **EPP fail-closed + pass-through stamp**: MUST set `failureMode: FailClose` on the `InferencePool.endpointPickerRef` so an unreachable EPP surfaces a local `5xx` (which the consolidated `charts/modelharness` `local_reply` maps to `epp_unavailable`), and MUST stamp `x-kaito-error-source: inferenceset` on pass-through responses via the `HTTPRoute` `ResponseHeaderModifier`. The actual `5xx`→envelope mapping (including the root-cause-neutral `503 model_unavailable`, §2.3 item 3) is owned by the consolidated per-namespace filter in `charts/modelharness`, NOT by a per-modeldeployment `EnvoyFilter`. No upstream patches to `llm-d-inference-scheduler` MAY be required by this work. |
| `llm-gateway-auth` | - **Deny path**: both deny builders (`apikey`, `azure`) MUST emit the OpenAI envelope and `x-kaito-error-source: authz`. gRPC `PermissionDenied` MUST map to HTTP `403 api_key_disabled`; other denies remain `401 invalid_api_key`. <br>- Its own chart MUST set `failure_mode_allow: false` on the ext_authz filter.<br>- **High availability**: as a cluster-scope filter on every request path, the `apikey-authz` ext_authz Deployment MUST run with at least 2 replicas (default `replicas: 2`, schema minimum of 2) and pod anti-affinity across nodes per §2.3 item 2.<br>- **Health endpoint for automatic ejection**: the ext_authz server MUST expose a gRPC health-check endpoint so the Gateway's ext_authz upstream cluster can actively health-check and passively outlier-detect replicas, forwarding prompt requests only to healthy ones; a single unhealthy replica MUST NOT trigger the fail-closed `502 ext_authz_unavailable` path. |

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
LAST SEEN   TYPE      REASON                                  OBJECT                 MESSAGE
12s         Warning   inferencesetInfraProvisioningFailed     Namespace/my-models    InferenceSet my-models/qwen: GPU node provisioning failed: quota exceeded for Standard_NV36ads_A10_v5 in eastus.
```

The `involvedObject` is the cluster-scoped workload `Namespace` (per the §1.1 schema constraint that events in `kube-system` cannot reference namespaced resources cross-namespace); the specific `InferenceSet` name is carried in `message` so the operator can pivot directly with `kubectl describe inferenceset -n my-models qwen`. The `reason` (`inferencesetInfraProvisioningFailed`) is the stable key tooling uses to deep-link into TSG-1 outside the event payload.

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
x-kaito-error-source: inferenceset

{ "error": { "type": "service_unavailable", "code": "model_unavailable",
             "message": "model(qwen) has no ready replicas; see Events in kube-system for root cause" } }
```

#### Story 5 — BBR outage no longer disguised as 404

When BBR's ext_proc pod is unavailable (cluster-level filter outage), the Istio Gateway returns a structured `502` with `code: bbr_unavailable` and `x-kaito-error-source: bbr` instead of falling through to the namespace catch-all `model_not_found`. Both the operator (via metrics + cluster-level condition) and the client (via response body) can tell BBR is at fault. The same disambiguation applies when `llm-gateway-auth` ext_authz is unavailable (`502 ext_authz_unavailable`, `x-kaito-error-source: authz`). In both cases the envelope is rendered by the cluster-wide chart `local_reply_config`; neither component is patched.

## Alternatives

- **Mirror sub-conditions as new `InferenceSet` condition Types (`InferenceSetInfraReady`, `InferenceSetModelPodsReady`).** Rejected: the KAITO Workspace controller already classifies these signals on child `Workspace.status.conditions`. The reporter reads `NodeClaimReady`, `InferenceReady`, and `NodesReady` directly and correlates child Workspaces through `inferenceset.kaito.sh/created-by`. New `InferenceSet` Types would duplicate that signal and add a CR-status contract no consumer reads. EPP readiness remains outside Workspace and is checked from the deterministic EPP Deployment.
- **Aggregate control-plane state into a new Kubernetes resource** — either per-layer ConfigMaps (`productionstack-status` / `modelharness-status`) or new `ModelDeployment` / `ProductionStack` CRDs with their own aggregator controllers. Rejected: introduces new resources, Helm ownership annotations, race-free reporter writes, and a separate upgrade story — all to give operators something they already get from `kubectl get events -n kube-system`. The unified event stream is queryable by every existing tool (kubectl, dashboards, log shippers); the reporter uses deterministic Event names and explicit create/update aggregation without adding a new CR-status contract.
- **Discriminate `model_unavailable` root cause on the request path** (patch `llm-d-inference-scheduler` to emit a more specific `code`, or add a sidecar that reads control-plane state and rewrites the response). Rejected: production-stack consumes `llm-d-inference-scheduler` as an unmodified upstream binary, and a status-reading sidecar adds a failure domain on the hot path. Every "zero ready endpoints" sub-cause (warm-up / crash / OOM / eviction) demands the same client behaviour (back off on `Retry-After` and retry), so a discriminated `code` would not change client behaviour. Operator-facing root cause is preserved as a `Warning` `Event` in `kube-system` per §1.2.

## Test Plan

**E2E tests** (under `test/e2e/`)

All control-plane assertions use `kubectl get events -n kube-system --field-selector source=productionstack-status-reporter,reason=<reason>` (or the Go equivalent in the test harness) instead of reading any ConfigMap or `InferenceSet.status` field. Because reporter Events persist until the cluster Event TTL expires, tests capture a per-reason baseline before perturbation and assert only Events created or refreshed after that baseline. The reporter rows below describe current E2E coverage; the three prefetch-based weight-download rows remain the target coverage for the completed prefetch-pod integration.

| Layer | Test file | Scenario | Asserted outcome |
| --- | --- | --- | --- |
| Cluster | `cluster_status_test.go` | scale `body-based-routing` Deployment to zero, then restore it | a fresh `Warning` `clusterBBRNotReady` appears in `kube-system`; after recovery, the Event stops being refreshed; the observed message contains no URL |
| Cluster | `cluster_status_test.go` | scale the configured `llm-gateway-auth` Deployment to zero | a fresh `Warning` `clusterGatewayAuthNotReady` appears and its message contains no URL |
| Cluster | `cluster_status_test.go` | scale the KAITO workspace controller Deployment to zero | a fresh `Warning` `clusterKaitoControllerNotReady` appears and its message contains no URL |
| Modeldeployment | `control_plane_error_test.go` | after the InferenceSet is healthy, scale its deterministic `<inferenceset>-inferencepool-epp` Deployment to zero | a fresh `Warning` `inferencesetEPPNotReady` appears on the workload `Namespace`; its message names the InferenceSet, states that the Deployment is scaled to zero, and contains no URL; the original replica count is restored after the test |
| Modeldeployment | `weight_download_slow_test.go` (new), sustained-slow | inject a prefetch-metric stub that reports throughput `< 20 MB/s` for **every** sample across a full 60 s evaluation window while the inference pod is in `ContainerCreating` | a single `Warning` `inferencesetWeightDownloadSlow` on the workload `Namespace` (per §1.1), whose `message` names the workload namespace, the InferenceSet name, the LLM workload `Pod`, the prefetch `Pod`, the evaluation window (`60s`), and the worst observed throughput across the window; not re-emitted while throughput stays below threshold; stops once the pod becomes Ready |
| Modeldeployment | `weight_download_slow_test.go` (new), transient-dip | inject a prefetch-metric stub that reports throughput `< 20 MB/s` for most of the 60 s window but produces at least one sample `≥ 20 MB/s` somewhere inside it | **no** `inferencesetWeightDownloadSlow` event is emitted (verifies the windowed-evaluation rule — a single in-threshold sample inside the window clears the verdict, so transient dips never raise the warning) |
| Modeldeployment | `weight_download_slow_test.go` (new), partial-window | inject a prefetch-metric stub that reports throughput `< 20 MB/s` but only enough samples to cover less than 60 s of the window (e.g. the pod has just started) | **no** `inferencesetWeightDownloadSlow` event is emitted until the window fills (verifies the reporter waits for the window to be fully populated before deciding, so a fresh pod does not immediately trip the warning) |
| Request path | extend `apikey_auth_test.go` | normal `401` deny (missing or unknown `Authorization`) | `401 invalid_api_key` envelope + `x-kaito-error-source: authz` |
| Request path | extend `apikey_auth_test.go` | valid token whose `APIKey` CR is explicitly disabled | `403 api_key_disabled` envelope + `x-kaito-error-source: authz` (verifies the §3 `llm-gateway-auth` deny-path 403 mapping) |
| Request path | extend `model_routing_test.go` | unknown model name | `404 model_not_found` envelope + `x-kaito-error-source: gateway` |
| Request path | `invalid_request_body_test.go` (new) | POST a body that BBR cannot parse (not JSON, missing `model`) | `400 invalid_request_body` envelope + `x-kaito-error-source: bbr` |
| Request path | `bbr_outage_test.go` (new) | scale BBR Deployment to zero | `502 bbr_unavailable` envelope + `x-kaito-error-source: bbr` (not `404`; verifies fail-closed BBR + cluster-wide `local_reply`) |
| Request path | `ext_authz_outage_test.go` (new) | scale `apikey-authz` Deployment to zero | `502 ext_authz_unavailable` envelope + `x-kaito-error-source: authz` (verifies fail-closed ext_authz + cluster-wide `local_reply`) |
| Request path | `cluster_filter_ha_test.go` (new), BBR single-replica loss | with BBR at 2 replicas, kill/cordon one replica (scale to 1, or `kubectl delete pod` one) and send a sustained stream of prompt requests | **zero** `502 bbr_unavailable` responses — every request succeeds because the Gateway's active health check + outlier detection ejects the lost replica and forwards only to the healthy one; a `Warning` `clusterBBRNotReady` is concurrently present in `kube-system` for the degraded Deployment (verifies the §2.3 item 2 HA + automatic-ejection requirement) |
| Request path | `cluster_filter_ha_test.go` (new), ext_authz single-replica loss | with `apikey-authz` at 2 replicas, kill one replica and send a sustained stream of authenticated prompt requests | **zero** `502 ext_authz_unavailable` and **zero** spurious `401` responses; all requests are authorised by the surviving replica; a `Warning` `clusterGatewayAuthNotReady` is concurrently present in `kube-system` (verifies the §2.3 item 2 HA + automatic-ejection requirement) |
| Request path | `gateway_dataplane_test.go` (new) | scale the namespace `Gateway` pod to zero | `502 gateway_unavailable` envelope + `x-kaito-error-source: gateway` (header absent + router flag → consolidated `local_reply` mapper #3) |
| Request path | `epp_outage_test.go` (new) | scale the EPP Deployment to zero | `502 epp_unavailable` envelope + `x-kaito-error-source: epp` (header present + no router flag → mapper #4) |
| Request path | `upstream_timeout_test.go` (new) | inject an inference pod that sleeps past the route timeout | `503 model_unavailable` envelope + `x-kaito-error-source: inferenceset` (upstream timeout `UT` with the model header present folds into `model_unavailable` per the coarse component-first scheme; no distinct `upstream_timeout` code) |
| Request path | `model_unavailable_test.go` (new), warm-up | `replicas=0`, send a request | `503 model_unavailable` with `Retry-After` + `x-kaito-error-source: inferenceset`; a concurrent `Warning` `inferencesetModelPodsNotReady` (or `inferencesetInfraProvisioningFailed` if no node yet) is present in `kube-system` |
| Request path | `model_unavailable_test.go` (new), crash | wait for Ready, then inject a crash-loop (`exit 1`) | same `503 model_unavailable` response shape — proves request-path code is root-cause-agnostic — while `Warning` `inferencesetModelPodsNotReady` is emitted on the workload `Namespace` (per §1.1; the owning `InferenceSet` is identified in `message`) |

**Manual verification.** Each TSG-1 control-plane `reason` and TSG-2 data-plane `code` is reachable via internal tooling from its corresponding event `reason` (control-plane) or response-body `code` (data-plane). Both TSGs are internal-only: control-plane event `message`s and data-plane response bodies alike MUST NOT carry TSG URLs.

## Implementation History

- [x] 2026-05-19: Proposed in [issue #71](https://github.com/kaito-project/production-stack/issues/71); initial proposal PR opened (modeldeployment-only scope)
- [x] 2026-05-21: Expanded scope to cluster + modelharness + modeldeployment; restructured under two top-level categories (control-plane / data-plane)
- [x] 2026-05-26: Removed all control-plane aggregator ConfigMaps; control-plane errors are now surfaced exclusively as Kubernetes `Event`s in `kube-system`. Consolidated the control-plane sections around one unified reason catalogue. Added `inferencesetWeightDownloadSlow` warning (default threshold `< 20 MB/s`, sourced from prefetch pod metrics).
- [x] 2026-05-28: Required `inferencesetWeightDownloadSlow` to include the workload namespace, `InferenceSet`, LLM workload Pod, and prefetch Pod in `message`; its Event follows the common schema and references the cluster-scoped workload `Namespace`.
- [x] 2026-05-28: Consolidated §2: merged the per-layer §2.2 / §2.3 / §2.4 catalogues into one §2.2 table and demoted the standalone §2.5 / §2.6 sections into a shorter §2.3 "Notable behaviors" callout. Rewrote §3 as a per-component "Requirements" table (reporter, `charts/productionstack`, `charts/modelharness`, `charts/modeldeployment`, `llm-gateway-auth`).
- [x] 2026-05-28: Expanded the data-plane Test Plan and the initial control-plane test design.
- [x] 2026-05-29: Aligned every control-plane Event's `involvedObject` with Kubernetes' cross-namespace Event validation: Events in `kube-system` reference the cluster-scoped `Namespace` containing the problematic resource, while the specific failing namespaced resource is identified in `message`.
- [x] 2026-05-29: Reformulated `inferencesetWeightDownloadSlow` as a sliding-window check (default **60 s**, configurable via `controlPlane.weightDownload.windowSeconds`): the event fires only when every sample in the window is strictly below `controlPlane.weightDownload.minMBps`, a single in-threshold sample suppresses emission, and the reporter waits for the window to be fully populated before deciding. Eliminates spurious warnings from transient dips. Updated the §1.2 catalogue row, the §3 `charts/modeldeployment` schema-validation requirement, and split the Test Plan `weight_download_slow_test.go` row into sustained-slow / transient-dip / partial-window scenarios.
- [x] 2026-06-02: Required the cluster-scope hot-path filters (`llm-gateway-auth` ext_authz and BBR ext_proc) to run highly-available (≥ 2 replicas, node anti-affinity) and the Istio Gateway to automatically eject an unhealthy replica via active gRPC health checking + passive outlier detection on each filter's upstream cluster, so prompt requests are forwarded only to healthy replicas and the fail-closed `502 bbr_unavailable` / `502 ext_authz_unavailable` path is reached only when *all* replicas of a filter are unhealthy. Added §2.3 item 2, the corresponding §3 requirements on `charts/productionstack` and `llm-gateway-auth`, and the `cluster_filter_ha_test.go` E2E rows.
- [x] 2026-08-06: Implemented `productionstack-status-reporter` as an HA, leader-elected umbrella subchart. The reporter is installed in the umbrella release namespace by default and publishes Events to `kube-system`. It periodically evaluates seven cluster Deployment-readiness reasons, three modelharness Gateway reasons, and five modeldeployment reasons; reads KAITO `Workspace` conditions for infra, model-pod, and node-pressure health; checks deterministic EPP Deployment readiness; aggregates stable Events by subject and reason; and applies startup-grace/debounce rules. Cross-layer suppression and the previously proposed CRD, auth-policy/APIKey, EnvoyFilter, network-policy, and route checks are not part of the implemented catalogue.
- [ ] TBD: Upstream code `llm-gateway-auth` (envelope + 403 status mapping)
- [ ] TBD: Charts merged — `charts/productionstack`, `charts/modelharness`, `charts/modeldeployment`
- [ ] TBD: TSGs merged — TSG-1 (control-plane errors) and TSG-2 (data-plane errors), both internal-only
