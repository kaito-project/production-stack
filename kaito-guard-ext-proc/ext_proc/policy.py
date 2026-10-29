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

"""Validate gateway policy settings and delegate scanner construction to core."""

from collections.abc import Mapping
from dataclasses import dataclass

from guardrail_core.config import (
    ParsedScannerConfig,
    build_scanner,
    parse_scanner_config,
)
from guardrail_core.scanning import Scanner

DEFAULT_BLOCK_MESSAGE = "The model output was blocked by output guardrails."


class PolicyConfigError(ValueError):
    """The gateway policy cannot be safely applied."""


@dataclass(frozen=True)
class GatewayPolicy:
    action: str
    block_message: str
    scanner_configs: tuple[ParsedScannerConfig, ...]

    @classmethod
    def from_dict(cls, raw: Mapping[str, object]) -> "GatewayPolicy":
        """Validate a decoded policy; reject rather than skip invalid entries."""
        if not isinstance(raw, Mapping):
            raise PolicyConfigError("gateway policy must be a mapping")
        if any(not isinstance(key, str) for key in raw):
            raise PolicyConfigError("gateway policy keys must be strings")
        unknown = set(raw) - {"action", "blockMessage", "scanners"}
        if unknown:
            raise PolicyConfigError(f"unknown gateway policy fields: {sorted(unknown)}")
        action = raw.get("action", "block")
        if not isinstance(action, str) or action not in {"block", "redact"}:
            raise PolicyConfigError("policy action must be 'block' or 'redact'")
        block_message = raw.get("blockMessage", DEFAULT_BLOCK_MESSAGE)
        if not isinstance(block_message, str) or not block_message.strip():
            raise PolicyConfigError("blockMessage must be a non-empty string")
        scanners = raw.get("scanners")
        if not isinstance(scanners, list) or not scanners:
            raise PolicyConfigError("scanners must be a non-empty list")
        configs: list[ParsedScannerConfig] = []
        for index, scanner in enumerate(scanners):
            if not isinstance(scanner, Mapping):
                raise PolicyConfigError(f"scanners[{index}] must be a mapping")
            try:
                configs.append(parse_scanner_config(scanner, default_action=action))
            except ValueError as exc:
                raise PolicyConfigError(f"invalid scanners[{index}]: {exc}") from exc
        return cls(
            action=action,
            block_message=block_message,
            scanner_configs=tuple(configs),
        )

    def build_scanners(self) -> tuple[Scanner, ...]:
        """Build scanners in policy order; construction failures propagate."""
        return tuple(
            build_scanner(config, default_action=self.action)
            for config in self.scanner_configs
        )
