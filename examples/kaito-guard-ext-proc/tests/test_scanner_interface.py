"""Test PR-1.4: Scanner Interface + Registry."""

import pytest
from scanners.interface import Scanner, ScanResult
from scanners.registry import ScannerRegistry


class MockScanner(Scanner):
    """Mock scanner for testing."""

    def __init__(self, name: str, pattern: str, should_find: bool = False):
        """Initialize mock scanner.

        Args:
            name: Scanner name.
            pattern: Pattern string.
            should_find: Whether this scanner should report finding the pattern.
        """
        self._name = name
        self._pattern = pattern
        self._should_find = should_find

    @property
    def name(self) -> str:
        """Return scanner name."""
        return self._name

    @property
    def pattern(self) -> str:
        """Return pattern."""
        return self._pattern

    def scan(self, content: str) -> ScanResult:
        """Mock scan implementation."""
        if self._should_find:
            # Find the pattern in content
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
        # Always report not found
        return ScanResult(
            scanner_name=self._name,
            pattern=self._pattern,
            found=False,
        )


class TestScanResult:
    """Test ScanResult dataclass."""

    def test_found_result_valid(self):
        """Test creating a valid found result."""
        result = ScanResult(
            scanner_name="test", pattern="secret", found=True, match_index=5, match_text="secret"
        )
        assert result.found is True
        assert result.match_index == 5
        assert result.match_text == "secret"

    def test_not_found_result_valid(self):
        """Test creating a valid not-found result."""
        result = ScanResult(
            scanner_name="test", pattern="secret", found=False
        )
        assert result.found is False
        assert result.match_index == -1
        assert result.match_text == ""

    def test_found_without_index_invalid(self):
        """Test that found=True requires match_index >= 0."""
        with pytest.raises(ValueError):
            ScanResult(
                scanner_name="test",
                pattern="secret",
                found=True,
                match_index=-1,
                match_text="secret",
            )

    def test_found_without_text_invalid(self):
        """Test that found=True requires non-empty match_text."""
        with pytest.raises(ValueError):
            ScanResult(
                scanner_name="test",
                pattern="secret",
                found=True,
                match_index=0,
                match_text="",
            )

    def test_not_found_with_index_invalid(self):
        """Test that found=False requires match_index = -1."""
        with pytest.raises(ValueError):
            ScanResult(
                scanner_name="test", pattern="secret", found=False, match_index=5
            )

    def test_not_found_with_text_invalid(self):
        """Test that found=False requires empty match_text."""
        with pytest.raises(ValueError):
            ScanResult(
                scanner_name="test",
                pattern="secret",
                found=False,
                match_text="secret",
            )


class TestScannerRegistry:
    """Test ScannerRegistry functionality."""

    def test_register_single_scanner(self):
        """Test registering a single scanner."""
        registry = ScannerRegistry()
        scanner = MockScanner("test_scanner", "pattern1")
        registry.register(scanner)
        assert len(registry) == 1
        assert registry.get("test_scanner") is scanner

    def test_register_multiple_scanners(self):
        """Test registering multiple scanners."""
        registry = ScannerRegistry()
        scanner1 = MockScanner("scanner1", "pattern1")
        scanner2 = MockScanner("scanner2", "pattern2")
        registry.register(scanner1)
        registry.register(scanner2)
        assert len(registry) == 2
        assert registry.list_scanners() == ["scanner1", "scanner2"]

    def test_register_duplicate_name_fails(self):
        """Test that duplicate scanner names are rejected."""
        registry = ScannerRegistry()
        scanner1 = MockScanner("test_scanner", "pattern1")
        scanner2 = MockScanner("test_scanner", "pattern2")
        registry.register(scanner1)
        with pytest.raises(ValueError, match="already registered"):
            registry.register(scanner2)

    def test_unregister_scanner(self):
        """Test unregistering a scanner."""
        registry = ScannerRegistry()
        scanner = MockScanner("test_scanner", "pattern1")
        registry.register(scanner)
        assert len(registry) == 1
        registry.unregister("test_scanner")
        assert len(registry) == 0
        assert registry.get("test_scanner") is None

    def test_unregister_nonexistent_fails(self):
        """Test that unregistering nonexistent scanner fails."""
        registry = ScannerRegistry()
        with pytest.raises(ValueError, match="not found"):
            registry.unregister("nonexistent")

    def test_get_existing_scanner(self):
        """Test getting an existing scanner."""
        registry = ScannerRegistry()
        scanner = MockScanner("test_scanner", "pattern1")
        registry.register(scanner)
        retrieved = registry.get("test_scanner")
        assert retrieved is scanner

    def test_get_nonexistent_scanner(self):
        """Test getting a nonexistent scanner returns None."""
        registry = ScannerRegistry()
        assert registry.get("nonexistent") is None

    def test_list_scanners_empty(self):
        """Test listing scanners on empty registry."""
        registry = ScannerRegistry()
        assert registry.list_scanners() == []

    def test_list_scanners_multiple(self):
        """Test listing multiple scanners."""
        registry = ScannerRegistry()
        registry.register(MockScanner("scanner1", "pattern1"))
        registry.register(MockScanner("scanner2", "pattern2"))
        registry.register(MockScanner("scanner3", "pattern3"))
        names = registry.list_scanners()
        assert names == ["scanner1", "scanner2", "scanner3"]

    def test_scan_all_no_matches(self):
        """Test scanning with all scanners when none match."""
        registry = ScannerRegistry()
        registry.register(MockScanner("s1", "p1", should_find=False))
        registry.register(MockScanner("s2", "p2", should_find=False))
        results = registry.scan("some content")
        assert len(results) == 2
        assert all(r.found is False for r in results)

    def test_scan_all_with_matches(self):
        """Test scanning with all scanners when some match."""
        registry = ScannerRegistry()
        registry.register(MockScanner("s1", "secret", should_find=True))
        registry.register(MockScanner("s2", "pattern", should_find=True))
        content = "my secret text pattern here"
        results = registry.scan(content)
        assert len(results) == 2
        assert results[0].found is True
        assert results[0].match_index == 3  # "secret" starts at index 3
        assert results[1].found is True
        assert results[1].match_index == 15  # "pattern" starts at index 15

    def test_scan_specific_scanner(self):
        """Test scanning with a specific scanner."""
        registry = ScannerRegistry()
        registry.register(MockScanner("s1", "p1", should_find=False))
        registry.register(MockScanner("s2", "p2", should_find=True))
        results = registry.scan("content with p2", scanner_name="s2")
        assert len(results) == 1
        assert results[0].scanner_name == "s2"
        assert results[0].found is True

    def test_scan_nonexistent_scanner_fails(self):
        """Test scanning with nonexistent scanner name fails."""
        registry = ScannerRegistry()
        registry.register(MockScanner("s1", "p1"))
        with pytest.raises(ValueError, match="not found"):
            registry.scan("content", scanner_name="nonexistent")

    def test_scan_empty_registry(self):
        """Test scanning with empty registry."""
        registry = ScannerRegistry()
        results = registry.scan("content")
        assert results == []

    def test_registry_repr(self):
        """Test string representation of registry."""
        registry = ScannerRegistry()
        assert "(empty)" in repr(registry)
        registry.register(MockScanner("s1", "p1"))
        assert "s1" in repr(registry)


class TestIntegration:
    """Integration tests for Scanner + Registry."""

    def test_realistic_security_scanning(self):
        """Test realistic multi-scanner security scanning scenario."""
        registry = ScannerRegistry()

        # Register multiple security scanners
        registry.register(MockScanner("api_key_detector", "sk-", should_find=True))
        registry.register(MockScanner("password_detector", "password", should_find=True))
        registry.register(MockScanner("token_detector", "token", should_find=False))

        # Scan content with multiple secrets
        content = "API key: sk-123456789 and password: secret123"
        results = registry.scan(content)

        # Verify results
        assert len(results) == 3
        api_key_result = next(r for r in results if r.scanner_name == "api_key_detector")
        password_result = next(r for r in results if r.scanner_name == "password_detector")
        token_result = next(r for r in results if r.scanner_name == "token_detector")

        assert api_key_result.found is True
        assert password_result.found is True
        assert token_result.found is False

    def test_dynamic_scanner_registration(self):
        """Test adding/removing scanners dynamically."""
        registry = ScannerRegistry()

        # Start with one scanner
        s1 = MockScanner("s1", "p1")
        registry.register(s1)
        assert len(registry) == 1

        # Add more scanners
        s2 = MockScanner("s2", "p2")
        s3 = MockScanner("s3", "p3")
        registry.register(s2)
        registry.register(s3)
        assert len(registry) == 3

        # Remove one scanner
        registry.unregister("s2")
        assert len(registry) == 2
        assert registry.get("s2") is None
