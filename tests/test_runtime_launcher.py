import os
import subprocess
from pathlib import Path

import pytest


REPO_ROOT = Path(__file__).resolve().parents[1]


def make_fake_go(tmp_path: Path) -> tuple[dict[str, str], Path]:
    binary_dir = tmp_path / "bin"
    binary_dir.mkdir()
    args_file = tmp_path / "go-args.txt"
    go_binary = binary_dir / "go"
    go_binary.write_text('#!/bin/sh\nprintf "%s\\n" "$*" > "$GO_ARGS_FILE"\n')
    go_binary.chmod(0o700)
    env = os.environ.copy()
    env.update(
        {
            "HOME": str(tmp_path / "home"),
            "XDG_DATA_HOME": str(tmp_path / "xdg"),
            "GO_ARGS_FILE": str(args_file),
            "PATH": f"{binary_dir}:{env['PATH']}",
        }
    )
    env.pop("BB_ASSISTANT_NEO4J_ENV_FILE", None)
    env.pop("BB_ASSISTANT_NEO4J_URI", None)
    env.pop("BB_ASSISTANT_NEO4J_USERNAME", None)
    env.pop("BB_ASSISTANT_NEO4J_PASSWORD", None)
    env.pop("BB_ASSISTANT_NEO4J_DATABASE", None)
    env.pop("BB_ASSISTANT_GRAPH_BACKEND", None)
    env.pop("BB_ASSISTANT_DATA_HOME", None)
    env.pop("BB_ASSISTANT_RUNTIME_ENV_FILE", None)
    env.pop("BB_ASSISTANT_VAULT_KEY", None)
    return env, args_file


def test_status_can_report_missing_neo4j_configuration(tmp_path: Path) -> None:
    env, args_file = make_fake_go(tmp_path)
    result = subprocess.run(
        [str(REPO_ROOT / "scripts" / "bb-assistant"), "status"],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert args_file.read_text().strip() == "run ./cmd/bb-assistant status"


def test_neo4j_web_startup_requires_private_runtime_configuration(tmp_path: Path) -> None:
    env, args_file = make_fake_go(tmp_path)
    result = subprocess.run(
        [str(REPO_ROOT / "scripts" / "bb-assistant"), "web-ui"],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode != 0
    assert "Neo4j runtime environment file not found" in result.stderr
    assert not args_file.exists()


def test_sqlite_compatibility_mode_does_not_require_neo4j_file(tmp_path: Path) -> None:
    env, args_file = make_fake_go(tmp_path)
    env["BB_ASSISTANT_GRAPH_BACKEND"] = "sqlite"
    result = subprocess.run(
        [str(REPO_ROOT / "scripts" / "bb-assistant"), "web-ui"],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert args_file.read_text().strip() == "run ./cmd/bb-assistant web-ui"


def test_launcher_loads_private_application_runtime_environment(tmp_path: Path) -> None:
    env, args_file = make_fake_go(tmp_path)
    binary_dir = Path(env["PATH"].split(":", maxsplit=1)[0])
    go_binary = binary_dir / "go"
    key_state_file = tmp_path / "vault-key-state.txt"
    go_binary.write_text(
        '#!/bin/sh\n'
        'printf "%s\\n" "$*" > "$GO_ARGS_FILE"\n'
        'if [ -n "${BB_ASSISTANT_VAULT_KEY:-}" ]; then '
        'printf present > "$GO_KEY_STATE_FILE"; '
        'else printf missing > "$GO_KEY_STATE_FILE"; fi\n'
    )
    go_binary.chmod(0o700)
    env["GO_KEY_STATE_FILE"] = str(key_state_file)
    env["BB_ASSISTANT_GRAPH_BACKEND"] = "sqlite"
    runtime_env = tmp_path / "runtime.env"
    runtime_env.write_text("BB_ASSISTANT_VAULT_KEY=synthetic-test-key\n")
    runtime_env.chmod(0o600)
    env["BB_ASSISTANT_RUNTIME_ENV_FILE"] = str(runtime_env)

    result = subprocess.run(
        [str(REPO_ROOT / "scripts" / "bb-assistant"), "web-ui"],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert args_file.read_text().strip() == "run ./cmd/bb-assistant web-ui"
    assert key_state_file.read_text() == "present"


@pytest.mark.parametrize("use_xdg", [True, False], ids=["xdg", "home-fallback"])
def test_launcher_loads_runtime_environment_from_canonical_default(
    tmp_path: Path, use_xdg: bool
) -> None:
    env, args_file = make_fake_go(tmp_path)
    if use_xdg:
        user_data = Path(env["XDG_DATA_HOME"])
    else:
        env.pop("XDG_DATA_HOME")
        user_data = Path(env["HOME"]) / ".local" / "share"
    data_root = user_data / "bb-graph-assistant"
    data_root.mkdir(parents=True)
    runtime_env = data_root / "runtime.env"
    runtime_env.write_text("BB_ASSISTANT_VAULT_KEY=synthetic-canonical-key\n")
    runtime_env.chmod(0o600)
    obsolete_directory = data_root / "graph-kb-assistant"
    obsolete_directory.mkdir()
    (obsolete_directory / "runtime.env").write_text(
        "BB_ASSISTANT_VAULT_KEY=synthetic-obsolete-key\n"
    )
    key_state_file = tmp_path / "vault-key-state.txt"
    go_binary = Path(env["PATH"].split(":", maxsplit=1)[0]) / "go"
    go_binary.write_text(
        '#!/bin/sh\n'
        'printf "%s\n" "$*" > "$GO_ARGS_FILE"\n'
        'if [ "${BB_ASSISTANT_VAULT_KEY:-}" = synthetic-canonical-key ]; then '
        'printf canonical > "$GO_KEY_STATE_FILE"; '
        'else printf incorrect > "$GO_KEY_STATE_FILE"; fi\n'
    )
    go_binary.chmod(0o700)
    env["GO_KEY_STATE_FILE"] = str(key_state_file)
    env["BB_ASSISTANT_GRAPH_BACKEND"] = "sqlite"

    result = subprocess.run(
        [str(REPO_ROOT / "scripts" / "bb-assistant"), "web-ui"],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert args_file.read_text().strip() == "run ./cmd/bb-assistant web-ui"
    assert key_state_file.read_text() == "canonical"
    assert (obsolete_directory / "runtime.env").read_text() == (
        "BB_ASSISTANT_VAULT_KEY=synthetic-obsolete-key\n"
    )


def test_launcher_uses_neo4j_configured_data_home_for_runtime_environment(
    tmp_path: Path,
) -> None:
    env, args_file = make_fake_go(tmp_path)
    user_data = Path(env["XDG_DATA_HOME"])
    canonical_root = user_data / "bb-graph-assistant"
    canonical_root.mkdir(parents=True)
    (canonical_root / "runtime.env").write_text(
        "BB_ASSISTANT_VAULT_KEY=synthetic-wrong-canonical-key\n"
    )

    configured_root = tmp_path / "neo4j-selected-data-root"
    configured_root.mkdir()
    runtime_env = configured_root / "runtime.env"
    runtime_env.write_text(
        "BB_ASSISTANT_VAULT_KEY=synthetic-selected-root-key\n"
    )
    runtime_env.chmod(0o600)
    neo4j_env = canonical_root / "neo4j.env"
    neo4j_env.write_text(
        "BB_ASSISTANT_GRAPH_BACKEND=neo4j\n"
        f"BB_ASSISTANT_DATA_HOME={configured_root}\n"
    )

    state_file = tmp_path / "runtime-state.txt"
    go_binary = Path(env["PATH"].split(":", maxsplit=1)[0]) / "go"
    go_binary.write_text(
        '#!/bin/sh\n'
        'printf "%s\\n" "$*" > "$GO_ARGS_FILE"\n'
        'printf "%s\\n" "$BB_ASSISTANT_DATA_HOME" > "$GO_RUNTIME_STATE_FILE"\n'
        'if [ "${BB_ASSISTANT_VAULT_KEY:-}" = synthetic-selected-root-key ]; then '
        'printf selected-root >> "$GO_RUNTIME_STATE_FILE"; '
        'else printf incorrect-key >> "$GO_RUNTIME_STATE_FILE"; fi\n'
    )
    go_binary.chmod(0o700)
    env.update(
        {
            "BB_ASSISTANT_NEO4J_ENV_FILE": str(neo4j_env),
            "GO_RUNTIME_STATE_FILE": str(state_file),
        }
    )

    result = subprocess.run(
        [str(REPO_ROOT / "scripts" / "bb-assistant"), "web-ui"],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert args_file.read_text().strip() == "run ./cmd/bb-assistant web-ui"
    assert state_file.read_text().splitlines() == [
        str(configured_root), "selected-root"
    ]


def test_launcher_preserves_explicit_runtime_configuration_precedence(
    tmp_path: Path,
) -> None:
    env, _ = make_fake_go(tmp_path)
    data_root = tmp_path / "explicit-data-root"
    data_root.mkdir()
    (data_root / "runtime.env").write_text(
        "BB_ASSISTANT_VAULT_KEY=synthetic-wrong-default-key\n"
    )
    runtime_env = tmp_path / "explicit-runtime.env"
    runtime_env.write_text(
        "BB_ASSISTANT_VAULT_KEY=synthetic-file-key\n"
        "BB_ASSISTANT_GRAPH_BACKEND=neo4j\n"
        f"BB_ASSISTANT_DATA_HOME={tmp_path / 'file-data-root'}\n"
        "BB_LAUNCHER_TEST_RUNTIME_MARKER=explicit-file\n"
    )
    runtime_env.chmod(0o600)
    neo4j_env = tmp_path / "synthetic-neo4j.env"
    neo4j_env.write_text(
        "BB_ASSISTANT_VAULT_KEY=synthetic-neo4j-key\n"
        "BB_ASSISTANT_GRAPH_BACKEND=neo4j\n"
        f"BB_ASSISTANT_DATA_HOME={tmp_path / 'neo4j-data-root'}\n"
    )
    state_file = tmp_path / "runtime-state.txt"
    go_binary = Path(env["PATH"].split(":", maxsplit=1)[0]) / "go"
    go_binary.write_text(
        '#!/bin/sh\n'
        'printf "%s\n" "$BB_ASSISTANT_DATA_HOME" "$BB_ASSISTANT_GRAPH_BACKEND" '
        '"$BB_LAUNCHER_TEST_RUNTIME_MARKER" > "$GO_RUNTIME_STATE_FILE"\n'
        'if [ "${BB_ASSISTANT_VAULT_KEY:-}" = synthetic-explicit-key ]; then '
        'printf explicit >> "$GO_RUNTIME_STATE_FILE"; '
        'else printf incorrect >> "$GO_RUNTIME_STATE_FILE"; fi\n'
    )
    go_binary.chmod(0o700)
    env.update(
        {
            "BB_ASSISTANT_DATA_HOME": str(data_root),
            "BB_ASSISTANT_GRAPH_BACKEND": "sqlite",
            "BB_ASSISTANT_VAULT_KEY": "synthetic-explicit-key",
            "BB_ASSISTANT_RUNTIME_ENV_FILE": str(runtime_env),
            "BB_ASSISTANT_NEO4J_ENV_FILE": str(neo4j_env),
            "GO_RUNTIME_STATE_FILE": str(state_file),
        }
    )
    result = subprocess.run(
        [str(REPO_ROOT / "scripts" / "bb-assistant"), "web-ui"],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert state_file.read_text().splitlines() == [
        str(data_root), "sqlite", "explicit-file", "explicit"
    ]
