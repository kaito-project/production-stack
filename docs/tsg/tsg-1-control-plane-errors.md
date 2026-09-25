# TSG-1 — Production-Stack Control-Plane Errors

| | |
| --- | --- |
| **Scope** | Control-plane (in-cluster) health of a production-stack deployment — cluster bootstrap, per-namespace modelharness, and per-InferenceSet modeldeployment. |
| **Signal** | Kubernetes `Event`s published to the `kube-system` namespace by the `productionstack-status-reporter` Deployment. |
| **Audience** | Internal operators / on-call. |
| **Companion** | [TSG-2 — Data-Plane Errors](tsg-2-data-plane-errors.md) (HTTP request-path failures). |
| **Source of truth** | `pkg/productionstack-status-reporter/` (this TSG is keyed off the stable `reason` strings defined there). |

> This TSG is keyed off the closed `reason` vocabulary emitted by the reporter. Every section anchor below is the literal `reason` string, so tooling can deep-link by reason.

---

## 1. Overview

The `productionstack-status-reporter` is a single, leader-elected Deployment (shipped by `charts/productionstack`) that, on every resync, evaluates the full control-plane reason catalogue across three layers and surfaces each finding as an aggregated Kubernetes `Event` in `kube-system`. It is the **single producer** of these reasons — no other component emits them.

The reporter has **read-only** API access; it never mutates a watched resource. It only creates/updates `Event`s in `kube-system`.

**Defaults** (configurable via `productionstack-status-reporter.*` chart values):

| Setting | Default |
| --- | --- |
| Resync interval | `30s` |
| Startup grace period | `3m` |
| Istio namespace / istiod Deployment | `istio-system` / `istiod` |
| KAITO namespace / controller Deployment | `kaito-system` / `kaito-workspace` |
| BBR namespace / Deployment | `kaito-system` / `body-based-router` |
| gateway-auth namespace / Deployment | `llm-gateway-auth` / `apikey-authz` |
| KEDA namespace | `keda` (`keda-operator`, `keda-operator-metrics-apiserver`) |
| keda-kaito-scaler namespace / Deployment | `keda` / `keda-kaito-scaler` |
| Weight-download window / threshold | `60s` / `20 MB/s` |
| Weight-download metric / port | `kaito_model_download_speed_bytes_per_second` / `5000` |

## 2. Prerequisites

- `kubectl` access to the cluster with permission to read `Event`s in `kube-system` (and to `describe` the workload-namespace objects named in event messages).
- The reporter Deployment must itself be running. If it is crash-looping no events are produced — see [Reporter is not emitting any events](#reporter-is-not-emitting-any-events).

## 3. The single query

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter \
  --sort-by=.lastTimestamp
```

Filter to one reason:

```sh
kubectl get events -n kube-system \
  --field-selector source=productionstack-status-reporter,reason=<REASON> \
  --sort-by=.lastTimestamp
```

### Reading the events

- **`involvedObject` is always cluster-scoped** — a `Namespace` or a `CustomResourceDefinition`. Events in `kube-system` cannot reference namespaced resources cross-namespace, so the **specific failing resource name is in `message`**. Pivot with `kubectl describe` using the namespace + name from the message.
- **`type`**: `Warning` for every `*NotReady` / `*Failed` / `*Missing` / `*Misconfigured` / `*Rejected` / `*Slow` reason; `Normal` for the positive `*Ready` reasons (`clusterReady`, `modelharnessReady`, `inferencesetReady`).
- **Aggregation**: a repeat of the same `(reason, involvedObject)` bumps `count` + `lastTimestamp` on the existing event rather than creating a new one.
- **Priority (one primary per layer)**: when multiple reasons fire for the same object/layer, the reporter surfaces the single highest-priority one. Chains (highest → lowest):
  - **Cluster**: `clusterCRDMissing` > `clusterIstioControlPlaneNotReady` > `clusterGatewayAuthNotReady` > `clusterBBRNotReady` > `clusterKaitoControllerNotReady` > `clusterNodeProvisionerNotReady` > `clusterKedaNotReady` > `clusterKedaKaitoScalerNotReady`
  - **Modelharness**: `modelharnessGatewayClassMissing` > `modelharnessGatewayProgrammingFailed` > `modelharnessExtAuthzProviderMissing` > `modelharnessAPIKeyReconcileFailed` > `modelharnessCatchAllFilterRejected` > `modelharnessNetworkPolicyMisconfigured`
  - **Modeldeployment**: `inferencesetInfraProvisioningFailed` > `inferencesetModelPodsNotReady` > `inferencesetRouteNotReady` > `inferencesetEPPNotReady` (`inferencesetWeightDownloadSlow` is **orthogonal** — emitted in addition to the primary).
- **Cross-layer suppression**: when an upstream `cluster*` reason is active, the reporter suppresses downstream reasons that are *definitionally* dependent on it (the downstream check has no input until the upstream is healthy). The active upstream's message then carries a deterministic suffix: `(suppressing downstream reasons: <r1>, <r2>, ... in N namespace(s))`. Suppression map (code):

  | Active upstream | Suppressed downstream |
  | --- | --- |
  | `clusterIstioControlPlaneNotReady` | `modelharnessGatewayClassMissing`, `modelharnessGatewayProgrammingFailed` |
  | `clusterGatewayAuthNotReady` | `modelharnessExtAuthzProviderMissing`, `modelharnessAPIKeyReconcileFailed` |
  | `clusterKaitoControllerNotReady` | `inferencesetInfraProvisioningFailed`, `inferencesetModelPodsNotReady` |
  | `clusterNodeProvisionerNotReady` | `inferencesetInfraProvisioningFailed` |
  | `clusterCRDMissing` (`apikeys.kaito.sh`) | `modelharnessAPIKeyReconcileFailed` |
  | `clusterCRDMissing` (`inferencesets.kaito.sh`) | all `inferenceset*` reasons |
  | `clusterCRDMissing` (`gateways.gateway.networking.k8s.io`) | `modelharnessGatewayClassMissing`, `modelharnessGatewayProgrammingFailed` |
  | `clusterCRDMissing` (`httproutes...` / `inferencepools...`) | `inferencesetRouteNotReady` |

  Reasons **not** in this map (e.g. `clusterBBRNotReady`, `clusterKedaNotReady`) never suppress anything and never carry the suffix.
- **Startup grace (`3m` default)**: cluster / harness / EPP / route findings are withheld while the backing resource is younger than the grace window (object-age gating) or, for findings without a backing object, until the problem has persisted that long (debounce). Workspace / model-pod findings bypass the grace window and instead surface **only terminal failures** (see the relevant sections) so the reporter stays quiet during legitimately long GPU provisioning / weight download.

## 4. Namespace discovery

The reporter only evaluates the modelharness / modeldeployment layers for namespaces labelled `productionstack.kaito.sh/managed-by=modelharness` (stamped by `charts/modelharness`). **An unlabelled namespace is invisible to the reporter** — no `modelharness*` / `inferenceset*` events will ever be produced for it. If you expect harness/model events and see none, confirm the label first:

```sh
kubectl get ns -l productionstack.kaito.sh/managed-by=modelharness
kubectl label ns <workload-ns> productionstack.kaito.sh/managed-by=modelharness   # if missing
```

## 5. Quick reference

| Reason | Type | Layer |
| --- | --- | --- |
| [`clusterCRDMissing`](#clustercrdmissing) | Warning | Cluster |
| [`clusterIstioControlPlaneNotReady`](#clusteristiocontrolplanenotready) | Warning | Cluster |
| [`clusterGatewayAuthNotReady`](#clustergatewayauthnotready) | Warning | Cluster |
| [`clusterBBRNotReady`](#clusterbbrnotready) | Warning | Cluster |
| [`clusterKaitoControllerNotReady`](#clusterkaitocontrollernotready) | Warning | Cluster |
| [`clusterNodeProvisionerNotReady`](#clusternodeprovisionernotready) | Warning | Cluster |
| [`clusterKedaNotReady`](#clusterkedanotready) | Warning | Cluster |
| [`clusterKedaKaitoScalerNotReady`](#clusterkedakaitoscalernotready) | Warning | Cluster |
| [`clusterReady`](#clusterready) | Normal | Cluster |
| [`modelharnessGatewayClassMissing`](#modelharnessgatewayclassmissing) | Warning | Modelharness |
| [`modelharnessGatewayProgrammingFailed`](#modelharnessgatewayprogrammingfailed) | Warning | Modelharness |
| [`modelharnessExtAuthzProviderMissing`](#modelharnessextauthzprovidermissing) | Warning | Modelharness |
| [`modelharnessAPIKeyReconcileFailed`](#modelharnessapikeyreconcilefailed) | Warning | Modelharness |
| [`modelharnessCatchAllFilterRejected`](#modelharnesscatchallfilterrejected) | Warning | Modelharness |
| [`modelharnessNetworkPolicyMisconfigured`](#modelharnessnetworkpolicymisconfigured) | Warning | Modelharness |
| [`modelharnessReady`](#modelharnessready) | Normal | Modelharness |
| [`inferencesetInfraProvisioningFailed`](#inferencesetinfraprovisioningfailed) | Warning | Modeldeployment |
| [`inferencesetModelPodsNotReady`](#inferencesetmodelpodsnotready) | Warning | Modeldeployment |
| [`inferencesetRouteNotReady`](#inferencesetroutenotready) | Warning | Modeldeployment |
| [`inferencesetEPPNotReady`](#inferenceseteppnotready) | Warning | Modeldeployment |
| [`inferencesetWeightDownloadSlow`](#inferencesetweightdownloadslow) | Warning | Modeldeployment |
| [`inferencesetReady`](#inferencesetready) | Normal | Modeldeployment |

---

## 6. Cluster-layer reasons

### clusterCRDMissing

**Meaning.** A required CRD is not registered with the API server. The probed set (via the discovery API): Gateway API (`gateways`, `httproutes`), GAIE (`inferencepools`, `inferenceobjectives`), KAITO (`inferencesets`, `workspaces`, `apikeys`), Istio (`envoyfilters`, `authorizationpolicies`), KEDA (`scaledobjects`, `clustertriggerauthentications`). One event per missing CRD; `involvedObject` is the `CustomResourceDefinition`.

**Message.** `required CustomResourceDefinition <name> is not registered with the API server; install the chart that ships it.`

**Diagnose.**
```sh
kubectl get crd <name>
kubectl api-resources | grep -i <resource>
```

**Mitigate.** Install/repair the chart that owns the CRD (Gateway API, GAIE, KAITO, Istio, or KEDA). CRD-dependent components additionally poll-then-startup-timeout-exit and are restarted by Kubernetes until the CRD appears. While this reason is active, downstream reasons that need the missing CRD are suppressed (see the suppression map).

### clusterIstioControlPlaneNotReady

**Meaning.** The `istiod` control-plane Deployment is not Ready.

**Message.** `istiod control plane Deployment istio-system/istiod has <r>/<d> ready replicas: <cause>.`

**Diagnose.**
```sh
kubectl -n istio-system get deploy istiod
kubectl -n istio-system describe deploy istiod
kubectl -n istio-system get pods -l app=istiod
```

**Mitigate.** Restore istiod (image pull, resources, webhook config). Suppresses `modelharnessGatewayClassMissing` / `modelharnessGatewayProgrammingFailed` (Gateway `Accepted`/`Programmed` conditions are written by istiod) until cleared.

### clusterGatewayAuthNotReady

**Meaning.** The `llm-gateway-auth` ext_authz Deployment (`apikey-authz`) is not Ready.

**Message.** `llm-gateway-auth ext_authz Deployment llm-gateway-auth/apikey-authz has <r>/<d> ready replicas: <cause>.`

**Diagnose.**
```sh
kubectl -n llm-gateway-auth get deploy apikey-authz apikey-operator
kubectl -n llm-gateway-auth describe deploy apikey-authz
```

**Mitigate.** Restore the ext_authz Deployment. On the request path this corresponds to TSG-2 [`ext_authz_unavailable`](tsg-2-data-plane-errors.md#ext_authz_unavailable). Suppresses `modelharnessExtAuthzProviderMissing` / `modelharnessAPIKeyReconcileFailed` until cleared.

### clusterBBRNotReady

**Meaning.** The body-based-routing (BBR) ext_proc Deployment is not Ready.

**Message.** `body-based-routing Deployment kaito-system/body-based-router has <r>/<d> ready replicas: <cause>.`

**Diagnose.**
```sh
kubectl -n kaito-system get deploy body-based-router
kubectl -n kaito-system describe deploy body-based-router
```

**Mitigate.** Restore BBR (≥ 2 replicas for HA). On the request path this corresponds to TSG-2 [`bbr_unavailable`](tsg-2-data-plane-errors.md#bbr_unavailable). This reason does **not** suppress any downstream reason.

### clusterKaitoControllerNotReady

**Meaning.** The KAITO workspace controller Deployment (`kaito-workspace`) is not Ready.

**Message.** `KAITO workspace controller Deployment kaito-system/kaito-workspace has <r>/<d> ready replicas: <cause>.`

**Diagnose.**
```sh
kubectl -n kaito-system get deploy kaito-workspace
kubectl -n kaito-system describe deploy kaito-workspace
```

**Mitigate.** Restore the controller. Suppresses `inferencesetInfraProvisioningFailed` / `inferencesetModelPodsNotReady` (those read `Workspace.status` conditions the controller writes) until cleared.

### clusterNodeProvisionerNotReady

**Meaning.** The registered node-provisioner Deployment (e.g. upstream Karpenter, or `gpu-node-mocker` in E2E) is not Ready. **Only checked when a provisioner is registered** via `clusterStatus.nodeProvisioner.{name,namespace}`; otherwise skipped (treated Ready).

**Message.** `node-provisioner Deployment <ns>/<name> has <r>/<d> ready replicas: <cause>.`

**Diagnose.**
```sh
kubectl -n <provisioner-ns> get deploy <provisioner-name>
kubectl -n <provisioner-ns> describe deploy <provisioner-name>
```

**Mitigate.** Restore the provisioner. Suppresses `inferencesetInfraProvisioningFailed` (a down provisioner means lack of NodeClaim progress is not evidence of failure) until cleared.

### clusterKedaNotReady

**Meaning.** A KEDA control-plane component (`keda-operator` or `keda-operator-metrics-apiserver`) in the `keda` namespace is not Ready.

**Message.** `KEDA control-plane Deployment keda/<keda-operator|keda-operator-metrics-apiserver> has <r>/<d> ready replicas: <cause>.`

**Diagnose.**
```sh
kubectl -n keda get deploy keda-operator keda-operator-metrics-apiserver
```

**Mitigate.** Restore KEDA. Does not suppress any downstream reason.

### clusterKedaKaitoScalerNotReady

**Meaning.** The `keda-kaito-scaler` Deployment is not Ready.

**Message.** `keda-kaito-scaler Deployment keda/keda-kaito-scaler has <r>/<d> ready replicas: <cause>.`

**Diagnose.**
```sh
kubectl -n keda get deploy keda-kaito-scaler
kubectl -n keda describe deploy keda-kaito-scaler
```

**Mitigate.** Restore the scaler. Does not suppress any downstream reason.

### clusterReady

**Meaning.** `Normal` aggregate — every `cluster*` warning has cleared. `involvedObject` is the umbrella release namespace. Informational; no action.

> `<cause>` in the deployment messages above is one of: `Deployment not found`, the Deployment's own progress/replica-failure message, or `no ready replicas`.

---

## 7. Modelharness-layer reasons

> All modelharness reasons set `involvedObject` to the workload `Namespace`; the specific object is named in `message`. Only emitted for label-discovered namespaces (§4).

### modelharnessGatewayClassMissing

**Meaning.** The namespace `Gateway` is not `Accepted` with reason `NoMatchingParent` / `InvalidParameters` / `UnsupportedValue` — a local `spec.gatewayClassName` misconfiguration. (Cluster-wide Istio absence is reported as `clusterIstioControlPlaneNotReady` instead.)

**Message.** `Namespace <ns>: Gateway <name> not accepted (<reason>): <msg>; check spec.gatewayClassName.`

**Diagnose.**
```sh
kubectl -n <ns> get gateway <name> -o jsonpath='{.status.conditions}' | jq
kubectl -n <ns> get gateway <name> -o jsonpath='{.spec.gatewayClassName}'
```

**Mitigate.** Set `spec.gatewayClassName: istio` (re-apply `charts/modelharness` with the correct value).

### modelharnessGatewayProgrammingFailed

**Meaning.** The namespace `Gateway` `Programmed=False` — harness-local programming failure (listener port collision, missing TLS secret, harness-local proxy startup).

**Message.** `Namespace <ns>: Gateway <name> programming failed (<reason>): <msg>.`

**Diagnose.**
```sh
kubectl -n <ns> get gateway <name> -o jsonpath='{.status}' | jq
kubectl -n <ns> get pods -l gateway.networking.k8s.io/gateway-name=<name>
```

**Mitigate.** Resolve the listener/TLS conflict named in `<msg>`; re-apply the chart.

### modelharnessExtAuthzProviderMissing

**Meaning.** A namespace `AuthorizationPolicy` references an ext_authz provider name that is **not** registered in `MeshConfig.extensionProviders` (compared against the `istio` ConfigMap in `istio-system`). Local chart misconfiguration. (Cluster-wide `llm-gateway-auth` outage is reported as `clusterGatewayAuthNotReady` instead.)

**Message.** `Namespace <ns>: AuthorizationPolicy '<name>' references extension provider '<provider>' which is not registered in MeshConfig.extensionProviders; re-apply charts/modelharness with the correct providerName.`

**Diagnose.**
```sh
kubectl -n <ns> get authorizationpolicy <name> -o jsonpath='{.spec.provider.name}'
kubectl -n istio-system get cm istio -o jsonpath='{.data.mesh}' | grep -A2 extensionProviders
```

**Mitigate.** Fix the `provider.name` to match a registered provider; re-apply the chart.

### modelharnessAPIKeyReconcileFailed

**Meaning.** A namespace `APIKey` CR has `Ready=False` — the `apikey-operator` is up but rejected this specific CR (invalid spec, Secret conflict). (Cluster-wide operator outage is reported as `clusterGatewayAuthNotReady`, which suppresses this.)

**Message.** `Namespace <ns>: APIKey <name> reconcile failed (<reason>): <msg>.`

**Diagnose.**
```sh
kubectl -n <ns> get apikey <name> -o jsonpath='{.status.conditions}' | jq
```

**Mitigate.** Fix the CR per `<reason>`/`<msg>` (resolve the Secret conflict / invalid field).

### modelharnessCatchAllFilterRejected

**Meaning.** The namespace catch-all `EnvoyFilter` (`model-not-found-*`) has `Reconciled=False` — rejected by Istio (workload-selector mismatch, schema error).

**Message.** `Namespace <ns>: catch-all EnvoyFilter <name> rejected by Istio: <msg>.`

**Diagnose.**
```sh
kubectl -n <ns> get envoyfilter <name> -o jsonpath='{.status}' | jq
```

**Mitigate.** Correct the EnvoyFilter (re-apply the chart); verify the workload selector matches the Gateway pods.

### modelharnessNetworkPolicyMisconfigured

**Meaning.** A `CiliumNetworkPolicy` `allowedIngressNamespaces` references a namespace that does not exist.

**Message.** `Namespace <ns>: CiliumNetworkPolicy <name> allowedIngressNamespaces references namespace '<ns2>' which does not exist.`

**Diagnose.**
```sh
kubectl get ns <ns2>     # expected NotFound
kubectl -n <ns> get ciliumnetworkpolicy <name> -o yaml
```

**Mitigate.** Create the missing namespace or remove/correct the reference in `networkPolicy.allowedIngressNamespaces`; re-apply the chart.

### modelharnessReady

**Meaning.** `Normal` aggregate — every `modelharness*` warning has cleared for the namespace. Informational.

---

## 8. Modeldeployment-layer reasons

> All modeldeployment reasons set `involvedObject` to the workload `Namespace`; the owning `InferenceSet` (and failing pod/route) is named in `message`.

### inferencesetInfraProvisioningFailed

**Meaning.** GPU node provisioning **terminally** failed for an InferenceSet's child Workspace — read from `Workspace.status.conditions[NodeClaimReady]=False` with a *terminal* reason (reason name contains `failed`/`error`). In-progress provisioning (`*Pending`/`*NotReady`) is **not** surfaced. Bypasses startup grace.

**Message.** `InferenceSet <ns>/<name>: Workspace <ws> GPU node provisioning failed (<reason>): <msg>.`

**Diagnose.**
```sh
kubectl -n <ns> get workspace -l inferenceset.kaito.sh/created-by=<name>
kubectl -n <ns> get workspace <ws> -o jsonpath='{.status.conditions}' | jq
```

**Mitigate.** Address the root cause in `<msg>` (GPU quota, instance-type availability, zone capacity, subscription registration). Suppressed while `clusterKaitoControllerNotReady` or `clusterNodeProvisionerNotReady` is active.

### inferencesetModelPodsNotReady

**Meaning.** Model (vLLM) pods **terminally** failed. Two detection paths: (a) `Workspace.status` `ResourceReady`/`InferenceReady=False` (with `NodeClaimReady=True`) and a terminal reason; (b) the model pod has a terminal container state. Transient startup (`ContainerCreating`/`PodInitializing`/`Pending`) is **not** surfaced. Bypasses startup grace.

Terminal container states detected: `ImagePullBackOff`, `ErrImagePull`, `ErrImageNeverPull`, `InvalidImageName`, `CreateContainerConfigError`, `CreateContainerError`, `RunContainerError`, `CrashLoopBackOff` (waiting); `OOMKilled`, `ContainerCannotRun`, `Error`, `DeadlineExceeded`, `StartError` (terminated); or pod phase `Failed`.

**Message.** `InferenceSet <ns>/<name>: model pod <pod> failed: <reason> on <container> container.` (pod path) or `InferenceSet <ns>/<name>: Workspace <ws> model pods failed (<cond>=False, <reason>): <msg>.` (workspace path)

**Diagnose.**
```sh
kubectl -n <ns> get pods -l inferenceset.kaito.sh/created-by=<name>
kubectl -n <ns> describe pod <pod>
kubectl -n <ns> logs <pod> --previous
```

**Mitigate.** Fix per the container state: image/registry access (`ImagePullBackOff`), memory request/limit (`OOMKilled`), crash root cause (`CrashLoopBackOff` → check logs). On the request path this corresponds to TSG-2 [`model_unavailable`](tsg-2-data-plane-errors.md#model_unavailable). Suppressed while `clusterKaitoControllerNotReady` is active.

### inferencesetRouteNotReady

**Meaning.** The model's `HTTPRoute` has a parent condition `Accepted=False` or `ResolvedRefs=False`, or its `InferencePool` has `ResolvedRefs=False`. Subject to startup grace (object-age gated).

**Message.** `InferenceSet <ns>/<name>: HTTPRoute <name> <Accepted|ResolvedRefs>=False: <msg>.` or `InferenceSet <ns>/<name>: InferencePool <name> ResolvedRefs=False: <msg>.`

**Diagnose.**
```sh
kubectl -n <ns> get httproute -l kaito.sh/inferenceset=<name> -o jsonpath='{.items[*].status.parents}' | jq
kubectl -n <ns> get inferencepool -l kaito.sh/inferenceset=<name> -o jsonpath='{.items[*].status}' | jq
kubectl -n <ns> get gateway      # confirm the parent Gateway exists
```

**Mitigate.** Restore the parent `Gateway`, fix the `parentRefs`, or correct the `InferencePool` selector so it matches running pods.

### inferencesetEPPNotReady

**Meaning.** The EPP Deployment (selected by `kaito.sh/inferenceset=<name>`) has fewer ready replicas than desired. Subject to startup grace (object-age gated).

**Message.** `InferenceSet <ns>/<name>: EPP Deployment <name> not ready: <r>/<d> ready replicas.`

**Diagnose.**
```sh
kubectl -n <ns> get deploy -l kaito.sh/inferenceset=<name>
kubectl -n <ns> describe deploy <epp-deploy>
kubectl -n <ns> logs deploy/<epp-deploy>
```

**Mitigate.** Fix the EPP (image pull, malformed ConfigMap, RBAC to list pods, `--pool-name` mismatch). On the request path this corresponds to TSG-2 [`epp_unavailable`](tsg-2-data-plane-errors.md#epp_unavailable).

### inferencesetWeightDownloadSlow

**Meaning.** **Orthogonal** warning (not part of the priority chain): while the LLM pod is initialising, **every** throughput sample in the sliding window (default `60s`) was below the threshold (default `20 MB/s`). One sample ≥ threshold suppresses it; the window must be fully populated (≥ window, ≥ 2 samples). Auto-resolves once the pod is Ready, the download completes (no sample), or vLLM's native `vllm:*` metrics appear.

**Message.** `InferenceSet <ns>/<name>: model-weights download is slow — every sample in the <window> window was below <min> MB/s (worst <worst> MB/s); LLM pod <pod>, source pod <src>.`

**Diagnose.**
```sh
kubectl -n <ns> get pods -l inferenceset.kaito.sh/created-by=<name>
# throughput gauge scraped from the pod's :5000/metrics
kubectl -n <ns> exec <src-pod> -- wget -qO- localhost:5000/metrics | grep kaito_model_download_speed_bytes_per_second
```

**Mitigate.** Improve registry/cache throughput (regional cache, faster storage class, image/weights mirror). This is a performance warning, not a hard failure — the deployment may still become Ready, just slowly.

### inferencesetReady

**Meaning.** `Normal` aggregate — every `inferenceset*` chain warning has cleared (`inferencesetWeightDownloadSlow` is orthogonal and not gating). Informational.

---

## 9. Reporter is not emitting any events

If `kubectl get events -n kube-system --field-selector source=productionstack-status-reporter` returns nothing:

1. **Reporter running?**
   ```sh
   kubectl -n kaito-system get deploy productionstack-status-reporter
   kubectl -n kaito-system logs deploy/productionstack-status-reporter
   ```
   If `ErrImagePull` / `CrashLoopBackOff`, fix the image/config first.
2. **Leader elected?** Only the leader emits; check the lease:
   ```sh
   kubectl -n kaito-system get lease productionstack-status-reporter
   ```
3. **Namespaces labelled?** Harness/model events require the discovery label (§4).
4. **Everything healthy?** A fully healthy stack emits only the `Normal` `*Ready` events.

## 10. Escalation

- Capture the event(s): `kubectl get events -n kube-system --field-selector source=productionstack-status-reporter -o yaml`.
- Capture the named object: `kubectl -n <ns> describe <kind> <name>` (from the message).
- Capture reporter logs: `kubectl -n kaito-system logs deploy/productionstack-status-reporter`.
- Escalate to the production-stack on-call with the `reason`, the workload namespace, and the named object.

## 11. References

- [TSG-2 — Data-Plane Errors](tsg-2-data-plane-errors.md)
- Proposal: [End-to-End Error Handling](../proposals/20260519-end-to-end-error-handling.md)
- Code: `pkg/productionstack-status-reporter/`
