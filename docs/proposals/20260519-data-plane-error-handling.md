---
title: Data-Plane Error Handling Across Cluster, Modelharness, and Modeldeployment Levels
authors:
  - "@rambohe-ch"
reviewers:
  - "@Fei-Guo"
  - "@zhuangqh"
  - "@tnsimon"
  - "@techworldhello"
creation-date: 2026-05-19
last-updated: 2026-11-02
status: implementable
see-also:
  - "https://github.com/kaito-project/production-stack/issues/71"
  - "/docs/proposals/20260519-control-plane-error-handling.md"
replaces: []
superseded-by: []
---

# Data-Plane Error Handling Across Cluster, Modelharness, and Modeldeployment Levels

## Table of Contents

- [Data-Plane Error Handling Across Cluster, Modelharness, and Modeldeployment Levels](#data-plane-error-handling-across-cluster-modelharness-and-modeldeployment-levels)
  - [Table of Contents](#table-of-contents)
  - [Glossary](#glossary)
  - [Summary](#summary)
  - [Motivation](#motivation)
    - [Goals](#goals)
    - [Non-Goals/Future Work](#non-goalsfuture-work)
  - [Proposal](#proposal)
    - [Implementation Details/Notes/Constraints](#implementation-detailsnotesconstraints)
      - [Error category overview](#error-category-overview)
      - [2. Data-plane errors](#2-data-plane-errors)
        - [2.1 Unified OpenAI-compatible error envelope](#21-unified-openai-compatible-error-envelope)
        - [2.2 Error catalogue](#22-error-catalogue)
        - [2.3 Notable behaviors](#23-notable-behaviors)
      - [3. Requirements](#3-requirements)
    - [User Stories](#user-stories)
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

Errors are organised into two top-level categories: **control-plane errors** (covered by the companion [control-plane error reporting proposal](./20260519-control-plane-error-handling.md)) and **data-plane errors** (this proposal). This proposal addresses data-plane errors at all three levels:

- **Data-plane errors** — failures observable on the HTTP request path against an installed stack. Standardised onto a single OpenAI-compatible JSON envelope, with a stable `code` and a new `x-kaito-error-source` header that pinpoints which hop (cluster filter, namespace gateway, modeldeployment EPP, upstream pod) produced the error.

## Motivation

Production-stack is built from independent OSS components (Istio Gateway, `llm-gateway-auth`, BBR, EPP, KAITO `InferenceSet`, KEDA, keda-kaito-scaler). Each component does its own error reporting in its own format and on its own resource, which means:

- **For end users.** The HTTP response a client receives for a broken stack is non-deterministic: `404` from the catch-all, `503` with no body from Envoy when EPP or BBR is unreachable, or `000` (connection reset) when ext_authz / ext_proc fails open. The response body shape changes by component.
- **For documentation.** Without stable `code` values on the response side, TSG-2 (data-plane errors, all three layers) — one of the two deliverables called out in #71 — cannot be deep-linked. (TSG-1, control-plane errors, is covered by the [control-plane proposal](./20260519-control-plane-error-handling.md).)

### Goals

- Define a data-plane-error taxonomy that covers cluster-level filters (ext_authz, BBR), modelharness-level routing (namespace Gateway, catch-all), and modeldeployment-level dispatch (EPP, upstream pod), all standardised onto one OpenAI-compatible JSON envelope.
- Eliminate the BBR/ext_authz-outage-looks-like-404 ambiguity.
- Distinguish `model_not_found` (route does not exist) from `model_unavailable` (route exists but `InferencePool` has zero ready endpoints — covers warming up, crash, OOM, eviction; root cause exposed via [control-plane `Event`s in `kube-system`](./20260519-control-plane-error-handling.md#1-control-plane-errors)).
- Publish TSG-2 (data-plane errors, covering cluster + modelharness + modeldeployment) — **internal-only**, keyed off the `code` strings defined here.

### Non-Goals/Future Work

- Centralised logging / alerting infrastructure; we reuse Kubernetes events, CR status, and Prometheus metrics, per #71 non-goals.
- Redesigning the request/response protocol beyond what's needed to carry actionable error information (per #71 non-goals).
- Rate-limit / quota errors (`429`). Whatever vLLM produces today is passed through unchanged.
- Errors that originate strictly outside the stack (e.g. cloud-provider AKS cluster-creation failure) are surfaced only as preconditions — production-stack does not own their root-cause remediation, only their detection and TSG cross-link.

## Proposal

### Implementation Details/Notes/Constraints

#### Error category overview

Every error in production-stack belongs to exactly one of two categories, and within each category is owned by exactly one of the three layers. This proposal covers the data-plane category; the control-plane category is covered by the [control-plane error reporting proposal](./20260519-control-plane-error-handling.md#error-category-overview).

| Category | Cluster level | Modelharness level | Modeldeployment level |
| --- | --- | --- | --- |
| **Data-plane errors** (request path → OpenAI-compatible HTTP responses) | - BBR ext_proc outage<br>- `llm-gateway-auth` ext_authz outage | - namespace gateway dataplane outage<br>- missing/invalid `APIKey` secret<br>- `CiliumNetworkPolicy` blackhole<br>- catch-all `model_not_found` | - EPP outage<br>- no ready model endpoints (warming up / crash / OOM / eviction)<br>- upstream pod timeout<br>- EPP internal error |

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
| `gateway`, `bbr`, `epp`, `inferenceset` | `charts/modelharness`, on each namespace `Gateway`: the consolidated `local_reply_config` (`headers_to_add`) and the `model-not-found-direct` catch-all `direct_response` routes. `inferenceset` on a proxied response is stamped by the modeldeployment `HTTPRoute` `ResponseHeaderModifier`. | `body-based-routing` and `llm-d-inference-scheduler` are consumed as unmodified upstream binaries. |
| `authz` (deny path: `401 invalid_api_key`, `403 api_key_disabled`) | `llm-gateway-auth` in-process — a same-org `kaito-project/*` sibling repo | Envoy `local_reply` cannot match on the per-deny gRPC code or body text needed to differentiate 401 vs 403. |
| `authz` (outage path: `502 ext_authz_unavailable`) | `charts/modelharness`, the per-namespace consolidated `local_reply` matching the `UAEX` response flag on a `5xx` | The in-process emitter is by definition unreachable when the authz Deployment is down. |

The one error response that carries neither the envelope nor `x-kaito-error-source` is BBR's native `400` for a body that is not valid JSON (see the exception under §2.2).

Request path (per `README.md`): `Client → Istio Gateway → ext_authz (llm-gateway-auth) → BBR → HTTPRoute → EPP → vLLM Pod`.

**Consolidated whole-path `local_reply` + component-first attribution.** All Envoy-generated `5xx` *local replies* on a namespace's request path — the cluster-filter hops (ext_authz / BBR), the namespace `Gateway` dataplane, AND the modeldeployment EPP / upstream-inference hops — are rewritten into the envelope by a **single** per-namespace `EnvoyFilter` (`gateway-filter-outage-local-reply`, rendered by `charts/modelharness`). There is no cluster-wide `local_reply`: every inference `Gateway` has its own HTTP connection manager, so the per-namespace filter already covers the cluster-filter hops, and the `MeshConfig`-based cluster-wide filter wiring was removed because the AKS Istio add-on does not allow modifying the shared `MeshConfig`. Consolidating into one mapper list removes the non-deterministic cross-`EnvoyFilter` merge order on the shared `Gateway` HCM that a split design would incur. Two orthogonal signals locate the at-fault component:

* **`X-Gateway-Model-Name` request header** — injected by BBR, which runs *before* route matching (EPP and the upstream run after). **Absent** ⇒ the failure is at/before BBR (infrastructure side: ext_authz, BBR, or the `Gateway` dataplane). **Present** ⇒ BBR ran and the request was routed to a model (model-serving side: EPP or the inference pods). The gateway strips any client-supplied value at ingress so it cannot be spoofed.
* **Envoy response flag** — a router/upstream flag (`UH`/`LH`/`UT`/`UPE`/`UF`/`UC`/`NC`) means the routed upstream or the dataplane failed; `UAEX` means ext_authz; **no** router flag on a local `5xx` means an ext_proc filter (BBR or EPP) failed closed.

Envoy's `local_reply` mapper filter is an `AccessLogFilter`, which has `and_filter` / `or_filter` / `response_flag_filter` / `header_filter` but **no** `not_filter`. "No router flag" is therefore expressed by **ordering**: every flag-bearing mapper precedes the two no-flag fallbacks, so a request that reaches the EPP/BBR fallbacks provably carries no router flag. The six mappers are evaluated top-to-bottom, first-match wins.

**Per-component codes are deliberately coarse** — one `code` per at-fault component (plus a stable `x-kaito-error-source`). The fine-grained root cause (zero-replica warm-up vs crash vs OOM vs eviction vs scheduler bug vs provisioning failure) is **not** carried on the response — it is published as a control-plane `Warning` `Event` in `kube-system` by the status reporter ([control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue)). Returning more detail to the prompt client adds no actionable value: every sub-cause of a given component failure demands the same client behaviour (back off, retry).

##### 2.2 Error catalogue

The table below lists every data-plane error `code`, the HTTP status it surfaces on, the at-fault component named by `x-kaito-error-source`, what triggers it, and the chart that owns rendering it. The layer names where the at-fault component lives: cluster-level components serve every namespace, so their outages affect every namespace, but every row below is rendered per namespace. Modeldeployment-level codes are per-InferenceSet.

| Layer | HTTP | `code` | `x-kaito-error-source` | Trigger | Owner |
| --- | --- | --- | --- | --- | --- |
| Cluster | 502 | `ext_authz_unavailable` | `authz` | The fail-closed ext_authz filter (`status_on_error: 503`) cannot reach `llm-gateway-auth`, or `llm-gateway-auth` returns an error: a local `5xx` with the `UAEX` response flag. Consolidated `local_reply` mapper #1. | `charts/modelharness` |
| Cluster | 502 | `bbr_unavailable` | `bbr` | The fail-closed BBR ext_proc filter (`status_on_error: 503`) is unreachable or errored before injecting `X-Gateway-Model-Name`: a local `5xx` with the header **absent** and **no** router flag. Consolidated `local_reply` mapper #5. | `charts/modelharness` |
| Modelharness | 401 | `invalid_api_key` | `authz` | `Authorization` missing, token does not match any `APIKey` Secret resolvable from the host subdomain, or token is syntactically malformed. Emitted in-process by `llm-gateway-auth`. | `llm-gateway-auth` (in-process) |
| Modelharness | 403 | `api_key_disabled` | `authz` | Valid `APIKey` resolved but not authorised for this gateway namespace, or the `APIKey` CR is explicitly marked disabled. Same in-process emitter as `invalid_api_key`, HTTP `403`. Requires the `llm-gateway-auth` deny-path change in §3 to actually surface `403` (today `apikey-authz` collapses every deny to `401`). | `llm-gateway-auth` (in-process) |
| Modelharness | 400 | `invalid_request_body` | `bbr` | The request body is valid JSON but has no usable `model` field. BBR does not reject it; it only skips injecting `X-Gateway-Model-Name`, so the request reaches the header-absent route of the `model-not-found-direct` catch-all, which answers with a `direct_response` (not a `local_reply` mapper). A body that is not valid JSON is the exception below. | `charts/modelharness` |
| Modelharness | 404 | `model_not_found` | `gateway` | `X-Gateway-Model-Name` is present but no `HTTPRoute` in this namespace matches | `charts/modelharness` |
| Modelharness | 502 | `gateway_unavailable` | `gateway` | `X-Gateway-Model-Name` **absent** (failure before model routing) AND a router/upstream flag fired: a route served by the namespace `Gateway` without model routing has no ready upstream (`UH`/`LH`) — e.g. the model-discovery route (`/v1/models`) while the `productionstack-status-reporter` has no ready replica — or the listener/cluster is not yet programmed while the harness converges (`UF`/`UC`/`NC`). Consolidated `local_reply` mapper #3. | `charts/modelharness` |
| Modelharness | 502 | `epp_unavailable` | `epp` | Local `5xx` with `X-Gateway-Model-Name` **present** and **no** router flag: the EPP (`InferencePool.endpointPickerRef`, `failureMode: FailClose`) ext_proc filter failed closed when unreachable. EPP ext_proc is auto-injected by GAIE so it takes the ext_proc default `500`; the consolidated `local_reply` (mapper #4) rewrites it. | `charts/modelharness` |
| Modelharness | 503 | `model_unavailable` | `inferenceset` | `X-Gateway-Model-Name` **present** (routed to a model) AND a router/upstream flag fired (`UH`/`LH` zero ready endpoints — warm-up / crash / OOM / eviction; `UT` upstream timeout, which fires only when an operator adds a route timeout — the modeldeployment `HTTPRoute` sets none and Istio applies no route timeout by default; `UPE` protocol error; `UF`/`UC`/`NC` connection failure). The `code` is deliberately root-cause-neutral because all sub-causes share the same client behaviour (back off on `Retry-After: 10` and retry). The operator-facing root cause is surfaced as a control-plane `Warning` `Event` in `kube-system` — either a pass-through KAITO `InferenceSetReady` reason (e.g. `QuotaExceeded`, `ContainerCrashLoopBackOff`) or `inferencesetEPPNotReady` ([control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue)). Consolidated `local_reply` mapper #2. | `charts/modelharness` |
| Modelharness | (preserved) | `gateway_internal_error` | `gateway` | Catch-all: any remaining local `5xx` not matched by a more specific mapper. `status_code` is preserved; only the body and `x-kaito-error-source` header are normalised. Consolidated `local_reply` mapper #6. | `charts/modelharness` |
| Modeldeployment | pass-through | (preserved) | `inferenceset` | Any non-error or vLLM-native error (e.g. `429` rate-limit) is proxied back unchanged; only `x-kaito-error-source: inferenceset` is stamped by the modeldeployment `HTTPRoute` `ResponseHeaderModifier`. It is **not** a local reply, so the consolidated `local_reply` never rewrites it. | `charts/modeldeployment` |

**Exception — a body that is not valid JSON.** BBR rejects a request body it cannot parse as JSON with its own `400`, an ext_proc immediate response whose plain-text body is BBR's parse error. The gateway passes it through unchanged: it carries neither the envelope nor `x-kaito-error-source`. It is deliberately not rewritten into `invalid_request_body`: the only `local_reply` match available — a local `400` without `X-Gateway-Model-Name` — would equally capture the missing-`model` `direct_response` above and Envoy's own protocol-level `400`s (e.g. invalid headers), misattributing the latter to `bbr`, and Envoy's `local_reply` mapper has no `not_filter` to exclude them. The case is rare in practice (OpenAI SDKs always send JSON), the status already tells the client it sent a bad request, and the passed-through text names the parse error.

##### 2.3 Notable behaviors

Three behaviours of the merged catalogue are worth calling out explicitly because they motivate concrete requirements in §3:

1. **Cluster-filter outages must not silently surface as `404`.** BBR ext_proc and `llm-gateway-auth` ext_authz both default to `failure_mode_allow: true`. Left at the default, a BBR outage would silently skip `X-Gateway-Model-Name` insertion and the request would fall through the namespace's catch-all `EnvoyFilter` as a misleading client error (the same trap exists for ext_authz failing open, which would additionally let unauthenticated traffic through). The catalogue closes this in two places: (a) `charts/modelharness` renders both filters fail-closed (`failure_mode_allow: false`, `status_on_error: 503`), and its per-namespace consolidated `local_reply` maps the resulting local `5xx` to `ext_authz_unavailable` (the `UAEX` flag) or `bbr_unavailable` (header absent, no router flag); (b) the modelharness catch-all `EnvoyFilter` distinguishes `X-Gateway-Model-Name` **absent** (→ `400 invalid_request_body`, a body without a usable `model`) from **present but no `HTTPRoute` matched** (→ `404 model_not_found`). A BBR outage never reaches the header-absent catch-all route: the fail-closed filter rejects the request first.
2. **Cluster-level filters run highly-available and a single bad replica must not break the request path.** `llm-gateway-auth` (ext_authz) and BBR (ext_proc) are **cluster-scope** components on the hot path of *every* request in the cluster, so a single replica is a single point of failure. Both run with **at least 2 replicas** by default, and only Ready replicas receive traffic:
   - **BBR** runs with `replicas: 2` by default (schema minimum 2) and always-rendered pod anti-affinity. Kubernetes readiness and liveness probes check BBR's gRPC health service on its dedicated health port. The readiness result drives the pod Ready condition, which drives the Istio EDS endpoints of the BBR ext_proc cluster, so an unhealthy replica is removed from the load-balancing set. On top of this active check, each namespace `Gateway` applies **passive outlier detection** to the BBR cluster (consecutive gateway failures / `5xx`), with `maxEjectionPercent` below 100 so outlier detection can never eject every replica at once. With ≥ 2 replicas behind both checks, the loss of one replica is transparent to clients, and `502 bbr_unavailable` is reached only when **all** replicas are unhealthy.
   - **`llm-gateway-auth`** relies on the replica defaults of its upstream charts (`apikey-authz` runs 3 replicas with a soft topology spread across nodes and zones; `azure-authz` runs 2) and on readiness only: no passive outlier detection is configured on the ext_authz cluster. A replica that starts failing requests can therefore return `502 ext_authz_unavailable` for the window before its readiness probe fails and it leaves the EDS endpoint set. This window is accepted (see Alternatives).

   The reporter surfaces a degraded (but serving) filter as a control-plane `Warning` — `clusterBBRNotReady` / `clusterGatewayAuthNotReady` ([control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue)) — whenever any of its pods is not Ready or Pending, so operators see partial degradation before it becomes a full outage.
3. **`model_unavailable` vs. `model_not_found` are deterministically separable on the request path.** `charts/modeldeployment` always renders an `HTTPRoute` for the model name regardless of whether the `InferencePool` currently has ready endpoints. Therefore: matched route + empty `InferencePool` → `503 model_unavailable` (root-cause-neutral; see Trigger column); no matching route → `404 model_not_found`. The operator-facing root cause for `model_unavailable` is intentionally **not** carried on the response — it is published as one of the modeldeployment-layer Warning events per [control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue), and TSG-2's `model_unavailable` entry directs the operator to inspect that event stream. Alternatives that would discriminate the root cause on the request path (EPP patches, control-plane-state-reading sidecars) are rejected — see Alternatives.

#### 3. Requirements

This section enumerates the requirements that any implementing PR MUST satisfy, grouped by the component that owns the change. Concrete code shape (file paths, struct definitions, template names, RBAC verb lists) is left to the component owners. Control-plane requirements (the `productionstack-status-reporter` and the chart labelling it relies on) are listed in the [control-plane proposal](./20260519-control-plane-error-handling.md#3-requirements).

| Component | Requirements |
| --- | --- |
| `charts/productionstack` (umbrella chart, incl. `charts/body-based-routing` sub-chart) | - **BBR high availability**: the `body-based-routing` sub-chart MUST render the BBR Deployment with at least 2 replicas (default `replicas: 2`, configurable but with a schema minimum of 2) and pod anti-affinity spreading replicas across nodes per §2.3 item 2.<br>- **Active health check**: the sub-chart MUST render Kubernetes readiness and liveness probes against BBR's gRPC health service on its health port, so only Ready replicas are in the EDS endpoint set of the BBR ext_proc cluster (§2.3 item 2). |
| `charts/modelharness` | - **Cluster-filter fail-closed**: MUST insert the BBR ext_proc and `llm-gateway-auth` ext_authz filters into the namespace `Gateway` with `failure_mode_allow: false` and `status_on_error: 503` per §2.3 item 1.<br>- **Consolidated whole-path `local_reply`**: MUST render a single per-namespace `EnvoyFilter` that rewrites every Envoy-generated `5xx` local reply on the request path into the §2.2 envelope, attributing the at-fault component from the `X-Gateway-Model-Name` header and the response flag per §2.2 (the six-mapper, ordering-based scheme that compensates for the absent `not_filter`). It MUST cover `ext_authz_unavailable`, `model_unavailable`, `gateway_unavailable`, `epp_unavailable`, `bbr_unavailable`, and the `gateway_internal_error` catch-all.<br>- **Catch-all**: the catch-all `EnvoyFilter` MUST distinguish `X-Gateway-Model-Name` **absent** → `400 invalid_request_body` and **present but unmatched** → `404 model_not_found` per §2.3 item 1.<br>- **Passive outlier detection on BBR**: MUST patch the BBR ext_proc upstream cluster with outlier detection whose `maxEjectionPercent` stays below 100, so a single erroring BBR replica is ejected without tripping the fail-closed `502 bbr_unavailable` path (§2.3 item 2). |
| `charts/modeldeployment` | - **EPP fail-closed + pass-through stamp**: MUST set `failureMode: FailClose` on the `InferencePool.endpointPickerRef` so an unreachable EPP surfaces a local `5xx` (which the consolidated `charts/modelharness` `local_reply` maps to `epp_unavailable`), and MUST stamp `x-kaito-error-source: inferenceset` on pass-through responses via the `HTTPRoute` `ResponseHeaderModifier`. The actual `5xx`→envelope mapping (including the root-cause-neutral `503 model_unavailable`, §2.3 item 3) is owned by the consolidated per-namespace filter in `charts/modelharness`, NOT by a per-modeldeployment `EnvoyFilter`. No upstream patches to `llm-d-inference-scheduler` MAY be required by this work. |
| `llm-gateway-auth` | - **Deny path**: both deny builders (`apikey`, `azure`) MUST emit the OpenAI envelope and `x-kaito-error-source: authz`. gRPC `PermissionDenied` MUST map to HTTP `403 api_key_disabled`; other denies remain `401 invalid_api_key`. Not yet implemented; tracked as a cross-repo dependency in Implementation History.<br>- **High availability**: as a cluster-scope filter on every request path, each ext_authz Deployment MUST run with at least 2 replicas by default and expose a readiness probe, so only Ready replicas receive traffic per §2.3 item 2. Today `apikey-authz` defaults to 3 replicas with a soft topology spread and `azure-authz` to 2. No passive outlier detection is required on the ext_authz cluster (see Alternatives). |

### User Stories

#### Story 4 — Client gets actionable HTTP error

A client calls `POST /v1/chat/completions` with `model: "qwen-typo"`. Production-stack returns:

```http
HTTP/1.1 404 Not Found
Content-Type: application/json
x-kaito-error-source: gateway

{ "error": { "type": "invalid_request_error", "code": "model_not_found",
             "message": "The model does not exist.", "param": "model" } }
```

If the operator has just scaled the deployment from zero, the same path returns instead:

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 10
x-kaito-error-source: inferenceset

{ "error": { "type": "service_unavailable", "code": "model_unavailable",
             "message": "The requested model is temporarily unavailable. Back off and retry; see Events in kube-system for the root cause.",
             "param": null } }
```

#### Story 5 — BBR outage no longer disguised as 404

When BBR's ext_proc pods are unavailable (cluster-level filter outage), the Istio Gateway returns a structured `502` with `code: bbr_unavailable` and `x-kaito-error-source: bbr` instead of falling through to the namespace catch-all. The client can tell from the response body that BBR is at fault; the operator sees the `clusterBBRNotReady` `Warning` `Event` in `kube-system` ([control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue)). The same disambiguation applies when `llm-gateway-auth` ext_authz is unavailable (`502 ext_authz_unavailable`, `x-kaito-error-source: authz`, with `clusterGatewayAuthNotReady`). In both cases the envelope is rendered by the per-namespace consolidated `local_reply` in `charts/modelharness`; neither component is patched.

## Alternatives

- **Discriminate `model_unavailable` root cause on the request path** (patch `llm-d-inference-scheduler` to emit a more specific `code`, or add a sidecar that reads control-plane state and rewrites the response). Rejected: production-stack consumes `llm-d-inference-scheduler` as an unmodified upstream binary, and a status-reading sidecar adds a failure domain on the hot path. Every "zero ready endpoints" sub-cause (warm-up / crash / OOM / eviction) demands the same client behaviour (back off on `Retry-After` and retry), so a discriminated `code` would not change client behaviour. Operator-facing root cause is preserved as a `Warning` `Event` in `kube-system` per [control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue).
- **Cluster-wide `local_reply` in `charts/productionstack`.** Rejected: the inference `Gateway`s each have their own HTTP connection manager, and the `MeshConfig.extensionProviders`-based cluster-wide filter wiring was removed because the AKS Istio add-on does not allow modifying the shared `MeshConfig`. A second filter patching the same `Gateway` HCM would also reintroduce the non-deterministic cross-`EnvoyFilter` merge order the consolidated per-namespace filter avoids. The per-namespace filter covers the cluster-filter hops on every `Gateway`, so nothing is lost.
- **Envoy active health checks (`health_checks`) on the BBR and ext_authz upstream clusters.** Rejected: those clusters are generated by Istio, and BBR serves its gRPC health service only on a dedicated health port, not on the ext_proc port Envoy dials. Kubernetes readiness probes against that health port achieve the same effect through the EDS endpoint set (§2.3 item 2), with no extra Envoy configuration.
- **Passive outlier detection on the ext_authz upstream cluster.** Not adopted for now: ext_authz relies on its replica count and readiness only. The accepted cost is that a replica that starts failing requests can return `502 ext_authz_unavailable` until its readiness probe fails. Revisit if this window is observed in production.
- **Wrap BBR's native `400` for a non-JSON body into the envelope** with a `local_reply` mapper (local `400` + `X-Gateway-Model-Name` absent, embedding BBR's text as the `message`). Rejected for the reasons given under the §2.2 exception: the match would also capture the missing-`model` `direct_response` and Envoy's protocol-level `400`s, misattributing the latter to `bbr`, for a case OpenAI SDK clients do not produce.

## Test Plan

**E2E tests** (under `test/e2e/`)

Rows that assert a concurrent control-plane `Warning` use `kubectl get events -n kube-system --field-selector source=productionstack-status-reporter,reason=<reason>` (or the Go equivalent in the test harness), per the [control-plane Test Plan](./20260519-control-plane-error-handling.md#test-plan). The **Status** column marks scenarios that are not covered yet.

| Layer | Test file | Scenario | Asserted outcome | Status |
| --- | --- | --- | --- | --- |
| Request path | `apikey_auth_spec.go` | normal `401` deny (missing or unknown `Authorization`) | `401 invalid_api_key` envelope + `x-kaito-error-source: authz` | Partial: only the `401` status is asserted; the envelope waits on the `llm-gateway-auth` deny-path change (§3) |
| Request path | `apikey_auth_spec.go` | valid token whose `APIKey` CR is explicitly disabled | `403 api_key_disabled` envelope + `x-kaito-error-source: authz` (verifies the §3 `llm-gateway-auth` deny-path 403 mapping) | Pending: waits on the `llm-gateway-auth` deny-path change (§3) |
| Request path | `model_routing_spec.go` | unknown model name | `404 model_not_found` envelope + `x-kaito-error-source: gateway` | Covered |
| Request path | `model_routing_spec.go` | POST a JSON body without a `model` field | `400 invalid_request_body` envelope + `x-kaito-error-source: bbr` | Covered |
| Request path | `model_routing_spec.go` | POST a body that is not valid JSON | BBR's native `400`, passed through unchanged (no envelope, no `x-kaito-error-source`; §2.2 exception). Only the status is asserted; the raw body is recorded in the test report. | Covered |
| Request path | `bbr_outage_spec.go` | scale BBR Deployment to zero | `502 bbr_unavailable` envelope + `x-kaito-error-source: bbr` (not `404`; verifies fail-closed BBR + the consolidated `local_reply` mapper #5) | Covered |
| Request path | `ext_authz_outage_spec.go` | scale `apikey-authz` Deployment to zero | `502 ext_authz_unavailable` envelope + `x-kaito-error-source: authz` (verifies fail-closed ext_authz + the consolidated `local_reply` mapper #1) | Covered |
| Request path | `cluster_filter_ha_spec.go`, BBR single-replica loss | with BBR at 2 replicas, scale to 1 and send a burst of prompt requests | **zero** `404` and **zero** `502 bbr_unavailable` responses; then scale to zero and assert the request path fails closed; a `Warning` `clusterBBRNotReady` is concurrently present in `kube-system` for the degraded Deployment (verifies §2.3 item 2) | Partial: the concurrent `clusterBBRNotReady` `Warning` is not asserted |
| Request path | `gateway_unavailable_spec.go` | scale the `productionstack-status-reporter` to zero and `GET /v1/models` (scaling the namespace `Gateway` itself to zero leaves no Envoy to answer) | `502 gateway_unavailable` envelope + `x-kaito-error-source: gateway` (header absent + `UH` → consolidated `local_reply` mapper #3) | Covered |
| Request path | `epp_outage_spec.go` | scale the EPP Deployment to zero | `502 epp_unavailable` envelope + `x-kaito-error-source: epp` (header present + no router flag → mapper #4) | Covered |
| Request path | `model_unavailable_spec.go`, warm-up | `replicas=0`, send a request | `503 model_unavailable` with `Retry-After` + `x-kaito-error-source: inferenceset` (warm-up is an in-progress state, so no control-plane `Warning` is required) | Covered |
| Request path | `model_unavailable_spec.go`, crash | wait for Ready, then inject a crash-loop (`exit 1`) | same `503 model_unavailable` response shape — proves request-path code is root-cause-agnostic — while a `Warning` carrying the pass-through KAITO `InferenceSetReady` reason (e.g. `ContainerCrashLoopBackOff`) is emitted on the workload `Namespace` (per [control-plane §1.1](./20260519-control-plane-error-handling.md#11-event-schema); the owning `InferenceSet` is identified in `message`) | Pending |

There is no upstream-timeout scenario: the model route sets no timeout (§2.2), so `UT` cannot be triggered without adding one.

**Manual verification.** Each TSG-2 data-plane `code` is reachable via internal tooling from its corresponding response-body `code`. TSG-2 is internal-only: data-plane response bodies MUST NOT carry TSG URLs.

## Implementation History

- [x] 2026-05-19: Proposed in [issue #71](https://github.com/kaito-project/production-stack/issues/71); initial proposal PR opened (modeldeployment-only scope)
- [x] 2026-05-21: Expanded scope to cluster + modelharness + modeldeployment; restructured under two top-level categories (control-plane / data-plane)
- [x] 2026-05-28: Consolidated §2: merged the per-layer §2.2 / §2.3 / §2.4 catalogues into one §2.2 table and demoted the standalone §2.5 / §2.6 sections into a shorter §2.3 "Notable behaviors" callout. Rewrote §3 as a per-component "Requirements" table (reporter, `charts/productionstack`, `charts/modelharness`, `charts/modeldeployment`, `llm-gateway-auth`).
- [x] 2026-05-28: Expanded the data-plane Test Plan and the initial control-plane test design.
- [x] 2026-06-02: Required the cluster-scope hot-path filters (`llm-gateway-auth` ext_authz and BBR ext_proc) to run highly-available (≥ 2 replicas, node anti-affinity) and the Istio Gateway to automatically eject an unhealthy replica via active gRPC health checking + passive outlier detection on each filter's upstream cluster, so prompt requests are forwarded only to healthy replicas and the fail-closed `502 bbr_unavailable` / `502 ext_authz_unavailable` path is reached only when *all* replicas of a filter are unhealthy. Added §2.3 item 2, the corresponding §3 requirements on `charts/productionstack` and `llm-gateway-auth`, and the `cluster_filter_ha_test.go` E2E rows.
- [x] 2026-10-21: Split from the combined `20260519-end-to-end-error-handling.md` proposal into this data-plane proposal and the companion [control-plane error reporting proposal](./20260519-control-plane-error-handling.md).
- [ ] TBD: Upstream code `llm-gateway-auth` (envelope + 403 status mapping)
- [x] Charts merged — `charts/productionstack`, `charts/modelharness`, `charts/modeldeployment`
- [x] 2026-11-02: Realigned the proposal with the implementation; status moved to `implementable`:
  - **Consolidated per-namespace `local_reply`.** The cluster-filter outage codes (`ext_authz_unavailable`, `bbr_unavailable`) are rendered by the per-namespace filter in `charts/modelharness`; the cluster-wide `local_reply` in `charts/productionstack` and the duplicate per-namespace `bbr_unavailable` row are removed. Fail-closed configuration of both cluster filters moved to the `charts/modelharness` requirements.
  - **Catalogue.** Removed `mesh_config_invalid`: the `MeshConfig.extensionProviders` wiring it guarded no longer exists. Narrowed `invalid_request_body` to a JSON body without a `model` field (a `direct_response`) and documented the non-JSON body exception (BBR's native `400`, passed through). The catch-all's header-absent route answers `400 invalid_request_body`, not `502 bbr_unavailable`. Documented that `UT` needs an operator-added route timeout.
  - **HA.** The active health check is the Kubernetes readiness probe driving the EDS endpoint set rather than Envoy `health_checks`. BBR adds passive outlier detection; ext_authz relies on replicas and readiness only, accepting a short `502 ext_authz_unavailable` window. The degraded-filter `Warning` fires when any pod is not Ready or Pending.
  - **Test Plan.** Renamed to the real `*_spec.go` files, added a coverage status column, replaced the `Gateway`-pod scale-down with the status-reporter scale-down for `gateway_unavailable`, and dropped the ext_authz single-replica and upstream-timeout rows.
- [ ] TBD: TSG-2 (data-plane errors) merged, internal-only
