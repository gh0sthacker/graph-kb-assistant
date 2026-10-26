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


if __name__ == "__main__":
    unittest.main()
