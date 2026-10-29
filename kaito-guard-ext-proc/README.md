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
the published PyPI package `kaito-guardrail-core==0.2.0`, which supplies both
streaming and configuration/build APIs. See the
[0.2.0 release](https://github.com/kaito-project/kaito-guardrail-core/releases/tag/kaito-guardrail-core-v0.2.0).
`xds-protos`, published by the gRPC project, supplies the generated Envoy
protobuf messages and service.

## Gateway policy adaptation

`ext_proc.policy.GatewayPolicy` validates an already-decoded gateway policy and
delegates scanner configuration and construction to `guardrail_core.config`.
It does not load files, define a local scanner registry, execute a policy, or
wire scanners into Envoy `Process`.

```python
from ext_proc.policy import GatewayPolicy

policy = GatewayPolicy.from_dict({
    "action": "block",
    "blockMessage": "The response was blocked.",
    "scanners": [
        {"type": "ban_substrings", "substrings": ["secret"]},
        {"type": "regex", "patterns": ["private"], "action": "redact"},
    ],
})
scanners = policy.build_scanners()
```

Only `action`, `blockMessage`, and `scanners` are accepted at the policy level.
The default action is `block`; each scanner can override it with `block` or
`redact`. `blockMessage` defaults to
`The model output was blocked by output guardrails.` and must be a non-empty
string. `scanners` must be a non-empty list. Shared core owns scanner names,
field normalization, validation, and construction; configured order is preserved.

Malformed policies raise `PolicyConfigError` instead of silently disabling
guardrails or skipping entries. Core validation errors include the scanner
entry index and retain the original cause. Scanner construction exceptions
propagate unchanged. Returned scanners implement core's buffered
`scan(prompt, output)` protocol, not the streaming window scanner protocol;
execution and streaming policy adaptation are subsequent work.

## Guardrail stream adapter

`ext_proc.guardrail_stream.GuardrailStream` connects SSE bytes, shared OpenAI
parsing, and the shared scan-before-release window for one response:

```python
from ext_proc.guardrail_stream import GuardrailStream

stream = GuardrailStream(scanner, holdback_len=holdback_len)
try:
    for wire_bytes in upstream:
        result = stream.feed(wire_bytes)
        emit(result.chunks)
        if result.blocked:
            refuse()
            break
    else:
        result = stream.finish()
        emit(result.chunks)
        if result.blocked:
            refuse()
finally:
    stream.cancel()
    upstream.close()
```

The caller supplies `scanner`, `holdback_len`, upstream ownership, emission,
and refusal/error policy. Only emit returned chunks; stop and refuse on block.
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
  and returns `WindowScanResult`. The future integration supplies scanner/policy
  adaptation, not a second local scanner or holdback implementation.
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
published framing/parser/window APIs and the guardrail stream adapter, runs the
unchanged gRPC skeleton tests, and builds the container.
The image is not published; these tests do not claim real Envoy protection.

## Container

Build from the repository root using
[`docker/Dockerfile.kaito-guard-ext-proc`](../docker/Dockerfile.kaito-guard-ext-proc):

```sh
docker build -f docker/Dockerfile.kaito-guard-ext-proc -t kaito-guard-ext-proc .
docker run --rm -p 9000:9000 kaito-guard-ext-proc
```
