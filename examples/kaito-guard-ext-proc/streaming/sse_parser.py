"""SSE (Server-Sent Events) parser for OpenAI-compatible LLM streaming.

Handles splitting raw bytes into complete SSE events and parsing delta.content.
"""

import json
import logging
from typing import Optional, Tuple

logger = logging.getLogger(__name__)


def split_sse_events(buf: bytes) -> Tuple[list[bytes], bytes]:
    """Split buffer into complete SSE events (delimited by \n\n).

    Args:
        buf: Raw bytes that may contain partial SSE events.

    Returns:
        Tuple of (list of complete events, remaining incomplete data).
    """
    events = []
    while True:
        idx = buf.find(b"\n\n")
        if idx < 0:
            break
        event = buf[:idx]
        events.append(event)
        buf = buf[idx + 2:]
    return events, buf


def parse_sse_event(event: bytes) -> Optional[dict]:
    """Parse a single SSE event and extract delta.content.

    Args:
        event: Raw event bytes (e.g., b"data: {...}").

    Returns:
        Parsed delta dict with "content" key, or None if invalid/empty.
    """
    try:
        event_str = event.decode("utf-8").strip()

        # Skip empty lines
        if not event_str:
            return None

        # Remove "data: " prefix
        if event_str.startswith("data:"):
            event_str = event_str[5:].strip()
        else:
            # Not a data line, skip
            return None

        # Handle [DONE] marker
        if event_str == "[DONE]":
            return {"delta": {"content": ""}, "done": True}

        # Parse JSON payload
        payload = json.loads(event_str)

        # Extract delta.content
        if "choices" in payload and len(payload["choices"]) > 0:
            choice = payload["choices"][0]
            if "delta" in choice:
                delta = choice["delta"]
                content = delta.get("content", "")
                return {"delta": {"content": content}, "finish_reason": choice.get("finish_reason")}

        return None
    except (UnicodeDecodeError, json.JSONDecodeError, KeyError) as e:
        logger.warning(f"[SSE] Failed to parse event: {e}")
        return None


def is_done_event(event: bytes) -> bool:
    """Check if event is the [DONE] marker.

    Args:
        event: Raw event bytes.

    Returns:
        True if this is the final marker.
    """
    try:
        event_str = event.decode("utf-8").strip()
        return event_str == "data: [DONE]" or event_str == "[DONE]"
    except UnicodeDecodeError:
        return False
