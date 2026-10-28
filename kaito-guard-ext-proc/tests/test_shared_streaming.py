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

"""Exercise the published streaming APIs together, without local safety wrappers."""

import asyncio
from importlib.metadata import metadata, requires, version
from unittest.mock import Mock, call

import pytest
from guardrail_core.streaming.openai import (
    OpenAIChatChunkParseStatus,
    build_openai_chat_delta_sse_chunk,
    build_sse_data_chunk,
    build_sse_done_chunk,
    parse_openai_chat_sse_event,
)
from guardrail_core.streaming.sse import (
    SSEDecodeError,
    SSEEvent,
    SSEEventTooLargeError,
    SSEFramer,
    SSEIncompleteEventError,
)
from guardrail_core.streaming.window import (
    StreamingBufferWindow,
    WindowEmitResult,
    WindowScanError,
    WindowScanner,
    WindowScanResult,
)


@pytest.fixture
def scanner():
    scanner = Mock(spec=WindowScanner)
    scanner.scan.return_value = WindowScanResult()
    return scanner


def test_published_dependency_contract():
    assert version("kaito-guardrail-core") == "0.1.1"
    dependencies = requires("kaito-guard-ext-proc")
    assert dependencies is not None
    assert "kaito-guardrail-core==0.1.1" in dependencies
    assert metadata("kaito-guard-ext-proc")["Requires-Python"] == ">=3.12"


@pytest.mark.parametrize("ending", ["\n", "\r\n", "\r"])
def test_framer_parser_and_holdback_at_every_byte_boundary(scanner, ending):
    content = "hello \u4f60\u597d"
    wire = (
        "\ufeff"
        + build_openai_chat_delta_sse_chunk(content).replace("\n", ending)
        + build_sse_done_chunk().replace("\n", ending)
    ).encode()

    for offset in range(len(wire) + 1):
        framer = SSEFramer()
        window = StreamingBufferWindow(scanner, holdback_len=3)
        emitted = []
        events = []
        statuses = []
        for chunk in (wire[:offset], wire[offset:]):
            batch = framer.feed(chunk)
            events.extend(batch)
            for event in batch:
                parsed = parse_openai_chat_sse_event(event)
                statuses.append(parsed.status)
                for choice in parsed.parsed_choices:
                    if choice.content is not None:
                        emitted.extend(window.feed(choice.content).chunks)
        for event in framer.finish():
            events.append(event)
            statuses.append(parse_openai_chat_sse_event(event).status)
        emitted.extend(window.flush().chunks)

        assert "".join(emitted) == content, offset
        assert b"".join(event.raw_bytes for event in events) == wire, offset
        assert statuses == [
            OpenAIChatChunkParseStatus.PARSED,
            OpenAIChatChunkParseStatus.DONE,
        ], offset


def test_shared_holdback_scans_before_release_and_preserves_context(scanner):
    window = StreamingBufferWindow(scanner, holdback_len=2)
    assert window.feed("hello") == WindowEmitResult(chunks=("hel",))
    assert window.feed(" world") == WindowEmitResult(chunks=("lo wor",))
    assert window.flush() == WindowEmitResult(chunks=("ld",))
    assert scanner.scan.call_args_list == [
        call("hello", flush=False, preceding_char=""),
        call("lo world", flush=False, preceding_char="l"),
        call("ld", flush=True, preceding_char="r"),
    ]


def test_holdback_is_measured_in_unicode_code_points(scanner):
    window = StreamingBufferWindow(scanner, holdback_len=1)
    assert window.feed("\u4f60\u597d").chunks == ("\u4f60",)
    assert window.flush().chunks == ("\u597d",)


@pytest.mark.parametrize("marker", ["secret", "\u654f\u611f"])
def test_shared_window_blocks_cross_chunk_matches(scanner, marker):
    scanner.scan.side_effect = lambda text, **_: WindowScanResult(
        blocked=marker in text
    )
    safe_prefix = "safe "
    for offset in range(1, len(marker)):
        window = StreamingBufferWindow(scanner, holdback_len=len(marker) - 1)
        first = window.feed(safe_prefix + marker[:offset])
        blocked = window.feed(marker[offset:])
        assert safe_prefix.startswith("".join(first.chunks)), offset
        assert blocked == WindowEmitResult(chunks=(), blocked=True), offset
        calls = scanner.scan.call_count
        assert window.feed("more").blocked
        assert window.flush().blocked
        assert scanner.scan.call_count == calls


def test_shared_window_redacts_cross_chunk_matches(scanner):
    scanner.scan.side_effect = lambda text, **_: WindowScanResult(
        sanitized_text=text.replace("secret", "[redacted]")
    )
    for offset in range(1, len("secret")):
        window = StreamingBufferWindow(scanner, holdback_len=5)
        results = [
            window.feed("safe " + "secret"[:offset]),
            window.feed("secret"[offset:]),
            window.flush(),
        ]
        assert "".join(chunk for result in results for chunk in result.chunks) == (
            "safe [redacted]"
        )
        assert window.redacted


def test_final_scan_can_block_the_retained_tail(scanner):
    scanner.scan.side_effect = lambda text, **kwargs: WindowScanResult(
        blocked=kwargs["flush"]
    )
    window = StreamingBufferWindow(scanner, holdback_len=10)
    assert window.feed("tail").chunks == ()
    scanner.scan.assert_not_called()
    assert window.flush() == WindowEmitResult(chunks=(), blocked=True)
    scanner.scan.assert_called_once_with("tail", flush=True, preceding_char="")


def test_shared_scanner_failure_is_terminal_and_preserves_cause(scanner):
    failure = RuntimeError("scanner unavailable")
    scanner.scan.side_effect = failure
    window = StreamingBufferWindow(scanner, holdback_len=0)
    with pytest.raises(WindowScanError) as error:
        window.feed("text")
    assert error.value.__cause__ is failure
    assert window.blocked
    assert window.feed("later").blocked
    assert window.flush().blocked
    scanner.scan.assert_called_once()


def test_shared_window_propagates_cancellation(scanner):
    scanner.scan.side_effect = asyncio.CancelledError()
    window = StreamingBufferWindow(scanner, holdback_len=0)
    with pytest.raises(asyncio.CancelledError):
        window.feed("text")
    scanner.scan.assert_called_once()


def test_windows_do_not_share_request_state(scanner):
    first = StreamingBufferWindow(scanner, holdback_len=10)
    second = StreamingBufferWindow(scanner, holdback_len=10)
    assert first.feed("first").chunks == ()
    assert second.feed("second").chunks == ()
    assert first.flush().chunks == ("first",)
    assert second.flush().chunks == ("second",)


@pytest.mark.parametrize("tail", [b"data: unfinished", b"data: \xe4", b"data: x\r"])
def test_truncated_eof_does_not_flush_pending_text(scanner, tail):
    window = StreamingBufferWindow(scanner, holdback_len=10)
    assert window.feed("tail").chunks == ()
    framer = SSEFramer()
    assert framer.feed(tail) == []
    with pytest.raises(SSEIncompleteEventError):
        framer.finish()
        window.flush()
    scanner.scan.assert_not_called()


def test_event_size_limit_is_not_disabled():
    framer = SSEFramer(max_event_bytes=16)
    with pytest.raises(SSEEventTooLargeError) as error:
        framer.feed(b"data: " + b"x" * 11)
    assert len(error.value.raw_bytes) == 16
    with pytest.raises(ValueError, match="closed"):
        framer.feed(b"\n\n")


def test_default_event_size_limit_is_enforced():
    framer = SSEFramer()
    with pytest.raises(SSEEventTooLargeError):
        framer.feed(b"x" * (1024 * 1024 + 1))


def test_invalid_utf8_fails_during_framing():
    with pytest.raises(SSEDecodeError):
        SSEFramer().feed(b"data: \xff\n\n")


def test_finish_does_not_make_the_framer_reusable():
    framer = SSEFramer()
    assert framer.finish() == []
    with pytest.raises(ValueError, match="closed"):
        framer.feed(b"data: [DONE]\n\n")


def test_shared_parser_requires_explicit_choice_index():
    event = SSEEvent.from_raw('data: {"choices":[{"delta":{"content":"text"}}]}')
    parsed = parse_openai_chat_sse_event(event)
    assert parsed.status == OpenAIChatChunkParseStatus.INVALID_PAYLOAD
    assert parsed.error is not None
    assert "index" in parsed.error


def test_bare_done_is_not_an_sse_data_event():
    assert parse_openai_chat_sse_event(SSEEvent.from_raw("[DONE]")).status == (
        OpenAIChatChunkParseStatus.NO_DATA
    )


def test_shared_choices_keep_indexes_and_non_text_entries():
    wire = build_sse_data_chunk(
        {
            "choices": [
                {"index": 1, "delta": {"role": "assistant"}},
                {"index": 0, "delta": {"content": "text"}, "finish_reason": "stop"},
            ]
        }
    ).encode()
    (event,) = SSEFramer().feed(wire)
    parsed = parse_openai_chat_sse_event(event)
    assert parsed.status == OpenAIChatChunkParseStatus.PARSED
    assert [
        (choice.choice_index, choice.content) for choice in parsed.parsed_choices
    ] == [
        (1, None),
        (0, "text"),
    ]
    assert parsed.parsed_choices[1].finish_reason == "stop"


@pytest.mark.parametrize(
    "raw, status",
    [
        (": keepalive", OpenAIChatChunkParseStatus.NO_DATA),
        ("data: [DONE]", OpenAIChatChunkParseStatus.DONE),
        ("data: invalid", OpenAIChatChunkParseStatus.MALFORMED_JSON),
        ("data: []", OpenAIChatChunkParseStatus.INVALID_PAYLOAD),
    ],
)
def test_shared_parser_exposes_status_instead_of_local_errors(raw, status):
    assert parse_openai_chat_sse_event(SSEEvent.from_raw(raw)).status == status
