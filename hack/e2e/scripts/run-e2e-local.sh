#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# run-e2e-local.sh — Run the full E2E environment locally.
#
# Usage:
#   hack/e2e/scripts/run-e2e-local.sh           # full cycle: setup → install → validate → test → teardown
#   hack/e2e/scripts/run-e2e-local.sh setup      # only create cluster + build images
#   hack/e2e/scripts/run-e2e-local.sh install     # only install components (cluster must exist)
#   hack/e2e/scripts/run-e2e-local.sh validate    # only validate components
#   hack/e2e/scripts/run-e2e-local.sh test        # only run Go e2e tests
#   hack/e2e/scripts/run-e2e-local.sh teardown    # only tear down cluster
#
# Environment variables (override defaults as needed):
#   RESOURCE_GROUP   (default: kaito-e2e-local)
#   CLUSTER_NAME     (default: kaito-e2e-local)
#   LOCATION         (default: swedencentral)
#   NODE_COUNT       (default: 2)
#   NODE_VM_SIZE     (default: Standard_D8s_v5)
#   E2E_PARALLEL     (default: 2) — Ginkgo parallel worker count
#   SKIP_TEARDOWN    (default: false) — set to "true" to keep cluster after tests
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# ── Load versions.env exactly once and export for child scripts ───────────
# Save any caller-provided overrides before sourcing defaults.
_ISTIO="${ISTIO_VERSION:-}"
_GWAPI="${GATEWAY_API_VERSION:-}" _BBR="${BBR_VERSION:-}"
_KEDA="${KEDA_VERSION:-}" _KKS="${KEDA_KAITO_SCALER_VERSION:-}"

# shellcheck source=../../../versions.env
source "${REPO_ROOT}/versions.env"

# Restore caller overrides (env vars take precedence over file).
[ -n "${_ISTIO}" ] && ISTIO_VERSION="${_ISTIO}"
[ -n "${_GWAPI}" ] && GATEWAY_API_VERSION="${_GWAPI}"
[ -n "${_BBR}" ]   && BBR_VERSION="${_BBR}"
[ -n "${_KEDA}" ]  && KEDA_VERSION="${_KEDA}"
[ -n "${_KKS}" ]   && KEDA_KAITO_SCALER_VERSION="${_KKS}"

export ISTIO_VERSION GATEWAY_API_VERSION BBR_VERSION KEDA_VERSION KEDA_KAITO_SCALER_VERSION AKS_K8S_VERSION

echo "=== Component versions (from versions.env) ==="
echo "  ISTIO_VERSION:             ${ISTIO_VERSION}"
echo "  GATEWAY_API_VERSION:       ${GATEWAY_API_VERSION}"
echo "  BBR_VERSION:               ${BBR_VERSION}"
echo "  KEDA_VERSION:              ${KEDA_VERSION}"
echo "  KEDA_KAITO_SCALER_VERSION: ${KEDA_KAITO_SCALER_VERSION}"
echo ""

export RESOURCE_GROUP="${RESOURCE_GROUP:-kaito-e2e-local}"
export CLUSTER_NAME="${CLUSTER_NAME:-kaito-e2e-local}"
export LOCATION="${LOCATION:-swedencentral}"
export NODE_COUNT="${NODE_COUNT:-2}"
export NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D8s_v5}"
export E2E_PARALLEL="${E2E_PARALLEL:-2}"
SKIP_TEARDOWN="${SKIP_TEARDOWN:-false}"

STEP="${1:-all}"

cleanup() {
  local exit_code=$?
  if [[ "${SKIP_TEARDOWN}" == "true" ]]; then
    echo ""
    echo "⚠️  SKIP_TEARDOWN=true — cluster left running."
    echo "   To tear down later: RESOURCE_GROUP=${RESOURCE_GROUP} hack/e2e/scripts/teardown-cluster.sh"
    return
  fi
  if [[ "${STEP}" == "all" ]]; then
    echo ""
    echo "=== Tearing down cluster ==="
    "${SCRIPT_DIR}/teardown-cluster.sh" || true
  fi
  exit "${exit_code}"
}

CONTAINER_TOOL="${CONTAINER_TOOL:-$(command -v podman 2>/dev/null || command -v docker 2>/dev/null)}"

derive_acr() {
  ACR_NAME="${ACR_NAME:-$(echo "${CLUSTER_NAME}acr" | tr -d '-' | head -c 50)}"
  ACR_LOGIN_SERVER=$(az acr show --name "${ACR_NAME}" --query loginServer -o tsv)
  export ACR_LOGIN_SERVER
  export SHADOW_CONTROLLER_IMAGE="${ACR_LOGIN_SERVER}/gpu-node-mocker:latest"
}

do_build_push() {
  derive_acr
  echo "=== Building and pushing image to ACR (${ACR_NAME}) ==="
  TOKEN=$(az acr login --name "${ACR_NAME}" --expose-token --query accessToken -o tsv)
  echo "${TOKEN}" | "${CONTAINER_TOOL}" login "${ACR_LOGIN_SERVER}" --username 00000000-0000-0000-0000-000000000000 --password-stdin
  "${CONTAINER_TOOL}" build --platform linux/amd64 -f "${REPO_ROOT}/docker/Dockerfile" -t "${SHADOW_CONTROLLER_IMAGE}" "${REPO_ROOT}"
  "${CONTAINER_TOOL}" push "${SHADOW_CONTROLLER_IMAGE}"
}

do_setup() {
  echo "=== Setting up cluster ==="
  "${SCRIPT_DIR}/setup-cluster.sh"
}

do_install() {
  if [[ -z "${SHADOW_CONTROLLER_IMAGE:-}" ]]; then
    derive_acr
  fi
  echo "=== Installing components ==="
  "${SCRIPT_DIR}/install-components.sh"
}

do_validate() {
  echo "=== Validating components ==="
  "${SCRIPT_DIR}/validate-components.sh"
}

do_test() {
  echo "=== Running E2E tests (E2E_PARALLEL=${E2E_PARALLEL}) ==="
  cd "${REPO_ROOT}"
  # Use the Ginkgo CLI so --procs=N actually spawns parallel workers.
  # `go test` by itself only runs a single process and ignores --procs.
  go run github.com/onsi/ginkgo/v2/ginkgo \
    --procs="${E2E_PARALLEL}" \
    --timeout=30m \
    -v \
    ./test/e2e/...
}

do_teardown() {
  echo "=== Tearing down cluster ==="
  "${SCRIPT_DIR}/teardown-cluster.sh"
}

case "${STEP}" in
  setup)      do_setup ;;
  build-push) do_build_push ;;
  install)    do_install ;;
  validate) do_validate ;;
  test)     do_test ;;
  teardown) do_teardown ;;
  all)
    trap cleanup EXIT
    do_setup
    do_install
    do_validate
    do_test
    ;;
  *)
    echo "Unknown step: ${STEP}"
    echo "Usage: $0 [setup|install|validate|test|teardown|all]"
    exit 1
    ;;
esac
