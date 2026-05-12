#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# validate-components.sh — Verify that all E2E infrastructure components are
# healthy before running tests.
#
# Exits with code 0 if all checks pass, non-zero otherwise.
# ---------------------------------------------------------------------------
set -euo pipefail

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

# ── Shadow-pod-controller (GPU node mocker) ──────────────────────────────
echo "=== Shadow-pod-controller ==="
if kubectl -n kaito-system wait --for=condition=ready pod -l app.kubernetes.io/name=gpu-node-mocker --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "gpu-node-mocker is Running"
else
  fail "gpu-node-mocker is NOT Running"
fi
kubectl -n kaito-system get pods -l app.kubernetes.io/name=gpu-node-mocker
echo ""

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
# BBR is installed into Istio's rootNamespace (istio-system) so its
# EnvoyFilter applies cluster-wide and per-case Gateways inherit
# body-based routing automatically. Validate it there.
echo "=== BBR (Body-Based Router) ==="
if kubectl -n istio-system wait --for=condition=ready pod -l app=body-based-router --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "BBR is Running"
else
  fail "BBR is NOT Running"
fi
kubectl -n istio-system get pods -l app=body-based-router 2>/dev/null || true
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
