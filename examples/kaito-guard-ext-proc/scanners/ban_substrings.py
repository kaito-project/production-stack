"""BanSubstrings scanner - detects banned substring patterns.

Reference implementation of a Scanner that checks for any of a list
of banned substrings in content. Used to block sensitive patterns,
keywords, or known harmful content.
"""

import logging
from typing import List

from scanners.interface import Scanner, ScanResult

logger = logging.getLogger(__name__)


class BanSubstringsScanner(Scanner):
    """Detects if content contains any banned substrings.

    Searches for exact substring matches (case-sensitive).
    Reports the first match found.
    """

    def __init__(self, name: str, banned_substrings: List[str]):
        """Initialize BanSubstringsScanner.

        Args:
            name: Unique name for this scanner.
            banned_substrings: List of substrings to ban. Empty list means no bans.

        Raises:
            ValueError: If name is empty or banned_substrings contains empty strings.
        """
        if not name:
            raise ValueError("Scanner name cannot be empty")

        for substring in banned_substrings:
            if not substring:
                raise ValueError("Banned substrings cannot be empty")

        self._name = name
        self._banned = banned_substrings

    @property
    def name(self) -> str:
        """Return scanner name."""
        return self._name

    @property
    def pattern(self) -> str:
        """Return pattern description.

        For multi-substring scanner, returns a descriptive string.
        """
        if len(self._banned) == 1:
            return self._banned[0]
        return f"{len(self._banned)} banned substrings"

    def scan(self, content: str) -> ScanResult:
        """Scan for any banned substring in content.

        Searches content for exact substring matches (case-sensitive).
        Reports first match found.

        Args:
            content: Text to scan.

        Returns:
            ScanResult with match details if found, not-found result otherwise.
        """
        # Search for each banned substring
        for banned in self._banned:
            idx = content.find(banned)
            if idx >= 0:
                logger.debug(f"[{self._name}] Found banned substring '{banned}' at index {idx}")
                return ScanResult(
                    scanner_name=self._name,
                    pattern=banned,
                    found=True,
                    match_index=idx,
                    match_text=banned,
                )

        # No banned substrings found
        logger.debug(f"[{self._name}] No banned substrings found in content")
        return ScanResult(
            scanner_name=self._name,
            pattern=self.pattern,
            found=False,
        )

    def __repr__(self) -> str:
        """String representation."""
        return f"BanSubstringsScanner(name={self._name!r}, banned_count={len(self._banned)})"
