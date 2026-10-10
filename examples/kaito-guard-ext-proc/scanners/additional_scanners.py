"""Additional Scanner implementations: Regex, Secrets, Sensitive (PII)."""

import logging
import re
from typing import List, Optional

from scanners.interface import Scanner, ScanResult

logger = logging.getLogger(__name__)


class RegexScanner(Scanner):
    """Detects patterns using regular expressions.

    Supports multiple regex patterns with priority order.
    Case-sensitive by default, can be made case-insensitive.
    """

    def __init__(self, name: str, patterns: List[str], case_insensitive: bool = False):
        """Initialize RegexScanner.

        Args:
            name: Unique scanner identifier.
            patterns: List of regex patterns to search for.
            case_insensitive: If True, use re.IGNORECASE flag.

        Raises:
            ValueError: If name is empty or pattern is invalid regex.
        """
        if not name:
            raise ValueError("Scanner name cannot be empty")

        self._name = name
        self._case_insensitive = case_insensitive
        self._compiled_patterns = []

        for pattern in patterns:
            try:
                flags = re.IGNORECASE if case_insensitive else 0
                compiled = re.compile(pattern, flags)
                self._compiled_patterns.append((pattern, compiled))
            except re.error as e:
                raise ValueError(f"Invalid regex pattern '{pattern}': {e}")

    @property
    def name(self) -> str:
        """Return scanner name."""
        return self._name

    @property
    def pattern(self) -> str:
        """Return pattern description."""
        if len(self._compiled_patterns) == 1:
            return self._compiled_patterns[0][0]
        return f"{len(self._compiled_patterns)} regex patterns"

    def scan(self, content: str) -> ScanResult:
        """Scan for regex patterns in content.

        Args:
            content: Text to scan.

        Returns:
            ScanResult with first match found.
        """
        for pattern_str, compiled_pattern in self._compiled_patterns:
            match = compiled_pattern.search(content)
            if match:
                logger.debug(
                    f"[{self._name}] Regex '{pattern_str}' matched at {match.start()}"
                )
                return ScanResult(
                    scanner_name=self._name,
                    pattern=pattern_str,
                    found=True,
                    match_index=match.start(),
                    match_text=match.group(0),
                )

        return ScanResult(
            scanner_name=self._name,
            pattern=self.pattern,
            found=False,
        )


class SecretsScanner(Scanner):
    """Detects secrets like API keys and tokens with optional redaction.

    Patterns supported:
    - OpenAI API keys: sk-... (48+ chars)
    - Anthropic keys: sk-ant-...
    - Generic secrets: xxxx-...
    - Environment variable assignments: API_KEY=value
    """

    def __init__(self, name: str, redaction_mode: str = "none"):
        """Initialize SecretsScanner.

        Args:
            name: Unique scanner identifier.
            redaction_mode: How to redact found secrets.
                - "none": Return as-is
                - "partial": Redact middle chars (sk-****...****)
                - "hash": Replace with hash
                - "full": Replace with [REDACTED]

        Raises:
            ValueError: If redaction_mode is invalid.
        """
        if not name:
            raise ValueError("Scanner name cannot be empty")
        if redaction_mode not in ("none", "partial", "hash", "full"):
            raise ValueError(f"Invalid redaction_mode: {redaction_mode}")

        self._name = name
        self._redaction_mode = redaction_mode

        # Patterns for common API keys and secrets
        self._patterns = [
            (r"sk-[A-Za-z0-9]{20,}", "OpenAI API key"),
            (r"sk-ant-[A-Za-z0-9]{40,}", "Anthropic API key"),
            (r"pk_[a-z]{2}_[A-Za-z0-9]{24,}", "Stripe key"),
            (r"ghp_[A-Za-z0-9]{36,}", "GitHub token"),
            (r"(API_KEY|apikey|api_key|api-key|secret|token|password)\s*=\s*\S+", "Assignment secret"),
        ]

    @property
    def name(self) -> str:
        """Return scanner name."""
        return self._name

    @property
    def pattern(self) -> str:
        """Return pattern description."""
        return "API keys, tokens, secrets"

    def scan(self, content: str) -> ScanResult:
        """Scan for secrets in content.

        Args:
            content: Text to scan.

        Returns:
            ScanResult with first secret found.
        """
        for pattern, description in self._patterns:
            match = re.search(pattern, content)
            if match:
                matched_text = match.group(0)
                redacted_text = self._redact(matched_text)

                logger.debug(f"[{self._name}] Found {description} at {match.start()}")

                return ScanResult(
                    scanner_name=self._name,
                    pattern=description,
                    found=True,
                    match_index=match.start(),
                    match_text=redacted_text,
                )

        return ScanResult(
            scanner_name=self._name,
            pattern=self.pattern,
            found=False,
        )

    def _redact(self, secret: str) -> str:
        """Apply redaction based on mode.

        Args:
            secret: The secret string to redact.

        Returns:
            Redacted version based on redaction_mode.
        """
        if self._redaction_mode == "none":
            return secret
        elif self._redaction_mode == "full":
            return "[REDACTED]"
        elif self._redaction_mode == "partial":
            # Keep first 4 and last 4 chars, redact middle
            if len(secret) <= 8:
                return secret[0] + "*" * (len(secret) - 2) + secret[-1]
            return secret[:4] + "*" * (len(secret) - 8) + secret[-4:]
        elif self._redaction_mode == "hash":
            # Hash-like representation
            return f"[HASH:{hash(secret) % 10000:04d}]"
        return secret


class SensitiveScanner(Scanner):
    """Detects personally identifiable information (PII) and sensitive data.

    Patterns:
    - Email addresses
    - Phone numbers (multiple formats)
    - Credit card numbers
    - IP addresses (IPv4 and IPv6)
    - Social Security Numbers
    """

    def __init__(self, name: str = "sensitive"):
        """Initialize SensitiveScanner.

        Args:
            name: Unique scanner identifier.
        """
        if not name:
            raise ValueError("Scanner name cannot be empty")

        self._name = name

        # PII patterns
        self._patterns = [
            (r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}\b", "Email address"),
            (r"\b(?:\+?1[-.\s]?)?\(?([0-9]{3})\)?[-.\s]?([0-9]{3})[-.\s]?([0-9]{4})\b", "Phone number"),
            (r"\b(?:\d{4}[-\s]?){3}\d{4}\b", "Credit card number"),
            (r"\b(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\b", "IPv4 address"),
            (r"\b(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}\b", "IPv6 address"),
            (r"\b\d{3}-\d{2}-\d{4}\b", "SSN format"),
        ]

    @property
    def name(self) -> str:
        """Return scanner name."""
        return self._name

    @property
    def pattern(self) -> str:
        """Return pattern description."""
        return "PII/Sensitive data (email, phone, credit card, IP, SSN)"

    def scan(self, content: str) -> ScanResult:
        """Scan for PII and sensitive data.

        Args:
            content: Text to scan.

        Returns:
            ScanResult with first PII match found.
        """
        for pattern, description in self._patterns:
            match = re.search(pattern, content)
            if match:
                logger.debug(f"[{self._name}] Found {description} at {match.start()}")

                return ScanResult(
                    scanner_name=self._name,
                    pattern=description,
                    found=True,
                    match_index=match.start(),
                    match_text=match.group(0),
                )

        return ScanResult(
            scanner_name=self._name,
            pattern=self.pattern,
            found=False,
        )
