# Phase 0 Spike: Python ext_proc vs Go + RAGEngine Core Reuse

## Goal

Evaluate whether Python ext_proc + RAGEngine guardrail core reuse is viable, or if Go implementation from scratch is better.

## Questions to Answer

### Q1: Performance
- [ ] Latency: Python gRPC ext_proc vs Go (p50/p99)
- [ ] Memory per pod
- [ ] Throughput (req/s)
- [ ] Streaming SSE overhead

**Test**: Load test both with streaming response (8KB chunks, 10s response)

### Q2: RAGEngine Core Reuse
- [ ] Can we extract guardrail scanner logic as standalone library?
- [ ] Does it depend on RAGEngine-specific context (model name, user, etc.)?
- [ ] What's the minimum adapter layer needed?
- [ ] Token counting / model-specific logic dependency?

**Investigation**:
- [ ] Read `/kaito/presets/ragengine/guardrails/output_guardrails.py`
- [ ] Check imports and dependencies
- [ ] Identify reusable vs RAGEngine-specific code
- [ ] Estimate refactor effort

### Q3: SSE Streaming in Python
- [ ] Can we implement OpenAI-compatible SSE parsing in Python as well as Go?
- [ ] asyncio + streaming request/response handling complexity?
- [ ] Backpressure/flow control patterns

**PoC**:
- [ ] Minimal Python ext_proc that handles SSE streaming
- [ ] Measure latency vs Go version

### Q4: Deployment & Operations
- [ ] Container size (Go binary vs Python runtime)
- [ ] Startup time
- [ ] Config hot-reload complexity (both languages)
- [ ] Observability (logging/metrics) parity

## Spike Tasks

### Task 1: RAGEngine Code Analysis
- [ ] Clone/read RAGEngine guardrail code
- [ ] Identify 8 scanner implementations
- [ ] Extract module structure
- [ ] Document dependency graph
- [ ] Estimate LOC to refactor for reuse

**Output**: `RAGENINE_ANALYSIS.md`

### Task 2: Minimal Python ext_proc Prototype
- [ ] Set up Python gRPC ext_proc skeleton
- [ ] Handle SSE streaming (split events → logical buffer)
- [ ] Implement 1 simple scanner (BanSubstrings)
- [ ] Load test vs Go PoC

**Output**: `python-extproc/` directory + benchmark results

### Task 3: Go Baseline Measurement
- [ ] Measure current Go PoC latency/memory/throughput
- [ ] Document load test methodology

**Output**: `go-baseline-results.txt`

### Task 4: Decision Matrix
- [ ] Compare Go vs Python on all dimensions
- [ ] Evaluate RAGEngine core reuse benefit
- [ ] Write recommendation

**Output**: `DECISION.md`

## Success Criteria

- [ ] Clear latency/memory/throughput numbers (both languages)
- [ ] Reuse effort estimated (hours, days, weeks?)
- [ ] Technical decision documented with rationale
- [ ] Trade-offs clearly stated

## Timeline

- Task 1: 1-2 days (code analysis)
- Task 2: 2-3 days (Python prototype + benchmark)
- Task 3: 1 day (Go baseline)
- Task 4: 1 day (decision)

**Total**: ~1 week

## Deliverables

1. `RAGENINE_ANALYSIS.md` - what's reusable, what's not
2. `python-extproc/` - minimal Python ext_proc code
3. `benchmark-results.csv` - latency/memory/throughput comparison
4. `DECISION.md` - final recommendation (Go / Python / Hybrid)
