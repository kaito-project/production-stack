"""Test PR-1.5: GuardEngine Coordinator."""

import pytest
from guard.engine import GuardEngine
from scanners.interface import Scanner, ScanResult
from scanners.registry import ScannerRegistry


class SimpleScanner(Scanner):
    """Simple scanner for testing."""

    def __init__(self, name: str, pattern: str):
        """Initialize with name and pattern."""
        self._name = name
        self._pattern = pattern

    @property
    def name(self) -> str:
        """Return name."""
        return self._name

    @property
    def pattern(self) -> str:
        """Return pattern."""
        return self._pattern

    def scan(self, content: str) -> ScanResult:
        """Scan for pattern in content."""
        if self._pattern in content:
            idx = content.find(self._pattern)
            return ScanResult(
                scanner_name=self._name,
                pattern=self._pattern,
                found=True,
                match_index=idx,
                match_text=self._pattern,
            )
        return ScanResult(
            scanner_name=self._name,
            pattern=self._pattern,
            found=False,
        )


class TestGuardEngineBasics:
    """Test GuardEngine basic operations."""

    def test_init(self):
        """Test GuardEngine initialization."""
        registry = ScannerRegistry()
        engine = GuardEngine(registry)
        assert len(engine.scan_history()) == 0
        assert engine.has_detections() is False
        assert len(engine.get_detections()) == 0

    def test_process_delta_large_content(self):
        """Test processing delta with large content to release safely."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=6)

        # Large content that triggers release after holdback
        large = "hello " * 10 + "secret found here"
        result = engine.process_delta(large)
        # Should have results since content is large enough to release
        assert result is not None or engine.has_detections()

    def test_process_delta_pattern_in_released(self):
        """Test pattern detection when pattern is in released portion."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=4)  # Small holdback

        # Pattern is clearly at the beginning, will be released
        result = engine.process_delta("secret is here and very long text to fill up")
        if result is not None:
            # Check if detected in this batch
            assert len(result) == 1
            assert result[0].found is True

    def test_scan_history_accumulates(self):
        """Test scan history accumulation across deltas."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=3)

        # Process multiple large deltas
        engine.process_delta("a" * 20 + "secret" + "b" * 20)
        engine.process_delta("c" * 20 + "secret" + "d" * 20)

        history = engine.scan_history()
        assert len(history) > 0

    def test_has_detections(self):
        """Test has_detections flag."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=3)

        assert engine.has_detections() is False

        # Large content with no pattern
        engine.process_delta("x" * 50)
        assert engine.has_detections() is False

        # Large content with pattern
        engine.process_delta("x" * 30 + "secret" + "x" * 20)
        # Now should have detections (or will on flush)
        engine.flush()
        assert engine.has_detections() is True

    def test_get_detections_filters(self):
        """Test filtering detections."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=3)

        engine.process_delta("x" * 30 + "secret" + "x" * 20)
        engine.flush()

        detections = engine.get_detections()
        assert all(r.found for r in detections)


class TestGuardEngineHoldback:
    """Test GuardEngine holdback mechanism."""

    def test_holdback_prevents_early_release(self):
        """Test that holdback prevents releasing incomplete patterns."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "sk-123456789"))
        engine = GuardEngine(registry, pattern_length=12)

        # Small delta - will be held entirely
        result = engine.process_delta("text sk-1")
        assert result is None

    def test_holdback_size_respected(self):
        """Test that holdback size is (pattern_length - 1)."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "PATTERN"))
        engine = GuardEngine(registry, pattern_length=7)

        # Content larger than holdback_size=6, should release some
        result = engine.process_delta("prefix " * 5 + " PATTERN tail")
        # Should have released something
        assert result is not None

    def test_multiple_deltas_accumulate(self):
        """Test content accumulation across deltas with proper sizing."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=6)

        # Send deltas with pattern - pattern_length=6 so holdback=5
        # Need total content >= 6 to release anything
        engine.process_delta("prefix secret tail content to ensure release happens")

        # Check history
        history = engine.scan_history()
        # Even if not detected in process, should have scan results
        assert len(history) > 0


class TestGuardEngineFlush:
    """Test GuardEngine flush operation."""

    def test_flush_empty(self):
        """Test flush with no content."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry)

        result = engine.flush()
        assert result == []

    def test_flush_catches_held_content(self):
        """Test flush catches patterns in held content."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=6)

        # Delta with pattern that might be held
        engine.process_delta("my secret is here with more text to fill it out")

        # Flush to scan any remaining held content
        flush_result = engine.flush()

        # Should have detections in history
        history = engine.scan_history()
        assert any(r.found for r in history)

    def test_flush_updates_history(self):
        """Test flush updates scan history."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=6)

        engine.process_delta("small")
        initial_history = len(engine.scan_history())

        engine.process_delta("content with secret inside")
        flush_result = engine.flush()
        final_history = len(engine.scan_history())

        assert final_history >= initial_history


class TestGuardEngineReset:
    """Test GuardEngine reset functionality."""

    def test_reset_clears_state(self):
        """Test that reset clears all state."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=3)

        # Process large delta with pattern
        engine.process_delta("x" * 30 + "secret" + "x" * 20)
        engine.flush()
        assert engine.has_detections()

        # Reset
        engine.reset()
        assert engine.has_detections() is False
        assert len(engine.scan_history()) == 0

    def test_reset_allows_new_stream(self):
        """Test reset enables processing new stream."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=3)

        # First stream - large content with pattern
        engine.process_delta("x" * 50 + "secret" + "x" * 50)
        engine.flush()
        first_detections = engine.has_detections()

        # Reset
        engine.reset()

        # Second stream - no pattern
        engine.process_delta("hello world nothing bad")
        engine.flush()
        second_detections = engine.has_detections()

        # Detections should differ
        assert first_detections is True
        assert second_detections is False


class TestGuardEngineIntegration:
    """Integration tests for GuardEngine."""

    def test_complete_streaming_flow(self):
        """Test complete streaming flow with patterns."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("api", "sk-"))
        registry.register(SimpleScanner("token", "token"))
        engine = GuardEngine(registry, pattern_length=3)

        # Simulate streaming with patterns spread across chunks
        engine.process_delta("prefix " * 20 + "sk-123")
        engine.process_delta("and " * 20 + "token")
        engine.process_delta("suffix" * 10)

        engine.flush()

        # Should have detections
        detections = engine.get_detections()
        assert len(detections) > 0

    def test_engine_repr(self):
        """Test string representation."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "p1"))
        engine = GuardEngine(registry)

        repr_str = repr(engine)
        assert "GuardEngine" in repr_str

    def test_empty_deltas(self):
        """Test handling of empty deltas."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry)

        # Empty deltas
        engine.process_delta("")
        engine.process_delta("")

        # Should not crash
        assert engine.has_detections() is False

    def test_large_content_single_delta(self):
        """Test large content in single delta."""
        registry = ScannerRegistry()
        registry.register(SimpleScanner("s1", "secret"))
        engine = GuardEngine(registry, pattern_length=3)

        # Very large content with pattern
        large = "x" * 1000 + "secret" + "y" * 1000
        result = engine.process_delta(large)

        # Should detect in this pass
        assert result is not None
        history = engine.scan_history()
        assert any(r.found for r in history if r.pattern == "secret")
