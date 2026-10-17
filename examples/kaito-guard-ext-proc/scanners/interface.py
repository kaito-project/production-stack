"""Scanner interface for detecting patterns in content.

Defines the abstract Scanner interface that all pattern detectors must implement.
Scanners are composed into a pipeline to detect multiple patterns sequentially.
"""

from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import List


@dataclass
class ScanResult:
    """Result of a single scan operation.

    Attributes:
        scanner_name: Name of the scanner that produced this result.
        pattern: The pattern that was searched for.
        found: Whether the pattern was found in the content.
        match_index: Index of the first match (-1 if not found).
        match_text: The actual matched text (empty if not found).
    """

    scanner_name: str
    pattern: str
    found: bool
    match_index: int = -1
    match_text: str = ""

    def __post_init__(self):
        """Validate result consistency."""
        if self.found:
            if self.match_index < 0:
                raise ValueError("match_index must be >= 0 when found=True")
            if not self.match_text:
                raise ValueError("match_text must be non-empty when found=True")
        else:
            if self.match_index >= 0:
                raise ValueError("match_index must be -1 when found=False")
            if self.match_text:
                raise ValueError("match_text must be empty when found=False")


class Scanner(ABC):
    """Abstract base class for all pattern scanners.

    Scanners detect specific patterns in content and return ScanResult objects.
    Each scanner is responsible for one pattern type (e.g., API keys, PII, etc).
    """

    @property
    @abstractmethod
    def name(self) -> str:
        """Unique name of this scanner (e.g., 'ban_substrings', 'regex_detector')."""

    @property
    @abstractmethod
    def pattern(self) -> str:
        """The pattern this scanner detects (e.g., 'sk-123456789' pattern)."""

    @abstractmethod
    def scan(self, content: str) -> ScanResult:
        """Scan content for the pattern.

        Args:
            content: The text to scan.

        Returns:
            ScanResult with found flag and match details.
        """
