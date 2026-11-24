"""Persistent stdio MCP client for the isolated Caido history importer."""
from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import threading
from typing import Any


_READ_TOOLS = frozenset({
    "caido_list_projects",
    "caido_select_project",
    "caido_list_requests",
    "caido_get_request",
})
_EXPECTED_SERVER_VERSION = "v4.3.0"
_SHA256 = re.compile(r"^[0-9a-f]{64}$")


class CaidoMCPConfigurationError(ValueError):
    """Safe, actionable local configuration error; contains no Caido traffic."""


def _verified_server_command(command: str, expected_sha256: str | None = None) -> str:
    """Resolve and verify the pinned Caido MCP executable before launching it."""
    resolved = shutil.which(command)
    if resolved is None:
        raise CaidoMCPConfigurationError("Configured Caido MCP server executable was not found")
    try:
        path = Path(resolved).resolve(strict=True)
    except OSError as exc:
        raise CaidoMCPConfigurationError("Configured Caido MCP server path is unavailable") from exc
    if not path.is_file() or not os.access(path, os.X_OK):
        raise CaidoMCPConfigurationError("Configured Caido MCP server is not an executable file")

    expected = expected_sha256 or os.environ.get("BB_ASSISTANT_CAIDO_MCP_SHA256")
    expected = expected.strip().lower() if isinstance(expected, str) else ""
    if not _SHA256.fullmatch(expected):
        raise CaidoMCPConfigurationError(
            "Set BB_ASSISTANT_CAIDO_MCP_SHA256 to the verified SHA-256 of the pinned v4.3.0 binary"
        )

    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    if not hmac.compare_digest(digest.hexdigest(), expected):
        raise CaidoMCPConfigurationError("Configured Caido MCP server SHA-256 does not match the pinned binary")

    # Probe version with a deliberately small environment; the importer's auth
    # token and sensitive-header setting are not needed for --version.
    probe_env = {"PATH": os.environ.get("PATH", "")}
    if os.environ.get("SYSTEMROOT"):
        probe_env["SYSTEMROOT"] = os.environ["SYSTEMROOT"]
    try:
        version = subprocess.run(
            [str(path), "--version"],
            check=False,
            capture_output=True,
            text=True,
            timeout=5,
            env=probe_env,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise CaidoMCPConfigurationError("Could not verify the Caido MCP server version") from exc
    reported = version.stdout.strip().split()
    reported_version = reported[-1].removeprefix("v") if reported else ""
    expected_version = _EXPECTED_SERVER_VERSION.removeprefix("v")
    if version.returncode != 0 or reported_version != expected_version:
        raise CaidoMCPConfigurationError(f"Caido MCP server must report version {_EXPECTED_SERVER_VERSION}")
    return str(path)


def _child_environment() -> dict[str, str]:
    """Pass only runtime necessities and Caido auth, not the whole parent env."""
    allowed = (
        "PATH", "HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "SYSTEMROOT",
        "CAIDO_URL", "CAIDO_ACCESS_TOKEN",
    )
    child = {name: os.environ[name] for name in allowed if os.environ.get(name)}
    if not child.get("CAIDO_ACCESS_TOKEN"):
        configured_token_file = os.environ.get("BB_ASSISTANT_CAIDO_TOKEN_FILE")
        token_file = (
            Path(configured_token_file).expanduser()
            if configured_token_file
            else Path.home() / ".caido-mcp" / "token.json"
        )
        if configured_token_file and not token_file.is_file():
            raise CaidoMCPConfigurationError("Configured Caido token file is unavailable")
        if token_file.exists():
            child["CAIDO_ACCESS_TOKEN"] = _read_caido_access_token(token_file)
    child.setdefault("CAIDO_URL", "http://127.0.0.1:8080")
    # This subprocess is owned by local graph ingestion, not the model-facing
    # MCP server. Sensitive header values are needed for parsing in memory;
    # only the explicit historical backfill persists them in the encrypted vault.
    child["CAIDO_ALLOW_SENSITIVE_HEADERS"] = "true"
    return child


def _read_caido_access_token(path: Path) -> str:
    """Read a current Caido login token without exposing it in service config."""
    try:
        if path.is_symlink():
            raise CaidoMCPConfigurationError("Caido token file must not be a symbolic link")
        metadata = path.stat()
        if not path.is_file():
            raise CaidoMCPConfigurationError("Caido token file is not a regular file")
        if os.name == "posix":
            if metadata.st_uid != os.geteuid() or metadata.st_mode & 0o077:
                raise CaidoMCPConfigurationError("Caido token file permissions are not private")
        payload = json.loads(path.read_text(encoding="utf-8"))
    except CaidoMCPConfigurationError:
        raise
    except (OSError, UnicodeError, json.JSONDecodeError):
        raise CaidoMCPConfigurationError("Caido token file cannot be read") from None

    if not isinstance(payload, dict):
        raise CaidoMCPConfigurationError("Caido token file has an unsupported format")
    access_token = payload.get("accessToken")
    expires_at = payload.get("expiresAt")
    if not isinstance(access_token, str) or not access_token or any(char in access_token for char in "\r\n\x00"):
        raise CaidoMCPConfigurationError("Caido token file has no usable access token")
    if not isinstance(expires_at, str):
        raise CaidoMCPConfigurationError("Caido token expiry is missing")
    try:
        expiry = datetime.fromisoformat(expires_at.replace("Z", "+00:00"))
        if expiry.tzinfo is None:
            raise ValueError
        if expiry.astimezone(timezone.utc) <= datetime.now(timezone.utc) + timedelta(minutes=5):
            raise CaidoMCPConfigurationError("Caido access token is expired or expiring; renew the Caido login")
    except CaidoMCPConfigurationError:
        raise
    except ValueError:
        raise CaidoMCPConfigurationError("Caido token expiry is invalid") from None
    return access_token


class CaidoMCPProcessClient:
    """Call only the Caido history read tools through a configured stdio server.

    The community server exposes additional mutation/replay tools, but they are
    never surfaced here: this process bridge accepts only four fixed tool names.
    """

    def __init__(
        self,
        *,
        command: str | None = None,
        expected_sha256: str | None = None,
        startup_timeout: float = 30.0,
        call_timeout: float = 180.0,
    ) -> None:
        requested_command = command or os.environ.get("BB_ASSISTANT_CAIDO_MCP_COMMAND") or "caido-mcp-server"
        if not isinstance(requested_command, str) or not requested_command.strip():
            raise ValueError("Caido MCP command must be a non-empty executable path")
        if startup_timeout <= 0 or call_timeout <= 0:
            raise ValueError("MCP timeouts must be positive")
        self._command = _verified_server_command(requested_command, expected_sha256)
        self._startup_timeout = startup_timeout
        self._call_timeout = call_timeout
        self._ready = threading.Event()
        self._thread: threading.Thread | None = None
        self._loop: asyncio.AbstractEventLoop | None = None
        self._stop_event: asyncio.Event | None = None
        self._client: Any = None
        self._startup_error: str | None = None

    def __enter__(self) -> "CaidoMCPProcessClient":
        if self._thread is not None:
            raise RuntimeError("Caido MCP client is already started")
        self._thread = threading.Thread(target=self._thread_main, name="bb-caido-mcp", daemon=True)
        self._thread.start()
        if not self._ready.wait(self._startup_timeout):
            self.close()
            raise RuntimeError("Caido MCP stdio server did not become ready")
        if self._startup_error is not None:
            self.close()
            raise RuntimeError(self._startup_error)
        return self

    def __exit__(self, _exc_type: Any, _exc: Any, _traceback: Any) -> None:
        self.close()

    def _thread_main(self) -> None:
        loop = asyncio.new_event_loop()
        self._loop = loop
        asyncio.set_event_loop(loop)
        self._stop_event = asyncio.Event()

        async def serve() -> None:
            try:
                from mcp import Client, StdioServerParameters

                params = StdioServerParameters(
                    command=self._command,
                    args=["serve"],
                    env=_child_environment(),
                )
                async with Client(params) as client:
                    self._client = client
                    self._ready.set()
                    await self._stop_event.wait()
            except BaseException:
                self._startup_error = "Could not start the configured Caido MCP stdio server"
                self._ready.set()

        task = loop.create_task(serve())
        task.add_done_callback(lambda _task: loop.stop())
        try:
            loop.run_forever()
        finally:
            self._client = None
            pending = asyncio.all_tasks(loop)
            for pending_task in pending:
                pending_task.cancel()
            if pending:
                loop.run_until_complete(asyncio.gather(*pending, return_exceptions=True))
            loop.close()

    def call_tool(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        """Synchronously call one allowlisted MCP tool and normalize its result."""
        if name not in _READ_TOOLS:
            raise ValueError("The Caido import bridge permits history read tools only")
        loop = self._loop
        client = self._client
        if loop is None or client is None or not loop.is_running():
            raise RuntimeError("Caido MCP stdio client is not connected")

        async def invoke() -> Any:
            return await client.call_tool(name, arguments)

        future = asyncio.run_coroutine_threadsafe(invoke(), loop)
        try:
            result = future.result(timeout=self._call_timeout)
        except Exception:
            future.cancel()
            raise RuntimeError("Caido MCP history read failed or timed out") from None
        structured = getattr(result, "structured_content", None)
        blocks = []
        for block in getattr(result, "content", ()):
            item: dict[str, Any] = {"type": getattr(block, "type", None)}
            text = getattr(block, "text", None)
            if isinstance(text, str):
                item["text"] = text
            blocks.append(item)
        return {
            "structuredContent": structured,
            "content": blocks,
            "isError": bool(getattr(result, "is_error", False)),
        }

    def close(self) -> None:
        loop = self._loop
        thread = self._thread
        if loop is not None and loop.is_running() and self._stop_event is not None:
            try:
                asyncio.run_coroutine_threadsafe(self._stop(), loop).result(timeout=10)
            except Exception:
                loop.call_soon_threadsafe(loop.stop)
        if thread is not None and thread.is_alive():
            thread.join(timeout=15)
        self._client = None

    async def _stop(self) -> None:
        if self._stop_event is not None:
            self._stop_event.set()
