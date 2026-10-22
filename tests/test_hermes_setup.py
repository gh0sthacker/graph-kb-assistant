"""Hermes app-profile generation stays private and least-privileged."""
from __future__ import annotations

import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SETUP_SCRIPT = ROOT / "scripts" / "setup-hermes"
EXPECTED_TOOLS = [
    "bb_list_projects",
    "bb_search_captures",
    "bb_surface_map",
    "bb_graph_slice",
    "bb_kb_stats",
    "bb_kb_search",
    "bb_kb_get_source",
    "bb_get_capture",
]


class HermesSetupTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="bb-hermes-profile-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.profile = self.root / "hermes-profile"
        self.install = self.root / "hermes-install"
        self.data = self.root / "assistant-data"

    def setup_environment(self) -> dict[str, str]:
        env = os.environ.copy()
        env.update({
            "BB_ASSISTANT_DATA_HOME": str(self.data),
            "BB_ASSISTANT_HERMES_HOME": str(self.profile),
            "BB_ASSISTANT_HERMES_INSTALL_DIR": str(self.install),
        })
        return env

    def run_setup(self) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [str(SETUP_SCRIPT)], cwd=ROOT, env=self.setup_environment(),
            text=True, capture_output=True, check=False,
        )

    def test_creates_idempotent_private_profile_with_exact_allowlist(self) -> None:
        first = self.run_setup()
        self.assertEqual(first.returncode, 0, first.stderr)

        config_path = self.profile / "config.yaml"
        config = config_path.read_text(encoding="utf-8")
        self.assertIn("# Managed by bb-graph-assistant setup-hermes.", config)

        toolset_block = config.split("toolsets:\n", 1)[1].split("\nmcp_servers:", 1)[0]
        self.assertEqual(
            re.findall(r"^  - ([a-z0-9_-]+)$", toolset_block, flags=re.MULTILINE),
            ["mcp-bb_assistant", "skills", "clarify"],
        )
        mcp_block = config.split("  bb_assistant:\n", 1)[1].split("\nskills:\n", 1)[0]
        self.assertEqual(
            re.findall(r"^        - ([a-z0-9_]+)$", mcp_block, flags=re.MULTILINE),
            EXPECTED_TOOLS,
        )
        self.assertNotIn("bb_get_capture_raw", mcp_block)
        self.assertIn("resources: false", mcp_block)
        self.assertIn("prompts: false", mcp_block)
        self.assertIn("supports_parallel_tool_calls: false", mcp_block)
        self.assertIn("trust: untrusted", mcp_block)
        command_line = next(line.strip() for line in mcp_block.splitlines() if line.strip().startswith("command:"))
        command = json.loads(command_line.split(":", 1)[1].strip())
        self.assertEqual(Path(command), ROOT / "scripts" / "bb-assistant-mcp")

        skills_line = next(line.strip() for line in config.splitlines() if line.strip().startswith("- ") and ".claude/skills" in line)
        skills_path = Path(json.loads(skills_line[2:].strip()))
        self.assertEqual(skills_path, ROOT / ".claude" / "skills")
        if os.name == "posix":
            self.assertEqual(stat.S_IMODE(config_path.stat().st_mode), 0o600)
            for directory in (self.profile, self.profile / "skills", self.profile / "pending" / "skills"):
                self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o700)

        second = self.run_setup()
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(config_path.read_text(encoding="utf-8"), config)

    def test_refuses_to_overwrite_unmanaged_profile(self) -> None:
        self.profile.mkdir(parents=True)
        config_path = self.profile / "config.yaml"
        original = "mcp_servers: {}\n"
        config_path.write_text(original, encoding="utf-8")

        result = self.run_setup()

        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(config_path.read_text(encoding="utf-8"), original)


if __name__ == "__main__":
    unittest.main()
