import sys
from unittest.mock import AsyncMock

import pytest

from ext_proc import server


@pytest.mark.parametrize("mode", [None, "BUFFERED", "STREAMED"])
def test_cli_loads_policy_before_serving(tmp_path, monkeypatch, mode):
    policy = tmp_path / "policy.json"
    policy.write_text(
        '{"action":"redact","scanner_configs":[{"type":"ban_substrings",'
        '"substrings":["secret"]}]}'
    )
    args = ["ext_proc.server", "--policy", str(policy)]
    if mode:
        args.extend(["--response-body-mode", mode])
    monkeypatch.setattr(sys, "argv", args)
    serve = AsyncMock()
    monkeypatch.setattr(server, "serve", serve)
    server.main()
    serve.assert_awaited_once()
    assert serve.await_args.kwargs["policy"].action == "redact"
    assert serve.await_args.kwargs["streaming"] is (mode == "STREAMED")


@pytest.mark.parametrize("contents", [None, "{", '{"scanner_configs":[]}'])
def test_bad_policy_does_not_start_server(tmp_path, monkeypatch, contents):
    policy = tmp_path / "policy.json"
    if contents is not None:
        policy.write_text(contents)
    monkeypatch.setattr(sys, "argv", ["ext_proc.server", "--policy", str(policy)])
    serve = AsyncMock()
    monkeypatch.setattr(server, "serve", serve)
    with pytest.raises(SystemExit) as exc:
        server.main()
    assert exc.value.code == 2
    serve.assert_not_called()


def test_explicit_mode_requires_policy(monkeypatch):
    monkeypatch.setattr(sys, "argv", ["ext_proc.server", "--response-body-mode", "STREAMED"])
    with pytest.raises(SystemExit) as exc:
        server.main()
    assert exc.value.code == 2
