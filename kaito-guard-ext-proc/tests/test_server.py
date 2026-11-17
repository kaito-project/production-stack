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
from unittest.mock import AsyncMock, Mock

import grpc
import pytest
from envoy.config.core.v3 import base_pb2
from envoy.extensions.filters.http.ext_proc.v3.processing_mode_pb2 import ProcessingMode
from envoy.service.ext_proc.v3 import (
    external_processor_pb2,
    external_processor_pb2_grpc,
)

from ext_proc import __version__
from ext_proc.policy import GuardrailPolicyConfig
from ext_proc.server import ExternalProcessorServicer, serve

pb = external_processor_pb2


def test_version():
    assert __version__ == "0.1.0"


@pytest.mark.asyncio
@pytest.mark.parametrize("configured", [False, True])
@pytest.mark.parametrize(
    "host, target", [("127.0.0.1", "127.0.0.1"), ("[::]", "[::1]")]
)
async def test_serve_accepts_ext_proc_requests_and_stops(
    unused_tcp_port, host, target, configured
):
    address = f"{target}:{unused_tcp_port}"
    task = asyncio.create_task(
        serve(
            host=host, port=unused_tcp_port,
            policy=make_policy() if configured else None,
        )
    )
    try:
        async with grpc.aio.insecure_channel(address) as channel:
            await asyncio.wait_for(channel.channel_ready(), timeout=5)
            stub = external_processor_pb2_grpc.ExternalProcessorStub(channel)
            requests = [
                response_headers(),
                pb.ProcessingRequest(
                    response_body=pb.HttpBody(
                        body=chat_body("secret"), end_of_stream=True
                    )
                ),
            ]
            responses = [
                response async for response in stub.Process(iter(requests), timeout=5)
            ]
            assert responses[0].HasField("response_headers")
            assert responses[1].WhichOneof("response") == (
                "immediate_response" if configured else "response_body"
            )
            if configured:
                assert responses[1].immediate_response.status.code == 403
    finally:
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    async with grpc.aio.insecure_channel(address) as channel:
        stub = external_processor_pb2_grpc.ExternalProcessorStub(channel)
        with pytest.raises(grpc.aio.AioRpcError) as exc:
            await stub.Process(iter([]), timeout=1).read()
        assert exc.value.code() == grpc.StatusCode.UNAVAILABLE


@pytest.mark.asyncio
async def test_serve_reports_bind_failure(monkeypatch):
    server = Mock()
    server.add_insecure_port.return_value = 0
    server.stop = AsyncMock()
    monkeypatch.setattr(grpc.aio, "server", lambda: server)

    with pytest.raises(RuntimeError, match="Failed to bind"):
        await asyncio.wait_for(serve(), timeout=1)

    server.start.assert_not_called()
    server.stop.assert_awaited_once_with(grace=0)


def make_policy(*scanners, action="block"):
    return GuardrailPolicyConfig.from_dict(
        {
            "action": action,
            "blockMessage": "Blocked by test policy.",
            "scanner_configs": list(scanners)
            or [{"type": "ban_substrings", "substrings": ["secret"]}],
        }
    )


def chat_body(*contents):
    return json.dumps(
        {
            "id": "chatcmpl-test",
            "object": "chat.completion",
            "choices": [
                {
                    "index": index,
                    "message": {"role": "assistant", "content": content},
                    "finish_reason": "stop",
                }
                for index, content in enumerate(contents)
            ],
            "usage": {"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
        },
        ensure_ascii=False,
    ).encode()


def response_headers(content_type="application/json", **extra):
    return pb.ProcessingRequest(
        response_headers=pb.HttpHeaders(
            headers=base_pb2.HeaderMap(
                headers=[
                    base_pb2.HeaderValue(key=key, raw_value=value.encode())
                    for key, value in {
                        ":status": "200",
                        "content-type": content_type,
                        **extra,
                    }.items()
                ]
            )
        )
    )


@asynccontextmanager
async def processor(policy):
    server = grpc.aio.server()
    external_processor_pb2_grpc.add_ExternalProcessorServicer_to_server(
        ExternalProcessorServicer(policy), server
    )
    port = server.add_insecure_port("127.0.0.1:0")
    assert port
    await server.start()
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{port}") as channel:
            await asyncio.wait_for(channel.channel_ready(), timeout=5)
            yield external_processor_pb2_grpc.ExternalProcessorStub(channel)
    finally:
        await server.stop(grace=0)


async def exchange(stub, body, *, headers=None, complete=True):
    call = stub.Process(timeout=5)
    try:
        await call.write(headers if headers is not None else response_headers())
        head = await call.read()
        if head.HasField("immediate_response"):
            assert await call.read() is grpc.aio.EOF
            return head, None
        assert head.WhichOneof("response") == "response_headers"
        assert head.mode_override.response_body_mode == ProcessingMode.BUFFERED
        await call.write(
            pb.ProcessingRequest(
                response_body=pb.HttpBody(body=body, end_of_stream=complete)
            )
        )
        result = await call.read()
        await call.done_writing()
        assert await call.read() is grpc.aio.EOF
        return head, result
    finally:
        call.cancel()


@pytest.mark.asyncio
@pytest.mark.parametrize("text", ["safe output", "", " \n", "\u5b89\u5168\u7684\u56de\u590d"])
async def test_process_passes_original_json_bytes(text):
    async with processor(make_policy()) as stub:
        _, result = await exchange(stub, chat_body(text))
    assert result == pb.ProcessingResponse(response_body=pb.BodyResponse())


@pytest.mark.asyncio
async def test_process_redacts_each_choice_preserving_other_fields():
    policy = make_policy(action="redact")
    body = chat_body("a secret", "safe reply", "\u53e6\u4e00\u4e2a secret")
    async with processor(policy) as stub:
        _, result = await exchange(
            stub, body, headers=response_headers("Application/JSON; charset=utf-8")
        )
    common = result.response_body.response
    expected = json.loads(body)
    expected["choices"][0]["message"]["content"] = "a [REDACTED]"
    expected["choices"][2]["message"]["content"] = "\u53e6\u4e00\u4e2a [REDACTED]"
    assert json.loads(common.body_mutation.body) == expected
    assert set(common.header_mutation.remove_headers) == {
        "etag", "content-md5", "digest", "content-digest", "repr-digest"
    }
    assert common.header_mutation.set_headers[0].header.key == "content-length"
    assert int(common.header_mutation.set_headers[0].header.raw_value) == len(
        common.body_mutation.body
    )


@pytest.mark.asyncio
async def test_process_blocks_whole_response_on_later_choice():
    async with processor(make_policy()) as stub:
        _, result = await exchange(stub, chat_body("safe first choice", "a secret"))
    assert result.WhichOneof("response") == "immediate_response"
    assert result.immediate_response.status.code == 403
    assert result.immediate_response.body == b"Blocked by test policy."
    assert result.immediate_response.details == "guardrail_blocked"


@pytest.mark.asyncio
async def test_process_honors_scanner_overrides_and_duplicate_scanner_types():
    policy = make_policy(
        {"type": "ban_substrings", "substrings": ["secret"], "action": "redact"},
        {"type": "ban_substrings", "substrings": ["forbidden"], "action": "block"},
        {"type": "ban_substrings", "substrings": ["safe"], "action": "block"},
        action="redact",
    )
    async with processor(policy) as stub:
        _, redacted = await exchange(stub, chat_body("a secret"))
        _, blocked = await exchange(stub, chat_body("forbidden"))
    assert json.loads(redacted.response_body.response.body_mutation.body)[
        "choices"
    ][0]["message"]["content"] == "a [REDACTED]"
    assert blocked.immediate_response.status.code == 403


@pytest.mark.asyncio
async def test_process_preserves_configured_block_before_redact_order():
    policy = make_policy(
        {"type": "ban_substrings", "substrings": ["secret"]},
        {"type": "regex", "patterns": ["secret"], "action": "redact"},
    )
    async with processor(policy) as stub:
        _, result = await exchange(stub, chat_body("secret"))
    assert result.immediate_response.status.code == 403


@pytest.mark.asyncio
async def test_process_allows_empty_redacted_content():
    policy = make_policy({"type": "invisible_text", "action": "redact"})
    async with processor(policy) as stub:
        _, result = await exchange(stub, chat_body("\u200b"))
    assert json.loads(result.response_body.response.body_mutation.body)[
        "choices"
    ][0]["message"]["content"] == ""


@pytest.mark.asyncio
async def test_process_rechecks_real_secrets_redaction():
    policy = make_policy({"type": "secrets", "action": "redact"})
    text = 'key = "' + "AKIA" + "Z" * 16 + '"'
    async with processor(policy) as stub:
        _, result = await exchange(stub, chat_body(text))
    assert json.loads(result.response_body.response.body_mutation.body)[
        "choices"
    ][0]["message"]["content"] == 'key = "******"'


@pytest.mark.asyncio
@pytest.mark.parametrize("valid, changed", [(False, False), (True, True)])
async def test_process_blocks_failed_secrets_revalidation(monkeypatch, valid, changed):
    from guardrail_core.scanning import ScanResult

    policy = make_policy({"type": "secrets", "action": "redact"})
    scanner = policy.build_scanners()[0]
    scanner.scan = Mock(
        side_effect=[
            ScanResult("redacted", False, 1.0),
            ScanResult("changed" if changed else "redacted", valid, 1.0),
        ]
    )
    monkeypatch.setattr(GuardrailPolicyConfig, "build_scanners", lambda self: (scanner,))
    async with processor(policy) as stub:
        _, result = await exchange(stub, chat_body("secret"))
    assert result.immediate_response.status.code == 403


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "body",
    [
        b"not-json secret",
        b"\xff",
        b'{"choices":',
        b"[]",
        b"null",
        b"{}",
        b'{"choices":[]}',
        b'{"choices":{}}',
        b'{"choices":[null]}',
        b'{"choices":[{"delta":{"content":"secret"}}]}',
        b'{"choices":[{"message":{"content":42}}]}',
        b'{"choices":[{"message":{"content":["secret"]}}]}',
        b'{"choices":[{"message":{"content":null,"tool_calls":[]}}]}',
        b'{"choices":[{"message":{"content":"safe"}}],"usage":NaN}',
    ],
)
async def test_process_rejects_malformed_or_non_text_responses(body):
    async with processor(make_policy()) as stub:
        _, result = await exchange(stub, body)
    assert result.immediate_response.status.code == 502
    assert result.immediate_response.body == b"Guardrail response processing failed."
    assert "secret" not in str(result)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "body",
    [
        b'{"choices":[{"message":{"content":"secret","content":"safe"}}]}',
        b'{"choices":[{"message":{"content":"secret","\\u0063ontent":"safe"}}]}',
        b'{"choices":[{"message":{"content":"secret"},"message":{"content":"safe"}}]}',
        b'{"choices":[{"message":{"content":"secret"}}],"choices":[{"message":{"content":"safe"}}]}',
        b'{"choices":[{"message":{"content":"safe"}}],"usage":{"total_tokens":1,"total_tokens":2}}',
    ],
)
async def test_process_rejects_duplicate_json_keys_without_releasing_original(body):
    async with processor(make_policy()) as stub:
        _, result = await exchange(stub, body)
    assert result.WhichOneof("response") == "immediate_response"
    assert result.immediate_response.status.code == 502
    assert result.immediate_response.body == b"Guardrail response processing failed."
    assert body not in result.SerializeToString()
    assert b"secret" not in result.SerializeToString()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "headers",
    [
        response_headers("text/event-stream"),
        response_headers("text/plain"),
        response_headers(""),
        response_headers(**{"content-encoding": "gzip"}),
        pb.ProcessingRequest(response_headers=pb.HttpHeaders(end_of_stream=True)),
    ],
)
async def test_process_rejects_unsupported_response_headers(headers):
    async with processor(make_policy()) as stub:
        result, body = await exchange(stub, b"", headers=headers)
    assert body is None
    assert result.immediate_response.status.code == 502
    assert result.immediate_response.details == "guardrail_unsupported_response"


@pytest.mark.asyncio
async def test_process_rejects_partial_body():
    async with processor(make_policy()) as stub:
        _, result = await exchange(stub, chat_body("safe"), complete=False)
    assert result.immediate_response.status.code == 502
    assert result.immediate_response.details == "guardrail_incomplete_response"


@pytest.mark.asyncio
async def test_scanner_error_fails_closed_without_leaking(monkeypatch, caplog):
    from guardrail_core.scanning import Scanner, ScanResult

    class BrokenScanner(Scanner):
        def scan(self, prompt: str, output: str) -> ScanResult:
            raise RuntimeError("private-model-text")

    monkeypatch.setattr(
        GuardrailPolicyConfig, "build_scanners", lambda self: (BrokenScanner(),)
    )
    async with processor(make_policy()) as stub:
        _, result = await exchange(stub, chat_body("private-model-text"))
    assert result.immediate_response.status.code == 502
    assert "private-model-text" not in str(result)
    assert "private-model-text" not in caplog.text
    assert "Guardrail response parsing or scanning failed" in caplog.text


@pytest.mark.asyncio
async def test_process_acknowledges_request_headers_and_requests_buffering():
    async with processor(make_policy()) as stub:
        call = stub.Process(timeout=5)
        await call.write(pb.ProcessingRequest(request_headers=pb.HttpHeaders()))
        response = await call.read()
        assert response.HasField("request_headers")
        assert response.mode_override.response_body_mode == ProcessingMode.BUFFERED
        assert response.mode_override.request_body_mode == ProcessingMode.NONE
        await call.write(response_headers())
        assert (await call.read()).HasField("response_headers")
        await call.write(
            pb.ProcessingRequest(
                response_body=pb.HttpBody(body=chat_body("safe"), end_of_stream=True)
            )
        )
        assert (await call.read()).HasField("response_body")
        await call.done_writing()
        assert await call.read() is grpc.aio.EOF


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "requests, code",
    [
        ([pb.ProcessingRequest()], grpc.StatusCode.INVALID_ARGUMENT),
        ([pb.ProcessingRequest(response_body=pb.HttpBody())], grpc.StatusCode.INVALID_ARGUMENT),
        ([response_headers(), response_headers()], grpc.StatusCode.INVALID_ARGUMENT),
        ([response_headers()], grpc.StatusCode.FAILED_PRECONDITION),
        ([], grpc.StatusCode.FAILED_PRECONDITION),
        (
            [pb.ProcessingRequest(response_headers=pb.HttpHeaders(), observability_mode=True)],
            grpc.StatusCode.FAILED_PRECONDITION,
        ),
    ],
)
async def test_process_rejects_invalid_sequence_or_mode(requests, code):
    async with processor(make_policy()) as stub:
        with pytest.raises(grpc.aio.AioRpcError) as exc:
            async for _ in stub.Process(iter(requests), timeout=5):
                pass
    assert exc.value.code() == code


@pytest.mark.asyncio
async def test_unconfigured_policy_is_explicitly_disabled(caplog):
    requests = [
        response_headers("text/plain"),
        pb.ProcessingRequest(response_body=pb.HttpBody(body=b"secret", end_of_stream=True)),
    ]
    async with processor(None) as stub:
        responses = [response async for response in stub.Process(iter(requests), timeout=5)]
    assert responses == [
        pb.ProcessingResponse(response_headers=pb.HeadersResponse()),
        pb.ProcessingResponse(response_body=pb.BodyResponse()),
    ]
    assert "response protection is disabled" in caplog.text


@pytest.mark.asyncio
async def test_process_isolates_concurrent_and_cancelled_calls():
    async with processor(make_policy()) as stub:
        call = stub.Process(timeout=5)
        await call.write(response_headers())
        assert (await call.read()).HasField("response_headers")
        call.cancel()
        (_, passed), (_, blocked) = await asyncio.gather(
            exchange(stub, chat_body("safe")),
            exchange(stub, chat_body("secret")),
        )
        _, next_passed = await exchange(stub, chat_body("still safe"))
    assert passed.HasField("response_body")
    assert blocked.immediate_response.status.code == 403
    assert next_passed.HasField("response_body")


@pytest.mark.asyncio
async def test_configured_scanner_construction_failure_prevents_startup(monkeypatch):
    def fail(self):
        raise RuntimeError("scanner construction failed")

    monkeypatch.setattr(GuardrailPolicyConfig, "build_scanners", fail)
    with pytest.raises(RuntimeError, match="scanner construction failed"):
        await serve(host="127.0.0.1", port=0, policy=make_policy())
