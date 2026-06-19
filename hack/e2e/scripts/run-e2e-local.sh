#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# run-e2e-local.sh — Run the full E2E environment locally.
#
# Usage:
#   hack/e2e/scripts/run-e2e-local.sh           # full cycle: setup → install → validate → test → teardown
#   hack/e2e/scripts/run-e2e-local.sh setup      # only create AKS cluster (RG+ACR must already exist; run prepare-image.sh first)
#   hack/e2e/scripts/run-e2e-local.sh install     # only install components (cluster must exist)
#   hack/e2e/scripts/run-e2e-local.sh validate    # only validate components
#   hack/e2e/scripts/run-e2e-local.sh test        # only run Go e2e tests
#   hack/e2e/scripts/run-e2e-local.sh teardown    # only tear down cluster
#
# Environment variables (override defaults as needed):
#   RESOURCE_GROUP   (default: kaito-e2e-local)
#   CLUSTER_NAME     (default: kaito-e2e-local)
#   LOCATION         (default: australiaeast)
#   NODE_COUNT       (default: 2)
#   NODE_VM_SIZE     (default: Standard_D8d_v4)
#   E2E_PARALLEL     (default: 2) — Ginkgo parallel worker count
#   SKIP_TEARDOWN    (default: false) — set to "true" to keep cluster after tests
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# ── Load versions.env exactly once and export for child scripts ───────────
# Save any caller-provided overrides before sourcing defaults.
_PROVIDER="${E2E_PROVIDER:-}"
_ISTIO="${ISTIO_VERSION:-}"
_GWAPI="${GATEWAY_API_VERSION:-}"
_KEDA="${KEDA_VERSION:-}"

# shellcheck source=../../../versions.env
source "${REPO_ROOT}/versions.env"

# Restore caller overrides (env vars take precedence over file).
[ -n "${_PROVIDER}" ] && E2E_PROVIDER="${_PROVIDER}"
[ -n "${_ISTIO}" ] && ISTIO_VERSION="${_ISTIO}"
[ -n "${_GWAPI}" ] && GATEWAY_API_VERSION="${_GWAPI}"
[ -n "${_KEDA}" ]  && KEDA_VERSION="${_KEDA}"

# Validate provider value.
case "${E2E_PROVIDER}" in
  upstream|azure) ;;
  *)
    echo "❌ Invalid E2E_PROVIDER='${E2E_PROVIDER}'. Must be 'upstream' or 'azure'." >&2
    exit 1
    ;;
esac

# Derive KEDA install namespace from provider.
#   upstream → install KEDA via Helm into the dedicated `keda` namespace.
#   azure    → KEDA is provided by the AKS managed add-on, which lives in
#              `kube-system`. The keda-kaito-scaler chart must be installed
#              in the same namespace as KEDA so KEDA can resolve the
#              ClusterTriggerAuthentication Secrets it ships.
if [ -z "${KEDA_NAMESPACE:-}" ]; then
  case "${E2E_PROVIDER}" in
    upstream) KEDA_NAMESPACE="keda" ;;
    azure)    KEDA_NAMESPACE="kube-system" ;;
  esac
fi

export E2E_PROVIDER KEDA_NAMESPACE ISTIO_VERSION GATEWAY_API_VERSION KEDA_VERSION LLM_GATEWAY_AUTH_VERSION LLM_GATEWAY_AUTH_IMAGE_TAG AKS_K8S_VERSION

# Node-provisioner selection (decoupled from real-vs-mocked); exported so the
# child scripts (setup/install/validate) inherit canonical values:
#   KAITO_NODE_PROVISIONER — azure-gpu-provisioner | karpenter (default karpenter).
#   ENABLE_NODE_MOCKER     — true (default): deploy gpu-node-mocker; false: real provisioner.
export KAITO_NODE_PROVISIONER="${KAITO_NODE_PROVISIONER:-karpenter}"
export ENABLE_NODE_MOCKER="${ENABLE_NODE_MOCKER:-true}"

echo "=== Component versions (from versions.env) ==="
echo "  E2E_PROVIDER:              ${E2E_PROVIDER}"
echo "  KEDA_NAMESPACE:            ${KEDA_NAMESPACE}"
echo "  ISTIO_VERSION:             ${ISTIO_VERSION}"
echo "  GATEWAY_API_VERSION:       ${GATEWAY_API_VERSION}"
echo "  KEDA_VERSION:              ${KEDA_VERSION}"
echo ""

export RESOURCE_GROUP="${RESOURCE_GROUP:-kaito-e2e-local}"
export CLUSTER_NAME="${CLUSTER_NAME:-kaito-e2e-local}"
export LOCATION="${LOCATION:-australiaeast}"
export NODE_COUNT="${NODE_COUNT:-2}"
export NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D8d_v4}"
export E2E_PARALLEL="${E2E_PARALLEL:-2}"
SKIP_TEARDOWN="${SKIP_TEARDOWN:-false}"

STEP="${1:-all}"

cleanup() {
  local exit_code=$?
  print_step_timings
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

# ── Step-level timing ─────────────────────────────────────────────────────
# Tracks wall-clock per top-level do_<step> invocation so we can spot the
# real bottleneck (cluster create vs. image build vs. component install vs.
# test run). Printed once just before exit by `print_step_timings`.
STEP_TIMINGS=()  # "<step>=<seconds>"
fmt_dur_local() {
  local s="$1"
  if (( s < 60 )); then
    printf '%ds' "${s}"
  else
    printf '%dm%02ds' "$((s/60))" "$((s%60))"
  fi
}
time_step() {
  local label="$1"; shift
  local start=${SECONDS}
  echo ""
  echo "▶︎ [step:${label}] starting at $(date '+%H:%M:%S')"
  "$@"
  local d=$((SECONDS - start))
  STEP_TIMINGS+=("${label}=${d}")
  echo "✔ [step:${label}] finished in $(fmt_dur_local "${d}")"
}
print_step_timings() {
  [[ ${#STEP_TIMINGS[@]} -eq 0 ]] && return 0
  echo ""
  echo "================ run-e2e-local.sh timing summary ================"
  local total=0
  for entry in "${STEP_TIMINGS[@]}"; do
    name="${entry%%=*}"
    secs="${entry##*=}"
    total=$((total + secs))
    printf '  %-15s %s\n' "${name}" "$(fmt_dur_local "${secs}")"
  done
  printf '  %-15s %s\n' "TOTAL" "$(fmt_dur_local "${total}")"
  echo "=================================================================="
}

do_setup() {
  echo "=== Setting up cluster ==="
  "${SCRIPT_DIR}/setup-cluster.sh"
}

do_install() {
  # SHADOW_CONTROLLER_IMAGE is required only when gpu-node-mocker is deployed
  # (ENABLE_NODE_MOCKER=true). With a real provisioner (Karpenter / AKS NAP)
  # no mocker is deployed, so the image is not needed.
  if [[ "${ENABLE_NODE_MOCKER}" == "true" && -z "${SHADOW_CONTROLLER_IMAGE:-}" ]]; then
    echo "❌ SHADOW_CONTROLLER_IMAGE is not set. Run prepare-image.sh first and export the resulting image= value." >&2
    exit 1
  fi
  if [[ -z "${STATUS_REPORTER_IMAGE:-}" ]]; then
    echo "❌ STATUS_REPORTER_IMAGE is not set. Run prepare-image.sh first and export the resulting status_reporter_image= value." >&2
    exit 1
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
  setup)      time_step setup      do_setup ;;
  install)    time_step install    do_install ;;
  validate)   time_step validate   do_validate ;;
  test)       time_step test       do_test ;;
  teardown)   time_step teardown   do_teardown ;;
  all)
    trap cleanup EXIT
    time_step setup    do_setup
    time_step install  do_install
    time_step validate do_validate
    time_step test     do_test
    ;;
  *)
    echo "Unknown step: ${STEP}"
    echo "Usage: $0 [setup|install|validate|test|teardown|all]"
    exit 1
    ;;
esac

# Print timings for non-`all` invocations (the `all` path emits them via the
# cleanup trap so the table appears even on early failure).
if [[ "${STEP}" != "all" ]]; then
  print_step_timings
fi
