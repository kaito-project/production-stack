# KAITO guardrail ext_proc skeleton

This package is the Envoy gateway/streaming adapter, not a guardrail
implementation. Guardrail logic belongs to the shared KAITO guardrail core,
as described in [kaito-project/production-stack#226](https://github.com/kaito-project/production-stack/issues/226).
The response-window adapter uses KAITO's shared streaming window; no local
scanners, holdback algorithm, or guard engine are provided here.

The server registers Envoy's `envoy.service.ext_proc.v3.ExternalProcessor`
service and listens on `[::]:9000`. `Process` returns gRPC `UNIMPLEMENTED`
until the adapter is wired into the service. This skeleton does not inspect or protect
traffic and must not be used as an active guardrail.

## Run and test

Requires Python 3.12 or newer, matching the shared core's minimum. From the repository root:

```sh
cd kaito-guard-ext-proc
python -m pip install -e '.[dev]'
python -m ext_proc.server
```

```sh
python -m pytest
```

Runtime dependencies are defined only in `setup.py`. `xds-protos`, published
by the gRPC project, supplies the generated Envoy protobuf messages and service.

## Shared response window

`guardrail-core` is installed from the authoritative
[KAITO commit `dda1b10e881ed69228978964a2e89ecf241757f9`](https://github.com/kaito-project/kaito/commit/dda1b10e881ed69228978964a2e89ecf241757f9),
using its immutable source archive and `guardrail_core` subdirectory. It does not
follow `main`. The installed package exposes `StreamingBufferWindow`,
`WindowScanner`, `WindowScanResult`, `WindowEmitResult`, and `WindowScanError`
from `guardrail_core.streaming_window`.

[`StreamingResponseWindow`](ext_proc/streaming_window.py) only manages lifecycle.
Create one instance per response, supplying a scanner and a holdback length.
Holdback counts Python string Unicode characters, not UTF-8 bytes. Scanner
construction, policy ordering, and sufficient holdback remain caller responsibilities;
stateful scanners must also be scoped to the response.

```python
from guardrail_core.streaming_window import WindowScanner

from ext_proc.streaming_window import StreamingResponseWindow


def checked_chunks(scanner: WindowScanner, holdback_len: int, upstream_text):
    response = StreamingResponseWindow(scanner, holdback_len=holdback_len)
    try:
        for text in upstream_text:
            result = response.feed(text)
            if result.blocked:
                raise RuntimeError("Response blocked")
            yield from result.chunks
        result = response.finish()
        if result.blocked:
            raise RuntimeError("Response blocked")
        yield from result.chunks
    finally:
        response.abort()
```

Only an upstream that verifies complete, successful termination may reach
`finish()`. Parse errors, truncation, and cancellation must instead call `abort()`
without flushing. An iterator that silently treats truncation as EOF is not a
suitable input. The example illustrates text-window usage, not an SSE parser
or Envoy output adapter.

- `feed()` and `finish()` return the shared `WindowEmitResult` unchanged.
- `blocked` and `redacted` expose shared-window state; `closed` is lifecycle state.
- A block result is terminal and must enter the caller's explicit rejection path.
- Scanner errors propagate as `WindowScanError` with their original cause;
  exceptions and cancellation make the response terminal and must enter the
  caller's failure path. Unchecked buffered text is never flushed on failure.
- `finish()` invokes the shared final `flush()` exactly once. Further `feed()`
  or `finish()` calls after any terminal state raise `RuntimeError`.
- `abort()` is idempotent and never emits text. Discard the response instance
  after termination. Neither this adapter nor its error messages log raw text.

This change does **not** integrate `Process`, migrate an SSE/OpenAI parser,
provide policy/scanner configuration, or establish Envoy end-to-end protection.
Tests validate the lifecycle adapter against direct calls to the pinned shared
window, not live gateway traffic.

## CI

The `kaito-guard-ext-proc` job in the repository's CI workflow installs
`.[dev]` with Python 3.12, runs pytest, and builds the container image.
The image build also imports the shared window, its interfaces/results, and the
adapter to verify that the runtime installation contains them.
The image is not published, and this job does not test real Envoy integration.

## Container

Build from the repository root using
[`docker/Dockerfile.kaito-guard-ext-proc`](../docker/Dockerfile.kaito-guard-ext-proc):

```sh
docker build -f docker/Dockerfile.kaito-guard-ext-proc -t kaito-guard-ext-proc .
docker run --rm -p 9000:9000 kaito-guard-ext-proc
```
