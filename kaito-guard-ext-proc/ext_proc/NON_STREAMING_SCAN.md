# Non-Streaming Response Scanning

## Overview

`scan_complete_response()` is a thin adapter that delegates to `guardrail_core.scanning.scan_output()` for sequential scanner execution on complete response text. It **does not implement policy-level logic** such as block/redact ordering or secrets revalidation — those remain the responsibility of the calling layer.

## What It Does

- ✅ Executes scanners sequentially on complete text
- ✅ Collects validity and score results
- ✅ Handles input validation (non-empty scanner list, string output)
- ✅ Wraps exceptions safely (fixed message, preserves cause)
- ✅ Returns shared contract: (sanitized_text, validity_dict, scores_dict)

## What It Does NOT Do

- ❌ Apply block/redact **ordering** (policy layer responsibility)
- ❌ Revalidate secrets across scanners
- ❌ Transform output based on severity/confidence
- ❌ Cache or persist state across calls

## Usage

```python
from guardrail_core.scanning import Scanner
from ext_proc.non_streaming_scan import scan_complete_response

# Obtain scanner instances
scanners: list[Scanner] = [...]

# Scan complete response
try:
    sanitized_text, validity, scores = scan_complete_response(
        scanners,
        prompt="User question",
        output="Complete response text",
        fail_fast=False
    )
    
    print(f"Text: {sanitized_text}")              # Last scanner's output (or None)
    print(f"Validity: {validity}")                # {name: bool}
    print(f"Scores: {scores}")                    # {name: float}
    
except NonStreamingScanError as e:
    # Exception message is generic; use __cause__ for debugging
    logging.error("Scan error", exc_info=e.__cause__)
```

## Return Values

**sanitized_text**: 
- Output from the **last** scanner to execute (not first)
- Each scanner's output replaces prior; final output is final scanner's result
- None if all scanners returned None
- Type contract: str | None

**validity_dict**: 
- {scanner_name: bool} for each scanner's pass/fail
- Populated for all executed scanners

**scores_dict**: 
- {scanner_name: float} confidence scores if available
- Empty dict if no scores provided

## Error Handling

`NonStreamingScanError` is raised for:
- Empty scanner list: `"At least one scanner is required."`
- Non-string output: `"Output must be a string."`
- Scanner execution failure: `"Scanning failed."`

**Important:** Public error message is always fixed and generic. Original exception is preserved as `__cause__` for debugging.

```python
try:
    result = scan_complete_response(scanners, prompt, output)
except NonStreamingScanError as e:
    # ✅ Safe: generic message
    user_message = str(e)  # "Scanning failed."
    
    # ✅ Safe for logs: exception type, not message
    logging.error("Scan failed", exc_info=type(e.__cause__).__name__)
    
    # ❌ Unsafe: exception message may contain internal details
    # logging.error("Scan failed", exc_info=e.__cause__)
```

## Architecture

### Flow

```
Client
  ↓
scan_complete_response(scanners, prompt, output)
  ↓
[Input validation: scanners, output type]
  ↓
guardrail_core.scanning.scan_output(scanners, prompt, output, fail_fast)
  ↓
[Sequential scanner execution, no policy ordering]
  ↓
return (sanitized_text, validity_dict, scores_dict)
```

### Key Characteristics

- **Stateless**: Each call is independent; no holdback window or buffer
- **Sequential**: Scanners run in input order; one failure doesn't skip others (unless fail_fast=True)
- **Pass-through**: Output from scanner N replaces output from scanner N-1
- **Non-transforming**: No policy-level modifications (block/redact order is caller's responsibility)

## Differences from Streaming

| Aspect | Streaming (`GuardrailStream`) | Non-Streaming |
|--------|------|---|
| Input | Chunks via `feed()` | Complete text in one call |
| API | `StreamingBufferWindow` | `guardrail_core.scanning.scan_output()` |
| State | Holdback buffer for cross-chunk patterns | No state; each call independent |
| Use case | SSE, real-time responses | Batch, complete responses |

## Testing

Run tests:

```bash
cd kaito-guard-ext-proc
pytest tests/test_non_streaming_scan.py -v
```

### Test Characteristics

- **Real Scanners**: Uses guardrail_core scanner types, not Mock
- **Strict Assertions**: Verify exact output, not permissive ranges
- **Call Tracking**: Confirm scanner invocation order and fail_fast behavior
- **Type Contracts**: Respect ScanResult.output as str (not None)

## Implementation Notes

**Lines 40-81 (core logic):**
- Lines 43-55: Docstring and contract specification
- Lines 56-59: Input validation
- Lines 61-65: Delegation to scan_output(), preserving full triple
- Line 66: Exception handling with safe messaging

**What's NOT here:**
- No policy-level block/redact ordering logic
- No secrets revalidation across scanners
- No confidence-based severity weighting
- No caching or cross-call state

Policy implementation (when/whether to block, how to order redactions) belongs in the calling layer, which has context about upstream systems and business logic.


