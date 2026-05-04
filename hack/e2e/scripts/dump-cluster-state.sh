#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# dump-cluster-state.sh — Print a debug snapshot of the cluster state.
# ---------------------------------------------------------------------------
set -euo pipefail

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║              CLUSTER STATE DEBUG SNAPSHOT                   ║"
echo "╚══════════════════════════════════════════════════════════════╝"

echo ""
echo "── Nodes ──────────────────────────────────────────────────────"
kubectl get nodes -o wide 2>/dev/null || true

echo ""
echo "── Pods (all namespaces) ──────────────────────────────────────"
kubectl get pods -A -o wide 2>/dev/null || true

echo ""
echo "── Non-running pods detail ────────────────────────────────────"
kubectl get pods -A --field-selector='status.phase!=Running,status.phase!=Succeeded' -o wide 2>/dev/null || true
for pod_ns in $(kubectl get pods -A --field-selector='status.phase!=Running,status.phase!=Succeeded' --no-headers 2>/dev/null | awk '{print $1"/"$2}'); do
  ns="${pod_ns%%/*}"
  pod="${pod_ns##*/}"
  echo ""
  echo "  ── describe ${ns}/${pod} ──"
  kubectl -n "${ns}" describe pod "${pod}" 2>/dev/null | tail -30 || true
  echo "  ── logs ${ns}/${pod} (last 50 lines) ──"
  kubectl -n "${ns}" logs "${pod}" --all-containers --tail=50 2>/dev/null || true
done

echo ""
echo "── Services (all namespaces) ──────────────────────────────────"
kubectl get svc -A 2>/dev/null || true

echo ""
echo "── Deployments (all namespaces) ───────────────────────────────"
kubectl get deployments -A -o wide 2>/dev/null || true

echo ""
echo "── KAITO resources ────────────────────────────────────────────"
kubectl get inferencesets -A 2>/dev/null || true
kubectl get inferencepools -A 2>/dev/null || true
kubectl get inferencemodels -A 2>/dev/null || true

echo ""
echo "── Gateways & HTTPRoutes ──────────────────────────────────────"
kubectl get gateways -A 2>/dev/null || true
kubectl get httproutes -A 2>/dev/null || true

echo ""
echo "── NetworkPolicies (all namespaces) ───────────────────────────"
kubectl get networkpolicies.networking.k8s.io -A 2>/dev/null || true
echo ""
echo "── NetworkPolicies (yaml, e2e namespaces) ─────────────────────"
for ns in $(kubectl get ns -o name 2>/dev/null | awk -F/ '/^namespace\/e2e-/{print $2}'); do
  np_count=$(kubectl -n "${ns}" get networkpolicies.networking.k8s.io --no-headers 2>/dev/null | wc -l | tr -d ' ')
  if [ "${np_count}" != "0" ]; then
    echo "  ── ${ns} ──"
    kubectl -n "${ns}" get networkpolicies.networking.k8s.io -o yaml 2>/dev/null || true
  fi
done

echo ""
echo "── CiliumNetworkPolicies / CiliumClusterwideNetworkPolicies ──"
kubectl get ciliumnetworkpolicies.cilium.io -A 2>/dev/null || true
kubectl get ciliumclusterwidenetworkpolicies.cilium.io 2>/dev/null || true

echo ""
echo "── CiliumEndpoints (e2e namespaces) ───────────────────────────"
for ns in $(kubectl get ns -o name 2>/dev/null | awk -F/ '/^namespace\/e2e-/{print $2}'); do
  ce_count=$(kubectl -n "${ns}" get ciliumendpoints.cilium.io --no-headers 2>/dev/null | wc -l | tr -d ' ')
  if [ "${ce_count}" != "0" ]; then
    echo "  ── ${ns} ──"
    kubectl -n "${ns}" get ciliumendpoints.cilium.io -o wide 2>/dev/null || true
  fi
done

echo ""
echo "── Cilium agent status (one node) ─────────────────────────────"
cilium_pod=$(kubectl -n kube-system get pods -l k8s-app=cilium -o name 2>/dev/null | head -1)
if [ -n "${cilium_pod}" ]; then
  kubectl -n kube-system exec "${cilium_pod}" -c cilium-agent -- cilium status --brief 2>/dev/null || true
  echo ""
  echo "  ── cilium endpoint list (top 30) ──"
  kubectl -n kube-system exec "${cilium_pod}" -c cilium-agent -- cilium endpoint list 2>/dev/null | head -30 || true
  echo ""
  echo "  ── cilium identity list (top 30) ──"
  kubectl -n kube-system exec "${cilium_pod}" -c cilium-agent -- cilium identity list 2>/dev/null | head -30 || true
fi

echo ""
echo "── Recent events (last 5 min) ────────────────────────────────"
kubectl get events -A --sort-by='.lastTimestamp' 2>/dev/null | tail -40 || true

echo ""
echo "── kaito-system logs (last 80 lines per pod) ─────────────────"
for pod in $(kubectl -n kaito-system get pods --no-headers -o custom-columns=':metadata.name' 2>/dev/null); do
  echo "  ── ${pod} ──"
  kubectl -n kaito-system logs "${pod}" --all-containers --tail=80 2>/dev/null || true
done

echo ""
echo "── keda logs (last 80 lines per pod) ─────────────────────────"
for pod in $(kubectl -n keda get pods --no-headers -o custom-columns=':metadata.name' 2>/dev/null); do
  echo "  ── ${pod} ──"
  kubectl -n keda logs "${pod}" --all-containers --tail=80 2>/dev/null || true
done

echo ""
echo "══════════════════════════════════════════════════════════════"
echo "  END OF DEBUG SNAPSHOT"
echo "══════════════════════════════════════════════════════════════"
