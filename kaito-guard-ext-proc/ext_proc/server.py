import argparse
import asyncio
import json
import logging
from collections.abc import AsyncGenerator, AsyncIterator
from contextlib import aclosing
from pathlib import Path

import grpc
from envoy.config.core.v3 import base_pb2
from envoy.extensions.filters.http.ext_proc.v3 import processing_mode_pb2
from envoy.service.ext_proc.v3 import (
    external_processor_pb2,
    external_processor_pb2_grpc,
)
from envoy.type.v3 import http_status_pb2
from guardrail_core.buffered import scan_policy_output

from ext_proc.guardrail_stream import GuardrailStream
from ext_proc.policy import GuardrailPolicyConfig

logger = logging.getLogger(__name__)

pb = external_processor_pb2
ProcessingMode = processing_mode_pb2.ProcessingMode


def _reject_json_constant(value: str) -> None:
    raise ValueError("Non-standard JSON constant")


def _unique_json_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate JSON key")
        result[key] = value
    return result


def _header_value(key: str, value: bytes) -> base_pb2.HeaderValueOption:
    return base_pb2.HeaderValueOption(
        header=base_pb2.HeaderValue(key=key, raw_value=value),
        append_action=base_pb2.HeaderValueOption.OVERWRITE_IF_EXISTS_OR_ADD,
    )


def _build_sse_header_update() -> pb.ProcessingResponse:
    mutation = pb.HeaderMutation(
        remove_headers=[
            "content-length",
            "etag",
            "content-md5",
            "digest",
            "content-digest",
            "repr-digest",
        ],
        set_headers=[_header_value("cache-control", b"no-store")],
    )
    return pb.ProcessingResponse(
        response_headers=pb.HeadersResponse(
            response=pb.CommonResponse(header_mutation=mutation)
        )
    )


def _immediate_response(status: int, message: str, details: str) -> pb.ProcessingResponse:
    return pb.ProcessingResponse(
        immediate_response=pb.ImmediateResponse(
            status=http_status_pb2.HttpStatus(code=status),
            headers=pb.HeaderMutation(
                set_headers=[
                    _header_value("content-type", b"text/plain; charset=utf-8")
                ]
            ),
            body=message.encode("utf-8"),
            details=details,
        )
    )


class ExternalProcessorServicer(external_processor_pb2_grpc.ExternalProcessorServicer):
    def __init__(
        self, policy: GuardrailPolicyConfig | None = None, *, streaming: bool = False
    ) -> None:
        self.policy = policy
        self.streaming = streaming
        if policy is None:
            logger.warning(
                "No guardrail policy configured; response protection is disabled"
            )
        elif streaming:
            policy.build_window_scanner()
        else:
            # Fail startup on scanner construction errors; never silently disable a policy.
            policy.build_scanners()

    def _scan_body(self, body: bytes) -> bytes | None:
        policy = self.policy
        assert policy is not None
        document = json.loads(
            body.decode("utf-8"),
            parse_constant=_reject_json_constant,
            object_pairs_hook=_unique_json_object,
        )
        if not isinstance(document, dict):
            raise ValueError("Expected a Chat Completions object")
        choices = document.get("choices")
        if not isinstance(choices, list) or not choices:
            raise ValueError("Expected non-empty choices")
        content_changed = False
        for choice in choices:
            if not isinstance(choice, dict) or not isinstance(choice.get("message"), dict):
                raise ValueError("Expected a choice message")
            message = choice["message"]
            original_content = message.get("content")
            if not isinstance(original_content, str):
                raise ValueError("Non-text choices are outside the supported scan scope")
            result = scan_policy_output(
                policy.scanner_configs, "", original_content, default_action=policy.action
            )
            if result.blocked:
                return None
            if result.output is None:
                raise ValueError("Allowed guardrail result is missing output")
            message["content"] = result.output
            content_changed |= result.output != original_content
        if not content_changed:
            return body
        return json.dumps(document, ensure_ascii=False, allow_nan=False).encode("utf-8")

    async def Process(
        self,
        request_iterator: AsyncIterator[pb.ProcessingRequest],
        context: grpc.aio.ServicerContext,
    ) -> AsyncIterator[pb.ProcessingResponse]:
        if self.streaming and self.policy is not None:
            async with aclosing(
                self._process_sse_response(request_iterator, context, self.policy)
            ) as responses:
                async for response in responses:
                    yield response
            return
        async for request in request_iterator:
            request_type = request.WhichOneof("request")
            if self.policy is None:
                if request_type is None:
                    await context.abort(
                        grpc.StatusCode.INVALID_ARGUMENT, "Missing processing phase"
                    )
                    return
                response = pb.ProcessingResponse()
                getattr(response, request_type).SetInParent()
                yield response
                continue
            if request.observability_mode:
                await context.abort(
                    grpc.StatusCode.FAILED_PRECONDITION,
                    "Guardrails require blocking ext_proc mode",
                )
                return
            if (
                request.HasField("protocol_config")
                and request.protocol_config.response_body_mode != ProcessingMode.BUFFERED
            ):
                logger.warning("Guardrail requires BUFFERED response body mode")
                yield _immediate_response(
                    502, "Unsupported guardrail mode.", "guardrail_unsupported_mode"
                )
                return
            if request_type == "request_headers":
                yield pb.ProcessingResponse(
                    request_headers=pb.HeadersResponse(),
                )
                continue
            if request_type == "response_headers":
                headers = {
                    header.key.lower(): (
                        header.raw_value.decode("latin-1")
                        if header.raw_value else header.value
                    )
                    for header in request.response_headers.headers.headers
                }
                content_type = (
                    headers.get("content-type", "").split(";", 1)[0].strip().lower()
                )
                content_encoding = (
                    headers.get("content-encoding", "identity").strip().lower()
                )
                if (
                    content_type != "application/json"
                    or content_encoding != "identity"
                    or request.response_headers.end_of_stream
                ):
                    logger.warning("Guardrail rejected unsupported response headers")
                    yield _immediate_response(
                        502,
                        "Unsupported guardrail response.",
                        "guardrail_unsupported_response",
                    )
                    return
                yield pb.ProcessingResponse(
                    response_headers=pb.HeadersResponse(),
                )
                continue
            if request_type == "response_body":
                if not request.response_body.end_of_stream:
                    logger.warning("Guardrail requires a complete buffered response body")
                    yield _immediate_response(
                        502,
                        "Incomplete guardrail response.",
                        "guardrail_incomplete_response",
                    )
                    return
                try:
                    scanned_body = await asyncio.to_thread(
                        self._scan_body, request.response_body.body
                    )
                except Exception:
                    # Do not log model text or chained scanner exceptions.
                    logger.error("Guardrail response parsing or scanning failed")
                    yield _immediate_response(
                        502,
                        "Guardrail response processing failed.",
                        "guardrail_processing_error",
                    )
                    return
                if scanned_body is None:
                    logger.info("Guardrail blocked response")
                    yield _immediate_response(
                        403, self.policy.block_message, "guardrail_blocked"
                    )
                    return
                response = pb.BodyResponse()
                if scanned_body != request.response_body.body:
                    response.response.body_mutation.body = scanned_body
                    response.response.header_mutation.set_headers.append(
                        _header_value(
                            "content-length", str(len(scanned_body)).encode("ascii")
                        )
                    )
                    response.response.header_mutation.remove_headers.extend(
                        [
                            "etag", "content-md5", "digest",
                            "content-digest", "repr-digest",
                        ]
                    )
                yield pb.ProcessingResponse(response_body=response)
                continue
            logger.warning("Guardrail received an unexpected processing phase")
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT, "Unexpected guardrail processing phase"
            )
            return


    async def _process_sse_response(
        self,
        request_iterator: AsyncIterator[external_processor_pb2.ProcessingRequest],
        context: grpc.aio.ServicerContext,
        policy: GuardrailPolicyConfig,
    ) -> AsyncGenerator[pb.ProcessingResponse, None]:
        guardrail_stream: GuardrailStream | None = None
        try:
            async for request in request_iterator:
                request_type = request.WhichOneof("request")
                if request.observability_mode:
                    await context.abort(
                        grpc.StatusCode.FAILED_PRECONDITION,
                        "Guardrails require blocking ext_proc mode",
                    )
                    return
                if (
                    request.HasField("protocol_config")
                    and request.protocol_config.response_body_mode
                    != ProcessingMode.STREAMED
                ):
                    logger.warning("Guardrail requires STREAMED response body mode")
                    yield _immediate_response(
                        502, "Unsupported guardrail mode.", "guardrail_unsupported_mode"
                    )
                    return
                if request_type == "request_headers" and guardrail_stream is None:
                    yield pb.ProcessingResponse(
                        request_headers=pb.HeadersResponse(),
                    )
                    continue
                if request_type == "response_headers" and guardrail_stream is None:
                    headers = {
                        header.key.lower(): (
                            header.raw_value.decode("latin-1")
                            if header.raw_value
                            else header.value
                        )
                        for header in request.response_headers.headers.headers
                    }
                    if (
                        headers.get("content-type", "").split(";", 1)[0].strip().lower()
                        != "text/event-stream"
                        or headers.get("content-encoding", "identity").strip().lower()
                        != "identity"
                        or request.response_headers.end_of_stream
                    ):
                        logger.warning(
                            "Guardrail rejected unsupported streaming headers"
                        )
                        yield _immediate_response(
                            502,
                            "Unsupported guardrail response.",
                            "guardrail_unsupported_response",
                        )
                        return
                    try:
                        guardrail_stream = GuardrailStream.from_policy(
                            policy, sse_output=True
                        )
                    except Exception:
                        logger.error("Guardrail streaming initialization failed")
                        yield _immediate_response(
                            502,
                            "Guardrail initialization failed.",
                            "guardrail_init_error",
                        )
                        return
                    yield _build_sse_header_update()
                    continue
                if request_type == "response_body" and guardrail_stream is not None:
                    try:
                        result = await asyncio.to_thread(
                            guardrail_stream.feed, request.response_body.body
                        )
                        output_chunks = result.chunks
                        if request.response_body.end_of_stream and not result.blocked:
                            result = await asyncio.to_thread(guardrail_stream.finish)
                            output_chunks += result.chunks
                    except Exception:
                        # Do not expose model text or chained scanner exception details.
                        logger.error("Guardrail streaming processing failed")
                        yield _immediate_response(
                            502,
                            "Guardrail streaming processing failed.",
                            "guardrail_stream_error",
                        )
                        return
                    if result.blocked:
                        logger.info("Guardrail blocked streaming response")
                        yield _immediate_response(
                            403, policy.block_message, "guardrail_blocked"
                        )
                        return
                    yield pb.ProcessingResponse(
                        response_body=pb.BodyResponse(
                            response=pb.CommonResponse(
                                body_mutation=pb.BodyMutation(
                                    body="".join(output_chunks).encode("utf-8")
                                )
                            )
                        )
                    )
                    if request.response_body.end_of_stream:
                        return
                    continue
                await context.abort(
                    grpc.StatusCode.INVALID_ARGUMENT,
                    f"Unexpected ext_proc message {request_type!r} in the current stream state",
                )
                return
            logger.warning("Guardrail stream ended without a complete HTTP response")
            await context.abort(
                grpc.StatusCode.FAILED_PRECONDITION, "Incomplete guardrail stream"
            )
        finally:
            if guardrail_stream is not None:
                guardrail_stream.cancel()


async def serve(
    host: str = "[::]", port: int = 9000, *, policy: GuardrailPolicyConfig | None = None,
    streaming: bool = False,
) -> None:
    servicer = ExternalProcessorServicer(policy, streaming=streaming)
    server = grpc.aio.server()
    external_processor_pb2_grpc.add_ExternalProcessorServicer_to_server(
        servicer, server
    )
    address = f"{host}:{port}"
    try:
        if server.add_insecure_port(address) == 0:
            raise RuntimeError(f"Failed to bind ext_proc server to {address}")
        await server.start()
        logger.info("ext_proc server listening on %s", address)
        await server.wait_for_termination()
    finally:
        await server.stop(grace=0)


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="Run the Envoy guardrail processor.")
    parser.add_argument("--policy", type=Path, help="Path to a JSON guardrail policy")
    parser.add_argument("--response-body-mode", choices=("BUFFERED", "STREAMED"))
    args = parser.parse_args(argv)
    if args.response_body_mode and args.policy is None:
        parser.error("--response-body-mode requires --policy")
    policy = None
    if args.policy is not None:
        try:
            with args.policy.open(encoding="utf-8") as source:
                raw = json.load(source, object_pairs_hook=_unique_json_object,
                                parse_constant=_reject_json_constant)
            policy = GuardrailPolicyConfig.from_dict(raw)
        except (OSError, ValueError) as exc:
            parser.exit(2, f"Cannot load guardrail policy {args.policy}: {exc}\n")
    asyncio.run(serve(policy=policy, streaming=args.response_body_mode == "STREAMED"))


if __name__ == "__main__":
    logging.basicConfig(
        level=logging.INFO,
        format="[ext_proc] %(asctime)s - %(name)s - %(levelname)s - %(message)s",
    )
    main()
