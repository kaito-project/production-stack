#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# validate-components.sh — Verify that all E2E infrastructure components are
# healthy before running tests.
#
# Exits with code 0 if all checks pass, non-zero otherwise.
# ---------------------------------------------------------------------------
set -euo pipefail

# Node-provisioner selection (decoupled from real-vs-mocked):
#   KAITO_NODE_PROVISIONER — azure-gpu-provisioner | karpenter (default karpenter).
#   ENABLE_NODE_MOCKER     — true (default): validate gpu-node-mocker; false: real provisioner.
NODE_PROVISIONER="${KAITO_NODE_PROVISIONER:-karpenter}"
ENABLE_NODE_MOCKER="${ENABLE_NODE_MOCKER:-true}"

FAILED=0
TIMEOUT="${VALIDATE_TIMEOUT:-120s}"
E2E_PROVIDER="${E2E_PROVIDER:-upstream}"

# Derive KEDA namespace from provider when not explicitly provided.
if [[ -z "${KEDA_NAMESPACE:-}" ]]; then
  case "${E2E_PROVIDER}" in
    upstream) KEDA_NAMESPACE="keda" ;;
    azure)    KEDA_NAMESPACE="kube-system" ;;
    *)
      echo "Invalid E2E_PROVIDER='${E2E_PROVIDER}'. Must be 'upstream' or 'azure'." >&2
      exit 1
      ;;
  esac
fi

pass() { echo "  ✅ $*"; }
fail() { echo "  ❌ $*"; FAILED=1; }

# ── Cluster nodes ─────────────────────────────────────────────────────────
echo "=== Cluster nodes ==="
if kubectl wait --for=condition=ready nodes --all --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "All AKS nodes are Ready"
else
  fail "Some AKS nodes are not Ready"
fi
kubectl get nodes -o wide
echo ""

# ── KAITO controller ─────────────────────────────────────────────────────
echo "=== KAITO workspace controller ==="
if kubectl -n kaito-system wait --for=condition=ready pod -l app.kubernetes.io/name=workspace --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "KAITO controller is Running"
else
  fail "KAITO controller is NOT Running"
fi
kubectl -n kaito-system get pods -l app.kubernetes.io/name=workspace
echo ""

# ── Node provisioner ──────────────────────────────────────────────────────
# With ENABLE_NODE_MOCKER=true we validate the gpu-node-mocker controller; with
# a real provisioner we validate only the components that provisioner deploys.
validate_gpu_node_mocker() {
  echo "=== Shadow-pod-controller ==="
  if kubectl -n kaito-system wait --for=condition=ready pod -l app.kubernetes.io/name=gpu-node-mocker --timeout="${TIMEOUT}" >/dev/null 2>&1; then
    pass "gpu-node-mocker is Running"
  else
    fail "gpu-node-mocker is NOT Running"
  fi
  kubectl -n kaito-system get pods -l app.kubernetes.io/name=gpu-node-mocker
  echo ""
}

validate_karpenter_csi() {
  echo "=== Local CSI driver (Karpenter mode) ==="
  if kubectl -n kaito-system rollout status deployment/csi-local-manager --timeout="${TIMEOUT}" >/dev/null 2>&1; then
    pass "csi-local-manager is Available"
  else
    fail "csi-local-manager is NOT Available"
  fi

  if kubectl -n kaito-system rollout status daemonset/csi-local-node --timeout="${TIMEOUT}" >/dev/null 2>&1; then
    pass "csi-local-node DaemonSet is Ready"
  else
    fail "csi-local-node DaemonSet is NOT Ready"
  fi

  kubectl -n kaito-system get deploy csi-local-manager 2>/dev/null || true
  kubectl -n kaito-system get ds csi-local-node 2>/dev/null || true
  echo ""
}

if [[ "${ENABLE_NODE_MOCKER}" == "true" ]]; then
  validate_gpu_node_mocker
elif [[ "${NODE_PROVISIONER}" == "karpenter" ]]; then
  validate_karpenter_csi
fi


# ── Istio (istiod) ──────────────────────────────────────────────────────
echo "=== Istio ==="
if kubectl -n istio-system wait --for=condition=ready pod -l app=istiod --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "istiod is Running"
else
  fail "istiod is NOT Running"
fi
kubectl -n istio-system get pods -l app=istiod
echo ""

# ── BBR ──────────────────────────────────────────────────────────────────
# BBR is a workload-only singleton co-located with the umbrella release
# (kaito-system). The per-namespace ext_proc EnvoyFilter that wires it
# into each Gateway's HCM is rendered by charts/modelharness; here we
# only validate the BBR Deployment is Running.
echo "=== BBR (Body-Based Router) ==="
if kubectl -n kaito-system wait --for=condition=ready pod -l app=body-based-router --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "BBR is Running"
else
  fail "BBR is NOT Running"
fi
kubectl -n kaito-system get pods -l app=body-based-router 2>/dev/null || true
echo ""

# (Catch-all 404 handling is now produced by an EnvoyFilter
# direct_response rendered per-namespace by charts/modelharness — no
# cluster-shared Service exists to validate.)

# ── KEDA ─────────────────────────────────────────────────────────────────
echo "=== KEDA (namespace: ${KEDA_NAMESPACE}, provider: ${E2E_PROVIDER}) ==="
if kubectl -n "${KEDA_NAMESPACE}" wait --for=condition=ready pod -l app=keda-operator --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "keda-operator is Running"
else
  fail "keda-operator is NOT Running"
fi
if kubectl -n "${KEDA_NAMESPACE}" wait --for=condition=ready pod -l app=keda-operator-metrics-apiserver --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "keda-operator-metrics-apiserver is Running"
else
  fail "keda-operator-metrics-apiserver is NOT Running"
fi
kubectl -n "${KEDA_NAMESPACE}" get pods 2>/dev/null || true
echo ""

# ── KEDA Kaito Scaler ────────────────────────────────────────────────────
echo "=== KEDA Kaito Scaler (namespace: ${KEDA_NAMESPACE}) ==="
if kubectl -n "${KEDA_NAMESPACE}" wait --for=condition=ready pod -l app.kubernetes.io/name=keda-kaito-scaler --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "keda-kaito-scaler is Running"
else
  fail "keda-kaito-scaler is NOT Running"
fi
kubectl -n "${KEDA_NAMESPACE}" get pods -l app.kubernetes.io/name=keda-kaito-scaler 2>/dev/null || true
echo ""

# ── LLM Gateway Auth (apikey-operator) ──────────────────────────────────
echo "=== LLM Gateway Auth (operator) ==="
if kubectl -n llm-gateway-auth wait --for=condition=ready pod -l app.kubernetes.io/component=apikey-operator --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "apikey-operator is Running"
else
  fail "apikey-operator is NOT Running"
fi
kubectl -n llm-gateway-auth get pods -l app.kubernetes.io/component=apikey-operator 2>/dev/null || true
echo ""

# ── LLM Gateway Auth (apikey-authz) ─────────────────────────────────────
echo "=== LLM Gateway Auth (authz) ==="
if kubectl -n llm-gateway-auth wait --for=condition=ready pod -l app.kubernetes.io/component=apikey-authz --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "apikey-authz is Running"
else
  fail "apikey-authz is NOT Running"
fi
kubectl -n llm-gateway-auth get pods -l app.kubernetes.io/component=apikey-authz 2>/dev/null || true
echo ""

# ── ProductionStack Status Reporter ─────────────────────────────────────
# Leader-elected control-plane reporter installed by the umbrella chart into
# kaito-system. It publishes aggregated control-plane Warning Events into
# kube-system, which the status-reporter e2e specs assert on.
echo "=== ProductionStack Status Reporter ==="
if kubectl -n kaito-system wait --for=condition=ready pod -l app.kubernetes.io/name=productionstack-status-reporter --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "productionstack-status-reporter is Running"
else
  fail "productionstack-status-reporter is NOT Running"
fi
kubectl -n kaito-system get pods -l app.kubernetes.io/name=productionstack-status-reporter 2>/dev/null || true
echo ""

# ── CRDs ─────────────────────────────────────────────────────────────────
echo "=== CRDs ==="
for crd in \
  gateways.gateway.networking.k8s.io \
  httproutes.gateway.networking.k8s.io \
  inferencepools.inference.networking.k8s.io \
  inferencesets.kaito.sh \
  workspaces.kaito.sh \
  scaledobjects.keda.sh \
  clustertriggerauthentications.keda.sh \
  apikeys.kaito.sh; do
  if kubectl get crd "${crd}" >/dev/null 2>&1; then
    pass "CRD ${crd} exists"
  else
    fail "CRD ${crd} is MISSING"
  fi
done
echo ""

# ── Summary ──────────────────────────────────────────────────────────────
if [[ "$FAILED" -eq 0 ]]; then
  echo "=== All validation checks passed ✅ ==="
else
  echo "=== Some validation checks FAILED ❌ ==="
  exit 1
fi
