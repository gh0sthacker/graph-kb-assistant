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
    "bb_graph_neighborhood",
    "bb_graph_slice",
    "bb_kb_stats",
    "bb_kb_search",
    "bb_kb_get_source",
    "bb_get_capture",
    "bb_consult_create_session",
    "bb_consult_compare",
    "bb_consult_record_proposal",
    "bb_consult_validate_proposal",
    "bb_candidate_save",
    "bb_consult_resume",
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
            "BB_ASSISTANT_KB_INDEX": str(self.data / "kb" / "index.sqlite3"),
            "BB_ASSISTANT_HERMES_HOME": str(self.profile),
            "BB_ASSISTANT_HERMES_INSTALL_DIR": str(self.install),
            "CAIDO_ACCESS_TOKEN": "synthetic-should-not-be-forwarded",
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
        env_block = (
            mcp_block.split("\n    env:\n", 1)[1]
            .split("\n    supports_parallel_tool_calls:", 1)[0]
        )
        self.assertIn(f'BB_ASSISTANT_DATA_HOME: "{self.data}"', env_block)
        self.assertIn(
            f'BB_ASSISTANT_KB_INDEX: "{self.data / "kb" / "index.sqlite3"}"',
            env_block,
        )
        self.assertEqual(
            re.findall(r"^      ([A-Z_]+):", env_block, flags=re.MULTILINE),
            ["BB_ASSISTANT_DATA_HOME", "BB_ASSISTANT_KB_INDEX"],
        )
        self.assertNotIn("synthetic-should-not-be-forwarded", config)
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

    def test_managed_tools_are_registered_by_the_local_mcp_server(self) -> None:
        env = os.environ.copy()
        env.pop("BB_ASSISTANT_ALLOW_RAW_CAPTURE_TOOL", None)
        result = subprocess.run(
            ["go", "run", "./cmd/bb-assistant", "mcp-tools"],
            cwd=ROOT,
            env=env,
            text=True,
            capture_output=True,
            check=False,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        catalog = json.loads(result.stdout)
        registered = {tool["name"] for tool in catalog["tools"]}
        self.assertEqual(set(EXPECTED_TOOLS) - registered, set())
        self.assertNotIn("bb_get_capture_raw", EXPECTED_TOOLS)

    def test_refuses_to_overwrite_unmanaged_profile(self) -> None:
        self.profile.mkdir(parents=True)
        config_path = self.profile / "config.yaml"
        original = "mcp_servers: {}\n"
        config_path.write_text(original, encoding="utf-8")

        result = self.run_setup()

        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(config_path.read_text(encoding="utf-8"), original)

    def test_upgrades_existing_managed_profile_without_replacing_other_settings(self) -> None:
        created = self.run_setup()
        self.assertEqual(created.returncode, 0, created.stderr)

        config_path = self.profile / "config.yaml"
        original = config_path.read_text(encoding="utf-8")
        old_profile = original.replace("        - bb_graph_neighborhood\n", "")
        for tool in EXPECTED_TOOLS[9:]:
            old_profile = old_profile.replace(f"        - {tool}\n", "")
        old_profile += "\ncustom_setting: keep-me\n"
        config_path.write_text(old_profile, encoding="utf-8")

        upgraded = self.run_setup()

        self.assertEqual(upgraded.returncode, 0, upgraded.stderr)
        updated = config_path.read_text(encoding="utf-8")
        self.assertEqual(updated.count("        - bb_graph_neighborhood\n"), 1)
        mcp_block = updated.split("  bb_assistant:\n", 1)[1].split("\nskills:\n", 1)[0]
        self.assertEqual(
            re.findall(r"^        - ([a-z0-9_]+)$", mcp_block, flags=re.MULTILINE),
            EXPECTED_TOOLS,
        )
        self.assertTrue(updated.endswith("\ncustom_setting: keep-me\n"))

        def remove_allowlist_entries(text: str) -> str:
            return re.sub(r"(?m)^        - bb_[a-z0-9_]+\n", "", text)

        self.assertEqual(
            remove_allowlist_entries(updated), remove_allowlist_entries(old_profile)
        )
        self.assertIn("Updated managed Hermes profile", upgraded.stdout)

        repeated = self.run_setup()
        self.assertEqual(repeated.returncode, 0, repeated.stderr)
        self.assertEqual(config_path.read_text(encoding="utf-8"), updated)

    def test_removes_stale_managed_tools_and_preserves_other_settings(self) -> None:
        created = self.run_setup()
        self.assertEqual(created.returncode, 0, created.stderr)

        config_path = self.profile / "config.yaml"
        original = config_path.read_text(encoding="utf-8")
        stale_profile = original.replace(
            "        - bb_candidate_save\n",
            "        - bb_candidate_save\n        - bb_deprecated_tool\n",
        ) + "\ncustom_setting: keep-me\n"
        config_path.write_text(stale_profile, encoding="utf-8")

        upgraded = self.run_setup()

        self.assertEqual(upgraded.returncode, 0, upgraded.stderr)
        updated = config_path.read_text(encoding="utf-8")
        self.assertNotIn("bb_deprecated_tool", updated)
        self.assertTrue(updated.endswith("\ncustom_setting: keep-me\n"))
        mcp_block = updated.split("  bb_assistant:\n", 1)[1].split("\nskills:\n", 1)[0]
        self.assertEqual(
            re.findall(r"^        - ([a-z0-9_]+)$", mcp_block, flags=re.MULTILINE),
            EXPECTED_TOOLS,
        )

    def test_refuses_malformed_or_duplicate_managed_allowlists_without_editing(self) -> None:
        created = self.run_setup()
        self.assertEqual(created.returncode, 0, created.stderr)
        config_path = self.profile / "config.yaml"
        original = config_path.read_text(encoding="utf-8")

        malformed = original.replace(
            "        - bb_surface_map\n", "        - bb surface map\n", 1
        )
        config_path.write_text(malformed, encoding="utf-8")
        result = self.run_setup()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("allowlist is ambiguous", result.stderr)
        self.assertEqual(config_path.read_text(encoding="utf-8"), malformed)

        duplicate = original.replace(
            "        - bb_surface_map\n", "        - bb_surface_map\n        - bb_surface_map\n", 1
        )
        config_path.write_text(duplicate, encoding="utf-8")
        result = self.run_setup()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("allowlist is ambiguous", result.stderr)
        self.assertEqual(config_path.read_text(encoding="utf-8"), duplicate)


if __name__ == "__main__":
    unittest.main()
