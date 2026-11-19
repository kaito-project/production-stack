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
from tests.test_sse_process import (
    DONE,
    RESPONSE_METADATA,
    extract_stream_text,
    make_content_event,
    make_policy,
    parse_sse_payloads,
)


def test_body_events_are_self_contained_without_empty_metadata_events():
    stream = GuardrailStream.from_policy(make_policy("redact"), sse_output=True)
    initial = stream.feed(
        build_sse_data_chunk(
            {
                **RESPONSE_METADATA,
                "choices": [{"index": 0, "delta": {"content": "hello secret"}}],
            }
        ).encode()
        + build_sse_data_chunk(
            {
                **RESPONSE_METADATA,
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            }
        ).encode()
        + DONE
    )
    assert initial.chunks == ()
    wire = "".join(stream.finish().chunks).encode()
    assert parse_sse_payloads(wire) == [
        {
            **RESPONSE_METADATA,
            "choices": [
                {
                    "index": 0,
                    "delta": {"content": "hello [REDACTED]"},
                    "finish_reason": None,
                }
            ],
        },
        {
            **RESPONSE_METADATA,
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
        },
    ]
    assert wire.endswith(DONE)


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
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    stream.feed(make_content_event("pending"))
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
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    result = stream.feed(
        make_content_event("hello")
        + build_sse_data_chunk(
            {"choices": [], "usage": {"unvalidated": "private"}}
        ).encode()
        + DONE
    )
    finished = stream.finish()
    wire = "".join(result.chunks + finished.chunks).encode()
    assert extract_stream_text(wire) == "hello"
    assert b"usage" not in wire
    assert b"private" not in wire
    assert wire.endswith(DONE)


@pytest.mark.parametrize("metadata", [{"id": {}}, {"created": True}, {"model": []}])
def test_protocol_metadata_is_type_checked(metadata):
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    with pytest.raises(GuardrailStreamError):
        stream.feed(build_sse_data_chunk({**metadata, "choices": []}).encode())


def test_finish_reason_does_not_replace_done():
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    stream.feed(make_content_event("pending"))
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


def test_cached_response_metadata_is_reused_on_text_and_terminal_events():
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    wire = "".join(
        stream.feed(
            build_sse_data_chunk(
                {
                    **RESPONSE_METADATA,
                    "choices": [{"index": 0, "delta": {"role": "assistant"}}],
                }
            ).encode()
        ).chunks
    ).encode()
    assert (
        stream.feed(
            build_sse_data_chunk(
                {
                    "choices": [{"index": 0, "delta": {"content": "hello"}}],
                }
            ).encode()
        ).chunks
        == ()
    )
    assert (
        stream.feed(
            build_sse_data_chunk(
                {
                    "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                }
            ).encode()
            + DONE
        ).chunks
        == ()
    )
    wire += "".join(stream.finish().chunks).encode()
    events = parse_sse_payloads(wire)
    assert len(events) == 3
    assert all(
        {key: item[key] for key in RESPONSE_METADATA} == RESPONSE_METADATA
        for item in events
    )
    assert events[0]["choices"][0]["delta"] == {"role": "assistant"}
    assert extract_stream_text(wire) == "hello"
    assert events[-1]["choices"][0]["finish_reason"] == "stop"


@pytest.mark.parametrize("key", list(RESPONSE_METADATA))
def test_missing_initial_response_metadata_fails_without_fabricating_fields(key):
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    chunk_metadata = {
        name: value for name, value in RESPONSE_METADATA.items() if name != key
    }
    with pytest.raises(GuardrailStreamError, match="Missing"):
        stream.feed(
            build_sse_data_chunk(
                {
                    **chunk_metadata,
                    "choices": [{"index": 0, "delta": {"content": "pending"}}],
                }
            ).encode()
        )
    assert stream.closed


@pytest.mark.parametrize(
    "key, value",
    [
        ("id", "other-response"),
        ("model", "other-model"),
        ("created", 2),
        ("object", "chat.completion"),
    ],
)
def test_changing_response_metadata_fails_closed(key, value):
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    stream.feed(make_content_event("pending"))
    with pytest.raises(GuardrailStreamError):
        stream.feed(
            build_sse_data_chunk(
                {
                    **RESPONSE_METADATA,
                    key: value,
                    "choices": [],
                }
            ).encode()
        )
    assert stream.closed
