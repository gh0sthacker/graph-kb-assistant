import os
import subprocess
from pathlib import Path


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
