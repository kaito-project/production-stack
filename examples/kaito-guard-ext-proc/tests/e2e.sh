#!/bin/bash

set -e

# E2E test for guardrail ext_proc (Go vs Python)
# Tests SSE streaming with secret detection across chunks

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKEND_PORT=${BACKEND_PORT:-8000}
EXTPROC_PORT=${EXTPROC_PORT:-9000}
ENVOY_ADMIN_PORT=${ENVOY_ADMIN_PORT:-9901}

echo "[E2E] Root dir: $SCRIPT_DIR"

# 1. Start mock backend
echo "[E2E] Starting mock backend on port $BACKEND_PORT..."
cd "$SCRIPT_DIR/mock-backend"
python3 server.py $BACKEND_PORT > /tmp/mock-backend.log 2>&1 &
BACKEND_PID=$!
sleep 1

echo "[E2E] Mock backend PID: $BACKEND_PID"

# 2. Start ext_proc (Go or Python)
EXT_PROC_TYPE=${EXT_PROC_TYPE:-go}
echo "[E2E] Starting $EXT_PROC_TYPE ext_proc on port $EXTPROC_PORT..."

if [ "$EXT_PROC_TYPE" = "go" ]; then
    cd "$SCRIPT_DIR/go"
    if [ ! -f "main" ]; then
        echo "[E2E] Building Go binary..."
        go build -o main main.go
    fi
    ./main > /tmp/extproc-go.log 2>&1 &
    EXTPROC_PID=$!
elif [ "$EXT_PROC_TYPE" = "python" ]; then
    cd "$SCRIPT_DIR/python"
    python3 main.py > /tmp/extproc-python.log 2>&1 &
    EXTPROC_PID=$!
else
    echo "[E2E] Unknown EXT_PROC_TYPE: $EXT_PROC_TYPE"
    exit 1
fi

echo "[E2E] ext_proc PID: $EXTPROC_PID"
sleep 2

# 3. Start Envoy
echo "[E2E] Starting Envoy..."
cd "$SCRIPT_DIR/envoy"
envoy -c envoy.yaml --log-level info > /tmp/envoy.log 2>&1 &
ENVOY_PID=$!
sleep 2

echo "[E2E] Envoy PID: $ENVOY_PID"

# Cleanup function
cleanup() {
    echo "[E2E] Cleaning up..."
    kill $BACKEND_PID 2>/dev/null || true
    kill $EXTPROC_PID 2>/dev/null || true
    kill $ENVOY_PID 2>/dev/null || true
    wait 2>/dev/null || true
}

trap cleanup EXIT

# 4. Run test
echo "[E2E] Running test..."
sleep 1

# Test: Call Envoy listener, which routes through ext_proc
echo "[E2E] Calling http://localhost:10000/v1/chat/completions"
response=$(curl -s -X POST http://localhost:10000/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "gpt-4",
        "messages": [{"role": "user", "content": "Say hello"}],
        "stream": true
    }')

echo "[E2E] Response:"
echo "$response"

# Check if secret was redacted
if echo "$response" | grep -q "\[REDACTED\]"; then
    echo "[E2E] ✓ Secret detection PASSED - found [REDACTED]"
    EXIT_CODE=0
else
    echo "[E2E] ✗ Secret detection FAILED - [REDACTED] not found"
    EXIT_CODE=1
fi

# 5. Print logs
echo ""
echo "[E2E] === Mock Backend Log ==="
tail -20 /tmp/mock-backend.log || true

echo ""
echo "[E2E] === Ext_proc Log ==="
if [ "$EXT_PROC_TYPE" = "go" ]; then
    tail -20 /tmp/extproc-go.log || true
else
    tail -20 /tmp/extproc-python.log || true
fi

echo ""
echo "[E2E] === Envoy Log ==="
tail -20 /tmp/envoy.log || true

exit $EXIT_CODE
