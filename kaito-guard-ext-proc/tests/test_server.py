# Copyright (c) KAITO authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import asyncio
from unittest.mock import AsyncMock, Mock

import grpc
import pytest
from envoy.service.ext_proc.v3 import (
    external_processor_pb2,
    external_processor_pb2_grpc,
)

from ext_proc import __version__
from ext_proc.server import serve


def test_version():
    assert __version__ == "0.1.0"


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "host, target", [("127.0.0.1", "127.0.0.1"), ("[::]", "[::1]")]
)
async def test_serve_accepts_ext_proc_requests_and_stops(unused_tcp_port, host, target):
    address = f"{target}:{unused_tcp_port}"
    task = asyncio.create_task(serve(host=host, port=unused_tcp_port))
    try:
        async with grpc.aio.insecure_channel(address) as channel:
            await asyncio.wait_for(channel.channel_ready(), timeout=5)
            stub = external_processor_pb2_grpc.ExternalProcessorStub(channel)
            with pytest.raises(grpc.aio.AioRpcError) as exc:
                await stub.Process(
                    iter([external_processor_pb2.ProcessingRequest()]), timeout=5
                ).read()
            assert exc.value.code() == grpc.StatusCode.UNIMPLEMENTED
            assert (
                exc.value.details()
                == "KAITO guardrail core integration is not implemented"
            )
    finally:
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    async with grpc.aio.insecure_channel(address) as channel:
        stub = external_processor_pb2_grpc.ExternalProcessorStub(channel)
        with pytest.raises(grpc.aio.AioRpcError) as exc:
            await stub.Process(iter([]), timeout=1).read()
        assert exc.value.code() == grpc.StatusCode.UNAVAILABLE


@pytest.mark.asyncio
async def test_serve_reports_bind_failure(monkeypatch):
    server = Mock()
    server.add_insecure_port.return_value = 0
    server.stop = AsyncMock()
    monkeypatch.setattr(grpc.aio, "server", lambda: server)

    with pytest.raises(RuntimeError, match="Failed to bind"):
        await asyncio.wait_for(serve(), timeout=1)

    server.start.assert_not_called()
    server.stop.assert_awaited_once_with(grace=0)
