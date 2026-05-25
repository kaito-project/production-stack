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
last-updated: 2026-05-25
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
        - [1.1 Cluster level](#11-cluster-level)
        - [1.2 Modelharness level](#12-modelharness-level)
        - [1.3 Modeldeployment level](#13-modeldeployment-level)
        - [1.4 Aggregation and propagation](#14-aggregation-and-propagation)
      - [2. Data-plane errors](#2-data-plane-errors)
        - [2.1 Unified OpenAI-compatible error envelope](#21-unified-openai-compatible-error-envelope)
        - [2.2 Cluster level](#22-cluster-level)
        - [2.3 Modelharness level](#23-modelharness-level)
        - [2.4 Modeldeployment level](#24-modeldeployment-level)
        - [2.5 Fixing the "BBR outage looks like 404" bug](#25-fixing-the-bbr-outage-looks-like-404-bug)
        - [2.6 Distinguishing "no ready endpoints" from "not found"](#26-distinguishing-no-ready-endpoints-from-not-found)
      - [3. Required code / chart changes](#3-required-code--chart-changes)
        - [3.1 Control-plane changes](#31-control-plane-changes)
        - [3.2 Data-plane changes](#32-data-plane-changes)
        - [3.3 Cross-cutting artefacts](#33-cross-cutting-artefacts)
    - [User Stories](#user-stories)
      - [Story 1 — Operator diagnoses a stuck modeldeployment](#story-1--operator-diagnoses-a-stuck-modeldeployment)
      - [Story 2 — Operator diagnoses a broken namespace (modelharness)](#story-2--operator-diagnoses-a-broken-namespace-modelharness)
      - [Story 3 — Operator diagnoses a broken cluster install](#story-3--operator-diagnoses-a-broken-cluster-install)
      - [Story 4 — Client gets actionable HTTP error](#story-4--client-gets-actionable-http-error)
      - [Story 5 — BBR outage no longer disguised as 404](#story-5--bbr-outage-no-longer-disguised-as-404)
    - [Requirements](#requirements)
      - [Functional Requirements](#functional-requirements)
      - [Non-Functional Requirements](#non-functional-requirements)
    - [Risks and Mitigations](#risks-and-mitigations)
  - [Alternatives](#alternatives)
  - [Upgrade Strategy](#upgrade-strategy)
  - [Additional Details](#additional-details)
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

1. **Control-plane errors** — failures observable on Kubernetes status conditions and events, covering install-time misconfiguration **and** post-install drift (a subchart Deployment crashing, a CRD being deleted, an `APIKey` Secret being rotated, an EPP pod entering `CrashLoopBackOff`, etc.). Surfaced on the resource that owns each layer (`ConfigMap/productionstack-status` for cluster level, `ConfigMap/modelharness-status` for modelharness, `InferenceSet.status` for modeldeployment).
2. **Data-plane errors** — failures observable on the HTTP request path against an installed stack. Standardised onto a single OpenAI-compatible JSON envelope, with a stable `code` and a new `x-kaito-error-source` header that pinpoints which hop (cluster filter, namespace gateway, modeldeployment EPP, upstream pod) produced the error.

## Motivation

Production-stack is built from independent OSS components (Istio Gateway, `llm-gateway-auth`, BBR, EPP, KAITO `InferenceSet`, KEDA, keda-kaito-scaler). Each component does its own error reporting in its own format and on its own resource, which means:

- **For operators.** Diagnosing "why is my model not ready?" requires walking objects across three layers and four-plus namespaces (`kaito-system`, `istio-system`, `llm-gateway-auth`, the workload namespace) and correlating events by timestamp. There is no aggregated readiness signal for any of the three layers.
- **For end users.** The HTTP response a client receives for a broken stack is non-deterministic: `404` from the catch-all, `503` with no body from Envoy when EPP or BBR is unreachable, or `000` (connection reset) when ext_authz / ext_proc fails open. The response body shape changes by component.
- **For documentation.** Without stable `Reason` strings (status side) and stable `code` values (response side), TSG-1 (control-plane errors, all three layers) and TSG-2 (data-plane errors, all three layers) — the two deliverables called out in #71, organised along the same two-category axis as this proposal — cannot be deep-linked.

### Goals

- Define a control-plane-error taxonomy that covers cluster, modelharness, and modeldeployment levels, with each level's failures aggregated onto a single Kubernetes resource per layer (no new CRDs).
- Define a data-plane-error taxonomy that covers cluster-level filters (ext_authz, BBR), modelharness-level routing (namespace Gateway, catch-all), and modeldeployment-level dispatch (EPP, upstream pod), all standardised onto one OpenAI-compatible JSON envelope.
- Eliminate the BBR/ext_authz-outage-looks-like-404 ambiguity.
- Distinguish `model_not_found` (route does not exist) from `model_unavailable` (route exists but `InferencePool` has zero ready endpoints — covers warming up, crash, OOM, eviction; root cause exposed via `InferenceSet.status`).
- Publish two TSGs aligned with the two top-level categories — TSG-1 (control-plane errors, covering cluster + modelharness + modeldeployment) and TSG-2 (data-plane errors, covering cluster + modelharness + modeldeployment) — both keyed off the `Reason` / `code` strings defined here.

### Non-Goals/Future Work

- Introducing new CRDs or aggregator controllers per level. We deliberately reuse existing status fields (and, for cluster level, a lightweight `ConfigMap`-backed summary published by the `productionstack-status-reporter` Deployment shipped with the umbrella chart).
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
| **Control-plane errors** (status conditions & events on K8s resources; install-time misconfig **and** post-install drift) | - umbrella chart subchart startup or runtime crash<br>- CRD installation or post-install deletion<br>- KAITO/Istio/KEDA controller readiness transitions | - namespace `Gateway` provisioning or runtime regression<br>- `AuthorizationPolicy` / `APIKey` / `EnvoyFilter` / `NetworkPolicy` provisioning or post-install drift | - `InferenceSet` / `InferencePool` / EPP / `HTTPRoute` startup or runtime regression<br>- infra (GPU node) provisioning or reclaim<br>- scaling misconfig |
| **Data-plane errors** (request path → OpenAI-compatible HTTP responses) | - BBR ext_proc outage<br>- `llm-gateway-auth` ext_authz outage | - namespace gateway dataplane outage<br>- missing/invalid `APIKey` secret<br>- `NetworkPolicy` blackhole<br>- catch-all `model_not_found` | - EPP outage<br>- no ready model endpoints (warming up / crash / OOM / eviction)<br>- upstream pod timeout<br>- EPP internal error |

The sections below enumerate the taxonomy for each cell, then describe aggregation and the per-component changes required.

#### 1. Control-plane errors

Control-plane errors are surfaced through Kubernetes-native channels (status conditions, events) and cover both install-time misconfiguration and post-install drift — anything an operator should see on a Kubernetes resource rather than on an HTTP response. Each layer aggregates onto a single resource per FR1; the **kind** of resource differs by layer to match what already exists in each layer's ownership model:

| Layer | Owning resource | Why this resource |
| --- | --- | --- |
| Cluster | `ConfigMap/productionstack-status` in the umbrella release namespace (default `kaito-system`), written by the `productionstack-status-reporter` Deployment | No natural owning CR spans Istio / KAITO / KEDA / BBR / `llm-gateway-auth`; a chart-rendered ConfigMap is the lightest rollup point. |
| Modelharness | `ConfigMap/modelharness-status` per workload namespace, written by the same reporter | No natural owning CR spans `Gateway` / `EnvoyFilter` / `AuthorizationPolicy` / `APIKey` / `NetworkPolicy`. Gateway API's own `status.conditions` on the namespace `Gateway` remain authoritative for Gateway-related Reasons and are mirrored into the ConfigMap. |
| Modeldeployment | `InferenceSet.status.conditions[]` | The KAITO `InferenceSet` CR is already the canonical declaration of one modeldeployment, already has a reconciler, and already publishes a `Ready` condition; the structured `Reason` vocabulary in §1.3 lives on that condition. No new ConfigMap is introduced. |

Lower-layer outages are collapsed onto a single `*DependencyMissing` Reason on the higher layer, so a cluster outage never fans out into N harness/modeldeployment-local Reasons across N namespaces. In every layer the surfaced `Reason` follows the closed vocabulary in §1.1–§1.3 (FR2), and every `message` ends with `See https://.../tsg/aimanager/control-plane-errors#<Reason>`.

##### 1.1 Cluster level

Owning resource: `ConfigMap/productionstack-status` in the umbrella chart's release namespace (default `kaito-system`), plus events on the same ConfigMap.

| `Reason` | Triggered by | Detection source |
| --- | --- | --- |
| `clusterCRDMissing` | Required CRD absent. The CRD set is derived from what `charts/productionstack`, `charts/modelharness`, and `charts/modeldeployment` actually render or reference (including RBAC grants for runtime informers):<br>- Gateway API (`gateway.networking.k8s.io/v1`): `Gateway`, `HTTPRoute`<br>- GAIE (`inference.networking.k8s.io/v1`): `InferencePool`; (`inference.networking.x-k8s.io/v1alpha1`): `InferenceObjective`, `InferenceModelRewrite` (watched by EPP per `charts/modeldeployment/templates/epp-rbac.yaml`)<br>- KAITO (`kaito.sh/v1alpha1`): `InferenceSet`, `Workspace`, `APIKey`<br>- Istio (`networking.istio.io/v1alpha3`): `EnvoyFilter`; (`networking.istio.io/v1`): `DestinationRule`; (`security.istio.io/v1`): `AuthorizationPolicy`<br>- KEDA (`keda.sh/v1alpha1`): `ScaledObject`, `ClusterTriggerAuthentication` | API discovery by the `productionstack-status-reporter` watcher (periodic); also each affected component poll-then-startup-timeout-exits and is restarted by Kubernetes until the CRD appears |
| `clusterBBRNotReady` | `body-based-routing` subchart Deployment NotReady:<br>- `ImagePullBackOff`<br>- missing `EnvoyFilter` injection point<br>- RBAC errors<br>- runtime crash / scale-to-zero | `Deployment.status` of BBR pod in `istio-system`; events on the EnvoyFilter |
| `clusterKedaKaitoScalerNotReady` | `keda-kaito-scaler` subchart Deployment NotReady or its `ScaledObject`/`TriggerAuthentication` rejected by KEDA (covers both install-time and runtime failures) | `Deployment.status` + KEDA `ScaledObject.status.conditions` |
| `clusterGatewayAuthNotReady` | `llm-gateway-auth` chart components (`apikey-operator`, `apikey-authz`) NotReady, or `MeshConfig` patch missing the `apikey-ext-authz` extension provider (covers both install-time and runtime failures) | `Deployment.status` + Istio `MeshConfig` lookup |
| `clusterIstioControlPlaneNotReady` | `istiod` not running / `IstioOperator` unhealthy | `Deployment.status` of `istiod` |
| `clusterKaitoControllerNotReady` | KAITO workspace controller `Deployment` NotReady | `Deployment.status` of KAITO controller |
| `clusterKedaNotReady` | KEDA control plane components NotReady: `keda-operator` Deployment and `keda-operator-metrics-apiserver` Deployment (in the `keda` namespace, regardless of whether KEDA is installed as a managed add-on or via upstream Helm). | `Deployment.status` of `keda-operator` and `keda-operator-metrics-apiserver` |
| `clusterNodeProvisionerNotReady` | Cluster node provisioner Deployment NotReady. The reporter probes whichever provisioner is registered:<br>- upstream Karpenter (`karpenter` Deployment in the `karpenter` namespace)<br>- `gpu-node-mocker` (`gpu-node-mocker` Deployment, see `charts/gpu-node-mocker`) used for E2E<br>- any other Deployment registered via `clusterStatus.nodeProvisioner.{name,namespace}` chart values<br><br>If none is registered, the check is skipped (treated as Ready), so clusters that pre-provision GPU nodes are not penalised. | `Deployment.status` of the configured node-provisioner Deployment |
| `clusterReady` (positive) | All of the above clear | aggregator |

Reasons map to **TSG-1** anchors under §cluster.

##### 1.2 Modelharness level

Owning resource: `ConfigMap/modelharness-status` in the workload namespace, plus Kubernetes `Event`s on the same ConfigMap.

- **Written by:** the same `productionstack-status-reporter` Deployment introduced at the cluster level (§1.1), extended with a namespace-scoped reconciler.
- **Discovery:** the chart renders an initially-empty `ConfigMap/modelharness-status` on install with a well-known label; the reporter discovers managed namespaces via that label selector rather than a configured namespace list.
- **Fields:** `ready`, `reason`, `message`, `components`.
- **Drift detection:** every condition is derived from live API objects, so post-install drift (admin deletes `llm-gateway-auth`, rotates the `APIKey` Secret, edits the catch-all `EnvoyFilter`) surfaces the same way as install-time misconfiguration.
- **Gateway API conditions:** `Gateway.status.conditions` (`Accepted`, `Programmed`, `ResolvedRefs`, `Listeners[*].Conditions`) remain source of truth for the two Gateway-related Reasons and are mirrored into the aggregated ConfigMap.

| `Reason` | Triggered by | Detection source |
| --- | --- | --- |
| `modelharnessDependencyMissing` | Cluster-level prerequisite not satisfied: `ConfigMap/productionstack-status.ready != "true"` (e.g. `clusterGatewayAuthNotReady`, `clusterBBRNotReady`, `clusterIstioControlPlaneNotReady`). Every harness-level Reason whose root cause sits in the cluster layer collapses onto this single Reason — the `message` carries the upstream `reason` string so operators are pointed directly at the cluster ConfigMap. | reconcile-time lookup of `ConfigMap/productionstack-status` in the umbrella release namespace |
| `modelharnessGatewayClassMissing` | `gatewayClassName: istio` not registered (local misconfiguration of the `Gateway.spec.gatewayClassName`; cluster-wide Istio absence is reported as `modelharnessDependencyMissing` instead) | watch `Gateway.status.conditions[Accepted]=False` (`Reason=NoMatchingParent` / `InvalidParameters`) |
| `modelharnessGatewayProgrammingFailed` | Harness-local Gateway programming failure (cluster-level Istio control plane outage is reported as `modelharnessDependencyMissing`):<br>- Listener port collision<br>- TLS secret missing<br>- Envoy proxy pod cannot start for harness-local reasons | watch `Gateway.status.conditions[Programmed]=False`, listener-level conditions |
| `modelharnessExtAuthzProviderMissing` | The namespace `AuthorizationPolicy` references a provider name that does **not** match the registered cluster-level provider — i.e. local chart misconfiguration (an admin hand-edited the rendered policy or supplied wrong `values.yaml`). Cluster-wide absence of the provider (because `llm-gateway-auth` is not installed) is reported as `modelharnessDependencyMissing` instead. | reconcile-time comparison of `AuthorizationPolicy.spec.action.provider.name` against `MeshConfig.extensionProviders[*].name` |
| `modelharnessAPIKeyReconcileFailed` | Local `APIKey` CR is invalid or conflicts with an existing Secret — i.e. the `apikey-operator` is up (cluster-level `clusterGatewayAuthNotReady` is clear) but rejected this specific CR. Operator down / cluster-wide failures are reported as `modelharnessDependencyMissing`. | watch `APIKey.status.conditions` (from `llm-gateway-auth`); only surfaced when cluster-level `clusterGatewayAuthNotReady` is clear |
| `modelharnessCatchAllFilterRejected` | namespace `EnvoyFilter` `model-not-found-direct` rejected by Istio (workload selector mismatch, schema error) | watch Istio `EnvoyFilter.status` (Istio publishes validation errors as a status annotation / condition); fallback heuristic when status is empty: assert the filter's `workloadSelector` matches the namespace `Gateway` pod labels |
| `modelharnessNetworkPolicyMisconfigured` | One of:<br>- `networkPolicy.allowedIngressNamespaces` references nonexistent namespaces<br>- KEDA namespace mismatch leaves the keda-kaito-scaler unable to reach inference pods | reconcile-time lookup by the reporter:<br>- for each name in `allowedIngressNamespaces`, verify the `Namespace` exists<br>- verify the KEDA scaler namespace from the cluster-level reporter matches |
| `modelharnessReady` (positive) | All of the above clear | aggregator |

Each `Reason` corresponds to an anchor in **TSG-1** under §modelharness. Priority order: see §1.4.

##### 1.3 Modeldeployment level

Owning resource: `InferenceSet.status.conditions[]`.

| `Reason` | HTTP-equivalent semantics | Triggered by | Detection source |
| --- | --- | --- | --- |
| `inferencesetInfraProvisioningFailed` | infra | GPU node cannot be provisioned (covers both initial provisioning and runtime re-provisioning after a previously Ready node is reclaimed or fails):<br>- quota exceeded<br>- instance type unavailable<br>- zone capacity<br>- subscription not registered | NodeClaim / Karpenter events; KAITO `Workspace` conditions |
| `inferencesetModelPodsNotReady` | workload | Same Reason regardless of whether the pod has ever reached Ready:<br>- **Install-time:** `ImagePullBackOff` on base image, model-weights pull failure, `InsufficientGPU`, PVC bind failure<br>- **Runtime:** `OOMKilled`, `CrashLoopBackOff`, readiness-probe regression, or eviction of a previously Ready pod | Pod `status.containerStatuses[*].state` + `restartCount`; KAITO-owned `Deployment.status` (continuously watched) |
| `inferencesetEPPNotReady` | control | - **Install-time:** EPP image pull failure, malformed `ConfigMap`, RBAC missing for list pods, `--pool-name` mismatch<br>- **Runtime:** EPP crash / restart loop / readiness-probe regression after the pod was previously Ready | EPP `Deployment.status.conditions` + Pod state + events (continuously watched) |
| `inferencesetRouteNotReady` | control | Any of these, either at install-time or as a runtime regression:<br>- `HTTPRoute` parent `Gateway` missing<br>- `ResolvedRefs=False`<br>- `InferencePool` selector matches zero pods<br>- parent `Gateway` deleted post-install or `InferencePool` selector drifts | `HTTPRoute.status.parents`, `InferencePool.status` (continuously watched) |
| `inferencesetDependencyMissing` | precondition | Lower-layer prerequisites not satisfied:<br>- Cluster level: `ConfigMap/productionstack-status` reports `Ready=False`<br>- Modelharness level: `ConfigMap/modelharness-status` in the workload namespace reports `Ready=False` | reconcile-time lookup of the two aggregated ConfigMaps |
| `inferencesetScalingMisconfigured` | config | One of:<br>- `enableScaling=true` with `maxReplicas < replicas`<br>- threshold ≤ 0<br>- keda-kaito-scaler absent | Helm `values.schema.json` + reconcile-time validation |
| `inferencesetReady` (positive; reuses upstream KAITO `kaitov1alpha1.InferenceSetReady`) | — | All of the above clear | aggregator |

Each `Reason` corresponds to an anchor in **TSG-1** under §modeldeployment. `inferencesetDependencyMissing` is the explicit cross-link to lower layers — a modeldeployment never re-derives cluster or modelharness root cause, it points the operator to the lower-layer aggregated status (still within TSG-1, but in the cluster/modelharness sections).

##### 1.4 Aggregation and propagation

The existing upstream `InferenceSetReady` condition is the **single rolled-up readiness signal** operators consume from `InferenceSet.status`:

```
InferenceSetReady = InferenceSetEPPReady
                  ∧ InferenceSetRouteReady
                  ∧ InferenceSetDependenciesReady
                  ∧ ∀ws ∈ ownedWorkspaces : (NodeClaimReady(ws) ∧ ResourceReady(ws) ∧ InferenceReady(ws))
```

Any conjunct flipping to `False` flips `InferenceSetReady` to `False`; the surfaced `Reason` is chosen by the priority chain below. Operators never need to consult sub-Types to detect a failure, only to diagnose it.

**Modeldeployment-layer condition Types** (on `InferenceSet.status.conditions[]`):

| `Type` | Source of truth |
| --- | --- |
| `InferenceSetEPPReady` | EPP `Deployment.status.conditions` + Pod state |
| `InferenceSetRouteReady` | `HTTPRoute.status.parents` + `InferencePool.status` |
| `InferenceSetDependenciesReady` | cluster `ConfigMap/productionstack-status` + namespace `Gateway.status` |
| `InferenceSetReady` (reuses upstream KAITO `kaitov1alpha1.InferenceSetConditionTypeReady`) | logical AND defined above |

Infra and model-pod sub-signals are read directly from the owned `Workspace` CR's existing `NodeClaimReady` / `ResourceReady` / `InferenceReady` rather than mirrored as standalone Types here (see Alternatives).

The aggregating controller is the existing KAITO `InferenceSet` reconciler. `charts/modeldeployment` stamps `kaito.sh/inferenceset: <name>` and `kaito.sh/owned-by: modeldeployment` on EPP `Deployment` and `HTTPRoute` so the reconciler can watch them without changing CRD ownership.

**Priority order per layer.** When more than one signal is unhealthy, the surfaced `Reason` is selected deterministically by the chain below. The pattern is uniformly: cross-layer dependency first → fail-fast config validation → install-order root cause → request-path order.

| Layer | Priority chain (highest first) |
| --- | --- |
| Modeldeployment | `inferencesetDependencyMissing` > `inferencesetScalingMisconfigured` > `inferencesetInfraProvisioningFailed` > `inferencesetModelPodsNotReady` > `inferencesetRouteNotReady` > `inferencesetEPPNotReady` |
| Cluster | `clusterCRDMissing` > `clusterIstioControlPlaneNotReady` > `clusterGatewayAuthNotReady` > `clusterBBRNotReady` > `clusterKaitoControllerNotReady` > `clusterNodeProvisionerNotReady` > `clusterKedaNotReady` > `clusterKedaKaitoScalerNotReady` |
| Modelharness | `modelharnessDependencyMissing` > `modelharnessGatewayClassMissing` > `modelharnessGatewayProgrammingFailed` > `modelharnessExtAuthzProviderMissing` > `modelharnessAPIKeyReconcileFailed` > `modelharnessCatchAllFilterRejected` > `modelharnessNetworkPolicyMisconfigured` |

Notes on the modeldeployment chain: `inferencesetInfraProvisioningFailed` precedes `inferencesetModelPodsNotReady` because no pod schedules until a NodeClaim succeeds; `inferencesetRouteNotReady` precedes `inferencesetEPPNotReady` because `HTTPRoute` matching runs upstream of the EPP ext_proc on the request path. `inferencesetInfraProvisioningFailed` fires when any owned `Workspace` has `NodeClaimReady=False`; `inferencesetModelPodsNotReady` fires when `ResourceReady=False` or `InferenceReady=False` while `NodeClaimReady=True`. `inferencesetRouteNotReady` and `inferencesetEPPNotReady` are the negations of their condition Types above.

#### 2. Data-plane errors

Data-plane errors are everything an HTTP client can observe. They are standardised onto one OpenAI-compatible envelope regardless of which layer produced them.

##### 2.1 Unified OpenAI-compatible error envelope

```json
{
  "error": {
    "type":    "invalid_request_error" | "authentication_error" | "service_unavailable" | "internal_error",
    "code":    "<stable string from §2.2–2.4>",
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

##### 2.2 Cluster level

Cluster-level data-plane errors come from cluster-wide ext_proc / ext_authz filters. They affect **every** namespace until remediated. Owned by `charts/productionstack`.

| HTTP | `code` | `x-kaito-error-source` | Trigger |
| --- | --- | --- | --- |
| 502 | `ext_authz_unavailable` | `authz` | `llm-gateway-auth` ext_authz Deployment unreachable or returning 5xx; envelope produced by the cluster-wide `local_reply` mapping the `ext_authz_error` response flag, with `response_headers_to_add` setting source to `authz` (the at-fault component) |
| 502 | `bbr_unavailable` | `bbr` | BBR ext_proc filter unreachable / errored; envelope produced by the cluster-wide `local_reply` mapping the `ext_proc_error` response flag, with `response_headers_to_add` setting source to `bbr`. `body-based-routing` itself is consumed unmodified from upstream (see §2.5) |
| 500 | `mesh_config_invalid` | `gateway` | `MeshConfig.extensionProviders` references unknown ext_authz/ext_proc cluster; Envoy aborts filter chain build |

##### 2.3 Modelharness level

Modelharness-level data-plane errors come from the per-namespace Gateway and the catch-all `EnvoyFilter`. Owned by `charts/modelharness`.

| HTTP | `code` | `x-kaito-error-source` | Trigger |
| --- | --- | --- | --- |
| 401 | `invalid_api_key` | `authz` | One of:<br>- `Authorization` missing<br>- token does not match any `APIKey` Secret resolvable from the host subdomain<br>- token is syntactically malformed<br><br>Emitted in-process by `llm-gateway-auth` (`internal/authz/apikey/server.go` `denyResponse`) as a `DeniedHttpResponse` carrying the OpenAI envelope and `x-kaito-error-source: authz` in `Headers` |
| 403 | `api_key_disabled` | `authz` | One of:<br>- Valid `APIKey` resolved but not authorized for this gateway namespace (today's `codes.PermissionDenied` branch in `apikey-authz`)<br>- the `APIKey` CR is explicitly marked disabled<br><br>Same in-process emitter as `invalid_api_key`, but with HTTP status `403` instead of the current hard-coded `401`. Requires the upstream `llm-gateway-auth` change in §3.2 to actually surface 403 (today `apikey-authz` collapses every deny to 401) |
| 400 | `invalid_request_body` | `bbr` | Body fails BBR parsing:<br>- not valid JSON<br>- not OpenAI chat-completions schema<br>- missing `model`<br><br>BBR signals an ext_proc body-parse failure; the chart-rendered cluster-level `local_reply` renders the envelope and sets `x-kaito-error-source: bbr` (the at-fault component) |
| 404 | `model_not_found` | `gateway` | `X-Gateway-Model-Name` present but no `HTTPRoute` in this namespace matches |
| 502 | `gateway_dataplane_unhealthy` | `gateway` | namespace `Gateway` pod has zero ready replicas; reported by upstream HC, mapped through a local_reply on the parent listener |
| 503 | `gateway_not_programmed` | `gateway` | namespace `Gateway` exists but `status.conditions[Programmed]=False`; emitted by a temporary direct-response while the harness is still being installed |

##### 2.4 Modeldeployment level

Modeldeployment-level data-plane errors come from EPP and the upstream inference pod. Owned by `charts/modeldeployment` (and upstream EPP).

| HTTP | `code` | `x-kaito-error-source` | Trigger |
| --- | --- | --- | --- |
| 502 | `epp_unavailable` | `epp` | EPP ext_proc unreachable / errored; Envoy filter-chain failure mapped via a chart-rendered `local_reply` on the EPP-targeted cluster, with `response_headers_to_add` setting source to `epp` |
| 500 | `epp_internal_error` | `epp` | EPP returned a non-routing error / 5xx to Envoy (panic, scheduler bug); the same chart-rendered `local_reply` maps `UpstreamProtocolError` / ext_proc upstream 5xx to this `code` and source `epp` |
| 503 | `model_unavailable` | `epp` | `HTTPRoute` matched but `InferencePool` has zero ready endpoints. The request path cannot distinguish initial warm-up from crash/OOM/eviction — both surface as Envoy `no_healthy_upstream` on the EPP-targeted cluster and are rewritten by a chart-rendered `local_reply` to this envelope with source `epp`. The operator-visible root cause lives on `InferenceSet.status.conditions[].Reason`:<br>- `inferencesetModelPodsNotReady`<br>- `inferencesetInfraProvisioningFailed`<br>- `inferencesetEPPNotReady`<br><br>The client retries on `Retry-After` regardless (see §2.6). |
| 504 | `upstream_timeout` | `inferenceset` | inference pod did not respond within the route timeout; the chart-rendered `local_reply` reports the inference pod (not EPP) as the at-fault component |
| pass-through | (preserved) | `inferenceset` | any non-error or vLLM-native error (e.g. `429` rate limit) is passed through unchanged, only the `x-kaito-error-source: inferenceset` header is added by a chart-rendered Envoy response-header filter on the upstream cluster |

##### 2.5 Fixing the "BBR outage looks like 404" bug

Today, the BBR ext_proc filter is shipped with the default Envoy ext_proc `failure_mode_allow=true` semantics, so when the BBR pod is unavailable the filter is skipped, the `X-Gateway-Model-Name` header is never set, and the request falls through to the namespace's `model-not-found-direct` `EnvoyFilter` — appearing to clients as `404 model_not_found`. The same trap exists for `llm-gateway-auth` ext_authz when it fails open.

The fix is three-pronged:

| Layer | Change | Effect |
| --- | --- | --- |
| Cluster filters (`charts/productionstack/charts/body-based-routing`, `llm-gateway-auth`) | Set `failure_mode_allow: false` on BBR ext_proc and on ext_authz | An unavailable cluster-level filter produces a hard failure at the Envoy filter chain instead of silently dropping the header / skipping auth. |
| Cluster `local_reply` (umbrella chart) | Render a cluster-wide `EnvoyFilter` mapping `ext_proc_error` / `ext_authz_error` response flags to the OpenAI envelope with `code: bbr_unavailable` / `ext_authz_unavailable` and `x-kaito-error-source: bbr` / `authz` (via `response_headers_to_add`) | Neither `body-based-routing` nor `llm-gateway-auth` needs to produce the envelope itself — the chart owns the YAML; the header value names the at-fault component. |
| Modelharness catch-all `EnvoyFilter` (`charts/modelharness`) | Split the existing single direct-response match into two:<br>- `x-gateway-model-name` **absent** → `502 bbr_unavailable` (defence in depth)<br>- `x-gateway-model-name` **present** and no `HTTPRoute` matched → `404 model_not_found` | Closes the same 404-disguise bug at the namespace edge in case the cluster-level `local_reply` did not fire. |

##### 2.6 Distinguishing "no ready endpoints" from "not found"

`charts/modeldeployment` always renders an `HTTPRoute` for the model name, regardless of whether the `InferencePool` currently has ready endpoints. We exploit this:

- If `X-Gateway-Model-Name` matches no `HTTPRoute` → `404 model_not_found` (modelharness catch-all). The model name is not declared in this cluster.
- If it matches an `HTTPRoute` but EPP returns no endpoint (`InferencePool` empty / all NotReady) → Envoy's `no_healthy_upstream` is rewritten by a chart-rendered `local_reply` on the EPP-targeted cluster to `503 model_unavailable` with `Retry-After: 10` and the OpenAI envelope (the body is templated at chart-render time; the model name is taken from the `HTTPRoute`-level `local_reply` template variable `%REQ(:authority)%` or the static chart value).

**Why one code covers multiple root causes.** "Zero ready endpoints" can mean the model is still warming up (scale-from-zero, image pull, weights download), or that previously-ready pods just crashed (`CrashLoopBackOff`, `OOMKilled`), or that the underlying node was evicted. The request path cannot tell these apart — all three surface as Envoy `no_healthy_upstream`. The `code` is therefore deliberately root-cause-neutral, because the correct client behaviour for all three is identical (back off on `Retry-After` and retry). The operator-facing root cause is not lost: it surfaces on `InferenceSet.status.conditions[].Reason` per §1.3 (`inferencesetInfraProvisioningFailed` / `inferencesetModelPodsNotReady` / `inferencesetEPPNotReady`), and TSG-2's `model_unavailable` entry directs the operator to inspect that field. Alternatives that would inject root-cause discrimination on the request path (an EPP code change, a sidecar reading `InferenceSet.status`) are rejected — see Alternatives.

#### 3. Required code / chart changes

This section is a review-friendly summary of every code and chart change implied by §1 and §2. Implementation specifics (exact file paths, line numbers, RBAC verbs, struct shapes) are left to the implementing PRs. §3.1 covers control-plane plumbing (status aggregation), §3.2 covers data-plane plumbing (envelope rendering), and §3.3 covers cross-cutting artefacts (shared schema, metrics, TSGs).

##### 3.1 Control-plane changes

| Component | Change | Purpose |
| --- | --- | --- |
| `charts/productionstack` | New `productionstack-status-reporter` Deployment (HA, leader-elected, read-only against the API server, no CRD) that publishes:<br>- `ConfigMap/productionstack-status` (cluster)<br>- `ConfigMap/modelharness-status` (per workload namespace, discovered by label selector) | Single owner of both ConfigMap aggregations defined in §1.1 and §1.2; continuously reconciles every Reason via informer watches so install-time misconfig and post-install drift are detected uniformly. |
| All CRD-dependent components (KAITO controller, EPP, keda-kaito-scaler, status reporter) | At startup, poll for required CRDs for a bounded interval; exit non-zero on timeout and rely on the Pod restart policy. | Replaces a Helm pre-install hook; pairs with the reporter's continuous `clusterCRDMissing` aggregation. |
| `charts/modelharness` | - Render an initially-empty `ConfigMap/modelharness-status`<br>- Stamp `kaito.sh/owned-by: modelharness` onto every harness-owned object<br>- Add `values.schema.json` for schema-level validation (e.g. non-empty `gatewayClassName`, `networkPolicy.allowedIngressNamespaces`) | Lets the reporter discover and roll up harness state via a single label selector per namespace. |
| `charts/modeldeployment` | - Stamp `kaito.sh/inferenceset` and `kaito.sh/owned-by: modeldeployment` onto EPP `Deployment`/`Service`, `HTTPRoute`, `InferencePool`, `ConfigMap`<br>- Add `values.schema.json` for schema-level validation (e.g. `maxReplicas >= replicas`, non-empty `model`, positive `scalingThreshold`) | Lets the KAITO `InferenceSet` reconciler watch the chart-owned objects without changing CRD ownership. |
| KAITO `InferenceSet` controller (upstream PR) | - Add the new condition Types and Reason constants from §1.3/§1.4<br>- Extend the reconciler to watch the chart-owned objects and the two aggregated ConfigMaps<br>- Replace the current single-boolean aggregation with the per-subsystem priority chain | Turns the existing single `Ready` boolean into the structured taxonomy required by FR1/FR2 without introducing new CRDs. `InferenceSetInfraReady` / `InferenceSetModelPodsReady` are intentionally **not** added — those signals live on the owned `Workspace` CR and are read directly when picking the aggregate Reason. |

##### 3.2 Data-plane changes

| Component | Change | Purpose |
| --- | --- | --- |
| `charts/productionstack` (incl. `charts/body-based-routing`) | - Set `failure_mode_allow: false` on BBR<br>- Render a cluster-wide `EnvoyFilter` that maps `ext_authz_error` / `ext_proc_error` response flags to the OpenAI envelope with `x-kaito-error-source: authz` / `bbr` | Single chokepoint for cluster-level error normalisation (§2.2, §2.5). Closes the "BBR/ext_authz outage looks like 404" bug. |
| `llm-gateway-auth` (upstream PR — sibling repo) | - Rewrite both deny builders (`apikey`, `azure`) to emit the OpenAI envelope and the `x-kaito-error-source: authz` header<br>- Map gRPC codes to correct HTTP status (`PermissionDenied` → `403 api_key_disabled`, otherwise `401 invalid_api_key`)<br>- Set `failure_mode_allow: false` in its own chart | Required because Envoy `local_reply` cannot differentiate per-deny gRPC codes / body text. Scope is narrowly the deny path; outage path stays in the cluster `local_reply`. |
| `charts/modelharness` | - Split the catch-all `EnvoyFilter` per §2.5 (`X-Gateway-Model-Name` absent → `502 bbr_unavailable`, present + no route → `404 model_not_found`)<br>- Add namespace-scoped `local_reply` overrides for `gateway_dataplane_unhealthy` and `gateway_not_programmed` | Closes the same 404-disguise bug at the namespace edge and surfaces install-time gateway data-plane errors with a stable `code`. |
| `charts/modeldeployment` | Render an `EnvoyFilter` on the EPP-targeted cluster that maps EPP-path Envoy response flags to the OpenAI envelope with the right `x-kaito-error-source`:<br>- `no_healthy_upstream` → `503 model_unavailable` / `epp`<br>- ext_proc filter error → `502 epp_unavailable` / `epp`<br>- upstream 5xx → `500 epp_internal_error` / `epp`<br>- route timeout → `504 upstream_timeout` / `inferenceset`<br>- pass-through responses: add `x-kaito-error-source: inferenceset` | Realises §2.4 and §2.6 entirely at the chart layer, with **no** patches to `llm-d-inference-scheduler` (consumed unmodified upstream). |

##### 3.3 Cross-cutting artefacts

- **Shared schema** — `pkg/errors/` (new, in this repo) owns the canonical Go types for both the envelope (consumed in-process by `llm-gateway-auth`) and the Reason enum (consumed by the KAITO `InferenceSet` controller and the status reporter). A single Helm sub-template (`_envelope.tpl` in `charts/productionstack`, consumed by the other charts via chart deps) renders the same JSON into every Envoy `local_reply_config`. A CI snapshot test in `pkg/errors` compares the marshalled Go struct against the rendered chart YAML to prevent drift.
- **Metrics** — A single Prometheus series, `productionstack_request_errors_total{level,source,code,model,namespace}`, covers every data-plane error path. `llm-gateway-auth` increments it in-process from its deny builders; every other path is populated by a Prometheus recording rule that projects Envoy's built-in `envoy_http_local_reply_count` / `envoy_http_downstream_rq_xx` onto the same label set, keyed off a `response_code_details` stat tag rendered by each `local_reply_config`. Each control-plane Reason transition emits a Kubernetes `Event` on the owning resource.
- **TSGs** — `docs/tsg/control-plane-errors.md` (TSG-1) and `docs/tsg/data-plane-errors.md` (TSG-2) each have three sections (cluster / modelharness / modeldeployment) with one entry per Reason / code. Every control-plane `message` ends with `See …/control-plane-errors#<Reason>`; every data-plane response body ends with `See …/data-plane-errors#<code>`.

### User Stories

#### Story 1 — Operator diagnoses a stuck modeldeployment

An operator installs a modeldeployment in a region where the requested `instanceType` has zero quota. They run:

```sh
kubectl describe inferenceset qwen -n my-models
```

and see:

```
Status:
  Conditions:
    Type:    InferenceSetReady
    Status:  False
    Reason:  inferencesetInfraProvisioningFailed
    Message: GPU node provisioning failed: quota exceeded for Standard_NV36ads_A10_v5 in eastus.
```

No other resource needs to be inspected to locate root cause. The `Reason` (`inferencesetInfraProvisioningFailed`) is the stable key the operator looks up in TSG-1.

#### Story 2 — Operator diagnoses a broken namespace (modelharness)

An operator installed `charts/modelharness` but `llm-gateway-auth` is not installed cluster-wide. They run:

```sh
kubectl get cm modelharness-status -n my-models -o yaml
```

and see the harness ConfigMap pointing directly at the cluster layer as the root cause, instead of re-classifying the cluster outage as a harness-local problem:

```yaml
data:
  ready: "false"
  reason: "modelharnessDependencyMissing"
  message: |
    Cluster-level prerequisite not ready (upstream reason=clusterGatewayAuthNotReady).
    See ConfigMap/productionstack-status in kaito-system.
  components: |
    gateway:             Ready
    authorizationPolicy: Blocked (modelharnessDependencyMissing: cluster clusterGatewayAuthNotReady)
    apiKey:              Blocked (modelharnessDependencyMissing: cluster clusterGatewayAuthNotReady)
    catchAllFilter:      Ready
    networkPolicy:       Ready
```

A single look at this one ConfigMap tells the operator two things: (a) the harness itself has no local misconfiguration, and (b) the cluster layer is the place to remediate. Had the operator instead incorrectly edited the rendered `AuthorizationPolicy` to reference a wrong provider name, the same ConfigMap would report `reason: modelharnessExtAuthzProviderMissing` — a harness-local Reason that is fixed by re-applying the chart.

#### Story 3 — Operator diagnoses a broken cluster install

An operator installs the `productionstack` umbrella chart but BBR cannot start (image pull failure on the air-gapped cluster). The umbrella's post-install Job writes a summary into `ConfigMap/productionstack-status` in `kaito-system`:

```sh
kubectl get cm productionstack-status -n kaito-system -o yaml
```

```yaml
data:
  ready: "false"
  reason: "clusterBBRNotReady"
  message: |
    body-based-routing deployment has 0/2 ready replicas: ImagePullBackOff on bbr container.
  components: |
    body-based-routing: NotReady (clusterBBRNotReady)
    keda-kaito-scaler:  Ready
    llm-gateway-auth:   Ready
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
             "message": "model(qwen) has no ready replicas; see InferenceSet.status for root cause" } }
```

#### Story 5 — BBR outage no longer disguised as 404

When BBR's ext_proc pod is unavailable (cluster-level filter outage), the Istio Gateway returns a structured `502` with `code: bbr_unavailable` and `x-kaito-error-source: bbr` instead of falling through to the namespace catch-all `model_not_found`. Both the operator (via metrics + cluster-level condition) and the client (via response body) can tell BBR is at fault. The same disambiguation applies when `llm-gateway-auth` ext_authz is unavailable (`502 ext_authz_unavailable`, `x-kaito-error-source: authz`). In both cases the envelope is rendered by the cluster-wide chart `local_reply_config`; neither component is patched.

### Requirements

#### Functional Requirements

##### FR1 — Per-layer aggregated status

Every deployment-time failure MUST be aggregated onto a single Kubernetes resource per layer, within one reconcile loop:

- **Cluster level**: `ConfigMap/productionstack-status` in the umbrella chart's release namespace, written and continuously refreshed by the `productionstack-status-reporter` `Deployment` shipped with the umbrella chart (no Helm post-install Job). Carries `Ready` plus per-subchart conditions for `body-based-routing`, `keda-kaito-scaler`, `llm-gateway-auth`, KAITO controller readiness, and the presence of required CRDs from Gateway API (`Gateway`, `HTTPRoute`), GAIE (`InferencePool`, `InferenceObjective`), KAITO (`InferenceSet`, `Workspace`, `APIKey`), Istio (`EnvoyFilter`, `DestinationRule`, `AuthorizationPolicy`), and KEDA (`ScaledObject`, `ClusterTriggerAuthentication`).
- **Modelharness level**: a single `ConfigMap/modelharness-status` in the workload namespace, written and continuously refreshed by the same `productionstack-status-reporter` Deployment. The native Gateway API conditions on the namespace `Gateway` (`Accepted`, `Programmed`, `ResolvedRefs`) remain authoritative for Gateway-related reasons and are mirrored into the aggregated ConfigMap; non-Gateway resources (catch-all `EnvoyFilter`, `APIKey`, `NetworkPolicy`) are aggregated onto the same ConfigMap via the reporter's namespace-scoped watches.
- **Modeldeployment level**: `InferenceSet.status.conditions` (see §1.3).

No new CRDs are introduced.

##### FR2 — Closed `Reason` vocabulary

Conditions across all three layers MUST use one of the `Reason` strings enumerated in §1.1–§1.3. All `Reason` strings are camelCase with a layer prefix for cross-layer disambiguation: cluster layer uses `cluster*`, modelharness layer uses `modelharness*`, modeldeployment layer uses `inferenceset*` (matching KAITO upstream's existing `inferencesetReady` / `inferencesetNotReady` convention). Adding a new `Reason` requires updating TSG-1 (control-plane errors) in the same PR; adding a new data-plane `code` requires updating TSG-2 (data-plane errors) in the same PR.

##### FR3 — OpenAI-compatible error envelope

Every data-plane error response MUST carry the body shape:

```json
{ "error": { "type": "<string>", "code": "<string>", "message": "<string>", "param": "<string|null>" } }
```

and the header `x-kaito-error-source`, regardless of which layer (cluster filter, namespace gateway, modeldeployment EPP) produced the error.

##### FR4 — Cluster-filter outage disambiguation

When a cluster-level filter (BBR ext_proc, `llm-gateway-auth` ext_authz) is unavailable, the Gateway MUST return a distinct `502 bbr_unavailable` / `502 ext_authz_unavailable`, not the namespace catch-all `404 model_not_found`.

##### FR5 — No-ready-endpoint disambiguation

When a request's model name matches a known `HTTPRoute` but the backing `InferencePool` has zero ready endpoints — regardless of root cause (initial warm-up, `CrashLoopBackOff`, `OOMKilled`, eviction, scaled-to-zero) — the Gateway MUST return `503 model_unavailable` with a `Retry-After` header, and MUST NOT collapse this state onto `404 model_not_found`. The request-path `code` is intentionally root-cause-agnostic (the client retries identically in every case); operator-facing root cause is surfaced on `InferenceSet.status.conditions[].Reason` per §1.3.

#### Non-Functional Requirements

##### NFR1 — Observability

Every data-plane error path MUST be observable on the unified Prometheus series `productionstack_request_errors_total{level,source,code,model,namespace}`, where `level ∈ {cluster, modelharness, modeldeployment}`. The series is populated by two producers:

| Producer | Paths | How the counter is populated |
| --- | --- | --- |
| In-process | `llm-gateway-auth` deny path (`401 invalid_api_key`, `403 api_key_disabled`) — the only request-path component with native production-stack code | Directly increments the counter from its deny builders with `level="modelharness", source="authz"`. |
| Chart-emitted (Envoy `local_reply`) | every other path: cluster-level `bbr_unavailable` / `ext_authz_unavailable`; modelharness-level `model_not_found` / `gateway_*` / `invalid_request_body`; modeldeployment-level `epp_*` / `model_unavailable` / `upstream_timeout` | A Prometheus **recording rule** joins Envoy's built-in `envoy_http_local_reply_count{response_code_details, response_code}` and `envoy_http_downstream_rq_xx` series and projects them onto the unified label set. Envoy filters do **not** increment custom Prometheus counters — the recording rule is the contract. |

Label sourcing:

- `response_code_details` (or an equivalent custom stat tag) MUST be rendered on every `local_reply_config` so the recording rule can reliably map an Envoy stat row back to the `code` / `source` / `level` triple.
- `namespace` is taken from the Envoy listener.
- `model` is best-effort (extracted from `X-Gateway-Model-Name` when present, empty otherwise — paths that fail before BBR ran will have an empty `model` label, which is acceptable for TSG triage).

Each control-plane-error `Reason` transition MUST emit a Kubernetes `Event` on the owning resource (`ConfigMap/productionstack-status` for cluster, `ConfigMap/modelharness-status` for modelharness, `InferenceSet` for modeldeployment).

##### NFR2 — Backwards compatibility

The OpenAI error envelope MUST be a superset of what clients see today. HTTP status codes for already-handled cases (`401`, existing `404 model_not_found`) MUST not regress.

##### NFR3 — No new control-plane components

The design MUST be implementable by editing existing charts, the KAITO `InferenceSet` controller, `llm-gateway-auth`, and EPP. The only new runtime addition is the `productionstack-status-reporter` `Deployment` (2 replicas with lease-based leader election, read-only against the API server plus patch-only on its own `ConfigMap` and lease); no new CRD, operator, sidecar, or Helm hook Job is introduced.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Aggregation requires upstream KAITO changes that may slip release windows. | The modeldeployment chart can ship the label conventions immediately; the controller change is additive and gated behind the labels, so older controllers degrade gracefully to today's behaviour. |
| `failure_mode_allow: false` on BBR / ext_authz turns silent outages into hard 502s, increasing visible error rate during incidents at the cluster level. | This is intentional — visibility is the goal. Operators get a clear signal, and the umbrella chart documents BBR and `llm-gateway-auth` as critical-path components in TSG-1. |
| The OpenAI envelope changes response shape for some current paths. | Status codes are preserved; only body and headers are added/normalised. We will validate against the production-stack E2E suite before release. |
| Multiple simultaneous failures could lead to confusing aggregated `Reason`. | A fixed priority order per layer (§1.4) makes the surfaced `Reason` deterministic; the per-subsystem conditions remain available for full detail. |
| Cluster-level `ConfigMap/productionstack-status` written by the `productionstack-status-reporter` Deployment adds a small new runtime component. | Reporter runs HA (2 replicas, leader-elected) and is read-only against the API server (patch-only on its own `ConfigMap` and `Lease`). Even with both replicas down, only the ConfigMap goes stale; the data path is unaffected. The reporter is mandatory because `ConfigMap/modelharness-status` and `inferencesetDependencyMissing` consume it as a hard dependency. |
| Components depending on CRDs may crash-loop while the required CRDs are still being installed (each component poll-then-startup-timeout-exits instead of using a Helm pre-install hook). | Intentional and self-healing: Kubernetes restarts the Pod and the component starts cleanly once all CRDs are registered. The crash-loop window is short (seconds) and visible via `kubectl get pods` plus the reporter's aggregated `clusterCRDMissing` reason. |
| KAITO's `gatewayAPIInferenceExtension` feature gate, if enabled, makes KAITO and `charts/modeldeployment` fight over the same `InferencePool` / `HTTPRoute`, causing `InferenceSetRouteReady` to oscillate. | The umbrella chart MUST NOT pass any flag that enables the gate; the default-`false` value in [`kaito/pkg/featuregates/featuregates.go`](https://github.com/kaito-project/kaito/blob/main/pkg/featuregates/featuregates.go) is treated as a contract. TSG-1 lists "check that `gatewayAPIInferenceExtension` is `false` on the KAITO controller" as the first remediation step under `inferencesetRouteNotReady`. |
| Modelharness-level `gateway_not_programmed` direct-response could leak during normal install if it fires before harness reconciliation completes. | The direct-response is bound to a short-lived listener filter that is removed automatically once `Gateway.status.conditions[Programmed]=True`. |

## Alternatives

- **Add `InferenceSetInfraReady` / `InferenceSetModelPodsReady` condition Types on `InferenceSet`.** Rejected: the owned `Workspace` CR already exposes `NodeClaimReady` (≈ infra), `ResourceReady` (≈ model pods scheduling / PVC / image), and `InferenceReady` (≈ inference service health), and KAITO already rolls those into `WorkspaceConditionTypeSucceeded` which the `InferenceSet` reconciler counts toward `readyReplicas`. Mirroring them as standalone InferenceSet Types would only duplicate that signal. What is missing today is disambiguation in the `Reason` field (the aggregate is a single `inferencesetNotReady` with message `N/M replicas are ready`, regardless of NodeClaim quota vs `CrashLoopBackOff`); the §1.3 Reasons close that gap by scanning the owned Workspaces' sub-conditions on the existing `InferenceSetReady` condition. EPP, `HTTPRoute`, and cross-layer dependencies are not covered by any `Workspace` sub-condition, so they get their own Types in §1.4.
- **Discriminate `model_unavailable` root cause on the request path** (patch `llm-d-inference-scheduler` to emit a more specific `code`, or add a sidecar that reads `InferenceSet.status` and rewrites the response). Rejected: production-stack consumes `llm-d-inference-scheduler` as an unmodified upstream binary, and a status-reading sidecar adds a failure domain on the hot path. Every "zero ready endpoints" sub-cause (warm-up / crash / OOM / eviction) demands the same client behaviour (back off on `Retry-After` and retry), so a discriminated `code` would not change what clients do. Operator-facing root cause is preserved on `InferenceSet.status.conditions[].Reason` (§1.3); the chart-rendered `local_reply_config` sets `x-kaito-error-source: epp` via `response_headers_to_add` so the at-fault component is still named on the response.
- **One proposal per layer (cluster / modelharness / modeldeployment).** Rejected: the three layers share the same envelope, the same `x-kaito-error-source` header, and the same TSG cross-link conventions; splitting them would force triplicate boilerplate and risk divergence. The two-category (control-plane / data-plane) structure keeps the cross-layer invariants visible while preserving per-layer ownership.
- **New `ModelDeployment` / `Modelharness` / `ProductionStack` CRDs + aggregator controllers.** Rejected: introduces three new control-plane components and three new reconciliation loops without giving operators anything they cannot already get by extending existing status fields (`InferenceSet.status`, `Gateway.status`) and a single ConfigMap.
- **Helm test hook / CLI plugin for status aggregation.** Rejected: only works at install time and cannot reflect drift; operators still have to inspect lower-layer objects after the fact.
- **Per-component error formats with a thin translation proxy.** Rejected: adds latency and another failure domain on the request path; the cluster-level Envoy `local_reply` (§2.2 / §2.5) achieves the same outcome at zero extra hops and normalises every layer in one place.
- **`failure_mode_allow: true` on BBR with a sentinel header to detect skip.** Rejected: relies on filter ordering invariants and is harder to reason about than a hard fail.

## Upgrade Strategy

- The chart changes are additive: new labels, new `values.schema.json`, new `EnvoyFilter` match conditions, and the new `productionstack-status-reporter` Deployment in the umbrella chart. Existing values files continue to install.
- The new `ConfigMap/productionstack-status` is **upgrade-safe by construction**:
  - The umbrella chart renders it with standard Helm ownership annotations (`meta.helm.sh/release-name`, `meta.helm.sh/release-namespace`, `app.kubernetes.io/managed-by: Helm`).
  - The reporter only patches the `data` keys (`ready`, `reason`, `message`, `components`) — never the metadata — so Helm adopts the resource in place on upgrade (no delete + recreate) and the reporter refreshes `data` on next reconcile.
  - The ConfigMap is always rendered, because `ConfigMap/modelharness-status` and `inferencesetDependencyMissing` read it as a hard dependency.
- The `InferenceSet.status` schema extension adds conditions; consumers that only read `status.readyReplicas` are unaffected.
- The OpenAI envelope changes the body of error responses but preserves status codes; clients that key off status codes are unaffected. Clients parsing the response body must already handle both empty and non-OpenAI bodies (current behaviour) and will see a strict subset of shapes after the change.
- Operators upgrading must bump `charts/productionstack`, `charts/modelharness`, and `charts/modeldeployment` together; the umbrella chart's version bump will be the gate. The recommended upgrade order is cluster → modelharness → modeldeployment, mirroring the install order in [README.md](../../README.md).

## Additional Details

### Test Plan

**Unit tests**

| Package | Scenarios |
| --- | --- |
| `pkg/errors` | envelope marshalling; `Reason` enum stability across all three layer constants |
| Modeldeployment aggregator | each priority transition in §1.4, including `inferencesetDependencyMissing` cross-link |
| `productionstack-status-reporter` | each `Reason` (subchart NotReady, CRD missing, `MeshConfig` provider missing) |
| CRD startup gate | every CRD-dependent component (KAITO controller, EPP, keda-kaito-scaler, status reporter) exits non-zero when a required CRD is absent and starts cleanly once it appears |

**E2E tests** (under `test/e2e/`)

| Layer | Test file | Scenario | Asserted outcome |
| --- | --- | --- | --- |
| Cluster | `cluster_status_test.go` (new) | scale `body-based-routing` Deployment to zero | `ConfigMap/productionstack-status` reports `clusterBBRNotReady` within one reporter resync |
| Cluster | `cluster_status_test.go` (new) | scale `llm-gateway-auth` Deployment to zero | reports `clusterGatewayAuthNotReady` |
| Cluster | `cluster_status_test.go` (new) | delete a required CRD | (a) reporter reports `clusterCRDMissing`; (b) CRD-dependent components exit on startup timeout and are restarted by Kubernetes until the CRD returns |
| Modelharness | `harness_status_test.go` (new) | install `charts/modelharness` while `llm-gateway-auth` is uninstalled | `ConfigMap/modelharness-status` reports `ready="false", reason=modelharnessDependencyMissing` with upstream `clusterGatewayAuthNotReady` in `message` |
| Modelharness | `harness_status_test.go` (new) | hand-edit `AuthorizationPolicy` provider name while `llm-gateway-auth` is healthy | transitions to `reason=modelharnessExtAuthzProviderMissing` |
| Modelharness | extend `network_policy_test.go` | `allowedIngressNamespaces` references a nonexistent namespace | `modelharnessNetworkPolicyMisconfigured` reported; clears when namespace is created |
| Modeldeployment | `control_plane_error_test.go` (new) | install with invalid `instanceType` | `InferenceSetReady=False, Reason=inferencesetInfraProvisioningFailed` |
| Modeldeployment | `control_plane_error_test.go` (new) | install while cluster `productionstack-status.ready="false"` | `InferenceSetReady=False, Reason=inferencesetDependencyMissing` |
| Request path | extend `apikey_auth_test.go` | normal `401` deny | OpenAI envelope + `x-kaito-error-source: authz` |
| Request path | extend `model_routing_test.go` | unknown model name | `404 model_not_found` envelope shape + `x-kaito-error-source: gateway` |
| Request path | `bbr_outage_test.go` (new) | scale BBR Deployment to zero | `502 bbr_unavailable` + `x-kaito-error-source: bbr` (not `404`) |
| Request path | `ext_authz_outage_test.go` (new) | scale `apikey-authz` Deployment to zero | `502 ext_authz_unavailable` + `x-kaito-error-source: authz` |
| Request path | `model_unavailable_test.go` (new), warm-up | `replicas=0`, send a request | `503 model_unavailable` with `Retry-After` + `x-kaito-error-source: epp`; `InferenceSet.status` Reason is `inferencesetModelPodsNotReady` (or `inferencesetInfraProvisioningFailed` if no node yet) |
| Request path | `model_unavailable_test.go` (new), crash | wait for Ready, then inject a crash-loop (`exit 1`) | same `503 model_unavailable` response shape — proves request-path code is root-cause-agnostic — while `InferenceSet.status` Reason flips to `inferencesetModelPodsNotReady` |

**Manual verification.** Each TSG-1 control-plane `Reason` and TSG-2 data-plane `code` is reachable from its corresponding `Message` / response-body URL.

## Implementation History

- [x] 2026-05-19: Proposed in [issue #71](https://github.com/kaito-project/production-stack/issues/71); initial proposal PR opened (modeldeployment-only scope)
- [x] 2026-05-21: Expanded scope to cluster + modelharness + modeldeployment; restructured under two top-level categories (control-plane / data-plane)
- [ ] TBD: Upstream code merged — shared `pkg/errors` schema, KAITO `InferenceSet` controller, and `llm-gateway-auth`
- [ ] TBD: Charts merged — `charts/productionstack` (incl. `productionstack-status-reporter`), `charts/modelharness`, `charts/modeldeployment`
- [ ] TBD: TSGs merged — TSG-1 (control-plane errors) and TSG-2 (data-plane errors)
