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
#   - GAIE CRDs                  (server-side apply of the
#                                 gateway-api-inference-extension manifests;
#                                 not covered by any AKS managed add-on
#                                 today, so the same install runs for
#                                 both upstream and azure providers).
#   - gpu-node-mocker            (no install-time dep on KAITO CRDs; the
#                                 binary discovery-checks `karpenter.sh/v1
#                                 NodeClaim` at startup and exits if the
#                                 CRD is not yet served, so kubelet retries
#                                 until KAITO finishes installing it)
#   - productionstack            (helm-installed from the in-tree umbrella
#                                 chart at charts/productionstack. Bundles
#                                 body-based-routing → istio-system and
#                                 keda-kaito-scaler → ${KEDA_NAMESPACE}
#                                 in a single helm release, replacing
#                                 the previous separate install_bbr +
#                                 install_keda_kaito_scaler steps. The
#                                 BBR subchart ships an EnvoyFilter that
#                                 requires the Istio control plane to be
#                                 up; Istio is installed by
#                                 setup-cluster.sh BEFORE this script
#                                 runs, so phase1 has no Istio race.)
#
# (Catch-all 404 handling is now provided by an EnvoyFilter
# direct_response rendered per-namespace by charts/modelharness — no
# cluster-shared Service is required, so install_model_not_found has
# been removed from this script.)
#
# Phase 2 (depends on Phase 1):
#   - LLM Gateway Auth           (apikeys.kaito.sh CRD + cluster-wide
#                                 ext_authz CUSTOM provider; depends on
#                                 Istio being installed in Phase 1).
#                                 The per-namespace AuthorizationPolicy
#                                 that activates ext_authz on each
#                                 Gateway pod is rendered later by the
#                                 modelharness chart at test time; its
#                                 placement in the Envoy filter chain
#                                 lands BEFORE BBR because BBR's
#                                 EnvoyFilter is anchored on
#                                 envoy.filters.http.ext_authz with
#                                 INSERT_AFTER, giving the runtime
#                                 order: ext_authz → bbr → router
#                                 (HTTPRoute → epp / model-not-found).
#
# Per-namespace shared resources (Gateway, catch-all HTTPRoute,
# ReferenceGrant, AuthorizationPolicy, APIKey CR) are NOT installed by
# this script. Each E2E test case provisions its own dedicated
# namespace at runtime via charts/modelharness (see
# test/e2e/utils/setup.go EnsureNamespace).
#
# Environment variables (must be set by caller, e.g. run-e2e-local.sh or CI):
#   BBR_VERSION               — BBR release version (informational only)
#   KEDA_KAITO_SCALER_VERSION — KEDA Kaito Scaler Helm chart version
#                               (informational only — the productionstack
#                               umbrella chart vendors the subchart in-tree
#                               so this version is not used at install time)
#   LLM_GATEWAY_AUTH_VERSION  — LLM Gateway Auth Helm chart version
#   LLM_GATEWAY_AUTH_IMAGE_TAG — LLM Gateway Auth container image tag
#   SHADOW_CONTROLLER_IMAGE   — gpu-node-mocker image (default: ghcr.io/kaito-project/gpu-node-mocker:latest)
#   INSTALL_PARALLEL          — set to "0" to disable parallelism (default: 1)
#
# Note: KEDA, Gateway API base CRDs, and the Istio control plane are
# installed by hack/e2e/scripts/setup-cluster.sh so both the upstream
# and azure providers converge on the same prerequisite state before
# this script runs. GAIE (Gateway API Inference Extension) CRDs stay
# in phase1-base here.
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Validate required version variables are set.
: "${BBR_VERSION:?BBR_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${KEDA_KAITO_SCALER_VERSION:?KEDA_KAITO_SCALER_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${LLM_GATEWAY_AUTH_VERSION:?LLM_GATEWAY_AUTH_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${LLM_GATEWAY_AUTH_IMAGE_TAG:?LLM_GATEWAY_AUTH_IMAGE_TAG is not set. Source versions.env or export it before calling this script.}"
SHADOW_CONTROLLER_IMAGE="${SHADOW_CONTROLLER_IMAGE:-ghcr.io/kaito-project/gpu-node-mocker:latest}"
INSTALL_PARALLEL="${INSTALL_PARALLEL:-1}"
E2E_PROVIDER="${E2E_PROVIDER:-upstream}"

# Source the shared run_phase / fmt_dur helpers.
# shellcheck source=lib-parallel.sh
source "${SCRIPT_DIR}/lib-parallel.sh"

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
echo "  BBR_VERSION:               ${BBR_VERSION}"
echo "  KEDA_KAITO_SCALER_VERSION: ${KEDA_KAITO_SCALER_VERSION}"
echo "  LLM_GATEWAY_AUTH_VERSION:  ${LLM_GATEWAY_AUTH_VERSION}"
echo "  LLM_GATEWAY_AUTH_IMAGE_TAG:${LLM_GATEWAY_AUTH_IMAGE_TAG}"
echo "  SHADOW_CONTROLLER_IMAGE:   ${SHADOW_CONTROLLER_IMAGE}"
echo "  INSTALL_PARALLEL:          ${INSTALL_PARALLEL}"
echo ""

# ── Shared state across functions ─────────────────────────────────────────
# Path to the in-tree productionstack umbrella chart, resolved relative
# to the repo root (this script lives at hack/e2e/scripts/install-components.sh,
# so ../../.. from SCRIPT_DIR is the repo root). The umbrella chart
# bundles body-based-routing and keda-kaito-scaler as subcharts, so a
# single `helm install` covers both components.
PRODUCTIONSTACK_CHART_DIR="${SCRIPT_DIR}/../../../charts/productionstack"

# ── 0. Ensure helm is available (sequential prep) ───────────────────────
if ! command -v helm &>/dev/null; then
  echo "Installing helm..."
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash
fi

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

install_gwie_crds() {
  # Use server-side apply (--server-side --force-conflicts) instead of the
  # default client-side apply. install_gwie_crds runs in parallel with
  # install_kaito in phase1-base, and the KAITO chart bundles the same
  # GWIE CRDs (inferencepools / inferenceobjectives in both
  # inference.networking.k8s.io and inference.networking.x-k8s.io groups).
  # Client-side apply does GET → CREATE-if-missing, which races with KAITO
  # creating the CRD between the GET and the CREATE and fails with
  # `AlreadyExists`. Server-side apply is a single atomic POST with a
  # field manager: if the object already exists it is merged in place
  # (with --force-conflicts taking ownership of any fields KAITO set).
  echo "=== Installing GWIE CRDs ==="
  kubectl apply --server-side --force-conflicts \
    -f "https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml"
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


install_productionstack() {
  # The productionstack umbrella chart at charts/productionstack bundles
  # both body-based-routing and keda-kaito-scaler as in-tree subcharts.
  # A single `helm install` covers both components, replacing the
  # previous separate install_bbr + install_keda_kaito_scaler steps.
  #
  # Per-subchart install namespaces are pinned via each subchart's
  # `namespaceOverride` value (Helm itself only accepts a single
  # `--namespace` per release):
  #   * body-based-routing → istio-system  (Istio's rootNamespace, so
  #     the chart-rendered EnvoyFilter applies cluster-wide to every
  #     Istio-managed Gateway; the chart defaults to INSERT_AFTER
  #     anchored on envoy.filters.http.ext_authz, giving the runtime
  #     order: ext_authz → bbr → router → epp/not-found).
  #   * keda-kaito-scaler  → ${KEDA_NAMESPACE}  (must be co-located with
  #     the KEDA control plane so KEDA can resolve the
  #     ClusterTriggerAuthentication Secrets it ships).
  #
  # The Helm release metadata Secret itself lives in `kaito-system`;
  # `helm uninstall productionstack -n kaito-system` correctly cleans
  # up resources across all override namespaces.
  #
  # Phase-1 parallelism note:
  #   install_productionstack runs in parallel with KAITO and friends in
  #   phase1-base. The BBR subchart's EnvoyFilter requires the Istio
  #   control plane (CRDs + istiod) to be up, but Istio is installed by
  #   setup-cluster.sh BEFORE install-components.sh runs, so there is no
  #   in-phase race — we just need to ensure the per-subchart target
  #   namespaces exist (Helm only auto-creates the release namespace,
  #   not the override targets).

  echo "⏳ Ensuring per-subchart target namespaces exist..."
  kubectl create namespace istio-system --dry-run=client -o yaml | kubectl apply -f -
  kubectl create namespace "${KEDA_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

  echo "=== Installing productionstack umbrella chart ==="
  echo "    body-based-routing → istio-system (BBR ${BBR_VERSION})"
  echo "    keda-kaito-scaler  → ${KEDA_NAMESPACE} (vendored ${KEDA_KAITO_SCALER_VERSION})"
  helm upgrade --install productionstack "${PRODUCTIONSTACK_CHART_DIR}" \
    --namespace kaito-system \
    --create-namespace \
    --set body-based-routing.enabled=true \
    --set body-based-routing.namespaceOverride=istio-system \
    --set keda-kaito-scaler.enabled=true \
    --set keda-kaito-scaler.namespaceOverride="${KEDA_NAMESPACE}" \
    --wait --timeout=300s

  echo "⏳ Waiting for BBR..."
  kubectl -n istio-system rollout status deployment/body-based-router --timeout=120s 2>/dev/null || \
    kubectl -n istio-system wait --for=condition=ready pod -l app=body-based-router --timeout=120s 2>/dev/null || \
    echo "⚠️  BBR not ready yet — continuing."

  echo "⏳ Waiting for keda-kaito-scaler..."
  kubectl -n "${KEDA_NAMESPACE}" rollout status deployment -l app.kubernetes.io/name=keda-kaito-scaler --timeout=180s || true
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

# ── Phased execution ──────────────────────────────────────────────────────
#
# Per-namespace shared resources (Gateway, catch-all HTTPRoute,
# ReferenceGrant, AuthorizationPolicy, APIKey CR) are provisioned per
# E2E test case via charts/modelharness, not pre-installed in `default`.
# Each test case lives in its own namespace.

run_phase phase1-base \
  install_kaito \
  install_gwie_crds \
  install_gpu_mocker \
  install_productionstack

run_phase phase2-istio-filters \
  install_llm_gateway_auth

echo ""
echo "✅ All components installed."

print_timing_summary "install-components.sh timing summary"
