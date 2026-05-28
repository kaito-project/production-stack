#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# install-components.sh — Install all E2E components onto the AKS cluster.
#
# Phase 1 (parallel): KAITO, GAIE CRDs, gpu-node-mocker, productionstack
#                     (umbrella chart bundling body-based-routing,
#                     keda-kaito-scaler, and llm-gateway-auth).
#
# Per-namespace shared resources (Gateway, HTTPRoute, AuthorizationPolicy,
# APIKey CR, etc.) are provisioned per-test via charts/modelharness.
#
# Required env vars (set by caller, e.g. run-e2e-local.sh or CI):
#   LLM_GATEWAY_AUTH_VERSION   — LLM Gateway Auth Helm chart version
#   LLM_GATEWAY_AUTH_IMAGE_TAG — LLM Gateway Auth container image tag
#   SHADOW_CONTROLLER_IMAGE    — gpu-node-mocker image (default: ghcr.io/kaito-project/gpu-node-mocker:latest)
#   INSTALL_PARALLEL           — set to "0" to disable parallelism (default: 1)
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

: "${LLM_GATEWAY_AUTH_VERSION:?LLM_GATEWAY_AUTH_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${LLM_GATEWAY_AUTH_IMAGE_TAG:?LLM_GATEWAY_AUTH_IMAGE_TAG is not set. Source versions.env or export it before calling this script.}"
SHADOW_CONTROLLER_IMAGE="${SHADOW_CONTROLLER_IMAGE:-ghcr.io/kaito-project/gpu-node-mocker:latest}"
INSTALL_PARALLEL="${INSTALL_PARALLEL:-1}"
E2E_PROVIDER="${E2E_PROVIDER:-upstream}"

# shellcheck source=lib-parallel.sh
source "${SCRIPT_DIR}/lib-parallel.sh"

# Derive KEDA install namespace from provider:
#   upstream → `keda` (Helm-installed KEDA)
#   azure    → `kube-system` (AKS managed KEDA add-on). keda-kaito-scaler
#              must be co-located with KEDA so KEDA can resolve the
#              ClusterTriggerAuthentication Secrets it ships.
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
echo "  LLM_GATEWAY_AUTH_VERSION:  ${LLM_GATEWAY_AUTH_VERSION}"
echo "  LLM_GATEWAY_AUTH_IMAGE_TAG:${LLM_GATEWAY_AUTH_IMAGE_TAG}"
echo "  SHADOW_CONTROLLER_IMAGE:   ${SHADOW_CONTROLLER_IMAGE}"
echo "  INSTALL_PARALLEL:          ${INSTALL_PARALLEL}"
echo ""

# Path to the in-tree productionstack umbrella chart (repo-root-relative).
PRODUCTIONSTACK_CHART_DIR="${SCRIPT_DIR}/../../../charts/productionstack"

# Ensure helm is available.
if ! command -v helm &>/dev/null; then
  echo "Installing helm..."
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash
fi

install_kaito() {
  echo "=== Installing KAITO workspace operator (latest chart, image: nightly-latest) ==="
  helm repo add kaito https://kaito-project.github.io/kaito/charts/kaito 2>/dev/null || true
  helm repo update kaito

  # featureGates.gatewayAPIInferenceExtension is intentionally DISABLED.
  # Per-model GAIE artifacts are provisioned by charts/modeldeployment; enabling
  # the gate would render a duplicate set of resources via Flux and conflict.
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
  # Use server-side apply: the KAITO chart bundles the same GWIE CRDs and
  # client-side apply races between GET → CREATE-if-missing. Server-side
  # apply is a single atomic POST with --force-conflicts taking ownership.
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

  # The mocker discovery-checks `nodeclaims.karpenter.sh` at startup and
  # exits if the CRD is not served, so early restarts are expected while
  # KAITO installs that CRD in parallel.
  echo "⏳ Waiting for gpu-node-mocker (will tolerate restarts while KAITO CRDs come online)..."
  kubectl -n kaito-system rollout status deployment/gpu-node-mocker --timeout=420s || true
}


install_productionstack() {
  # Umbrella chart at charts/productionstack vendors body-based-routing and
  # keda-kaito-scaler as in-tree subcharts and pulls llm-gateway-apikey
  # from oci://mcr.microsoft.com/aks/kaito/helm as a Helm dependency
  # (no in-tree fork — `helm dependency update` vendors the tarball into
  # charts/productionstack/charts/ at install time).
  #
  # Per-subchart install namespaces:
  #   * body-based-routing → istio-system    (in-tree fork; namespaceOverride.
  #     Istio rootNamespace so the chart-rendered EnvoyFilter applies
  #     cluster-wide; anchored on envoy.filters.http.ext_authz with
  #     INSERT_AFTER, giving the runtime order:
  #     ext_authz → bbr → router → epp/not-found.)
  #   * keda-kaito-scaler  → ${KEDA_NAMESPACE}  (in-tree fork; namespaceOverride.
  #     Co-located with KEDA.)
  #   * llm-gateway-apikey → release namespace (upstream chart has no
  #     namespaceOverride knob; every namespaced resource it ships lands in
  #     the umbrella's release namespace). To keep the LGA operator + authz
  #     control plane in `llm-gateway-auth` — where validate-components.sh
  #     and the e2e suite expect them — the umbrella release itself is
  #     installed into `llm-gateway-auth` rather than `kaito-system`.
  #
  # The release Secret therefore lives in `llm-gateway-auth`;
  # `helm uninstall productionstack -n llm-gateway-auth` correctly cleans
  # up across all override namespaces because Helm tracks the rendered
  # manifest, not the namespace.
  #
  # Note: the BBR EnvoyFilter's anchorSubFilter (`envoy.filters.http.ext_authz`)
  # is only present in the runtime filter chain once the LGA MeshConfig
  # patch Job has completed AND a workload AuthorizationPolicy has been
  # rendered (per-namespace, by charts/modelharness). The EnvoyFilter CR
  # is created up-front; it slots in lazily, which is fine.

  echo "⏳ Ensuring per-subchart target namespaces exist..."
  kubectl create namespace istio-system     --dry-run=client -o yaml | kubectl apply -f -
  kubectl create namespace "${KEDA_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

  echo "⏳ Vendoring upstream llm-gateway-apikey OCI dependency..."
  helm dependency update "${PRODUCTIONSTACK_CHART_DIR}"

  echo "=== Installing productionstack umbrella chart ==="
  echo "    body-based-routing → istio-system"
  echo "    keda-kaito-scaler  → ${KEDA_NAMESPACE}"
  echo "    llm-gateway-apikey → llm-gateway-auth (release ns; chart appVersion ${LLM_GATEWAY_AUTH_VERSION}, image tag ${LLM_GATEWAY_AUTH_IMAGE_TAG})"
  # Pass image tag overrides under the upstream chart key
  # `llm-gateway-apikey` (must match the dependency `name:` in Chart.yaml).
  helm upgrade --install productionstack "${PRODUCTIONSTACK_CHART_DIR}" \
    --namespace llm-gateway-auth \
    --create-namespace \
    --set body-based-routing.enabled=true \
    --set body-based-routing.namespaceOverride=istio-system \
    --set keda-kaito-scaler.enabled=true \
    --set keda-kaito-scaler.namespaceOverride="${KEDA_NAMESPACE}" \
    --set llm-gateway-apikey.enabled=true \
    --set llm-gateway-apikey.operator.image.tag="${LLM_GATEWAY_AUTH_IMAGE_TAG}" \
    --set llm-gateway-apikey.authz.image.tag="${LLM_GATEWAY_AUTH_IMAGE_TAG}" \
    --wait --timeout=600s

  echo "⏳ Waiting for BBR..."
  kubectl -n istio-system rollout status deployment/body-based-router --timeout=120s 2>/dev/null || \
    kubectl -n istio-system wait --for=condition=ready pod -l app=body-based-router --timeout=120s 2>/dev/null || \
    echo "⚠️  BBR not ready yet — continuing."

  echo "⏳ Waiting for keda-kaito-scaler..."
  kubectl -n "${KEDA_NAMESPACE}" rollout status deployment -l app.kubernetes.io/name=keda-kaito-scaler --timeout=180s || true

  echo "⏳ Waiting for apikey-operator..."
  kubectl -n llm-gateway-auth wait --for=condition=Available \
    deployment/apikey-operator --timeout=15m

  echo "⏳ Waiting for apikey-authz..."
  kubectl -n llm-gateway-auth wait --for=condition=Available \
    deployment/apikey-authz --timeout=15m
}

# ── Phased execution ──────────────────────────────────────────────────────
run_phase phase1-base \
  install_kaito \
  install_gwie_crds \
  install_gpu_mocker \
  install_productionstack

echo ""
echo "✅ All components installed."

print_timing_summary "install-components.sh timing summary"
