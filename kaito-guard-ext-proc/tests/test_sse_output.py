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

import pytest
from guardrail_core.streaming.openai import build_sse_data_chunk

from ext_proc.guardrail_stream import GuardrailStream, GuardrailStreamError
from tests.test_sse_process import DONE, event, payloads, policy, text


@pytest.mark.parametrize(
    "delta",
    [
        {"tool_calls": []},
        {"function_call": {}},
        {"refusal": "unscanned"},
        {"audio": "unscanned"},
        {"role": "user"},
    ],
)
def test_unsupported_deltas_fail_closed(delta):
    stream = GuardrailStream.from_policy(policy(), sse_output=True)
    stream.feed(event("pending"))
    with pytest.raises(GuardrailStreamError):
        stream.feed(
            build_sse_data_chunk(
                {
                    "choices": [{"index": 0, "delta": delta}],
                }
            ).encode()
        )
    assert stream.closed


def test_usage_is_not_forwarded_as_unvalidated_payload():
    stream = GuardrailStream.from_policy(policy(), sse_output=True)
    result = stream.feed(
        event("hello")
        + build_sse_data_chunk(
            {"choices": [], "usage": {"unvalidated": "private"}}
        ).encode()
        + DONE
    )
    finished = stream.finish()
    wire = "".join(result.chunks + finished.chunks).encode()
    assert text(wire) == "hello"
    assert b"usage" not in wire
    assert b"private" not in wire
    assert wire.endswith(DONE)


@pytest.mark.parametrize("metadata", [{"id": {}}, {"created": True}, {"model": []}])
def test_protocol_metadata_is_type_checked(metadata):
    stream = GuardrailStream.from_policy(policy(), sse_output=True)
    with pytest.raises(GuardrailStreamError):
        stream.feed(build_sse_data_chunk({**metadata, "choices": []}).encode())


def test_finish_reason_does_not_replace_done():
    stream = GuardrailStream.from_policy(policy(), sse_output=True)
    stream.feed(event("pending"))
    assert (
        stream.feed(
            build_sse_data_chunk(
                {
                    "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                }
            ).encode()
        ).chunks
        == ()
    )
    with pytest.raises(GuardrailStreamError):
        stream.finish()
    assert stream.closed


def test_metadata_order_is_preserved_and_terminal_event_follows_text():
    stream = GuardrailStream.from_policy(policy(), sse_output=True)
    wire = b""
    for model in ("first", "second"):
        wire += "".join(
            stream.feed(
                build_sse_data_chunk(
                    {
                        "model": model,
                        "choices": [{"index": 0, "delta": {"role": "assistant"}}],
                    }
                ).encode()
            ).chunks
        ).encode()
    stream.feed(event("hello") + DONE)
    wire += "".join(stream.finish().chunks).encode()
    assert [item["model"] for item in payloads(wire) if "model" in item] == [
        "first",
        "second",
    ]
    assert text(wire) == "hello"
