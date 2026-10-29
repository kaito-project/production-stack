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

"""Scan complete response text using shared guardrail core API."""

from collections.abc import Sequence
from guardrail_core.scanning import Scanner, scan_output


class NonStreamingScanError(ValueError):
    """Error during non-streaming response scanning."""

    def __init__(self, message: str) -> None:
        """Initialize with fixed message; preserve original error as cause."""
        super().__init__(message)


def scan_complete_response(
    scanners: Sequence[Scanner],
    prompt: str,
    output: str,
    fail_fast: bool = False,
) -> tuple[str | None, dict[str, bool]]:
    """Scan complete response text through shared guardrail core scanners.

    Delegates to guardrail_core.scanning.scan_output for full-text scanning,
    reusing strategy order and secrets revalidation from core. Processes the
    complete response in one pass; no state is retained across calls.

    Args:
        scanners: List of Scanner implementations to run sequentially.
        prompt: Original prompt/request context (may be empty for batch mode).
        output: Complete response text to scan.
        fail_fast: Stop on first scanner failure (default: process all).

    Returns:
        Tuple of (sanitized_text, validity_dict) where:
        - sanitized_text: Cleaned output from first scanner, or None if no
          scanners or all returned None.
        - validity_dict: {scanner_name: passed} for each scanner run.

    Raises:
        NonStreamingScanError: If no scanners provided or output is not a string.
    """
    if not scanners:
        raise NonStreamingScanError("At least one scanner is required.")
    if not isinstance(output, str):
        raise NonStreamingScanError("Output must be a string.")

    try:
        sanitized, validity, _scores = scan_output(
            scanners, prompt, output, fail_fast=fail_fast
        )
        return sanitized, validity
    except Exception as e:
        raise NonStreamingScanError("Scanning failed.") from e
