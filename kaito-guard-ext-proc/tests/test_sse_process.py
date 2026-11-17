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
import json
from contextlib import asynccontextmanager
from unittest.mock import Mock

import grpc
import pytest
from envoy.config.core.v3 import base_pb2
from envoy.extensions.filters.http.ext_proc.v3.processing_mode_pb2 import ProcessingMode
from envoy.service.ext_proc.v3 import external_processor_pb2 as pb
from envoy.service.ext_proc.v3.external_processor_pb2_grpc import ExternalProcessorStub
from guardrail_core.streaming.openai import (
    build_openai_chat_delta_sse_chunk,
    build_sse_data_chunk,
    build_sse_done_chunk,
)
from guardrail_core.streaming.scanners import ScannerWindowAdapter, StreamingPolicyError
from guardrail_core.streaming.sse import SSEFramer

from ext_proc.guardrail_stream import GuardrailStream
from ext_proc.policy import GuardrailPolicyConfig
from ext_proc.server import ExternalProcessorServicer, serve

DONE = build_sse_done_chunk().encode()


def event(content):
    return build_openai_chat_delta_sse_chunk(content).encode()


def policy(action="block"):
    return GuardrailPolicyConfig.from_dict(
        {
            "action": action,
            "blockMessage": "Blocked by streaming policy.",
            "scanner_configs": [{"type": "ban_substrings", "substrings": ["secret"]}],
        }
    )


def headers(content_type="text/event-stream"):
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


def payloads(wire):
    framer = SSEFramer()
    events = framer.feed(wire) + framer.finish()
    return [
        json.loads(item.data)
        for item in events
        if item.data is not None and item.data != "[DONE]"
    ]


def text(wire):
    return "".join(
        choice.get("delta", {}).get("content", "")
        for item in payloads(wire)
        for choice in item.get("choices", [])
    )


@asynccontextmanager
async def processor(config, port):
    task = asyncio.create_task(serve("127.0.0.1", port, policy=config))
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{port}") as channel:
            await asyncio.wait_for(channel.channel_ready(), timeout=5)
            yield ExternalProcessorStub(channel)
    finally:
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task


async def open_call(stub):
    call = stub.Process(timeout=5)
    await call.write(headers("Text/Event-Stream; charset=utf-8"))
    response = await call.read()
    assert response.HasField("response_headers")
    assert response.mode_override.response_body_mode == ProcessingMode.STREAMED
    assert (
        "content-length"
        in response.response_headers.response.header_mutation.remove_headers
    )
    return call


async def body(call, wire, *, final=False):
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


def output(response):
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
            "choices": [{"index": 0, "delta": {"role": "assistant"}}],
        }
    ).encode()
    finish = build_sse_data_chunk(
        {
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
        }
    ).encode()
    async with processor(policy(), unused_tcp_port) as stub:
        call = await open_call(stub)
        first = output(await body(call, role + event("hello \u4f60\u597d")))
        assert payloads(first)[0] == payloads(role)[0]
        assert text(first) == ""
        pending = output(await body(call, finish + DONE))
        assert b"[DONE]" not in first + pending
        assert b'"finish_reason":"stop"' not in first + pending
        completed = output(await body(call, b"", final=True))
    assert text(completed) == "hello \u4f60\u597d"
    assert payloads(completed)[-1]["choices"][0]["finish_reason"] == "stop"
    assert completed.endswith(DONE)
    assert (first + pending + completed).count(DONE) == 1


@pytest.mark.asyncio
async def test_cross_chunk_redaction_uses_real_shared_scanners(unused_tcp_port):
    wire = event("safe " * 60 + "se") + event("cret \u56de\u590d") + DONE
    async with processor(policy("redact"), unused_tcp_port) as stub:
        call = await open_call(stub)
        received = b""
        for chunk in (wire[:7], wire[7:350], wire[350:-1], wire[-1:]):
            received += output(await body(call, chunk))
        received += output(await body(call, b"", final=True))
    assert text(received) == "safe " * 60 + "[REDACTED] \u56de\u590d"
    assert b"secret" not in received
    assert received.count(DONE) == 1


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "wire",
    [
        event("pending"),
        event("pending") + DONE + b"data: unfinished",
        event("pending") + DONE + event("late"),
        event("pending") + DONE + b"data:\n\n",
        event("pending") + DONE + DONE,
        event("pending") + b"data: {invalid}\n\n",
    ],
)
async def test_truncation_or_invalid_sse_never_flushes_holdback(unused_tcp_port, wire):
    async with processor(policy(), unused_tcp_port) as stub:
        call = await open_call(stub)
        response = await body(call, wire, final=True)
    assert response.immediate_response.status.code == 502
    assert b"pending" not in response.SerializeToString()
    assert b"[DONE]" not in response.SerializeToString()


@pytest.mark.asyncio
async def test_block_discards_pending_output(unused_tcp_port):
    async with processor(policy(), unused_tcp_port) as stub:
        call = await open_call(stub)
        response = await body(call, event("a secret") + DONE, final=True)
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
    async with processor(policy(), unused_tcp_port) as stub:
        call = await open_call(stub)
        wire = event("private-text" if final else "private-text " * 30) + DONE
        response = await body(call, wire, final=final)
    assert response.immediate_response.status.code == 502
    assert b"private-text" not in response.SerializeToString()
    assert "private-text" not in caplog.text
    assert "Guardrail streaming processing failed" in caplog.text


@pytest.mark.asyncio
async def test_rpc_eof_is_not_http_eof(unused_tcp_port):
    async with processor(policy(), unused_tcp_port) as stub:
        call = await open_call(stub)
        assert output(await body(call, event("pending") + DONE)) == b""
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
    async with processor(policy(), unused_tcp_port) as stub:
        call = await open_call(stub)
        assert output(await body(call, event("pending") + DONE)) == b""
        call.cancel()
        await asyncio.wait_for(cancelled.wait(), timeout=5)
    assert created[0].closed
    finish.assert_not_called()


@pytest.mark.asyncio
@pytest.mark.parametrize("content_type", ["application/json", "text/plain", ""])
async def test_unsupported_response_rejected_before_header_ack(
    unused_tcp_port, content_type
):
    async with processor(policy(), unused_tcp_port) as stub:
        response = await stub.Process(iter([headers(content_type)]), timeout=5).read()
    assert response.immediate_response.status.code == 502


def test_unsupported_streaming_policy_fails_startup():
    config = GuardrailPolicyConfig.from_dict(
        {
            "scanner_configs": [{"type": "regex", "patterns": ["secret"]}],
        }
    )
    with pytest.raises(StreamingPolicyError):
        ExternalProcessorServicer(config)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "mode",
    [
        ProcessingMode.BUFFERED,
        ProcessingMode.FULL_DUPLEX_STREAMED,
    ],
)
async def test_declared_non_streamed_mode_fails_closed(unused_tcp_port, mode):
    request = headers()
    request.protocol_config.response_body_mode = mode
    async with processor(policy(), unused_tcp_port) as stub:
        response = await stub.Process(iter([request]), timeout=5).read()
    assert response.immediate_response.status.code == 502
    assert response.immediate_response.details == "guardrail_unsupported_mode"
