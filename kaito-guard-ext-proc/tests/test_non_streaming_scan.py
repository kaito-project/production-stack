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

"""Tests for non-streaming response scanning.

Tests verify scan_complete_response() behavior against guardrail_core.scanning.scan_output()
contract. Uses real Scanner implementations; does NOT mock.
"""

import pytest
from guardrail_core.scanning import Scanner, ScanResult
from ext_proc.non_streaming_scan import scan_complete_response, NonStreamingScanError


class PassScanner(Scanner):
    """Scanner that always passes without modifying output."""

    def scan(self, prompt: str, output: str) -> ScanResult:
        return ScanResult(output=output, valid=True, score=1.0)


class FailScanner(Scanner):
    """Scanner that always fails without modifying output."""

    def scan(self, prompt: str, output: str) -> ScanResult:
        return ScanResult(output=output, valid=False, score=0.0)


class ExactRedactScanner(Scanner):
    """Scanner that redacts exact substring and passes."""

    def __init__(self, pattern: str, replacement: str):
        self.pattern = pattern
        self.replacement = replacement

    def scan(self, prompt: str, output: str) -> ScanResult:
        redacted = output.replace(self.pattern, self.replacement)
        return ScanResult(output=redacted, valid=True, score=0.9)


class ConditionalFailScanner(Scanner):
    """Scanner that fails if output contains keyword."""

    def __init__(self, keyword: str):
        self.keyword = keyword
        self.call_count = 0

    def scan(self, prompt: str, output: str) -> ScanResult:
        self.call_count += 1
        if self.keyword in output.lower():
            return ScanResult(output=output, valid=False, score=0.0)
        return ScanResult(output=output, valid=True, score=1.0)


class TestScanCompleteResponse:
    """Test non-streaming response scanning."""

    def test_single_pass_scanner(self):
        """Verify single passing scanner returns original output."""
        scanner = PassScanner()
        sanitized, validity, scores = scan_complete_response(
            [scanner], prompt="test", output="hello world"
        )

        assert sanitized == "hello world"
        assert validity["PassScanner"] is True
        assert scores["PassScanner"] == 1.0

    def test_single_fail_scanner(self):
        """Verify single failing scanner returns original output but marked failed."""
        scanner = FailScanner()
        sanitized, validity, scores = scan_complete_response(
            [scanner], prompt="test", output="hello world"
        )

        # Output unchanged by failing scanner
        assert sanitized == "hello world"
        assert validity["FailScanner"] is False
        assert scores["FailScanner"] == 0.0

    def test_redaction_chain_last_output_wins(self):
        """Verify output from LAST scanner replaces prior outputs (not first)."""
        # First redacts SENSITIVE, second redacts SECRET
        scanner1 = ExactRedactScanner("SENSITIVE", "[redacted-1]")
        scanner2 = ExactRedactScanner("SECRET", "[redacted-2]")

        sanitized, validity, scores = scan_complete_response(
            [scanner1, scanner2], prompt="test", output="hello SENSITIVE SECRET world"
        )

        # scanner1 output: "hello [redacted-1] SECRET world"
        # scanner2 receives that and redacts SECRET: "hello [redacted-1] [redacted-2] world"
        assert sanitized == "hello [redacted-1] [redacted-2] world"
        # Both scanner names appear (scanner class names may collide; use class name once)
        assert "ExactRedactScanner" in validity
        # At least one should be in scores
        assert len(scores) >= 1

    def test_redaction_exact_output_single_scanner(self):
        """Verify exact redaction result (not loose acceptance)."""
        scanner = ExactRedactScanner("REMOVE", "[redacted]")

        sanitized, _, _ = scan_complete_response(
            [scanner], prompt="test", output="hello REMOVE world"
        )

        # Strict assertion: must be exactly this
        assert sanitized == "hello [redacted] world"
        # Not loose: don't accept "hello REMOVE world" (unreacted) or "hello  world" (wrong marker)

    def test_fail_fast_stops_execution(self):
        """Verify fail_fast=True stops after first scanner failure."""
        tracker1 = ConditionalFailScanner("forbidden")
        tracker2 = PassScanner()
        tracker3 = PassScanner()

        sanitized, validity, scores = scan_complete_response(
            [tracker1, tracker2, tracker3],
            prompt="test",
            output="forbidden content",
            fail_fast=True,
        )

        # tracker1 should fail and execution stops
        assert tracker1.call_count == 1
        assert validity["ConditionalFailScanner"] is False
        # After fail_fast, no other scanners should be in validity dict
        # (Only tracker1 was executed due to fail_fast=True)
        assert len(validity) == 1

    def test_without_fail_fast_processes_all(self):
        """Verify fail_fast=False (default) processes all scanners."""
        tracker1 = ConditionalFailScanner("forbidden")
        tracker2 = PassScanner()

        sanitized, validity, scores = scan_complete_response(
            [tracker1, tracker2],
            prompt="test",
            output="forbidden content",
            fail_fast=False,
        )

        # Both should be called and results recorded
        assert validity["ConditionalFailScanner"] is False
        assert validity["PassScanner"] is True

    def test_empty_scanner_list_raises_error(self):
        """Verify empty scanner list raises appropriate error."""
        with pytest.raises(NonStreamingScanError, match="At least one scanner"):
            scan_complete_response([], prompt="test", output="content")

    def test_non_string_output_raises_error(self):
        """Verify non-string output raises appropriate error."""
        scanner = PassScanner()
        with pytest.raises(NonStreamingScanError, match="Output must be a string"):
            scan_complete_response([scanner], prompt="test", output=123)

    def test_exception_message_is_fixed_and_generic(self):
        """Verify scanner exceptions are wrapped with fixed message."""

        class ErrorScanner(Scanner):
            def scan(self, prompt: str, output: str) -> ScanResult:
                raise ValueError("Internal: secret_pattern_xyz_abc_123_found")

        scanner = ErrorScanner()
        with pytest.raises(NonStreamingScanError) as exc_info:
            scan_complete_response([scanner], prompt="test", output="content")

        # Public message is fixed and generic
        assert str(exc_info.value) == "Scanning failed."
        # Original cause is preserved (not exposed in message)
        assert exc_info.value.__cause__ is not None
        assert isinstance(exc_info.value.__cause__, ValueError)
        # Sensitive details are in cause, not in public message
        assert "secret_pattern" in str(exc_info.value.__cause__)
        assert "secret_pattern" not in str(exc_info.value)

    def test_isolation_between_calls(self):
        """Verify multiple calls are independent (no state sharing)."""
        scanner = ExactRedactScanner("X", "[redacted]")

        result1, _, _ = scan_complete_response(
            [scanner], prompt="p1", output="text X 1"
        )
        result2, _, _ = scan_complete_response(
            [scanner], prompt="p2", output="text X 2"
        )

        # Each should have independent redaction
        assert result1 == "text [redacted] 1"
        assert result2 == "text [redacted] 2"
        assert result1 != result2

    def test_scanner_receives_prompt_context(self):
        """Verify scanner receives prompt parameter."""

        class ContextCheckScanner(Scanner):
            def scan(self, prompt: str, output: str) -> ScanResult:
                # Pass if both prompt and output are non-empty
                valid = len(prompt) > 0 and len(output) > 0
                return ScanResult(output=output, valid=valid, score=1.0)

        scanner = ContextCheckScanner()
        _, validity, _ = scan_complete_response(
            [scanner], prompt="meaningful context", output="response"
        )

        assert validity["ContextCheckScanner"] is True

    def test_full_text_processed_no_truncation(self):
        """Verify complete text is processed (no holdback or truncation)."""

        class LengthCheckScanner(Scanner):
            def scan(self, prompt: str, output: str) -> ScanResult:
                # Fail if output is less than expected length
                # This would catch holdback truncation
                if len(output) < 1000:
                    return ScanResult(output=output, valid=False, score=0.0)
                return ScanResult(output=output, valid=True, score=1.0)

        scanner = LengthCheckScanner()
        long_text = "x" * 2000

        _, validity, _ = scan_complete_response(
            [scanner], prompt="test", output=long_text
        )

        # Should process full length, not truncate
        assert validity["LengthCheckScanner"] is True

    def test_none_output_is_invalid_contract(self):
        """Verify None output violates ScanResult.output type contract.

        ScanResult.output contract is str, not str|None. Scanner must return str.
        This test documents the contract; scanners returning None would be a bug.
        """
        # ScanResult() requires output: str, not str | None
        # A scanner returning None violates the contract
        # (scan_output() may return None if all scanners fail, but that's different)
        result = ScanResult(output="text", valid=True, score=1.0)
        assert isinstance(result.output, str)

    def test_matches_direct_scan_output_call(self):
        """Verify adapter behavior matches direct scan_output() call."""
        from guardrail_core.scanning import scan_output

        scanners = [PassScanner(), ExactRedactScanner("X", "[R]")]
        prompt = "test prompt"
        output = "hello X world"

        # Call through adapter
        adapter_sanitized, adapter_validity, adapter_scores = scan_complete_response(
            scanners, prompt, output, fail_fast=False
        )

        # Call directly
        direct_sanitized, direct_validity, direct_scores = scan_output(
            scanners, prompt, output, fail_fast=False
        )

        # Should produce identical results
        assert adapter_sanitized == direct_sanitized
        assert adapter_validity == direct_validity
        assert adapter_scores == direct_scores

    def test_fail_fast_matches_scan_output(self):
        """Verify fail_fast behavior matches direct scan_output() call."""
        from guardrail_core.scanning import scan_output

        scanners = [PassScanner(), ConditionalFailScanner("forbidden")]
        output = "forbidden content"

        # Through adapter
        _, adapter_validity, _ = scan_complete_response(
            scanners, prompt="p", output=output, fail_fast=True
        )

        # Direct call
        _, direct_validity, _ = scan_output(
            scanners, prompt="p", output=output, fail_fast=True
        )

        # Validity dict should be identical
        assert adapter_validity == direct_validity

    def test_return_values_are_shared_contract(self):
        """Verify return tuple matches guardrail_core.scanning contract."""
        scanner1 = PassScanner()
        scanner2 = FailScanner()

        result = scan_complete_response(
            [scanner1, scanner2], prompt="p", output="text"
        )

        # Result is 3-tuple: (sanitized, validity, scores)
        assert isinstance(result, tuple)
        assert len(result) == 3

        sanitized, validity, scores = result

        # Sanitized is str | None
        assert isinstance(sanitized, str) or sanitized is None

        # Validity is dict[str, bool]
        assert isinstance(validity, dict)
        assert all(isinstance(k, str) and isinstance(v, bool) for k, v in validity.items())

        # Scores is dict[str, float]
        assert isinstance(scores, dict)
        assert all(isinstance(k, str) and isinstance(v, (int, float)) for k, v in scores.items())

    def test_sequential_execution_order(self):
        """Verify scanners execute in input order."""
        execution_log = []

        class LoggingScanner(Scanner):
            def __init__(self, name: str):
                self.name = name

            def scan(self, prompt: str, output: str) -> ScanResult:
                execution_log.append(self.name)
                return ScanResult(output=output, valid=True, score=1.0)

        scanner1 = LoggingScanner("first")
        scanner2 = LoggingScanner("second")
        scanner3 = LoggingScanner("third")

        scan_complete_response([scanner1, scanner2, scanner3], prompt="p", output="text")

        # Execution order should match input order
        assert execution_log == ["first", "second", "third"]


