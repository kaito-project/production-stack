import json
from dataclasses import replace
from typing import NotRequired, TypedDict

from guardrail_core.streaming.openai import (
    OpenAIChatChunkParseStatus,
    parse_openai_chat_sse_event,
)
from guardrail_core.streaming.sse import (
    SSEEvent,
    SSEFramer,
    SSEFramingError,
    SSEIncompleteEventError,
)


class SSEParseError(ValueError):
    """Invalid SSE encoding, JSON, or OpenAI chunk structure."""


class ContentDelta(TypedDict):
    content: str


class ParsedEvent(TypedDict):
    delta: ContentDelta
    finish_reason: NotRequired[str | None]
    done: NotRequired[bool]


def split_sse_events(buf: bytes, *, final: bool = False) -> tuple[list[bytes], bytes]:
    """Split complete events; final resolves trailing CR, not missing delimiters."""
    # This legacy buffer API has no size limit; stateful consumers should use
    # SSEFramer directly to enforce its bounded buffering and strict EOF policy.
    framer = SSEFramer(max_event_bytes=max(1, len(buf)))
    try:
        events = framer.feed(buf)
        if final:
            try:
                events.extend(framer.finish())
            except SSEIncompleteEventError as exc:
                events.extend(exc.completed_events)
    except SSEFramingError as exc:
        raise SSEParseError(str(exc)) from exc
    consumed = sum(len(event.raw_bytes) for event in events)
    return [event.raw_bytes.rstrip(b"\r\n") for event in events], buf[consumed:]


def _to_sse_event(event: bytes) -> SSEEvent:
    try:
        text = event.decode("utf-8-sig")
    except UnicodeDecodeError as exc:
        raise SSEParseError("Invalid UTF-8 in SSE event") from exc
    if text.strip() == "[DONE]":
        text = "data: [DONE]"
    return SSEEvent.from_raw(text, raw_bytes=event)


def parse_sse_event(event: bytes) -> ParsedEvent | None:
    """Parse a single-choice OpenAI event; raise SSEParseError for invalid data."""
    sse_event = _to_sse_event(event)
    result = parse_openai_chat_sse_event(sse_event)
    if result.status == OpenAIChatChunkParseStatus.NO_DATA:
        return None
    if result.status == OpenAIChatChunkParseStatus.DONE:
        return {"delta": {"content": ""}, "done": True}
    if result.status == OpenAIChatChunkParseStatus.MALFORMED_JSON:
        raise SSEParseError(f"Invalid JSON in SSE data: {result.error}")

    payload = result.payload
    if payload is None:
        raise SSEParseError("SSE payload must be an object")
    # Keep production-stack's stricter shape and single-choice policy.
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
    if "index" not in choice:
        # The shared parser requires an explicit index; legacy callers omit it.
        normalized = {**payload, "choices": [{**choice, "index": 0}]}
        result = parse_openai_chat_sse_event(
            replace(sse_event, data=json.dumps(normalized))
        )
    if result.status != OpenAIChatChunkParseStatus.PARSED:
        raise SSEParseError(result.error)
    parsed_choice = result.parsed_choices[0]
    return {
        "delta": {"content": parsed_choice.content or ""},
        "finish_reason": parsed_choice.finish_reason,
    }


def is_done_event(event: bytes) -> bool:
    """Use the same SSE field extraction and encoding checks as parsing."""
    data = _to_sse_event(event).data
    return data is not None and data.strip() == "[DONE]"
