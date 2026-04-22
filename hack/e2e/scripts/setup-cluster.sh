#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# setup-cluster.sh — Create an AKS cluster + ACR for E2E testing.
#
# Environment variables (all required unless defaults are acceptable):
#   RESOURCE_GROUP   — Azure resource group name  (default: kaito-rg)
#   CLUSTER_NAME     — AKS cluster name           (default: kaito-aks)
#   ACR_NAME         — ACR registry name           (default: <cluster_name>acr, sanitized)
#   LOCATION         — Azure region               (default: swedencentral)
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
LOCATION="${LOCATION:-swedencentral}"
NODE_COUNT="${NODE_COUNT:-2}"
NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D8s_v5}"

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
  --node-count "${NODE_COUNT}" \
  --node-vm-size "${NODE_VM_SIZE}" \
  --enable-managed-identity \
  --attach-acr "${ACR_NAME}" \
  --network-policy azure \
  --generate-ssh-keys \
  ${AKS_K8S_VERSION:+--kubernetes-version "${AKS_K8S_VERSION}"}

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
