#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# prepare-image.sh — Create the resource group + ACR, then build and push
# the gpu-node-mocker image into ACR.
#
# This step is intentionally split from setup-cluster.sh so that AKS-create
# wall time can be measured in isolation. Both `az group create` and
# `az acr create` are idempotent, so this script is safe to re-run.
#
# Environment variables:
#   RESOURCE_GROUP   — Azure resource group name (default: kaito-rg)
#   CLUSTER_NAME     — AKS cluster name          (default: kaito-aks)
#   ACR_NAME         — ACR registry name         (default: <cluster_name>acr, sanitized)
#   LOCATION         — Azure region              (default: australiaeast)
#   IMG              — Local docker tag for the gpu-node-mocker image
#                      (default: gpu-node-mocker:latest)
#
# Outputs (on stdout):
#   image=<acr-fqdn>/gpu-node-mocker:<tag>
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

RESOURCE_GROUP="${RESOURCE_GROUP:-kaito-rg}"
CLUSTER_NAME="${CLUSTER_NAME:-kaito-aks}"
# ACR names must be alphanumeric, 5-50 chars. Strip dashes from cluster name.
ACR_NAME="${ACR_NAME:-$(echo "${CLUSTER_NAME}acr" | tr -d '-' | head -c 50)}"
LOCATION="${LOCATION:-australiaeast}"
IMG="${IMG:-gpu-node-mocker:latest}"

# Verify the container tool used by the Makefile (docker/podman) is installed
# and its daemon is reachable. Fail fast here so users get an actionable error
# before `az group create` runs, instead of a cryptic failure midway through
# `make docker-build`.
CONTAINER_TOOL="${CONTAINER_TOOL:-$(command -v docker 2>/dev/null || command -v podman 2>/dev/null || true)}"
if [[ -z "${CONTAINER_TOOL}" ]]; then
  echo "Error: neither 'docker' nor 'podman' found in PATH." >&2
  echo "Install Docker (e.g. 'sudo apt-get install docker.io') or set CONTAINER_TOOL." >&2
  exit 1
fi
if ! "${CONTAINER_TOOL}" info &>/dev/null; then
  echo "Error: '${CONTAINER_TOOL}' is installed but the daemon is not running." >&2
  echo "Start it with: sudo systemctl start $(basename "${CONTAINER_TOOL}")" >&2
  exit 1
fi
export CONTAINER_TOOL

echo "=== Creating resource group ${RESOURCE_GROUP} in ${LOCATION} ===" >&2
az group create --name "${RESOURCE_GROUP}" --location "${LOCATION}" >&2

echo "=== Creating ACR ${ACR_NAME} ===" >&2
az acr create --resource-group "${RESOURCE_GROUP}" --name "${ACR_NAME}" --sku Basic >&2

echo "=== Building gpu-node-mocker image (${IMG}) ===" >&2
IMG="${IMG}" make -C "${REPO_ROOT}" docker-build >&2

echo "=== Pushing gpu-node-mocker image to ACR (${ACR_NAME}) ===" >&2
ACR_NAME="${ACR_NAME}" IMG="${IMG}" make -C "${REPO_ROOT}" e2e-push-image
