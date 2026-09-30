#!/bin/bash

set -e

# E2E tests for Python ext_proc + Envoy 1.29.2
# Three correctness cases:
# 1. Redact: normal secret redaction
# 2. Boundary: secret split across SSE events
# 3. Concurrency: multiple concurrent requests

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKEND_PORT=${BACKEND_PORT:-8000}
EXTPROC_PORT=${EXTPROC_PORT:-9000}
ENVOY_ADMIN_PORT=${ENVOY_ADMIN_PORT:-9901}

echo "[E2E] === KAITO Guard Python ext_proc PoC ==="
echo "[E2E] Envoy 1.29.2 + Mock Backend + Python ext_proc"
echo ""

# Cleanup function
cleanup() {
    echo "[E2E] Cleaning up..."
    kill $BACKEND_PID 2>/dev/null || true
    kill $EXTPROC_PID 2>/dev/null || true
    docker stop e2e-envoy 2>/dev/null || true
    wait 2>/dev/null || true
}

trap cleanup EXIT

# 1. Start mock backend
echo "[E2E] [Setup] Starting mock backend on port $BACKEND_PORT..."
cd "$SCRIPT_DIR/mock-backend"
python3 server.py $BACKEND_PORT > /tmp/mock-backend.log 2>&1 &
BACKEND_PID=$!
sleep 1
echo "[E2E] [Setup] ✓ Mock backend running (PID: $BACKEND_PID)"

# 2. Start Python ext_proc
echo "[E2E] [Setup] Starting Python ext_proc on port $EXTPROC_PORT..."
cd "$SCRIPT_DIR/python"
python3 main.py > /tmp/extproc-python.log 2>&1 &
EXTPROC_PID=$!
sleep 1
echo "[E2E] [Setup] ✓ Python ext_proc running (PID: $EXTPROC_PID)"

# 3. Start Envoy (Docker 1.29.2)
echo "[E2E] [Setup] Starting Envoy 1.29.2 (docker)..."
cd "$SCRIPT_DIR/envoy"
docker run -d \
  --name e2e-envoy \
  --network host \
  -v "$(pwd)/envoy.yaml:/etc/envoy/envoy.yaml:ro" \
  envoyproxy/envoy:v1.29-latest \
  /usr/local/bin/envoy -c /etc/envoy/envoy.yaml --log-level info \
  > /dev/null 2>&1

sleep 2
echo "[E2E] [Setup] ✓ Envoy running"

echo ""
echo "[E2E] === Test Case 1: Redact ==="
response1=$(curl -s -X POST http://localhost:10000/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "gpt-4",
        "messages": [{"role": "user", "content": "Say hello"}],
        "stream": true
    }' 2>/dev/null)

if echo "$response1" | grep -q "\[REDACTED\]"; then
    echo "[E2E] ✓ PASS: Secret was redacted"
    CASE1_PASS=1
else
    echo "[E2E] ✗ FAIL: Secret was NOT redacted"
    echo "Response: $response1"
    CASE1_PASS=0
fi

echo ""
echo "[E2E] === Test Case 2: Boundary (secret split across chunks) ==="
# This should also redact since mock backend sends sk-12 / 3456789 split
response2=$(curl -s -N -X POST http://localhost:10000/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "gpt-4",
        "messages": [{"role": "user", "content": "Test boundary"}],
        "stream": true
    }' 2>/dev/null)

# Check: should see [REDACTED], should NOT see "sk-" alone or "3456789"
if echo "$response2" | grep -q "\[REDACTED\]" && ! echo "$response2" | grep -qE "sk-[0-9]|[0-9]{7}"; then
    echo "[E2E] ✓ PASS: Boundary case handled (no unsafe prefix leaked)"
    CASE2_PASS=1
else
    echo "[E2E] ✗ FAIL: Boundary case failed (unsafe prefix may have leaked)"
    echo "Response: $response2"
    CASE2_PASS=0
fi

echo ""
echo "[E2E] === Test Case 3: Concurrency (3 parallel requests) ==="
# Run 3 concurrent requests
echo "[E2E] Starting 3 concurrent requests..."
(curl -s -X POST http://localhost:10000/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4","messages":[{"role":"user","content":"req1"}],"stream":true}' 2>/dev/null) > /tmp/req1.log &
PID1=$!

(curl -s -X POST http://localhost:10000/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4","messages":[{"role":"user","content":"req2"}],"stream":true}' 2>/dev/null) > /tmp/req2.log &
PID2=$!

(curl -s -X POST http://localhost:10000/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4","messages":[{"role":"user","content":"req3"}],"stream":true}' 2>/dev/null) > /tmp/req3.log &
PID3=$!

wait $PID1 $PID2 $PID3

# All should have [REDACTED]
req1_pass=$(grep -q "\[REDACTED\]" /tmp/req1.log && echo 1 || echo 0)
req2_pass=$(grep -q "\[REDACTED\]" /tmp/req2.log && echo 1 || echo 0)
req3_pass=$(grep -q "\[REDACTED\]" /tmp/req3.log && echo 1 || echo 0)

if [ "$req1_pass" -eq 1 ] && [ "$req2_pass" -eq 1 ] && [ "$req3_pass" -eq 1 ]; then
    echo "[E2E] ✓ PASS: All 3 concurrent requests correctly redacted"
    CASE3_PASS=1
else
    echo "[E2E] ✗ FAIL: Some concurrent requests failed"
    echo "  req1: $([ "$req1_pass" -eq 1 ] && echo PASS || echo FAIL)"
    echo "  req2: $([ "$req2_pass" -eq 1 ] && echo PASS || echo FAIL)"
    echo "  req3: $([ "$req3_pass" -eq 1 ] && echo PASS || echo FAIL)"
    CASE3_PASS=0
fi

echo ""
echo "[E2E] === Summary ==="
TOTAL_PASS=$((CASE1_PASS + CASE2_PASS + CASE3_PASS))
echo "[E2E] Redact:      $([ $CASE1_PASS -eq 1 ] && echo '✓ PASS' || echo '✗ FAIL')"
echo "[E2E] Boundary:    $([ $CASE2_PASS -eq 1 ] && echo '✓ PASS' || echo '✗ FAIL')"
echo "[E2E] Concurrency: $([ $CASE3_PASS -eq 1 ] && echo '✓ PASS' || echo '✗ FAIL')"
echo "[E2E] Total: $TOTAL_PASS / 3 passed"

if [ $TOTAL_PASS -eq 3 ]; then
    echo ""
    echo "[E2E] ✓✓✓ ALL TESTS PASSED ✓✓✓"
    echo "[E2E] Python ext_proc works with production-stack Istio/Envoy 1.29.2"
    EXIT_CODE=0
else
    echo ""
    echo "[E2E] ✗✗✗ SOME TESTS FAILED ✗✗✗"
    EXIT_CODE=1
fi

echo ""
echo "[E2E] === Logs ==="
echo "[E2E] Mock Backend:"
tail -5 /tmp/mock-backend.log

echo ""
echo "[E2E] Python ext_proc:"
tail -5 /tmp/extproc-python.log

exit $EXIT_CODE
