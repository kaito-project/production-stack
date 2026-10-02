"""Test PR-2.4: Additional Scanners (Regex, Secrets, Sensitive)."""

import pytest
from scanners.additional_scanners import RegexScanner, SecretsScanner, SensitiveScanner


class TestRegexScanner:
    """Test RegexScanner functionality."""

    def test_init_valid(self):
        """Test valid initialization."""
        scanner = RegexScanner("test", [r"\d+"])
        assert scanner.name == "test"

    def test_init_invalid_regex(self):
        """Test that invalid regex raises ValueError."""
        with pytest.raises(ValueError, match="Invalid regex"):
            RegexScanner("test", [r"[invalid("])

    def test_init_empty_name(self):
        """Test that empty name raises ValueError."""
        with pytest.raises(ValueError, match="name cannot be empty"):
            RegexScanner("", [r"\d+"])

    def test_scan_no_match(self):
        """Test scanning with no match."""
        scanner = RegexScanner("test", [r"\d{10}"])
        result = scanner.scan("no numbers here")
        assert result.found is False

    def test_scan_single_pattern_match(self):
        """Test matching single pattern."""
        scanner = RegexScanner("digits", [r"\d+"])
        result = scanner.scan("The year is 2026")
        assert result.found is True
        assert result.match_text == "2026"
        assert result.match_index == 12

    def test_scan_multiple_patterns(self):
        """Test multiple patterns with first match."""
        scanner = RegexScanner("multi", [r"[a-z]+", r"\d+"])
        result = scanner.scan("123 abc 456")
        # Should find "abc" first (letters before digits in the string)
        assert result.found is True
        assert result.pattern == r"[a-z]+"

    def test_scan_case_insensitive(self):
        """Test case-insensitive matching."""
        scanner = RegexScanner("email", [r"[A-Z]+"], case_insensitive=True)
        result = scanner.scan("hello world")
        assert result.found is True
        assert result.match_text == "hello"

    def test_scan_case_sensitive(self):
        """Test case-sensitive matching."""
        scanner = RegexScanner("email", [r"[A-Z]+"], case_insensitive=False)
        result = scanner.scan("hello world")
        assert result.found is False

    def test_pattern_property(self):
        """Test pattern property."""
        scanner = RegexScanner("test", [r"\d+"])
        assert scanner.pattern == r"\d+"

    def test_pattern_property_multiple(self):
        """Test pattern property with multiple patterns."""
        scanner = RegexScanner("test", [r"\d+", r"[a-z]+", r"[A-Z]+"])
        assert "3 regex patterns" in scanner.pattern


class TestSecretsScanner:
    """Test SecretsScanner functionality."""

    def test_init_valid(self):
        """Test valid initialization."""
        scanner = SecretsScanner("secrets", redaction_mode="full")
        assert scanner.name == "secrets"

    def test_init_invalid_redaction_mode(self):
        """Test that invalid redaction mode raises ValueError."""
        with pytest.raises(ValueError, match="Invalid redaction_mode"):
            SecretsScanner("test", redaction_mode="invalid")

    def test_init_empty_name(self):
        """Test that empty name raises ValueError."""
        with pytest.raises(ValueError, match="name cannot be empty"):
            SecretsScanner("")

    def test_scan_openai_key(self):
        """Test detecting OpenAI API key."""
        scanner = SecretsScanner("test", redaction_mode="none")
        result = scanner.scan("My key is sk-proj1234567890abcdefghijklmnop12345")
        assert result.found is True
        assert "OpenAI" in result.pattern

    def test_scan_no_secrets(self):
        """Test scanning content with no secrets."""
        scanner = SecretsScanner("test")
        result = scanner.scan("Just some normal text here")
        assert result.found is False

    def test_redaction_none(self):
        """Test redaction_mode='none'."""
        scanner = SecretsScanner("test", redaction_mode="none")
        result = scanner.scan("key is sk-abcdef1234567890123456789012345")
        assert result.found is True
        assert result.match_text.startswith("sk-")

    def test_redaction_full(self):
        """Test redaction_mode='full'."""
        scanner = SecretsScanner("test", redaction_mode="full")
        result = scanner.scan("key is sk-abcdef1234567890123456789012345")
        assert result.found is True
        assert result.match_text == "[REDACTED]"

    def test_redaction_partial(self):
        """Test redaction_mode='partial'."""
        scanner = SecretsScanner("test", redaction_mode="partial")
        result = scanner.scan("key is sk-abcdef1234567890123456789012345")
        assert result.found is True
        # Should keep first 4 and last 4
        assert result.match_text.startswith("sk-a")
        assert "*" in result.match_text

    def test_redaction_hash(self):
        """Test redaction_mode='hash'."""
        scanner = SecretsScanner("test", redaction_mode="hash")
        result = scanner.scan("key is sk-abcdef1234567890123456789012345")
        assert result.found is True
        assert "[HASH:" in result.match_text

    def test_scan_assignment(self):
        """Test detecting secret assignment."""
        scanner = SecretsScanner("test")
        result = scanner.scan("API_KEY=my-secret-value-here")
        assert result.found is True


class TestSensitiveScanner:
    """Test SensitiveScanner functionality."""

    def test_init_valid(self):
        """Test valid initialization."""
        scanner = SensitiveScanner("pii")
        assert scanner.name == "pii"

    def test_init_empty_name(self):
        """Test that empty name raises ValueError."""
        with pytest.raises(ValueError, match="name cannot be empty"):
            SensitiveScanner("")

    def test_scan_email(self):
        """Test detecting email address."""
        scanner = SensitiveScanner()
        result = scanner.scan("Contact: john.doe@example.com for details")
        assert result.found is True
        assert "Email" in result.pattern
        assert "john.doe@example.com" in result.match_text

    def test_scan_phone_us_format(self):
        """Test detecting US phone number."""
        scanner = SensitiveScanner()
        result = scanner.scan("Call 555-123-4567 for support")
        assert result.found is True
        assert "Phone" in result.pattern

    def test_scan_credit_card(self):
        """Test detecting credit card number."""
        scanner = SensitiveScanner()
        result = scanner.scan("Card: 4111-1111-1111-1111 expires next year")
        assert result.found is True
        assert "Credit card" in result.pattern

    def test_scan_ipv4(self):
        """Test detecting IPv4 address."""
        scanner = SensitiveScanner()
        result = scanner.scan("Server IP: 192.168.1.1 is accessible")
        assert result.found is True
        assert "IPv4" in result.pattern
        assert "192.168.1.1" in result.match_text

    def test_scan_ipv6(self):
        """Test detecting IPv6 address."""
        scanner = SensitiveScanner()
        result = scanner.scan("IPv6: 2001:0db8:85a3:0000:0000:8a2e:0370:7334 here")
        assert result.found is True
        assert "IPv6" in result.pattern

    def test_scan_ssn(self):
        """Test detecting SSN format."""
        scanner = SensitiveScanner()
        result = scanner.scan("SSN: 123-45-6789 for John Doe")
        assert result.found is True
        assert "SSN" in result.pattern

    def test_scan_no_pii(self):
        """Test scanning content with no PII."""
        scanner = SensitiveScanner()
        result = scanner.scan("This is just regular text with no sensitive data")
        assert result.found is False

    def test_pattern_property(self):
        """Test pattern property."""
        scanner = SensitiveScanner()
        assert "PII" in scanner.pattern
        assert "email" in scanner.pattern.lower()


class TestIntegration:
    """Integration tests with Scanner interface and registry."""

    def test_regex_scanner_interface(self):
        """Test RegexScanner implements Scanner interface."""
        from scanners.interface import Scanner

        scanner = RegexScanner("test", [r"\d+"])
        assert isinstance(scanner, Scanner)

    def test_secrets_scanner_interface(self):
        """Test SecretsScanner implements Scanner interface."""
        from scanners.interface import Scanner

        scanner = SecretsScanner("test")
        assert isinstance(scanner, Scanner)

    def test_sensitive_scanner_interface(self):
        """Test SensitiveScanner implements Scanner interface."""
        from scanners.interface import Scanner

        scanner = SensitiveScanner()
        assert isinstance(scanner, Scanner)

    def test_with_registry(self):
        """Test scanners with ScannerRegistry."""
        from scanners.registry import ScannerRegistry

        registry = ScannerRegistry()
        registry.register(RegexScanner("numbers", [r"\d+"]))
        registry.register(SecretsScanner("secrets"))
        registry.register(SensitiveScanner("pii"))

        # All three should be registered
        assert len(registry) == 3
        assert registry.get("numbers") is not None
        assert registry.get("secrets") is not None
        assert registry.get("pii") is not None

    def test_scan_all_with_mixed_content(self):
        """Test scanning with multiple scanner types."""
        from scanners.registry import ScannerRegistry

        registry = ScannerRegistry()
        registry.register(RegexScanner("numbers", [r"\d+"]))
        registry.register(SecretsScanner("secrets", redaction_mode="full"))
        registry.register(SensitiveScanner("pii"))

        content = "x" * 50 + "My email is john@example.com and API_KEY=secret123" + "y" * 50

        results = registry.scan(content)
        assert len(results) == 3

        # Check that email was found
        email_found = any("Email" in r.pattern for r in results if r.found)
        assert email_found

    def test_with_guard_engine(self):
        """Test integration with GuardEngine."""
        from guard.engine import GuardEngine
        from scanners.registry import ScannerRegistry

        registry = ScannerRegistry()
        registry.register(RegexScanner("numbers", [r"\d+"]))
        registry.register(SensitiveScanner("pii"))

        engine = GuardEngine(registry, pattern_length=12)

        # Large content with email pattern
        content = "x" * 50 + "Email: test@example.com " + "y" * 50
        result = engine.process_delta(content)

        # Should scan something
        history = engine.scan_history()
        assert len(history) > 0


class TestRealWorldScenarios:
    """Real-world usage scenarios."""

    def test_log_line_with_secrets(self):
        """Test scanning typical log line with secrets."""
        scanner = SecretsScanner("log_secrets", redaction_mode="partial")
        log = "2026-10-02 ERROR: API_KEY=sk-1234567890abcdefghijkl connection failed"
        result = scanner.scan(log)
        assert result.found is True
        assert "[REDACTED]" not in result.match_text  # partial mode

    def test_config_with_pii(self):
        """Test scanning config file with PII."""
        scanner = SensitiveScanner("config_pii")
        config = """
        admin_email: admin@company.com
        backup_phone: 555-123-4567
        billing_card: 4111-1111-1111-1111
        """
        result = scanner.scan(config)
        assert result.found is True
        assert "Email" in result.pattern

    def test_multiple_secrets_first_match(self):
        """Test that first secret in order is found."""
        scanner = SecretsScanner("all_secrets", redaction_mode="full")
        content = "API_KEY=secret token=xyz ghp_12345678901234567890123456789012345678"
        result = scanner.scan(content)
        assert result.found is True
        # Should find the first pattern that matches

    def test_pii_in_user_comment(self):
        """Test finding PII in user-generated content."""
        scanner = SensitiveScanner()
        comment = "Contact john.smith@domain.com for details"
        result = scanner.scan(comment)
        assert result.found is True
        assert "Email" in result.pattern
