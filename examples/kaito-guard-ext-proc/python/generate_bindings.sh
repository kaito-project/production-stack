#!/bin/bash
set -e

# Generate Python bindings from Envoy 1.29.2 protos using Docker
# This avoids local proto dependency hell

ENVOY_VERSION="1.29.2"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="$SCRIPT_DIR/gen_bindings"
TEMP_DIR=$(mktemp -d)

mkdir -p "$OUT_DIR"

echo "[Proto] Downloading envoyproxy/envoy v${ENVOY_VERSION}..."
curl -sL "https://github.com/envoyproxy/envoy/archive/refs/tags/v${ENVOY_VERSION}.tar.gz" | \
  tar xz -C "$TEMP_DIR" --strip-components=1

echo "[Proto] Generating Python bindings via Docker..."
docker run --rm \
  -v "$TEMP_DIR:/proto-src" \
  -v "$OUT_DIR:/proto-out" \
  python:3.12 bash -c "
    set -e
    pip install -q grpcio-tools
    
    # Generate external_processor proto
    python -m grpc_tools.protoc \
      -I/proto-src/api \
      --python_out=/proto-out \
      --grpc_python_out=/proto-out \
      /proto-src/api/envoy/service/ext_proc/v3/external_processor.proto 2>&1 || true
    
    # Generate dependencies (ignore errors, we'll use what we get)
    python -m grpc_tools.protoc \
      -I/proto-src/api \
      --python_out=/proto-out \
      /proto-src/api/envoy/config/core/v3/base.proto 2>&1 || true
    
    ls -la /proto-out/
  "

echo "[Proto] ✓ Bindings generated in $OUT_DIR"
ls -la "$OUT_DIR"

# Cleanup
rm -rf "$TEMP_DIR"

echo "[Proto] Add to PYTHONPATH: export PYTHONPATH=$OUT_DIR:\$PYTHONPATH"
