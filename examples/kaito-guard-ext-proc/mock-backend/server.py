#!/usr/bin/env python3
"""
Mock vLLM backend that streams OpenAI-compatible SSE responses.
Used to test ext_proc (Go/Python) without real LLM.
"""

import json
import sys
import time
from http.server import HTTPServer, BaseHTTPRequestHandler


class MockBackendHandler(BaseHTTPRequestHandler):
    """Simple HTTP server that streams OpenAI-compatible SSE."""

    def do_POST(self):
        """Handle /v1/chat/completions POST."""
        if self.path != "/v1/chat/completions":
            self.send_response(404)
            self.end_headers()
            return

        # Parse request body
        content_length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(content_length).decode("utf-8")
        try:
            request = json.loads(body)
        except json.JSONDecodeError:
            self.send_response(400)
            self.end_headers()
            return

        # Response headers
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "keep-alive")
        self.end_headers()

        # Stream response
        self._stream_response()

    def _stream_response(self):
        """Stream test response with optional secret pattern."""
        # Test cases
        test_cases = [
            # Case 1: Normal response (no secret)
            [
                "Hello, ",
                "this is ",
                "a normal ",
                "response.",
            ],
            # Case 2: Secret split across chunks
            # "sk-123456789" should be detected and redacted
            [
                "Here is ",
                "a secret: ",
                "sk-12",
                "3456789",
                " (should be redacted)",
            ],
            # Case 3: Secret at boundary
            [
                "Secret at end: ",
                "sk-123456789",
            ],
        ]

        # For demo, use Case 2
        chunks = test_cases[1]

        for i, chunk in enumerate(chunks):
            delta = {"role": "assistant", "content": chunk}
            message = {
                "id": "chatcmpl-test",
                "object": "text_completion",
                "created": int(time.time()),
                "model": "gpt-4",
                "choices": [{"delta": delta, "index": 0, "finish_reason": None}],
            }
            event = f"data: {json.dumps(message)}\n\n"
            self.wfile.write(event.encode("utf-8"))
            self.wfile.flush()

            # Small delay between chunks
            time.sleep(0.01)

        # Final [DONE] event
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def log_message(self, format, *args):
        """Suppress default logging."""
        print(f"[Mock Backend] {format % args}", file=sys.stderr)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8000
    server = HTTPServer(("localhost", port), MockBackendHandler)
    print(f"[Mock Backend] listening on http://localhost:{port}", file=sys.stderr)
    server.serve_forever()
