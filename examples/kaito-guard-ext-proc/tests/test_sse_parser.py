"""Test PR-1.2: SSE Parser."""

import pytest
from streaming.sse_parser import split_sse_events, parse_sse_event, is_done_event


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
        """Test handling malformed JSON."""
        event = b"data: {invalid json}"
        parsed = parse_sse_event(event)
        assert parsed is None

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
        """Test malformed bytes."""
        assert is_done_event(b"\xff\xfe") is False


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
