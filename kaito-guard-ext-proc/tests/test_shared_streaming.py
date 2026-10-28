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

"""Check the published dependency and a minimal shared API composition."""

from importlib.metadata import metadata, requires, version
from unittest.mock import Mock

from guardrail_core.streaming.openai import (
    OpenAIChatChunkParseStatus,
    build_openai_chat_delta_sse_chunk,
    build_sse_done_chunk,
    parse_openai_chat_sse_event,
)
from guardrail_core.streaming.sse import SSEFramer
from guardrail_core.streaming.window import (
    StreamingBufferWindow,
    WindowScanner,
    WindowScanResult,
)


def test_published_dependency_contract():
    assert version("kaito-guardrail-core") == "0.1.1"
    dependencies = requires("kaito-guard-ext-proc")
    assert dependencies is not None
    assert "kaito-guardrail-core==0.1.1" in dependencies
    assert metadata("kaito-guard-ext-proc")["Requires-Python"] == ">=3.12"


def test_published_streaming_apis_compose():
    scanner = Mock(spec=WindowScanner)
    scanner.scan.return_value = WindowScanResult()
    framer = SSEFramer()
    window = StreamingBufferWindow(scanner, holdback_len=2)
    wire = (
        build_openai_chat_delta_sse_chunk("hello") + build_sse_done_chunk()
    ).encode()
    events = framer.feed(wire) + framer.finish()
    content, done = [parse_openai_chat_sse_event(event) for event in events]
    assert content.status == OpenAIChatChunkParseStatus.PARSED
    assert done.status == OpenAIChatChunkParseStatus.DONE
    emitted = []
    for text in content.contents:
        emitted.extend(window.feed(text).chunks)
    emitted.extend(window.flush().chunks)
    assert "".join(emitted) == "hello"
