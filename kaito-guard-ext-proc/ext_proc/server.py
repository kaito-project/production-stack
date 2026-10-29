import asyncio
import logging
from collections.abc import AsyncIterator

import grpc
from envoy.service.ext_proc.v3 import (
    external_processor_pb2,
    external_processor_pb2_grpc,
)

logger = logging.getLogger(__name__)


class ExternalProcessorServicer(external_processor_pb2_grpc.ExternalProcessorServicer):
    async def Process(
        self,
        request_iterator: AsyncIterator[external_processor_pb2.ProcessingRequest],
        context: grpc.aio.ServicerContext,
    ) -> None:
        await context.abort(
            grpc.StatusCode.UNIMPLEMENTED,
            "KAITO guardrail core integration is not implemented",
        )


async def serve(host: str = "[::]", port: int = 9000) -> None:
    server = grpc.aio.server()
    external_processor_pb2_grpc.add_ExternalProcessorServicer_to_server(
        ExternalProcessorServicer(), server
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
