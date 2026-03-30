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
echo "=== BBR (Body-Based Router) ==="
if kubectl wait --for=condition=ready pod -l app=body-based-router --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "BBR is Running"
else
  fail "BBR is NOT Running"
fi
kubectl get pods -l app=body-based-router 2>/dev/null || true
echo ""

# ── Gateway pod ──────────────────────────────────────────────────────────
echo "=== Inference Gateway ==="
if kubectl wait --for=condition=ready pod -l gateway.networking.k8s.io/gateway-name=inference-gateway --timeout="${TIMEOUT}" >/dev/null 2>&1; then
  pass "Gateway pod is Running"
else
  fail "Gateway pod is NOT Running"
fi
kubectl get pods -l gateway.networking.k8s.io/gateway-name=inference-gateway 2>/dev/null || true
echo ""

# ── CRDs ─────────────────────────────────────────────────────────────────
echo "=== CRDs ==="
for crd in \
  gateways.gateway.networking.k8s.io \
  httproutes.gateway.networking.k8s.io \
  inferencepools.inference.networking.k8s.io; do
  if kubectl get crd "${crd}" >/dev/null 2>&1; then
    pass "CRD ${crd} exists"
  else
    fail "CRD ${crd} is MISSING"
  fi
done
echo ""

# ── 1. Inference pods Running ────────────────────────────────────────────
echo "=== Inference pods ==="
for name in falcon-7b-instruct ministral-3-3b-instruct; do
  label="inferenceset.kaito.sh/created-by=${name}"
  if kubectl wait --for=condition=ready pod -l "${label}" --timeout="${TIMEOUT}" >/dev/null 2>&1; then
    pass "Pods for ${name} are Running"
  else
    fail "Pods for ${name} are NOT Running"
  fi
  kubectl get pods -l "${label}" 2>/dev/null || true
done
echo ""

# ── 2. InferencePools exist with Running EPP ─────────────────────────────
echo "=== InferencePools ==="
for pool in falcon-7b-instruct-inferencepool ministral-3-3b-instruct-inferencepool; do
  if kubectl get inferencepool "${pool}" >/dev/null 2>&1; then
    # Check that the EPP pod for this pool is Running (try multiple label patterns)
    EPP_READY=$(kubectl get pods --no-headers 2>/dev/null | grep "${pool}-epp" | grep -c "Running" || true)
    if [[ "${EPP_READY:-0}" -gt 0 ]]; then
      pass "InferencePool ${pool} exists, EPP Running"
    else
      fail "InferencePool ${pool} exists but EPP pod is not Running"
    fi
  else
    fail "InferencePool ${pool} is MISSING"
  fi
done
echo ""

# ── 3. InferenceSets replicas ready ──────────────────────────────────────
echo "=== InferenceSets ==="
for ws in falcon-7b-instruct ministral-3-3b-instruct; do
  if kubectl get inferenceset "${ws}" >/dev/null 2>&1; then
    READY=$(kubectl get inferenceset "${ws}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    DESIRED=$(kubectl get inferenceset "${ws}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "?")
    if [[ "${READY}" == "${DESIRED}" && "${READY}" != "0" ]]; then
      pass "InferenceSet ${ws} ready=${READY}/${DESIRED}"
    else
      fail "InferenceSet ${ws} ready=${READY}/${DESIRED}"
    fi
  else
    fail "InferenceSet ${ws} is MISSING"
  fi
done
echo ""

# ── 4. HTTPRoute Accepted=True ───────────────────────────────────────────
echo "=== HTTPRoute ==="
ROUTE_ACCEPTED=$(kubectl get httproute llm-route \
  -o jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}' 2>/dev/null || echo "")
if [[ "${ROUTE_ACCEPTED}" == "True" ]]; then
  pass "HTTPRoute llm-route Accepted=True"
else
  fail "HTTPRoute llm-route Accepted=${ROUTE_ACCEPTED:-<not found>}"
fi
echo ""

# ── 5. DestinationRules exist ────────────────────────────────────────────
echo "=== DestinationRules ==="
for dr in falcon-7b-instruct-inferencepool-epp ministral-3-3b-instruct-inferencepool-epp; do
  if kubectl get destinationrule "${dr}" >/dev/null 2>&1; then
    pass "DestinationRule ${dr} exists"
  else
    fail "DestinationRule ${dr} is MISSING"
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
