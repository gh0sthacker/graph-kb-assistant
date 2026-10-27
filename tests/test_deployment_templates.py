from pathlib import Path
import unittest


class DeploymentTemplateTests(unittest.TestCase):
    def test_managed_service_wires_pinned_retrieval_model_worker(self) -> None:
        project_root = Path(__file__).resolve().parents[1]
        unit = (project_root / "deploy" / "graph-kb-assistant-system.service").read_text()
        self.assertIn("Environment=BB_ASSISTANT_KB_MODELS=1", unit)
        self.assertIn(
            "Environment=BB_ASSISTANT_KB_PYTHON="
            "/home/max/.local/share/bb-graph-assistant/kb-models-venv/bin/python",
            unit,
        )

    def test_ct108_sync_template_is_scoped_and_bounded(self) -> None:
        project_root = Path(__file__).resolve().parents[1]
        unit = (project_root / "systemd" / "graph-kb-assistant-caido-sync.service").read_text()
        self.assertIn("User=max", unit)
        self.assertIn("ReadWritePaths=/home/max/.local/share/bb-graph-assistant/graph-kb-assistant", unit)
        self.assertIn("--require-scope", unit)
        self.assertIn("--backfill-pages 1", unit)
        self.assertIn("--backfill-page-size 25", unit)
        self.assertIn("--backfill-body-limit 65536", unit)
        self.assertIn("--page-size 25", unit)
        self.assertIn("--body-limit 65536", unit)
        self.assertIn("CPUQuota=200%", unit)
        self.assertIn("MemoryMax=2G", unit)
        env = (project_root / "systemd" / "graph-kb-assistant-caido-sync.env.example").read_text()
        self.assertIn("BB_ASSISTANT_CAIDO_SCOPE_ORIGINS=https://demo.example.invalid:443", env)


if __name__ == "__main__":
    unittest.main()
