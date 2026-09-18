#!/usr/bin/env bash
# Shared declarative provider profiles for the E2E scripts.
# Add a provider by extending each table below and implementing its selected
# hooks in the owning scripts. Callers do not need provider conditionals.

if [[ "${__E2E_LIB_PROVIDER_SOURCED:-}" == "1" ]]; then
  return 0
fi
__E2E_LIB_PROVIDER_SOURCED=1

E2E_PROVIDER="${E2E_PROVIDER:-azure}"

_E2E_SUPPORTED_PROVIDERS=(upstream azure)
_E2E_PROVIDER_OPERATIONS=(
  configure_cluster
  verify_cluster
  prepare_istio_cli
  install_keda
  setup_istio
  prepare_kaito_crds
  install_gwie
  configure_productionstack
  validate_istio
)
declare -A _E2E_KEDA_NAMESPACE=(
  [upstream]="keda"
  [azure]="kube-system"
)
declare -A _E2E_ISTIO_NAMESPACE=(
  [upstream]="istio-system"
  [azure]="aks-istio-system"
)
declare -A _E2E_PROVIDER_HOOKS=(
  [upstream:configure_cluster]="e2e_provider_noop"
  [azure:configure_cluster]="configure_app_routing_cluster"
  [upstream:verify_cluster]="e2e_provider_noop"
  [azure:verify_cluster]="verify_managed_gwie"
  [upstream:prepare_istio_cli]="prepare_upstream_istio_cli"
  [azure:prepare_istio_cli]="e2e_provider_noop"
  [upstream:install_keda]="install_helm_keda"
  [azure:install_keda]="verify_managed_keda"
  [upstream:setup_istio]="install_istio"
  [azure:setup_istio]="install_app_routing_istio_prereqs"
  [upstream:prepare_kaito_crds]="e2e_provider_noop"
  [azure:prepare_kaito_crds]="prepare_managed_kaito_crds"
  [upstream:install_gwie]="install_upstream_gwie"
  [azure:install_gwie]="use_managed_gwie"
  [upstream:configure_productionstack]="e2e_provider_noop"
  [azure:configure_productionstack]="configure_app_routing_productionstack"
  [upstream:validate_istio]="e2e_provider_noop"
  [azure:validate_istio]="validate_app_routing_istio"
)

if [[ -z "${_E2E_KEDA_NAMESPACE[${E2E_PROVIDER}]+x}" ]]; then
  printf "Invalid E2E_PROVIDER='%s'. Supported providers: %s.\n" \
    "${E2E_PROVIDER}" "${_E2E_SUPPORTED_PROVIDERS[*]}" >&2
  return 1
fi

KEDA_NAMESPACE="${KEDA_NAMESPACE:-${_E2E_KEDA_NAMESPACE[${E2E_PROVIDER}]}}"
E2E_ISTIO_NAMESPACE="${_E2E_ISTIO_NAMESPACE[${E2E_PROVIDER}]}"

for operation in "${_E2E_PROVIDER_OPERATIONS[@]}"; do
  if [[ -z "${_E2E_PROVIDER_HOOKS[${E2E_PROVIDER}:${operation}]:-}" ]]; then
    echo "Provider '${E2E_PROVIDER}' does not define operation '${operation}'." >&2
    return 1
  fi
done
unset operation

# Explicitly represent operations a provider does not need. This keeps every
# provider profile complete and lets callers dispatch hooks unconditionally
# instead of reintroducing provider-specific or optional-hook checks.
e2e_provider_noop() {
  :
}

run_provider_hook() {
  local operation="$1"
  local hook="${_E2E_PROVIDER_HOOKS[${E2E_PROVIDER}:${operation}]:-}"
  if [[ -z "${hook}" ]]; then
    echo "Provider '${E2E_PROVIDER}' does not define operation '${operation}'." >&2
    return 1
  fi
  shift
  if ! declare -F "${hook}" >/dev/null; then
    echo "Provider '${E2E_PROVIDER}' operation '${operation}' uses undefined hook '${hook}'." >&2
    return 1
  fi
  "${hook}" "$@"
}

export E2E_PROVIDER KEDA_NAMESPACE E2E_ISTIO_NAMESPACE
