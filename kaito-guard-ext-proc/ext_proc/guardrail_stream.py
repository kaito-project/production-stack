# Copyright (c) KAITO authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Compose shared streaming APIs for one single-choice response, not Envoy I/O."""

from guardrail_core.streaming.openai import (
    OpenAIChatChunkParseStatus,
    parse_openai_chat_sse_event,
)
from guardrail_core.streaming.sse import SSEEvent, SSEFramer
from guardrail_core.streaming.window import (
    StreamingBufferWindow,
    WindowEmitResult,
    WindowScanner,
)


class GuardrailStreamError(ValueError):
    """The response violates the single-choice streaming protocol."""


class GuardrailStream:
    """Scan one response's SSE text before releasing it.

    Use a separate instance per response. The caller handles upstream closure
    and refusal responses, and must call cancel() on external failure or
    cancellation.
    """

    def __init__(
        self,
        scanner: WindowScanner,
        *,
        holdback_len: int,
        max_event_bytes: int = 1024 * 1024,
    ) -> None:
        """Set holdback_len in Unicode code points to cover cross-chunk matches."""
        self._framer: SSEFramer | None = SSEFramer(max_event_bytes=max_event_bytes)
        self._window: StreamingBufferWindow | None = StreamingBufferWindow(
            scanner, holdback_len=holdback_len
        )
        self._done_seen = False
        self._blocked = False

    @property
    def closed(self) -> bool:
        return self._window is None

    @property
    def blocked(self) -> bool:
        return self._blocked

    def feed(self, chunk: bytes) -> WindowEmitResult:
        """Return approved chunks; blocked means stop.

        Errors propagate and permanently close the stream without flushing.
        """
        return self._advance(chunk, final=False)

    def finish(self) -> WindowEmitResult:
        """At normal EOF, validate framing and DONE before flushing once.

        Errors propagate and permanently close the stream without flushing.
        """
        return self._advance(b"", final=True)

    def cancel(self) -> None:
        """Discard pending state without scanning or emitting; idempotent."""
        self._window = None
        self._framer = None

    def _advance(self, chunk: bytes, *, final: bool) -> WindowEmitResult:
        framer, window = self._framer, self._window
        if framer is None or window is None:
            raise RuntimeError("Guardrail stream is closed.")

        succeeded = False
        try:
            events = framer.finish() if final else framer.feed(chunk)
            result = self._consume(events, window)
            if not result.blocked and final:
                if not self._done_seen:
                    raise GuardrailStreamError("Stream ended without a DONE event.")
                tail = window.flush()
                result = WindowEmitResult(
                    chunks=() if tail.blocked else result.chunks + tail.chunks,
                    blocked=tail.blocked,
                )
            self._blocked = result.blocked
            succeeded = True
            return result
        finally:
            # Also discard state for BaseException control signals.
            if final or not succeeded or self._blocked:
                self.cancel()

    def _consume(
        self, events: list[SSEEvent], window: StreamingBufferWindow
    ) -> WindowEmitResult:
        chunks: list[str] = []
        for event in events:
            parsed = parse_openai_chat_sse_event(event)
            if parsed.status == OpenAIChatChunkParseStatus.NO_DATA:
                continue
            if self._done_seen:
                raise GuardrailStreamError("Data received after the DONE event.")
            if parsed.status == OpenAIChatChunkParseStatus.DONE:
                self._done_seen = True
                continue
            if parsed.status != OpenAIChatChunkParseStatus.PARSED:
                raise GuardrailStreamError(
                    f"Invalid OpenAI stream event: {parsed.status}."
                )
            if not parsed.parsed_choices:
                continue
            if (
                len(parsed.parsed_choices) != 1
                or parsed.parsed_choices[0].choice_index != 0
            ):
                raise GuardrailStreamError(
                    "Stream requires exactly one choice with index zero."
                )
            content = parsed.parsed_choices[0].content
            if content is not None:
                result = window.feed(content)
                if result.blocked:
                    return WindowEmitResult(chunks=(), blocked=True)
                chunks.extend(result.chunks)
        return WindowEmitResult(chunks=tuple(chunks))
