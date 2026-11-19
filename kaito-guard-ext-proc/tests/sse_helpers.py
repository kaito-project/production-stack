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

import json

from guardrail_core.streaming.openai import (
    build_sse_data_chunk,
    build_sse_done_chunk,
)
from guardrail_core.streaming.sse import SSEFramer

from ext_proc.policy import GuardrailPolicyConfig

DONE = build_sse_done_chunk().encode()
RESPONSE_METADATA = {
    "id": "chat-test",
    "object": "chat.completion.chunk",
    "created": 1,
    "model": "test",
}


def make_content_event(content):
    return build_sse_data_chunk(
        {
            **RESPONSE_METADATA,
            "choices": [
                {"index": 0, "delta": {"content": content}, "finish_reason": None}
            ],
        }
    ).encode()


def make_policy(action="block"):
    return GuardrailPolicyConfig.from_dict(
        {
            "action": action,
            "blockMessage": "Blocked by streaming policy.",
            "scanner_configs": [{"type": "ban_substrings", "substrings": ["secret"]}],
        }
    )


def parse_sse_payloads(wire):
    framer = SSEFramer()
    events = framer.feed(wire) + framer.finish()
    return [
        json.loads(item.data)
        for item in events
        if item.data is not None and item.data != "[DONE]"
    ]


def extract_stream_text(wire):
    return "".join(
        choice.get("delta", {}).get("content", "")
        for item in parse_sse_payloads(wire)
        for choice in item.get("choices", [])
    )
