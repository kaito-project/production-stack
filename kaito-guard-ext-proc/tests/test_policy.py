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

from unittest.mock import Mock

import pytest
from guardrail_core.config import build_scanner, parse_scanner_config

from ext_proc import policy as policy_module
from ext_proc.policy import DEFAULT_BLOCK_MESSAGE, GatewayPolicy, PolicyConfigError


def test_defaults_and_real_core_scanner():
    policy = GatewayPolicy.from_dict(
        {"scanners": [{"type": "ban_substrings", "substrings": ["secret"]}]}
    )
    assert policy.action == "block"
    assert policy.block_message == DEFAULT_BLOCK_MESSAGE
    assert policy.scanner_configs[0].action_on_hit == "block"
    result = policy.build_scanners()[0].scan("", "a secret")
    assert not result.valid
    assert result.output == "a secret"


def test_policy_actions_order_and_normalization():
    policy = GatewayPolicy.from_dict(
        {
            "action": "redact",
            "blockMessage": "Blocked by gateway policy.",
            "scanners": [
                {
                    "type": "BanSubstrings",
                    "substrings": ["secret"],
                    "caseSensitive": True,
                },
                {"type": "regex", "patterns": ["token"], "action": "block"},
            ],
        }
    )
    assert policy.block_message == "Blocked by gateway policy."
    assert [config.type for config in policy.scanner_configs] == [
        "ban_substrings",
        "regex",
    ]
    assert [config.action_on_hit for config in policy.scanner_configs] == [
        "redact",
        "block",
    ]
    redact, block = policy.build_scanners()
    assert redact.scan("", "secret SECRET").output == "[REDACTED] SECRET"
    assert not block.scan("", "token").valid
    assert block.scan("", "token").output == "token"


@pytest.mark.parametrize("action", ["block", "redact"])
@pytest.mark.parametrize(
    "scanner",
    [
        {"type": "ban_substrings", "substrings": ["secret"]},
        {"type": "regex", "patterns": ["secret"]},
        {"type": "invisible_text"},
        {"type": "json"},
        {"type": "reading_time", "max_time": 1},
        {"type": "token_limit", "limit": 10},
        {"type": "secrets", "redact_mode": "partial"},
        {"type": "sensitive", "detectors": ["email"]},
    ],
)
def test_adapter_matches_direct_shared_core(action, scanner):
    policy = GatewayPolicy.from_dict({"action": action, "scanners": [scanner]})
    adapted = policy.build_scanners()[0]
    direct = build_scanner(parse_scanner_config(scanner, default_action=action))
    for text in ("safe output", "a secret", "alice@example.com", '{"ok": 1}'):
        assert adapted.scan("", text) == direct.scan("", text)


@pytest.mark.parametrize(
    "raw",
    [
        None,
        [],
        {},
        {"scanners": None},
        {"scanners": []},
        {"scanners": {}},
        {"scanners": [None]},
        {"scanners": ["regex"]},
        {"scanners": [{"type": "invisible_text"}], "action": "ignore"},
        {"scanners": [{"type": "invisible_text"}], "action": None},
        {"scanners": [{"type": "invisible_text"}], "blockMessage": ""},
        {"scanners": [{"type": "invisible_text"}], "blockMessage": " "},
        {"scanners": [{"type": "invisible_text"}], "blockMessage": 1},
        {"scanners": [{"type": "invisible_text"}], "failOpen": True},
        {"scanners": [{"type": "invisible_text"}], 1: True},
    ],
)
def test_invalid_gateway_config_is_rejected(raw):
    with pytest.raises(PolicyConfigError):
        GatewayPolicy.from_dict(raw)


@pytest.mark.parametrize(
    "scanner",
    [
        {},
        {"type": "unknown"},
        {"type": "regex", "patterns": ["["]},
        {"type": "regex", "patterns": ["secret"], "action": "ignore"},
        {"type": "regex", "patterns": ["secret"], "isBlocked": "false"},
        {"type": "regex", "patterns": ["secret", 1]},
        {"type": "regex", "patterns": ["secret"], "typo": True},
    ],
)
def test_core_validation_errors_have_entry_context(scanner):
    with pytest.raises(PolicyConfigError, match=r"scanners\[1\]") as exc:
        GatewayPolicy.from_dict({"scanners": [{"type": "invisible_text"}, scanner]})
    assert isinstance(exc.value.__cause__, ValueError)


def test_validation_does_not_build_scanners(monkeypatch):
    builder = Mock()
    monkeypatch.setattr(policy_module, "build_scanner", builder)
    with pytest.raises(PolicyConfigError):
        GatewayPolicy.from_dict(
            {"scanners": [{"type": "invisible_text"}, {"type": "unknown"}]}
        )
    builder.assert_not_called()


def test_build_failure_propagates(monkeypatch):
    failure = RuntimeError("scanner construction failed")
    monkeypatch.setattr(policy_module, "build_scanner", Mock(side_effect=failure))
    policy = GatewayPolicy.from_dict({"scanners": [{"type": "invisible_text"}]})
    with pytest.raises(RuntimeError) as exc:
        policy.build_scanners()
    assert exc.value is failure
