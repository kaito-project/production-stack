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
from unittest.mock import Mock, call

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
    assert first == result == WindowEmitResult(chunks=())
    assert not stream.closed
    final = stream.finish()
    if action == "block":
        assert final == WindowEmitResult(chunks=(), blocked=True)
    else:
        assert final == WindowEmitResult(chunks=("safe [REDACTED] end",))
    assert_terminal(stream)


@pytest.mark.parametrize("action", ["block", "redact"])
def test_real_scanner_only_flushes_after_done_and_valid_eof(action):
    stream = GuardrailStream.from_policy(policy(action))
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
    stream = GuardrailStream.from_policy(
        policy(scanner_configs=[{"type": "invisible_text"}])
    )
    stream.feed(event("secret"))
    with pytest.raises(error):
        stream.feed(tail)
        stream.finish()
    real.scan.assert_not_called()
    assert_terminal(stream)


def test_cancellation_discards_without_scanning(monkeypatch):
    real = Mock()
    monkeypatch.setattr(scanner_module, "build_scanner", Mock(return_value=real))
    stream = GuardrailStream.from_policy(
        policy(scanner_configs=[{"type": "invisible_text"}])
    )
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
    text = "secret" if final else "pending " * 40
    stream = GuardrailStream.from_policy(
        policy(scanner_configs=[{"type": "invisible_text"}]), prompt="request prompt"
    )
    if final:
        stream.feed(event(text) + DONE)
    with pytest.raises(asyncio.CancelledError if cancelled else WindowScanError) as exc:
        stream.finish() if final else stream.feed(event(text))
    real.scan.assert_called_once_with("request prompt", text)
    if not cancelled:
        assert exc.value.__cause__ is cause
    assert_terminal(stream)


@pytest.mark.parametrize("blocker_first", [False, True])
def test_redaction_exposes_match_to_blocker_regardless_of_configuration_order(
    blocker_first,
):
    configs = [
        {"type": "invisible_text", "action": "redact"},
        {"type": "ban_substrings", "substrings": ["secret"]},
    ]
    config = policy(
        scanner_configs=list(reversed(configs)) if blocker_first else configs
    )
    stream = GuardrailStream.from_policy(config)
    assert stream.feed(event("se\u200bcret") + DONE) == WindowEmitResult(chunks=())
    assert stream.finish() == WindowEmitResult(chunks=(), blocked=True)
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
    assert first.feed(event("cret") + DONE) == WindowEmitResult(chunks=())
    assert first.finish().blocked
    assert_terminal(first)
    assert_terminal(second)


@pytest.mark.parametrize(
    "scanner_config",
    [
        {"type": "json"},
        {"type": "regex", "patterns": ["secret"]},
        {"type": "reading_time", "max_time": 1},
        {"type": "token_limit", "limit": 10},
        {"type": "ban_substrings", "substrings": ["secret"], "contains_all": True},
        {"type": "secrets", "action": "redact", "redact_mode": "partial"},
    ],
)
def test_unsupported_streaming_settings_remain_available_for_buffered_scanning(
    scanner_config,
):
    buffered = policy(scanner_configs=[scanner_config])
    assert len(buffered.build_scanners()) == 1
    with pytest.raises(StreamingPolicyError, match=r"scanner_configs\[0\]"):
        GuardrailStream.from_policy(buffered)


@pytest.mark.parametrize("holdback_len", [0, 100, 255])
def test_holdback_below_released_core_minimum_is_rejected(holdback_len):
    with pytest.raises(StreamingPolicyError, match="at least 256"):
        GuardrailStream.from_policy(policy(), holdback_len=holdback_len)


def test_construction_failure_propagates_before_stream_creation(monkeypatch):
    cause = RuntimeError("scanner construction failed")
    monkeypatch.setattr(scanner_module, "build_scanner", Mock(side_effect=cause))
    with pytest.raises(RuntimeError) as exc:
        GuardrailStream.from_policy(policy())
    assert exc.value is cause


def test_safe_output_is_progressively_released_with_derived_holdback():
    config = policy()
    assert config.build_window_scanner().holdback_len == 256
    stream = GuardrailStream.from_policy(config)
    text = "\u4f60" * 256
    assert stream.feed(event(text)) == WindowEmitResult(chunks=())
    assert stream.feed(event("!")) == WindowEmitResult(chunks=("\u4f60",))
    assert stream.feed(DONE) == WindowEmitResult(chunks=())
    assert not stream.closed
    assert stream.finish() == WindowEmitResult(chunks=(text[1:] + "!",))
    assert_terminal(stream)


def test_block_discards_approved_chunks_in_the_same_batch():
    stream = GuardrailStream.from_policy(policy())
    assert stream.feed(event("hello " * 50) + event("secret!") + DONE) == (
        WindowEmitResult(chunks=(), blocked=True)
    )
    assert_terminal(stream)


def test_non_secrets_redaction_does_not_require_the_old_fixed_point_contract():
    config = policy(
        "redact",
        [{"type": "ban_substrings", "substrings": ["REDACTED"], "match_type": "str"}],
    )
    stream = GuardrailStream.from_policy(config)
    assert stream.feed(event("REDACTED") + DONE) == WindowEmitResult(chunks=())
    assert stream.finish() == WindowEmitResult(chunks=("[REDACTED]",))
    assert_terminal(stream)


def test_policy_factory_preserves_event_size_bound():
    stream = GuardrailStream.from_policy(policy(), max_event_bytes=100)
    stream.feed(event("pending"))
    with pytest.raises(SSEEventTooLargeError):
        stream.feed(b"data: " + b"x" * 100)
    assert_terminal(stream)


@pytest.mark.parametrize("action", ["block", "redact"])
@pytest.mark.parametrize("suffix", ["", "!", "ary"])
def test_default_word_mode_waits_for_right_boundary_or_eof(action, suffix):
    prefix = "safe " * 60
    stream = GuardrailStream.from_policy(policy(action))
    first = stream.feed(event(prefix + "secret"))
    assert not first.blocked
    assert prefix.startswith("".join(first.chunks))
    assert not stream.closed
    following = stream.feed(event(suffix) + DONE)
    chunks = first.chunks + following.chunks
    if not stream.closed:
        assert not following.blocked
        final = stream.finish()
        chunks += final.chunks
    if action == "block" and suffix != "ary":
        assert stream.blocked
        assert prefix.startswith("".join(chunks))
    else:
        expected = "secretary" if suffix == "ary" else "[REDACTED]" + suffix
        assert "".join(chunks) == prefix + expected
        assert not stream.blocked
    assert_terminal(stream)


@pytest.mark.parametrize("action", ["block", "redact"])
@pytest.mark.parametrize("preceding_char", ["x", "_", "\u4f60"])
def test_word_left_boundary_uses_previously_released_character(action, preceding_char):
    text = preceding_char + "secret" + " " * 250
    stream = GuardrailStream.from_policy(policy(action))
    first = stream.feed(event(text))
    assert first == WindowEmitResult(chunks=(preceding_char,))
    second = stream.feed(event("!") + DONE)
    final = stream.finish()
    assert "".join(first.chunks + second.chunks + final.chunks) == text + "!"
    assert not stream.blocked
    assert_terminal(stream)


@pytest.mark.parametrize("match_type,minimum", [("word", 300), ("str", 299)])
def test_long_substring_requires_more_than_default_holdback(match_type, minimum):
    config = policy(
        scanner_configs=[
            {
                "type": "ban_substrings",
                "substrings": ["x" * 300],
                "match_type": match_type,
            }
        ]
    )
    assert config.build_window_scanner().holdback_len == minimum
    with pytest.raises(StreamingPolicyError, match=f"at least {minimum}"):
        GuardrailStream.from_policy(config, holdback_len=minimum - 1)


@pytest.mark.parametrize("blocker_first", [False, True])
def test_redactor_removes_match_before_blocker_even_if_blocker_is_configured_first(
    blocker_first,
):
    configs = [
        {"type": "ban_substrings", "substrings": ["secret"], "action": "redact"},
        {"type": "ban_substrings", "substrings": ["secret"], "action": "block"},
    ]
    config = policy(
        scanner_configs=list(reversed(configs)) if blocker_first else configs
    )
    stream = GuardrailStream.from_policy(config)
    assert stream.feed(event("a secret") + DONE) == WindowEmitResult(chunks=())
    assert stream.finish() == WindowEmitResult(chunks=("a [REDACTED]",))
    assert not stream.blocked
    assert_terminal(stream)


def test_real_secrets_redaction_is_rechecked_before_release(monkeypatch):
    config = policy("redact", [{"type": "secrets", "redact_mode": "all"}])
    real = config.build_scanners()[0]
    scan = Mock(wraps=real.scan)
    monkeypatch.setattr(real, "scan", scan)
    monkeypatch.setattr(scanner_module, "build_scanner", Mock(return_value=real))
    text = 'key = "' + "AKIA" + "Z" * 16 + '"'
    stream = GuardrailStream.from_policy(config, prompt="request prompt")
    assert stream.feed(event(text[:15])) == WindowEmitResult(chunks=())
    assert stream.feed(event(text[15:]) + DONE) == WindowEmitResult(chunks=())
    assert stream.finish() == WindowEmitResult(chunks=('key = "******"',))
    assert scan.call_args_list == [
        call("request prompt", text),
        call("request prompt", 'key = "******"'),
    ]
    assert_terminal(stream)


@pytest.mark.parametrize("valid,changed", [(False, False), (True, True)])
def test_secrets_failed_recheck_blocks_instead_of_releasing(
    monkeypatch, valid, changed
):
    config = policy("redact", [{"type": "secrets", "redact_mode": "all"}])
    real = config.build_scanners()[0]
    text = 'key = "' + "AKIA" + "Z" * 16 + '"'
    detected = real.scan("", text)
    assert not detected.valid
    checked_output = detected.output + "changed" if changed else detected.output
    scan = Mock(side_effect=[detected, ScanResult(checked_output, valid, 1.0)])
    monkeypatch.setattr(real, "scan", scan)
    monkeypatch.setattr(scanner_module, "build_scanner", Mock(return_value=real))
    stream = GuardrailStream.from_policy(config)
    assert stream.feed(event(text) + DONE) == WindowEmitResult(chunks=())
    assert stream.finish() == WindowEmitResult(chunks=(), blocked=True)
    assert scan.call_args_list == [call("", text), call("", detected.output)]
    assert_terminal(stream)


@pytest.mark.parametrize("action", ["block", "redact"])
@pytest.mark.parametrize("split", range(1, len("alice@example.io")))
def test_sensitive_detects_email_across_events_before_eof(action, split):
    prefix = "safe " * 52
    # A two-letter TLD cannot already be a valid shorter email in the first event.
    email = "alice@example.io"
    stream = GuardrailStream.from_policy(
        policy(action, [{"type": "sensitive", "detectors": ["email"]}])
    )
    first = stream.feed(event(prefix + email[:split]))
    assert not first.blocked
    assert prefix.startswith("".join(first.chunks))
    following = stream.feed(event(email[split:] + " end") + DONE)
    if action == "block":
        assert following == WindowEmitResult(chunks=(), blocked=True)
        assert stream.blocked
    else:
        final = stream.finish()
        assert "".join(first.chunks + following.chunks + final.chunks) == (
            prefix + "<EMAIL> end"
        )
        assert not stream.blocked
    assert_terminal(stream)


@pytest.mark.parametrize("action", ["block", "redact"])
@pytest.mark.parametrize("split", range(1, len("alice@example.com")))
def test_short_sensitive_content_is_scanned_as_complete_text_at_eof(action, split):
    email = "alice@example.com"
    stream = GuardrailStream.from_policy(
        policy(action, [{"type": "sensitive", "detectors": ["email"]}])
    )
    assert stream.feed(event(email[:split])) == WindowEmitResult(chunks=())
    assert stream.feed(event(email[split:]) + DONE) == WindowEmitResult(chunks=())
    assert stream.finish() == (
        WindowEmitResult(chunks=(), blocked=True)
        if action == "block"
        else WindowEmitResult(chunks=("<EMAIL>",))
    )
    assert_terminal(stream)
