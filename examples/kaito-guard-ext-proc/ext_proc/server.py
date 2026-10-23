"""Envoy external processor gRPC server for response guardrailing.

Implements the envoy.service.ext_proc.v3.ExternalProcessor service.
Listens on :9000 by default.
"""

import asyncio
import logging

import grpc

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format="[ext_proc] %(asctime)s - %(name)s - %(levelname)s - %(message)s",
)
logger = logging.getLogger(__name__)


class ExternalProcessorServicer:
    """Placeholder for Envoy ext_proc service.

    Phase 1.1: Skeleton only. Process() method to be implemented in Phase 1.7.
    """

    async def Process(self, request_iterator, context):
        """Handle bidirectional streaming ext_proc requests.

        Args:
            request_iterator: Async iterator of ProcessingRequest messages.
            context: gRPC handler context.

        Yields:
            ProcessingResponse messages.
        """
        logger.info("[Phase 1.1] Process() called - stub implementation")
        # Placeholder: will be implemented in PR-1.7 after streaming layers are ready.
        raise NotImplementedError("GuardEngine not integrated yet")


async def serve(host: str = "0.0.0.0", port: int = 9000) -> None:
    """Start the gRPC server.

    Args:
        host: Bind address.
        port: Bind port.
    """
    logger.info(f"[ext_proc] Starting server on {host}:{port}")

    # Placeholder: real server implementation in Phase 1.7
    # For now, just log startup and keep running.
    logger.info("[ext_proc] Server started (stub mode - no gRPC binding yet)")

    try:
        await asyncio.sleep(float("inf"))
    except KeyboardInterrupt:
        logger.info("[ext_proc] Shutdown requested")


if __name__ == "__main__":
    asyncio.run(serve())
