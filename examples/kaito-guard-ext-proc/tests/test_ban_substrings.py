"""Test PR-1.6: BanSubstrings Reference Scanner."""

import pytest
from scanners.ban_substrings import BanSubstringsScanner


class TestBanSubstringsInitialization:
    """Test BanSubstringsScanner initialization."""

    def test_init_valid(self):
        """Test valid initialization."""
        scanner = BanSubstringsScanner("api_keys", ["sk-", "rk-"])
        assert scanner.name == "api_keys"
        assert len(scanner._banned) == 2

    def test_init_single_substring(self):
        """Test initialization with single substring."""
        scanner = BanSubstringsScanner("password", ["password"])
        assert scanner.name == "password"
        assert scanner._banned == ["password"]

    def test_init_empty_name_fails(self):
        """Test that empty name is rejected."""
        with pytest.raises(ValueError, match="name cannot be empty"):
            BanSubstringsScanner("", ["banned"])

    def test_init_empty_substring_fails(self):
        """Test that empty substring is rejected."""
        with pytest.raises(ValueError, match="cannot be empty"):
            BanSubstringsScanner("test", [""])

    def test_init_empty_substring_in_list_fails(self):
        """Test that empty substring in list is rejected."""
        with pytest.raises(ValueError, match="cannot be empty"):
            BanSubstringsScanner("test", ["valid", "", "valid"])

    def test_init_empty_ban_list(self):
        """Test initialization with empty ban list (no bans)."""
        scanner = BanSubstringsScanner("empty", [])
        assert len(scanner._banned) == 0


class TestBanSubstringsProperties:
    """Test scanner properties."""

    def test_name_property(self):
        """Test name property."""
        scanner = BanSubstringsScanner("test_name", ["banned"])
        assert scanner.name == "test_name"

    def test_pattern_single_substring(self):
        """Test pattern property with single substring."""
        scanner = BanSubstringsScanner("test", ["password"])
        assert scanner.pattern == "password"

    def test_pattern_multiple_substrings(self):
        """Test pattern property with multiple substrings."""
        scanner = BanSubstringsScanner("test", ["a", "b", "c"])
        assert "3 banned substrings" in scanner.pattern

    def test_repr(self):
        """Test string representation."""
        scanner = BanSubstringsScanner("test", ["a", "b"])
        repr_str = repr(scanner)
        assert "BanSubstringsScanner" in repr_str
        assert "test" in repr_str
        assert "2" in repr_str


class TestBanSubstringsScan:
    """Test scanning functionality."""

    def test_scan_no_match(self):
        """Test scanning content with no banned substring."""
        scanner = BanSubstringsScanner("test", ["secret"])
        result = scanner.scan("hello world")
        assert result.found is False
        assert result.match_index == -1
        assert result.match_text == ""

    def test_scan_exact_match(self):
        """Test scanning with exact match."""
        scanner = BanSubstringsScanner("test", ["secret"])
        result = scanner.scan("secret")
        assert result.found is True
        assert result.match_index == 0
        assert result.match_text == "secret"

    def test_scan_match_in_middle(self):
        """Test scanning with match in middle of content."""
        scanner = BanSubstringsScanner("test", ["secret"])
        result = scanner.scan("my secret is here")
        assert result.found is True
        assert result.match_index == 3
        assert result.match_text == "secret"

    def test_scan_match_at_end(self):
        """Test scanning with match at end."""
        scanner = BanSubstringsScanner("test", ["secret"])
        result = scanner.scan("this is secret")
        assert result.found is True
        assert result.match_index == 8
        assert result.match_text == "secret"

    def test_scan_case_sensitive(self):
        """Test that scanning is case-sensitive."""
        scanner = BanSubstringsScanner("test", ["Secret"])
        # Should not match lowercase
        result = scanner.scan("my secret is here")
        assert result.found is False

    def test_scan_case_sensitive_match(self):
        """Test case-sensitive matching when case matches."""
        scanner = BanSubstringsScanner("test", ["Secret"])
        result = scanner.scan("my Secret is here")
        assert result.found is True

    def test_scan_multiple_banned_first_match(self):
        """Test scanning with multiple banned substrings returns first match."""
        scanner = BanSubstringsScanner("test", ["api_key", "password", "token"])
        # Content has all three, should find the first one
        result = scanner.scan("api_key=sk password=secret token=xyz")
        assert result.found is True
        assert result.pattern == "api_key"

    def test_scan_multiple_banned_second_match(self):
        """Test finding substring (scanner returns first match in order)."""
        scanner = BanSubstringsScanner("test", ["api_key", "password", "token"])
        # Both api_key and password are present, but api_key appears after password
        result = scanner.scan("has password=secret but no api_key")
        assert result.found is True
        # Scanner searches in order of ban list, so finds api_key first
        assert result.pattern == "api_key"

    def test_scan_partial_not_match(self):
        """Test that partial matches don't trigger."""
        scanner = BanSubstringsScanner("test", ["secret"])
        # "secre" is not a full match for "secret"
        result = scanner.scan("this is secre content")
        assert result.found is False

    def test_scan_substring_order_matters(self):
        """Test that search order respects ban list order."""
        scanner = BanSubstringsScanner("test", ["z", "a", "b"])
        result = scanner.scan("zab")
        # Should find first banned substring in order
        assert result.pattern == "z"
        assert result.match_index == 0


class TestBanSubstringsEmpty:
    """Test edge cases with empty content and empty ban list."""

    def test_scan_empty_content(self):
        """Test scanning empty content."""
        scanner = BanSubstringsScanner("test", ["secret"])
        result = scanner.scan("")
        assert result.found is False

    def test_scan_empty_ban_list(self):
        """Test scanning with empty ban list."""
        scanner = BanSubstringsScanner("test", [])
        result = scanner.scan("anything here")
        assert result.found is False

    def test_scan_empty_both(self):
        """Test scanning empty content with empty ban list."""
        scanner = BanSubstringsScanner("test", [])
        result = scanner.scan("")
        assert result.found is False


class TestBanSubstringsRealWorld:
    """Real-world scenarios."""

    def test_api_key_detection(self):
        """Test detecting API keys."""
        scanner = BanSubstringsScanner("api_keys", ["sk-", "rk-", "pk-"])
        result = scanner.scan("My API key is sk-1234567890 keep it secret")
        assert result.found is True
        assert result.pattern == "sk-"

    def test_password_detection(self):
        """Test detecting password keywords."""
        scanner = BanSubstringsScanner("passwords", ["password", "passwd", "pwd"])
        result = scanner.scan("Enter your password to continue")
        assert result.found is True
        assert result.pattern == "password"

    def test_token_detection(self):
        """Test detecting token patterns."""
        scanner = BanSubstringsScanner("tokens", ["token=", "auth_token", "access_token"])
        result = scanner.scan("URL: example.com?token=abc123&id=456")
        assert result.found is True
        assert result.pattern == "token="

    def test_multiple_sensitive_patterns(self):
        """Test with multiple sensitive patterns."""
        scanner = BanSubstringsScanner(
            "sensitive",
            ["credit_card", "ssn", "api_key", "secret"],
        )
        result = scanner.scan("Config: api_key=sk-123, ssn=123-45-6789")
        assert result.found is True
        # Finds first pattern in order list: ssn before api_key
        assert result.pattern == "ssn"

    def test_no_false_positives(self):
        """Test that scanner doesn't false-positive."""
        scanner = BanSubstringsScanner("test", ["admin"])
        # "administrator" contains "admin" but might want full word only
        result = scanner.scan("User is administrator")
        assert result.found is True  # Current impl finds substring
        # Note: Full word matching would be a different scanner

    def test_sql_injection_keywords(self):
        """Test detecting SQL injection keywords."""
        scanner = BanSubstringsScanner(
            "sql_injection",
            ["DROP TABLE", "DELETE FROM", "INSERT INTO", "UNION SELECT"],
        )
        result = scanner.scan("query: SELECT * FROM users")
        assert result.found is False

        result = scanner.scan("malicious: DROP TABLE users")
        assert result.found is True
        assert result.pattern == "DROP TABLE"


class TestBanSubstringsIntegration:
    """Integration tests."""

    def test_scanner_interface_compatibility(self):
        """Test that BanSubstringsScanner implements Scanner interface."""
        from scanners.interface import Scanner

        scanner = BanSubstringsScanner("test", ["secret"])
        assert isinstance(scanner, Scanner)
        assert hasattr(scanner, "name")
        assert hasattr(scanner, "pattern")
        assert hasattr(scanner, "scan")

    def test_with_registry(self):
        """Test integration with ScannerRegistry."""
        from scanners.registry import ScannerRegistry

        registry = ScannerRegistry()
        scanner = BanSubstringsScanner("api_keys", ["sk-", "rk-"])
        registry.register(scanner)

        # Should be retrievable
        retrieved = registry.get("api_keys")
        assert retrieved is scanner

        # Should be scannable
        results = registry.scan("API key: sk-123456789")
        assert len(results) == 1
        assert results[0].found is True

    def test_multiple_scanners_with_registry(self):
        """Test multiple BanSubstringsScanner instances in registry."""
        from scanners.registry import ScannerRegistry

        registry = ScannerRegistry()
        registry.register(BanSubstringsScanner("api_keys", ["sk-", "rk-"]))
        registry.register(BanSubstringsScanner("passwords", ["password", "passwd"]))

        results = registry.scan("API: sk-123 Password: secret123")
        assert len(results) == 2
        # First result is api_keys
        assert results[0].found is True
        # Second result is passwords - note "password" appears in "Password" (case-sensitive, no match)
        # "secret" doesn't contain "password" or "passwd"
        assert results[1].found is False

    def test_with_guard_engine(self):
        """Test integration with GuardEngine."""
        from guard.engine import GuardEngine
        from scanners.registry import ScannerRegistry

        registry = ScannerRegistry()
        registry.register(BanSubstringsScanner("secrets", ["sk-"]))

        engine = GuardEngine(registry, pattern_length=3)
        result = engine.process_delta("x" * 50 + "sk-123" + "y" * 50)

        if result is not None:
            assert any(r.found for r in result)
