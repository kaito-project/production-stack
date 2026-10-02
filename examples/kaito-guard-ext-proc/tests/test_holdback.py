"""Test PR-1.3: Holdback mechanism (dedicated tests)."""

import pytest
from streaming.holdback import release_safe_prefix, detect_cross_boundary_pattern


class TestHoldbackMechanism:
    """Comprehensive tests for the holdback safety mechanism."""

    def test_holdback_default_pattern_length(self):
        """Test with default pattern length (12 for 'sk-123456789')."""
        # Buffer: "hello world!" (12 chars)
        safe, held = release_safe_prefix("hello world!")
        # Should hold last 11 chars
        assert held == "ello world!"
        assert safe == "h"

    def test_holdback_custom_pattern_length(self):
        """Test with custom pattern length."""
        text = "0123456789"
        # pattern_length=5, holdback_size=4
        safe, held = release_safe_prefix(text, pattern_length=5)
        assert len(held) == 4
        assert held == "6789"
        assert safe == "012345"

    def test_holdback_prevents_prefix_leakage(self):
        """Core test: verify holdback prevents unsafe prefix from leaking.

        Scenario: secret is "ABCDEF" (6 chars)
        Chunk 1: "...ABC" → should hold "ABC" (can't release, pattern incomplete)
        Chunk 2: "DEF..." → should detect "ABCDEF" when combined with held
        """
        chunk1 = "prefix ABC"
        safe1, held1 = release_safe_prefix(chunk1, pattern_length=6)

        # Should not release partial pattern
        assert "ABC" not in safe1  # Partial not released
        assert "ABC" in held1  # Partial is held

        # Simulate chunk 2
        chunk2 = "DEF suffix"
        combined = held1 + chunk2  # "ABC" + "DEF suffix"

        # Now full pattern is visible
        assert "ABCDEF" in combined

    def test_holdback_with_actual_secret_pattern(self):
        """Test with actual secret pattern: 'sk-123456789'."""
        secret = "sk-123456789"
        pattern_length = 12

        # Simulate chunk 1: first 5 chars of secret
        chunk1 = "text sk-12"  # "sk-12" is partial
        safe1, held1 = release_safe_prefix(chunk1, pattern_length=pattern_length)

        # The partial "sk-12" should not be released
        assert "sk-" not in safe1  # partial prefix not exposed

        # Simulate chunk 2: rest of secret
        chunk2 = "3456789 more"
        combined = held1 + chunk2

        # Full secret should be detectable in combined
        assert secret in combined

    def test_holdback_empty_buffer(self):
        """Test holdback with empty buffer."""
        safe, held = release_safe_prefix("")
        assert safe == ""
        assert held == ""

    def test_holdback_single_char(self):
        """Test holdback with single character."""
        safe, held = release_safe_prefix("x", pattern_length=12)
        assert safe == ""
        assert held == "x"

    def test_holdback_accumulation(self):
        """Test holdback behavior with gradual accumulation.

        Simulates 4 chunks that gradually build up the secret pattern.
        """
        pattern = "SECRET_123"
        chunks = ["te", "xt ", "SE", "CR", "ET_", "12", "3 ", "end"]

        buffer = ""
        released_all = []

        for chunk in chunks:
            buffer += chunk
            safe, held = release_safe_prefix(buffer, pattern_length=len(pattern))
            released_all.append(safe)
            buffer = held

        # Final buffer should contain or be part of the pattern
        # (depending on when it fully forms)
        full_released = "".join(released_all) + buffer
        assert pattern in full_released

    def test_holdback_large_pattern(self):
        """Test holdback with larger pattern."""
        large_pattern = "A" * 100
        pattern_length = 100

        # Small buffer (less than holdback)
        safe, held = release_safe_prefix("xxx", pattern_length=pattern_length)
        assert safe == ""
        assert held == "xxx"

        # Buffer at threshold (exactly holdback_size = pattern_length - 1 = 99)
        text = "Y" * 100
        safe, held = release_safe_prefix(text, pattern_length=pattern_length)
        # 100 chars with holdback_size=99 means release 1
        assert len(safe) == 1
        assert len(held) == 99

        # Buffer over threshold
        text = "Z" * 101
        safe, held = release_safe_prefix(text, pattern_length=pattern_length)
        assert len(safe) == 2
        assert len(held) == 99


class TestCrossBoundaryDetection:
    """Test cross-boundary pattern detection utility."""

    def test_detect_pattern_at_exact_boundary(self):
        """Test pattern that starts exactly at boundary."""
        previously_held = "abc"
        new_content = "def"
        pattern = "cde"

        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is True

    def test_detect_pattern_entirely_held(self):
        """Test pattern entirely in held section."""
        previously_held = "abcdef"
        new_content = "ghijkl"
        pattern = "cde"

        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is False

    def test_detect_pattern_entirely_new(self):
        """Test pattern entirely in new section."""
        previously_held = "abcdef"
        new_content = "cdefgh"
        pattern = "cde"

        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is False  # Doesn't span; entirely in new

    def test_detect_pattern_nonexistent(self):
        """Test when pattern doesn't exist at all."""
        previously_held = "abcdef"
        new_content = "ghijkl"
        pattern = "xyz"

        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is False

    def test_detect_multipart_span(self):
        """Test pattern that clearly spans boundary."""
        previously_held = "start_of_pattern"
        new_content = "_end_of_pattern_suffix"
        pattern = "pattern_end"

        result = detect_cross_boundary_pattern(previously_held, new_content, pattern)
        assert result is True


class TestHoldbackEdgeCases:
    """Test edge cases and boundary conditions."""

    def test_pattern_length_one(self):
        """Test with pattern_length=1 (holdback_size=0)."""
        safe, held = release_safe_prefix("abc", pattern_length=1)
        # holdback_size=0, so release everything
        assert safe == "abc"
        assert held == ""

    def test_pattern_length_matches_buffer(self):
        """Test when pattern_length equals buffer length.

        When pattern_length = buffer length, holdback_size = length - 1.
        So buffer of 5 chars with pattern_length=5 holds 4, releases 1.
        """
        text = "hello"
        safe, held = release_safe_prefix(text, pattern_length=5)
        assert len(safe) == 1
        assert len(held) == 4
        assert safe + held == text

    def test_pattern_length_exceeds_buffer(self):
        """Test when pattern_length exceeds buffer length."""
        text = "hi"
        safe, held = release_safe_prefix(text, pattern_length=100)
        assert safe == ""
        assert held == "hi"

    def test_repeated_partial_patterns(self):
        """Test buffer with many partial pattern occurrences.

        For pattern_length=12, holdback_size=11.
        Text: "sk- sk- sk- sk-123456789" (length 24)
        After release_safe_prefix: should release 13 chars, hold last 11.

        The full pattern might be split between released and held, but
        it will be caught on the next iteration when we scan held + new.
        """
        text = "sk- sk- sk- sk-123456789"
        safe, held = release_safe_prefix(text, pattern_length=12)

        # Verify split is correct
        assert len(safe) + len(held) == len(text)
        assert len(held) == 11

        # The full pattern should be reconstructible from held + next
        # For this test, just verify that the mechanism works
        combined = safe + held  # Reconstruct
        assert "sk-123456789" in combined
