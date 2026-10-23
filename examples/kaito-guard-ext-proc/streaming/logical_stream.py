"""Logical stream buffer for concatenating delta.content across SSE events.

Accumulates delta content from multiple SSE events into a logical stream.
Coordinates with holdback mechanism to prevent unsafe prefix leakage.
"""

import logging
from typing import Tuple

logger = logging.getLogger(__name__)


class LogicalStreamBuffer:
    """Accumulates delta.content into a logical stream.

    Handles concatenation of fragments from sequential SSE events.
    """

    def __init__(self):
        """Initialize the buffer."""
        self._buffer = ""

    def append(self, content: str) -> None:
        """Append content to the logical buffer.

        Args:
            content: Delta content to append (may be empty).
        """
        if content:
            self._buffer += content
            logger.debug(f"[LogicalStream] Appended {len(content)} chars, buffer now: {self._buffer!r}")

    def get(self) -> str:
        """Get current buffer contents.

        Returns:
            Current accumulated content.
        """
        return self._buffer

    def clear(self) -> None:
        """Clear the buffer."""
        self._buffer = ""
        logger.debug("[LogicalStream] Buffer cleared")

    def flush_and_clear(self) -> str:
        """Get and clear buffer in one operation.

        Returns:
            Contents before clear.
        """
        result = self._buffer
        self.clear()
        return result

    def __len__(self) -> int:
        """Get buffer length."""
        return len(self._buffer)

    def __repr__(self) -> str:
        """String representation."""
        return f"LogicalStreamBuffer(len={len(self._buffer)}, content={self._buffer[:50]!r}...)"
