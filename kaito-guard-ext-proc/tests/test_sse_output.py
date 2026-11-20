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
from tests.sse_helpers import (
    DONE,
    RESPONSE_METADATA,
    extract_stream_text,
    make_content_event,
    make_policy,
    parse_sse_payloads,
)


@pytest.mark.parametrize("choice_metadata", [{}, {"logprobs": None}])
def test_body_events_are_self_contained_without_empty_metadata_events(choice_metadata):
    stream = GuardrailStream.from_policy(make_policy("redact"), sse_output=True)
    initial = stream.feed(
        build_sse_data_chunk(
            {
                **RESPONSE_METADATA,
                "choices": [
                    {"index": 0, "delta": {"content": "hello secret"}, **choice_metadata}
                ],
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


def test_long_response_emits_redacted_sse_before_eof():
    stream = GuardrailStream.from_policy(make_policy("redact"), sse_output=True)
    suffix = "ordinary text " * 120
    result = stream.feed(make_content_event("hello secret " + suffix))
    early = "".join(result.chunks).encode()
    assert extract_stream_text(early).startswith("hello [REDACTED] ")
    assert DONE not in early

    assert stream.feed(DONE).chunks == ()
    final = "".join(stream.finish().chunks).encode()
    assert extract_stream_text(early + final) == "hello [REDACTED] " + suffix
    assert final.endswith(DONE)
    assert final.count(DONE) == 1


def test_vllm_terminal_chunk_drops_null_extensions():
    # Text-only terminal shape from vLLM v0.30.0 chat_completion/serving.py.
    terminal = build_sse_data_chunk({
        **RESPONSE_METADATA,
        "choices": [{
            "index": 0, "delta": {}, "logprobs": None, "finish_reason": "stop",
            "stop_reason": None, "token_ids": None,
        }],
    }).encode()
    stream = GuardrailStream.from_policy(make_policy("redact"), sse_output=True)
    result = stream.feed(make_content_event("secret") + terminal + DONE)
    wire = "".join(result.chunks + stream.finish().chunks).encode()
    assert extract_stream_text(wire) == "[REDACTED]"
    assert parse_sse_payloads(wire)[-1] == {
        **RESPONSE_METADATA,
        "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
    }
    assert wire.endswith(DONE)


@pytest.mark.parametrize("extra", [{"stop_reason": 0}, {"token_ids": []}, {"unknown": None}])
def test_unsupported_choice_extensions_fail_closed(extra):
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    with pytest.raises(GuardrailStreamError):
        stream.feed(build_sse_data_chunk({
            **RESPONSE_METADATA,
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop", **extra}],
        }).encode())
    assert stream.closed


def test_block_at_eof_discards_sse_text_finish_and_done():
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    terminal = build_sse_data_chunk({
        "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
    }).encode()
    pending = stream.feed(make_content_event("secret") + terminal + DONE)
    assert not pending.blocked and pending.chunks == ()
    result = stream.finish()
    assert result.blocked and result.chunks == ()
    assert stream.closed


def test_non_null_logprobs_fails_closed():
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    with pytest.raises(GuardrailStreamError, match="Non-null OpenAI logprobs"):
        stream.feed(
            build_sse_data_chunk(
                {
                    **RESPONSE_METADATA,
                    "choices": [
                        {"index": 0, "delta": {"content": "hello"}, "logprobs": {}}
                    ],
                }
            ).encode()
        )
    assert stream.closed


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
            {"id": "other-response", "choices": [], "usage": {"unvalidated": "private"}}
        ).encode()
        + DONE
    )
    finished = stream.finish()
    wire = "".join(result.chunks + finished.chunks).encode()
    assert extract_stream_text(wire) == "hello"
    assert b"usage" not in wire
    assert b"private" not in wire
    assert wire.endswith(DONE)


@pytest.mark.parametrize(
    "metadata",
    [{"id": {}}, {"object": "chat.completion"}, {"created": True}, {"model": []}],
)
def test_protocol_metadata_is_type_checked(metadata):
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    with pytest.raises(GuardrailStreamError):
        stream.feed(
            build_sse_data_chunk(
                {
                    **RESPONSE_METADATA,
                    **metadata,
                    "choices": [{"index": 0, "delta": {"role": "assistant"}}],
                }
            ).encode()
        )


def test_invalid_metadata_on_empty_prelude_is_ignored():
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    prelude = build_sse_data_chunk({"id": None, "choices": []}).encode()
    assert stream.feed(prelude).chunks == ()
    result = stream.feed(make_content_event("hello") + DONE)
    wire = "".join(result.chunks + stream.finish().chunks).encode()
    assert extract_stream_text(wire) == "hello"
    assert wire.endswith(DONE)


@pytest.mark.parametrize(
    "reason", ["stop", "length", "tool_calls", "content_filter", "function_call"]
)
def test_openai_finish_reason_is_preserved_at_eof(reason):
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    terminal = build_sse_data_chunk(
        {"choices": [{"index": 0, "delta": {}, "finish_reason": reason}]}
    ).encode()
    assert stream.feed(make_content_event("hello") + terminal + DONE).chunks == ()
    wire = "".join(stream.finish().chunks).encode()
    assert parse_sse_payloads(wire)[-1]["choices"][0]["finish_reason"] == reason
    assert wire.endswith(DONE)


def test_unknown_finish_reason_aborts_without_terminal_events():
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    assert stream.feed(make_content_event("hello")).chunks == ()
    terminal = build_sse_data_chunk(
        {"choices": [{"index": 0, "delta": {}, "finish_reason": "unknown-status"}]}
    ).encode()
    with pytest.raises(GuardrailStreamError, match="Unsupported OpenAI finish_reason"):
        stream.feed(terminal + DONE)
    assert stream.closed


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


@pytest.mark.parametrize(
    "later_metadata",
    [
        {},
        {"id": RESPONSE_METADATA["id"]},
        {"id": "other-response"},
        {"id": None},
        {"id": 123},
        {"id": RESPONSE_METADATA["id"], "created": 2, "model": "other-model"},
        {"object": "chat.completion", "created": 1.0, "model": None},
    ],
)
def test_cached_response_metadata_is_reused_on_text_and_terminal_events(later_metadata):
    stream = GuardrailStream.from_policy(make_policy("redact"), sse_output=True)
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
                    **later_metadata,
                    "choices": [{"index": 0, "delta": {"content": "secret"}}],
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
    assert extract_stream_text(wire) == "[REDACTED]"
    assert events[-1]["choices"][0]["finish_reason"] == "stop"


@pytest.mark.parametrize("key", list(RESPONSE_METADATA))
def test_missing_initial_response_metadata_fails_without_fabricating_fields(key):
    stream = GuardrailStream.from_policy(make_policy(), sse_output=True)
    stream.feed(build_sse_data_chunk({**RESPONSE_METADATA, "choices": []}).encode())
    chunk_metadata = {
        name: value for name, value in RESPONSE_METADATA.items() if name != key
    }
    with pytest.raises(GuardrailStreamError, match="Missing"):
        stream.feed(
            build_sse_data_chunk(
                {
                    **chunk_metadata,
                    "choices": [{"index": 0, "delta": {"role": "assistant"}}],
                }
            ).encode()
        )
    assert stream.closed
