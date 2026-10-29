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
from ext_proc.non_streaming_scan import scan_complete_response, NonStreamingScanError

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
    
    print(f"Text: {sanitized_text}")              # Last scanner's output
    print(f"Validity: {validity}")                # {name: bool}
    print(f"Scores: {scores}")                    # {name: float}
    
except NonStreamingScanError as e:
    # Safe: generic message, no internal details
    logging.error("Scan failed error_type=%s", type(e.__cause__).__name__)
```

## API

**`scan_complete_response(scanners, prompt, output, fail_fast=False)`**

Returns: `(sanitized_text: str | None, validity_dict: dict[str, bool], scores_dict: dict[str, float])`

- **sanitized_text**: Output from the last executed scanner (each replaces prior)
- **validity_dict**: {scanner_name: bool} pass/fail per scanner
- **scores_dict**: {scanner_name: float} confidence scores

Raises: `NonStreamingScanError` for empty scanner list, non-string output, or execution failure.

## Error Handling

Exception message is fixed and generic. Original exception is preserved as `__cause__`.

```python
import logging
from ext_proc.non_streaming_scan import NonStreamingScanError

try:
    result = scan_complete_response(scanners, prompt, output)
except NonStreamingScanError as e:
    # ✅ Safe: only type name, no message content
    logging.error("Scan failed error_type=%s", type(e.__cause__).__name__)
    
    # ❌ Unsafe: message may contain internal details
    # logging.error("Scan failed", exc_info=e.__cause__)
```

## Implementation Notes

Adapter delegates to `guardrail_core.scanning.scan_output()` with no additional logic. Each call is stateless and independent. Policy-level processing (what to block, how to order redactions) is the caller's responsibility.



