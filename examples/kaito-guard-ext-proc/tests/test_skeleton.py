"""Test PR-1.1: Skeleton + Dependencies."""

import pytest
from ext_proc import __version__


def test_version():
    """Test that version is set."""
    assert __version__ == "0.1.0"


def test_imports():
    """Test that core dependencies can be imported."""
    import grpc
    import logging
    import asyncio

    assert grpc is not None
    assert logging is not None
    assert asyncio is not None


@pytest.mark.asyncio
async def test_servicer_placeholder():
    """Test that ExternalProcessorServicer exists and raises NotImplementedError."""
    from ext_proc.server import ExternalProcessorServicer

    servicer = ExternalProcessorServicer()
    assert servicer is not None

    # Phase 1.1: Process() should raise NotImplementedError (placeholder)
    with pytest.raises(NotImplementedError):
        # Simulate async generator by calling Process (which raises immediately)
        await servicer.Process(iter([]), None)
