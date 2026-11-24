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

"""Small wire-level checks using a real Envoy, upstream, and gRPC processor."""

import asyncio
import json
import os
import shutil
from contextlib import asynccontextmanager
from http.client import HTTPConnection

import grpc
import pytest

from ext_proc.server import serve
from tests.test_server import chat_body, make_policy


@pytest.fixture
def envoy_binary():
    binary = os.environ.get("ENVOY_BINARY") or shutil.which("envoy")
    if not binary:
        pytest.skip("Set ENVOY_BINARY to run the real Envoy tests")
    return binary


def envoy_config(listener_port, upstream_port, processor_port):
    def cluster(name, port, *, http2=False):
        config = {
            "name": name,
            "type": "STATIC",
            "connect_timeout": "1s",
            "load_assignment": {
                "cluster_name": name,
                "endpoints": [
                    {
                        "lb_endpoints": [
                            {
                                "endpoint": {
                                    "address": {
                                        "socket_address": {
                                            "address": "127.0.0.1",
                                            "port_value": port,
                                        }
                                    }
                                }
                            }
                        ]
                    }
                ],
            },
        }
        if http2:
            config["http2_protocol_options"] = {}
        return config

    return {
        "static_resources": {
            "listeners": [
                {
                    "name": "http",
                    "address": {
                        "socket_address": {
                            "address": "127.0.0.1",
                            "port_value": listener_port,
                        }
                    },
                    "per_connection_buffer_limit_bytes": 1048576,
                    "filter_chains": [
                        {
                            "filters": [
                                {
                                    "name": "envoy.filters.network.http_connection_manager",
                                    "typed_config": {
                                        "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
                                        "stat_prefix": "test",
                                        "route_config": {
                                            "name": "test",
                                            "virtual_hosts": [
                                                {
                                                    "name": "test",
                                                    "domains": ["*"],
                                                    "routes": [
                                                        {
                                                            "match": {"prefix": "/"},
                                                            "route": {
                                                                "cluster": "upstream"
                                                            },
                                                        }
                                                    ],
                                                }
                                            ],
                                        },
                                        "http_filters": [
                                            {
                                                "name": "envoy.filters.http.ext_proc",
                                                "typed_config": {
                                                    "@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor",
                                                    "grpc_service": {
                                                        "envoy_grpc": {
                                                            "cluster_name": "processor"
                                                        },
                                                        "timeout": "5s",
                                                    },
                                                    "message_timeout": "5s",
                                                    "failure_mode_allow": False,
                                                    "allow_mode_override": False,
                                                    "processing_mode": {
                                                        "request_header_mode": "SKIP",
                                                        "request_body_mode": "NONE",
                                                        "request_trailer_mode": "SKIP",
                                                        "response_header_mode": "SEND",
                                                        "response_body_mode": "BUFFERED",
                                                        "response_trailer_mode": "SKIP",
                                                    },
                                                },
                                            },
                                            {
                                                "name": "envoy.filters.http.router",
                                                "typed_config": {
                                                    "@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"
                                                },
                                            },
                                        ],
                                    },
                                }
                            ]
                        }
                    ],
                }
            ],
            "clusters": [
                cluster("upstream", upstream_port),
                cluster("processor", processor_port, http2=True),
            ],
        }
    }


async def wait_for_listener(port, process):
    async with asyncio.timeout(10):
        while True:
            if process.returncode is not None:
                raise RuntimeError(f"Envoy exited with status {process.returncode}")
            try:
                _, writer = await asyncio.open_connection("127.0.0.1", port)
            except OSError:
                await asyncio.sleep(0.02)
            else:
                writer.close()
                await writer.wait_closed()
                return


@asynccontextmanager
async def proxy(binary, tmp_path, unused_tcp_port_factory, policy, body):
    async def upstream(reader, writer):
        try:
            await reader.readuntil(b"\r\n\r\n")
            writer.write(
                b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
                + f"Content-Length: {len(body)}\r\n".encode()
                + b"ETag: original-body\r\nConnection: close\r\n\r\n"
                + body[: len(body) // 2]
            )
            await writer.drain()
            await asyncio.sleep(0.05)
            writer.write(body[len(body) // 2 :])
            await writer.drain()
        finally:
            writer.close()
            await writer.wait_closed()

    upstream_server = await asyncio.start_server(upstream, "127.0.0.1", 0)
    upstream_port = upstream_server.sockets[0].getsockname()[1]
    listener_port = unused_tcp_port_factory()
    processor_port = unused_tcp_port_factory()
    config = tmp_path / "envoy.json"
    config.write_text(
        json.dumps(envoy_config(listener_port, upstream_port, processor_port))
    )
    grpc_task = asyncio.create_task(serve("127.0.0.1", processor_port, policy=policy))
    process = None
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{processor_port}") as channel:
            await asyncio.wait_for(channel.channel_ready(), timeout=5)
        with (tmp_path / "envoy.log").open("wb") as log:
            process = await asyncio.create_subprocess_exec(
                binary,
                "-c",
                str(config),
                "--concurrency",
                "1",
                "--disable-hot-restart",
                "--log-level",
                "warn",
                stdout=log,
                stderr=log,
            )
            try:
                await wait_for_listener(listener_port, process)
                yield listener_port
            finally:
                if process.returncode is None:
                    process.terminate()
                try:
                    await asyncio.wait_for(process.wait(), timeout=5)
                except TimeoutError:
                    process.kill()
                    await process.wait()
                    raise
    finally:
        upstream_server.close()
        await upstream_server.wait_closed()
        grpc_task.cancel()
        try:
            await grpc_task
        except asyncio.CancelledError:
            pass


def get_response(port):
    connection = HTTPConnection("127.0.0.1", port, timeout=5)
    try:
        connection.request("GET", "/", headers={"Connection": "close"})
        response = connection.getresponse()
        return response.status, response.headers, response.read()
    finally:
        connection.close()


@pytest.mark.asyncio
async def test_envoy_allows_clean_json_without_rewriting(
    envoy_binary, tmp_path, unused_tcp_port_factory
):
    body = chat_body("safe reply", "another reply") + b"\n"
    async with proxy(
        envoy_binary, tmp_path, unused_tcp_port_factory, make_policy(), body
    ) as port:
        status, _, received = await asyncio.to_thread(get_response, port)
    assert status == 200
    assert received == body


@pytest.mark.asyncio
async def test_envoy_malformed_json_returns_generic_502(
    envoy_binary, tmp_path, unused_tcp_port_factory
):
    body = b'{"choices":[{"message":{"content":"pending-original-text"'
    async with proxy(
        envoy_binary, tmp_path, unused_tcp_port_factory, make_policy(), body
    ) as port:
        status, _, received = await asyncio.to_thread(get_response, port)
    assert status == 502
    assert received == b"Guardrail response processing failed."


@pytest.mark.asyncio
async def test_envoy_block_returns_403_without_original_body_bytes(
    envoy_binary, tmp_path, unused_tcp_port_factory
):
    body = chat_body("original-prefix-must-not-leak", "a secret")
    async with proxy(
        envoy_binary, tmp_path, unused_tcp_port_factory, make_policy(), body
    ) as port:
        status, headers, received = await asyncio.to_thread(get_response, port)
    assert status == 403
    assert received == b"Blocked by test policy."
    assert b"original-prefix-must-not-leak" not in received
    assert b"secret" not in received
    assert headers.get_all("content-length") == [str(len(received))]


@pytest.mark.asyncio
async def test_envoy_redaction_replaces_body_and_content_length(
    envoy_binary, tmp_path, unused_tcp_port_factory
):
    body = chat_body("a secret \u56de\u590d", "safe reply")
    async with proxy(
        envoy_binary,
        tmp_path,
        unused_tcp_port_factory,
        make_policy(action="redact"),
        body,
    ) as port:
        status, headers, received = await asyncio.to_thread(get_response, port)
    expected = json.loads(body)
    expected["choices"][0]["message"]["content"] = "a [REDACTED] \u56de\u590d"
    assert status == 200
    assert json.loads(received) == expected
    assert b"secret" not in received
    assert "etag" not in headers
    assert headers.get_all("content-length") == [str(len(received))]
