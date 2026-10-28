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
from unittest.mock import Mock, call

import pytest
from guardrail_core.streaming.openai import (
    build_openai_chat_delta_sse_chunk,
    build_sse_data_chunk,
    build_sse_done_chunk,
)
from guardrail_core.streaming.sse import (
    SSEDecodeError,
    SSEEventTooLargeError,
    SSEIncompleteEventError,
)
from guardrail_core.streaming.window import (
    WindowEmitResult,
    WindowScanError,
    WindowScanner,
    WindowScanResult,
)

from ext_proc.logical_stream import LogicalStream, LogicalStreamError


def content_event(text):
    return build_openai_chat_delta_sse_chunk(text).encode()


DONE = build_sse_done_chunk().encode()


@pytest.fixture
def scanner():
    scanner = Mock(spec=WindowScanner)
    scanner.scan.return_value = WindowScanResult()
    return scanner


def assert_closed(stream, scanner):
    assert stream.closed
    calls = scanner.scan.call_count
    with pytest.raises(RuntimeError, match="closed"):
        stream.feed(content_event("more"))
    with pytest.raises(RuntimeError, match="closed"):
        stream.finish()
    stream.cancel()
    assert scanner.scan.call_count == calls


@pytest.mark.parametrize("ending", ["\n", "\r\n", "\r"])
def test_stream_at_every_byte_boundary(scanner, ending):
    text = "hello \u4f60\u597d"
    wire = (
        ("\ufeff" + build_openai_chat_delta_sse_chunk(text) + build_sse_done_chunk())
        .replace("\n", ending)
        .encode()
    )
    for offset in range(len(wire) + 1):
        stream = LogicalStream(scanner, holdback_len=3)
        chunks = []
        for part in (wire[:offset], wire[offset:]):
            chunks.extend(stream.feed(part).chunks)
        chunks.extend(stream.finish().chunks)
        assert "".join(chunks) == text, offset
        assert_closed(stream, scanner)


def test_done_waits_for_normal_eof_before_flushing(scanner):
    stream = LogicalStream(scanner, holdback_len=5)
    assert stream.feed(content_event("hello")) == WindowEmitResult(chunks=())
    assert stream.feed(DONE) == WindowEmitResult(chunks=())
    assert not stream.closed
    scanner.scan.assert_not_called()
    assert stream.finish() == WindowEmitResult(chunks=("hello",))
    scanner.scan.assert_called_once_with("hello", flush=True, preceding_char="")
    assert_closed(stream, scanner)


def test_context_and_sanitized_output_use_shared_window(scanner):
    scanner.scan.side_effect = lambda text, **_: WindowScanResult(
        sanitized_text=text.replace("secret", "[removed]")
    )
    stream = LogicalStream(scanner, holdback_len=5)
    chunks = []
    for text in ("safe se", "cret end"):
        chunks.extend(stream.feed(content_event(text)).chunks)
    stream.feed(DONE)
    chunks.extend(stream.finish().chunks)
    assert "".join(chunks) == "safe [removed] end"
    assert scanner.scan.call_args_list == [
        call("safe se", flush=False, preceding_char=""),
        call("fe secret end", flush=False, preceding_char="a"),
        call("] end", flush=True, preceding_char="d"),
    ]


@pytest.mark.parametrize("split", range(1, len("secret")))
def test_cross_event_match_blocks_and_stops_same_batch(scanner, split):
    scanner.scan.side_effect = lambda text, **_: WindowScanResult(
        blocked="secret" in text
    )
    stream = LogicalStream(scanner, holdback_len=5)
    first = stream.feed(content_event("safe " + "secret"[:split]))
    assert "safe ".startswith("".join(first.chunks))
    result = stream.feed(
        content_event("secret"[split:]) + content_event("ignored") + DONE
    )
    assert result == WindowEmitResult(chunks=(), blocked=True)
    assert stream.blocked
    assert all(not c.kwargs["flush"] for c in scanner.scan.call_args_list)
    assert_closed(stream, scanner)


def test_block_on_final_scan_discards_tail(scanner):
    scanner.scan.return_value = WindowScanResult(blocked=True)
    stream = LogicalStream(scanner, holdback_len=10)
    stream.feed(content_event("secret") + DONE)
    assert stream.finish() == WindowEmitResult(chunks=(), blocked=True)
    scanner.scan.assert_called_once_with("secret", flush=True, preceding_char="")
    assert_closed(stream, scanner)


@pytest.mark.parametrize(
    "payload",
    [
        {"choices": [{"index": 1, "delta": {"content": "wrong"}}]},
        {"choices": [{"index": 0}, {"index": 1}]},
        {"choices": [{"index": 0}, {"index": 0}]},
        {"choices": [{"delta": {"content": "missing index"}}]},
        {"choices": [{"index": True}]},
        {"choices": [{"index": 0, "delta": {"content": 1}}]},
        {"choices": "invalid"},
    ],
)
def test_invalid_choice_or_payload_discards_pending(scanner, payload):
    stream = LogicalStream(scanner, holdback_len=100)
    stream.feed(content_event("pending"))
    with pytest.raises(LogicalStreamError):
        stream.feed(build_sse_data_chunk(payload).encode())
    scanner.scan.assert_not_called()
    assert_closed(stream, scanner)


def test_empty_choices_and_non_text_choices_are_metadata(scanner):
    stream = LogicalStream(scanner, holdback_len=100)
    wire = b": heartbeat\n\n"
    for payload in [
        {"choices": [{"index": 0, "delta": {"role": "assistant"}}]},
        {"choices": [{"index": 0, "delta": {"content": None}}]},
        {"choices": [], "usage": {"total_tokens": 1}},
        {"choices": [{"index": 0, "finish_reason": "stop"}]},
    ]:
        wire += build_sse_data_chunk(payload).encode()
    assert stream.feed(wire + DONE) == WindowEmitResult(chunks=())
    assert stream.finish() == WindowEmitResult(chunks=())
    scanner.scan.assert_not_called()


@pytest.mark.parametrize("invalid", [b"data: {bad}\n\n", b"data: []\n\n"])
def test_parse_error_discards_even_same_batch_output(scanner, invalid):
    stream = LogicalStream(scanner, holdback_len=0)
    with pytest.raises(LogicalStreamError):
        stream.feed(content_event("approved") + invalid + DONE)
    assert_closed(stream, scanner)


@pytest.mark.parametrize("after_done", [DONE, content_event("late")])
def test_data_after_done_is_terminal(scanner, after_done):
    stream = LogicalStream(scanner, holdback_len=100)
    stream.feed(content_event("pending") + DONE)
    with pytest.raises(LogicalStreamError, match="after"):
        stream.feed(after_done)
    scanner.scan.assert_not_called()
    assert_closed(stream, scanner)


def test_missing_done_discards_pending(scanner):
    stream = LogicalStream(scanner, holdback_len=100)
    stream.feed(content_event("pending"))
    with pytest.raises(LogicalStreamError, match="without a DONE"):
        stream.finish()
    scanner.scan.assert_not_called()
    assert_closed(stream, scanner)


@pytest.mark.parametrize("done", [b"", DONE])
def test_truncated_eof_is_not_normal_completion(scanner, done):
    stream = LogicalStream(scanner, holdback_len=100)
    stream.feed(content_event("pending") + done + b"data: unfinished")
    with pytest.raises(SSEIncompleteEventError):
        stream.finish()
    scanner.scan.assert_not_called()
    assert_closed(stream, scanner)


@pytest.mark.parametrize(
    "wire,error",
    [
        (b"data: \xff\n\n", SSEDecodeError),
        (b"data: " + b"x" * 100, SSEEventTooLargeError),
    ],
)
def test_framing_failure_discards_pending(scanner, wire, error):
    stream = LogicalStream(scanner, holdback_len=100, max_event_bytes=100)
    stream.feed(content_event("pending"))
    with pytest.raises(error):
        stream.feed(wire)
    scanner.scan.assert_not_called()
    assert_closed(stream, scanner)


@pytest.mark.parametrize("final", [False, True])
@pytest.mark.parametrize("cancelled", [False, True])
def test_scanner_failure_or_cancellation_is_terminal(scanner, final, cancelled):
    cause = asyncio.CancelledError() if cancelled else RuntimeError("scanner failed")
    scanner.scan.side_effect = cause
    stream = LogicalStream(scanner, holdback_len=100 if final else 0)
    if final:
        stream.feed(content_event("pending") + DONE)
    with pytest.raises(asyncio.CancelledError if cancelled else WindowScanError) as exc:
        stream.finish() if final else stream.feed(content_event("pending"))
    if not cancelled:
        assert exc.value.__cause__ is cause
    assert_closed(stream, scanner)


def test_caller_cancellation_discards_pending(scanner):
    stream = LogicalStream(scanner, holdback_len=100)
    stream.feed(content_event("pending") + DONE)
    stream.cancel()
    scanner.scan.assert_not_called()
    assert_closed(stream, scanner)


def test_request_state_is_isolated(scanner):
    scanner.scan.side_effect = lambda text, **_: WindowScanResult(
        blocked="secret" in text
    )
    first = LogicalStream(scanner, holdback_len=5)
    second = LogicalStream(scanner, holdback_len=5)
    first.feed(content_event("se"))
    second.feed(content_event("cret") + DONE)
    assert second.finish().chunks == ("cret",)
    first.feed(content_event("cret!"))
    assert first.blocked
    assert_closed(first, scanner)
    assert_closed(second, scanner)
