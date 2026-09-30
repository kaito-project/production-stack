#!/bin/bash

# Generate Python bindings from Envoy ext_proc proto (Istio 1.29.2)
# Uses official envoyproxy go-control-plane as source

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="$SCRIPT_DIR/gen"

mkdir -p "$OUT_DIR"

echo "[Proto] Generating Python bindings for Envoy ext_proc..."

# Install protoc if needed
if ! command -v protoc &> /dev/null; then
    echo "[Proto] Installing protoc..."
    python3 -m pip install grpcio-tools
fi

# Clone envoyproxy/envoy repo (specific tag for Istio 1.29.2)
# Istio 1.29.2 uses Envoy 1.29.2
ENVOY_VERSION="v1.29.2"
TEMP_DIR=$(mktemp -d)

echo "[Proto] Cloning envoyproxy/envoy $ENVOY_VERSION..."
git clone --depth 1 --branch $ENVOY_VERSION https://github.com/envoyproxy/envoy "$TEMP_DIR" 2>/dev/null || \
  git -C "$TEMP_DIR" checkout $ENVOY_VERSION 2>/dev/null || true

# Generate Python bindings
echo "[Proto] Generating Python code..."
python3 -m grpc_tools.protoc \
  -I"$TEMP_DIR" \
  --python_out="$OUT_DIR" \
  --grpc_python_out="$OUT_DIR" \
  "$TEMP_DIR"/envoy/service/ext_proc/v3/external_processor.proto

# Generate stubs for dependencies
python3 -m grpc_tools.protoc \
  -I"$TEMP_DIR" \
  --python_out="$OUT_DIR" \
  "$TEMP_DIR"/envoy/config/core/v3/base.proto \
  "$TEMP_DIR"/envoy/config/core/v3/extension.proto \
  "$TEMP_DIR"/envoy/extensions/filters/http/ext_proc/v3/ext_proc.proto \
  2>/dev/null || true

# Cleanup
rm -rf "$TEMP_DIR"

echo "[Proto] Generated Python bindings in: $OUT_DIR"
echo "[Proto] Add to PYTHONPATH before running: export PYTHONPATH=$OUT_DIR:$PYTHONPATH"
