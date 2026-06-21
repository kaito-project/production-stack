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
#   SHADOW_CONTROLLER_IMAGE    — gpu-node-mocker image (default: ghcr.io/kaito-project/gpu-node-mocker:latest)
#   INSTALL_PARALLEL           — set to "0" to disable parallelism (default: 1)
#
# llm-gateway-apikey chart version + image tag are pinned by
# charts/productionstack/Chart.yaml (chart dep) and the upstream chart's
# appVersion respectively — bump by editing Chart.yaml and re-running
# `helm dependency update charts/productionstack`.
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

SHADOW_CONTROLLER_IMAGE="${SHADOW_CONTROLLER_IMAGE:-ghcr.io/kaito-project/gpu-node-mocker:latest}"
INSTALL_PARALLEL="${INSTALL_PARALLEL:-1}"
E2E_PROVIDER="${E2E_PROVIDER:-upstream}"

# shellcheck source=lib-parallel.sh
source "${SCRIPT_DIR}/lib-parallel.sh"
# shellcheck source=lib-node-provisioner.sh
source "${SCRIPT_DIR}/lib-node-provisioner.sh"

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
echo "  NODE_PROVISIONER:          ${NODE_PROVISIONER}"
echo "  KEDA_NAMESPACE:            ${KEDA_NAMESPACE}"
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

# ── KAITO chart resolution (per node provisioner) ─────────────────────────
# Each implementation populates the install plan consumed by install_kaito:
#   KAITO_CHART_REF       — helm chart reference for `helm upgrade --install`
#   KAITO_EXTRA_SET_ARGS  — array of extra `--set ...` args
#   KAITO_CHART_TMPDIR    — temp dir to clean up after install (empty if none)

np_gpu_node_mocker__kaito_chart() {
  helm repo add kaito https://kaito-project.github.io/kaito/charts/kaito 2>/dev/null || true
  helm repo update kaito
  KAITO_CHART_REF="kaito/workspace"
}

np_karpenter__kaito_chart() {
  # The published Helm chart already has the NAP-related feature gates
  # (`featureGates.disableNodeAutoProvisioning`, `localCSIDriver`), but it
  # does not yet expose `nodeProvisioner` or template the corresponding
  # `--node-provisioner` / Karpenter node-class args. Those chart changes are
  # present on kaito main but are not in the published chart repo yet, so we
  # install from the in-tree chart on main and override it here.
  echo "  Karpenter mode: installing from kaito main branch chart (published chart lacks nodeProvisioner)"
  KAITO_CHART_TMPDIR=$(mktemp -d)
  git clone --depth 1 --quiet \
    https://github.com/kaito-project/kaito.git "${KAITO_CHART_TMPDIR}"
  KAITO_CHART_REF="${KAITO_CHART_TMPDIR}/charts/kaito/workspace"
  KAITO_EXTRA_SET_ARGS=(--set nodeProvisioner=karpenter)
}

install_kaito() {
  echo "=== Installing KAITO workspace operator (image: nightly-latest) ==="

  KAITO_CHART_REF=""
  KAITO_EXTRA_SET_ARGS=()
  KAITO_CHART_TMPDIR=""
  node_provisioner_run kaito_chart
  if [[ -n "${KAITO_CHART_TMPDIR}" ]]; then
    trap 'rm -rf "${KAITO_CHART_TMPDIR}"' RETURN
  fi

  # Per-model GAIE artifacts are provisioned by charts/modeldeployment; enabling
  # the gate would render a duplicate set of resources via Flux and conflict.
  helm upgrade --install kaito "${KAITO_CHART_REF}" \
    --namespace kaito-system \
    --create-namespace \
    --set featureGates.enableInferenceSetController=true \
    --set featureGates.gatewayAPIInferenceExtension=false \
    --set image.repository=ghcr.io/kaito-project/kaito/workspace \
    --set image.tag=nightly-latest \
    --set image.pullPolicy=Always \
    "${KAITO_EXTRA_SET_ARGS[@]}" \
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

# ── Node-provisioner install (per node provisioner) ───────────────────────
# gpu-node-mocker deploys its controller here. Karpenter provisions real nodes
# via AKS NAP and has no in-cluster component to install, so it defines no hook
# and install_node_provisioner is a no-op for it.
np_gpu_node_mocker__install() {
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

install_node_provisioner() {
  node_provisioner_run install
}


install_productionstack() {
  # Umbrella chart at charts/productionstack vendors body-based-routing and
  # keda-kaito-scaler as in-tree subcharts and pulls llm-gateway-apikey
  # from oci://mcr.microsoft.com/aks/kaito/helm as a Helm dependency
  # (no in-tree fork — `helm dependency update` vendors the tarball into
  # charts/productionstack/charts/ at install time).
  #
  # Per-subchart install namespaces (each via the subchart's own
  # `namespaceOverride` value):
  #   * body-based-routing → kaito-system    (inherits the umbrella
  #     release namespace). BBR is a workload-only singleton now: this
  #     subchart renders just the Deployment + Service + RBAC and NO
  #     EnvoyFilter, so it no longer has to live in Istio's root
  #     namespace. The ext_proc EnvoyFilter that injects BBR into each
  #     Gateway's HCM (anchored INSERT_BEFORE the InferencePool
  #     ext_proc, giving ext_authz → bbr → ext_proc/epp → router) is
  #     rendered per-namespace by charts/modelharness and scoped to that
  #     namespace's Gateway pod.
  #   * keda-kaito-scaler  → ${KEDA_NAMESPACE}  (co-located with KEDA so
  #     KEDA can resolve the ClusterTriggerAuthentication Secrets it
  #     ships.)
  #   * llm-gateway-apikey → llm-gateway-auth  (upstream chart 0.0.8-alpha+
  #     supports namespaceOverride; the LGA operator + authz control
  #     plane live here, matching where validate-components.sh and the
  #     e2e suite expect them.)
  #
  # The umbrella release itself is installed into `kaito-system`; the
  # release Secret therefore lives in `kaito-system`. `helm uninstall
  # productionstack -n kaito-system` correctly cleans up across all
  # override namespaces because Helm tracks the rendered manifest, not
  # the namespace.
  #
  # Note: the BBR EnvoyFilter rendered per-namespace by
  # charts/modelharness only slots into the runtime filter chain once
  # Istio has injected the InferencePool ext_proc anchor; it is created
  # up-front and attaches lazily, which is fine.

  echo "⏳ Ensuring per-subchart target namespaces exist..."
  kubectl create namespace "${KEDA_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
  kubectl create namespace llm-gateway-auth  --dry-run=client -o yaml | kubectl apply -f -

  echo "⏳ Vendoring upstream llm-gateway-apikey OCI dependency..."
  helm dependency update "${PRODUCTIONSTACK_CHART_DIR}"

  echo "=== Installing productionstack umbrella chart ==="
  echo "    body-based-routing → kaito-system (umbrella release namespace)"
  echo "    keda-kaito-scaler  → ${KEDA_NAMESPACE}"
  echo "    llm-gateway-apikey → llm-gateway-auth (chart version pinned in Chart.yaml)"
  helm upgrade --install productionstack "${PRODUCTIONSTACK_CHART_DIR}" \
    --namespace kaito-system \
    --create-namespace \
    --set keda-kaito-scaler.namespaceOverride="${KEDA_NAMESPACE}" \
    --set llm-gateway-apikey.enabled=true \
    --set llm-gateway-apikey.namespaceOverride=llm-gateway-auth \
    --wait --timeout=600s

  echo "⏳ Waiting for BBR..."
  kubectl -n kaito-system rollout status deployment/body-based-router --timeout=120s 2>/dev/null || \
    kubectl -n kaito-system wait --for=condition=ready pod -l app=body-based-router --timeout=120s 2>/dev/null || \
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
# install_node_provisioner deploys the in-cluster component for the active
# node provisioner (gpu-node-mocker) or is a no-op when none is needed
# (Karpenter / AKS NAP provisions real nodes with no in-cluster install).
run_phase phase1-base \
  install_kaito \
  install_gwie_crds \
  install_node_provisioner \
  install_productionstack

echo ""
echo "✅ All components installed."

print_timing_summary "install-components.sh timing summary"
