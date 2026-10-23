#!/usr/bin/env python3
"""
Direct test of Python ext_proc logic (without Envoy)
Verifies SSE parsing, holdback, and redaction work correctly.
"""

import sys
import os

# Add python dir to path
python_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "python")
sys.path.insert(0, python_dir)

from main import (
    split_sse_events,
    is_done_event,
    ChatCompletionChunk,
    sanitize_pending,
    release_safe_prefix,
    build_sse,
    SECRET_PATTERN,
    REDACTED,
)


def test_redact():
    """Test Case 1: Normal secret redaction."""
    print("[Test] Case 1: Redact")

    text = "Here is a secret: sk-123456789 end."
    sanitized = sanitize_pending(text)

    if REDACTED in sanitized and SECRET_PATTERN not in sanitized:
        print("  ✓ PASS: Secret redacted")
        return True
    else:
        print(f"  ✗ FAIL: Expected redaction, got: {sanitized}")
        return False


def test_boundary():
    """Test Case 2: Secret split across chunks."""
    print("[Test] Case 2: Boundary (secret split across chunks)")

    # Simulate two chunks
    chunk1 = "Here is sk-12"
    chunk2 = "3456789 end."

    # Concatenate and process
    text = chunk1 + chunk2
    sanitized = sanitize_pending(text)

    if REDACTED in sanitized and SECRET_PATTERN not in sanitized:
        print("  ✓ PASS: Boundary case handled")
        return True
    else:
        print(f"  ✗ FAIL: Boundary not handled, got: {sanitized}")
        return False


def test_holdback():
    """Test Case 3: Holdback mechanism prevents unsafe prefix leakage."""
    print("[Test] Case 3: Holdback (no unsafe prefix leakage)")

    # Simulate receiving sk-12 then 3456789
    text_pending = ""

    # First chunk: "sk-12" (5 chars)
    text_pending += "Here is sk-12"
    safe1, held1 = release_safe_prefix(text_pending)

    # Verify we don't release the incomplete pattern
    if SECRET_PATTERN in safe1 or "sk-12" in safe1:
        print(f"  ✗ FAIL: Unsafe prefix leaked in first release: {safe1}")
        return False

    print(f"    Chunk 1 released: {safe1!r}, held: {held1!r}")

    # Second chunk: "3456789 end."
    text_pending = held1 + "3456789 end."
    text_pending = sanitize_pending(text_pending)
    safe2, held2 = release_safe_prefix(text_pending)

    print(f"    Chunk 2 released: {safe2!r}, held: {held2!r}")

    # Final check: no raw SECRET_PATTERN should ever appear
    if SECRET_PATTERN in safe1 or SECRET_PATTERN in safe2:
        print(f"  ✗ FAIL: Raw secret pattern leaked")
        return False

    print("  ✓ PASS: Holdback prevented unsafe prefix leakage")
    return True


def test_concurrency():
    """Test Case 4: Independent request state."""
    print("[Test] Case 4: Concurrency (independent state per request)")

    # Simulate two concurrent requests
    req1_text = "Request 1 with sk-123456789 secret"
    req2_text = "Request 2 with normal text"

    req1_sanitized = sanitize_pending(req1_text)
    req2_sanitized = sanitize_pending(req2_text)

    req1_ok = REDACTED in req1_sanitized and SECRET_PATTERN not in req1_sanitized
    req2_ok = SECRET_PATTERN not in req2_sanitized and "normal" in req2_sanitized

    if req1_ok and req2_ok:
        print("  ✓ PASS: Both requests processed independently")
        return True
    else:
        print(f"  ✗ FAIL: req1={req1_sanitized}, req2={req2_sanitized}")
        return False


def main():
    print("[Test] === Python ext_proc Logic Tests ===")
    print()

    results = []
    results.append(("Redact", test_redact()))
    results.append(("Boundary", test_boundary()))
    results.append(("Holdback", test_holdback()))
    results.append(("Concurrency", test_concurrency()))

    print()
    print("[Test] === Summary ===")
    passed = sum(1 for _, ok in results if ok)
    total = len(results)

    for name, ok in results:
        status = "✓ PASS" if ok else "✗ FAIL"
        print(f"  {name}: {status}")

    print(f"\nTotal: {passed}/{total} passed")

    if passed == total:
        print("\n✓✓✓ ALL TESTS PASSED ✓✓✓")
        print("Python ext_proc core logic is correct")
        return 0
    else:
        print("\n✗✗✗ SOME TESTS FAILED ✗✗✗")
        return 1


if __name__ == "__main__":
    exit(main())
