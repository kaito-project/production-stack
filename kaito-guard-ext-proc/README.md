# KAITO guardrail ext_proc skeleton

This package is the Envoy gateway/streaming adapter, not a guardrail
implementation. Guardrail logic belongs to the shared KAITO guardrail core,
as described in [kaito-project/production-stack#226](https://github.com/kaito-project/production-stack/issues/226).
Core integration is deferred until its dependency and API are pinned; no local
scanners or guard engine are provided here.

The server registers Envoy's `envoy.service.ext_proc.v3.ExternalProcessor`
service and listens on `[::]:9000`. `Process` returns gRPC `UNIMPLEMENTED`
until the shared core is integrated. This skeleton does not inspect or protect
traffic and must not be used as an active guardrail.

## Run and test

Requires Python 3.11 or newer. From the repository root:

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

## SSE parser

`ext_proc.streaming.sse_parser` provides standalone framing and OpenAI chunk
parsing helpers. They are included in the existing package and container;
they are not yet wired into the server's `Process` method.

- `split_sse_events(buffer)` returns complete event bytes and an unchanged
  incomplete remainder. Append the next chunk to that remainder. LF, CRLF,
  and CR line endings are recognized; a trailing CR is retained until the next
  byte disambiguates it from a split CRLF.
- `parse_sse_event(event)` extracts SSE `data` fields line by line, joins
  multiple data fields with newlines, and parses the first OpenAI choice.
  Comments and other SSE fields do not become content.
- `is_done_event(event)` uses the same data extraction to recognize `[DONE]`,
  including `data:[DONE]` and the legacy unprefixed marker.

The parser preserves the `delta.content` / `finish_reason` result shape and
returns `{"delta": {"content": ""}, "done": True}` for `[DONE]`. Valid events
without data or with an empty choices list return `None`; missing or null
content in a valid delta becomes an empty string. Invalid UTF-8, malformed
JSON, and invalid payload/choices/delta/content types raise `SSEParseError`,
rather than being treated as harmless empty events. The DONE helper also
raises this error for invalid UTF-8.

The future adapter must handle `SSEParseError` through its explicit failure
policy. These helpers do not enforce policies, scan content, implement
holdback, or integrate the shared guardrail core.

## CI

The `kaito-guard-ext-proc` job in the repository's CI workflow installs
`.[dev]` with Python 3.11, runs pytest, and builds the container image.
The image is not published, and this job does not test real Envoy integration.

## Container

Build from the repository root using
[`docker/Dockerfile.kaito-guard-ext-proc`](../docker/Dockerfile.kaito-guard-ext-proc):

```sh
docker build -f docker/Dockerfile.kaito-guard-ext-proc -t kaito-guard-ext-proc .
docker run --rm -p 9000:9000 kaito-guard-ext-proc
```
