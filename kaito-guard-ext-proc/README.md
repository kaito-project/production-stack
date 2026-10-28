# KAITO guardrail ext_proc skeleton

This package is the Envoy gateway adapter, not a guardrail implementation.
SSE framing, OpenAI chunk parsing, and scan-before-release holdback come directly
from the published `kaito-guardrail-core==0.1.1` package. There is no local parser,
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

Runtime dependencies are defined in `setup.py`. `kaito-guardrail-core==0.1.1`
is installed from PyPI. `xds-protos`, published by the gRPC project, supplies
the generated Envoy protobuf messages and service.

## Direct shared streaming APIs

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
  invalid data must enter the future adapter's explicit failure policy, not be
  treated as harmless empty content.
- Choice indexes must be explicit integers; omitted indexes are not defaulted
  locally. A bare `[DONE]` without a data field is not an SSE DONE event.
- The shared parser retains all choices, including non-text entries. It does
  not enforce a product's single-choice policy. Future `Process` integration
  must restrict requests to `n=1` and validate every choice as index zero, or
  maintain independent stream/window state per choice. Do not concatenate
  different choices.

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

These are requirements for future `Process` integration, not implemented gateway
behavior. Overlapping local safety logic must not be added in parallel.

## CI

The `kaito-guard-ext-proc` job installs `.[dev]` with Python 3.12, exercises the
published framing/parser/window APIs together, runs the unchanged gRPC skeleton
tests, and builds the container. The image build verifies the shared API imports.
The image is not published; these tests do not claim real Envoy protection.

## Container

Build from the repository root using
[`docker/Dockerfile.kaito-guard-ext-proc`](../docker/Dockerfile.kaito-guard-ext-proc):

```sh
docker build -f docker/Dockerfile.kaito-guard-ext-proc -t kaito-guard-ext-proc .
docker run --rm -p 9000:9000 kaito-guard-ext-proc
```
