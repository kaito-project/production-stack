"""Holdback mechanism for detecting patterns split across SSE events.

Prevents unsafe prefix leakage when a secret pattern is split across multiple
SSE events (e.g., "sk-12" in event 1, "3456789" in event 2).

The key insight: hold back (pattern_length - 1) bytes from being released
to ensure any cross-boundary pattern is caught by the next scan pass.
"""

import logging
from typing import Tuple

logger = logging.getLogger(__name__)


def release_safe_prefix(pending: str, pattern_length: int = 12) -> Tuple[str, str]:
    """Release text that is safe from cross-event pattern overlap.

    Holds back (pattern_length - 1) bytes to ensure no pattern gets split
    across the boundary between released and held text.

    Args:
        pending: Accumulated text that may contain a pattern.
        pattern_length: Length of the pattern we're trying to detect.
                       Default 12 for "sk-123456789".

    Returns:
        Tuple of (safe_to_release, must_hold_back).
        - safe_to_release: text that can be forwarded downstream
        - must_hold_back: text that must stay in the buffer for next scan
    """
    holdback_size = pattern_length - 1

    if len(pending) <= holdback_size:
        # Buffer is too small; hold everything
        logger.debug(f"[Holdback] Pending {len(pending)} <= holdback {holdback_size}, holding all")
        return "", pending

    # Release everything except the last (pattern_length - 1) bytes
    release_len = len(pending) - holdback_size
    safe = pending[:release_len]
    held = pending[release_len:]

    logger.debug(
        f"[Holdback] Released {len(safe)} chars, holding {len(held)} chars "
        f"(pattern_length={pattern_length})"
    )
    return safe, held


def detect_cross_boundary_pattern(
    previously_held: str, new_content: str, pattern: str
) -> bool:
    """Detect if a pattern spans the boundary between held and new content.

    Useful for debugging: shows when a pattern would have been missed
    without the holdback mechanism.

    Args:
        previously_held: Text held from the previous scan.
        new_content: Text added in the current scan.
        pattern: Pattern to search for.

    Returns:
        True if pattern spans the boundary.
    """
    combined = previously_held + new_content
    if pattern not in combined:
        return False

    # Pattern exists in combined; check if it spans the boundary
    idx = combined.find(pattern)
    if idx < 0:
        return False

    pattern_end = idx + len(pattern)
    # Spans if pattern starts before boundary and ends after
    return idx < len(previously_held) < pattern_end
