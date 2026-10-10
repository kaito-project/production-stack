"""Test PR-1.3: Logical Stream + Holdback."""

import pytest
from streaming.logical_stream import LogicalStreamBuffer
from streaming.holdback import release_safe_prefix, detect_cross_boundary_pattern


class TestLogicalStreamBuffer:
    """Test logical stream buffer accumulation."""

    def test_append_single(self):
        """Test appending a single string."""
        buf = LogicalStreamBuffer()
        buf.append("hello")
        assert buf.get() == "hello"
        assert len(buf) == 5

    def test_append_multiple(self):
        """Test appending multiple strings."""
        buf = LogicalStreamBuffer()
        buf.append("hello")
        buf.append(" ")
        buf.append("world")
        assert buf.get() == "hello world"
        assert len(buf) == 11

    def test_append_empty(self):
        """Test that empty appends don't change buffer."""
        buf = LogicalStreamBuffer()
        buf.append("hello")
        buf.append("")
        buf.append("world")
        assert buf.get() == "helloworld"

    def test_clear(self):
        """Test clearing the buffer."""
        buf = LogicalStreamBuffer()
        buf.append("hello")
        buf.clear()
        assert buf.get() == ""
        assert len(buf) == 0

    def test_flush_and_clear(self):
        """Test flush_and_clear returns content and clears."""
        buf = LogicalStreamBuffer()
        buf.append("hello")
        result = buf.flush_and_clear()
        assert result == "hello"
        assert buf.get() == ""


class TestReleaseSafePrefix:
    """Test holdback mechanism (safe prefix release)."""

    def test_small_buffer_holds_all(self):
        """Test that small buffers are held entirely."""
        safe, held = release_safe_prefix("sk-12", pattern_length=12)
        assert safe == ""
        assert held == "sk-12"

    def test_exact_holdback_size(self):
        """Test buffer exactly at holdback size."""
        safe, held = release_safe_prefix("x" * 11, pattern_length=12)
        assert safe == ""
        assert held == "x" * 11

    def test_one_more_than_holdback(self):
        """Test buffer one char more than holdback size."""
        safe, held = release_safe_prefix("x" * 12, pattern_length=12)
        assert safe == "x"
        assert held == "x" * 11

    def test_large_buffer(self):
        """Test large buffer releases most of it."""
        text = "Here is a secret: sk-12"
        safe, held = release_safe_prefix(text, pattern_length=12)
        assert len(safe) + len(held) == len(text)
        assert len(held) == 11
        assert safe == text[:-11]
        assert held == text[-11:]

    def test_cross_event_scenario(self):
        """Test cross-event scenario: pattern split across boundaries."""
        # Event 1: "Here is sk-12"
        buf1 = "Here is sk-12"
        safe1, held1 = release_safe_prefix(buf1, pattern_length=12)
        assert len(held1) == 11
        # The held part should contain "sk-" to prevent leakage
        assert "sk-" in held1

        # Event 2: "3456789 end"
        new_content = "3456789 end"
        combined = held1 + new_content
        assert "sk-123456789" in combined


class TestDetectCrossBoundaryPattern:
    """Test cross-boundary pattern detection utility."""

    def test_pattern_within_held(self):
        """Test pattern entirely in previously held text."""
        previously_held = "secret sk-123456789 here"
        new_content = " more text"
        pattern = "sk-123456789"
        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is False

    def test_pattern_within_new(self):
        """Test pattern entirely in new content."""
        previously_held = "some text"
        new_content = " secret sk-123456789 here"
        pattern = "sk-123456789"
        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is False

    def test_pattern_spans_boundary(self):
        """Test pattern that spans the boundary."""
        previously_held = "text with sk-12"
        new_content = "3456789 more"
        pattern = "sk-123456789"
        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is True

    def test_no_pattern(self):
        """Test when pattern doesn't exist."""
        previously_held = "some text"
        new_content = " more text"
        pattern = "sk-123456789"
        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is False

    def test_pattern_at_exact_boundary(self):
        """Test pattern starting exactly at boundary."""
        previously_held = "abc"
        new_content = "def"
        pattern = "cde"
        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is True


class TestIntegration:
    """Integration tests: logical stream + holdback together."""

    def test_streaming_with_holdback(self):
        """Test realistic streaming scenario."""
        buf = LogicalStreamBuffer()

        # Event 1
        buf.append("Here is a")
        safe1, held1 = release_safe_prefix(buf.get(), pattern_length=12)
        buf.clear()
        buf.append(held1)

        # Event 2
        buf.append(" secret sk-12")
        safe2, held2 = release_safe_prefix(buf.get(), pattern_length=12)
        assert len(held2) == 11
        buf.clear()
        buf.append(held2)

        # Event 3
        buf.append("3456789 end")
        combined = buf.get()
        assert "sk-123456789" in combined

    def test_multiple_chunks(self):
        """Test buffer receiving pattern parts across chunks."""
        buf = LogicalStreamBuffer()

        # Chunk 1
        buf.append("prefix sk-1")
        safe1, held1 = release_safe_prefix(buf.get(), pattern_length=12)
        buf.clear()
        buf.append(held1)

        # Chunk 2
        buf.append("23456789 suffix")
        combined = buf.get()
        assert "sk-123456789" in combined
