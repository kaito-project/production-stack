#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# setup-cluster.sh — Create an AKS cluster + ACR for E2E testing.
#
# Environment variables (all required unless defaults are acceptable):
#   RESOURCE_GROUP   — Azure resource group name  (default: kaito-rg)
#   CLUSTER_NAME     — AKS cluster name           (default: kaito-aks)
#   ACR_NAME         — ACR registry name           (default: <cluster_name>acr, sanitized)
#   LOCATION         — Azure region               (default: westus2)
#   NODE_COUNT       — Number of worker nodes      (default: 2)
#   NODE_VM_SIZE     — VM SKU for the node pool    (default: Standard_D4s_v3)
#
# Outputs (exported for use by install-components.sh):
#   ACR_LOGIN_SERVER — e.g. kaitoaksacr.azurecr.io
# ---------------------------------------------------------------------------
set -euo pipefail

RESOURCE_GROUP="${RESOURCE_GROUP:-kaito-rg}"
CLUSTER_NAME="${CLUSTER_NAME:-kaito-aks}"
# ACR names must be alphanumeric, 5-50 chars. Strip dashes from cluster name.
ACR_NAME="${ACR_NAME:-$(echo "${CLUSTER_NAME}acr" | tr -d '-' | head -c 50)}"
LOCATION="${LOCATION:-westus2}"
NODE_COUNT="${NODE_COUNT:-2}"
NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D8s_v3}"

echo "=== Adding extensions and registering feature flags for Automatic cluster ==="
az extension add --name aks-preview
az extension update --name aks-preview

az feature register --namespace Microsoft.ContainerService --name AutomaticSKUPreview

echo "=== Waiting for AutomaticSKUPreview feature to be registered ==="
while true; do
  STATE=$(az feature show --namespace Microsoft.ContainerService --name AutomaticSKUPreview --query "properties.state" -o tsv)
  echo "  AutomaticSKUPreview state: ${STATE}"
  if [[ "${STATE}" == "Registered" ]]; then
    break
  fi
  echo "  Waiting 30s for registration to complete..."
  sleep 30
done

echo "=== Propagating feature registration to provider ==="
az provider register --namespace Microsoft.ContainerService

echo "=== Waiting for Microsoft.ContainerService provider to be registered ==="
while true; do
  STATE=$(az provider show --namespace Microsoft.ContainerService --query "registrationState" -o tsv)
  echo "  Provider state: ${STATE}"
  if [[ "${STATE}" == "Registered" ]]; then
    break
  fi
  echo "  Waiting 30s for provider registration to propagate..."
  sleep 30
done

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

echo "=== Creating AKS cluster ${CLUSTER_NAME} ==="
az aks create \
  --resource-group "${RESOURCE_GROUP}" \
  --name "${CLUSTER_NAME}" \
  --sku automatic \
  --node-count "${NODE_COUNT}" \
  --node-vm-size "${NODE_VM_SIZE}" \
  --zones 1 2 3 \
  --enable-managed-identity \
  --attach-acr "${ACR_NAME}" \
  --safeguards-level Warning \
  --safeguards-excluded-ns "kaito-system,istio-system,default" \
  --generate-ssh-keys

echo "=== Granting Azure Kubernetes Service RBAC Cluster Admin to current identity ==="
AKS_ID=$(az aks show --resource-group "${RESOURCE_GROUP}" --name "${CLUSTER_NAME}" --query id -o tsv)

# Extract the principal's object ID from the access token (JWT uses base64url encoding)
TOKEN_PAYLOAD=$(az account get-access-token --query accessToken -o tsv | cut -d. -f2)
TOKEN_PAYLOAD=$(echo "${TOKEN_PAYLOAD}" | tr '_-' '/+' | awk '{while(length % 4) $0=$0"="; print}')

CURRENT_USER=$(echo "${TOKEN_PAYLOAD}" | base64 -d | jq -r '.oid')
if [[ -z "${CURRENT_USER}" || "${CURRENT_USER}" == "null" ]]; then
  echo "ERROR: Could not determine current user/identity principal ID"
  exit 1
fi

echo "  Assigning role to principal: ${CURRENT_USER}"
az role assignment create \
  --assignee "${CURRENT_USER}" \
  --role "Azure Kubernetes Service RBAC Cluster Admin" \
  --scope "${AKS_ID}"

echo "=== Fetching kubeconfig ==="
az aks get-credentials \
  --resource-group "${RESOURCE_GROUP}" \
  --name "${CLUSTER_NAME}" \
  --overwrite-existing

# Convert kubeconfig to use azurecli for auth
kubelogin convert-kubeconfig -l azurecli

echo "=== Waiting for all nodes to be Ready ==="
kubectl wait --for=condition=ready nodes --all --timeout=300s

echo ""
echo "✅ AKS cluster ${CLUSTER_NAME} is ready."
echo "   ACR: ${ACR_LOGIN_SERVER}"
echo ""
kubectl get nodes -o wide
