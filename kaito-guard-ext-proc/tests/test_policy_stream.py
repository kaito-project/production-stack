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
from unittest.mock import Mock

import pytest
from guardrail_core.scanning import ScanResult
from guardrail_core.streaming import scanners as scanner_module
from guardrail_core.streaming.openai import (
    build_openai_chat_delta_sse_chunk,
    build_sse_done_chunk,
)
from guardrail_core.streaming.scanners import StreamingPolicyError
from guardrail_core.streaming.sse import SSEEventTooLargeError, SSEIncompleteEventError
from guardrail_core.streaming.window import WindowEmitResult, WindowScanError

from ext_proc.guardrail_stream import GuardrailStream, GuardrailStreamError
from ext_proc.policy import GuardrailPolicyConfig


DONE = build_sse_done_chunk().encode()


def event(text):
    return build_openai_chat_delta_sse_chunk(text).encode()


def policy(action="block", scanner_configs=None):
    return GuardrailPolicyConfig.from_dict(
        {
            "action": action,
            "scanner_configs": scanner_configs
            if scanner_configs is not None
            else [
                {
                    "type": "ban_substrings",
                    "substrings": ["secret"],
                    "match_type": "str",
                },
            ],
        }
    )


def assert_terminal(stream):
    assert stream.closed
    with pytest.raises(RuntimeError, match="closed"):
        stream.feed(event("late"))
    with pytest.raises(RuntimeError, match="closed"):
        stream.finish()
    stream.cancel()


@pytest.mark.parametrize("action", ["block", "redact"])
def test_policy_real_scanner_at_every_wire_boundary(action):
    text = "safe secret end"
    wire = event(text) + DONE
    for offset in range(len(wire) + 1):
        stream = GuardrailStream.from_policy(policy(action))
        chunks = []
        for part in (wire[:offset], wire[offset:]):
            result = stream.feed(part)
            chunks.extend(result.chunks)
            if result.blocked:
                break
        if not stream.closed:
            result = stream.finish()
            chunks.extend(result.chunks)
        if action == "block":
            assert stream.blocked
            assert not chunks
        else:
            assert "".join(chunks) == "safe [REDACTED] end"
            assert not stream.blocked
        assert_terminal(stream)


@pytest.mark.parametrize("action", ["block", "redact"])
@pytest.mark.parametrize("split", range(1, len("secret")))
def test_policy_cross_event_match(action, split):
    stream = GuardrailStream.from_policy(policy(action))
    first = stream.feed(event("safe " + "secret"[:split]))
    result = stream.feed(event("secret"[split:] + " end") + DONE)
    if action == "block":
        assert result == WindowEmitResult(chunks=(), blocked=True)
        assert "safe ".startswith("".join(first.chunks))
    else:
        final = stream.finish()
        assert "".join(first.chunks + result.chunks + final.chunks) == (
            "safe [REDACTED] end"
        )
    assert_terminal(stream)


@pytest.mark.parametrize("action", ["block", "redact"])
def test_real_scanner_only_flushes_after_done_and_valid_eof(action):
    stream = GuardrailStream.from_policy(policy(action), holdback_len=100)
    assert stream.feed(event("secret") + DONE) == WindowEmitResult(chunks=())
    assert not stream.closed
    assert stream.finish() == (
        WindowEmitResult(chunks=(), blocked=True)
        if action == "block"
        else WindowEmitResult(chunks=("[REDACTED]",))
    )
    assert_terminal(stream)


@pytest.mark.parametrize(
    "tail,error",
    [
        (b"", GuardrailStreamError),
        (DONE + b"data: unfinished", SSEIncompleteEventError),
        (b"data: {bad}\n\n", GuardrailStreamError),
        (DONE + event("late"), GuardrailStreamError),
    ],
)
def test_failure_discards_pending_without_scanning(monkeypatch, tail, error):
    real = Mock()
    real.scan.return_value = ScanResult("secret", False, 1.0)
    monkeypatch.setattr(scanner_module, "build_scanner", Mock(return_value=real))
    stream = GuardrailStream.from_policy(policy(), holdback_len=100)
    stream.feed(event("secret"))
    with pytest.raises(error):
        stream.feed(tail)
        stream.finish()
    real.scan.assert_not_called()
    assert_terminal(stream)


def test_cancellation_discards_without_scanning(monkeypatch):
    real = Mock()
    monkeypatch.setattr(scanner_module, "build_scanner", Mock(return_value=real))
    stream = GuardrailStream.from_policy(policy(), holdback_len=100)
    stream.feed(event("secret") + DONE)
    stream.cancel()
    real.scan.assert_not_called()
    assert_terminal(stream)


@pytest.mark.parametrize("final", [False, True])
@pytest.mark.parametrize("cancelled", [False, True])
def test_scanner_failure_is_terminal_and_preserves_cause(monkeypatch, final, cancelled):
    cause = asyncio.CancelledError() if cancelled else RuntimeError("scanner failed")
    real = Mock()
    real.scan.side_effect = cause
    monkeypatch.setattr(scanner_module, "build_scanner", Mock(return_value=real))
    stream = GuardrailStream.from_policy(
        policy(), prompt="request prompt", holdback_len=100 if final else None
    )
    if final:
        stream.feed(event("secret") + DONE)
    with pytest.raises(asyncio.CancelledError if cancelled else WindowScanError) as exc:
        stream.finish() if final else stream.feed(event("secret"))
    real.scan.assert_called_once_with("request prompt", "secret")
    if not cancelled:
        assert exc.value.__cause__ is cause
    assert_terminal(stream)


def test_per_scanner_overrides_and_order():
    config = policy(
        scanner_configs=[
            {"type": "invisible_text", "action": "redact"},
            {"type": "ban_substrings", "substrings": ["secret"], "match_type": "str"},
        ]
    )
    stream = GuardrailStream.from_policy(config)
    assert stream.feed(event("se\u200bcret")) == WindowEmitResult(
        chunks=(), blocked=True
    )
    assert_terminal(stream)


def test_empty_sanitization_is_not_treated_as_unchanged():
    stream = GuardrailStream.from_policy(policy("redact", [{"type": "invisible_text"}]))
    assert stream.feed(event("\u200b") + DONE) == WindowEmitResult(chunks=())
    assert stream.finish() == WindowEmitResult(chunks=())
    assert_terminal(stream)


def test_policy_factory_creates_isolated_scanners(monkeypatch):
    builder = Mock(wraps=scanner_module.build_scanner)
    monkeypatch.setattr(scanner_module, "build_scanner", builder)
    config = policy()
    first = GuardrailStream.from_policy(config)
    second = GuardrailStream.from_policy(config)
    assert builder.call_count == 2
    first.feed(event("se"))
    second.feed(event("cret") + DONE)
    assert second.finish().chunks == ("cret",)
    assert first.feed(event("cret")).blocked
    assert_terminal(first)
    assert_terminal(second)


def test_unsupported_policy_and_insufficient_holdback_are_rejected():
    buffered = policy(scanner_configs=[{"type": "json"}])
    assert len(buffered.build_scanners()) == 1
    with pytest.raises(StreamingPolicyError, match=r"scanner_configs\[0\]"):
        GuardrailStream.from_policy(buffered)
    with pytest.raises(StreamingPolicyError, match="at least 5"):
        GuardrailStream.from_policy(policy(), holdback_len=4)


def test_construction_failure_propagates_before_stream_creation(monkeypatch):
    cause = RuntimeError("scanner construction failed")
    monkeypatch.setattr(scanner_module, "build_scanner", Mock(side_effect=cause))
    with pytest.raises(RuntimeError) as exc:
        GuardrailStream.from_policy(policy())
    assert exc.value is cause


def test_safe_output_is_progressively_released_with_derived_holdback():
    stream = GuardrailStream.from_policy(policy())
    assert stream.feed(event("hello world") + DONE) == WindowEmitResult(
        chunks=("hello ",)
    )
    assert stream.finish() == WindowEmitResult(chunks=("world",))
    assert_terminal(stream)


def test_block_discards_approved_chunks_in_the_same_batch():
    stream = GuardrailStream.from_policy(policy())
    assert stream.feed(event("hello world") + event("secret") + DONE) == (
        WindowEmitResult(chunks=(), blocked=True)
    )
    assert_terminal(stream)


@pytest.mark.parametrize("final", [False, True])
def test_unstable_real_redaction_is_a_terminal_scanner_error(final):
    config = policy(
        "redact",
        [{"type": "ban_substrings", "substrings": ["REDACTED"], "match_type": "str"}],
    )
    stream = GuardrailStream.from_policy(config, holdback_len=100 if final else None)
    if final:
        stream.feed(event("REDACTED") + DONE)
    with pytest.raises(WindowScanError) as exc:
        stream.finish() if final else stream.feed(event("REDACTED"))
    assert isinstance(exc.value.__cause__, StreamingPolicyError)
    assert_terminal(stream)


def test_policy_factory_preserves_event_size_bound():
    stream = GuardrailStream.from_policy(
        policy(), holdback_len=100, max_event_bytes=100
    )
    stream.feed(event("pending"))
    with pytest.raises(SSEEventTooLargeError):
        stream.feed(b"data: " + b"x" * 100)
    assert_terminal(stream)
