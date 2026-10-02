"""GuardEngine coordinator for orchestrating guardrail processing.

Coordinates multiple components (LogicalStreamBuffer, Holdback, ScannerRegistry)
to detect patterns in streaming content from SSE events.
"""

import logging
from typing import List, Optional

from scanners.interface import ScanResult
from scanners.registry import ScannerRegistry
from streaming.holdback import release_safe_prefix
from streaming.logical_stream import LogicalStreamBuffer

logger = logging.getLogger(__name__)


class GuardEngine:
    """Coordinates pattern detection in streaming content.

    Manages the flow of SSE deltas through buffering, holdback, and scanning.
    """

    def __init__(self, scanner_registry: ScannerRegistry, pattern_length: int = 12):
        """Initialize GuardEngine.

        Args:
            scanner_registry: Registry of scanners to use for detection.
            pattern_length: Length of patterns to detect (default 12 for "sk-123456789").
        """
        self._registry = scanner_registry
        self._pattern_length = pattern_length
        self._buffer = LogicalStreamBuffer()
        self._held = ""
        self._scan_results: List[ScanResult] = []

    def process_delta(self, delta_content: str) -> Optional[List[ScanResult]]:
        """Process a single delta from an SSE event.

        Accumulates content, applies holdback, and scans for patterns.

        Args:
            delta_content: New content from this SSE delta.

        Returns:
            List of ScanResult if content was released and scanned, None if held back.
        """
        # Accumulate the new delta into the buffer
        self._buffer.append(delta_content)
        current_content = self._buffer.get()

        # Combine held content with current
        combined = self._held + current_content

        # Apply holdback: split into safe-to-release and must-hold-back
        safe, held = release_safe_prefix(combined, self._pattern_length)

        # Update held for next iteration
        self._held = held

        # If nothing to release, return None (wait for more content)
        if not safe:
            logger.debug(f"[GuardEngine] No safe content to release, holding {len(held)} chars")
            return None

        # Clear the buffer since we've extracted safe content
        self._buffer.clear()

        # Scan the safe content with all registered scanners
        scan_results = self._registry.scan(safe)

        # Store for access via scan_history()
        self._scan_results.extend(scan_results)

        logger.debug(
            f"[GuardEngine] Released {len(safe)} chars, found_patterns="
            f"{sum(1 for r in scan_results if r.found)}/{len(scan_results)}"
        )

        return scan_results

    def flush(self) -> List[ScanResult]:
        """Flush all remaining content (end of stream).

        After stream ends, scan any remaining held content to catch
        patterns at stream boundaries.

        Returns:
            List of ScanResult from scanning remaining content.
        """
        # Combine held content with buffer
        final_content = self._held + self._buffer.get()

        if not final_content:
            logger.debug("[GuardEngine] Nothing to flush")
            return []

        # Scan the final content
        scan_results = self._registry.scan(final_content)
        self._scan_results.extend(scan_results)

        logger.debug(f"[GuardEngine] Flushed {len(final_content)} chars at stream end")

        return scan_results

    def has_detections(self) -> bool:
        """Check if any patterns were detected.

        Returns:
            True if any scan result has found=True.
        """
        return any(r.found for r in self._scan_results)

    def scan_history(self) -> List[ScanResult]:
        """Get complete history of scan results.

        Returns:
            All ScanResult objects from process_delta and flush calls.
        """
        return self._scan_results.copy()

    def get_detections(self) -> List[ScanResult]:
        """Get only the scan results where patterns were found.

        Returns:
            Filtered list containing only found results.
        """
        return [r for r in self._scan_results if r.found]

    def reset(self) -> None:
        """Reset the engine for a new stream.

        Clears buffers, held content, and scan history.
        """
        self._buffer.clear()
        self._held = ""
        self._scan_results = []
        logger.debug("[GuardEngine] Reset for new stream")

    def __repr__(self) -> str:
        """String representation."""
        return (
            f"GuardEngine(scanners={len(self._registry)}, "
            f"buffer_len={len(self._buffer)}, held_len={len(self._held)}, "
            f"detections={sum(1 for r in self._scan_results if r.found)})"
        )
