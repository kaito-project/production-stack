# KAITO guardrail ext_proc skeleton

This package is the Envoy gateway adapter, not a guardrail implementation.
SSE framing, OpenAI chunk parsing, and scan-before-release holdback come directly
from the KAITO-owned `kaito-guardrail-core` package. There is no local parser,
holdback algorithm, scanner, or guard engine.

The server registers Envoy's `envoy.service.ext_proc.v3.ExternalProcessor`
service and listens on `[::]:9000`. `Process` still returns gRPC `UNIMPLEMENTED`.
The shared APIs are not wired into request handling yet; this skeleton does not
inspect or protect traffic and must not be used as an active guardrail.

## Run and test

Requires Python 3.12 or newer, matching the shared core's minimum. From the
repository root:

```sh
cd kaito-guard-ext-proc
python -m pip install -e '.[dev]'
python -m ext_proc.server
```

```sh
python -m pytest
```

Runtime dependencies are defined in `setup.py`. The shared core is pinned to
the published PyPI package `kaito-guardrail-core==0.2.1`, which supplies scanner
configuration, real scanner/window adaptation, and SSE/parser/window APIs. See
the [0.2.1 release](https://github.com/kaito-project/kaito-guardrail-core/releases/tag/kaito-guardrail-core-v0.2.1).
`xds-protos`, published by the gRPC project, supplies the generated Envoy
protobuf messages and service.

## Guardrail policy configuration adaptation

`ext_proc.policy.GuardrailPolicyConfig` validates an already-decoded guardrail
policy configuration and delegates scanner configuration and construction to
`guardrail_core.config`. It does not load files, define a local scanner registry, or
wire scanners into Envoy `Process`.

```python
from ext_proc.policy import GuardrailPolicyConfig

policy = GuardrailPolicyConfig.from_dict({
    "action": "block",
    "blockMessage": "The response was blocked.",
    "scanner_configs": [
        {"type": "ban_substrings", "substrings": ["secret"]},
        {"type": "regex", "patterns": ["private"], "action": "redact"},
    ],
})
scanners = policy.build_scanners()
```

Only `action`, `blockMessage`, and `scanner_configs` are accepted at the policy level.
The default action is `block`; each scanner can override it with `block` or
`redact`. `blockMessage` defaults to
`The model output was blocked by output guardrails.` and must be a non-empty
string. `scanner_configs` must be a non-empty list of configuration mappings;
the old `scanners` key is rejected as an unknown field. Shared core owns scanner names,
field normalization, validation, and construction. Buffered scanner construction
preserves configured order; streaming execution follows the core ordering below.

Malformed policies raise `PolicyConfigError` instead of silently disabling
guardrails or skipping entries. Core validation errors include the scanner
entry index and retain the original cause. Scanner construction exceptions
propagate unchanged. Returned scanners implement core's buffered
`scan(prompt, output)` protocol. `build_window_scanner(prompt=..., holdback_len=...)`
delegates streaming adaptation and fresh scanner construction to the shared
`ScannerWindowAdapter`; no local scanner execution or detection logic is added.

## Guardrail stream adapter

`ext_proc.guardrail_stream.GuardrailStream` connects SSE bytes, shared OpenAI
parsing, and the shared scan-before-release window for one response:

```python
from ext_proc.guardrail_stream import GuardrailStream
from ext_proc.policy import GuardrailPolicyConfig

policy = GuardrailPolicyConfig.from_dict({
    "action": "block",
    "scanner_configs": [
        {
            "type": "ban_substrings",
            "substrings": ["secret"],
        },
        {"type": "invisible_text", "action": "redact"},
    ],
})
stream = GuardrailStream.from_policy(policy, prompt=prompt)
try:
    for wire_bytes in upstream:
        result = stream.feed(wire_bytes)
        emit(result.chunks)
        if result.blocked:
            refuse(policy.block_message)
            break
    else:
        result = stream.finish()
        emit(result.chunks)
        if result.blocked:
            refuse(policy.block_message)
finally:
    stream.cancel()
    upstream.close()
```

`from_policy()` builds fresh real core scanners for each response, forwards the
request prompt, and honors per-scanner action overrides. Core runs **all redactors
before blockers**, even when a blocker is configured first; relative order is
preserved within each group. Core derives the minimum holdback with a **256
Unicode-code-point floor**, increasing it for longer configured substrings.
An explicitly smaller `holdback_len` is rejected. Short responses are retained
until valid completion rather than immediately scanned or progressively emitted.
Blocking discards the current batch and retained
tail; redaction is applied before release, including an empty sanitized output.
The existing `GuardrailStream(scanner, holdback_len=...)` API remains available
for caller-supplied `WindowScanner` implementations.

Core 0.2.1 supports `ban_substrings` in default `word` or explicit `str` mode,
`invisible_text`, `secrets`, and `sensitive`, in block/redact modes. Word matching
uses the previously released character as left context and defers an unconfirmed
right boundary until more text or normal EOF. Secrets redaction requires
`redact_mode="all"`; core rescans the sanitized text and blocks if verification
fails. There is no blanket requirement that every redactor be a fixed point.

`contains_all=True`, regexes, JSON, and cumulative reading/token limits remain
unsupported for streaming and raise an indexed `StreamingPolicyError` before a
stream is created. Buffered construction still supports all eight scanner types.
Detection is bounded by the retained window, not a whole-response guarantee;
the core owns these scanner-specific semantics. Scanner exceptions propagate
through `WindowScanError` and close the stream without flushing.

The caller supplies upstream ownership, emission, and refusal/error policy.
Only emit returned chunks; stop and refuse on block.
The adapter accepts empty choices as metadata; non-empty choices must contain
exactly one choice with index zero. It requires `[DONE]` **and** successful EOF
framing validation before flushing. Data after DONE is rejected; comments are
ignored. Finish, block, errors, and cancellation permanently close the adapter;
subsequent input or finish raises `RuntimeError`. `cancel()` is idempotent and
discards pending text without scanning or flushing.

Shared framing and scanner exceptions propagate. Invalid parsed events or
choice policy violations raise `GuardrailStreamError`, without including model
text. Call `cancel()` on upstream failure/cancellation, never `finish()`.
This is tested composition code, not an Envoy `Process` implementation; the
server remains unchanged and does not protect traffic.

## Shared streaming contracts

Import the APIs from their owning package; no ext_proc compatibility layer is
needed:

```python
from guardrail_core.streaming.openai import (
    OpenAIChatChunkParseStatus,
    parse_openai_chat_sse_event,
)
from guardrail_core.streaming.sse import SSEFramer
from guardrail_core.streaming.window import (
    StreamingBufferWindow,
    WindowScanner,
    WindowScanError,
    WindowScanResult,
)
```

The old `ext_proc.streaming.sse_parser` module and its `split_sse_events`,
`parse_sse_event`, `is_done_event`, and `SSEParseError` interfaces are removed.
They had no production callers; the server remains unchanged. This intentionally
adopts the shared contracts rather than reproducing the old compatibility rules.

### Framing and parsing

Create one `SSEFramer(max_event_bytes=...)` per response, feed upstream bytes
in order, and call `finish()` once at normal EOF.

- Keep the framer's event-size bound: the default is 1 MiB, including wire
  delimiters. Do not set the bound from the size of the current input buffer.
- `feed()` and `finish()` return `SSEEvent` objects. `event.raw_bytes` preserves
  the original bytes, including delimiters, and `event.data` contains joined SSE
  data fields. LF, CRLF, CR, split UTF-8 characters, and an initial BOM are handled
  by core, not decoded or normalized locally.
- Incomplete EOF, invalid UTF-8, and oversized events raise shared framing
  exceptions. There is no incomplete-remainder success fallback.
- `parse_openai_chat_sse_event(event)` returns a shared result with `PARSED`,
  `DONE`, `NO_DATA`, `MALFORMED_JSON`, or `INVALID_PAYLOAD` status. Malformed or
  invalid data enters the adapter's explicit failure path, not be
  treated as harmless empty content.
- Choice indexes must be explicit integers; omitted indexes are not defaulted
  locally. A bare `[DONE]` without a data field is not an SSE DONE event.
- The shared parser retains all choices, including non-text entries. It does
  not enforce a product's single-choice policy. The guardrail stream adapter
  validates this policy; future `Process` integration must also restrict
  requests to `n=1`. Do not concatenate different choices.

### Holdback and stream lifecycle

Create one `StreamingBufferWindow(scanner, holdback_len=...)` per response
(or per choice). Pass decoded content to `feed()` and release only the returned
`WindowEmitResult.chunks`. Core owns buffering, scanner invocation, redaction,
blocking, and safe-prefix release.

- `holdback_len` is measured in Unicode code points, not bytes or graphemes.
  The caller must choose enough holdback for the scanner's cross-chunk matching
  requirements; an insufficient value cannot prevent partial-match release.
- The scanner implements `WindowScanner.scan(text, *, flush, preceding_char)`
  and returns `WindowScanResult`. Configured real scanners use core's
  `ScannerWindowAdapter`, not a second local scanner or holdback implementation.
- Stop emission and discard the response window when a result is blocked.
  Scanner exceptions permanently block the shared window and raise
  `WindowScanError` with the original exception chained.
- Call `flush()` only after normal completion and successful framer `finish()`.
  EOF truncation, parse failures, cancellation, and upstream errors must discard
  the window **without flushing retained text**. A DONE marker does not excuse a
  truncated trailing event.
- Cancellation propagates; the caller owns upstream closure, refusal responses,
  and terminal response state. Unlike `SSEFramer.finish()`, window `flush()` does
  not close the window. Do not reuse a completed or abandoned response's window.

The guardrail stream adapter manages these lifecycle rules; future `Process`
integration must supply transport and policy handling, not duplicate core logic.

## CI

The `kaito-guard-ext-proc` job installs `.[dev]` with Python 3.12, exercises the
pinned shared framing/parser/window APIs and the policy-created real scanners,
runs the unchanged gRPC skeleton tests, and builds the container.
The image is not published; these tests do not claim real Envoy protection.

## Container

Build from the repository root using
[`docker/Dockerfile.kaito-guard-ext-proc`](../docker/Dockerfile.kaito-guard-ext-proc):

```sh
docker build -f docker/Dockerfile.kaito-guard-ext-proc -t kaito-guard-ext-proc .
docker run --rm -p 9000:9000 kaito-guard-ext-proc
```
