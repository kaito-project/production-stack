#!/bin/bash

# Generate Python bindings from Envoy ext_proc proto (Istio 1.29.2)

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="$SCRIPT_DIR/gen"

mkdir -p "$OUT_DIR"

echo "[Proto] Generating Python bindings for Envoy ext_proc (v1.29.2)..."

# Install protoc if needed
if ! command -v protoc &> /dev/null; then
    echo "[Proto] Installing protoc..."
    python3 -m pip install grpcio-tools
fi

# Download Envoy 1.29.2 source
ENVOY_VERSION="1.29.2"
TEMP_DIR=$(mktemp -d)

echo "[Proto] Downloading envoyproxy/envoy $ENVOY_VERSION..."
curl -sL "https://github.com/envoyproxy/envoy/archive/refs/tags/v${ENVOY_VERSION}.tar.gz" | \
  tar xz -C "$TEMP_DIR" --strip-components=1

if [ ! -d "$TEMP_DIR/envoy/service/ext_proc/v3" ]; then
    echo "[Proto] ERROR: Failed to extract proto files"
    exit 1
fi

# Generate Python bindings for ext_proc
echo "[Proto] Generating Python stubs..."
python3 -m grpc_tools.protoc \
  -I"$TEMP_DIR" \
  --python_out="$OUT_DIR" \
  --grpc_python_out="$OUT_DIR" \
  "$TEMP_DIR"/envoy/service/ext_proc/v3/external_processor.proto

# Generate support protos
python3 -m grpc_tools.protoc \
  -I"$TEMP_DIR" \
  --python_out="$OUT_DIR" \
  "$TEMP_DIR"/envoy/config/core/v3/base.proto \
  "$TEMP_DIR"/envoy/config/core/v3/extension.proto \
  2>/dev/null || echo "[Proto] (some deps already generated)"

# Create __init__.py for package
cat > "$OUT_DIR/__init__.py" << 'EOFPKG'
"""Generated Envoy ext_proc Python bindings (Istio 1.29.2)."""
EOFPKG

mkdir -p "$OUT_DIR/envoy/service/ext_proc/v3"
mkdir -p "$OUT_DIR/envoy/config/core/v3"

# Cleanup
rm -rf "$TEMP_DIR"

echo "[Proto] ✓ Generated Python bindings in: $OUT_DIR"
echo "[Proto] Usage: export PYTHONPATH=$OUT_DIR:\$PYTHONPATH"
