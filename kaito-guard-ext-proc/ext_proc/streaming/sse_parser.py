import json
import re
from typing import NotRequired, TypedDict


class SSEParseError(ValueError):
    """Invalid SSE encoding, JSON, or OpenAI chunk structure."""


class ContentDelta(TypedDict):
    content: str


class ParsedEvent(TypedDict):
    delta: ContentDelta
    finish_reason: NotRequired[str | None]
    done: NotRequired[bool]


_LINE_ENDING = re.compile(rb"\r\n|\r|\n")


def split_sse_events(buf: bytes, *, final: bool = False) -> tuple[list[bytes], bytes]:
    """Split on blank SSE lines; final resolves a trailing CR at EOF.

    Input without a terminating blank line remains in the remainder, even at EOF.
    """
    events: list[bytes] = []
    event_start = line_start = event_end = 0
    for ending in _LINE_ENDING.finditer(buf):
        # A trailing CR may be the first byte of a CRLF in the next chunk.
        if not final and ending.group() == b"\r" and ending.end() == len(buf):
            break
        if ending.start() == line_start:
            events.append(buf[event_start:event_end])
            event_start = ending.end()
            event_end = event_start
        else:
            event_end = ending.start()
        line_start = ending.end()
    return events, buf[event_start:]


def _extract_data(event: bytes) -> str | None:
    try:
        text = event.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise SSEParseError("Invalid UTF-8 in SSE event") from exc

    # Preserve the original helper's support for an unprefixed DONE marker.
    if text.strip() == "[DONE]":
        return "[DONE]"

    data: list[str] = []
    for line in re.split(r"\r\n|\r|\n", text):
        if not line or line.startswith(":"):
            continue
        field, _, value = line.partition(":")
        if field == "data":
            data.append(value.removeprefix(" "))
    return "\n".join(data) if data else None


def parse_sse_event(event: bytes) -> ParsedEvent | None:
    """Parse a single OpenAI choice; raise SSEParseError for malformed data.

    None denotes an event without data, empty data, or an empty choices list.
    Missing/null content is a valid non-text delta, represented by "".
    Multiple choices and explicit choice indices other than integer 0 are rejected.
    """
    data = _extract_data(event)
    if data is None or not data.strip():
        return None
    if data.strip() == "[DONE]":
        return {"delta": {"content": ""}, "done": True}
    try:
        payload = json.loads(data)
    except json.JSONDecodeError as exc:
        raise SSEParseError("Invalid JSON in SSE data") from exc

    if not isinstance(payload, dict):
        raise SSEParseError("SSE payload must be an object")
    choices = payload.get("choices")
    if not isinstance(choices, list):
        raise SSEParseError("payload.choices must be a list")
    if not choices:
        return None
    if len(choices) != 1:
        raise SSEParseError("Only one choice is supported")
    choice = choices[0]
    if not isinstance(choice, dict):
        raise SSEParseError("choices[0] must be an object")
    choice_index = choice.get("index", 0)
    if type(choice_index) is not int or choice_index != 0:
        raise SSEParseError("Only choice index 0 is supported")
    delta = choice.get("delta")
    if not isinstance(delta, dict):
        raise SSEParseError("choices[0].delta must be an object")
    content = delta.get("content")
    if content is None:
        content = ""
    if not isinstance(content, str):
        raise SSEParseError("delta.content must be a string or null")
    finish_reason = choice.get("finish_reason")
    if finish_reason is not None and not isinstance(finish_reason, str):
        raise SSEParseError("finish_reason must be a string or null")
    return {"delta": {"content": content}, "finish_reason": finish_reason}


def is_done_event(event: bytes) -> bool:
    """Use the same SSE field extraction and encoding checks as parsing."""
    data = _extract_data(event)
    return data is not None and data.strip() == "[DONE]"
