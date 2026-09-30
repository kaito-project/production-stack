# KAITO Guard Phase 0 PoC: Envoy ext_proc SSE Streaming

Isolated experiment environment for evaluating Python vs Go ext_proc + RAGEngine core reuse.

## Structure

```
.
├── go/                    # Go ext_proc implementation (baseline)
│   └── main.go
├── python/                # Python ext_proc implementation (WIP)
│   └── main.py (to create)
├── envoy/                 # Envoy config (identical for both backends)
│   └── envoy.yaml (to create)
├── mock-backend/          # Mock vLLM streaming SSE responses
│   └── server.py
└── tests/
    └── e2e.sh             # End-to-end test script
```

## Quick Start

### Prerequisites

```bash
# Go
go version  # should be 1.21+

# Python
python3 --version  # should be 3.9+

# Envoy (for local testing)
envoy --version  # or use Docker
```

### Run Go Baseline

```bash
cd examples/kaito-guard-ext-proc

# Build Go binary
cd go && go build -o main main.go && cd ..

# Run test (starts mock backend, Go ext_proc, Envoy)
EXT_PROC_TYPE=go bash tests/e2e.sh
```

### Run Python Variant (WIP)

```bash
# Create python/main.py first (see below)
EXT_PROC_TYPE=python bash tests/e2e.sh
```

## Test Flow

1. **Mock Backend** (port 8000)
   - Streams OpenAI-compatible SSE with test cases
   - Case 1: Normal response
   - Case 2: Secret split across chunks ("sk-123456789")
   - Case 3: Secret at boundary

2. **Ext_proc** (port 9000)
   - Receives SSE chunks from Envoy
   - Parses logical content (across chunk boundaries)
   - Detects secret pattern using holdback
   - Redacts and rebuilds SSE
   - Streams back to Envoy

3. **Envoy** (port 10000 listener → 9000 ext_proc → 8000 backend)
   - Routes `/v1/chat/completions` to mock backend
   - Intercepts response with ext_proc
   - Returns sanitized response

4. **Test** 
   - Calls `http://localhost:10000/v1/chat/completions`
   - Checks if secret was redacted `[REDACTED]`

## Metrics to Track

- **Latency**: p50/p99 SSE event processing time
- **Memory**: Peak memory during streaming
- **Throughput**: Events/sec
- **Correctness**: Secret detection accuracy, holdback logic

## Next Steps

1. Create `python/main.py` (Python ext_proc)
2. Create `envoy/envoy.yaml` (shared config)
3. Run `EXT_PROC_TYPE=go bash tests/e2e.sh` (baseline)
4. Run `EXT_PROC_TYPE=python bash tests/e2e.sh` (compare)
5. Benchmark both (latency, memory, throughput)
