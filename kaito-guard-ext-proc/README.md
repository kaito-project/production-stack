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
