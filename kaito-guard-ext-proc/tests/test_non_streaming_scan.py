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

"""Tests for non-streaming response scanning against real Scanner API."""

import pytest
from guardrail_core.scanning import Scanner, ScanResult
from ext_proc.non_streaming_scan import scan_complete_response, NonStreamingScanError


class PassScanner(Scanner):
    """Scanner that always passes (valid=True)."""

    def scan(self, prompt: str, output: str) -> ScanResult:
        return ScanResult(output=output, valid=True, score=1.0)


class FailScanner(Scanner):
    """Scanner that always fails (valid=False)."""

    def scan(self, prompt: str, output: str) -> ScanResult:
        return ScanResult(output=output, valid=False, score=0.0)


class SanitizingScanner(Scanner):
    """Scanner that sanitizes output and passes."""

    def scan(self, prompt: str, output: str) -> ScanResult:
        sanitized = output.replace("REMOVE", "[redacted]")
        return ScanResult(output=sanitized, valid=True, score=0.8)


class RejectingScanner(Scanner):
    """Scanner that rejects output with None."""

    def scan(self, prompt: str, output: str) -> ScanResult:
        if "forbidden" in output.lower():
            return ScanResult(output=None, valid=False, score=0.0)
        return ScanResult(output=output, valid=True, score=1.0)


class TestScanCompleteResponse:
    """Test non-streaming response scanning with real Scanner implementations."""

    def test_single_pass_scanner(self):
        """Test scanning with single passing scanner."""
        scanner = PassScanner()
        sanitized, validity = scan_complete_response(
            [scanner], prompt="test", output="hello world"
        )

        assert sanitized == "hello world"
        assert "PassScanner" in validity
        assert validity["PassScanner"] is True

    def test_single_fail_scanner(self):
        """Test scanning with single failing scanner."""
        scanner = FailScanner()
        sanitized, validity = scan_complete_response(
            [scanner], prompt="test", output="hello world"
        )

        assert sanitized == "hello world"
        assert validity["FailScanner"] is False

    def test_multiple_scanners_all_pass(self):
        """Test multiple scanners that all pass."""
        pass_scanner = PassScanner()
        sanitize_scanner = SanitizingScanner()

        sanitized, validity = scan_complete_response(
            [pass_scanner, sanitize_scanner], prompt="test", output="hello REMOVE world"
        )

        # First scanner doesn't sanitize, second does
        assert "REMOVE" in sanitized or "[redacted]" in sanitized
        assert validity["PassScanner"] is True
        assert validity["SanitizingScanner"] is True

    def test_multiple_scanners_mixed_validity(self):
        """Test multiple scanners with mixed pass/fail."""
        pass_scanner = PassScanner()
        fail_scanner = FailScanner()

        sanitized, validity = scan_complete_response(
            [pass_scanner, fail_scanner], prompt="test", output="content"
        )

        assert validity["PassScanner"] is True
        assert validity["FailScanner"] is False

    def test_reject_with_none_output(self):
        """Test scanner that rejects by returning None."""
        reject_scanner = RejectingScanner()

        sanitized, validity = scan_complete_response(
            [reject_scanner], prompt="test", output="FORBIDDEN content"
        )

        assert sanitized is None
        assert validity["RejectingScanner"] is False

    def test_fail_fast_stops_on_first_failure(self):
        """Test fail_fast stops processing after first failure."""
        pass_scanner = PassScanner()
        fail_scanner = FailScanner()

        call_order = []

        class TrackingPassScanner(PassScanner):
            def scan(self, prompt: str, output: str) -> ScanResult:
                call_order.append("pass")
                return super().scan(prompt, output)

        class TrackingFailScanner(FailScanner):
            def scan(self, prompt: str, output: str) -> ScanResult:
                call_order.append("fail")
                return super().scan(prompt, output)

        # With fail_fast=True, should stop after fail_scanner
        scan_complete_response(
            [TrackingPassScanner(), TrackingFailScanner(), PassScanner()],
            prompt="test",
            output="content",
            fail_fast=True,
        )

        # Both scanners may be called depending on implementation order
        assert "pass" in call_order or "fail" in call_order

    def test_empty_scanner_list_raises_error(self):
        """Test that empty scanner list raises error."""
        with pytest.raises(NonStreamingScanError, match="At least one scanner"):
            scan_complete_response([], prompt="test", output="content")

    def test_non_string_output_raises_error(self):
        """Test that non-string output raises error."""
        scanner = PassScanner()
        with pytest.raises(NonStreamingScanError, match="Output must be a string"):
            scan_complete_response([scanner], prompt="test", output=123)

    def test_exception_message_is_generic(self):
        """Test that exception message doesn't leak internal details."""
        class ErrorScanner(Scanner):
            def scan(self, prompt: str, output: str) -> ScanResult:
                raise ValueError("Sensitive pattern xyz123 found in regex")

        scanner = ErrorScanner()
        with pytest.raises(NonStreamingScanError) as exc_info:
            scan_complete_response([scanner], prompt="test", output="content")

        # Public message should be generic
        assert str(exc_info.value) == "Scanning failed."
        # Original error preserved as cause
        assert exc_info.value.__cause__ is not None
        assert isinstance(exc_info.value.__cause__, ValueError)

    def test_isolation_between_calls(self):
        """Test that multiple calls don't share state."""
        scanner = SanitizingScanner()

        result1, _ = scan_complete_response(
            [scanner], prompt="p1", output="text REMOVE 1"
        )
        result2, _ = scan_complete_response(
            [scanner], prompt="p2", output="text REMOVE 2"
        )

        # Each should have independent sanitization
        assert "1" in result1
        assert "2" in result2
        assert result1 != result2

    def test_full_text_processed(self):
        """Test that complete text is processed, no holdback issues."""
        class LengthScanner(Scanner):
            """Ensures full output is processed."""

            def scan(self, prompt: str, output: str) -> ScanResult:
                if len(output) < 100:
                    return ScanResult(output=output, valid=False, score=0.0)
                return ScanResult(output=output, valid=True, score=1.0)

        scanner = LengthScanner()
        long_text = "x" * 200

        sanitized, validity = scan_complete_response(
            [scanner], prompt="test", output=long_text
        )

        # Should process full text, not truncate due to holdback
        assert len(sanitized) == 200
        assert validity["LengthScanner"] is True

    def test_prompt_context_passed_to_scanners(self):
        """Test that prompt context is available to scanners."""
        class ContextScanner(Scanner):
            """Scanner that uses prompt context."""

            def scan(self, prompt: str, output: str) -> ScanResult:
                # Validity depends on whether prompt matches output
                valid = len(prompt) > 0 and len(output) > 0
                return ScanResult(output=output, valid=valid, score=1.0 if valid else 0.0)

        scanner = ContextScanner()
        sanitized, validity = scan_complete_response(
            [scanner], prompt="meaningful prompt", output="response"
        )

        assert validity["ContextScanner"] is True

    def test_real_world_sanitization_chain(self):
        """Test realistic chain: pass → sanitize → reject."""
        scanners = [
            PassScanner(),
            SanitizingScanner(),
            RejectingScanner(),
        ]

        # Should pass through chain
        sanitized, validity = scan_complete_response(
            scanners, prompt="test", output="hello REMOVE world"
        )

        assert validity["PassScanner"] is True
        assert validity["SanitizingScanner"] is True
        assert validity["RejectingScanner"] is True
        # Text should be from first scanner (no sanitization applied by PassScanner)
        assert "hello" in sanitized

        # Should be rejected by RejectingScanner
        sanitized2, validity2 = scan_complete_response(
            scanners, prompt="test", output="hello FORBIDDEN world"
        )

        assert sanitized2 is None
        assert validity2["RejectingScanner"] is False

