import asyncio
import logging
from collections.abc import AsyncIterator

import grpc
from envoy.config.core.v3 import base_pb2
from envoy.extensions.filters.http.ext_proc.v3.processing_mode_pb2 import ProcessingMode
from envoy.service.ext_proc.v3 import (
    external_processor_pb2,
    external_processor_pb2_grpc,
)
from envoy.type.v3.http_status_pb2 import HttpStatus

from ext_proc.guardrail_stream import GuardrailStream
from ext_proc.policy import GuardrailPolicyConfig

logger = logging.getLogger(__name__)

pb = external_processor_pb2


def _failure(status: int, message: str, details: str) -> pb.ProcessingResponse:
    return pb.ProcessingResponse(
        immediate_response=pb.ImmediateResponse(
            status=HttpStatus(code=status),
            body=message.encode("utf-8"),
            details=details,
        )
    )


class ExternalProcessorServicer(external_processor_pb2_grpc.ExternalProcessorServicer):
    def __init__(self, policy: GuardrailPolicyConfig | None = None) -> None:
        self.policy = policy
        if policy is None:
            logger.warning(
                "No guardrail policy configured; response protection is disabled"
            )
        else:
            policy.build_window_scanner()

    async def Process(
        self,
        request_iterator: AsyncIterator[external_processor_pb2.ProcessingRequest],
        context: grpc.aio.ServicerContext,
    ) -> AsyncIterator[pb.ProcessingResponse]:
        stream: GuardrailStream | None = None
        request_headers_received = False
        try:
            async for request in request_iterator:
                request_type = request.WhichOneof("request")
                if self.policy is None and request_type is not None:
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
                    and request.protocol_config.response_body_mode
                    != ProcessingMode.STREAMED
                ):
                    logger.warning("Guardrail requires STREAMED response body mode")
                    yield _failure(
                        502, "Unsupported guardrail mode.", "guardrail_unsupported_mode"
                    )
                    return
                if (
                    request_type == "request_headers"
                    and stream is None
                    and not request_headers_received
                ):
                    request_headers_received = True
                    yield pb.ProcessingResponse(
                        request_headers=pb.HeadersResponse(),
                    )
                    continue
                if (
                    request_type == "response_headers"
                    and stream is None
                    and self.policy is not None
                ):
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
                        yield _failure(
                            502,
                            "Unsupported guardrail response.",
                            "guardrail_unsupported_response",
                        )
                        return
                    try:
                        stream = GuardrailStream.from_policy(
                            self.policy, sse_output=True
                        )
                    except Exception:
                        logger.error("Guardrail streaming initialization failed")
                        yield _failure(
                            502,
                            "Guardrail initialization failed.",
                            "guardrail_init_error",
                        )
                        return
                    yield pb.ProcessingResponse(
                        response_headers=pb.HeadersResponse(
                            response=pb.CommonResponse(
                                header_mutation=pb.HeaderMutation(
                                    remove_headers=[
                                        "content-length",
                                        "etag",
                                        "content-md5",
                                        "digest",
                                        "content-digest",
                                        "repr-digest",
                                    ],
                                    set_headers=[
                                        base_pb2.HeaderValueOption(
                                            header=base_pb2.HeaderValue(
                                                key="cache-control",
                                                raw_value=b"no-store",
                                            ),
                                            append_action=base_pb2.HeaderValueOption.OVERWRITE_IF_EXISTS_OR_ADD,
                                        )
                                    ],
                                )
                            )
                        ),
                    )
                    continue
                if request_type == "response_body" and stream is not None:
                    try:
                        result = await asyncio.to_thread(
                            stream.feed, request.response_body.body
                        )
                        chunks = result.chunks
                        if request.response_body.end_of_stream and not result.blocked:
                            result = await asyncio.to_thread(stream.finish)
                            chunks += result.chunks
                    except Exception:
                        # Do not expose model text or chained scanner exception details.
                        logger.error("Guardrail streaming processing failed")
                        yield _failure(
                            502,
                            "Guardrail streaming processing failed.",
                            "guardrail_stream_error",
                        )
                        return
                    if result.blocked:
                        logger.info("Guardrail blocked streaming response")
                        assert self.policy is not None
                        yield _failure(
                            403, self.policy.block_message, "guardrail_blocked"
                        )
                        return
                    yield pb.ProcessingResponse(
                        response_body=pb.BodyResponse(
                            response=pb.CommonResponse(
                                body_mutation=pb.BodyMutation(
                                    body="".join(chunks).encode("utf-8")
                                )
                            )
                        )
                    )
                    if request.response_body.end_of_stream:
                        return
                    continue
                await context.abort(
                    grpc.StatusCode.INVALID_ARGUMENT,
                    "Unexpected guardrail processing phase",
                )
                return
            if self.policy is not None:
                logger.warning(
                    "Guardrail stream ended without a complete HTTP response"
                )
                await context.abort(
                    grpc.StatusCode.FAILED_PRECONDITION, "Incomplete guardrail stream"
                )
        finally:
            if stream is not None:
                stream.cancel()


async def serve(
    host: str = "[::]", port: int = 9000, *, policy: GuardrailPolicyConfig | None = None
) -> None:
    servicer = ExternalProcessorServicer(policy)
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


if __name__ == "__main__":
    logging.basicConfig(
        level=logging.INFO,
        format="[ext_proc] %(asctime)s - %(name)s - %(levelname)s - %(message)s",
    )
    asyncio.run(serve())
