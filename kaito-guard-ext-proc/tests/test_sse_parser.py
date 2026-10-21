"""Test PR-1.2: SSE Parser."""

import json

import pytest

from ext_proc.streaming.sse_parser import (
    SSEParseError,
    is_done_event,
    parse_sse_event,
    split_sse_events,
)


class TestSplitSSEEvents:
    """Test SSE event splitting."""

    def test_single_event(self):
        """Test splitting a single complete event."""
        raw = b'data: {"choices":[{"delta":{"content":"hello"}}]}\n\n'
        events, remaining = split_sse_events(raw)

        assert len(events) == 1
        assert remaining == b""
        assert events[0] == b'data: {"choices":[{"delta":{"content":"hello"}}]}'

    def test_multiple_events(self):
        """Test splitting multiple events."""
        raw = (
            b'data: {"choices":[{"delta":{"content":"hello"}}]}\n\n'
            b'data: {"choices":[{"delta":{"content":" world"}}]}\n\n'
        )
        events, remaining = split_sse_events(raw)

        assert len(events) == 2
        assert remaining == b""
        assert events[0] == b'data: {"choices":[{"delta":{"content":"hello"}}]}'
        assert events[1] == b'data: {"choices":[{"delta":{"content":" world"}}]}'

    def test_incomplete_event(self):
        """Test buffer with incomplete event at end."""
        raw = (
            b'data: {"choices":[{"delta":{"content":"hello"}}]}\n\n'
            b'data: {"choices":[{"delta":{"content":"in'
        )
        events, remaining = split_sse_events(raw)

        assert len(events) == 1
        assert remaining == b'data: {"choices":[{"delta":{"content":"in'
        assert events[0] == b'data: {"choices":[{"delta":{"content":"hello"}}]}'

    def test_empty_buffer(self):
        """Test empty input."""
        events, remaining = split_sse_events(b"")
        assert events == []
        assert remaining == b""


class TestParseSSEEvent:
    """Test SSE event parsing."""

    def test_parse_delta_content(self):
        """Test parsing delta.content from OpenAI format."""
        event = b'data: {"choices":[{"delta":{"content":"hello"}}]}'
        parsed = parse_sse_event(event)

        assert parsed is not None
        assert parsed["delta"]["content"] == "hello"

    def test_parse_with_finish_reason(self):
        """Test parsing with finish_reason."""
        event = b'data: {"choices":[{"delta":{"content":"."}, "finish_reason":"stop"}]}'
        parsed = parse_sse_event(event)

        assert parsed is not None
        assert parsed["delta"]["content"] == "."
        assert parsed["finish_reason"] == "stop"

    def test_parse_done_marker(self):
        """Test parsing [DONE] marker."""
        event = b"data: [DONE]"
        parsed = parse_sse_event(event)

        assert parsed is not None
        assert parsed["done"] is True
        assert parsed["delta"]["content"] == ""

    def test_parse_empty_content(self):
        """Test event with no content delta."""
        event = b'data: {"choices":[{"delta":{}}]}'
        parsed = parse_sse_event(event)

        assert parsed is not None
        assert parsed["delta"]["content"] == ""

    def test_parse_empty_event(self):
        """Test empty event line."""
        parsed = parse_sse_event(b"")
        assert parsed is None

    def test_parse_malformed_json(self):
        """Malformed JSON must not be confused with an empty event."""
        event = b"data: {invalid json}"
        with pytest.raises(SSEParseError, match="Invalid JSON"):
            parse_sse_event(event)

    def test_parse_non_data_line(self):
        """Test non-data line (e.g., comment)."""
        event = b": comment"
        parsed = parse_sse_event(event)
        assert parsed is None


class TestIsDoneEvent:
    """Test done event detection."""

    def test_done_with_prefix(self):
        """Test [DONE] with 'data: ' prefix."""
        assert is_done_event(b"data: [DONE]") is True

    def test_done_without_prefix(self):
        """Test [DONE] without prefix."""
        assert is_done_event(b"[DONE]") is True

    def test_not_done(self):
        """Test non-done event."""
        assert is_done_event(b"data: hello") is False

    def test_malformed_not_done(self):
        with pytest.raises(SSEParseError, match="UTF-8"):
            is_done_event(b"\xff\xfe")


class TestIntegration:
    """Integration test: split then parse."""

    def test_streaming_sequence(self):
        """Test a realistic streaming sequence."""
        # Simulate receiving chunks that end mid-event
        chunk1 = b'data: {"choices":[{"delta":{"content":"hello'
        chunk2 = b' world"}}]}\n\ndata: {"choices":[{"delta":{"content":'
        chunk3 = b' "!"}}]}\n\ndata: [DONE]\n\n'

        # Accumulate and split
        buf = b""
        all_contents = []

        for chunk in [chunk1, chunk2, chunk3]:
            buf += chunk
            events, buf = split_sse_events(buf)
            for event in events:
                parsed = parse_sse_event(event)
                if parsed and "content" in parsed.get("delta", {}):
                    # Skip empty content (from [DONE] marker)
                    if parsed["delta"]["content"]:
                        all_contents.append(parsed["delta"]["content"])

        assert all_contents == ["hello world", "!"]
        assert buf == b""


@pytest.mark.parametrize("newline", [b"\n", b"\r\n"])
def test_every_chunk_boundary(newline):
    first = b'data: {"choices":[{"delta":{"content":"hello"}}]}'
    second = b"data:[DONE]"
    raw = first + newline * 2 + second + newline * 2
    for offset in range(len(raw) + 1):
        events, remaining = split_sse_events(raw[:offset])
        following, remaining = split_sse_events(remaining + raw[offset:])
        assert events + following == [first, second], offset
        assert remaining == b"", offset


@pytest.mark.parametrize("ending", [b"\n\n", b"\r\n\r\n", b"\n\r\n", b"\r\n\n", b"\r\r"])
def test_byte_at_a_time_stream(ending):
    raw = b'data: {"choices":[{"delta":{"content":"hi"}}]}' + ending
    raw += b"data:[DONE]" + ending
    remainder = b""
    events = []
    # The next byte disambiguates a trailing CR from an incomplete CRLF.
    for byte in raw + b":":
        complete, remainder = split_sse_events(remainder + bytes([byte]))
        events.extend(complete)
    assert [parse_sse_event(event) for event in events] == [
        {"delta": {"content": "hi"}, "finish_reason": None},
        {"delta": {"content": ""}, "done": True},
    ]
    assert remainder == b":"


@pytest.mark.parametrize("ending", [b"\n", b"\r\n", b"\r"])
def test_single_line_ending_is_not_an_event(ending):
    raw = b"data: {}" + ending
    assert split_sse_events(raw) == ([], raw)


def test_trailing_cr_is_retained_until_next_chunk():
    raw = b"data: [DONE]\r\n\r"
    assert split_sse_events(raw) == ([], raw)
    assert split_sse_events(raw + b"\n") == ([b"data: [DONE]"], b"")


@pytest.mark.parametrize("newline", [b"\n", b"\r\n", b"\r"])
def test_fields_comments_and_multiline_data(newline):
    event = newline.join([
        b": keepalive",
        b"event: message",
        b"id: 12",
        b"retry: 1000",
        b"data: {",
        b'data:"choices": [{"delta": {"content": "hello"}}]',
        b"data: }",
    ])
    assert parse_sse_event(event) == {
        "delta": {"content": "hello"}, "finish_reason": None,
    }


@pytest.mark.parametrize("event", [
    b"[DONE]",
    b"data:[DONE]",
    b"data: [DONE]",
    b": comment\r\nevent: message\r\nid: 1\r\ndata:[DONE]",
])
def test_done_helpers_agree(event):
    assert is_done_event(event) is True
    assert parse_sse_event(event) == {"delta": {"content": ""}, "done": True}


@pytest.mark.parametrize("event", [
    b"",
    b": keepalive",
    b"event: message\nid: 1",
    b"data",
    b"data:",
    b"data: ",
    b'data: {"choices":[],"usage":{"total_tokens":10}}',
])
def test_valid_events_without_text(event):
    assert parse_sse_event(event) is None
    assert is_done_event(event) is False


@pytest.mark.parametrize("delta", [
    {},
    {"role": "assistant"},
    {"content": None},
    {"content": ""},
    {"tool_calls": []},
])
def test_non_text_delta_is_not_an_error(delta):
    event = b"data: " + json.dumps({"choices": [{"delta": delta}]}).encode()
    assert parse_sse_event(event) == {
        "delta": {"content": ""}, "finish_reason": None,
    }


@pytest.mark.parametrize("payload, message", [
    (None, "payload must be an object"),
    ([], "payload must be an object"),
    ("text", "payload must be an object"),
    ({}, "choices must be a list"),
    ({"error": {"message": "upstream failed"}}, "choices must be a list"),
    ({"choices": None}, "choices must be a list"),
    ({"choices": {}}, "choices must be a list"),
    ({"choices": "text"}, "choices must be a list"),
    ({"choices": [None]}, r"choices\[0\] must be an object"),
    ({"choices": [1]}, r"choices\[0\] must be an object"),
    ({"choices": [{}]}, "delta must be an object"),
    ({"choices": [{"delta": None}]}, "delta must be an object"),
    ({"choices": [{"delta": []}]}, "delta must be an object"),
    ({"choices": [{"delta": "text"}]}, "delta must be an object"),
    ({"choices": [{"delta": {"content": 123}}]}, "content must be a string"),
    ({"choices": [{"delta": {"content": False}}]}, "content must be a string"),
    ({"choices": [{"delta": {"content": []}}]}, "content must be a string"),
    ({"choices": [{"delta": {"content": {}}}]}, "content must be a string"),
    ({"choices": [{"delta": {}, "finish_reason": 123}]}, "finish_reason"),
])
def test_invalid_payload_raises_explicit_error(payload, message):
    event = b"data: " + json.dumps(payload).encode()
    with pytest.raises(SSEParseError, match=message):
        parse_sse_event(event)


@pytest.mark.parametrize("helper", [parse_sse_event, is_done_event])
def test_invalid_utf8_raises_explicit_error(helper):
    with pytest.raises(SSEParseError, match="UTF-8"):
        helper(b"data: \xff")


def test_text_and_finish_only_chunks():
    for delta, reason in [({"content": "hello"}, None), ({}, "stop")]:
        event = b"data: " + json.dumps({
            "choices": [{"delta": delta, "finish_reason": reason}],
        }).encode()
        parsed = parse_sse_event(event)
        assert parsed == {
            "delta": {"content": delta.get("content", "")},
            "finish_reason": reason,
        }


def test_utf8_character_split_across_chunks():
    content = "\u4f60\u597d"
    raw = ('data: {"choices":[{"delta":{"content":"' + content + '"}}]}\r\n\r\n').encode()
    offset = raw.index(content.encode()) + 1
    events, remaining = split_sse_events(raw[:offset])
    assert events == []
    events, remaining = split_sse_events(remaining + raw[offset:])
    assert remaining == b""
    assert parse_sse_event(events[0]) == {
        "delta": {"content": content}, "finish_reason": None,
    }
