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

# Node-provisioner selection (decoupled from real-vs-mocked):
#   KAITO_NODE_PROVISIONER — azure-gpu-provisioner | karpenter (default karpenter).
#   ENABLE_NODE_MOCKER     — true (default): deploy gpu-node-mocker, which fakes
#                            the selected flow; false: use the real provisioner.
NODE_PROVISIONER="${KAITO_NODE_PROVISIONER:-karpenter}"
ENABLE_NODE_MOCKER="${ENABLE_NODE_MOCKER:-true}"

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
echo "  ENABLE_NODE_MOCKER:        ${ENABLE_NODE_MOCKER}"
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

# ── KAITO chart resolution ────────────────────────────────────────────────
# Sets KAITO_CHART_REF (and KAITO_CHART_TMPDIR when a clone is needed). The
# karpenter provisioner needs the kaito main-branch chart because the published
# chart does not yet expose `nodeProvisioner`; azure-gpu-provisioner uses the
# published chart.
resolve_kaito_chart() {
  if [[ "${NODE_PROVISIONER}" == "karpenter" ]]; then
    # The published Helm chart does not yet expose `nodeProvisioner` or template
    # the corresponding `--node-provisioner` / Karpenter node-class args. Those
    # chart changes are present on kaito main but not in the published chart repo
    # yet, so install from the in-tree chart on main.
    echo "  Karpenter mode: installing from kaito main branch chart (published chart lacks nodeProvisioner)"
    KAITO_CHART_TMPDIR=$(mktemp -d)
    git clone --depth 1 --quiet \
      https://github.com/kaito-project/kaito.git "${KAITO_CHART_TMPDIR}"
    KAITO_CHART_REF="${KAITO_CHART_TMPDIR}/charts/kaito/workspace"
  else
    helm repo add kaito https://kaito-project.github.io/kaito/charts/kaito 2>/dev/null || true
    helm repo update kaito
    KAITO_CHART_REF="kaito/workspace"
  fi
}

install_kaito() {
  echo "=== Installing KAITO workspace operator (image: nightly-latest) ==="

  KAITO_CHART_REF=""
  KAITO_CHART_TMPDIR=""
  resolve_kaito_chart
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
    --set nodeProvisioner="${NODE_PROVISIONER}" \
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

# ── Node-provisioner install ──────────────────────────────────────────────
# When ENABLE_NODE_MOCKER=true, deploy gpu-node-mocker, which fakes the
# selected ${NODE_PROVISIONER} flow. When false, the real provisioner is used
# instead: karpenter is installed via separate helm steps (CI / make targets)
# and azure-gpu-provisioner is handled by KAITO itself — so there is nothing to
# install in-cluster here.
install_gpu_node_mocker() {
  echo "=== Deploying gpu-node-mocker (GPU node mocker, --node-provisioner=${NODE_PROVISIONER}) ==="
  helm install gpu-node-mocker ./charts/gpu-node-mocker \
    --namespace kaito-system \
    --create-namespace \
    --set nodeProvisioner="${NODE_PROVISIONER}" \
    --set image.repository="${SHADOW_CONTROLLER_IMAGE%:*}" \
    --set image.tag="${SHADOW_CONTROLLER_IMAGE##*:}"

  # In karpenter mode the mocker discovery-checks nodeclaims.karpenter.sh
  # (shipped by KAITO) plus nodepools.karpenter.sh and aksnodeclasses.karpenter.
  # azure.com (both shipped by this chart's crds/) at startup and exits if any
  # is not served, so early restarts are expected while the KAITO-owned
  # nodeclaims CRD comes online in parallel.
  echo "⏳ Waiting for gpu-node-mocker (will tolerate restarts while karpenter CRDs come online)..."
  kubectl -n kaito-system rollout status deployment/gpu-node-mocker --timeout=420s || true
}

install_node_provisioner() {
  if [[ "${ENABLE_NODE_MOCKER}" == "true" ]]; then
    install_gpu_node_mocker
    return
  fi
  echo "=== ENABLE_NODE_MOCKER=false: skipping gpu-node-mocker (using real ${NODE_PROVISIONER}) ==="
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
# install_node_provisioner deploys gpu-node-mocker when ENABLE_NODE_MOCKER=true,
# or is a no-op when false (the real provisioner — karpenter via separate helm
# steps, or KAITO's own azure-gpu-provisioner — needs no in-cluster install).
run_phase phase1-base \
  install_kaito \
  install_gwie_crds \
  install_node_provisioner \
  install_productionstack

echo ""
echo "✅ All components installed."

print_timing_summary "install-components.sh timing summary"
