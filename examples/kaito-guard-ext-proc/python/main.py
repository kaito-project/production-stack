#!/usr/bin/env python3
"""
Python implementation of OpenAI SSE semantic holdback ext_proc.
Matches Go behavior for comparison testing.
"""

import asyncio
import json
import logging
import sys
from typing import List, Optional, Tuple

import grpc
from grpc import aio

# Generated protobuf code (you'll need: python3 -m grpc_tools.protoc)
# For now, we'll use the public go-control-plane protos
from google.protobuf import empty_pb2

# Note: These imports assume envoyproxy/go-control-plane generated Python bindings
# In production, generate with:
# python3 -m grpc_tools.protoc -I. --python_out=. --grpc_python_out=. envoy/service/ext_proc/v3/*.proto

# Placeholder for actual imports (comment out for dev, use real imports in production)
try:
    from envoy.service.ext_proc.v3 import external_processor_pb2 as extproc_pb2
    from envoy.service.ext_proc.v3 import external_processor_pb2_grpc as extproc_grpc
except ImportError:
    # Fallback: Define minimal message types for testing
    class MockExtProc:
        class ProcessingRequest:
            class ResponseHeaders:
                pass

            class ResponseBody:
                def __init__(self):
                    self.body = b""
                    self.end_of_stream = False

        class ProcessingResponse:
            class ResponseHeaders:
                pass

            class ResponseBody:
                class BodyResponse:
                    class CommonResponse:
                        class BodyMutation:
                            class StreamedBodyResponse:
                                def __init__(self, body, end_of_stream):
                                    self.body = body
                                    self.end_of_stream = end_of_stream

    extproc_pb2 = MockExtProc()


logging.basicConfig(
    level=logging.INFO,
    format="[Python] %(message)s",
)
logger = logging.getLogger(__name__)

SECRET_PATTERN = "sk-123456789"
REDACTED = "[REDACTED]"
HOLDBACK_SIZE = len(SECRET_PATTERN) - 1


class ChatCompletionChunk:
    """Represents OpenAI chat completion chunk."""

    def __init__(self, content: str = ""):
        self.content = content

    def to_dict(self):
        return {
            "id": "chatcmpl-test",
            "object": "text_completion",
            "created": 0,
            "model": "gpt-4",
            "choices": [
                {
                    "delta": {"role": "assistant", "content": self.content},
                    "index": 0,
                    "finish_reason": None,
                }
            ],
        }

    @staticmethod
    def from_sse_line(line: str) -> Optional["ChatCompletionChunk"]:
        """Parse SSE 'data: {...}' line."""
        if not line.startswith("data:"):
            return None

        payload = line[5:].strip()
        if payload == "[DONE]":
            return None

        try:
            obj = json.loads(payload)
            if (
                "choices" in obj
                and len(obj["choices"]) > 0
                and "delta" in obj["choices"][0]
            ):
                content = obj["choices"][0]["delta"].get("content", "")
                return ChatCompletionChunk(content)
        except json.JSONDecodeError:
            logger.warning(f"[Python] Failed to parse JSON: {payload}")

        return None


def split_sse_events(buffer: bytes) -> Tuple[List[bytes], bytes]:
    """Split buffer by \\n\\n separator. Return (events, remaining)."""
    events = []
    while b"\n\n" in buffer:
        idx = buffer.index(b"\n\n")
        events.append(buffer[:idx])
        buffer = buffer[idx + 2 :]
    return events, buffer


def is_done_event(event: bytes) -> bool:
    """Check if event is [DONE]."""
    return event.strip() == b"data: [DONE]"


def sanitize_pending(text: str) -> str:
    """Apply secret detection and redaction."""
    if SECRET_PATTERN in text:
        logger.info(f"[Python] DETECTED SECRET in logical text: {text!r}")
        text = text.replace(SECRET_PATTERN, REDACTED)
        logger.info(f"[Python] REDACTED logical text to: {text!r}")
    return text


def release_safe_prefix(pending: str) -> Tuple[str, str]:
    """Release text that's too long to contain the pattern."""
    if len(pending) <= HOLDBACK_SIZE:
        return "", pending
    release_len = len(pending) - HOLDBACK_SIZE
    return pending[:release_len], pending[release_len:]


def build_sse(content: str) -> bytes:
    """Build OpenAI-compatible SSE event."""
    chunk = ChatCompletionChunk(content)
    payload = json.dumps(chunk.to_dict())
    event = f"data: {payload}\n\n".encode("utf-8")
    return event


class GuardProcessor:
    """OpenAI SSE semantic holdback ext_proc."""

    async def process(self, request_iterator):
        """Process streaming ext_proc requests."""
        logger.info("[Python] new ext_proc stream (OpenAI SSE semantic holdback)")

        sse_buffer = b""
        text_pending = ""
        done_seen = False

        try:
            async for req in request_iterator:
                if req.HasField("response_headers"):
                    logger.info("[Python] received response headers")
                    yield self._make_response_headers()

                elif req.HasField("response_body"):
                    body_req = req.response_body
                    chunk = body_req.body
                    end_of_stream = body_req.end_of_stream

                    logger.info(
                        f"[Python] recv raw body chunk len={len(chunk)} EndOfStream={end_of_stream}"
                    )

                    sse_buffer += chunk
                    events, sse_buffer = split_sse_events(sse_buffer)

                    output = b""

                    for event in events:
                        logger.info(f"[Python] complete SSE event: {event!r}")

                        if is_done_event(event):
                            logger.info("[Python] received [DONE]")
                            # Flush pending logical text
                            text_pending = sanitize_pending(text_pending)
                            if text_pending:
                                output += build_sse(text_pending)
                                logger.info(
                                    f"[Python] flushing final logical content: {text_pending!r}"
                                )
                                text_pending = ""

                            output += b"data: [DONE]\n\n"
                            done_seen = True
                            continue

                        # Parse SSE event
                        try:
                            event_str = event.decode("utf-8")
                            chunk_obj = ChatCompletionChunk.from_sse_line(event_str)
                        except (UnicodeDecodeError, json.JSONDecodeError) as e:
                            logger.warning(f"[Python] Failed to parse SSE: {e}")
                            continue

                        if chunk_obj is None:
                            logger.info(
                                "[Python] SSE event has no supported delta.content"
                            )
                            continue

                        content = chunk_obj.content
                        logger.info(f"[Python] extracted delta.content={content!r}")

                        # Logical concatenation
                        text_pending += content
                        logger.info(f"[Python] logical pending before scan={text_pending!r}")

                        # Scan
                        text_pending = sanitize_pending(text_pending)

                        # Release safe prefix
                        safe, held = release_safe_prefix(text_pending)
                        text_pending = held

                        if safe:
                            output += build_sse(safe)
                            logger.info(
                                f"[Python] releasing safe logical prefix={safe!r} holding={text_pending!r}"
                            )
                        else:
                            logger.info(f"[Python] holding logical text={text_pending!r}")

                    if end_of_stream:
                        logger.info("[Python] HTTP body EndOfStream=true")

                        # Handle incomplete SSE
                        if sse_buffer:
                            logger.info(
                                f"[Python] dropping incomplete SSE bytes at EOS: {sse_buffer!r}"
                            )
                            sse_buffer = b""

                        # Flush remaining text
                        if not done_seen and text_pending:
                            text_pending = sanitize_pending(text_pending)
                            output += build_sse(text_pending)
                            logger.info(
                                f"[Python] EOS flushing logical content={text_pending!r}"
                            )
                            text_pending = ""

                        yield self._make_response_body(output, end_of_stream=True)
                    else:
                        yield self._make_response_body(output, end_of_stream=False)

                elif req.HasField("response_trailers"):
                    logger.info("[Python] received response trailers")
                    yield self._make_response_trailers()

                else:
                    logger.info(f"[Python] ignoring request type {type(req)}")
                    continue

        except asyncio.CancelledError:
            logger.info("[Python] stream cancelled")
        except Exception as e:
            logger.error(f"[Python] error: {e}", exc_info=True)
            raise

    def _make_response_headers(self):
        """Build response headers response."""
        # Minimal mock response
        response = type("ProcessingResponse", (), {})()
        response.response_headers = type("ResponseHeaders", (), {})()
        return response

    def _make_response_body(self, body: bytes, end_of_stream: bool):
        """Build response body response."""
        response = type("ProcessingResponse", (), {})()
        response.response_body = type("BodyResponse", (), {})()
        response.response_body.response = type("CommonResponse", (), {})()
        response.response_body.response.status = 0  # CONTINUE
        response.response_body.response.body_mutation = type("BodyMutation", (), {})()
        response.response_body.response.body_mutation.streamed_response = type(
            "StreamedBodyResponse", (), {}
        )()
        response.response_body.response.body_mutation.streamed_response.body = body
        response.response_body.response.body_mutation.streamed_response.end_of_stream = (
            end_of_stream
        )
        return response

    def _make_response_trailers(self):
        """Build response trailers response."""
        response = type("ProcessingResponse", (), {})()
        response.response_trailers = type("TrailersResponse", (), {})()
        return response


async def main():
    """Run gRPC server."""
    processor = GuardProcessor()

    # For actual gRPC, you'd use:
    # server = aio.server()
    # extproc_grpc.add_ExternalProcessorServicer_to_server(processor, server)
    # await server.start()

    # For now, minimal mock server
    logger.info(
        "[Python] OpenAI SSE semantic holdback ext_proc (mock mode, not real gRPC)"
    )
    logger.info(f"[Python] HOLDBACK_SIZE={HOLDBACK_SIZE} SECRET_PATTERN={SECRET_PATTERN!r} REDACTED={REDACTED!r}")

    # Keep running
    await asyncio.sleep(float("inf"))


if __name__ == "__main__":
    asyncio.run(main())
