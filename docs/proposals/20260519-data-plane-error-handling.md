---
title: Data-Plane Error Handling
authors:
  - "@rambohe-ch"
reviewers:
  - "@Fei-Guo"
  - "@zhuangqh"
  - "@tnsimon"
  - "@techworldhello"
creation-date: 2026-05-19
last-updated: 2026-11-19
status: implementable
see-also:
  - "https://github.com/kaito-project/production-stack/issues/71"
  - "/docs/proposals/20260519-control-plane-error-handling.md"
replaces: []
superseded-by: []
---

# Data-Plane Error Handling

## Table of Contents

- [Data-Plane Error Handling](#data-plane-error-handling)
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

- **modelharness**: The Helm release rendered by [`charts/modelharness`](../../charts/modelharness) — one per workload namespace. Provisions the namespace `Gateway`, the per-namespace dataplane `EnvoyFilter`s (`bbr-ext-proc`, `model-not-found-direct`, `gateway-filter-outage-local-reply`, `gateway-cors` while CORS is enabled, and, when auth is enabled, `apikey-ext-authz`, or on Azure `ext-authz` with the `entra-jwt` `RequestAuthentication` for the `azure` and `hybrid` modes), the model-discovery `HTTPRoute` (`models-api`, which serves `/v1/models` from the `productionstack-status-reporter`), the `APIKey` CR, and the `CiliumNetworkPolicy` resources (requires the cluster Cilium dataplane; see `charts/modelharness` for rationale).
- **modeldeployment**: The Helm release rendered by [`charts/modeldeployment`](../../charts/modeldeployment) — one `InferenceSet`, one `InferencePool`, one EPP `Deployment`/`Service`/RBAC/`ConfigMap`, and one `HTTPRoute`, parented to the per-namespace `Gateway`.
- **EPP**: Endpoint Picker — per-model ext_proc pod that performs KV-cache aware routing: the `llm-d-router` endpoint picker (formerly `llm-d-inference-scheduler`), which implements the Gateway API Inference Extension's Endpoint Picker Protocol.
- **BBR**: Body-Based Router — the cluster-wide ext_proc filter (in the umbrella release namespace, `kaito-system` by default) that parses request bodies and injects the `X-Gateway-Model-Name` header.
- **InferenceSet**: `kaito.sh/v1beta1` CR owned by the KAITO controller; the canonical declaration of one model deployment.

## Summary

Production-stack today has no coherent error story on the request path. Every hop between the client and the model — the namespace `Gateway`, the ext_authz filter (`llm-gateway-auth`), the BBR ext_proc filter, the EPP, and the vLLM pods — fails in its own way, so distinct failures (ext_authz outage, BBR outage, unprogrammed namespace Gateway, EPP outage, model still warming up, model name truly unknown) all collapse onto indistinguishable `404`s or unbodied `503`s, and response bodies do not follow a stable schema.

Errors are organised into two top-level categories: **control-plane errors** (covered by the companion [control-plane error reporting proposal](./20260519-control-plane-error-handling.md)) and **data-plane errors** (this proposal):

- **Data-plane errors** — failures observable on the HTTP request path against an installed stack. Every `5xx` the gateway generates, and every error production-stack's own components return, is standardised onto a single OpenAI-compatible JSON envelope, with a stable `code` and a new `x-kaito-error-source` header that pinpoints which hop (ext_authz, BBR, namespace gateway, EPP, inference pods) is at fault. A `4xx` from an upstream component passes through unchanged.

## Motivation

Production-stack is built from independent OSS components (Istio Gateway, `llm-gateway-auth`, BBR, EPP, KAITO `InferenceSet`, KEDA, keda-kaito-scaler). Each component does its own error reporting in its own format and on its own resource, which means:

- **For end users.** The HTTP response a client receives for a broken stack is non-deterministic: `404` from the catch-all, `503` with no body from Envoy when EPP or BBR is unreachable, or `000` (connection reset) when ext_authz / ext_proc fails open. The response body shape changes by component.
- **For documentation.** Without stable `code` values on the response side, TSG-2 (data-plane errors) — one of the two deliverables called out in #71 — cannot be deep-linked. (TSG-1, control-plane errors, is covered by the [control-plane proposal](./20260519-control-plane-error-handling.md).)

### Goals

- Define a data-plane-error taxonomy that covers every hop on the request path — ext_authz, BBR, namespace Gateway routing (including the catch-all), EPP, and the inference pods. Every `5xx` the gateway generates, and every error production-stack's own components return, uses one OpenAI-compatible JSON envelope; a `4xx` from an upstream component passes through unchanged (§2.1).
- Eliminate the BBR/ext_authz-outage-looks-like-404 ambiguity.
- Distinguish `model_not_found` (route does not exist) from `model_unavailable` (route exists but `InferencePool` has zero ready endpoints — covers warming up, crash, OOM, eviction; root cause exposed via [control-plane `Event`s in `kube-system`](./20260519-control-plane-error-handling.md#1-control-plane-errors)).
- Publish TSG-2 (data-plane errors) — **internal-only**, keyed off the `code` strings defined here.

### Non-Goals/Future Work

- Centralised logging / alerting infrastructure; we reuse Kubernetes events, CR status, and Prometheus metrics, per #71 non-goals.
- Redesigning the request/response protocol beyond what's needed to carry actionable error information (per #71 non-goals).
- Rate-limit / quota errors (`429`). Whatever the EPP (when it sheds a request) or vLLM produces today is passed through unchanged.
- Errors that originate strictly outside the stack (e.g. cloud-provider AKS cluster-creation failure) are surfaced only as preconditions — production-stack does not own their root-cause remediation, only their detection and TSG cross-link.

## Proposal

### Implementation Details/Notes/Constraints

#### Error category overview

Every error in production-stack belongs to exactly one of two categories. This proposal covers the data-plane category; the control-plane category, which is organised by the three deployment layers (cluster, modelharness, modeldeployment), is covered by the [control-plane error reporting proposal](./20260519-control-plane-error-handling.md#error-category-overview).

Data-plane errors are not organised by those layers: every data-plane error is observed on a namespace `Gateway`'s request path, regardless of which layer deployed the failing component. They are classified instead by the request-path hop at fault, which `x-kaito-error-source` names on every error that carries the envelope (§2.1):

| Request-path hop (`x-kaito-error-source`) | Data-plane errors (request path → OpenAI-compatible HTTP responses) |
| --- | --- |
| ext_authz (`authz`) | - `llm-gateway-auth` outage<br>- missing, invalid, or disabled credential (API key, or Entra token on Azure) |
| BBR (`bbr`) | - BBR ext_proc outage<br>- request body without a usable `model` |
| Namespace `Gateway` (`gateway`) | - gateway dataplane not ready or not yet programmed<br>- unknown model name (`model_not_found`)<br>- browser request rejected by the CORS policy<br>- model discovery (`/v1/models`) errors<br>- any other unclassified local `5xx` |
| EPP (`epp`) | - EPP outage or failure<br>- request to an endpoint the EPP does not parse, or a body without the field its endpoint requires (passed through unchanged, without `x-kaito-error-source`) |
| Inference pods (`inferenceset`) | - no ready model endpoints (warming up / crash / OOM / eviction)<br>- upstream timeout, protocol, or connection failure<br>- vLLM-native errors (passed through unchanged, without `x-kaito-error-source`) |

#### 2. Data-plane errors

Data-plane errors are everything an HTTP client can observe. The `5xx` the gateway generates and the errors production-stack's own components return share one OpenAI-compatible envelope, whichever hop is at fault; a `4xx` from an upstream component passes through unchanged (§2.1).

##### 2.1 Unified OpenAI-compatible error envelope

```json
{
  "error": {
    "type":    "invalid_request_error" | "authentication_error" | "service_unavailable",
    "code":    "<stable string from §2.2>",
    "message": "<human-readable>",
    "param":   "<json-path or null>"
  }
}
```

Server-side errors, `gateway_internal_error` included, use `service_unavailable`; `authentication_error` is reserved for the `llm-gateway-auth` deny path (§3).

**Which responses carry the envelope.** The gateway rewrites only the `5xx` local replies Envoy generates on the request path (the consolidated `local_reply` below), never a `4xx`. A `5xx` means the platform failed: the client needs to know whether and when to retry, and the operator which component to look at, and Envoy's own `5xx` (a short text or an empty body) tells neither. A `4xx` asks the client to fix its request: retrying does not help, the component that rejected the request states the reason best in its own words, and OpenAI SDKs raise the matching error type from the status alone (e.g. `BadRequestError` for `400`). Drawing the line by status keeps it the same for every component, including those whose `4xx` a `local_reply` mapper could not single out (see Alternatives).

A `4xx` therefore carries the envelope only when the component that returns it emits the envelope itself: the `Gateway`'s catch-all routes and CORS filter (rendered by `charts/modelharness`), the `productionstack-status-reporter`, and `llm-gateway-auth`'s deny path once it does (§3; until then its `401`/`403` pass through unchanged). A `4xx` from an upstream component, one that production-stack consumes unmodified, passes through unchanged, without the envelope or `x-kaito-error-source`: BBR's `400` for a body that is not valid JSON, the EPP's `400` for a request it cannot parse, Istio `jwt_authn`'s rejection of an Entra token that fails validation (all three described under §2.2), the EPP's `429` when it sheds a request (rate limiting is a Non-Goal), and Envoy's own protocol-level `4xx` (e.g. a `400` for an invalid header). Responses proxied back from the inference pods, successes and vLLM-native errors alike, are not local replies, so they also pass through unchanged, without `x-kaito-error-source` (§2.2 pass-through row).

Every response that carries the envelope also carries `x-kaito-error-source: gateway | authz | bbr | epp | inferenceset`; the value names the **at-fault component** (the thing the operator should look at first). Emission per source:

| Source value | Emitted by | Why |
| --- | --- | --- |
| `gateway`, `bbr`, `epp`, `inferenceset` | `charts/modelharness`, on each namespace `Gateway`: the consolidated `local_reply_config` (`headers_to_add`), the `model-not-found-direct` catch-all `direct_response` routes, and the `gateway-cors` Lua filter's `403`. | `body-based-routing` and `llm-d-router` are consumed as unmodified upstream binaries. |
| `gateway` (model discovery: `/v1/models`, `/v1/models/{id}`) | `productionstack-status-reporter` in-process, behind the harness's `models-api` `HTTPRoute` | The reporter serves these endpoints itself. When it has no ready replica or does not answer in time, the consolidated `local_reply` answers `502 gateway_unavailable` instead. |
| `authz` (deny path: `401 invalid_api_key`, `403 api_key_disabled`) | `llm-gateway-auth` in-process — a same-org `kaito-project/*` sibling repo | Envoy `local_reply` cannot match on the per-deny gRPC code or body text needed to differentiate 401 vs 403. |
| `authz` (outage path: `502 ext_authz_unavailable`) | `charts/modelharness`, the per-namespace consolidated `local_reply` matching the `UAEX` response flag on a `5xx` | The in-process emitter is by definition unreachable when the authz Deployment is down. |

Request path: `Client → Istio Gateway → CORS (gateway-cors) → ext_authz (llm-gateway-auth) → strip routing headers (kaito.strip-routing-headers) → BBR → HTTPRoute → EPP → vLLM Pod`. `filter_order_spec.go` pins this filter order on the `Gateway`.

**Consolidated whole-path `local_reply` + component-first attribution.** All Envoy-generated `5xx` *local replies* on a namespace's request path — the cluster-filter hops (ext_authz / BBR), the namespace `Gateway` dataplane, AND the modeldeployment EPP / upstream-inference hops — are rewritten into the envelope by a **single** per-namespace `EnvoyFilter` (`gateway-filter-outage-local-reply`, rendered by `charts/modelharness`). There is no cluster-wide `local_reply`: every inference `Gateway` has its own HTTP connection manager, so the per-namespace filter already covers the cluster-filter hops, and the `MeshConfig`-based cluster-wide filter wiring was removed because the AKS Istio add-on does not allow modifying the shared `MeshConfig`. Consolidating into one mapper list removes the non-deterministic cross-`EnvoyFilter` merge order on the shared `Gateway` HCM that a split design would incur. Three orthogonal signals locate the at-fault component:

* **`X-Gateway-Model-Name` request header** — injected by BBR, which runs *before* route matching (EPP and the upstream run after). **Absent** ⇒ the failure is at/before BBR (infrastructure side: ext_authz, BBR, or the `Gateway` dataplane). **Present** ⇒ BBR ran and the request was routed to a model (model-serving side: EPP or the inference pods). BBR sets it, and `X-Gateway-Base-Model-Name`, only when the body names a `model`, so a value the client sets itself would survive on a request without one. `charts/modelharness` therefore removes any client-supplied copy of both headers in a Lua filter (`kaito.strip-routing-headers`) directly before BBR, so only BBR sets them. Removing a header also drops the route Envoy picked from it before the filter chain ran, so the request is routed as if the client had never sent it. Without the strip, a request without a `model` would be routed to the model the client named (the EPP forwards a bodyless request, such as `GET /metrics`, to one of the model's pods), and a BBR outage would surface as `503 model_unavailable`.
* **Envoy response flag** — a router/upstream flag (`UH`/`LH`/`UT`/`UPE`/`UF`/`UC`/`NC`) means the routed upstream or the dataplane failed; `UAEX` means ext_authz. An ext_proc filter (BBR or EPP) never sets a response flag.
* **Status code** — separates the local replies that carry no response flag. Each value relied on is a documented contract or pinned by `charts/modelharness`, not an Envoy implementation detail. An EPP that is up but has no ready endpoint MUST reject the request with `503` ([Endpoint Picker Protocol](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/v1.5.0/docs/proposals/004-endpoint-picker-protocol/README.md); a request it sheds gets `429`, which is not a `5xx` and passes through unchanged). When the EPP ext_proc call itself **fails** (unreachable, gRPC error, a header mutation Envoy rejects, an out-of-order message), Envoy answers with the filter's `status_on_error`; Istio generates the EPP filter from the `InferencePool` without setting it, so it is Envoy's documented default `500`. The BBR filter's `status_on_error` is pinned to `503`, and BBR itself only rejects with a `400`. With the model header **present**, `503` therefore means "the EPP cannot place the request" (`model_unavailable`) and any other `5xx` means "the EPP failed" (`epp_unavailable`); with it **absent**, `503` means "BBR failed" (`bbr_unavailable`).

Envoy's `local_reply` mapper filter is an `AccessLogFilter`, which has `and_filter` / `or_filter` / `response_flag_filter` / `header_filter` / `status_code_filter` but **no** `not_filter`. "No router flag" and "any other `5xx`" are therefore expressed by **ordering**: every flag-bearing mapper precedes the no-flag mappers, so a `5xx` that reaches mappers #4a / #4b / #5 provably carries no router flag, and #4a (`503`) precedes #4b. Every mapper requires a `5xx`, the flag-bearing #2 and #3 included: Envoy sends their flags with a `5xx` by default, but `NC`'s status comes from the route's `cluster_not_found_response_code`, and `UT` answers `204` when the request carries `x-envoy-upstream-rq-timeout-alt-response`. Only these core filter types are used, no `extension_filter`, so the patch does not depend on an optional Envoy extension being built into the proxy. The seven mappers are evaluated top-to-bottom, first-match wins:

| # | Match | Result |
| --- | --- | --- |
| 1 | `UAEX` + status ≥ 500 | `502 ext_authz_unavailable` (`authz`) |
| 2 | model header present + router flag + status ≥ 500 | `503 model_unavailable` (`inferenceset`) |
| 3 | model header absent + router flag + status ≥ 500 | `502 gateway_unavailable` (`gateway`) |
| 4a | model header present + status `503` | `503 model_unavailable` (`inferenceset`) |
| 4b | model header present + any other status ≥ 500 | `502 epp_unavailable` (`epp`) |
| 5 | model header absent + status `503` | `502 bbr_unavailable` (`bbr`) |
| 6 | status ≥ 500 | `gateway_internal_error` (`gateway`, status preserved) |

**Per-component codes are deliberately coarse** — one `code` per at-fault component (plus a stable `x-kaito-error-source`). The fine-grained root cause (zero-replica warm-up vs crash vs OOM vs eviction vs scheduler bug vs provisioning failure) is **not** carried on the response — it is published as a control-plane `Warning` `Event` in `kube-system` by the status reporter ([control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue)). Returning more detail to the prompt client adds no actionable value: every sub-cause of a given component failure demands the same client behaviour (back off, retry).

##### 2.2 Error catalogue

The table below lists every data-plane error `code` on the inference path, the HTTP status it surfaces on, the at-fault component named by `x-kaito-error-source`, what triggers it, and the chart that owns rendering it; the model-discovery endpoints' codes follow it. Every row is observed on a namespace `Gateway`'s request path; an outage of a cluster-scope component (ext_authz, BBR) therefore surfaces identically on every namespace.

| HTTP | `code` | `x-kaito-error-source` | Trigger | Owner |
| --- | --- | --- | --- | --- |
| 502 | `ext_authz_unavailable` | `authz` | The fail-closed ext_authz filter (`status_on_error: 503`) cannot reach `llm-gateway-auth`, or `llm-gateway-auth` returns an error: a local `5xx` with the `UAEX` response flag. Consolidated `local_reply` mapper #1. | `charts/modelharness` |
| 502 | `bbr_unavailable` | `bbr` | The fail-closed BBR ext_proc filter (`status_on_error: 503`) is unreachable or errored before injecting `X-Gateway-Model-Name`: a local `503` with the header **absent** and no response flag. BBR itself only ever rejects with a `400` (body is not JSON, passed through as described below), so no BBR outage is lost by matching exactly its pinned `503`; any other unflagged header-absent `5xx` falls through to `gateway_internal_error`. Consolidated `local_reply` mapper #5. | `charts/modelharness` |
| 401 | `invalid_api_key` | `authz` | `Authorization` missing, token does not match any `APIKey` Secret resolvable from the host subdomain, or token is syntactically malformed. Emitted in-process by `llm-gateway-auth`. | `llm-gateway-auth` (in-process) |
| 403 | `api_key_disabled` | `authz` | Valid `APIKey` resolved but not authorised for this gateway namespace, or the `APIKey` CR is explicitly marked disabled. Same in-process emitter as `invalid_api_key`, HTTP `403`. Requires the `llm-gateway-auth` deny-path change in §3 to actually surface `403` (today `apikey-authz` collapses every deny to `401`). | `llm-gateway-auth` (in-process) |
| 400 | `invalid_request_body` | `bbr` | The request body is valid JSON but has no usable `model` field. BBR does not reject it; it only skips injecting `X-Gateway-Model-Name`, so the request reaches the header-absent route of the `model-not-found-direct` catch-all, which answers with a `direct_response` (not a `local_reply` mapper). A body that is not valid JSON gets BBR's own `400` instead, passed through as described below. | `charts/modelharness` |
| 404 | `model_not_found` | `gateway` | `X-Gateway-Model-Name` is present but no `HTTPRoute` in this namespace matches. The response also echoes the requested name in `x-kaito-requested-model`. The model-discovery endpoint answers an unknown model id with the same body (below). | `charts/modelharness` |
| 403 | `cors_forbidden` | `gateway` | A browser request (it carries `Origin`) that the CORS policy rejects: the origin is not in `cors.allowedOrigins` (the default `*` allows every origin), the method is not in `cors.allowedMethods`, or a preflight asks for a method or header that is not allowed. The `gateway-cors` Lua filter answers locally, before ext_authz; it is rendered while `cors.enabled` (the default). A request without `Origin`, or an `OPTIONS` without `Access-Control-Request-Method`, is not checked. | `charts/modelharness` |
| 502 | `gateway_unavailable` | `gateway` | `X-Gateway-Model-Name` **absent** (failure before model routing) AND a router/upstream flag fired on a local `5xx`: a route served by the namespace `Gateway` without model routing fails upstream — e.g. the model-discovery `HTTPRoute` (`/v1/models`) while the `productionstack-status-reporter` has no ready replica (`UH`/`LH`) or does not answer within `modelsAPI.timeout` (`UT`) — or the listener/cluster is not yet programmed while the harness converges (`UF`/`UC`/`NC`). Consolidated `local_reply` mapper #3. | `charts/modelharness` |
| 502 | `epp_unavailable` | `epp` | Local `5xx` other than `503` with `X-Gateway-Model-Name` **present** and no response flag: the EPP (`InferencePool.endpointPickerRef`, `failureMode: FailClose`) ext_proc call failed — the EPP is unreachable, returned a gRPC error, or sent a header mutation Envoy rejects or an out-of-order message — and the filter failed closed with its `status_on_error`. Istio generates the EPP ext_proc filter from the `InferencePool` without setting it, so it is Envoy's default `500`. An internal error the EPP reports itself (`500`) lands here too. Consolidated `local_reply` mapper #4b. The `503` the EPP returns when it cannot place a request is `model_unavailable`. | `charts/modelharness` |
| 503 | `model_unavailable` | `inferenceset` | `X-Gateway-Model-Name` **present** (routed to a model) AND either (a) the EPP is up but rejected the request with the Endpoint Picker Protocol's `503` `ImmediateResponse` because it cannot place it — no ready endpoint (warm-up / crash / OOM / eviction / scaled to zero; the steady state of an empty pool) or an expired scale-from-zero queue wait (mapper #4a). Or (b) a router/upstream flag fired on a local `5xx` (`UH`/`LH` zero ready endpoints, e.g. while the EPP's and the gateway's view of the pool disagree; `UT` upstream timeout, which fires only when an operator adds a route timeout — the modeldeployment `HTTPRoute` sets none and Istio applies no route timeout by default; `UPE` protocol error; `UF`/`UC`/`NC` connection failure). The `code` is deliberately root-cause-neutral because all sub-causes share the same client behaviour (back off on `Retry-After: 10` and retry). The operator-facing root cause is surfaced as a control-plane `Warning` `Event` in `kube-system` — either a pass-through KAITO `InferenceSetReady` reason (e.g. `QuotaExceeded`, `ContainerCrashLoopBackOff`) or `inferencesetEPPNotReady` ([control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue)). Consolidated `local_reply` mappers #4a and #2. | `charts/modelharness` |
| (preserved) | `gateway_internal_error` | `gateway` | Catch-all: any remaining local `5xx` not matched by a more specific mapper. `status_code` is preserved; only the body and `x-kaito-error-source` header are normalised. Consolidated `local_reply` mapper #6. | `charts/modelharness` |
| pass-through | (preserved) | (none) | Any non-error or vLLM-native error (e.g. `429` rate-limit) is proxied back unchanged, without `x-kaito-error-source`: the header marks only errors that carry the envelope (§2.1), and the modeldeployment `HTTPRoute` cannot stamp it on errors alone (a `ResponseHeaderModifier` applies to every response, successes included). It is **not** a local reply, so the consolidated `local_reply` never rewrites it. | `charts/modeldeployment` |

**Passed through — a body that is not valid JSON.** BBR rejects a request body it cannot parse as JSON with its own `400`, an ext_proc immediate response whose plain-text body is BBR's parse error. BBR does not check `Content-Type`, so the `multipart/form-data` uploads of `/v1/audio/transcriptions`, `/v1/audio/translations` and `/v1/images/edits` get it too. Like every `4xx` from an upstream component (§2.1), it passes through unchanged, without the envelope or `x-kaito-error-source`; the passed-through text names the parse error.

**Passed through — a request the EPP cannot parse.** The EPP (`llm-d-router` `v1.0.0`) parses the body of every request routed to a model. The modeldeployment chart configures three parsers: the OpenAI parser (paths ending in `/chat/completions`, `/completions` and their `/render` sub-paths, `/embeddings`, `/responses`, `/conversations`, `/images/generations`, `/images/edits`, `/audio/speech`), the Anthropic parser (`/messages`, `/messages/count_tokens`) and vLLM's `/inference/v1/generate` parser. These are the EPP's defaults without the passthrough parser it appends to them, which would forward a request on any other path to a model server pod, exposing model-server endpoints such as `/metrics` or `/v1/load_lora_adapter` through the gateway. The EPP rejects two kinds of request with its own `400`, an ext_proc immediate response whose JSON body is an OpenAI error object with a numeric `code`:

- a request to any other path, e.g. `/v1/moderations`, `/v1/rerank`, `/tokenize` or `/metrics` (`{"error":{"message":"no parser registered matching path suffix for: /tokenize","type":"invalid_request_error","code":400}}`);
- a body without the field its endpoint requires, e.g. at least one entry in `messages` for chat completions, `prompt` for completions, `input` for embeddings and responses, `items` for conversations (`{"error":{"message":"error extracting request body: invalid chat completions request: must have valid messages field","type":"invalid_request_error","code":400}}`).

Like every `4xx` from an upstream component (§2.1), the `400` passes through unchanged, without the envelope or `x-kaito-error-source`.

**Passed through — an Entra token that fails validation (Azure).** With `auth.mode` `azure` or `hybrid`, the namespace's `entra-jwt` `RequestAuthentication` makes Istio's `jwt_authn` filter validate a bearer token before `llm-gateway-auth` checks it. A token that fails validation (bad signature, wrong issuer, expired) is rejected by `jwt_authn` itself with `401`, or `403` when its audience is not allowed, using Envoy's own body. Like every `4xx` from an upstream component (§2.1), it passes through unchanged, without the envelope or `x-kaito-error-source`. A request without any credential passes `jwt_authn` and is denied by `llm-gateway-auth` (the §3 deny path).

**Model discovery.** `GET /v1/models` and `GET /v1/models/{id}` are served by the `productionstack-status-reporter` through the harness's `models-api` `HTTPRoute`, which overwrites `X-Kaito-Gateway-Namespace` with the `Gateway`'s namespace. The reporter answers its own errors with the §2.1 envelope and `x-kaito-error-source: gateway`. When it has no ready replica or does not answer within `modelsAPI.timeout`, the consolidated `local_reply` answers `502 gateway_unavailable` instead (mapper #3).

| HTTP | `code` | Trigger |
| --- | --- | --- |
| 404 | `model_not_found` | `GET /v1/models/{id}` names a model that is not deployed in this namespace. Same body as the catch-all's `404`. |
| 400 | `invalid_model_id` | `GET /v1/models/` without a model id. |
| 405 | `method_not_allowed` | A method other than `GET`; the response carries `Allow: GET`. |
| 404 | `namespace_not_found` | The `Gateway`'s namespace does not exist or does not carry production-stack's managed-by label. |
| 503 | `models_api_unavailable` | The reporter cannot read namespaces or models from the Kubernetes API. Retryable. |
| 400 | `invalid_gateway_namespace` | `X-Kaito-Gateway-Namespace` is missing or not a valid namespace name. Unreachable through a namespace `Gateway`, whose route always sets it; only a direct call to the reporter gets it. |
| 404 | `unknown_url` | A path other than `/v1/models` and `/v1/models/…`. Unreachable through a namespace `Gateway`, whose route forwards only those paths. |

##### 2.3 Notable behaviors

Three behaviours of the merged catalogue are worth calling out explicitly because they motivate concrete requirements in §3:

1. **Cluster-filter outages must not silently surface as `404`.** BBR ext_proc and `llm-gateway-auth` ext_authz both default to `failure_mode_allow: true`. Left at the default, a BBR outage would silently skip `X-Gateway-Model-Name` insertion and the request would fall through the namespace's catch-all `EnvoyFilter` as a misleading client error (the same trap exists for ext_authz failing open, which would additionally let unauthenticated traffic through). The catalogue closes this in two places: (a) `charts/modelharness` renders both filters fail-closed (`failure_mode_allow: false`, `status_on_error: 503`), and its per-namespace consolidated `local_reply` maps the resulting local `503` to `ext_authz_unavailable` (the `UAEX` flag) or `bbr_unavailable` (header absent, no flag); (b) the modelharness catch-all `EnvoyFilter` distinguishes `X-Gateway-Model-Name` **absent** (→ `400 invalid_request_body`, a body without a usable `model`) from **present but no `HTTPRoute` matched** (→ `404 model_not_found`). A BBR outage never reaches the header-absent catch-all route: the fail-closed filter rejects the request first. The distinction also needs BBR in the filter chain of every namespace `Gateway`, including one with no model yet: BBR is anchored after ext_authz, not on the EPP ext_proc filter, which Istio adds only once an `InferencePool` is attached (§3). Without BBR, a request naming a model would get `400 invalid_request_body` instead of `404 model_not_found`.
2. **Cluster-scope filters run highly-available and a single bad replica must not break the request path.** `llm-gateway-auth` (ext_authz) and BBR (ext_proc) are **cluster-scope** components on the hot path of *every* request in the cluster, so a single replica is a single point of failure. Both run with **at least 2 replicas** by default, and only Ready replicas receive traffic:
   - **BBR** runs with `replicas: 2` by default (schema minimum 2) and always-rendered pod anti-affinity. Kubernetes readiness and liveness probes check BBR's gRPC health service on its dedicated health port. The readiness result drives the pod Ready condition, which drives the Istio EDS endpoints of the BBR ext_proc cluster, so an unhealthy replica is removed from the load-balancing set. On top of this active check, each namespace `Gateway` applies **passive outlier detection** to the BBR cluster (consecutive gateway failures / `5xx`), with `maxEjectionPercent` below 100 so outlier detection can never eject every replica at once. With ≥ 2 replicas behind both checks, the loss of one replica is transparent to clients, and `502 bbr_unavailable` is reached only when **all** replicas are unhealthy.
   - **`llm-gateway-auth`** relies on the replica defaults of its upstream charts (`apikey-authz` runs 3 replicas with a soft topology spread across nodes and zones; `azure-authz` runs 2) and on readiness only: no passive outlier detection is configured on the ext_authz cluster. A replica that starts failing requests can therefore return `502 ext_authz_unavailable` for the window before its readiness probe fails and it leaves the EDS endpoint set. This window is accepted (see Alternatives).

   The reporter surfaces a degraded (but serving) filter as a control-plane `Warning` — `clusterBBRNotReady` / `clusterGatewayAuthNotReady` ([control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue)) — whenever any of its pods is not Ready or Pending, so operators see partial degradation before it becomes a full outage.
3. **`model_unavailable` vs. `model_not_found` are deterministically separable on the request path.** `charts/modeldeployment` always renders an `HTTPRoute` for the model name regardless of whether the `InferencePool` currently has ready endpoints. Therefore: matched route + empty `InferencePool` → `503 model_unavailable` (root-cause-neutral; see Trigger column); no matching route → `404 model_not_found`. Likewise an empty pool behind a **healthy** EPP is not confused with an EPP outage: the EPP rejects the request with the Endpoint Picker Protocol's `503` (→ `model_unavailable`, mapper #4a), while a failed EPP call surfaces as the ext_proc filter's `status_on_error`, Envoy's default `500` (→ `epp_unavailable`, mapper #4b). The split relies on the EPP following the protocol and on Istio leaving the EPP filter's `status_on_error` unset; `model_unavailable_spec.go` and `epp_outage_spec.go` catch an EPP or Istio upgrade that breaks either. The operator-facing root cause for `model_unavailable` is intentionally **not** carried on the response — it is published as one of the modeldeployment-layer Warning events per [control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue), and TSG-2's `model_unavailable` entry directs the operator to inspect that event stream. Alternatives that would discriminate the root cause on the request path (EPP patches, control-plane-state-reading sidecars) are rejected — see Alternatives.

#### 3. Requirements

This section enumerates the requirements that any implementing PR MUST satisfy, grouped by the component that owns the change. Concrete code shape (file paths, struct definitions, template names, RBAC verb lists) is left to the component owners. Control-plane requirements (the `productionstack-status-reporter`'s health reporting and the chart labelling it relies on) are listed in the [control-plane proposal](./20260519-control-plane-error-handling.md#3-requirements); the reporter's model-discovery endpoints are covered here.

| Component | Requirements |
| --- | --- |
| `charts/productionstack` (umbrella chart, incl. `charts/body-based-routing` sub-chart) | - **BBR high availability**: the `body-based-routing` sub-chart MUST render the BBR Deployment with at least 2 replicas (default `replicas: 2`, configurable but with a schema minimum of 2) and pod anti-affinity spreading replicas across nodes per §2.3 item 2.<br>- **Active health check**: the sub-chart MUST render Kubernetes readiness and liveness probes against BBR's gRPC health service on its health port, so only Ready replicas are in the EDS endpoint set of the BBR ext_proc cluster (§2.3 item 2). |
| `charts/modelharness` | - **Cluster-filter fail-closed**: MUST insert the BBR ext_proc and `llm-gateway-auth` ext_authz filters into the namespace `Gateway` with `failure_mode_allow: false` and `status_on_error: 503` per §2.3 item 1.<br>- **BBR placement**: MUST insert BBR after the ext_authz filters, NOT relative to the EPP ext_proc filter, which Istio adds only when an `InferencePool` is attached, so BBR runs on every namespace `Gateway`, with or without a model (§2.3 item 1).<br>- **Client-supplied routing headers**: MUST remove, directly before BBR, every request header the modeldeployment `HTTPRoute` routes on (`X-Gateway-Model-Name`, `X-Gateway-Base-Model-Name`), so only BBR sets them, and the request MUST then be routed as if the client had never sent them (§2.1). The filter MUST NOT run after BBR, where it would delete BBR's own headers.<br>- **Consolidated whole-path `local_reply`**: MUST render a single per-namespace `EnvoyFilter` that rewrites every Envoy-generated `5xx` local reply on the request path into the §2.2 envelope, attributing the at-fault component from the `X-Gateway-Model-Name` header, the response flag, and the status code per §2.1 (the seven-mapper, ordering-based scheme that compensates for the absent `not_filter`). It MUST use only core `AccessLogFilter` types, so it does not depend on an optional Envoy extension, and the BBR outage mapper MUST match the `status_on_error` rendered for the BBR filter. It MUST cover `ext_authz_unavailable`, `model_unavailable`, `gateway_unavailable`, `epp_unavailable`, `bbr_unavailable`, and the `gateway_internal_error` catch-all. Every mapper, the flag-bearing ones included, MUST require a `5xx`, so it never rewrites a `4xx` (§2.1). Every mapper MUST set the envelope's `content-type: application/json` through its `body_format_override`: Envoy resets a `content-type` that `headers_to_add` writes when the local reply already carried the same value, as the EPP's `503` does.<br>- **Catch-all**: the catch-all `EnvoyFilter` MUST distinguish `X-Gateway-Model-Name` **absent** → `400 invalid_request_body` and **present but unmatched** → `404 model_not_found` per §2.3 item 1.<br>- **CORS deny**: while CORS is enabled, the `gateway-cors` filter MUST run before ext_authz and answer a browser request the policy rejects with `403 cors_forbidden` (§2.2).<br>- **Model discovery route**: MUST route `/v1/models` and `/v1/models/{id}` to the `productionstack-status-reporter` through the `models-api` `HTTPRoute`, overwriting `X-Kaito-Gateway-Namespace` with the `Gateway`'s namespace and bounding the call with `modelsAPI.timeout`, so a reporter outage or timeout surfaces as `502 gateway_unavailable` (§2.2).<br>- **Passive outlier detection on BBR**: MUST patch the BBR ext_proc upstream cluster with outlier detection whose `maxEjectionPercent` stays below 100, so a single erroring BBR replica is ejected without tripping the fail-closed `502 bbr_unavailable` path (§2.3 item 2). |
| `charts/modeldeployment` | - **EPP fail-closed**: MUST set `failureMode: FailClose` on the `InferencePool.endpointPickerRef` so an unreachable EPP surfaces an Envoy-generated local `500` (which the consolidated `charts/modelharness` `local_reply` maps to `epp_unavailable`). The EPP image the chart pins MUST reject a request it cannot place with the Endpoint Picker Protocol's `503`, which maps to `model_unavailable` (§2.3 item 3). The actual `5xx`→envelope mapping (including the root-cause-neutral `503 model_unavailable`, §2.3 item 3) is owned by the consolidated per-namespace filter in `charts/modelharness`, NOT by a per-modeldeployment `EnvoyFilter`. No upstream patches to `llm-d-router` MAY be required by this work.<br>- **EPP parsers**: the EPP config MUST list the EPP's parsers without its passthrough parser, so the EPP rejects a request on a path none of them claims with its `400` instead of forwarding it to a model server, which would expose model-server endpoints such as `/metrics` or `/v1/load_lora_adapter` through the gateway (§2.2).<br>- **No pass-through stamp**: the `HTTPRoute` MUST NOT set `x-kaito-error-source` on proxied responses: a `ResponseHeaderModifier` applies to every response, successes included, and the header marks only errors that carry the envelope (§2.1). |
| `productionstack-status-reporter` (model discovery) | - **Error envelope**: MUST answer every model-discovery error with the §2.1 envelope and `x-kaito-error-source: gateway`, using the §2.2 model-discovery codes. An unknown model id MUST get the same `404 model_not_found` body as the catch-all. |
| `llm-gateway-auth` | - **Deny path**: both deny builders (`apikey`, `azure`) MUST emit the OpenAI envelope and `x-kaito-error-source: authz`. gRPC `PermissionDenied` MUST map to HTTP `403 api_key_disabled`; other denies remain `401 invalid_api_key`. Not yet implemented; tracked as a cross-repo dependency in Implementation History.<br>- **High availability**: as a cluster-scope filter on every request path, each ext_authz Deployment MUST run with at least 2 replicas by default and expose a readiness probe, so only Ready replicas receive traffic per §2.3 item 2. Today `apikey-authz` defaults to 3 replicas with a soft topology spread and `azure-authz` to 2. No passive outlier detection is required on the ext_authz cluster (see Alternatives). |

### User Stories

#### Story 4 — Client gets actionable HTTP error

A client calls `POST /v1/chat/completions` with `model: "qwen-typo"`. Production-stack returns:

```http
HTTP/1.1 404 Not Found
Content-Type: application/json
x-kaito-error-source: gateway
x-kaito-requested-model: qwen-typo

{ "error": { "type": "invalid_request_error", "code": "model_not_found",
             "message": "The model does not exist.", "param": "model" } }
```

If the operator has just scaled the deployment from zero, the same path returns instead:

```http
HTTP/1.1 503 Service Unavailable
Content-Type: application/json
Retry-After: 10
x-kaito-error-source: inferenceset

{ "error": { "type": "service_unavailable", "code": "model_unavailable",
             "message": "The requested model is temporarily unavailable. Back off and retry; see Events in kube-system for the root cause.",
             "param": null } }
```

#### Story 5 — BBR outage no longer disguised as 404

When BBR's ext_proc pods are unavailable (cluster-scope filter outage), the Istio Gateway returns a structured `502` with `code: bbr_unavailable` and `x-kaito-error-source: bbr` instead of falling through to the namespace catch-all. The client can tell from the response body that BBR is at fault; the operator sees the `clusterBBRNotReady` `Warning` `Event` in `kube-system` ([control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue)). The same disambiguation applies when `llm-gateway-auth` ext_authz is unavailable (`502 ext_authz_unavailable`, `x-kaito-error-source: authz`, with `clusterGatewayAuthNotReady`). In both cases the envelope is rendered by the per-namespace consolidated `local_reply` in `charts/modelharness`; neither component is patched.

## Alternatives

- **Discriminate `model_unavailable` root cause on the request path** (patch `llm-d-router` to emit a more specific `code`, or add a sidecar that reads control-plane state and rewrites the response). Rejected: production-stack consumes `llm-d-router` as an unmodified upstream binary, and a status-reading sidecar adds a failure domain on the hot path. Every "zero ready endpoints" sub-cause (warm-up / crash / OOM / eviction) demands the same client behaviour (back off on `Retry-After` and retry), so a discriminated `code` would not change client behaviour. Operator-facing root cause is preserved as a `Warning` `Event` in `kube-system` per [control-plane §1.2](./20260519-control-plane-error-handling.md#12-reason-catalogue).
- **Cluster-wide `local_reply` in `charts/productionstack`.** Rejected: the inference `Gateway`s each have their own HTTP connection manager, and the `MeshConfig.extensionProviders`-based cluster-wide filter wiring was removed because the AKS Istio add-on does not allow modifying the shared `MeshConfig`. A second filter patching the same `Gateway` HCM would also reintroduce the non-deterministic cross-`EnvoyFilter` merge order the consolidated per-namespace filter avoids. The per-namespace filter covers the cluster-filter hops on every `Gateway`, so nothing is lost.
- **Envoy active health checks (`health_checks`) on the BBR and ext_authz upstream clusters.** Rejected: those clusters are generated by Istio, and BBR serves its gRPC health service only on a dedicated health port, not on the ext_proc port Envoy dials. Kubernetes readiness probes against that health port achieve the same effect through the EDS endpoint set (§2.3 item 2), with no extra Envoy configuration.
- **Passive outlier detection on the ext_authz upstream cluster.** Not adopted for now: ext_authz relies on its replica count and readiness only. The accepted cost is that a replica that starts failing requests can return `502 ext_authz_unavailable` until its readiness probe fails. Revisit if this window is observed in production.
- **Tell an EPP outage from an EPP rejection by Envoy's response code details** (match the `ext_proc_error` prefix Envoy puts on the details of a failed ext_proc call, through the CEL access-log filter `response.code_details.startsWith('ext_proc_error')`), so the split does not depend on the status the EPP picks. Rejected: the CEL filter is an alpha Envoy extension, and a proxy built without it rejects the whole HCM patch; Envoy documents response code details as not stable; and several EPP failures carry no such prefix (an internal error the EPP reports itself, a header mutation Envoy rejects, an out-of-order message), so they would be reported as `503 model_unavailable`. The status codes the adopted design relies on are a documented protocol requirement and Envoy's documented default (§2.1).
- **Sentinel `status_on_error` on the EPP ext_proc filter** (patch the filter Istio generates per `InferencePool` to fail closed with an otherwise unused status, and match that status). Rejected: Envoy's default `500` already differs from the `503` the EPP rejects with, so a sentinel adds no information, and setting it needs an `EnvoyFilter` patch on an Istio-generated filter, since per-route `ExtProcOverrides` cannot set `status_on_error`.
- **Collapse EPP outages into `model_unavailable`** (drop `epp_unavailable`; the operator learns about an EPP outage from `inferencesetEPPNotReady`). Not adopted: it loses the per-component attribution in `code` and `x-kaito-error-source` that every other hop has.
- **Rewrite an upstream component's `4xx` into the envelope** with `local_reply` mappers, keeping the component's text as the `message`: e.g. model header present + `400` for the EPP's `400`, model header absent + `400` for BBR's `400` for a body that is not valid JSON. Rejected (§2.1). It changes no client behaviour: a `4xx` asks the client to fix its request, the component's own text already names the problem, and OpenAI SDKs raise the matching error type from the status alone. The matches are also heuristics. BBR's `400` cannot be singled out: a local `400` without `X-Gateway-Model-Name` is also the missing-`model` `direct_response` or one of Envoy's protocol-level `400`s, which would be misattributed to `bbr`, and the mapper has no `not_filter` to exclude them. The EPP's two kinds of `400` (unsupported path, missing field) cannot be told apart, so one `code` would cover both. Wrapping only the EPP's `400` would leave some upstream `4xx` wrapped and others not, and keeping its text needs `json_format`, whose values can only be strings, so the envelope would lose `"param": null`.
- **Stamp `x-kaito-error-source: inferenceset` on every response proxied from the inference pods** (an `HTTPRoute` `ResponseHeaderModifier`, so a vLLM-native error also names its hop). Rejected: the filter cannot depend on the response status, so every success would carry a header meant to name the at-fault component of an error (the behaviour before [#280](https://github.com/kaito-project/production-stack/pull/280)). Stamping errors only would need an extra Lua filter running on every response.

## Test Plan

**Chart render tests** — in `test/e2e/deploy/helm/chart_test.go`, run with `go test ./test/e2e/deploy/helm/` (needs `helm`):

- `TestModelHarnessOutageReplyMappers` renders `charts/modelharness` and pins the §2.1 mapper order, each mapper's match signals (model header, router flag, status comparison), status, `x-kaito-error-source`, and `Retry-After`. It fails if a mapper uses an `extension_filter`, and checks mapper #5 against the BBR filter's rendered `status_on_error`. It pins the exact mapper list, including the `5xx` requirement of every mapper, so adding a mapper that rewrites a `4xx` (§2.1), or dropping that requirement, fails it.
- `TestModelHarnessBBRPlacement` checks BBR's insert position for every auth mode: directly after the last ext_authz filter the auth `EnvoyFilter`s render, or first when they render none (§3).
- `TestModelHarnessStripsRoutingHeadersBeforeBBR` checks that the `bbr-ext-proc` `EnvoyFilter` inserts `kaito.strip-routing-headers` directly before BBR, in a patch after the BBR insertion it anchors on, and that the filter removes every header the modeldeployment `HTTPRoute` routes on, in both its default and its Azure variant (§3).
- `TestModelDeploymentHTTPRouteSetsNoErrorSource` checks that the modeldeployment `HTTPRoute` does not set `x-kaito-error-source` (§3).
- `TestModelHarnessModelsAPIRoute` checks the `models-api` `HTTPRoute`: the path match, the `X-Kaito-Gateway-Namespace` overwrite, the backend and the `modelsAPI.timeout` request timeout (§3).

**Unit tests** — `pkg/productionstack-status-reporter/modelsapi/modelsapi_test.go` covers every model-discovery `code` and its status (§2.2).

**E2E tests** (under `test/e2e/`)

Rows that assert a concurrent control-plane `Warning` use `kubectl get events -n kube-system --field-selector source=productionstack-status-reporter,reason=<reason>` (or the Go equivalent in the test harness), per the [control-plane Test Plan](./20260519-control-plane-error-handling.md#test-plan). The **Status** column marks scenarios that are not covered yet.

| Test file | Scenario | Asserted outcome | Status |
| --- | --- | --- | --- |
| `apikey_auth_spec.go` | normal `401` deny (missing or unknown `Authorization`) | `401 invalid_api_key` envelope + `x-kaito-error-source: authz` | Partial: only the `401` status is asserted; the envelope waits on the `llm-gateway-auth` deny-path change (§3) |
| `apikey_auth_spec.go` | valid token whose `APIKey` CR is explicitly disabled | `403 api_key_disabled` envelope + `x-kaito-error-source: authz` (verifies the §3 `llm-gateway-auth` deny-path 403 mapping) | Pending: waits on the `llm-gateway-auth` deny-path change (§3) |
| `model_routing_spec.go` | unknown model name | `404 model_not_found` envelope + `x-kaito-error-source: gateway` | Covered |
| `model_routing_spec.go` | POST a JSON body without a `model` field | `400 invalid_request_body` envelope + `x-kaito-error-source: bbr` | Covered |
| `model_routing_spec.go` | POST a body that is not valid JSON | BBR's native `400`, passed through unchanged (no envelope, no `x-kaito-error-source`; §2.1). Only the status is asserted; the raw body is recorded in the test report. | Covered |
| `model_routing_spec.go` | set `X-Gateway-Model-Name` or `X-Gateway-Base-Model-Name` to a deployed model on a JSON body without a `model`, and on a bodyless `GET /metrics` | `400 invalid_request_body` envelope + `x-kaito-error-source: bbr`: the request is not routed to the model the client named (verifies the §3 routing-header strip) | Covered |
| `model_routing_spec.go` | send a request whose body names a deployed model to a model-server endpoint no EPP parser claims (`GET /metrics`, `POST /v1/load_lora_adapter`) | the EPP's native `400` (`no parser registered …`), passed through unchanged (no envelope, no `x-kaito-error-source`; §2.1): the request does not reach the model server (verifies the §3 `charts/modeldeployment` EPP parsers requirement) | Covered |
| `model_routing_spec.go` | a chat completion without `messages` | the EPP's native `400`, passed through unchanged (no envelope, no `x-kaito-error-source`; §2.1) | Pending |
| `gateway_without_model_spec.go` | install only the modelharness in a fresh namespace (no `InferencePool`) | the `Gateway` config has no EPP ext_proc filter but still runs BBR after ext_authz; a request naming a model → `404 model_not_found`, one without a model → `400 invalid_request_body` (verifies the §3 BBR placement) | Covered on the upstream provider; skipped on azure |
| `filter_order_spec.go` | read the `Gateway`'s filter chain from `/config_dump`; send unauthenticated or invalid-key requests naming an unknown model, and an authenticated request without a `model` | filter order CORS → ext_authz → strip routing headers → BBR → EPP → router; `401`, never `404 model_not_found` (the catch-all still runs auth); `400 invalid_request_body` + `x-kaito-error-source: bbr` | Covered |
| `apikey_auth_spec.go`, browser CORS | a preflight or actual request from a disallowed origin, or with a disallowed method or request header | `403 cors_forbidden` envelope + `x-kaito-error-source: gateway`, answered before authentication | Partial: only the `403` status and the missing `Access-Control-Allow-Origin` are asserted |
| `model_routing_spec.go`, `model_discovery_without_model_spec.go` | `GET /v1/models/{id}` for an unknown model | `404 model_not_found` envelope from the model-discovery endpoint | Covered |
| `bbr_outage_spec.go` | scale BBR Deployment to zero | `502 bbr_unavailable` envelope + `x-kaito-error-source: bbr` (not `404`; verifies fail-closed BBR + the consolidated `local_reply` mapper #5: header absent + status `503`), also when the client sets `X-Gateway-Model-Name` itself (verifies the routing-header strip, §2.1) | Covered |
| `ext_authz_outage_spec.go` | scale `apikey-authz` Deployment to zero | `502 ext_authz_unavailable` envelope + `x-kaito-error-source: authz` (verifies fail-closed ext_authz + the consolidated `local_reply` mapper #1) | Covered |
| `cluster_filter_ha_spec.go`, BBR single-replica loss | with BBR at 2 replicas, scale to 1 and send a burst of prompt requests | **zero** `404` and **zero** `502 bbr_unavailable` responses; then scale to zero and assert the request path fails closed; a `Warning` `clusterBBRNotReady` is concurrently present in `kube-system` for the degraded Deployment (verifies §2.3 item 2) | Partial: the concurrent `clusterBBRNotReady` `Warning` is not asserted |
| `gateway_unavailable_spec.go` | scale the `productionstack-status-reporter` to zero and `GET /v1/models` (scaling the namespace `Gateway` itself to zero leaves no Envoy to answer) | `502 gateway_unavailable` envelope + `x-kaito-error-source: gateway` (header absent + `UH` → consolidated `local_reply` mapper #3) | Covered |
| `epp_outage_spec.go` | scale the EPP Deployment to zero | `502 epp_unavailable` envelope + `x-kaito-error-source: epp` (header present + the ext_proc default `500` → mapper #4b) | Covered |
| `model_unavailable_spec.go`, warm-up | `replicas=0`, send a request | `503 model_unavailable` with `Retry-After` + `x-kaito-error-source: inferenceset`, never `epp_unavailable`, and still so for 30 s after the first match — the steady state is the healthy EPP rejecting the request with `503` (mapper #4a), not only the transient router-flag window (mapper #2). Warm-up is an in-progress state, so no control-plane `Warning` is required | Covered |
| `model_unavailable_spec.go`, crash | wait for Ready, then inject a crash-loop (`exit 1`) | same `503 model_unavailable` response shape — proves request-path code is root-cause-agnostic — while a `Warning` carrying the pass-through KAITO `InferenceSetReady` reason (e.g. `ContainerCrashLoopBackOff`) is emitted on the workload `Namespace` (per [control-plane §1.1](./20260519-control-plane-error-handling.md#11-event-schema); the owning `InferenceSet` is identified in `message`) | Pending |

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
- [x] 2026-11-13: Dropped the cluster / modelharness / modeldeployment level framing from data-plane errors: those layers organise control-plane errors only. Data-plane errors are classified by the request-path hop at fault (`x-kaito-error-source`); the error category overview now lists errors per hop, and the `Layer` columns were removed from the §2.2 catalogue and the Test Plan.
- [x] 2026-11-16: An empty pool behind a healthy EPP surfaced as `502 epp_unavailable`: the llm-d EPP rejects such a request with its own `503` `ImmediateResponse`, which carries no router flag, so it matched the EPP outage mapper. Fixed in [#277](https://github.com/kaito-project/production-stack/pull/277) by splitting the unflagged mappers on the status code: with the model header present, `503` → `model_unavailable` (#4a) and any other `5xx` → `epp_unavailable` (#4b); mapper #5 (`bbr_unavailable`) now matches exactly BBR's pinned `503`. Verified on the AKS Istio proxy image (`proxyv2:v1.31.3-1`, Envoy 1.39) against a mock EPP: EPP `503` → `model_unavailable`; EPP `429` → passed through; EPP unreachable, gRPC error, internal error, rejected header mutation or out-of-order message → `epp_unavailable`. Added the chart render test and the steady-state assertion in `model_unavailable_spec.go`; documented the response-code-details (CEL) alternative.
- [x] 2026-11-16: BBR was anchored on the EPP ext_proc filter, which Istio adds only once an `InferencePool` is attached, so a namespace without a model ran without BBR and answered a request naming a model with `400 invalid_request_body` instead of `404 model_not_found`. Since [#279](https://github.com/kaito-project/production-stack/pull/279) BBR is inserted after the ext_authz filters; added the §3 BBR placement requirement and the `gateway_without_model_spec.go` row.
- [x] 2026-11-16: `x-kaito-error-source` marks only errors generated on the request path: the modeldeployment `HTTPRoute` no longer stamps `inferenceset` on responses proxied from the inference pods, which put it on every success ([#280](https://github.com/kaito-project/production-stack/pull/280)). Updated §2.1, the §2.2 pass-through row and the §3 `charts/modeldeployment` requirement; documented the alternative.
- [x] 2026-11-17: Completed the §2.2 catalogue with the errors the implementation already returns: `403 cors_forbidden` from the `gateway-cors` filter, the model-discovery endpoints' codes, the `UT` trigger of `gateway_unavailable`, and `jwt_authn`'s rejection of an invalid Entra token on Azure. Corrected §2.1: the gateway does not strip a client-supplied `X-Gateway-Model-Name`; BBR overwrites it when the body names a `model`.
- [x] 2026-11-17: Audited the proposal against `main`. Documented the EPP's native `400` for a request it cannot parse (a §2.2 exception), the `x-kaito-requested-model` header on `404 model_not_found`, and the CORS filter on the request path. Added §3 requirements for the CORS reply, the model-discovery route and the status reporter's model-discovery errors. Added `TestModelHarnessModelsAPIRoute` and `filter_order_spec.go` to the Test Plan. Dropped the unused `internal_error` envelope type, and named the EPP by its current upstream, `llm-d-router`.
- [x] 2026-11-18: Stated which responses carry the envelope: the gateway rewrites only the `5xx` local replies Envoy generates, never a `4xx`, and a `4xx` carries the envelope only when the component that returns it emits it. A `4xx` from an upstream component (BBR, the EPP, Istio `jwt_authn`, Envoy) therefore passes through unchanged; the former exceptions follow from this rule. Added the §3 requirement that the consolidated `local_reply` not rewrite a `4xx`, and documented wrapping an upstream component's `4xx` as a rejected alternative.
- [x] 2026-11-19: A client-supplied `X-Gateway-Model-Name` or `X-Gateway-Base-Model-Name` survived on a request whose body named no `model`, so the request was routed to the model the client named, and a BBR outage surfaced as `503 model_unavailable`. Since [#285](https://github.com/kaito-project/production-stack/pull/285), a Lua filter directly before BBR removes both headers, so only BBR sets them; this reverses the 2026-11-17 correction. Outage mappers #2 and #3 now also require a `5xx`, like every other mapper: `NC`'s status is configurable and `UT` can answer `204`. Updated §2.1, the router-flag rows of §2.2, the §3 `charts/modelharness` requirements and the Test Plan.
- [x] 2026-11-19: Bumped the EPP to `llm-d-router` `v1.0.0`. Its `400`s now carry an OpenAI error object as JSON, and its OpenAI parser also claims `/images/generations`, `/images/edits` and `/audio/speech`; updated the §2.2 EPP pass-through paragraph. `v1.0.0` appends a passthrough parser to its default parsers, which forwards a request on a path none of them claims to a model server; the modeldeployment chart now lists the default parsers without it, so such a request is still rejected with a `400` (new §3 `charts/modeldeployment` requirement and Test Plan row). Its `503` for an empty pool now carries `content-type: application/json`, so Envoy reset the `content-type` that the `local_reply` mappers wrote through `headers_to_add` to `text/plain`; the mappers now set it through `body_format_override` (§3 `charts/modelharness` requirement).
- [ ] TBD: TSG-2 (data-plane errors) merged, internal-only
