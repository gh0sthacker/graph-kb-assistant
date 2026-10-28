"""Offline command-line contract tests for KB snapshot import."""
from __future__ import annotations

from contextlib import redirect_stderr, redirect_stdout
import io
import json
from pathlib import Path
import tempfile
import unittest
from typing import Any
from unittest.mock import patch

from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.cli import _embedding_candidate_index_path, main
from bb_assistant.kb.index import KBIndex
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


class EmbeddingSourceFilterCLITests(unittest.TestCase):
    class _FixtureEmbedder:
        model_name = "synthetic-test-model"

        def __init__(self, **kwargs) -> None:
            pass

        def embed_documents_batch(self, texts, *, batch_size: int):
            return [[1.0, 0.0] for _ in texts]

    class _NoopThermalMonitor:
        def wait_for_cool_dwell(self) -> None:
            pass

        def check_sustained(self) -> None:
            pass

    def _candidate_index(self, root: Path) -> Path:
        candidate = root / "data" / "kb" / "candidates" / "filter-test" / "index.sqlite3"
        index = KBIndex(candidate)
        documents = tuple(
            SourceDocument.create(
                source_name=family,
                source_id=f"{family}-{ordinal}",
                title=f"{family} fixture {ordinal}",
                text=(
                    f"""id: {family}-{ordinal}
info:
  name: {family} fixture {ordinal}
  severity: info
"""
                    if family == "nuclei"
                    else f"Synthetic {family} record number {ordinal}."
                ),
                metadata={"document_role": "content"},
            )
            for family in ("nvd", "nuclei", "owasp")
            for ordinal in range(2)
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        self.assertEqual(len(chunks), 6)
        self.assertEqual({family: sum(chunk.source_name == family for chunk in chunks)
                          for family in ("nvd", "nuclei", "owasp")},
                         {"nvd": 2, "nuclei": 2, "owasp": 2})
        index.replace_snapshot(chunks, "f" * 64, sources=documents)
        return candidate

    def _run_embed(self, candidate: Path, *filter_args: str) -> tuple[dict[str, Any], str]:
        output = io.StringIO()
        progress = io.StringIO()
        with patch(
            "bb_assistant.kb.embeddings.RedAmonEmbedder", self._FixtureEmbedder,
        ), patch(
            "bb_assistant.kb.thermal.ThermalMonitor", return_value=self._NoopThermalMonitor(),
        ), redirect_stdout(output), redirect_stderr(progress):
            exit_code = main([
                "--index-path", str(candidate), "embed", "--thermal-sensor", "unused-fixture-sensor",
                *filter_args,
            ])
        self.assertEqual(exit_code, 0)
        return json.loads(output.getvalue()), progress.getvalue()

    def test_exclude_sources_embeds_only_remaining_families_with_filtered_progress_total(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            candidate = self._candidate_index(Path(temporary))
            result, progress = self._run_embed(candidate, "--exclude-sources", "nvd")

        self.assertEqual(result["embedded_chunks"], 4)
        self.assertEqual(progress.strip(), "embedded 4/4 KB chunks")
        coverage = result["vector_coverage_by_source"]
        self.assertEqual(coverage["nvd"], {"chunks": 2, "vectors": 0})
        self.assertEqual(coverage["nuclei"], {"chunks": 2, "vectors": 2})
        self.assertEqual(coverage["owasp"], {"chunks": 2, "vectors": 2})
        self.assertFalse(result["vector_storage_complete"])

    def test_filtered_embedding_remains_staged_until_other_families_are_embedded(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            candidate = self._candidate_index(Path(temporary))
            partial, _ = self._run_embed(candidate, "--exclude-sources", "nvd")
            complete, progress = self._run_embed(candidate, "--sources", "nvd")

        self.assertFalse(partial["vector_storage_complete"])
        self.assertEqual(complete["embedded_chunks"], 6)
        self.assertTrue(complete["vector_storage_complete"])
        self.assertEqual(progress.strip(), "embedded 2/2 KB chunks")

    def test_sources_embeds_only_selected_family_with_filtered_progress_total(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            candidate = self._candidate_index(Path(temporary))
            result, progress = self._run_embed(candidate, "--sources", "nuclei")

        self.assertEqual(result["embedded_chunks"], 2)
        self.assertEqual(progress.strip(), "embedded 2/2 KB chunks")

    def test_sources_accepts_comma_separated_family_list(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            candidate = self._candidate_index(Path(temporary))
            result, progress = self._run_embed(candidate, "--sources", "nuclei,owasp")

        self.assertEqual(result["embedded_chunks"], 4)
        self.assertEqual(progress.strip(), "embedded 4/4 KB chunks")
        coverage = result["vector_coverage_by_source"]
        self.assertEqual(coverage["nvd"], {"chunks": 2, "vectors": 0})

    def test_sources_and_exclude_sources_cannot_be_combined(self) -> None:
        error = io.StringIO()
        with redirect_stderr(error), self.assertRaises(SystemExit) as raised:
            main([
                "--index-path", "/unused/candidate/index.sqlite3", "embed",
                "--thermal-sensor", "unused-fixture-sensor",
                "--sources", "nuclei", "--exclude-sources", "nvd",
            ])

        self.assertEqual(raised.exception.code, 2)
        self.assertIn("--sources and --exclude-sources cannot be used together", error.getvalue())
