#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# setup-cluster.sh — Create an AKS cluster for E2E testing.
#
# Prerequisite: the resource group and ACR must already exist (created by
# `prepare-image.sh` or the CI "Prepare gpu-node-mocker image" step). This
# script intentionally only creates the AKS cluster so its wall time can be
# measured in isolation.
#
# Environment variables (all required unless defaults are acceptable):
#   RESOURCE_GROUP        — Azure resource group name  (default: kaito-rg)
#   CLUSTER_NAME          — AKS cluster name           (default: kaito-aks)
#   ACR_NAME              — ACR registry name           (default: <cluster_name>acr, sanitized)
#   LOCATION              — Azure region               (default: australiaeast)
#   NODE_COUNT            — Number of worker nodes      (default: 2)
#   NODE_VM_SIZE          — VM SKU for the node pool    (default: Standard_D8s_v5)
#   E2E_PROVIDER          — upstream|azure              (default: upstream)
#   GATEWAY_API_VERSION   — Gateway API CRD version    (sourced from versions.env)
#   KEDA_VERSION          — KEDA Helm chart version    (sourced from versions.env)
#   ISTIO_VERSION         — Istio control-plane version (sourced from versions.env)
#   KEDA_NAMESPACE        — Override the namespace KEDA is installed into
#                           (derived from E2E_PROVIDER when unset)
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RESOURCE_GROUP="${RESOURCE_GROUP:-kaito-rg}"
CLUSTER_NAME="${CLUSTER_NAME:-kaito-aks}"
# ACR names must be alphanumeric, 5-50 chars. Strip dashes from cluster name.
ACR_NAME="${ACR_NAME:-$(echo "${CLUSTER_NAME}acr" | tr -d '-' | head -c 50)}"
LOCATION="${LOCATION:-australiaeast}"
NODE_COUNT="${NODE_COUNT:-2}"
NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D8s_v5}"
E2E_PROVIDER="${E2E_PROVIDER:-upstream}"

# Optional AKS-managed add-ons toggled by provider.
#   azure    -> enable the managed KEDA add-on so the cluster ships with
#               KEDA pre-installed in `kube-system`, and install-components.sh
#               skips the Helm-based KEDA install. Also enable the managed
#               Gateway API CRDs add-on (preview, requires aks-preview
#               extension and the `ManagedGatewayAPIPreview` feature flag)
#               so install-components.sh can skip the upstream
#               kubectl-apply of standard-install.yaml.
#               Doc: https://learn.microsoft.com/azure/aks/managed-gateway-api
#   upstream -> no managed add-ons; KEDA + Gateway API CRDs are installed
#               via Helm/kubectl later.
EXTRA_AKS_ARGS=()
case "${E2E_PROVIDER}" in
  azure)
    EXTRA_AKS_ARGS+=(--enable-keda --enable-gateway-api)

    # Managed Gateway API requires the aks-preview extension and the
    # `ManagedGatewayAPIPreview` feature flag to be registered on the
    # subscription. Make both prerequisites idempotent.
    echo "=== Ensuring aks-preview Azure CLI extension is installed ==="
    if ! az extension show --name aks-preview >/dev/null 2>&1; then
      az extension add --name aks-preview --yes
    else
      az extension update --name aks-preview >/dev/null || true
    fi

    echo "=== Ensuring ManagedGatewayAPIPreview feature flag is registered ==="
    FEATURE_STATE=$(az feature show \
      --namespace Microsoft.ContainerService \
      --name ManagedGatewayAPIPreview \
      --query properties.state -o tsv 2>/dev/null || echo "NotRegistered")
    if [[ "${FEATURE_STATE}" != "Registered" ]]; then
      echo "Registering ManagedGatewayAPIPreview (current state: ${FEATURE_STATE})..."
      az feature register \
        --namespace Microsoft.ContainerService \
        --name ManagedGatewayAPIPreview >/dev/null
      # Wait until the feature transitions to Registered (usually <2 min,
      # can take up to ~15 min the first time).
      for _ in $(seq 1 60); do
        FEATURE_STATE=$(az feature show \
          --namespace Microsoft.ContainerService \
          --name ManagedGatewayAPIPreview \
          --query properties.state -o tsv 2>/dev/null || echo "")
        [[ "${FEATURE_STATE}" == "Registered" ]] && break
        sleep 15
      done
      if [[ "${FEATURE_STATE}" != "Registered" ]]; then
        echo "WARNING: ManagedGatewayAPIPreview not Registered after wait (state=${FEATURE_STATE}). Continuing — az aks create will fail loudly if it is truly required." >&2
      fi
      # Propagate the registration to the ContainerService RP.
      az provider register --namespace Microsoft.ContainerService >/dev/null || true
    fi
    ;;
  upstream)
    ;;
  *)
    echo "Invalid E2E_PROVIDER='${E2E_PROVIDER}'. Must be 'upstream' or 'azure'." >&2
    exit 1
    ;;
esac

echo "=== Creating AKS cluster ${CLUSTER_NAME} (provider=${E2E_PROVIDER}) ==="
az aks create \
  --resource-group "${RESOURCE_GROUP}" \
  --name "${CLUSTER_NAME}" \
  --node-count "${NODE_COUNT}" \
  --node-vm-size "${NODE_VM_SIZE}" \
  --enable-managed-identity \
  --attach-acr "${ACR_NAME}" \
  --network-plugin azure \
  --network-plugin-mode overlay \
  --network-dataplane cilium \
  --network-policy cilium \
  --generate-ssh-keys \
  ${AKS_K8S_VERSION:+--kubernetes-version "${AKS_K8S_VERSION}"} \
  ${EXTRA_AKS_ARGS[@]+"${EXTRA_AKS_ARGS[@]}"}

echo "=== Fetching kubeconfig ==="
az aks get-credentials \
  --resource-group "${RESOURCE_GROUP}" \
  --name "${CLUSTER_NAME}" \
  --overwrite-existing

echo "=== Waiting for all nodes to be Ready ==="
kubectl wait --for=condition=ready nodes --all --timeout=300s

# ─────────────────────────────────────────────────────────────────────────
# Cluster-prep components (KEDA + Gateway API base CRDs + Istio)
#
# These pieces sit BETWEEN cluster creation and application install:
# they are infrastructure prerequisites every E2E run needs, and on AKS
# some of them are sourced from managed add-ons enabled at cluster-create
# time (KEDA, base Gateway API CRDs). Installing them here keeps the
# upstream and azure provider paths converging into the SAME end state
# before install-components.sh runs — that script is therefore
# provider-agnostic and never has to branch on E2E_PROVIDER for these
# components.
#
# Per-provider behavior:
#   azure    → managed KEDA + managed Gateway API CRDs are already
#              installed by `az aks create --enable-keda
#              --enable-gateway-api`. We only wait for their controllers
#              / CRDs to be served. Istio is still installed via
#              istioctl here.
#   upstream → install KEDA via Helm into a dedicated `keda` namespace
#              and apply the upstream Gateway API base CRDs via kubectl.
#              Istio is installed via istioctl.
#
# Istio is installed here (rather than in install-components.sh's
# phase1-base) so the productionstack umbrella chart — which ships an
# EnvoyFilter requiring networking.istio.io/v1alpha3 to be Established
# — can install in parallel with KAITO et al. without racing on the
# Istio control plane being up.
#
# GAIE (Gateway API Inference Extension) CRDs are installed later by
# install-components.sh in phase1-base — no AKS managed add-on covers
# them today, and keeping them in install-components.sh lets the apply
# run in parallel with the KAITO install (which bundles the same CRDs).
# ─────────────────────────────────────────────────────────────────────────

# Validate the versions required by this section. Defaults can be sourced
# from versions.env via run-e2e-local.sh / CI before calling this script.
: "${GATEWAY_API_VERSION:?GATEWAY_API_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${KEDA_VERSION:?KEDA_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${ISTIO_VERSION:?ISTIO_VERSION is not set. Source versions.env or export it before calling this script.}"

# Derive KEDA install namespace from provider when not explicitly provided.
if [[ -z "${KEDA_NAMESPACE:-}" ]]; then
  case "${E2E_PROVIDER}" in
    upstream) KEDA_NAMESPACE="keda" ;;
    azure)    KEDA_NAMESPACE="kube-system" ;;
  esac
fi
export KEDA_NAMESPACE

# Ensure helm is available for the upstream KEDA install. (Prep is done
# sequentially before fan-out so all parallel tasks see helm on PATH.)
if ! command -v helm &>/dev/null; then
  echo "=== Installing helm ==="
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash
fi

# Ensure istioctl is available before fan-out so the parallel install
# task does not need to download it under a forked subshell.
if ! command -v istioctl &>/dev/null; then
  echo "=== Installing istioctl ${ISTIO_VERSION} ==="
  curl -L https://istio.io/downloadIstio | ISTIO_VERSION="${ISTIO_VERSION}" sh -
  export PATH="${PWD}/istio-${ISTIO_VERSION}/bin:${PATH}"
fi
echo "Using istioctl: $(command -v istioctl)"

# Source the shared run_phase / fmt_dur helpers so KEDA, Gateway API
# CRDs, and Istio can install concurrently.
# shellcheck source=lib-parallel.sh
source "${SCRIPT_DIR}/lib-parallel.sh"

# ── KEDA ──────────────────────────────────────────────────────────────────
install_keda() {
  case "${E2E_PROVIDER}" in
    azure)
      echo "=== Verifying managed KEDA add-on in ${KEDA_NAMESPACE} ==="
      kubectl -n "${KEDA_NAMESPACE}" rollout status deployment/keda-operator --timeout=180s || true
      kubectl -n "${KEDA_NAMESPACE}" rollout status deployment/keda-operator-metrics-apiserver --timeout=180s || true
      ;;
    upstream)
      echo "=== Installing KEDA ${KEDA_VERSION} via Helm into ${KEDA_NAMESPACE} ==="
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
      ;;
  esac
}

# ── Gateway API base CRDs ────────────────────────────────────────────────
install_gateway_api_crds() {
  case "${E2E_PROVIDER}" in
    azure)
      echo "=== Verifying managed Gateway API CRDs are served ==="
      # The managed add-on installs the standard-channel CRDs at
      # cluster-create time. Block briefly in case the install hasn't
      # propagated yet, then fall back to upstream install if it never does.
      local served=0
      for _ in $(seq 1 30); do
        if kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1; then
          echo "  ✅ gateways CRD is served by the managed add-on"
          served=1
          break
        fi
        sleep 2
      done
      if [[ "${served}" -ne 1 ]]; then
        echo "  ❌ Managed Gateway API CRDs not present after 60s — falling back to upstream install"
        kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
      fi
      ;;
    upstream)
      echo "=== Installing Gateway API CRDs ${GATEWAY_API_VERSION} ==="
      kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
      ;;
  esac
}

# GAIE (Gateway API Inference Extension) CRDs are installed by
# install-components.sh in phase1-base (same for every provider — no
# AKS managed add-on covers them today). Keeping them there lets the
# install run in parallel with KAITO, which bundles the same CRDs.

# ── Istio control plane ──────────────────────────────────────────────────
# Install the istiod control plane (Istio core CRDs + namespace + istiod
# Deployment) into istio-system. The EnvoyFilter CRD that the
# productionstack BBR subchart depends on is part of "Istio core" and is
# therefore Established before istiod's rollout completes.
install_istio() {
  echo "=== Installing Istio ${ISTIO_VERSION} ==="
  istioctl install \
    --set profile=minimal \
    --set hub=docker.io/istio \
    --set tag="${ISTIO_VERSION}" \
    --set "values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true" \
    -y

  echo "⏳ Waiting for istiod..."
  kubectl -n istio-system rollout status deployment/istiod --timeout=300s
}

# Fan out the three independent installs in parallel. They share no
# runtime state and each writes to a distinct set of API objects, so the
# longest task gates this phase.
run_phase cluster-prep \
  install_keda \
  install_gateway_api_crds \
  install_istio

echo ""
echo "✅ AKS cluster ${CLUSTER_NAME} is ready."
echo ""
kubectl get nodes -o wide

print_timing_summary "setup-cluster.sh timing summary"
