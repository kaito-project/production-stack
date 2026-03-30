#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# install-components.sh — Install all E2E components onto the AKS cluster.
#
# Components installed (in order):
#   1. KAITO workspace operator (Helm)
#   2. GPU node mocker / gpu-node-mocker (Helm)
#   3. Gateway API CRDs
#   4. Istio v1.29 (minimal profile)
#   5. GWIE CRDs (InferencePool, InferenceModel)
#   6. BBR (Body-Based Router) v1.3.1
#   7. Inference Gateway
#   8. InferencePools, InferenceModels, HTTPRoute
#   9. InferenceSets (KAITO workloads on fake nodes)
#
# Environment variables:
#   KAITO_VERSION             — KAITO Helm chart version    (default: v0.9.1)
#   ISTIO_VERSION             — Istio version               (default: 1.29.0)
#   GATEWAY_API_VERSION       — Gateway API CRD version     (default: v1.2.0)
#   BBR_VERSION               — BBR release version         (default: v1.3.1)
#   SHADOW_CONTROLLER_IMAGE   — gpu-node-mocker image (default: ghcr.io/kaito-project/gpu-node-mocker:latest)
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="${SCRIPT_DIR}/../manifests"

KAITO_VERSION="${KAITO_VERSION:-v0.9.1}"
ISTIO_VERSION="${ISTIO_VERSION:-1.29.0}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.2.0}"
BBR_VERSION="${BBR_VERSION:-v1.3.1}"
SHADOW_CONTROLLER_IMAGE="${SHADOW_CONTROLLER_IMAGE:-ghcr.io/kaito-project/gpu-node-mocker:latest}"

# ── 0. Ensure helm is available ───────────────────────────────────────────
if ! command -v helm &>/dev/null; then
  echo "Installing helm..."
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash
fi

# ── 1. KAITO workspace operator ──────────────────────────────────────────
echo ""
echo "=== 1/9: Installing KAITO workspace operator ${KAITO_VERSION} ==="
helm repo add kaito https://kaito-project.github.io/kaito/charts 2>/dev/null || true
helm repo update kaito
helm install kaito kaito/workspace \
  --version "${KAITO_VERSION}" \
  --namespace kaito-system \
  --create-namespace \
  --set featureGates.enableInferenceSetController=true \
  --set featureGates.gatewayAPIInferenceExtension=true \
  --wait --timeout=300s

echo "⏳ Waiting for KAITO controller..."
kubectl -n kaito-system rollout status deployment -l app.kubernetes.io/name=workspace --timeout=120s || true
kubectl -n kaito-system wait --for=condition=ready pod -l app.kubernetes.io/name=workspace --timeout=120s || \
  echo "⚠️  KAITO pods not ready yet — continuing (will re-check later)."

# ── 2. GPU node mocker (gpu-node-mocker) ──────────────────────────
echo ""
echo "=== 2/9: Deploying gpu-node-mocker (GPU node mocker) ==="
helm install gpu-node-mocker ./charts/gpu-node-mocker \
  --namespace kaito-system \
  --create-namespace \
  --set image.repository="${SHADOW_CONTROLLER_IMAGE%:*}" \
  --set image.tag="${SHADOW_CONTROLLER_IMAGE##*:}"

echo "⏳ Waiting for gpu-node-mocker..."
kubectl -n kaito-system rollout status deployment/gpu-node-mocker --timeout=120s || true

# ── 3. Gateway API CRDs ─────────────────────────────────────────────────
echo ""
echo "=== 3/9: Installing Gateway API CRDs ${GATEWAY_API_VERSION} ==="
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"

# ── 4. Istio ─────────────────────────────────────────────────────────────
echo ""
echo "=== 4/9: Installing Istio ${ISTIO_VERSION} ==="
if ! command -v istioctl &>/dev/null; then
  echo "Installing istioctl..."
  curl -L https://istio.io/downloadIstio | ISTIO_VERSION="${ISTIO_VERSION}" sh -
  export PATH="${PWD}/istio-${ISTIO_VERSION}/bin:${PATH}"
fi

echo "Using istioctl: $(which istioctl)"
istioctl install \
  --set profile=minimal \
  --set hub=docker.io/istio \
  --set tag="${ISTIO_VERSION}" \
  --set "values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true" \
  -y

echo "⏳ Waiting for istiod..."
kubectl -n istio-system rollout status deployment/istiod --timeout=180s

# ── 5. GWIE CRDs (InferencePool, InferenceModel) ────────────────────────
echo ""
echo "=== 5/9: Installing GWIE CRDs ==="
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml"

# ── 6. BBR (Body-Based Router) ──────────────────────────────────────────
echo ""
echo "=== 6/9: Installing BBR ${BBR_VERSION} ==="
helm upgrade --install body-based-router oci://registry.k8s.io/gateway-api-inference-extension/charts/body-based-routing \
  --version "${BBR_VERSION}" \
  --set provider.name=istio \
  --wait

echo "⏳ Waiting for BBR..."
kubectl rollout status deployment/body-based-router --timeout=120s 2>/dev/null || \
  kubectl wait --for=condition=ready pod -l app=body-based-router --timeout=120s 2>/dev/null || \
  echo "⚠️  BBR not ready yet — continuing."

# ── 7. Inference Gateway ────────────────────────────────────────────────
echo ""
echo "=== 7/9: Deploying inference Gateway ==="
kubectl apply -f "${MANIFESTS_DIR}/gateway.yaml"

echo "⏳ Waiting for Gateway pod..."
for _ in $(seq 1 30); do
  if kubectl get pods -l gateway.networking.k8s.io/gateway-name=inference-gateway --no-headers 2>/dev/null | grep -q .; then
    break
  fi
  sleep 5
done

kubectl wait --for=condition=ready pod \
  -l gateway.networking.k8s.io/gateway-name=inference-gateway \
  --timeout=180s 2>/dev/null || \
  echo "⚠️  Gateway pod not ready yet — continuing."

# ── 8. HTTPRoute, error service, DestinationRules ───────────────────────
# Note: InferencePools + EPP are auto-created by KAITO when InferenceSets are applied.
echo ""
echo "=== 8/9: Deploying routing, error service ==="
kubectl apply -f "${MANIFESTS_DIR}/model-not-found.yaml"
kubectl apply -f "${MANIFESTS_DIR}/httproute.yaml"
kubectl apply -f "${MANIFESTS_DIR}/destination-rules.yaml"
kubectl apply -f "${MANIFESTS_DIR}/inference-debug-filter.yaml"

echo "⏳ Waiting for model-not-found service..."
kubectl rollout status deployment/model-not-found --timeout=60s 2>/dev/null || true

# ── 9. InferenceSets (KAITO workloads) ──────────────────────────────────
echo ""
echo "=== 9/9: Deploying InferenceSets ==="
kubectl apply -f "${MANIFESTS_DIR}/inference-sets.yaml"

echo ""
echo "✅ All components installed."
