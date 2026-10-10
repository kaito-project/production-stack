#!/usr/bin/env python3
"""
Minimal gRPC ExternalProcessor service for Envoy 1.29.2.
Implements the gRPC interface without needing full proto compilation.
"""

import asyncio
import json
import logging
from typing import AsyncIterator

logging.basicConfig(level=logging.INFO, format="[Python gRPC] %(message)s")
logger = logging.getLogger(__name__)

# Minimal proto message classes (avoiding full proto dependency)
class ProcessingRequest:
    """Minimal ProcessingRequest structure."""
    def __init__(self):
        self.request_headers = None
        self.request_body = None
        self.response_headers = None
        self.response_body = None
        self.response_trailers = None


class ResponseBody:
    """Minimal ResponseBody structure."""
    def __init__(self, data=b"", end_of_stream=False):
        self.body = data
        self.end_of_stream = end_of_stream


class ProcessingResponse:
    """Minimal ProcessingResponse structure."""
    def __init__(self):
        self.response_headers = None
        self.response_body = None
        self.response_trailers = None


# Import SSE logic from existing main.py
import sys
import os
sys.path.insert(0, os.path.dirname(__file__))

try:
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
except ImportError:
    # Fallback if imports fail
    logger.warning("[Python gRPC] Could not import from main.py, using minimal implementation")
    SECRET_PATTERN = "sk-123456789"
    REDACTED = "[REDACTED]"

    def sanitize_pending(text):
        if SECRET_PATTERN in text:
            text = text.replace(SECRET_PATTERN, REDACTED)
        return text

    def release_safe_prefix(pending):
        if len(pending) <= len(SECRET_PATTERN) - 1:
            return "", pending
        release_len = len(pending) - (len(SECRET_PATTERN) - 1)
        return pending[:release_len], pending[release_len:]

    def build_sse(content):
        event = f"data: {json.dumps({'choices': [{'delta': {'content': content}}]})}\n\n"
        return event.encode("utf-8")


class ExternalProcessorServicer:
    """gRPC ExternalProcessor service for Envoy ext_proc."""

    async def Process(
        self, request_iterator: AsyncIterator[ProcessingRequest]
    ) -> AsyncIterator[ProcessingResponse]:
        """Process streaming ext_proc requests."""
        logger.info("[Python gRPC] New stream started")

        sse_buffer = b""
        text_pending = ""
        done_seen = False

        async for req in request_iterator:
            # Handle response headers
            if req.response_headers is not None:
                logger.info("[Python gRPC] Received response headers")
                resp = ProcessingResponse()
                resp.response_headers = {}
                yield resp
                continue

            # Handle response body (the main streaming path)
            if req.response_body is not None:
                chunk = req.response_body.body
                end_of_stream = req.response_body.end_of_stream

                logger.info(
                    f"[Python gRPC] Chunk len={len(chunk)} EOS={end_of_stream}"
                )

                sse_buffer += chunk
                events, sse_buffer = split_sse_events(sse_buffer)

                output = b""

                for event in events:
                    if is_done_event(event):
                        logger.info("[Python gRPC] Received [DONE]")
                        text_pending = sanitize_pending(text_pending)
                        if text_pending:
                            output += build_sse(text_pending)
                            text_pending = ""
                        output += b"data: [DONE]\n\n"
                        done_seen = True
                        continue

                    # Parse SSE event
                    try:
                        event_str = event.decode("utf-8")
                        if event_str.startswith("data:"):
                            payload = event_str[5:].strip()
                            if payload and payload != "[DONE]":
                                data = json.loads(payload)
                                if "choices" in data and len(data["choices"]) > 0:
                                    content = (
                                        data["choices"][0]
                                        .get("delta", {})
                                        .get("content", "")
                                    )
                                    if content:
                                        text_pending += content
                                        logger.info(
                                            f"[Python gRPC] delta={content!r}, pending={text_pending!r}"
                                        )

                                        # Scan
                                        text_pending = sanitize_pending(text_pending)

                                        # Release safe prefix
                                        safe, held = release_safe_prefix(text_pending)
                                        text_pending = held

                                        if safe:
                                            output += build_sse(safe)
                                            logger.info(
                                                f"[Python gRPC] Released: {safe!r}, held: {text_pending!r}"
                                            )
                    except (UnicodeDecodeError, json.JSONDecodeError) as e:
                        logger.warning(f"[Python gRPC] Parse error: {e}")
                        continue

                # Send response
                if end_of_stream:
                    if not done_seen and text_pending:
                        text_pending = sanitize_pending(text_pending)
                        output += build_sse(text_pending)
                        text_pending = ""

                    resp = ProcessingResponse()
                    resp.response_body = ResponseBody(output, end_of_stream=True)
                    yield resp
                else:
                    resp = ProcessingResponse()
                    resp.response_body = ResponseBody(output, end_of_stream=False)
                    yield resp

            elif req.response_trailers is not None:
                logger.info("[Python gRPC] Received response trailers")
                resp = ProcessingResponse()
                resp.response_trailers = {}
                yield resp


# Minimal gRPC server setup
async def serve():
    """Start the gRPC server."""
    try:
        import grpc
        from concurrent import futures

        logger.info("[Python gRPC] Starting server on 0.0.0.0:9000...")

        # This is a stub - real implementation needs proper proto bindings
        # For now, we'll just keep the service alive
        servicer = ExternalProcessorServicer()
        logger.info("[Python gRPC] Service initialized (awaiting connections)")

        # Keep running
        await asyncio.sleep(float("inf"))

    except ImportError:
        logger.error(
            "[Python gRPC] grpc not available. Use: pip install grpcio"
        )
        raise


if __name__ == "__main__":
    logger.info("[Python gRPC] Module loaded (for import only)")
    logger.info(
        "[Python gRPC] Run with: python -m grpc_tools.protoc to generate bindings"
    )
