# TSG-2 — Production-Stack Data-Plane Errors

| | |
| --- | --- |
| **Scope** | Data-plane (HTTP request-path) failures returned to OpenAI-compatible clients by the namespace Gateway / Envoy. |
| **Signal** | An OpenAI-style JSON error body + a stable `x-kaito-error-source` response header. |
| **Audience** | Internal operators / on-call (and, indirectly, API clients reading the error envelope). |
| **Companion** | [TSG-1 — Control-Plane Errors](tsg-1-control-plane-errors.md) (in-cluster health Events). |
| **Source of truth** | `charts/modelharness/templates/` (this TSG is keyed off the stable `code` strings rendered there). |

> This TSG is keyed off the closed `code` vocabulary in the JSON error envelope. Every section anchor below is the literal `code` string.

---

## 1. Overview

When a request cannot be served, the namespace Gateway returns a **single, normalized** OpenAI-compatible error envelope rather than a raw Envoy 5xx. Each failure mode is mapped to exactly one `code` plus a stable `x-kaito-error-source` header that names the responsible component. The fine-grained root cause is **not** in the HTTP body — it is in the `kube-system` Events produced by the reporter (see [TSG-1](tsg-1-control-plane-errors.md)). The body is deliberately terse and client-safe.

### Error envelope

```json
{ "error": { "type": "<type>", "code": "<code>", "message": "<safe message>", "param": <null|field> } }
```

### Response headers

| Header | Meaning |
| --- | --- |
| `x-kaito-error-source` | The component blamed: `authz`, `bbr`, `gateway`, `epp`, `inferenceset`. The primary triage signal. |
| `content-type: application/json` | Always set on synthesized replies. |
| `x-kaito-requested-model` | On `model_not_found`, echoes the requested model name. |

### Request path & attribution

```
client → namespace Gateway (Envoy) → ext_authz (llm-gateway-auth) → BBR ext_proc → HTTPRoute → EPP ext_proc → vLLM model pods
```

Two facts drive attribution, evaluated by a **single ordered, first-match** `local_reply_config` on the Gateway listener (`charts/modelharness/templates/envoyfilter-outage-reply.yaml`):

- **`X-Gateway-Model-Name` header** — injected by BBR *after* it parses the body and *before* routing. **Absent** ⇒ the request failed *before/at* infra routing (authz, BBR, gateway dataplane). **Present** ⇒ it reached model-side routing (EPP, model pods).
- **Envoy response flag** — `UAEX` (ext_authz), `UH`/`LH`/`UF`/`UC`/`NC`/`UT`/`UPE` (no healthy upstream / not yet programmed), or none.

Ordered mappers (first match wins):

| # | Condition | → `code` (HTTP) / source |
| --- | --- | --- |
| 1 | `UAEX` + status ≥ 500 | `ext_authz_unavailable` (502) / `authz` |
| 2 | model header **present** + router flag | `model_unavailable` (503) / `inferenceset` |
| 3 | model header **absent** + router flag | `gateway_unavailable` (502) / `gateway` |
| 4 | model header **present** + 5xx, no flag | `epp_unavailable` (502) / `epp` |
| 5 | model header **absent** + 5xx, no flag | `bbr_unavailable` (502) / `bbr` |
| 6 | any remaining ≥ 500 | `gateway_internal_error` (original 5xx) / `gateway` |

Two more codes come from the catch-all routing filter (`charts/modelharness/templates/envoyfilter-not-found.yaml`), evaluated as the lowest-priority `HTTPRoute`s (not 5xx — they are deliberate client-error direct responses):

| Condition | → `code` (HTTP) / source |
| --- | --- |
| model header present, no per-model route | `model_not_found` (404) / `gateway` |
| model header absent (no `model` in body) | `invalid_request_body` (400) / `bbr` |

**Authentication denials** (`401` / `403`) are emitted **in-process** by the `llm-gateway-auth` ext_authz service (sibling repo), not by Envoy `local_reply` — they are legitimate denies and are deliberately left untouched by mapper #1 (the `≥ 500` guard).

## 2. Triage in three steps

1. **Read `x-kaito-error-source`.** It names the component to investigate.
   ```sh
   curl -sS -D- -o /dev/null https://<gateway-host>/v1/chat/completions \
     -H 'content-type: application/json' \
     -d '{"model":"<model>","messages":[{"role":"user","content":"ping"}]}' \
     | grep -i 'x-kaito-error-source\|HTTP/'
   ```
2. **Cross-link to the control plane.** Most 5xx codes have a one-to-one control-plane reason in [TSG-1](tsg-1-control-plane-errors.md) — pivot there for the root cause:
   ```sh
   kubectl get events -n kube-system --field-selector source=productionstack-status-reporter --sort-by=.lastTimestamp
   ```
3. **Mitigate** per the section below, then re-test.

## 3. Quick reference

| `code` | HTTP | `x-kaito-error-source` | TSG-1 cross-link |
| --- | --- | --- | --- |
| [`ext_authz_unavailable`](#ext_authz_unavailable) | 502 | `authz` | [`clusterGatewayAuthNotReady`](tsg-1-control-plane-errors.md#clustergatewayauthnotready) |
| [`bbr_unavailable`](#bbr_unavailable) | 502 | `bbr` | [`clusterBBRNotReady`](tsg-1-control-plane-errors.md#clusterbbrnotready) |
| [`gateway_unavailable`](#gateway_unavailable) | 502 | `gateway` | [`modelharnessGatewayProgrammingFailed`](tsg-1-control-plane-errors.md#modelharnessgatewayprogrammingfailed) |
| [`epp_unavailable`](#epp_unavailable) | 502 | `epp` | [`inferencesetEPPNotReady`](tsg-1-control-plane-errors.md#inferenceseteppnotready) |
| [`model_unavailable`](#model_unavailable) | 503 | `inferenceset` | [`inferencesetModelPodsNotReady`](tsg-1-control-plane-errors.md#inferencesetmodelpodsnotready) |
| [`gateway_internal_error`](#gateway_internal_error) | (original 5xx) | `gateway` | — |
| [`model_not_found`](#model_not_found) | 404 | `gateway` | [`inferencesetRouteNotReady`](tsg-1-control-plane-errors.md#inferencesetroutenotready) |
| [`invalid_request_body`](#invalid_request_body) | 400 | `bbr` | — (client error) |
| [`invalid_api_key`](#invalid_api_key) | 401 | (authz, in-process) | — (client error) |
| [`api_key_disabled`](#api_key_disabled) | 403 | (authz, in-process) | [`modelharnessAPIKeyReconcileFailed`](tsg-1-control-plane-errors.md#modelharnessapikeyreconcilefailed) |

---

## 4. Infrastructure-side 5xx (model header absent)

### ext_authz_unavailable

**HTTP 502 · `x-kaito-error-source: authz` · mapper #1 (`UAEX` + status ≥ 500).**

**Body.**
```json
{"error":{"type":"service_unavailable","code":"ext_authz_unavailable","message":"The authentication service is temporarily unavailable. Please retry.","param":null}}
```

**Trigger.** The ext_authz filter failed closed (the `llm-gateway-auth` ext_authz service is down/unreachable; the filter is pinned to `status_on_error: 503`, which `UAEX` + the `≥ 500` guard rewrites to a clean 502). Legitimate `401`/`403` denies are **not** affected.

**Confirm / mitigate.** Pivot to TSG-1 [`clusterGatewayAuthNotReady`](tsg-1-control-plane-errors.md#clustergatewayauthnotready):
```sh
kubectl -n llm-gateway-auth get deploy apikey-authz
kubectl -n llm-gateway-auth logs deploy/apikey-authz
```
Restore the ext_authz Deployment (≥ 2 replicas for HA). Client guidance: retryable, back off and retry.

### bbr_unavailable

**HTTP 502 · `x-kaito-error-source: bbr` · mapper #5 (model header absent + 5xx, no router flag).**

**Body.**
```json
{"error":{"type":"service_unavailable","code":"bbr_unavailable","message":"The request router is temporarily unavailable. Please retry.","param":null}}
```

**Trigger.** The BBR ext_proc filter failed closed *before* it could inject `X-Gateway-Model-Name` (pinned `status_on_error: 503`). This fires only when **all** BBR replicas are unhealthy — passive outlier detection ejects bad replicas first and is hard-capped below 100% so a single-replica blip cannot trip this path.

**Confirm / mitigate.** Pivot to TSG-1 [`clusterBBRNotReady`](tsg-1-control-plane-errors.md#clusterbbrnotready):
```sh
kubectl -n kaito-system get deploy body-based-router
kubectl -n kaito-system logs deploy/body-based-router
```
Restore BBR (≥ 2 replicas; verify `bbr.outlierDetection`). Client guidance: retryable.

### gateway_unavailable

**HTTP 502 · `x-kaito-error-source: gateway` · mapper #3 (model header absent + router flag).**

**Body.**
```json
{"error":{"type":"service_unavailable","code":"gateway_unavailable","message":"The namespace gateway has no ready upstream; retry shortly.","param":null}}
```

**Trigger.** The namespace Gateway dataplane itself could not forward — no healthy upstream (`UH`/`LH`) or the listener/cluster is not yet programmed while the harness converges (`UF`/`UC`/`NC`). Ordered before #5 so a flagged infra-side failure is attributed to the gateway, not BBR.

**Confirm / mitigate.** Pivot to TSG-1 [`modelharnessGatewayProgrammingFailed`](tsg-1-control-plane-errors.md#modelharnessgatewayprogrammingfailed) / [`modelharnessGatewayClassMissing`](tsg-1-control-plane-errors.md#modelharnessgatewayclassmissing):
```sh
kubectl -n <ns> get gateway <name> -o jsonpath='{.status}' | jq
kubectl -n <ns> get pods -l gateway.networking.k8s.io/gateway-name=<name>
```
Usually transient during convergence; if persistent, fix Gateway programming. Client guidance: retryable.

### gateway_internal_error

**HTTP: original 5xx preserved · `x-kaito-error-source: gateway` · mapper #6 (catch-all).**

**Body.**
```json
{"error":{"type":"service_unavailable","code":"gateway_internal_error","message":"The namespace gateway encountered an unexpected error. Please retry.","param":null}}
```

**Trigger.** Any local 5xx not matched by mappers #1–#5 — the safety net so no raw Envoy 5xx ever escapes. `status_code` is intentionally **not** overridden (the original 5xx is preserved); only the body and source header are normalized.

**Confirm / mitigate.** No dedicated control-plane reason. Inspect the Gateway pod:
```sh
kubectl -n <ns> logs -l gateway.networking.k8s.io/gateway-name=<name> --tail=200
kubectl get events -n kube-system --field-selector source=productionstack-status-reporter
```
If this recurs, capture the Envoy access logs (original status + response flags) and escalate — it indicates an unmapped failure mode.

## 5. Model-side 5xx (model header present)

### model_unavailable

**HTTP 503 · `x-kaito-error-source: inferenceset` · mapper #2 (model header present + router flag).**

**Body.**
```json
{"error":{"type":"service_unavailable","code":"model_unavailable","message":"The requested model is temporarily unavailable. Back off and retry; see Events in kube-system for the root cause.","param":null}}
```

**Trigger.** The request routed to a model whose backing pods are not serving (no healthy upstream behind the `InferencePool`). The body explicitly points operators to `kube-system` Events.

**Confirm / mitigate.** Pivot to TSG-1 [`inferencesetModelPodsNotReady`](tsg-1-control-plane-errors.md#inferencesetmodelpodsnotready) (and possibly [`inferencesetInfraProvisioningFailed`](tsg-1-control-plane-errors.md#inferencesetinfraprovisioningfailed) / [`inferencesetWeightDownloadSlow`](tsg-1-control-plane-errors.md#inferencesetweightdownloadslow)):
```sh
kubectl -n <ns> get pods -l inferenceset.kaito.sh/created-by=<inferenceset>
kubectl get events -n kube-system --field-selector source=productionstack-status-reporter | grep <inferenceset>
```
Client guidance: retryable (503), back off.

> Note: a genuine 5xx returned *by* a healthy vLLM upstream is a proxied response, not a local reply — it passes through unchanged and carries `x-kaito-error-source: inferenceset` (stamped on the per-model HTTPRoute), so it is not re-synthesized by these mappers.

### epp_unavailable

**HTTP 502 · `x-kaito-error-source: epp` · mapper #4 (model header present + 5xx, no router flag).**

**Body.**
```json
{"error":{"type":"service_unavailable","code":"epp_unavailable","message":"The endpoint picker is temporarily unavailable. Please retry.","param":null}}
```

**Trigger.** The EPP (endpoint picker) ext_proc filter failed closed — the `InferencePool` `endpointPickerRef` is `FailClose`, so when Envoy cannot reach the EPP it emits the ext_proc default 500 with no router flag, on the model-serving side.

**Confirm / mitigate.** Pivot to TSG-1 [`inferencesetEPPNotReady`](tsg-1-control-plane-errors.md#inferenceseteppnotready):
```sh
kubectl -n <ns> get deploy -l kaito.sh/inferenceset=<inferenceset>
kubectl -n <ns> logs deploy/<epp-deploy>
```
Restore the EPP Deployment. Client guidance: retryable.

## 6. Client-error responses (4xx)

### model_not_found

**HTTP 404 · `x-kaito-error-source: gateway` · catch-all route (model header present, no per-model route).**

**Body.**
```json
{"error":{"message":"The model does not exist.","type":"invalid_request_error","param":"model","code":"model_not_found"}}
```
Also sets `x-kaito-requested-model: <model>`.

**Trigger.** BBR parsed a `model` field and injected the header, but no per-model `HTTPRoute` matched — either a genuinely unknown model name, or the model's route is not (yet) programmed.

**Confirm / mitigate.** If the model *should* exist, pivot to TSG-1 [`inferencesetRouteNotReady`](tsg-1-control-plane-errors.md#inferencesetroutenotready):
```sh
kubectl -n <ns> get httproute -l kaito.sh/inferenceset=<inferenceset>
kubectl -n <ns> get inferenceset
```
Otherwise this is a legitimate client error (typo'd model name). Not retryable without changing the `model`.

### invalid_request_body

**HTTP 400 · `x-kaito-error-source: bbr` · catch-all route (model header absent).**

**Body.**
```json
{"error":{"message":"The request body did not specify a model; the \"model\" field is required.","type":"invalid_request_error","param":"model","code":"invalid_request_body"}}
```

**Trigger.** BBR could not extract a usable `model` field, so no header was injected and the request fell through to the unconditional fallback route. A genuine client error (missing/malformed `model`).

**Mitigate.** Client must send a valid JSON body with a `model` field. Not retryable as-is.

### invalid_api_key

**HTTP 401 · authentication denial emitted in-process by `llm-gateway-auth` ext_authz (not an Envoy `local_reply`).**

**Trigger.** Missing/unknown/revoked API key. This is a deliberate `< 500` deny, left untouched by the outage mappers.

**Mitigate.** Client error — supply a valid `Authorization` / API-key header. No control-plane reason; verify the `APIKey` CRs exist if a *known-good* key is rejected:
```sh
kubectl -n <ns> get apikey
```

### api_key_disabled

**HTTP 403 · authentication denial emitted in-process by `llm-gateway-auth` ext_authz.**

**Trigger.** The API key is recognized but disabled/expired.

**Mitigate.** Re-enable / rotate the key. If a key that *should* be active is rejected, pivot to TSG-1 [`modelharnessAPIKeyReconcileFailed`](tsg-1-control-plane-errors.md#modelharnessapikeyreconcilefailed):
```sh
kubectl -n <ns> get apikey <name> -o jsonpath='{.status.conditions}' | jq
```

## 7. Escalation

- Capture the full response headers + body (`curl -D-`), especially `x-kaito-error-source` and the HTTP status.
- Pivot to the matching TSG-1 reason and capture its `kube-system` event + the named object.
- For `gateway_internal_error` (unmapped), capture Envoy access logs (status + response flags) from the Gateway pod and escalate to the production-stack on-call.

## 8. References

- [TSG-1 — Control-Plane Errors](tsg-1-control-plane-errors.md)
- Proposal: [End-to-End Error Handling](../proposals/20260519-end-to-end-error-handling.md)
- Charts: `charts/modelharness/templates/envoyfilter-outage-reply.yaml`, `charts/modelharness/templates/envoyfilter-not-found.yaml`, `charts/modelharness/templates/envoyfilter-ext-authz.yaml`
