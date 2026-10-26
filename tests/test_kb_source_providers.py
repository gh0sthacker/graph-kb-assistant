"""Offline source-provider fixtures; these tests never access upstream feeds."""
from __future__ import annotations

from io import BytesIO
import hashlib
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import yaml

from bb_assistant.kb.exploitdb import RedAmonExploitDBProvider
from bb_assistant.kb.gtfobins import RedAmonGTFOBinsProvider
from bb_assistant.kb.lolbas import RedAmonLOLBASProvider
from bb_assistant.kb.nuclei import RedAmonNucleiProvider
from bb_assistant.kb.owasp import RedAmonOWASPProvider
from bb_assistant.kb.pinned_github_archive import read_pinned_archive
from bb_assistant.kb.index import KBIndex
from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.redamon_local import RedAmonLocalToolDocsProvider
from bb_assistant.kb.rebuild import _source_document_from_record, export_redamon_snapshot, import_snapshot, rechunk_index
from bb_assistant.kb.redamon_sources import (
    RedAmonSourceAdapter,
    SourceBatch,
    SourceRecord,
    SourceRequest,
)


REVISION = "a" * 40
GTFO_URL = "https://github.com/GTFOBins/GTFOBins.github.io"
LOLBAS_URL = "https://github.com/LOLBAS-Project/LOLBAS"
OWASP_URL = "https://github.com/OWASP/wstg"
NUCLEI_URL = "https://github.com/projectdiscovery/nuclei-templates"
EXPLOITDB_URL = "https://gitlab.com/exploit-database/exploitdb"


def _archive(repository: str, files: dict[str, bytes], *, revision: str = REVISION) -> bytes:
    """Build a tiny GitHub-style archive in memory."""
    output = BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz") as archive:
        root = f"{repository}-{revision}"
        for relative, raw in files.items():
            info = tarfile.TarInfo(f"{root}/{relative}")
            info.size = len(raw)
            info.mtime = 0
            archive.addfile(info, BytesIO(raw))
    return output.getvalue()


def _request(name: str, url: str, filters: dict[str, object]) -> SourceRequest:
    return SourceRequest(
        name,
        "full",
        {"id": name, "url": url},
        REVISION,
        filters,
    )


class ArchiveProviderFixtureTests(unittest.TestCase):
    def test_gtfobins_and_lolbas_pass_canonical_adapter_with_required_notices(self) -> None:
        gtfo_archive = _archive(GTFO_URL.rsplit("/", 1)[-1], {
            "_gtfobins/curl.md": b"# curl\nFixture GTFOBins content.\n",
            "LICENSE": b"GPL-3.0 fixture license\n",
        })
        lolbas_archive = _archive("LOLBAS", {
            "yml/OSBinaries/Fixture.yml": b"Name: Fixture\nDescription: fixture content\n",
            "LICENSE": b"GPL-3.0 fixture license\n",
            "NOTICE.md": b"MITRE ATT&CK notice fixture\n",
        })
        manifest = {"profile": "full", "sources": [
            {
                "id": "gtfobins", "kind": "github_archive", "url": GTFO_URL,
                "revision": REVISION, "license_id": "GPL-3.0-only",
                "license_evidence": "fixture", "license_status": "confirmed_at_pinned_revision",
            },
            {
                "id": "lolbas", "kind": "github_archive", "url": LOLBAS_URL,
                "revision": REVISION, "license_id": "GPL-3.0-only",
                "license_evidence": "fixture", "license_status": "confirmed_at_pinned_revision",
            },
        ]}
        snapshot = RedAmonSourceAdapter(
            manifest,
            {
                "gtfobins": RedAmonGTFOBinsProvider(fetcher=lambda _url: gtfo_archive),
                "lolbas": RedAmonLOLBASProvider(fetcher=lambda _url: lolbas_archive),
            },
            source_manifest_sha256="b" * 64,
        ).build()

        coverage = {row["source_name"]: row for row in snapshot.source_coverage}
        self.assertEqual(coverage["gtfobins"]["state"], "done")
        self.assertEqual(coverage["lolbas"]["state"], "done")
        self.assertEqual(snapshot.coverage_state, "PARTIAL")  # Five families are intentionally absent.
        self.assertEqual(coverage["gtfobins"]["license_notice_count"], 1)
        self.assertEqual(coverage["lolbas"]["license_notice_count"], 2)
        self.assertEqual(
            {doc.source_id for doc in snapshot.documents if doc.source_name == "lolbas" and doc.source_id.startswith("NOTICE:")},
            {"NOTICE:license", "NOTICE:notice.md"},
        )
        self.assertTrue(any("MITRE ATT&CK" in doc.text for doc in snapshot.documents if doc.source_id == "NOTICE:notice.md"))

    def test_wstg_fixture_selects_test_cases_and_excludes_readmes(self) -> None:
        archive = _archive("wstg", {
            "LICENSE": b"CC-BY-SA fixture notice\n",
            "document/4-Web_Application_Security_Testing/01-Example.md": b"# Selected test\n",
            "document/4-Web_Application_Security_Testing/README.md": b"Do not index this README.\n",
            "document/3-The_OWASP_Testing_Framework/other.md": b"Outside selected test directory.\n",
        })
        request = _request("owasp", OWASP_URL, {
            "test_directory": "document/4-Web_Application_Security_Testing",
            "exclude_readme": True,
        })

        batch = RedAmonOWASPProvider(fetcher=lambda _url: archive)(request)
        content = [record for record in batch.records if record.document_role == "content"]
        self.assertEqual([record.source_id for record in content], [
            "document/4-Web_Application_Security_Testing/01-Example.md",
        ])
        self.assertTrue(batch.notices_complete)
        self.assertEqual(batch.expected_count, 1)
        self.assertEqual(batch.source_revision, REVISION)
        self.assertTrue(any(record.document_role == "license_notice" and "CC-BY-SA" in record.original_text for record in batch.records))

    def test_nuclei_fixture_preserves_yaml_and_applies_exclusions(self) -> None:
        archive = _archive("nuclei-templates", {
            "LICENSE.md": b"MIT fixture notice\n",
            "http/fixture.yaml": b"id: fixture-template\ninfo:\n  name: Fixture\n",
            ".github/workflows/fixture.yaml": b"excluded workflow\n",
            ".pre-commit-config.yaml": b"excluded config\n",
            ".pre-commit-config.yml": b"excluded config\n",
        })
        request = _request("nuclei", NUCLEI_URL, {
            "extensions": (".yaml", ".yml"),
            "exclude_prefixes": (".github/",),
            "exclude_paths": (".pre-commit-config.yaml", ".pre-commit-config.yml"),
        })

        batch = RedAmonNucleiProvider(fetcher=lambda _url: archive)(request)
        content = [record for record in batch.records if record.document_role == "content"]
        self.assertEqual([record.source_id for record in content], ["http/fixture.yaml"])
        self.assertEqual(content[0].original_text, "id: fixture-template\ninfo:\n  name: Fixture\n")
        self.assertTrue(batch.notices_complete)
        self.assertTrue(any(record.source_id == "NOTICE:license.md" for record in batch.records))

    def test_archive_reader_rejects_mismatched_root_traversal_and_symlinks(self) -> None:
        wrong_root = _archive("wrong-repository", {"_gtfobins/a.md": b"x"})
        with self.assertRaisesRegex(ValueError, "root does not match"):
            read_pinned_archive(
                wrong_root, repository_slug="GTFOBins.github.io", revision=REVISION,
                content_roots=("_gtfobins",), allowed_extensions=(".md",),
            )

        traversal = _archive("GTFOBins.github.io", {"../escape.md": b"x"})
        with self.assertRaisesRegex(ValueError, "unsafe path"):
            read_pinned_archive(
                traversal, repository_slug="GTFOBins.github.io", revision=REVISION,
                content_roots=("_gtfobins",), allowed_extensions=(".md",),
            )

        symlink_output = BytesIO()
        with tarfile.open(fileobj=symlink_output, mode="w:gz") as archive:
            info = tarfile.TarInfo(f"GTFOBins.github.io-{REVISION}/_gtfobins/link.md")
            info.type = tarfile.SYMTYPE
            info.linkname = "/etc/passwd"
            archive.addfile(info)
        with self.assertRaisesRegex(ValueError, "non-regular entry"):
            read_pinned_archive(
                symlink_output.getvalue(), repository_slug="GTFOBins.github.io", revision=REVISION,
                content_roots=("_gtfobins",), allowed_extensions=(".md",),
            )


class ExploitDBProviderFixtureTests(unittest.TestCase):
    def test_csv_provider_keeps_allowlist_and_reports_duplicates_and_bad_rows(self) -> None:
        csv_data = (
            "id,description,date_published,platform,type,port,codes,file\n"
            "123,old description,2020-01-01,Linux,local,0,CVE-2020-1,OLD_SYNTHETIC_POC\n"
            "123,new description,2021-01-01,Linux,remote,443,CVE-2021-2,SECRET_SYNTHETIC_POC\n"
            "124,,2022-01-01,Linux,remote,80,,BAD_SYNTHETIC_POC\n"
        ).encode()
        readme_data = b"This project (and SearchSploit) is released under GPL v2.0.\n"
        license_data = b"GPL-2.0-or-later fixture license\n"

        def fetcher(url: str, _max_bytes: int) -> bytes:
            if url.endswith("/files_exploits.csv"):
                return csv_data
            if url.endswith("/README.md"):
                return readme_data
            if url.endswith("/LICENSE.md"):
                return license_data
            raise AssertionError(f"unexpected fixture URL: {url}")

        request = _request("exploitdb", EXPLOITDB_URL, {
            "content_scope": "descriptions_and_metadata_only",
        })
        batch = RedAmonExploitDBProvider(fetcher=fetcher)(request)
        content = [record for record in batch.records if record.document_role == "content"]
        self.assertEqual(len(content), 1)
        self.assertEqual(content[0].source_id, "123")
        self.assertEqual(content[0].metadata["description"], "new description")
        self.assertEqual(content[0].metadata["row_number"], 2)
        self.assertEqual(
            content[0].search_text,
            "new description | Platform: Linux | Type: remote | Published: 2021-01-01 | Codes: CVE-2021-2",
        )
        self.assertNotIn("file", content[0].metadata)
        self.assertNotIn("SECRET_SYNTHETIC_POC", content[0].original_text)
        self.assertNotIn("OLD_SYNTHETIC_POC", content[0].original_text)
        self.assertEqual(batch.expected_count, 1)
        self.assertEqual(batch.fetched_count, 1)
        self.assertEqual(batch.rejected_count, 1)
        self.assertFalse(batch.complete)
        self.assertTrue(batch.notices_complete)
        self.assertTrue(any(record.source_id == "NOTICE:README.md" for record in batch.records))
        self.assertTrue(any(record.source_id == "NOTICE:license.md" for record in batch.records))


class SourceRightsAndCoverageGateTests(unittest.TestCase):
    def test_unresolved_wstg_nuclei_and_exploitdb_terms_block_before_fetch(self) -> None:
        cases = (
            {
                "id": "owasp", "kind": "github_archive", "url": OWASP_URL,
                "revision": REVISION, "license_id": "CC-BY-SA-4.0", "license_evidence": "current repo only",
                "license_status": "current_repo_confirmed_pinned_revision_pending",
            },
            {
                "id": "nuclei", "kind": "github_archive", "url": NUCLEI_URL,
                "revision": REVISION, "license_id": "MIT", "license_evidence": "maintainer discussion only",
                "license_status": "current_repo_confirmed_pinned_revision_pending",
            },
            {
                "id": "exploitdb", "kind": "gitlab_raw_feed", "url": EXPLOITDB_URL,
                "revision": REVISION, "repository_license_id": "GPL-2.0-or-later",
                "license_evidence": "repository license only", "license_status": "review_required",
                "dataset_terms": "review_required", "content_scope": "descriptions_and_metadata_only",
            },
        )
        for spec in cases:
            with self.subTest(source=spec["id"]):
                calls: list[str] = []

                def provider(request: SourceRequest) -> SourceBatch:
                    calls.append(request.source_name)
                    raise AssertionError("blocked provider must not be called")

                snapshot = RedAmonSourceAdapter(
                    {"profile": "full", "sources": [spec]},
                    {str(spec["id"]): provider},
                    source_manifest_sha256="c" * 64,
                ).build()
                row = next(item for item in snapshot.source_coverage if item["source_name"] == spec["id"])
                self.assertEqual(row["state"], "blocked")
                self.assertEqual(calls, [])

    def test_pinned_exploitdb_repository_license_allows_metadata_only_feed(self) -> None:
        spec = {
            "id": "exploitdb", "kind": "gitlab_raw_feed", "url": EXPLOITDB_URL,
            "revision": REVISION, "repository_license_id": "GPL-2.0-or-later",
            "license_evidence": "pinned LICENSE.md", "license_status": "confirmed_at_pinned_revision",
            "dataset_terms": "confirmed_at_pinned_revision",
            "dataset_terms_evidence": "pinned README.md",
            "dataset_terms_basis": "Metadata-only feed from the officially licensed pinned repository.",
            "content_scope": "descriptions_and_metadata_only",
        }

        def provider(request: SourceRequest) -> SourceBatch:
            records = (
                SourceRecord(
                    source_id="123", title="EDB-123: Fixture", original_text='{"description":"Fixture"}',
                    source_locator="files_exploits.csv#row=2", source_revision=REVISION,
                    metadata={"description": "Fixture"},
                ),
                SourceRecord(
                    source_id="NOTICE:README.md", title="README", original_text="GPL v2 project notice",
                    source_locator="README.md", source_revision=REVISION, document_role="license_notice",
                ),
                SourceRecord(
                    source_id="NOTICE:license.md", title="LICENSE", original_text="GPL license text",
                    source_locator="LICENSE.md", source_revision=REVISION, document_role="license_notice",
                ),
            )
            return SourceBatch(
                source_revision=REVISION, records=records, expected_count=1, fetched_count=1,
                parsed_count=1, rejected_count=0, complete=True, artifact_sha256="f" * 64,
                fetched_at="2026-10-25T00:00:00+00:00", applied_filters=dict(request.filters),
                notices_complete=True,
            )

        snapshot = RedAmonSourceAdapter(
            {"profile": "full", "sources": [spec]}, {"exploitdb": provider},
            source_manifest_sha256="a" * 64,
        ).build()
        row = next(item for item in snapshot.source_coverage if item["source_name"] == "exploitdb")
        self.assertEqual(row["state"], "done")
        self.assertIn("dataset_terms_evidence", row["source_metadata"])
        self.assertIn("NOTICE:README.md", {doc.source_id for doc in snapshot.documents})

    def test_mismatched_counts_keep_an_otherwise_eligible_family_partial(self) -> None:
        spec = {
            "id": "gtfobins", "kind": "github_archive", "url": GTFO_URL,
            "revision": REVISION, "license_id": "GPL-3.0-only", "license_evidence": "fixture",
            "license_status": "confirmed_at_pinned_revision",
        }

        def provider(request: SourceRequest) -> SourceBatch:
            record = SourceRecord(
                source_id="_gtfobins/example.md", title="example", original_text="Fixture content",
                source_locator="_gtfobins/example.md", source_revision=REVISION,
            )
            notice = SourceRecord(
                source_id="NOTICE:license", title="License", original_text="GPL fixture",
                source_locator="LICENSE", source_revision=REVISION, document_role="license_notice",
            )
            return SourceBatch(
                source_revision=REVISION, records=(record, notice), expected_count=1,
                fetched_count=2, parsed_count=1, rejected_count=0, complete=True,
                artifact_sha256="d" * 64, applied_filters=dict(request.filters), notices_complete=True,
            )

        snapshot = RedAmonSourceAdapter(
            {"profile": "full", "sources": [spec]}, {"gtfobins": provider},
            source_manifest_sha256="e" * 64,
        ).build()
        row = next(item for item in snapshot.source_coverage if item["source_name"] == "gtfobins")
        self.assertEqual(row["state"], "partial")
        self.assertIn("fetched_count_does_not_match_exact_expected_count", row["blockers"])


class VendoredToolDocsSnapshotTests(unittest.TestCase):
    def test_snapshot_refresh_reuses_vectors_for_unchanged_chunk_identities(self) -> None:
        class FakeEmbedder:
            model_name = "fixture-model"

            def __init__(self) -> None:
                self.encoded: list[str] = []

            def embed_documents_batch(self, texts: list[str], *, batch_size: int) -> list[list[float]]:
                self.encoded.extend(texts)
                return [[float(len(text)), 1.0] for text in texts]

        stable = SourceDocument.create(
            source_name="nvd", source_id="CVE-2026-0001", title="Retired title marker", text="Stable source content",
        )
        stable_after = SourceDocument.create(
            source_name="nvd", source_id="CVE-2026-0001", title="Current title marker", text="Stable source content",
        )
        changed_before = SourceDocument.create(
            source_name="nvd", source_id="CVE-2026-0002", title="CVE-2026-0002", text="Old source content",
        )
        changed_after = SourceDocument.create(
            source_name="nvd", source_id="CVE-2026-0002", title="CVE-2026-0002", text="New source content",
        )
        before_chunks = (*chunk_document(stable), *chunk_document(changed_before))
        after_chunks = (*chunk_document(stable_after), *chunk_document(changed_after))
        with tempfile.TemporaryDirectory(prefix="bb-assistant-kb-vector-reuse-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3")
            embedder = FakeEmbedder()
            index.replace_snapshot(before_chunks, "1" * 64, sources=(stable, changed_before))
            self.assertEqual(index.rebuild_embeddings(embedder, batch_size=2), 2)
            first_encoded = list(embedder.encoded)

            index.replace_snapshot(after_chunks, "2" * 64, sources=(stable_after, changed_after))
            with index.connect() as connection:
                retained = connection.execute("SELECT chunk_id FROM chunk_embeddings").fetchall()
            self.assertEqual([row[0] for row in retained], [before_chunks[0].chunk_id])
            self.assertEqual(index.search("retired"), ())
            self.assertEqual(index.search("current")[0].chunk_id, after_chunks[0].chunk_id)
            self.assertEqual(index.rebuild_embeddings(embedder, batch_size=2), 2)
            self.assertEqual(len(embedder.encoded) - len(first_encoded), 1)
            self.assertEqual(index.stats()["embedding_snapshot_id"], "2" * 64)
            refreshed = rechunk_index(index, project_root=Path(__file__).resolve().parents[1])
            self.assertEqual(refreshed["source_documents"], 2)
            self.assertEqual(refreshed["chunks"], 2)
            self.assertEqual(index.rebuild_embeddings(embedder, batch_size=2), 2)
            self.assertTrue(index.stats()["vector_ready"])
            self.assertEqual(len(embedder.encoded) - len(first_encoded), 1)

    def test_rechunk_resumes_after_a_committed_source_batch(self) -> None:
        documents = (
            SourceDocument.create(
                source_name="nvd", source_id="CVE-2026-0001", title="CVE-2026-0001",
                text="Authorization boundary regression fixture one.",
            ),
            SourceDocument.create(
                source_name="nvd", source_id="CVE-2026-0002", title="CVE-2026-0002",
                text="Authorization boundary regression fixture two.",
            ),
        )
        initial_chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-assistant-kb-rechunk-resume-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3")
            index.replace_snapshot(initial_chunks, "1" * 64, sources=documents)

            def interrupt_after_first_batch(done: int, total: int) -> None:
                if done == 1:
                    raise RuntimeError("simulated process interruption")

            with self.assertRaisesRegex(RuntimeError, "simulated process interruption"):
                rechunk_index(
                    index,
                    project_root=Path(__file__).resolve().parents[1],
                    batch_documents=1,
                    progress_callback=interrupt_after_first_batch,
                )
            self.assertEqual(index.stats()["maintenance_state"], "rechunking")
            with self.assertRaisesRegex(RuntimeError, "maintenance is in progress"):
                index.search("authorization")

            result = rechunk_index(
                index, project_root=Path(__file__).resolve().parents[1], batch_documents=1,
            )
            self.assertEqual(result["source_documents"], 2)
            self.assertEqual(result["chunks"], 2)
            self.assertIsNone(index.stats()["maintenance_state"])
            self.assertEqual(len(index.search("authorization")), 2)

    def test_snapshot_preserves_raw_upstream_json_separately_from_search_text(self) -> None:
        raw = '{"cve":{"id":"CVE-2026-12345","future":"retained"}}'
        document = SourceDocument.create(
            source_name="nvd", source_id="CVE-2026-12345", title="CVE-2026-12345",
            text="Normalized RedAmon CVE description.\nCVSS: 9.8 (critical)",
            raw_source_text=raw,
        )
        with tempfile.TemporaryDirectory(prefix="bb-assistant-kb-raw-source-") as temporary:
            root = Path(temporary)
            chunks = chunk_document(document)
            index = KBIndex(root / "index.sqlite3")
            index.replace_snapshot(chunks, "1" * 64, sources=(document,))
            stored = index.get_source("nvd", "CVE-2026-12345")
            self.assertIsNotNone(stored)
            self.assertEqual(stored["text"], document.text)
            self.assertEqual(stored["raw_source_text"], raw)
            self.assertEqual(stored["raw_source_sha256"], hashlib.sha256(raw.encode()).hexdigest())

    def test_legacy_snapshot_source_rows_remain_importable(self) -> None:
        document = SourceDocument.create(
            source_name="gtfobins", source_id="curl", title="curl", text="Legacy source text.",
        )
        row = {
            "source_name": document.source_name,
            "source_id": document.source_id,
            "title": document.title,
            "text": document.text,
            "source_url": document.source_url,
            "source_locator": document.source_locator,
            "source_revision": document.source_revision,
            "license_id": document.license_id,
            "attribution": document.attribution,
            "metadata": document.metadata,
            "content_sha256": document.content_sha256,
        }
        restored = _source_document_from_record(row)
        self.assertEqual(restored, document)

    def test_redamon_chunking_keeps_atomic_records_and_heading_based_markdown(self) -> None:
        structured = SourceDocument.create(
            source_name="nuclei", source_id="template.yaml", title="Template",
            text="id: demo\ninfo:\n  name: Demo template\n" + ("description " * 500),
        )
        structured_chunks = chunk_document(structured)
        self.assertEqual(len(structured_chunks), 1)
        self.assertTrue(structured_chunks[0].metadata["chunk_truncated"])
        self.assertEqual(structured_chunks[0].text, structured.text[:len(structured_chunks[0].text) - 3] + "...")
        self.assertFalse(structured_chunks[0].metadata["source_span_exact"])
        self.assertEqual(structured_chunks[0].end_char, len(structured_chunks[0].text) - 3)
        self.assertEqual(
            structured_chunks[0].chunk_sha256,
            "bd68ce1355d3cf0bd066136d06bda336a76194ca7cd72eda88167f31b4f09efa",
        )

        markdown_text = (
            "# Guide\n\nIntro\n\n## First\n\nShort section.\n\n"
            "## Second\n\nAnother short section.\n\n"
            "## Third\n\n" + ("Paragraph text. " * 500)
        )
        markdown = SourceDocument.create(
            source_name="tool_docs", source_id="guide.md", title="Guide", text=markdown_text,
        )
        markdown_chunks = chunk_document(markdown)
        self.assertGreaterEqual(len(markdown_chunks), 2)
        self.assertEqual(markdown_chunks[0].title, "Guide")
        self.assertIn("## First", markdown_chunks[0].text)
        self.assertIn("## Second", markdown_chunks[0].text)
        self.assertTrue(all(chunk.chunk_schema == "bb-kb-chunk-v2" for chunk in markdown_chunks))
        for chunk in markdown_chunks:
            if chunk.metadata.get("source_span_exact", True):
                self.assertEqual(markdown.text[chunk.start_char:chunk.end_char], chunk.text)

        # Golden outputs captured from RedAmon ChunkStrategy on these fixtures.
        short_sections = SourceDocument.create(
            source_name="tool_docs", source_id="short.md", title="ignored",
            text="# Guide\n\nIntro\n\n## A\n\nShort A.\n\n## B\n\nShort B.\n\n## C\n\nShort C.",
        )
        self.assertEqual(
            [chunk.text for chunk in chunk_document(short_sections)],
            ["# Guide\n\nIntro\n\n## A\n\nShort A.\n\n## B\n\nShort B.\n\n## C\n\nShort C."],
        )
        multiline = SourceDocument.create(
            source_name="tool_docs", source_id="multiline.md", title="ignored",
            text="## One\n\nAlpha\n\n\n## Two\n\nBeta",
        )
        self.assertEqual([chunk.text for chunk in chunk_document(multiline)], ["## One\n\nAlpha\n\n## Two\n\nBeta"])
        self.assertFalse(chunk_document(multiline)[0].metadata["source_span_exact"])
        long_markdown = SourceDocument.create(
            source_name="tool_docs", source_id="long.md", title="ignored",
            text="## Large\n\n" + ("Paragraph text. " * 500),
        )
        self.assertEqual(
            [chunk.chunk_sha256 for chunk in chunk_document(long_markdown)],
            [
                "e1e8f3715d0a429590807c2fc08d126ce2f49b79c505673c14c5a08ccebe1364",
                "e2283ccfa3d632ae48252268359c2f68cb0b4d12ce943572d0c653eb0c940259",
            ],
        )

    def test_pinned_tooldocs_export_import_search_and_partial_coverage(self) -> None:
        project_root = Path(__file__).resolve().parents[1]
        bundle = project_root / "references/redamon/agentic-skills"
        bundle_manifest = json.loads((bundle / "SOURCE-MANIFEST.json").read_text("utf-8"))
        raw_manifest = (project_root / "src/bb_assistant/kb/sources.yaml").read_bytes()
        manifest = yaml.safe_load(raw_manifest)
        spec = next(item for item in manifest["sources"] if item["id"] == "tool_docs")
        self.assertEqual(spec["revision"], bundle_manifest["source_commit"])
        snapshot = RedAmonSourceAdapter(
            {"profile": manifest["profile"], "sources": [spec]},
            {"tool_docs": RedAmonLocalToolDocsProvider(project_root=project_root)},
            source_manifest_sha256=hashlib.sha256(raw_manifest).hexdigest(),
        ).build(max_chars=1200, overlap_chars=0)
        coverage = next(row for row in snapshot.source_coverage if row["source_name"] == "tool_docs")
        self.assertEqual(coverage["state"], "done")
        self.assertEqual(coverage["fetched_count"], 46)
        self.assertEqual(len(snapshot.documents), 47)  # All 46 documents plus the MIT notice.
        tool_doc_ids = {
            document.source_id for document in snapshot.documents
            if document.source_name == "tool_docs" and document.metadata.get("document_role") != "license_notice"
        }
        self.assertEqual(tool_doc_ids, {row["path"] for row in bundle_manifest["documents"]})
        self.assertTrue({
            "post_exploitation/docker_escape.md",
            "post_exploitation/linux_privesc.md",
            "post_exploitation/windows_privesc.md",
        }.issubset(tool_doc_ids))
        self.assertEqual(snapshot.coverage_state, "PARTIAL")  # Other six source families are not in this fixture.

        with tempfile.TemporaryDirectory(prefix="bb-assistant-kb-test-") as temporary:
            temp_root = Path(temporary)
            exported = export_redamon_snapshot(
                snapshot, temp_root / "snapshots", max_chars=1200, overlap_chars=0,
            )
            index = KBIndex(temp_root / "index.sqlite3")
            with patch("bb_assistant.kb.rebuild._read_jsonl", side_effect=AssertionError("bulk JSONL materializer used")):
                stats = import_snapshot(temp_root / "snapshots" / exported["snapshot_id"], index)
            self.assertEqual(stats["coverage_state"], "PARTIAL")
            self.assertEqual(stats["source_documents"], 47)
            self.assertEqual(stats["source_manifest_sha256"], hashlib.sha256(raw_manifest).hexdigest())
            self.assertTrue(stats["source_manifest_matches_current"])
            hits = index.search("PKINIT", top_k=10, source_names=("tool_docs",))
            hit = next(item for item in hits if item.source_id == "active_directory/ad_cs_esc.md")
            self.assertEqual(hit.source_revision, bundle_manifest["source_commit"])
            self.assertRegex(hit.source_sha256, r"^[a-f0-9]{64}$")
            self.assertRegex(hit.chunk_sha256, r"^[a-f0-9]{64}$")
            self.assertGreater(hit.end_char, hit.start_char)
            self.assertFalse(any(item.source_id == "NOTICE:license" for item in hits))

            chunks_path = temp_root / "snapshots" / exported["snapshot_id"] / "chunks.jsonl"
            original_chunks = chunks_path.read_bytes()
            # Whitespace preserves valid JSON while changing the pinned file hash.
            chunks_path.write_bytes(original_chunks.replace(b"{", b"{ ", 1))
            with self.assertRaisesRegex(ValueError, "manifest byte count mismatch|JSONL hash mismatch"):
                import_snapshot(temp_root / "snapshots" / exported["snapshot_id"], index)
            self.assertEqual(index.stats()["snapshot_id"], exported["snapshot_id"])

    def test_search_does_not_return_license_notice_records(self) -> None:
        content = SourceDocument.create(
            source_name="tool_docs", source_id="guide.md", title="Synthetic guide",
            text="Use PKINIT in this synthetic fixture.", metadata={"document_role": "content"},
        )
        notice = SourceDocument.create(
            source_name="tool_docs", source_id="NOTICE:LICENSE", title="MIT License",
            text="This synthetic notice includes the rare term sublicensing.",
            metadata={"document_role": "license_notice"},
        )
        documents = (content, notice)
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-assistant-kb-search-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3")
            index.replace_snapshot(chunks, "f" * 64, sources=documents)
            self.assertEqual(index.search("PKINIT")[0].source_id, "guide.md")
            self.assertEqual(index.search("sublicensing"), ())


if __name__ == "__main__":
    unittest.main()
