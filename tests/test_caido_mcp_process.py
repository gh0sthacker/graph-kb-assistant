"""Pinned executable checks for the Caido MCP importer subprocess."""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from bb_assistant.caido_mcp_process import (
    CaidoMCPProcessClient,
    _EXPECTED_SERVER_VERSION,
    _verified_server_command,
)


class VerifiedCaidoServerCommandTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="bb-caido-server-pin-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.executable = self.root / "caido-mcp-server"

    def write_executable(self, version: str) -> str:
        self.executable.write_text(
            "#!/bin/sh\n"
            "if [ \"$1\" = \"--version\" ]; then\n"
            f"  printf '%s\\n' 'caido-mcp-server version {version}'\n"
            "  exit 0\n"
            "fi\n"
            "exit 1\n",
            encoding="utf-8",
        )
        self.executable.chmod(0o700)
        return hashlib.sha256(self.executable.read_bytes()).hexdigest()

    def pin_environment(self, sha256: str) -> None:
        self.env = patch.dict(
            os.environ,
            {"PATH": str(self.root), "BB_ASSISTANT_CAIDO_MCP_SHA256": sha256},
            clear=False,
        )
        self.env.start()
        self.addCleanup(self.env.stop)

    def test_requires_expected_digest(self) -> None:
        self.write_executable(_EXPECTED_SERVER_VERSION)
        self.pin_environment("")
        with self.assertRaisesRegex(ValueError, "BB_ASSISTANT_CAIDO_MCP_SHA256"):
            _verified_server_command("caido-mcp-server")

    def test_rejects_digest_mismatch(self) -> None:
        self.write_executable(_EXPECTED_SERVER_VERSION)
        self.pin_environment("0" * 64)
        with self.assertRaisesRegex(ValueError, "SHA-256 does not match"):
            _verified_server_command("caido-mcp-server")

    def test_rejects_unpinned_version_after_digest_matches(self) -> None:
        digest = self.write_executable("dev")
        self.pin_environment(digest)
        with self.assertRaisesRegex(ValueError, _EXPECTED_SERVER_VERSION):
            _verified_server_command("caido-mcp-server")

    def test_returns_resolved_executable_when_digest_and_version_match(self) -> None:
        digest = self.write_executable(_EXPECTED_SERVER_VERSION)
        self.pin_environment(digest)
        self.assertEqual(
            _verified_server_command("caido-mcp-server"),
            str(self.executable.resolve()),
        )

    def test_process_bridge_rejects_every_non_history_tool(self) -> None:
        client = object.__new__(CaidoMCPProcessClient)
        for name in (
            "caido_send_request", "caido_edit_request", "caido_batch_send",
            "caido_automate_task_control", "caido_create_finding",
        ):
            with self.subTest(tool=name):
                with self.assertRaisesRegex(ValueError, "history read tools only"):
                    client.call_tool(name, {})


if __name__ == "__main__":
    unittest.main()
