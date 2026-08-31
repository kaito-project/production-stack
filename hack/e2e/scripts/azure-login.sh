#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# azure-login.sh — Log in with the self-hosted runner's managed identity and
# verify the subscription every E2E resource (RG, ACR, AKS cluster) is created
# in. Prints the subscription id on stdout; progress output goes to stderr.
#
# The runner's managed identity can access more than one subscription, and
# `az login --identity` simply leaves whichever one Azure reports as default
# active. That default is not stable: when it moves, resources are created in a
# subscription where the identity holds no role-assignment rights and
# `az aks create --attach-acr` fails with "Could not create a role assignment
# for ACR".
#
# The id comes from E2E_SUBSCRIPTION_ID, which the E2E runner provides in its
# own environment, rather than from a GitHub secret or variable: GitHub passes
# neither to workflows triggered by a pull request from a fork, which is how
# the E2E PR workflow runs.
#
# `az account set` is deliberately NOT used to apply it: the CLI profile is
# global state on a runner shared by concurrent jobs, so one job would flip the
# active subscription out from under another. Every az call in hack/e2e passes
# --subscription "${AZURE_SUBSCRIPTION_ID}" instead.
#
# Environment variables:
#   E2E_SUBSCRIPTION_ID   — subscription id, provided by the runner
#   AZURE_SUBSCRIPTION_ID — overrides it (used for local runs)
# ---------------------------------------------------------------------------
set -euo pipefail

SUBSCRIPTION_ID="${AZURE_SUBSCRIPTION_ID:-${E2E_SUBSCRIPTION_ID:-}}"

if [[ -z "${SUBSCRIPTION_ID}" ]]; then
  cat >&2 <<'EOF'
ERROR: no Azure subscription id available.

The E2E runner is expected to export E2E_SUBSCRIPTION_ID; set it in the
runner's environment, or export AZURE_SUBSCRIPTION_ID for a local run. Falling
back to the managed identity's default subscription is deliberately
unsupported — that default has moved before and silently provisioned the
cluster in the wrong subscription.
EOF
  exit 1
fi

az login --identity --output none >&2

RESOLVED_SUBSCRIPTION_ID="$(az account show --subscription "${SUBSCRIPTION_ID}" --query id -o tsv)"
if [[ "${RESOLVED_SUBSCRIPTION_ID}" != "${SUBSCRIPTION_ID}" ]]; then
  echo "ERROR: the runner identity cannot reach subscription ${SUBSCRIPTION_ID}." >&2
  exit 1
fi

echo "=== Azure subscription for this run: ${SUBSCRIPTION_ID} ===" >&2
echo "${SUBSCRIPTION_ID}"
