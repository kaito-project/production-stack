# Copyright (c) KAITO authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import asyncio
import os
import shutil
from contextlib import asynccontextmanager, suppress
from email.parser import BytesHeaderParser
from http.client import HTTPConnection, IncompleteRead
from pathlib import Path
from string import Template
from unittest.mock import Mock

import pytest
from guardrail_core.streaming.scanners import ScannerWindowAdapter

from ext_proc.guardrail_stream import GuardrailStream
from tests.sse_helpers import (
    DONE,
    extract_stream_text,
    make_content_event,
    make_policy,
)
from tests.test_sse_process import running_processor


@pytest.fixture
def envoy_binary():
    binary = os.environ.get("ENVOY_BINARY") or shutil.which("envoy")
    if not binary:
        pytest.skip("Set ENVOY_BINARY to run real Envoy streaming tests")
    return binary


@asynccontextmanager
async def running_envoy_proxy(
    binary,
    tmp_path,
    unused_tcp_port_factory,
    config,
    chunks,
    *,
    content_type="text/event-stream",
    body_gate=None,
    next_chunk_gate=None,
    eof_gate=None,
    truncate_upstream_response=False,
):
    upstream_chunks_sent = asyncio.Event()
    handlers = set()

    async def upstream(reader, writer):
        task = asyncio.current_task()
        handlers.add(task)
        try:
            request = await reader.readuntil(b"\r\n\r\n")
            request_headers = BytesHeaderParser().parsebytes(
                request.split(b"\r\n", 1)[1]
            )
            await reader.readexactly(int(request_headers.get("content-length", "0")))
            writer.write(
                b"HTTP/1.1 200 OK\r\n"
                + f"Content-Type: {content_type}\r\n".encode()
                + b"Transfer-Encoding: chunked\r\nConnection: close\r\n\r\n"
            )
            await writer.drain()
            if body_gate is not None:
                await body_gate.wait()
            for index, chunk in enumerate(chunks):
                if index == 1 and next_chunk_gate is not None:
                    await next_chunk_gate.wait()
                writer.write(f"{len(chunk):x}\r\n".encode() + chunk + b"\r\n")
                await writer.drain()
                await asyncio.sleep(0.02)
            upstream_chunks_sent.set()
            if eof_gate is not None:
                await eof_gate.wait()
            if not truncate_upstream_response:
                writer.write(b"0\r\n\r\n")
                await writer.drain()
        except (ConnectionError, asyncio.IncompleteReadError):
            pass
        finally:
            writer.close()
            with suppress(ConnectionError):
                await writer.wait_closed()
            handlers.discard(task)

    upstream_server = await asyncio.start_server(upstream, "127.0.0.1", 0)
    upstream_port = upstream_server.sockets[0].getsockname()[1]
    listener_port = unused_tcp_port_factory()
    processor_port = unused_tcp_port_factory()
    template = Template(Path(__file__).with_name("envoy_sse.yaml").read_text())
    path = tmp_path / "envoy.yaml"
    path.write_text(
        template.substitute(
            LISTENER_PORT=listener_port,
            UPSTREAM_PORT=upstream_port,
            PROCESSOR_PORT=processor_port,
        )
    )
    try:
        async with running_processor(config, processor_port):
            with (tmp_path / "envoy.log").open("wb") as log:
                process = await asyncio.create_subprocess_exec(
                    binary,
                    "-c",
                    str(path),
                    "--concurrency",
                    "1",
                    "--disable-hot-restart",
                    "--log-level",
                    "warn",
                    stdout=log,
                    stderr=log,
                )
                try:
                    async with asyncio.timeout(10):
                        while True:
                            if process.returncode is not None:
                                raise RuntimeError(
                                    f"Envoy exited: {(tmp_path / 'envoy.log').read_text()}"
                                )
                            try:
                                _, writer = await asyncio.open_connection(
                                    "127.0.0.1", listener_port
                                )
                            except OSError:
                                await asyncio.sleep(0.02)
                            else:
                                writer.close()
                                await writer.wait_closed()
                                break
                    yield listener_port, upstream_chunks_sent
                finally:
                    if process.returncode is None:
                        process.terminate()
                    try:
                        await asyncio.wait_for(process.wait(), timeout=5)
                    except TimeoutError:
                        process.kill()
                        await process.wait()
                        raise
    finally:
        upstream_server.close()
        pending = list(handlers)
        for task in pending:
            task.cancel()
        await asyncio.gather(*pending, return_exceptions=True)
        await upstream_server.wait_closed()


def get_response(port, on_headers=None, on_body=None):
    connection = HTTPConnection("127.0.0.1", port, timeout=5)
    received = bytearray()
    complete = True
    try:
        connection.request("GET", "/", headers={"Connection": "close"})
        response = connection.getresponse()
        if on_headers is not None:
            on_headers(response.status)
        try:
            while chunk := response.read1(65536):
                received.extend(chunk)
                if on_body is not None:
                    on_body()
        except IncompleteRead as error:
            received.extend(error.partial)
            complete = False
        except ConnectionResetError:
            complete = False
        return response.status, response.headers, bytes(received), complete
    finally:
        connection.close()


@pytest.mark.asyncio
async def test_real_envoy_allows_clean_sse(
    envoy_binary, tmp_path, unused_tcp_port_factory
):
    async with running_envoy_proxy(
        envoy_binary, tmp_path, unused_tcp_port_factory, make_policy(),
        [make_content_event("hello "), make_content_event("world"), DONE],
    ) as (port, _):
        status, _, received, complete = await asyncio.to_thread(get_response, port)
    assert status == 200 and complete
    assert extract_stream_text(received) == "hello world"
    assert received.endswith(DONE) and received.count(DONE) == 1


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "chunks",
    [
        [make_content_event("se"), make_content_event("cret"), DONE],
        [make_content_event("pending"), b"data: {broken}\n\n", DONE],
        [make_content_event("pending")],
    ],
    ids=["cross-event-block", "malformed-event", "missing-done"],
)
async def test_real_envoy_rejects_sse_without_releasing_pending_body(
    envoy_binary, tmp_path, unused_tcp_port_factory, chunks
):
    body_gate = asyncio.Event()
    loop = asyncio.get_running_loop()
    async with running_envoy_proxy(
        envoy_binary, tmp_path, unused_tcp_port_factory, make_policy(),
        chunks, body_gate=body_gate,
    ) as (port, _):
        status, _, received, complete = await asyncio.to_thread(
            get_response, port, lambda _: loop.call_soon_threadsafe(body_gate.set)
        )
    # Headers were already committed: the client sees an abort, not a new status.
    assert status == 200 and not complete
    assert received == b""


@pytest.mark.asyncio
async def test_real_envoy_redacts_and_waits_for_http_eof(
    envoy_binary,
    tmp_path,
    unused_tcp_port_factory,
    monkeypatch,
):
    eof_gate = asyncio.Event()
    finish = Mock(wraps=GuardrailStream.finish)
    original_finish = GuardrailStream.finish
    monkeypatch.setattr(GuardrailStream, "finish", lambda self: finish(self))
    finish.side_effect = original_finish
    async with running_envoy_proxy(
        envoy_binary,
        tmp_path,
        unused_tcp_port_factory,
        make_policy("redact"),
        [make_content_event("hello se"), make_content_event("cret \u56de\u590d"), DONE],
        eof_gate=eof_gate,
    ) as (port, upstream_chunks_sent):
        task = asyncio.create_task(asyncio.to_thread(get_response, port))
        try:
            await asyncio.wait_for(upstream_chunks_sent.wait(), timeout=5)
            assert not task.done()
            finish.assert_not_called()
        finally:
            eof_gate.set()
        status, headers, received, complete = await task
    assert status == 200 and complete
    assert extract_stream_text(received) == "hello [REDACTED] \u56de\u590d"
    assert received.count(DONE) == 1
    assert received.endswith(DONE)
    assert "content-length" not in headers
    finish.assert_called_once()


@pytest.mark.asyncio
async def test_real_envoy_error_before_headers_is_fixed_502(
    envoy_binary,
    tmp_path,
    unused_tcp_port_factory,
):
    async with running_envoy_proxy(
        envoy_binary,
        tmp_path,
        unused_tcp_port_factory,
        make_policy(),
        [make_content_event("original-must-not-leak")],
        content_type="application/json",
    ) as (port, _):
        status, _, received, complete = await asyncio.to_thread(get_response, port)
    assert status == 502 and complete
    assert received == b"Unsupported guardrail response."


@pytest.mark.asyncio
@pytest.mark.parametrize("scanner_error", [False, True])
async def test_real_envoy_error_after_headers_before_any_body_aborts(
    envoy_binary,
    tmp_path,
    unused_tcp_port_factory,
    monkeypatch,
    scanner_error,
):
    body_gate = asyncio.Event()
    loop = asyncio.get_running_loop()
    statuses = []
    if scanner_error:
        monkeypatch.setattr(
            ScannerWindowAdapter, "scan", Mock(side_effect=RuntimeError("private"))
        )

    def on_headers(status):
        statuses.append(status)
        loop.call_soon_threadsafe(body_gate.set)

    async with running_envoy_proxy(
        envoy_binary,
        tmp_path,
        unused_tcp_port_factory,
        make_policy(),
        [make_content_event("a secret"), DONE],
        body_gate=body_gate,
    ) as (port, _):
        status, _, received, complete = await asyncio.to_thread(
            get_response, port, on_headers
        )
    assert statuses == [200]
    assert status == 200 and not complete
    assert received == b""
    assert DONE not in received


@pytest.mark.asyncio
async def test_real_envoy_upstream_truncation_after_done_does_not_flush(
    envoy_binary,
    tmp_path,
    unused_tcp_port_factory,
):
    async with running_envoy_proxy(
        envoy_binary,
        tmp_path,
        unused_tcp_port_factory,
        make_policy(),
        [make_content_event("pending-must-not-leak"), DONE],
        truncate_upstream_response=True,
    ) as (port, _):
        status, _, received, complete = await asyncio.to_thread(get_response, port)
    assert status == 200 and not complete
    assert received == b""


@pytest.mark.asyncio
async def test_real_envoy_late_block_cannot_retract_approved_prefix(
    envoy_binary,
    tmp_path,
    unused_tcp_port_factory,
):
    next_chunk_gate = asyncio.Event()
    loop = asyncio.get_running_loop()
    safe = "safe " * 100
    async with running_envoy_proxy(
        envoy_binary,
        tmp_path,
        unused_tcp_port_factory,
        make_policy(),
        [make_content_event(safe), make_content_event("a secret"), DONE],
        next_chunk_gate=next_chunk_gate,
    ) as (port, _):
        status, _, received, complete = await asyncio.to_thread(
            get_response,
            port,
            None,
            lambda: loop.call_soon_threadsafe(next_chunk_gate.set),
        )
    assert status == 200 and not complete
    released = extract_stream_text(received)
    assert released and safe.startswith(released)
    assert b"secret" not in received
    assert DONE not in received


@pytest.mark.asyncio
async def test_real_client_disconnect_cancels_without_finish(
    envoy_binary,
    tmp_path,
    unused_tcp_port_factory,
    monkeypatch,
):
    cancelled = asyncio.Event()
    factory = GuardrailStream.from_policy
    created = []

    def create(*args, **kwargs):
        stream = factory(*args, **kwargs)
        original_cancel = stream.cancel

        def cancel():
            original_cancel()
            cancelled.set()

        stream.cancel = cancel
        created.append(stream)
        return stream

    monkeypatch.setattr(GuardrailStream, "from_policy", create)
    finish = Mock(side_effect=AssertionError("Must not finish after client disconnect"))
    monkeypatch.setattr(GuardrailStream, "finish", finish)
    async with running_envoy_proxy(
        envoy_binary,
        tmp_path,
        unused_tcp_port_factory,
        make_policy(),
        [make_content_event("pending"), DONE],
        eof_gate=asyncio.Event(),
    ) as (port, upstream_chunks_sent):
        reader, writer = await asyncio.open_connection("127.0.0.1", port)
        writer.write(b"GET / HTTP/1.1\r\nHost: test\r\n\r\n")
        await writer.drain()
        assert b"200" in await asyncio.wait_for(
            reader.readuntil(b"\r\n\r\n"), timeout=5
        )
        await asyncio.wait_for(upstream_chunks_sent.wait(), timeout=5)
        writer.close()
        await writer.wait_closed()
        await asyncio.wait_for(cancelled.wait(), timeout=5)
    assert created[0].closed
    finish.assert_not_called()
