#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# install-components.sh — Install all E2E components onto the AKS cluster.
#
# Components are grouped into phases that respect dependency order; within
# each phase, independent components install in parallel to shorten the
# critical path. Set INSTALL_PARALLEL=0 to fall back to sequential mode for
# debugging.
#
# Phase 1 (parallel, no deps):
#   - KAITO workspace operator
#   - Gateway API CRDs
#   - GWIE CRDs (InferencePool, InferenceModel)
#   - KEDA
#   - KEDA Kaito Scaler         (no dep on KEDA at chart-install time;
#                                only emits ScaledObjects later, so safe
#                                to install in parallel with KEDA)
#   - gpu-node-mocker            (no install-time dep on KAITO CRDs; the
#                                 binary discovery-checks `karpenter.sh/v1
#                                 NodeClaim` at startup and exits if the
#                                 CRD is not yet served, so kubelet retries
#                                 until KAITO finishes installing it)
#   - BBR chart prefetch (git clone fork repo only)
#   - Cluster-shared model-not-found Service in `default` (consumed by
#     every workload namespace's catch-all HTTPRoute via a
#     ReferenceGrant rendered by charts/modelharness).
#
# Phase 2 (parallel, depends on Phase 1):
#   - Istio                      (after Gateway API CRDs)
#
# Phase 3 (parallel, depends on Istio):
#   - BBR (Body-Based Router)    (helm install into istio-system)
#   - LLM Gateway Auth           (apikeys.kaito.sh CRD + AuthorizationPolicy)
#
# Per-namespace shared resources (Gateway, catch-all HTTPRoute,
# ReferenceGrant, AuthorizationPolicy, APIKey CR) are NOT installed by
# this script. Each E2E test case provisions its own dedicated
# namespace at runtime via charts/modelharness (see
# test/e2e/utils/setup.go EnsureNamespace).
#
# Environment variables (must be set by caller, e.g. run-e2e-local.sh or CI):
#   ISTIO_VERSION             — Istio version
#   GATEWAY_API_VERSION       — Gateway API CRD version
#   BBR_VERSION               — BBR release version (informational only)
#   KEDA_VERSION              — KEDA Helm chart version
#   KEDA_KAITO_SCALER_VERSION — KEDA Kaito Scaler Helm chart version
#   LLM_GATEWAY_AUTH_VERSION  — LLM Gateway Auth Helm chart version
#   LLM_GATEWAY_AUTH_IMAGE_TAG — LLM Gateway Auth container image tag
#   SHADOW_CONTROLLER_IMAGE   — gpu-node-mocker image (default: ghcr.io/kaito-project/gpu-node-mocker:latest)
#   INSTALL_PARALLEL          — set to "0" to disable parallelism (default: 1)
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="${SCRIPT_DIR}/../manifests"

# Validate required version variables are set.
: "${ISTIO_VERSION:?ISTIO_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${GATEWAY_API_VERSION:?GATEWAY_API_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${BBR_VERSION:?BBR_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${KEDA_VERSION:?KEDA_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${KEDA_KAITO_SCALER_VERSION:?KEDA_KAITO_SCALER_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${LLM_GATEWAY_AUTH_VERSION:?LLM_GATEWAY_AUTH_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${LLM_GATEWAY_AUTH_IMAGE_TAG:?LLM_GATEWAY_AUTH_IMAGE_TAG is not set. Source versions.env or export it before calling this script.}"
SHADOW_CONTROLLER_IMAGE="${SHADOW_CONTROLLER_IMAGE:-ghcr.io/kaito-project/gpu-node-mocker:latest}"
INSTALL_PARALLEL="${INSTALL_PARALLEL:-1}"
E2E_PROVIDER="${E2E_PROVIDER:-upstream}"

# Derive KEDA install namespace from provider when not explicitly provided.
#   upstream -> install KEDA via Helm into the dedicated `keda` namespace.
#   azure    -> KEDA is provided by the AKS managed add-on, which lives in
#               `kube-system`. The keda-kaito-scaler chart must be installed
#               in the same namespace as KEDA so KEDA can resolve the
#               ClusterTriggerAuthentication Secrets it ships.
if [[ -z "${KEDA_NAMESPACE:-}" ]]; then
  case "${E2E_PROVIDER}" in
    upstream) KEDA_NAMESPACE="keda" ;;
    azure)    KEDA_NAMESPACE="kube-system" ;;
    *)
      echo "Invalid E2E_PROVIDER='${E2E_PROVIDER}'. Must be 'upstream' or 'azure'." >&2
      exit 1
      ;;
  esac
fi
export KEDA_NAMESPACE

echo "=== Component versions ==="
echo "  E2E_PROVIDER:              ${E2E_PROVIDER}"
echo "  KEDA_NAMESPACE:            ${KEDA_NAMESPACE}"
echo "  ISTIO_VERSION:             ${ISTIO_VERSION}"
echo "  GATEWAY_API_VERSION:       ${GATEWAY_API_VERSION}"
echo "  BBR_VERSION:               ${BBR_VERSION}"
echo "  KEDA_VERSION:              ${KEDA_VERSION}"
echo "  KEDA_KAITO_SCALER_VERSION: ${KEDA_KAITO_SCALER_VERSION}"
echo "  LLM_GATEWAY_AUTH_VERSION:  ${LLM_GATEWAY_AUTH_VERSION}"
echo "  LLM_GATEWAY_AUTH_IMAGE_TAG:${LLM_GATEWAY_AUTH_IMAGE_TAG}"
echo "  SHADOW_CONTROLLER_IMAGE:   ${SHADOW_CONTROLLER_IMAGE}"
echo "  INSTALL_PARALLEL:          ${INSTALL_PARALLEL}"
echo ""

# ── Shared state across functions ─────────────────────────────────────────
LOGDIR="$(mktemp -d -t e2e-install-XXXXXX)"
BBR_CHART_TMPDIR="$(mktemp -d -t bbr-chart-XXXXXX)"
BBR_CHART_SUBPATH="config/charts/body-based-routing"
trap 'rm -rf "${BBR_CHART_TMPDIR}" "${LOGDIR}"' EXIT

# ── Helper: format an elapsed-seconds count as a human-friendly duration ──
fmt_dur() {
  local s="$1"
  if (( s < 60 )); then
    printf '%ds' "${s}"
  else
    printf '%dm%02ds' "$((s/60))" "$((s%60))"
  fi
}

# ── Helper: run a list of functions in parallel and aggregate logs ────────
# Usage: run_phase <phase-name> <fn1> <fn2> ...
#
# Per-task and per-phase wall-clock timings are printed at the end of each
# phase; the master summary is printed once after the final phase.
PHASE_TIMINGS=()  # "<phase-name>=<seconds>"
TASK_TIMINGS=()   # "<phase-name>/<task>=<seconds>"
run_phase() {
  local phase="$1"; shift
  local phase_dir="${LOGDIR}/${phase}"
  mkdir -p "${phase_dir}"
  local phase_start=${SECONDS}

  if [[ "${INSTALL_PARALLEL}" != "1" || $# -le 1 ]]; then
    # Sequential fallback (or single-task phase): stream output directly.
    for fn in "$@"; do
      echo ""
      echo "── [${phase}] ${fn} ──"
      local task_start=${SECONDS}
      "${fn}"
      local task_dur=$((SECONDS - task_start))
      TASK_TIMINGS+=("${phase}/${fn}=${task_dur}")
      echo "  ⏱  [${phase}] ${fn} took $(fmt_dur "${task_dur}")"
    done
    PHASE_TIMINGS+=("${phase}=$((SECONDS - phase_start))")
    echo ""
    echo "⏱  Phase '${phase}' total: $(fmt_dur "$((SECONDS - phase_start))")"
    return 0
  fi

  echo ""
  echo "── [${phase}] launching $# tasks in parallel: $* ──"
  local pids=() names=() task_starts=()
  for fn in "$@"; do
    local task_start=${SECONDS}
    (
      set -e
      "${fn}"
    ) >"${phase_dir}/${fn}.log" 2>&1 &
    pids+=($!)
    names+=("${fn}")
    task_starts+=("${task_start}")
  done

  local rc=0
  local failed=()
  local task_durs=()
  for i in "${!pids[@]}"; do
    if wait "${pids[$i]}"; then
      local d=$((SECONDS - task_starts[i]))
      task_durs+=("${d}")
      echo "  ✅ [${phase}] ${names[$i]} ($(fmt_dur "${d}"))"
      TASK_TIMINGS+=("${phase}/${names[$i]}=${d}")
    else
      local d=$((SECONDS - task_starts[i]))
      task_durs+=("${d}")
      echo "  ❌ [${phase}] ${names[$i]} ($(fmt_dur "${d}"))"
      TASK_TIMINGS+=("${phase}/${names[$i]}=${d}")
      failed+=("${names[$i]}")
      rc=1
    fi
  done

  # Always replay logs so users can see what each parallel task did.
  for n in "${names[@]}"; do
    echo ""
    echo "────── [${phase}] ${n} log ──────"
    cat "${phase_dir}/${n}.log"
  done

  local phase_dur=$((SECONDS - phase_start))
  PHASE_TIMINGS+=("${phase}=${phase_dur}")
  echo ""
  echo "⏱  Phase '${phase}' total: $(fmt_dur "${phase_dur}") (longest task gates this phase)"

  if [[ $rc -ne 0 ]]; then
    echo ""
    echo "❌ Phase '${phase}' failed: ${failed[*]}"
  fi
  return "${rc}"
}

# ── 0. Ensure helm + istioctl are available (sequential prep) ─────────────
if ! command -v helm &>/dev/null; then
  echo "Installing helm..."
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash
fi

if ! command -v istioctl &>/dev/null; then
  echo "Installing istioctl ${ISTIO_VERSION}..."
  curl -L https://istio.io/downloadIstio | ISTIO_VERSION="${ISTIO_VERSION}" sh -
  export PATH="${PWD}/istio-${ISTIO_VERSION}/bin:${PATH}"
fi
echo "Using istioctl: $(command -v istioctl)"

# ── Component install functions ───────────────────────────────────────────

install_kaito() {
  echo "=== Installing KAITO workspace operator (latest chart, image: nightly-latest) ==="
  helm repo add kaito https://kaito-project.github.io/kaito/charts/kaito 2>/dev/null || true
  helm repo update kaito

  # Install pattern follows the official KAITO nightly install guide verbatim:
  #   https://kaito-project.github.io/kaito/docs/installation/#using-nightly-builds-for-testing-purpose
  # i.e. the latest published chart from the helm repo (no --version pin)
  # combined with image.tag=nightly-latest so both chart templates and
  # controller binary track upstream HEAD.
  #
  # featureGates.gatewayAPIInferenceExtension is intentionally DISABLED.
  # Per-model GAIE artifacts (InferencePool + EPP Deployment/Service/RBAC +
  # HTTPRoute) are now provisioned by the modeldeployment Helm chart at
  # charts/modeldeployment via the E2E test suite. Enabling the feature gate
  # here would cause KAITO to render a duplicate set of resources via Flux
  # and conflict with the chart-managed artifacts.
  helm install kaito kaito/workspace \
    --namespace kaito-system \
    --create-namespace \
    --set featureGates.enableInferenceSetController=true \
    --set featureGates.gatewayAPIInferenceExtension=false \
    --set image.repository=ghcr.io/kaito-project/kaito/workspace \
    --set image.tag=nightly-latest \
    --set image.pullPolicy=Always \
    --wait --timeout=300s

  echo "⏳ Waiting for KAITO controller..."
  kubectl -n kaito-system rollout status deployment -l app.kubernetes.io/name=workspace --timeout=120s || true
  kubectl -n kaito-system wait --for=condition=ready pod -l app.kubernetes.io/name=workspace --timeout=120s || \
    echo "⚠️  KAITO pods not ready yet — continuing (will re-check later)."
}

install_gateway_api_crds() {
  if [[ "${E2E_PROVIDER}" == "azure" ]]; then
    echo "=== Skipping upstream Gateway API CRDs (provider=azure, AKS managed Gateway API add-on is enabled) ==="
    echo "Verifying gateways.gateway.networking.k8s.io is served..."
    # The managed add-on installs the standard-channel CRDs at cluster-create
    # time. Block briefly in case the install hasn't propagated yet.
    for _ in $(seq 1 30); do
      if kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1; then
        echo "  ✅ gateways CRD is served by the managed add-on"
        return 0
      fi
      sleep 2
    done
    echo "  ❌ Managed Gateway API CRDs not present after 60s — falling back to upstream install"
  fi

  echo "=== Installing Gateway API CRDs ${GATEWAY_API_VERSION} ==="
  kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
}

install_gwie_crds() {
  echo "=== Installing GWIE CRDs ==="
  kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml"
}

install_keda() {
  if [[ "${E2E_PROVIDER}" == "azure" ]]; then
    echo "=== Skipping Helm KEDA install (provider=azure, AKS managed KEDA add-on is enabled) ==="
    echo "Verifying managed KEDA in ${KEDA_NAMESPACE}..."
    # The managed add-on installs KEDA at cluster-create time; wait briefly
    # in case the controller rollout has not fully completed.
    kubectl -n "${KEDA_NAMESPACE}" rollout status deployment/keda-operator --timeout=180s || true
    kubectl -n "${KEDA_NAMESPACE}" rollout status deployment/keda-operator-metrics-apiserver --timeout=180s || true
    return 0
  fi

  echo "=== Installing KEDA ${KEDA_VERSION} ==="
  helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
  helm repo update kedacore
  helm upgrade --install keda kedacore/keda \
    --version "${KEDA_VERSION}" \
    --namespace "${KEDA_NAMESPACE}" \
    --create-namespace \
    --wait --timeout=300s

  echo "⏳ Waiting for KEDA operator..."
  kubectl -n "${KEDA_NAMESPACE}" rollout status deployment/keda-operator --timeout=180s || true
  kubectl -n "${KEDA_NAMESPACE}" rollout status deployment/keda-operator-metrics-apiserver --timeout=180s || true
}

prefetch_bbr_chart() {
  # We install BBR from the rambohe-ch fork's `support-insecure-serving` branch
  # so that BBR can be launched in insecure-serving mode (no TLS on the
  # ext_proc gRPC listener), which matches the plaintext ext_proc cluster
  # wired up by the Istio EnvoyFilter rendered by the chart. The chart is
  # fetched via a shallow git clone into a temp directory; the BBR_VERSION
  # variable is retained for log clarity but is not used as a chart version
  # pin in this branch.
  local repo="https://github.com/rambohe-ch/gateway-api-inference-extension.git"
  local ref="support-insecure-serving"
  echo "=== Prefetching BBR chart from ${repo} (branch: ${ref}) ==="
  git clone --depth 1 --branch "${ref}" "${repo}" "${BBR_CHART_TMPDIR}/gaie" >/dev/null
  echo "BBR chart cloned to ${BBR_CHART_TMPDIR}/gaie/${BBR_CHART_SUBPATH}"
}

install_gpu_mocker() {
  echo "=== Deploying gpu-node-mocker (GPU node mocker) ==="
  helm install gpu-node-mocker ./charts/gpu-node-mocker \
    --namespace kaito-system \
    --create-namespace \
    --set image.repository="${SHADOW_CONTROLLER_IMAGE%:*}" \
    --set image.tag="${SHADOW_CONTROLLER_IMAGE##*:}"

  # The mocker starts with a discovery check for `nodeclaims.karpenter.sh`
  # and exits if the CRD is not yet served. Because we now install in
  # parallel with KAITO (which ships that CRD) in phase1, the first few
  # restarts are expected; allow enough time for kubelet's CrashLoopBackOff
  # to retry once KAITO finishes (which is bounded by KAITO's own
  # --timeout=300s + a CRD-rollout grace window).
  echo "⏳ Waiting for gpu-node-mocker (will tolerate restarts while KAITO CRDs come online)..."
  kubectl -n kaito-system rollout status deployment/gpu-node-mocker --timeout=420s || true
}

install_istio() {
  echo "=== Installing Istio ${ISTIO_VERSION} ==="
  istioctl install \
    --set profile=minimal \
    --set hub=docker.io/istio \
    --set tag="${ISTIO_VERSION}" \
    --set "values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true" \
    -y

  echo "⏳ Waiting for istiod..."
  kubectl -n istio-system rollout status deployment/istiod --timeout=180s
}

install_keda_kaito_scaler() {
  echo "=== Installing KEDA Kaito Scaler ${KEDA_KAITO_SCALER_VERSION} into ${KEDA_NAMESPACE} ==="
  helm repo add keda-kaito-scaler https://kaito-project.github.io/keda-kaito-scaler/charts/kaito-project 2>/dev/null || true
  helm repo update keda-kaito-scaler
  helm upgrade --install keda-kaito-scaler keda-kaito-scaler/keda-kaito-scaler \
    --version "${KEDA_KAITO_SCALER_VERSION}" \
    --namespace "${KEDA_NAMESPACE}" \
    --create-namespace \
    --wait --timeout=300s

  echo "⏳ Waiting for keda-kaito-scaler..."
  kubectl -n "${KEDA_NAMESPACE}" rollout status deployment -l app.kubernetes.io/name=keda-kaito-scaler --timeout=180s || true
}

install_bbr() {
  # Installed into istio-system (Istio's rootNamespace) so that the
  # EnvoyFilter rendered by the chart applies cluster-wide to every
  # Istio-managed gateway, including per-case Gateways provisioned in
  # isolated namespaces by the e2e framework. Without this, the BBR
  # EnvoyFilter would be namespace-scoped to `default` and per-case
  # Gateways would never see the body-based-routing ext_proc filter,
  # breaking model name extraction and downstream HTTPRoute matching.
  # The chart also rewrites the ext_proc cluster_name FQDN to
  # `body-based-router.istio-system.svc.cluster.local` automatically.
  #
  # NOTE: The fork's chart template already pins the BBR EnvoyFilter to
  # `match.context: GATEWAY`, so the previous post-install JSON patch that
  # scoped the filter to gateway HCMs only is no longer needed.
  echo "=== Installing BBR (rambohe-ch fork, insecure-serving mode) ==="
  helm upgrade --install body-based-router "${BBR_CHART_TMPDIR}/gaie/${BBR_CHART_SUBPATH}" \
    --namespace istio-system \
    --set provider.name=istio \
    --set bbr.secureServing=false \
    --wait

  echo "⏳ Waiting for BBR..."
  kubectl -n istio-system rollout status deployment/body-based-router --timeout=120s 2>/dev/null || \
    kubectl -n istio-system wait --for=condition=ready pod -l app=body-based-router --timeout=120s 2>/dev/null || \
    echo "⚠️  BBR not ready yet — continuing."

  # Strip `spec.targetRefs` from the rendered EnvoyFilter.
  #
  # The fork's chart hard-codes a `targetRefs` block pointing at a single
  # Gateway named `inference-gateway` in the EnvoyFilter's own namespace
  # (`istio-system`). Istio's EnvoyFilter `targetRefs` is namespace-local
  # (no `namespace` field), so the filter never attaches to:
  #   - the cluster-wide `inference-gateway` Gateway in `default`, nor
  #   - the per-case e2e Gateways provisioned in isolated namespaces
  #     (e.g., `e2e-prefix-cache`, `e2e-auth`, `e2e-gpu-mocker`, …).
  # The BBR ext_proc filter therefore never runs, `x-gateway-model-name`
  # is never injected, and every model-name-based HTTPRoute falls through
  # to the catch-all `model-not-found` Service — surfaced in tests as
  # `404 model_not_found` from the gateway.
  #
  # Removing `spec.targetRefs` lets Istio fan the filter out cluster-wide;
  # combined with the chart's existing `match.context: GATEWAY`, this
  # restores the previous "applies to every Istio-managed gateway, no
  # sidecars" behavior. Run as a JSON Patch `remove` (idempotent guarded
  # with `|| true` so subsequent reinstalls don't fail).
  echo "⏳ Removing spec.targetRefs from body-based-router EnvoyFilter so it applies to all gateways..."
  kubectl -n istio-system patch envoyfilter body-based-router --type=json \
    -p='[{"op":"remove","path":"/spec/targetRefs"}]' 2>/dev/null || \
    echo "⚠️  Failed to remove targetRefs from body-based-router EnvoyFilter (may already be removed)."
}

install_llm_gateway_auth() {
  echo "=== Installing LLM Gateway Auth ${LLM_GATEWAY_AUTH_VERSION} ==="
  helm upgrade --install llm-gateway-apikey \
    oci://mcr.microsoft.com/aks/kaito/helm/llm-gateway-apikey \
    --version "${LLM_GATEWAY_AUTH_VERSION}" \
    --namespace llm-gateway-auth \
    --create-namespace \
    --set operator.image.repository=mcr.microsoft.com/aks/kaito/apikey-operator \
    --set operator.image.tag="${LLM_GATEWAY_AUTH_IMAGE_TAG}" \
    --set authz.image.repository=mcr.microsoft.com/aks/kaito/apikey-authz \
    --set authz.image.tag="${LLM_GATEWAY_AUTH_IMAGE_TAG}" \
    --set istio.enabled=true \
    --set istio.meshConfigConfigMap.patch=true \
    --set istio.gatewayNamespace=default \
    --set istio.gatewaySelector."gateway\.networking\.k8s\.io/gateway-name"=inference-gateway \
    --set crds.install=true \
    --wait --timeout=300s

  echo "⏳ Waiting for apikey-operator..."
  kubectl -n llm-gateway-auth rollout status deployment/apikey-operator --timeout=180s || true

  echo "⏳ Waiting for apikey-authz..."
  kubectl -n llm-gateway-auth rollout status deployment/apikey-authz --timeout=180s || true
}

install_model_not_found() {
  # Cluster-shared catch-all 404 Service in `default`. Every workload
  # namespace's modelharness release renders a catch-all HTTPRoute that
  # forwards unmatched requests to this Service across namespaces,
  # authorised by a per-namespace ReferenceGrant.
  echo "=== Deploying cluster-shared model-not-found Service in default ==="
  kubectl apply -f "${MANIFESTS_DIR}/model-not-found.yaml"

  echo "⏳ Waiting for model-not-found service..."
  kubectl -n default rollout status deployment/model-not-found --timeout=120s || true
}

# ── Phased execution ──────────────────────────────────────────────────────
#
# Per-namespace shared resources (Gateway, catch-all HTTPRoute,
# ReferenceGrant, AuthorizationPolicy, APIKey CR) are provisioned per
# E2E test case via charts/modelharness, not pre-installed in `default`.
# Each test case lives in its own namespace.

run_phase phase1-base \
  install_kaito \
  install_gateway_api_crds \
  install_gwie_crds \
  install_keda \
  install_keda_kaito_scaler \
  install_gpu_mocker \
  prefetch_bbr_chart \
  install_model_not_found

run_phase phase2-istio \
  install_istio

run_phase phase3-istio-filters \
  install_bbr \
  install_llm_gateway_auth

echo ""
echo "✅ All components installed."

# ── Timing summary ────────────────────────────────────────────────────────
echo ""
echo "================ install-components.sh timing summary ================"
TOTAL=0
for entry in "${PHASE_TIMINGS[@]}"; do
  name="${entry%%=*}"
  secs="${entry##*=}"
  TOTAL=$((TOTAL + secs))
  printf '  phase  %-30s %s\n' "${name}" "$(fmt_dur "${secs}")"
done
printf '  TOTAL  %-30s %s\n' "(sum of phase wall-clocks)" "$(fmt_dur "${TOTAL}")"
echo ""
echo "  Per-task wall-clocks (within a parallel phase, the longest task gates the phase):"
for entry in "${TASK_TIMINGS[@]}"; do
  name="${entry%%=*}"
  secs="${entry##*=}"
  printf '    %-50s %s\n' "${name}" "$(fmt_dur "${secs}")"
done
echo "======================================================================"
