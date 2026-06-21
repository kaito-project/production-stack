#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# lib-node-provisioner.sh — Node-provisioner abstraction for the E2E pipeline.
#
# KAITO provisions GPU nodes for inference workspaces through a pluggable
# "node provisioner". The E2E suite supports two interchangeable
# implementations, selected per test scenario via the KAITO_NODE_PROVISIONER
# environment variable:
#
#   gpu-node-mocker (default) — a controller that fabricates fake GPU nodes
#                               and shadow pods; no real Azure VMs are created.
#   karpenter                 — real Karpenter / AKS NAP provisioning of GPU
#                               VMs, backed by the local CSI driver.
#
# Dispatch model
# --------------
# A script that has provisioner-specific behavior for a phase defines an
# implementation hook named:
#
#     np_<impl-key>__<hook>      e.g. np_karpenter__validate
#
# where <impl-key> is the provisioner name with dashes replaced by
# underscores (gpu-node-mocker → gpu_node_mocker). The script then calls:
#
#     node_provisioner_run <hook> [args...]
#
# which dispatches to the hook for the active provisioner. A missing hook is
# treated as a no-op, so an implementation only defines the behaviors that
# differ from the default.
# ---------------------------------------------------------------------------

# Guard against double-sourcing within a single process.
if [[ "${__E2E_LIB_NODE_PROVISIONER_SOURCED:-}" == "1" ]]; then
  return 0
fi
__E2E_LIB_NODE_PROVISIONER_SOURCED=1

# Canonicalize the selected provisioner. Empty/unset defaults to gpu-node-mocker
# (the legacy behavior — gpu-node-mocker was the only provisioner originally).
NODE_PROVISIONER="${KAITO_NODE_PROVISIONER:-gpu-node-mocker}"
[[ -z "${NODE_PROVISIONER}" ]] && NODE_PROVISIONER="gpu-node-mocker"
case "${NODE_PROVISIONER}" in
  gpu-node-mocker|karpenter) ;;
  *)
    echo "Invalid KAITO_NODE_PROVISIONER='${KAITO_NODE_PROVISIONER}'. Must be 'gpu-node-mocker' or 'karpenter'." >&2
    exit 1
    ;;
esac
export NODE_PROVISIONER

# Function-name-safe key for hook dispatch (dashes → underscores).
_NODE_PROVISIONER_KEY="${NODE_PROVISIONER//-/_}"

# ── node_provisioner_is <name> ────────────────────────────────────────────
# Predicate: is the active provisioner exactly <name>?
node_provisioner_is() {
  [[ "${NODE_PROVISIONER}" == "$1" ]]
}

# ── Capability predicates ─────────────────────────────────────────────────
# Read clearly at call sites that only need a yes/no decision.

# Uses the gpu-node-mocker controller + its container image.
node_provisioner_uses_mocker() { node_provisioner_is gpu-node-mocker; }

# Provisions real nodes via Karpenter / AKS NAP.
node_provisioner_uses_karpenter() { node_provisioner_is karpenter; }

# ── node_provisioner_run <hook> [args...] ─────────────────────────────────
# Dispatch <hook> to the active implementation (np_<impl-key>__<hook>),
# forwarding any extra args. No-op when the implementation does not define it.
node_provisioner_run() {
  local hook="$1"; shift
  local fn="np_${_NODE_PROVISIONER_KEY}__${hook}"
  if declare -F "${fn}" >/dev/null 2>&1; then
    "${fn}" "$@"
  fi
}
