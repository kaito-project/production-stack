from importlib import import_module

import pytest

from ext_proc import __version__
from ext_proc.server import ExternalProcessorServicer


def test_version():
    assert __version__ == "0.1.0"


@pytest.mark.parametrize(
    "module", ["grpc", "google.protobuf", "pydantic", "yaml", "tiktoken"]
)
def test_imports(module):
    import_module(module)


@pytest.mark.asyncio
async def test_servicer_placeholder():
    servicer = ExternalProcessorServicer()

    with pytest.raises(NotImplementedError, match="GuardEngine not integrated yet"):
        await servicer.Process(iter([]), None)
