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

import asyncio
from collections.abc import Iterator
from itertools import product

import pytest
from guardrail_core.streaming_window import (
    StreamingBufferWindow,
    WindowEmitResult,
    WindowScanError,
    WindowScanResult,
)

from ext_proc.streaming_window import StreamingResponseWindow


class TestScanner:
    __test__ = False

    def __init__(
        self,
        *,
        block: bool = False,
        error: BaseException | None = None,
        fail_on_flush: bool = False,
    ) -> None:
        self.block = block
        self.error = error
        self.fail_on_flush = fail_on_flush
        self.calls: list[tuple[str, bool, str]] = []

    def scan(
        self, text: str, *, flush: bool = False, preceding_char: str = ""
    ) -> WindowScanResult:
        self.calls.append((text, flush, preceding_char))
        if self.error is not None and (flush or not self.fail_on_flush):
            raise self.error
        if "secret" in text:
            if self.block:
                return WindowScanResult(blocked=True)
            return WindowScanResult(sanitized_text=text.replace("secret", "[X]"))
        return WindowScanResult()


def partitions(text: str) -> Iterator[tuple[str, ...]]:
    for cuts in product((False, True), repeat=len(text) - 1):
        start = 0
        parts = []
        for index, cut in enumerate(cuts, start=1):
            if cut:
                parts.append(text[start:index])
                start = index
        parts.append(text[start:])
        yield tuple(parts)


@pytest.mark.parametrize("parts", list(partitions("secret")))
@pytest.mark.parametrize("block", [False, True])
def test_cross_feed_match_and_shared_window_equivalence(parts, block):
    scanner = TestScanner(block=block)
    direct_scanner = TestScanner(block=block)
    response = StreamingResponseWindow(scanner, holdback_len=6)
    direct = StreamingBufferWindow(direct_scanner, holdback_len=6)
    emitted = []
    for text in ("ok ", *parts, "!"):
        result = response.feed(text)
        assert result == direct.feed(text)
        assert isinstance(result, WindowEmitResult)
        emitted.extend(result.chunks)
        assert response.redacted == direct.redacted
        assert response.blocked == direct.blocked
        if result.blocked:
            assert response.closed
            break
    else:
        result = response.finish()
        assert result == direct.flush()
        emitted.extend(result.chunks)

    assert scanner.calls == direct_scanner.calls
    if block:
        assert "secret" not in "".join(emitted)
        assert "ok ".startswith("".join(emitted))
    else:
        assert "".join(emitted) == "ok [X]!"
        assert response.redacted
    assert response.closed


@pytest.mark.parametrize("holdback", [0, 1, 5, 100])
@pytest.mark.parametrize("parts", [("",), ("a", "bc", "def"), ("你", "好", "世界")])
def test_normal_and_unicode_input_match_direct_window(parts, holdback):
    scanner = TestScanner()
    direct_scanner = TestScanner()
    response = StreamingResponseWindow(scanner, holdback_len=holdback)
    direct = StreamingBufferWindow(direct_scanner, holdback_len=holdback)
    emitted = []
    for text in parts:
        result = response.feed(text)
        assert result == direct.feed(text)
        emitted.extend(result.chunks)
    result = response.finish()
    assert result == direct.flush()
    emitted.extend(result.chunks)
    assert "".join(emitted) == "".join(parts)
    assert scanner.calls == direct_scanner.calls
    assert response.closed
    assert not response.redacted
    assert not response.blocked


def test_preceding_character_is_managed_by_shared_window():
    scanner = TestScanner()
    response = StreamingResponseWindow(scanner, holdback_len=2)
    assert response.feed("abc").chunks == ("a",)
    assert response.feed("de").chunks == ("bc",)
    assert response.finish().chunks == ("de",)
    assert scanner.calls == [
        ("abc", False, ""),
        ("bcde", False, "a"),
        ("de", True, "c"),
    ]


@pytest.mark.parametrize("block", [False, True])
def test_final_scan_can_redact_or_block(block):
    scanner = TestScanner(block=block)
    response = StreamingResponseWindow(scanner, holdback_len=100)
    assert response.feed("secret").chunks == ()
    assert scanner.calls == []
    result = response.finish()
    assert result == WindowEmitResult(chunks=() if block else ("[X]",), blocked=block)
    assert response.blocked == block
    assert response.redacted == (not block)
    assert response.closed
    assert scanner.calls == [("secret", True, "")]


def test_finish_is_terminal_and_never_flushes_twice():
    scanner = TestScanner()
    response = StreamingResponseWindow(scanner, holdback_len=100)
    response.feed("retained")
    assert response.finish().chunks == ("retained",)
    with pytest.raises(RuntimeError, match="closed"):
        response.finish()
    with pytest.raises(RuntimeError, match="closed"):
        response.feed("later")
    response.abort()
    response.abort()
    assert scanner.calls == [("retained", True, "")]


@pytest.mark.parametrize(
    "upstream_error", [ValueError("parse error"), EOFError(), asyncio.CancelledError()]
)
def test_upstream_failure_aborts_without_scanning_or_releasing_tail(upstream_error):
    scanner = TestScanner()
    response = StreamingResponseWindow(scanner, holdback_len=100)
    assert response.feed("unchecked tail").chunks == ()
    with pytest.raises(type(upstream_error)):
        try:
            raise upstream_error
        finally:
            response.abort()
    response.abort()
    assert response.closed
    with pytest.raises(RuntimeError, match="closed"):
        response.feed("later")
    with pytest.raises(RuntimeError, match="closed"):
        response.finish()
    assert scanner.calls == []


@pytest.mark.parametrize("flush", [False, True])
@pytest.mark.parametrize("cancel", [False, True])
def test_scanner_failure_or_cancellation_is_terminal(flush, cancel, caplog):
    error = asyncio.CancelledError() if cancel else ValueError("private scanner text")
    scanner = TestScanner(error=error, fail_on_flush=flush)
    response = StreamingResponseWindow(scanner, holdback_len=4)
    assert response.feed("tail").chunks == ()
    expected = asyncio.CancelledError if cancel else WindowScanError
    with pytest.raises(expected) as caught:
        if flush:
            response.finish()
        else:
            response.feed("!")
    if cancel:
        assert caught.value is error
    else:
        assert caught.value.__cause__ is error
        assert response.blocked
        assert "private scanner text" not in str(caught.value)
    assert response.closed
    calls = list(scanner.calls)
    with pytest.raises(RuntimeError, match="closed"):
        response.feed("later")
    with pytest.raises(RuntimeError, match="closed"):
        response.finish()
    assert scanner.calls == calls
    assert not caplog.records


def test_block_prevents_further_input_or_flush():
    scanner = TestScanner(block=True)
    response = StreamingResponseWindow(scanner, holdback_len=6)
    assert response.feed("secret").chunks == ()
    assert response.feed("!").blocked
    assert response.blocked
    assert response.closed
    with pytest.raises(RuntimeError, match="closed"):
        response.feed("later")
    with pytest.raises(RuntimeError, match="closed"):
        response.finish()
    assert scanner.calls == [("secret!", False, "")]


@pytest.mark.parametrize("termination", ["finish", "abort", "block", "error"])
def test_response_state_is_not_shared(termination):
    first_scanner = TestScanner(block=termination == "block")
    first = StreamingResponseWindow(first_scanner, holdback_len=6)
    second_scanner = TestScanner()
    second = StreamingResponseWindow(second_scanner, holdback_len=6)
    first.feed("sec")
    second.feed("ret")
    if termination == "finish":
        assert first.finish().chunks == ("sec",)
    elif termination == "abort":
        first.abort()
    elif termination == "block":
        assert first.feed("ret!").blocked
    else:
        first_scanner.error = ValueError("scan failure")
        with pytest.raises(WindowScanError):
            first.feed("ret!")
    assert second.finish().chunks == ("ret",)
    assert not second.blocked
    assert not second.redacted
    assert second_scanner.calls == [("ret", True, "")]


def test_invalid_holdback_uses_shared_validation():
    with pytest.raises(ValueError, match="non-negative"):
        StreamingResponseWindow(TestScanner(), holdback_len=-1)
