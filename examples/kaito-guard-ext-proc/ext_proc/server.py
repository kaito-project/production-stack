"""Envoy external processor stub for response guardrailing."""

import asyncio
import logging

logging.basicConfig(
    level=logging.INFO,
    format="[ext_proc] %(asctime)s - %(name)s - %(levelname)s - %(message)s",
)
logger = logging.getLogger(__name__)


class ExternalProcessorServicer:
    """Placeholder for Envoy ext_proc service."""

    async def Process(self, request_iterator, context):
        raise NotImplementedError("GuardEngine not integrated yet")


async def serve(host: str = "0.0.0.0", port: int = 9000) -> None:
    """Keep the stub running without binding a gRPC port."""
    logger.info(
        "ext_proc stub for %s:%s; no gRPC port is bound",
        host,
        port,
    )

    try:
        await asyncio.sleep(float("inf"))
    except KeyboardInterrupt:
        logger.info("Shutdown requested")


if __name__ == "__main__":
    asyncio.run(serve())
