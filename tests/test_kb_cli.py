"""Offline command-line contract tests for KB snapshot import."""
from __future__ import annotations

from contextlib import redirect_stdout
import io
import json
from pathlib import Path
import tempfile
import unittest

from bb_assistant.kb.cli import _embedding_candidate_index_path, main
from bb_assistant.kb.redamon_sources import RedAmonSourceAdapter, SourceBatch, SourceRecord, SourceRequest
from bb_assistant.kb.rebuild import export_redamon_snapshot


class ImportSnapshotCLITests(unittest.TestCase):
    def test_imports_verified_snapshot_without_fetch_and_preserves_partial_exit(self) -> None:
        revision = "a" * 40
        manifest = {
            "profile": "full",
            "sources": [{
                "id": "tool_docs",
                "kind": "local_markdown",
                "revision": revision,
                "repository_license_id": "MIT",
                "license_evidence": "fixture-license",
                "license_status": "confirmed_at_pinned_revision",
                "include_categories": ["tooling"],
                "exclude": [],
            }],
        }

        def provider(request: SourceRequest) -> SourceBatch:
            self.assertEqual(request.source_name, "tool_docs")
            return SourceBatch(
                source_revision=revision,
                records=(
                    SourceRecord(
                        source_id="fixture-doc",
                        title="Fixture doc",
                        original_text="Synthetic snapshot import fixture.",
                        source_locator="fixture/doc.md",
                        metadata={"category_dir": "tooling"},
                    ),
                    SourceRecord(
                        source_id="NOTICE:license",
                        title="MIT license",
                        original_text="MIT fixture notice.",
                        source_locator="LICENSE",
                        document_role="license_notice",
                    ),
                ),
                expected_count=1,
                fetched_count=1,
                parsed_count=1,
                complete=True,
                artifact_sha256="b" * 64,
                applied_filters={},
                notices_complete=True,
            )

        snapshot = RedAmonSourceAdapter(
            manifest,
            {"tool_docs": provider},
            source_manifest_sha256="c" * 64,
        ).build()

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            exported = export_redamon_snapshot(snapshot, root / "snapshots")
            snapshot_dir = root / "snapshots" / str(exported["snapshot_id"])
            output = io.StringIO()
            with redirect_stdout(output):
                exit_code = main([
                    "--index-path", str(root / "index.sqlite3"),
                    "import-snapshot", str(snapshot_dir),
                ])

        result = json.loads(output.getvalue())
        self.assertEqual(exit_code, 3)
        self.assertEqual(result["coverage_state"], "PARTIAL")
        self.assertEqual(result["source_documents"], 2)  # Content plus retained license provenance.
        self.assertGreater(result["chunks"], 0)


class EmbeddingDestinationGuardTests(unittest.TestCase):
    def test_embedding_only_accepts_existing_candidate_index(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "data" / "kb"
            candidate = root / "candidates" / "full-test" / "index.sqlite3"
            candidate.parent.mkdir(parents=True)
            candidate.write_bytes(b"candidate fixture")
            self.assertEqual(_embedding_candidate_index_path(candidate), candidate.resolve())

            active = root / "index.sqlite3"
            active.parent.mkdir(parents=True, exist_ok=True)
            active.write_bytes(b"active fixture")
            with self.assertRaisesRegex(ValueError, "restricted to kb/candidates"):
                _embedding_candidate_index_path(active)

    def test_embedding_refuses_missing_candidate_before_database_creation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            missing = Path(temporary) / "data" / "kb" / "candidates" / "run" / "index.sqlite3"
            with self.assertRaisesRegex(ValueError, "must already exist"):
                _embedding_candidate_index_path(missing)
            self.assertFalse(missing.exists())
