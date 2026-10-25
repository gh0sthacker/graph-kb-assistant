"""ChatGPT connection uses only a fake Hermes and temporary profile state."""
from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess

import pytest


ROOT = Path(__file__).resolve().parents[1]
CONNECT = ROOT / "scripts" / "connect-chatgpt"
REMOVED_ENV = (
    "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENROUTER_API_KEY", "OPENROUTER_BASE_URL",
    "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
    "HERMES_INFERENCE_MODEL", "HERMES_CODEX_BASE_URL",
)
EXPECTED_CONFIG = [
    ["config", "set", "auth.adopt_external_logins", "false"],
    ["config", "set", "model.provider", "openai-codex"],
    ["config", "set", "model.base_url", "https://chatgpt.com/backend-api/codex"],
]


def fake_environment(tmp_path: Path) -> tuple[dict[str, str], Path, Path]:
    data = tmp_path / "assistant-data"
    profile = data / "hermes"
    executable = data / "hermes-agent" / ".hermes" / "bin" / "hermes"
    executable.parent.mkdir(parents=True)
    calls = tmp_path / "calls.jsonl"
    executable.write_text(
        "#!/usr/bin/env python3\n"
        "import json, os, sys\n"
        "from pathlib import Path\n"
        "args = sys.argv[1:]\n"
        "assert args[:2] == ['--in', os.environ['BB_TEST_REPO']]\n"
        "args = args[2:]\n"
        "record = {'args': args, 'home': os.environ['HERMES_HOME'], "
        "'provider': os.environ.get('HERMES_INFERENCE_PROVIDER'), "
        "'removed': [k for k in json.loads(os.environ['BB_TEST_REMOVED']) if k in os.environ]}\n"
        "with Path(os.environ['BB_TEST_CALLS']).open('a') as f:\n"
        "    f.write(json.dumps(record) + '\\n')\n"
        "if args[:2] == ['config', 'set']:\n"
        "    if args[2] == os.environ.get('BB_TEST_FAIL_KEY'):\n"
        "        raise SystemExit(19)\n"
        "elif args[:5] == ['auth', 'add', 'openai-codex', '--type', 'oauth']:\n"
        "    raise SystemExit(int(os.environ.get('BB_TEST_AUTH_EXIT', '0')))\n"
        "else:\n"
        "    raise SystemExit('Unexpected fake Hermes invocation')\n",
        encoding="utf-8",
    )
    executable.chmod(0o700)
    # Never inherit private profile paths, credentials, or the user's HOME.
    env = {
        "PATH": os.environ["PATH"],
        "HOME": str(tmp_path / "synthetic-home"),
        "BB_ASSISTANT_DATA_HOME": str(data),
        "BB_TEST_REPO": str(ROOT),
        "BB_TEST_CALLS": str(calls),
        "BB_TEST_REMOVED": json.dumps(REMOVED_ENV),
        "HERMES_INFERENCE_PROVIDER": "synthetic-wrong-provider",
        **{key: "synthetic-inherited-value" for key in REMOVED_ENV},
    }
    return env, calls, profile


def invoke(env: dict[str, str], *args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(CONNECT), *args], cwd=ROOT, env=env,
        capture_output=True, text=True, check=False,
    )


@pytest.mark.parametrize("options", [
    [], ["--browser"], ["--no-browser"],
    ["--browser", "--no-browser", "--timeout", "42.5"],
    ["--browser", "--timeout=60"],
])
def test_fresh_oauth_route_and_external_profile(tmp_path: Path, options: list[str]) -> None:
    env, calls, profile = fake_environment(tmp_path)
    result = invoke(env, *options)
    assert result.returncode == 0, result.stderr
    records = [json.loads(line) for line in calls.read_text().splitlines()]
    expected_options = ["--browser", "--timeout", "60"] if "--timeout=60" in options else options
    assert [r["args"] for r in records] == EXPECTED_CONFIG + [
        ["auth", "add", "openai-codex", "--type", "oauth", *expected_options]
    ]
    assert all(r["home"] == str(profile) for r in records)
    assert all(r["provider"] == "openai-codex" for r in records)
    assert all(r["removed"] == [] for r in records)
    config = (profile / "config.yaml").read_text()
    assert "mcp-bb_assistant" in config
    assert "write_approval: true" in config
    assert not (Path(env["HOME"]) / ".codex").exists()


@pytest.mark.parametrize("options", [
    ["--provider", "openrouter"], ["--type", "api-key"], ["--insecure"],
    ["--timeout"], ["--timeout", "nan"], ["--timeout=-1"], ["--timeout", "0"],
])
def test_bad_options_cannot_reach_hermes(tmp_path: Path, options: list[str]) -> None:
    env, calls, profile = fake_environment(tmp_path)
    result = invoke(env, *options)
    assert result.returncode == 2
    assert not calls.exists()
    assert not profile.exists()


def test_help_does_not_initialize_profile(tmp_path: Path) -> None:
    env, calls, profile = fake_environment(tmp_path)
    result = invoke(env, "--help")
    assert result.returncode == 0
    assert "device code" in result.stdout
    assert not calls.exists()
    assert not profile.exists()


@pytest.mark.parametrize("key", [row[2] for row in EXPECTED_CONFIG])
def test_config_failure_stops_before_login(tmp_path: Path, key: str) -> None:
    env, calls, _ = fake_environment(tmp_path)
    env["BB_TEST_FAIL_KEY"] = key
    result = invoke(env)
    assert result.returncode == 19
    records = [json.loads(line) for line in calls.read_text().splitlines()]
    assert not any(r["args"][0] == "auth" for r in records)


def test_auth_exit_status_is_preserved(tmp_path: Path) -> None:
    env, _, _ = fake_environment(tmp_path)
    env["BB_TEST_AUTH_EXIT"] = "130"
    result = invoke(env)
    assert result.returncode == 130
