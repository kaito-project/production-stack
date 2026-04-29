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
#   7. HTTPRoute catch-all, error service, debug filter
#   8. cert-manager (required by LLM Gateway Auth webhook)
#   9. KEDA (Helm)
#  10. KEDA Kaito Scaler (Helm)
#  11. Inference Gateway (default namespace) — installed near-last so all
#       upstream filters (BBR, GAIE, debug filter) are already present
#       when the gateway controller renders the gateway pod.
#  12. LLM Gateway Auth — API key ext_authz (Helm)
#
# Environment variables (must be set by caller, e.g. run-e2e-local.sh or CI):
#   ISTIO_VERSION             — Istio version
#   GATEWAY_API_VERSION       — Gateway API CRD version
#   BBR_VERSION               — BBR release version
#   KEDA_VERSION              — KEDA Helm chart version
#   KEDA_KAITO_SCALER_VERSION — KEDA Kaito Scaler Helm chart version
#   LLM_GATEWAY_AUTH_VERSION  — LLM Gateway Auth Helm chart version
#   LLM_GATEWAY_AUTH_IMAGE_TAG — LLM Gateway Auth container image tag
#   SHADOW_CONTROLLER_IMAGE   — gpu-node-mocker image (default: ghcr.io/kaito-project/gpu-node-mocker:latest)
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="${SCRIPT_DIR}/../manifests"

# Validate required version variables are set.
: "${ISTIO_VERSION:?ISTIO_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${GATEWAY_API_VERSION:?GATEWAY_API_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${BBR_VERSION:?BBR_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${KEDA_VERSION:?KEDA_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${KEDA_KAITO_SCALER_VERSION:?KEDA_KAITO_SCALER_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${LLM_GATEWAY_AUTH_VERSION:?LLM_GATEWAY_AUTH_VERSION is not set. Source versions.env or export it before calling this script.}"
: "${LLM_GATEWAY_AUTH_IMAGE_TAG:?LLM_GATEWAY_AUTH_IMAGE_TAG is not set. Source versions.env or export it before calling this script.}"
SHADOW_CONTROLLER_IMAGE="${SHADOW_CONTROLLER_IMAGE:-ghcr.io/kaito-project/gpu-node-mocker:latest}"

echo "=== Component versions ==="
echo "  ISTIO_VERSION:             ${ISTIO_VERSION}"
echo "  GATEWAY_API_VERSION:       ${GATEWAY_API_VERSION}"
echo "  BBR_VERSION:               ${BBR_VERSION}"
echo "  KEDA_VERSION:              ${KEDA_VERSION}"
echo "  KEDA_KAITO_SCALER_VERSION: ${KEDA_KAITO_SCALER_VERSION}"
echo "  LLM_GATEWAY_AUTH_VERSION:  ${LLM_GATEWAY_AUTH_VERSION}"
echo "  LLM_GATEWAY_AUTH_IMAGE_TAG:${LLM_GATEWAY_AUTH_IMAGE_TAG}"
echo "  SHADOW_CONTROLLER_IMAGE:   ${SHADOW_CONTROLLER_IMAGE}"
echo ""

# ── 0. Ensure helm is available ───────────────────────────────────────────
if ! command -v helm &>/dev/null; then
  echo "Installing helm..."
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash
fi

# ── 1. KAITO workspace operator ──────────────────────────────────────────
echo ""
echo "=== 1/12: Installing KAITO workspace operator (latest chart, image: nightly-latest) ==="
helm repo add kaito https://kaito-project.github.io/kaito/charts/kaito 2>/dev/null || true
helm repo update kaito

# Install pattern follows the official KAITO nightly install guide verbatim:
#   https://kaito-project.github.io/kaito/docs/installation/#using-nightly-builds-for-testing-purpose
# i.e. the latest published chart from the helm repo (no --version pin)
# combined with image.tag=nightly-latest so both chart templates and
# controller binary track upstream HEAD.
#
# featureGates.gatewayAPIInferenceExtension is intentionally DISABLED.
# Per-model GAIE artifacts (InferencePool + EPP Deployment/Service/RBAC +
# HTTPRoute) are now provisioned by the modeldeployment Helm chart at
# charts/modeldeployment via the E2E test suite. Enabling the feature gate
# here would cause KAITO to render a duplicate set of resources via Flux
# and conflict with the chart-managed artifacts.
helm install kaito kaito/workspace \
  --namespace kaito-system \
  --create-namespace \
  --set featureGates.enableInferenceSetController=true \
  --set featureGates.gatewayAPIInferenceExtension=false \
  --set image.repository=ghcr.io/kaito-project/kaito/workspace \
  --set image.tag=nightly-latest \
  --set image.pullPolicy=Always \
  --wait --timeout=300s

echo "⏳ Waiting for KAITO controller..."
kubectl -n kaito-system rollout status deployment -l app.kubernetes.io/name=workspace --timeout=120s || true
kubectl -n kaito-system wait --for=condition=ready pod -l app.kubernetes.io/name=workspace --timeout=120s || \
  echo "⚠️  KAITO pods not ready yet — continuing (will re-check later)."

# ── 2. GPU node mocker (gpu-node-mocker) ──────────────────────────
echo ""
echo "=== 2/12: Deploying gpu-node-mocker (GPU node mocker) ==="
helm install gpu-node-mocker ./charts/gpu-node-mocker \
  --namespace kaito-system \
  --create-namespace \
  --set image.repository="${SHADOW_CONTROLLER_IMAGE%:*}" \
  --set image.tag="${SHADOW_CONTROLLER_IMAGE##*:}"

echo "⏳ Waiting for gpu-node-mocker..."
kubectl -n kaito-system rollout status deployment/gpu-node-mocker --timeout=120s || true

# ── 3. Gateway API CRDs ─────────────────────────────────────────────────
echo ""
echo "=== 3/12: Installing Gateway API CRDs ${GATEWAY_API_VERSION} ==="
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"

# ── 4. Istio ─────────────────────────────────────────────────────────────
echo ""
echo "=== 4/12: Installing Istio ${ISTIO_VERSION} ==="
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
echo "=== 5/12: Installing GWIE CRDs ==="
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml"

# ── 6. BBR (Body-Based Router) ──────────────────────────────────────────
# Installed into istio-system (Istio's rootNamespace) so that the
# EnvoyFilter rendered by the chart applies cluster-wide to every
# Istio-managed gateway, including per-case Gateways provisioned in
# isolated namespaces by the e2e framework. Without this, the BBR
# EnvoyFilter would be namespace-scoped to `default` and per-case
# Gateways would never see the body-based-routing ext_proc filter,
# breaking model name extraction and downstream HTTPRoute matching.
# The chart also rewrites the ext_proc cluster_name FQDN to
# `body-based-router.istio-system.svc.cluster.local` automatically.
echo ""
echo "=== 6/12: Installing BBR ${BBR_VERSION} ==="
helm upgrade --install body-based-router oci://registry.k8s.io/gateway-api-inference-extension/charts/body-based-routing \
  --version "${BBR_VERSION}" \
  --namespace istio-system \
  --set provider.name=istio \
  --wait

echo "⏳ Waiting for BBR..."
kubectl -n istio-system rollout status deployment/body-based-router --timeout=120s 2>/dev/null || \
  kubectl -n istio-system wait --for=condition=ready pod -l app=body-based-router --timeout=120s 2>/dev/null || \
  echo "⚠️  BBR not ready yet — continuing."

# ── 7. HTTPRoute catch-all, error service, debug filter ─────────────────
# Note: Per-model InferenceSets, InferencePools, EPP Deployments, and
# model-specific HTTPRoutes are provisioned by the modeldeployment Helm
# chart (charts/modeldeployment) via the E2E test suite (see
# test/e2e/utils/setup.go). Only cluster-wide routing primitives (catch-all
# HTTPRoute, error service, debug filter) are installed here. The catch-all
# HTTPRoute parents the default Inference Gateway installed in step 11
# below — applying it before the gateway exists is harmless; it simply
# stays inactive until the gateway controller picks it up.
echo ""
echo "=== 7/12: Deploying routing catch-all, error service ==="
kubectl apply -f "${MANIFESTS_DIR}/model-not-found.yaml"
kubectl apply -f "${MANIFESTS_DIR}/inference-debug-filter.yaml"

echo "⏳ Waiting for model-not-found service..."
kubectl rollout status deployment/model-not-found --timeout=60s 2>/dev/null || true

# ── 8. cert-manager ───────────────────────────────────────────────
# Required by LLM Gateway Auth (apikey-operator webhook).
echo ""
echo "=== 8/12: Installing cert-manager ==="
helm repo add jetstack https://charts.jetstack.io 2>/dev/null || true
helm repo update jetstack
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --set crds.enabled=true \
  --wait --timeout=300s

echo "⏳ Waiting for cert-manager..."
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=180s || true
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s || true

# ── 9. KEDA ────────────────────────────────────────────────────────
echo ""
echo "=== 9/12: Installing KEDA ${KEDA_VERSION} ==="
helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
helm repo update kedacore
helm upgrade --install keda kedacore/keda \
  --version "${KEDA_VERSION}" \
  --namespace keda \
  --create-namespace \
  --wait --timeout=300s

echo "⏳ Waiting for KEDA operator..."
kubectl -n keda rollout status deployment/keda-operator --timeout=180s || true
kubectl -n keda rollout status deployment/keda-operator-metrics-apiserver --timeout=180s || true

# ── 10. KEDA Kaito Scaler ───────────────────────────────────────────
echo ""
echo "=== 10/12: Installing KEDA Kaito Scaler ${KEDA_KAITO_SCALER_VERSION} ==="
helm repo add keda-kaito-scaler https://kaito-project.github.io/keda-kaito-scaler/charts/kaito-project 2>/dev/null || true
helm repo update keda-kaito-scaler
helm upgrade --install keda-kaito-scaler keda-kaito-scaler/keda-kaito-scaler \
  --version "${KEDA_KAITO_SCALER_VERSION}" \
  --namespace kaito-system \
  --create-namespace \
  --wait --timeout=300s

echo "⏳ Waiting for keda-kaito-scaler..."
kubectl -n kaito-system rollout status deployment -l app.kubernetes.io/name=keda-kaito-scaler --timeout=180s || true

# ── 11. Inference Gateway (default namespace) ──────────────────────────
# Deployed near-last so every upstream filter (BBR, GAIE, debug filter) is
# already in place when the Istio gateway-controller renders the gateway
# pod. Per-case Gateways for the e2e suite are provisioned at runtime by
# the test framework (utils.EnsureNamespace) inside per-case namespaces.
echo ""
echo "=== 11/12: Deploying default inference Gateway ==="
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

# ── 12. LLM Gateway Auth (API key ext_authz) ───────────────────────
echo ""
echo "=== 12/12: Installing LLM Gateway Auth ${LLM_GATEWAY_AUTH_VERSION} ==="

# Determine the gateway namespace — the namespace where the inference-gateway pod runs.
GW_NAMESPACE="$(kubectl get pods -A -l gateway.networking.k8s.io/gateway-name=inference-gateway \
  -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || echo "default")"
echo "  Gateway namespace detected: ${GW_NAMESPACE}"

helm upgrade --install llm-gateway-apikey \
  oci://mcr.microsoft.com/aks/kaito/helm/llm-gateway-apikey \
  --version "${LLM_GATEWAY_AUTH_VERSION}" \
  --namespace llm-gateway-auth \
  --create-namespace \
  --set operator.image.repository=mcr.microsoft.com/aks/kaito/apikey-operator \
  --set operator.image.tag="${LLM_GATEWAY_AUTH_IMAGE_TAG}" \
  --set authz.image.repository=mcr.microsoft.com/aks/kaito/apikey-authz \
  --set authz.image.tag="${LLM_GATEWAY_AUTH_IMAGE_TAG}" \
  --set operator.webhook.enabled=true \
  --set istio.enabled=false \
  --set crds.install=true \
  --wait --timeout=300s

echo "⏳ Waiting for apikey-operator..."
kubectl -n llm-gateway-auth rollout status deployment/apikey-operator --timeout=180s || true

echo "⏳ Waiting for apikey-authz..."
kubectl -n llm-gateway-auth rollout status deployment/apikey-authz --timeout=180s || true

# NOTE: The MeshConfig extensionProvider registration (apikey-ext-authz) and
# the AuthorizationPolicy are now handled by the modeldeployment Helm chart
# when authAPIKeyEnabled=true. See charts/modeldeployment/templates/.

echo ""
echo "✅ All components installed."
