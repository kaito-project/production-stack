# Non-Streaming Response Scanning

## Overview

`scan_complete_response()` scans complete response text through guardrail core's shared scanner API, reusing strategy order, secrets revalidation, and result contracts from `guardrail_core.scanning`.

Unlike the streaming adapter (`GuardrailStream`), this function:
- Processes the **complete** response in one call (no state retention)
- Delegates directly to `guardrail_core.scanning.scan_output()` 
- Returns sanitized text + per-scanner validity flags (shared contract)
- Avoids holdback windows; suitable for batch/complete-text workflows

## Usage

```python
from guardrail_core.scanning import Scanner
from ext_proc.non_streaming_scan import scan_complete_response

# Implement or obtain Scanner instances
scanners: list[Scanner] = [...]

# Scan complete response
try:
    sanitized_text, validity = scan_complete_response(
        scanners,
        prompt="User question",
        output="Complete response text",
        fail_fast=False
    )
    print(f"Sanitized: {sanitized_text}")
    print(f"Validity: {validity}")  # {scanner_name: passed}
except NonStreamingScanError as e:
    print(f"Scanning failed: {e}")
```

## API

### `scan_complete_response(scanners, prompt, output, fail_fast=False)`

**Parameters:**
- `scanners`: Sequence of `Scanner` implementations (Protocol: `scan(prompt, output) -> ScanResult`)
- `prompt`: Original prompt/request context (may be empty for batch)
- `output`: Complete response text (must be string)
- `fail_fast`: Stop after first scanner failure (default: False, process all)

**Returns:**
- `sanitized_text`: Cleaned output from first scanner, or None if no scanners/all returned None
- `validity_dict`: `{scanner_name: bool}` indicating pass/fail for each scanner

**Raises:**
- `NonStreamingScanError`: No scanners provided, invalid output type, or scanning failed

### `NonStreamingScanError`

Custom exception that:
- Wraps underlying scanner errors with fixed message (no exception details leaked)
- Preserves original exception as `__cause__` for debugging
- Never includes scanner exception message in public error text

## Architecture

### Flow

```
Client
  ↓
scan_complete_response(scanners, prompt, output)
  ↓
[Input validation: scanners, output type]
  ↓
guardrail_core.scanning.scan_output(scanners, prompt, output)
  ↓
[Sequential scanner execution, strategy order respected]
  ↓
return (sanitized_text, validity_dict)
```

### Key Differences from Streaming

| Aspect | Streaming (`GuardrailStream`) | Non-Streaming |
|--------|------|---|
| Input | Chunks via `feed()` | Complete text in one call |
| API | `StreamingBufferWindow` (holdback) | `guardrail_core.scanning.scan_output()` |
| State | Maintains frame + window buffer | No persistent state |
| Holdback handling | Required for cross-chunk patterns | Not applicable; full text available |
| Suitable for | SSE, chunked responses | Batch, complete responses, post-processing |

### Why Not Use `StreamingBufferWindow`?

- Non-streaming has the **complete text** available; no need for holdback
- Using holdback introduces state complexity without benefit
- Only calling `feed()` (not `flush()`) leaves tail text unscanned
- Same window instance across requests can leak state
- Core's `scan_output()` is the correct API for complete text

## Error Handling

The adapter ensures errors never leak scanner internals:

```python
# ✅ Good: fixed message, preserves cause
try:
    result = scan_complete_response(scanners, prompt, output)
except NonStreamingScanError as e:
    logging.error("Scan failed", exc_info=e)  # exc_info shows __cause__
    # Send generic message to client

# ❌ Bad: scanner exception message may contain sensitive patterns
except Exception as e:
    error_msg = f"Scanning failed: {e}"  # DON'T: may leak secrets/regex
```

## Testing

Test with real `Scanner` implementations:

```bash
pytest tests/test_non_streaming_scan.py -v
```

Tests verify:
- Correct Scanner Protocol usage
- Sanitized text returned from valid scanner
- Validity dict populated correctly
- Both pass and fail cases
- Exception handling (no exception details in message)
- Fail-fast behavior
- Empty scanner list rejection
- Input type validation

## Lines 40-80

Core scanning logic (lines 40-62):
- Line 42-47: Input validation (scanners list, output type)
- Line 49-62: Delegate to `scan_output()`, handle exceptions, preserve cause

Result contract (lines 49-62):
- Returns guardrail core's shared tuple: `(sanitized_text, validity_dict)`
- No custom result class, no invented fields
- Errors wrapped with fixed message + `raise ... from e`

