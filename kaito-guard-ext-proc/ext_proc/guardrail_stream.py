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
    OpenAIChatChunkParseResult,
    build_openai_chat_delta_sse_chunk,
    build_sse_data_chunk,
    build_sse_done_chunk,
    parse_openai_chat_sse_event,
)
from guardrail_core.streaming.sse import SSEEvent, SSEFramer
from guardrail_core.streaming.window import (
    StreamingBufferWindow,
    WindowEmitResult,
    WindowScanner,
)

from ext_proc.policy import GuardrailPolicyConfig


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
        sse_output: bool = False,
    ) -> None:
        """Set holdback in code points; sse_output selects rebuilt SSE over text."""
        self._framer: SSEFramer | None = SSEFramer(max_event_bytes=max_event_bytes)
        self._window: StreamingBufferWindow | None = StreamingBufferWindow(
            scanner, holdback_len=holdback_len
        )
        self._done_seen = False
        self._blocked = False
        self._sse_output = sse_output
        self._finish_event: dict[str, object] | None = None

    @classmethod
    def from_policy(
        cls,
        policy: GuardrailPolicyConfig,
        *,
        prompt: str = "",
        holdback_len: int | None = None,
        max_event_bytes: int = 1024 * 1024,
        sse_output: bool = False,
    ) -> "GuardrailStream":
        """Build fresh core scanners and a correctly sized window for one response."""
        scanner = policy.build_window_scanner(prompt=prompt, holdback_len=holdback_len)
        return cls(
            scanner,
            holdback_len=scanner.holdback_len,
            max_event_bytes=max_event_bytes,
            sse_output=sse_output,
        )

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
        self._finish_event = None

    def _emit_text(self, chunks: tuple[str, ...]) -> tuple[str, ...]:
        if self._sse_output:
            return tuple(build_openai_chat_delta_sse_chunk(text) for text in chunks)
        return chunks

    def _metadata(self, parsed: OpenAIChatChunkParseResult) -> tuple[str, ...]:
        payload = parsed.payload
        assert payload is not None
        envelope: dict[str, object] = {}
        for key in ("id", "object", "created", "model"):
            if key not in payload:
                continue
            value = payload[key]
            if (
                key == "created"
                and (isinstance(value, bool) or not isinstance(value, int))
            ) or (key != "created" and not isinstance(value, str)):
                raise GuardrailStreamError("Invalid OpenAI protocol metadata.")
            envelope[key] = value
        if not parsed.parsed_choices:
            return (
                (build_sse_data_chunk({**envelope, "choices": []}),) if envelope else ()
            )
        choice = parsed.parsed_choices[0]
        assert choice.choice is not None
        if set(choice.choice) - {"index", "delta", "finish_reason"}:
            raise GuardrailStreamError("Unsupported OpenAI choice fields.")
        delta = choice.choice.get("delta") or {}
        if set(delta) - {"role", "content"}:
            raise GuardrailStreamError("Unsupported non-text OpenAI delta.")
        if "role" in delta and delta["role"] != "assistant":
            raise GuardrailStreamError("Unsupported OpenAI role.")
        if self._finish_event is not None:
            raise GuardrailStreamError("Choice received after finish_reason.")
        if choice.finish_reason is not None:
            self._finish_event = {
                **envelope,
                "choices": [
                    {
                        "index": 0,
                        "delta": {},
                        "finish_reason": choice.finish_reason,
                    }
                ],
            }
        metadata_delta = {"role": delta["role"]} if "role" in delta else {}
        if envelope or metadata_delta:
            return (
                build_sse_data_chunk(
                    {
                        **envelope,
                        "choices": [{"index": 0, "delta": metadata_delta}],
                    }
                ),
            )
        return ()

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
                ending: tuple[str, ...] = ()
                if self._sse_output and not tail.blocked:
                    if self._finish_event is not None:
                        ending += (build_sse_data_chunk(self._finish_event),)
                    ending += (build_sse_done_chunk(),)
                result = WindowEmitResult(
                    chunks=()
                    if tail.blocked
                    else (result.chunks + self._emit_text(tail.chunks) + ending),
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
            if self._done_seen and event.data is not None:
                raise GuardrailStreamError("Data received after the DONE event.")
            parsed = parse_openai_chat_sse_event(event)
            if parsed.status == OpenAIChatChunkParseStatus.NO_DATA:
                continue
            if parsed.status == OpenAIChatChunkParseStatus.DONE:
                self._done_seen = True
                continue
            if parsed.status != OpenAIChatChunkParseStatus.PARSED:
                raise GuardrailStreamError(
                    f"Invalid OpenAI stream event: {parsed.status}."
                )
            if parsed.parsed_choices and (
                len(parsed.parsed_choices) != 1
                or parsed.parsed_choices[0].choice_index != 0
            ):
                raise GuardrailStreamError(
                    "Stream requires exactly one choice with index zero."
                )
            if self._sse_output:
                chunks.extend(self._metadata(parsed))
            if not parsed.parsed_choices:
                continue
            content = parsed.parsed_choices[0].content
            if content is not None:
                result = window.feed(content)
                if result.blocked:
                    return WindowEmitResult(chunks=(), blocked=True)
                chunks.extend(self._emit_text(result.chunks))
        return WindowEmitResult(chunks=tuple(chunks))
