#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# teardown-cluster.sh — Delete the E2E AKS cluster and resource group.
#
# Environment variables:
#   RESOURCE_GROUP  — Azure resource group (default: kaito-gwie-e2e)
#   AZURE_SUBSCRIPTION_ID — subscription passed to every az call; unset falls
#                     back to the CLI default
# ---------------------------------------------------------------------------
set -euo pipefail

RESOURCE_GROUP="${RESOURCE_GROUP:-kaito-rg}"

# Passed per call rather than via `az account set`: the CLI profile is global
# state and the E2E runner is shared with concurrent jobs.
AZ_SUB=()
if [[ -n "${AZURE_SUBSCRIPTION_ID:-}" ]]; then
  AZ_SUB=(--subscription "${AZURE_SUBSCRIPTION_ID}")
fi

echo "=== Deleting resource group ${RESOURCE_GROUP} ==="
if [[ "$(az group exists ${AZ_SUB[@]+"${AZ_SUB[@]}"} --name "${RESOURCE_GROUP}")" != "true" ]]; then
  echo "✅ Resource group does not exist; nothing to tear down."
  exit 0
fi
az group delete ${AZ_SUB[@]+"${AZ_SUB[@]}"} \
  --name "${RESOURCE_GROUP}" \
  --yes \
  --no-wait

echo "✅ Resource group deletion initiated (async)."
