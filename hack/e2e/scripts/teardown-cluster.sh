#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# teardown-cluster.sh — Delete the E2E AKS cluster and resource group.
#
# Environment variables:
#   RESOURCE_GROUP  — Azure resource group (default: kaito-gwie-e2e)
# ---------------------------------------------------------------------------
set -euo pipefail

RESOURCE_GROUP="${RESOURCE_GROUP:-kaito-rg}"

echo "=== Deleting resource group ${RESOURCE_GROUP} ==="
az group delete \
  --name "${RESOURCE_GROUP}" \
  --yes \
  --no-wait

echo "✅ Resource group deletion initiated (async)."
