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
import threading
from contextlib import asynccontextmanager
from unittest.mock import Mock

import grpc
import pytest
from envoy.config.core.v3 import base_pb2
from envoy.extensions.filters.http.ext_proc.v3.processing_mode_pb2 import ProcessingMode
from envoy.service.ext_proc.v3 import external_processor_pb2 as pb
from envoy.service.ext_proc.v3.external_processor_pb2_grpc import ExternalProcessorStub
from guardrail_core.streaming.openai import build_sse_data_chunk
from guardrail_core.streaming.scanners import ScannerWindowAdapter, StreamingPolicyError

from ext_proc.guardrail_stream import GuardrailStream
from ext_proc.policy import GuardrailPolicyConfig
from ext_proc.server import ExternalProcessorServicer, serve
from tests.sse_helpers import (
    DONE,
    extract_stream_text,
    make_content_event,
    make_policy,
    parse_sse_payloads,
)


def make_response_headers_request(content_type="text/event-stream"):
    return pb.ProcessingRequest(
        response_headers=pb.HttpHeaders(
            headers=base_pb2.HeaderMap(
                headers=[
                    base_pb2.HeaderValue(
                        key="content-type", raw_value=content_type.encode()
                    )
                ]
            )
        )
    )


@asynccontextmanager
async def running_processor(config, port):
    task = asyncio.create_task(serve("127.0.0.1", port, policy=config, streaming=True))
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{port}") as channel:
            await asyncio.wait_for(channel.channel_ready(), timeout=5)
            yield ExternalProcessorStub(channel)
    finally:
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task


async def open_sse_call(stub):
    call = stub.Process(timeout=5)
    await call.write(make_response_headers_request("Text/Event-Stream; charset=utf-8"))
    response = await call.read()
    assert response.HasField("response_headers")
    assert not response.HasField("mode_override")
    assert (
        "content-length"
        in response.response_headers.response.header_mutation.remove_headers
    )
    return call


async def send_body_and_read_response(call, wire, *, final=False):
    await call.write(
        pb.ProcessingRequest(
            response_body=pb.HttpBody(
                body=wire,
                end_of_stream=final,
            )
        )
    )
    response = await call.read()
    if final:
        assert await call.read() is grpc.aio.EOF
    return response


def extract_replacement_body(response):
    assert response.WhichOneof("response") == "response_body"
    mutation = response.response_body.response.body_mutation
    assert mutation.WhichOneof("mutation") == "body"
    return mutation.body


@pytest.mark.asyncio
async def test_done_and_finish_reason_wait_for_normal_http_eof(unused_tcp_port):
    role = build_sse_data_chunk(
        {
            "id": "chat-test",
            "object": "chat.completion.chunk",
            "created": 1,
            "model": "test",
            "choices": [
                {"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}
            ],
        }
    ).encode()
    finish = build_sse_data_chunk(
        {
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
        }
    ).encode()
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        call = await open_sse_call(stub)
        first = extract_replacement_body(
            await send_body_and_read_response(
                call, role + make_content_event("hello \u4f60\u597d")
            )
        )
        assert parse_sse_payloads(first)[0] == parse_sse_payloads(role)[0]
        assert extract_stream_text(first) == ""
        pending = extract_replacement_body(
            await send_body_and_read_response(call, finish + DONE)
        )
        assert b"[DONE]" not in first + pending
        assert b'"finish_reason":"stop"' not in first + pending
        completed = extract_replacement_body(
            await send_body_and_read_response(call, b"", final=True)
        )
    assert extract_stream_text(completed) == "hello \u4f60\u597d"
    assert parse_sse_payloads(completed)[-1]["choices"][0]["finish_reason"] == "stop"
    assert completed.endswith(DONE)
    assert (first + pending + completed).count(DONE) == 1


@pytest.mark.asyncio
async def test_cross_chunk_redaction_uses_real_shared_scanners(unused_tcp_port):
    wire = (
        make_content_event("safe " * 60 + "se")
        + make_content_event("cret \u56de\u590d")
        + DONE
    )
    async with running_processor(make_policy("redact"), unused_tcp_port) as stub:
        call = await open_sse_call(stub)
        received = b""
        for chunk in (wire[:7], wire[7:350], wire[350:-1], wire[-1:]):
            received += extract_replacement_body(
                await send_body_and_read_response(call, chunk)
            )
        received += extract_replacement_body(
            await send_body_and_read_response(call, b"", final=True)
        )
    assert extract_stream_text(received) == "safe " * 60 + "[REDACTED] \u56de\u590d"
    assert b"secret" not in received
    assert received.count(DONE) == 1


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "wire",
    [
        make_content_event("pending"),
        make_content_event("pending") + DONE + b"data: unfinished",
        make_content_event("pending") + DONE + make_content_event("late"),
        make_content_event("pending") + DONE + b"data:\n\n",
        make_content_event("pending") + DONE + DONE,
        make_content_event("pending") + b"data: {invalid}\n\n",
    ],
)
async def test_truncation_or_invalid_sse_never_flushes_holdback(unused_tcp_port, wire):
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        call = await open_sse_call(stub)
        response = await send_body_and_read_response(call, wire, final=True)
    assert response.immediate_response.status.code == 502
    assert b"pending" not in response.SerializeToString()
    assert b"[DONE]" not in response.SerializeToString()


@pytest.mark.asyncio
async def test_block_discards_pending_output(unused_tcp_port):
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        call = await open_sse_call(stub)
        response = await send_body_and_read_response(
            call, make_content_event("a secret") + DONE, final=True
        )
    assert response.immediate_response.status.code == 403
    assert response.immediate_response.body == b"Blocked by streaming policy."
    assert b"secret" not in response.SerializeToString()
    assert b"[DONE]" not in response.SerializeToString()


@pytest.mark.asyncio
@pytest.mark.parametrize("final", [False, True])
async def test_scanner_exception_is_generic_and_never_flushes(
    unused_tcp_port, monkeypatch, caplog, final
):
    monkeypatch.setattr(
        ScannerWindowAdapter, "scan", Mock(side_effect=RuntimeError("private-text"))
    )
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        call = await open_sse_call(stub)
        wire = (
            make_content_event("private-text" if final else "private-text " * 30)
            + DONE
        )
        response = await send_body_and_read_response(call, wire, final=final)
    assert response.immediate_response.status.code == 502
    assert b"private-text" not in response.SerializeToString()
    assert "private-text" not in caplog.text
    assert "Guardrail streaming processing failed" in caplog.text


@pytest.mark.asyncio
async def test_rpc_eof_is_not_http_eof(unused_tcp_port):
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        call = await open_sse_call(stub)
        assert (
            extract_replacement_body(
                await send_body_and_read_response(
                    call, make_content_event("pending") + DONE
                )
            )
            == b""
        )
        await call.done_writing()
        with pytest.raises(grpc.aio.AioRpcError) as exc:
            await call.read()
    assert exc.value.code() == grpc.StatusCode.FAILED_PRECONDITION


@pytest.mark.asyncio
async def test_client_cancellation_discards_without_finish(
    unused_tcp_port, monkeypatch
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
    finish = Mock(side_effect=AssertionError("Must not flush a cancelled response"))
    monkeypatch.setattr(GuardrailStream, "finish", finish)
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        call = await open_sse_call(stub)
        assert (
            extract_replacement_body(
                await send_body_and_read_response(
                    call, make_content_event("pending") + DONE
                )
            )
            == b""
        )
        call.cancel()
        await asyncio.wait_for(cancelled.wait(), timeout=5)
    assert created[0].closed
    finish.assert_not_called()


@pytest.mark.asyncio
async def test_cancel_while_feed_is_running(unused_tcp_port, monkeypatch):
    loop = asyncio.get_running_loop()
    entered = asyncio.Event()
    cleaned = asyncio.Event()
    exited = asyncio.Event()
    release = threading.Event()
    original_scan = ScannerWindowAdapter.scan
    original_feed = GuardrailStream.feed
    original_cancel = GuardrailStream.cancel

    def scan(scanner, *args, **kwargs):
        loop.call_soon_threadsafe(entered.set)
        assert release.wait(timeout=5)
        return original_scan(scanner, *args, **kwargs)

    def feed(stream, body):
        try:
            return original_feed(stream, body)
        finally:
            loop.call_soon_threadsafe(exited.set)

    def cancel(stream):
        original_cancel(stream)
        loop.call_soon_threadsafe(cleaned.set)

    monkeypatch.setattr(ScannerWindowAdapter, "scan", scan)
    monkeypatch.setattr(GuardrailStream, "feed", feed)
    monkeypatch.setattr(GuardrailStream, "cancel", cancel)
    finish = Mock(side_effect=AssertionError("Unexpected finish after cancellation"))
    monkeypatch.setattr(GuardrailStream, "finish", finish)
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        call = await open_sse_call(stub)
        await call.write(
            pb.ProcessingRequest(
                response_body=pb.HttpBody(body=make_content_event("safe " * 100))
            )
        )
        try:
            await asyncio.wait_for(entered.wait(), timeout=5)
            assert call.cancel()
            assert await call.code() == grpc.StatusCode.CANCELLED
            with pytest.raises(asyncio.CancelledError):
                await call.read()
        finally:
            release.set()
        await asyncio.wait_for(exited.wait(), timeout=5)
        await asyncio.wait_for(cleaned.wait(), timeout=5)
    finish.assert_not_called()


@pytest.mark.asyncio
@pytest.mark.parametrize("content_type", ["application/json", "text/plain", ""])
async def test_unsupported_response_rejected_before_header_ack(
    unused_tcp_port, content_type
):
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        response = await stub.Process(
            iter([make_response_headers_request(content_type)]), timeout=5
        ).read()
    assert response.immediate_response.status.code == 502


def test_unsupported_streaming_policy_fails_startup():
    config = GuardrailPolicyConfig.from_dict(
        {
            "scanner_configs": [{"type": "regex", "patterns": ["secret"]}],
        }
    )
    with pytest.raises(StreamingPolicyError):
        ExternalProcessorServicer(config, streaming=True)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "mode",
    [
        ProcessingMode.BUFFERED,
        ProcessingMode.FULL_DUPLEX_STREAMED,
    ],
)
async def test_declared_non_streamed_mode_fails_closed(unused_tcp_port, mode):
    request = make_response_headers_request()
    request.protocol_config.response_body_mode = mode
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        response = await stub.Process(iter([request]), timeout=5).read()
    assert response.immediate_response.status.code == 502
    assert response.immediate_response.details == "guardrail_unsupported_mode"


@pytest.mark.asyncio
async def test_header_responses_do_not_override_configured_mode(unused_tcp_port):
    async with running_processor(make_policy(), unused_tcp_port) as stub:
        call = stub.Process(timeout=5)
        await call.write(pb.ProcessingRequest(request_headers=pb.HttpHeaders()))
        request_ack = await call.read()
        assert request_ack.HasField("request_headers")
        assert not request_ack.HasField("mode_override")
        await call.write(make_response_headers_request())
        response_ack = await call.read()
        assert response_ack.HasField("response_headers")
        assert not response_ack.HasField("mode_override")
        result = await send_body_and_read_response(
            call, make_content_event("hello") + DONE, final=True
        )
    assert extract_stream_text(extract_replacement_body(result)) == "hello"
