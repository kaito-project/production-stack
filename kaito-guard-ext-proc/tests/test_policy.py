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
from ext_proc.policy import (
    DEFAULT_BLOCK_MESSAGE,
    GuardrailPolicyConfig,
    PolicyConfigError,
)


def test_defaults_and_real_core_scanner():
    policy = GuardrailPolicyConfig.from_dict(
        {"scanner_configs": [{"type": "ban_substrings", "substrings": ["secret"]}]}
    )
    assert policy.action == "block"
    assert policy.block_message == DEFAULT_BLOCK_MESSAGE
    assert policy.scanner_configs[0].action_on_hit == "block"
    result = policy.build_scanners()[0].scan("", "a secret")
    assert not result.valid
    assert result.output == "a secret"


def test_policy_actions_order_and_normalization():
    policy = GuardrailPolicyConfig.from_dict(
        {
            "action": "redact",
            "blockMessage": "Blocked by guardrail policy.",
            "scanner_configs": [
                {
                    "type": "BanSubstrings",
                    "substrings": ["secret"],
                    "caseSensitive": True,
                },
                {"type": "regex", "patterns": ["token"], "action": "block"},
            ],
        }
    )
    assert policy.block_message == "Blocked by guardrail policy."
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
    "scanner_config",
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
def test_adapter_matches_direct_shared_core(action, scanner_config):
    policy = GuardrailPolicyConfig.from_dict(
        {"action": action, "scanner_configs": [scanner_config]}
    )
    adapted = policy.build_scanners()[0]
    direct = build_scanner(parse_scanner_config(scanner_config, default_action=action))
    for text in ("safe output", "a secret", "alice@example.com", '{"ok": 1}'):
        assert adapted.scan("", text) == direct.scan("", text)


@pytest.mark.parametrize(
    "raw",
    [
        None,
        [],
        {},
        {"scanner_configs": None},
        {"scanner_configs": []},
        {"scanner_configs": {}},
        {"scanner_configs": [None]},
        {"scanner_configs": ["regex"]},
        {"scanner_configs": [{"type": "invisible_text"}], "action": "ignore"},
        {"scanner_configs": [{"type": "invisible_text"}], "action": None},
        {"scanner_configs": [{"type": "invisible_text"}], "blockMessage": ""},
        {"scanner_configs": [{"type": "invisible_text"}], "blockMessage": " "},
        {"scanner_configs": [{"type": "invisible_text"}], "blockMessage": 1},
        {"scanner_configs": [{"type": "invisible_text"}], "failOpen": True},
        {"scanner_configs": [{"type": "invisible_text"}], 1: True},
    ],
)
def test_invalid_guardrail_policy_config_is_rejected(raw):
    with pytest.raises(PolicyConfigError):
        GuardrailPolicyConfig.from_dict(raw)


@pytest.mark.parametrize("include_scanner_configs", [False, True])
def test_old_scanners_key_is_rejected(include_scanner_configs):
    raw = {"scanners": [{"type": "invisible_text"}]}
    if include_scanner_configs:
        raw["scanner_configs"] = [{"type": "invisible_text"}]
    with pytest.raises(
        PolicyConfigError,
        match=r"unknown guardrail policy configuration fields: \['scanners'\]",
    ):
        GuardrailPolicyConfig.from_dict(raw)


def test_non_mapping_scanner_config_error_has_entry_context():
    with pytest.raises(
        PolicyConfigError, match=r"scanner_configs\[1\] must be a mapping"
    ):
        GuardrailPolicyConfig.from_dict(
            {"scanner_configs": [{"type": "invisible_text"}, None]}
        )


@pytest.mark.parametrize(
    "scanner_config",
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
def test_core_validation_errors_have_entry_context(scanner_config):
    with pytest.raises(PolicyConfigError, match=r"scanner_configs\[1\]") as exc:
        GuardrailPolicyConfig.from_dict(
            {"scanner_configs": [{"type": "invisible_text"}, scanner_config]}
        )
    assert isinstance(exc.value.__cause__, ValueError)


def test_validation_does_not_build_scanners(monkeypatch):
    builder = Mock()
    monkeypatch.setattr(policy_module, "build_scanner", builder)
    with pytest.raises(PolicyConfigError):
        GuardrailPolicyConfig.from_dict(
            {"scanner_configs": [{"type": "invisible_text"}, {"type": "unknown"}]}
        )
    builder.assert_not_called()


def test_build_failure_propagates(monkeypatch):
    failure = RuntimeError("scanner construction failed")
    monkeypatch.setattr(policy_module, "build_scanner", Mock(side_effect=failure))
    policy = GuardrailPolicyConfig.from_dict(
        {"scanner_configs": [{"type": "invisible_text"}]}
    )
    with pytest.raises(RuntimeError) as exc:
        policy.build_scanners()
    assert exc.value is failure
