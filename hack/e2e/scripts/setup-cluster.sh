#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# setup-cluster.sh — Create an AKS cluster + ACR for E2E testing.
#
# Environment variables (all required unless defaults are acceptable):
#   RESOURCE_GROUP   — Azure resource group name  (default: kaito-rg)
#   CLUSTER_NAME     — AKS cluster name           (default: kaito-aks)
#   ACR_NAME         — ACR registry name           (default: <cluster_name>acr, sanitized)
#   LOCATION         — Azure region               (default: australiaeast)
#   NODE_COUNT       — Number of worker nodes      (default: 2)
#   NODE_VM_SIZE     — VM SKU for the node pool    (default: Standard_D8s_v5)
#
# Outputs (exported for use by install-components.sh):
#   ACR_LOGIN_SERVER — e.g. kaitoaksacr.azurecr.io
# ---------------------------------------------------------------------------
set -euo pipefail

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

echo "=== Creating resource group ${RESOURCE_GROUP} in ${LOCATION} ==="
az group create \
  --name "${RESOURCE_GROUP}" \
  --location "${LOCATION}"

echo "=== Creating ACR ${ACR_NAME} ==="
az acr create \
  --resource-group "${RESOURCE_GROUP}" \
  --name "${ACR_NAME}" \
  --sku Basic

ACR_LOGIN_SERVER=$(az acr show --name "${ACR_NAME}" --query loginServer -o tsv)
export ACR_LOGIN_SERVER
echo "ACR login server: ${ACR_LOGIN_SERVER}"

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

echo ""
echo "✅ AKS cluster ${CLUSTER_NAME} is ready."
echo "   ACR: ${ACR_LOGIN_SERVER}"
echo ""
kubectl get nodes -o wide
