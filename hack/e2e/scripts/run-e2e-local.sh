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
#   LOCATION         (default: westus2)
#   NODE_COUNT       (default: 2)
#   NODE_VM_SIZE     (default: Standard_D8s_v3)
#   SKIP_TEARDOWN    (default: false) — set to "true" to keep cluster after tests
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

export RESOURCE_GROUP="${RESOURCE_GROUP:-kaito-e2e-local}"
export CLUSTER_NAME="${CLUSTER_NAME:-kaito-e2e-local}"
export LOCATION="${LOCATION:-westus2}"
export NODE_COUNT="${NODE_COUNT:-2}"
export NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D8s_v3}"
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

derive_acr() {
  ACR_NAME="${ACR_NAME:-$(echo "${CLUSTER_NAME}acr" | tr -d '-' | head -c 50)}"
  ACR_LOGIN_SERVER=$(az acr show --name "${ACR_NAME}" --query loginServer -o tsv)
  export ACR_LOGIN_SERVER
  export SHADOW_CONTROLLER_IMAGE="${ACR_LOGIN_SERVER}/gpu-node-mocker:latest"
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

# TODO: implement real Go e2e tests and run them here
do_test() {
  echo "=== Running E2E tests ==="
  cd "${REPO_ROOT}"
  echo "TODO - go test -v -timeout 30m ./..."
}

do_teardown() {
  echo "=== Tearing down cluster ==="
  "${SCRIPT_DIR}/teardown-cluster.sh"
}

case "${STEP}" in
  setup)    do_setup ;;
  install)  do_install ;;
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
