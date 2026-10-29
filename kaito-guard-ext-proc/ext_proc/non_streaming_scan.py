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

    pass


def scan_complete_response(
    scanners: Sequence[Scanner],
    prompt: str,
    output: str,
    fail_fast: bool = False,
) -> tuple[str | None, dict[str, bool], dict[str, float]]:
    """Scan complete response text through shared guardrail core scanners.

    Delegates to guardrail_core.scanning.scan_output() for sequential execution
    of scanners. Processes complete response in one pass; no state retained
    across calls.

    Important: This adapter calls scanners but does NOT implement policy-level
    block/redact ordering or secrets revalidation. Those are responsibility of
    the calling layer.

    Args:
        scanners: List of Scanner implementations to run sequentially.
        prompt: Original prompt/request context (may be empty for batch mode).
        output: Complete response text to scan.
        fail_fast: Stop on first scanner failure (default: process all).

    Returns:
        Tuple of (sanitized_text, validity_dict, scores_dict):
        - sanitized_text: Output from last executed scanner (or None if all
          return None). Each scanner output replaces prior; final output is
          last scanner's result.
        - validity_dict: {scanner_name: bool} pass/fail for each scanner.
        - scores_dict: {scanner_name: float} confidence scores if available.

    Raises:
        NonStreamingScanError: If no scanners provided or output is not string.
    """
    if not scanners:
        raise NonStreamingScanError("At least one scanner is required.")
    if not isinstance(output, str):
        raise NonStreamingScanError("Output must be a string.")

    try:
        sanitized, validity, scores = scan_output(
            scanners, prompt, output, fail_fast=fail_fast
        )
        return sanitized, validity, scores
    except Exception as e:
        raise NonStreamingScanError("Scanning failed.") from e
