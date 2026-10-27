from __future__ import annotations

import hashlib
import importlib.machinery
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
from types import SimpleNamespace

import pytest


ROOT = Path(__file__).resolve().parents[1]
PIN = "4585b3442cf3c6e075fca5d3b512a8f47be501b6"
DEFAULT_REDAMON_ROOT = Path("/tmp/graph-kb-redamon-src")


def _module():
    script = ROOT / "scripts" / "compare-redamon-corpus"
    loader = importlib.machinery.SourceFileLoader("compare_redamon_corpus_test", str(script))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _hash(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def test_pinned_reference_transformations_match_tiny_retained_fixture(tmp_path: Path) -> None:
    """Exercise all seven actual pinned upstream parser/to_chunks paths offline."""
    root = Path(os.environ.get("REDAMON_SOURCE_ROOT", DEFAULT_REDAMON_ROOT))
    if not root.is_dir():
        pytest.skip("pinned RedAmon source checkout is not present; no source fetch is performed")
    tool = _module()
    clients, modules = tool._load_upstream(root, PIN)
    samples = {
        "tool_docs": (
            "tooling/testtool.md",
            "---\nname: testtool\ndescription: safe synthetic utility\n---\n# testtool\n\n## Syntax\n`testtool --help`\n",
            {"category_dir": "tooling", "filename": "testtool.md", "tool_name": "testtool"},
        ),
        "gtfobins": (
            "_gtfobins/testbin",
            "---\nfunctions:\n  shell:\n    - code: testbin -c first\n      description: first synthetic shell example\n    - code: testbin -c last\n      description: last synthetic shell example\n...\n",
            {"binary_name": "testbin"},
        ),
        "lolbas": (
            "yml/Test.yml",
            "Name: Test\nDescription: Synthetic utility\nCommands:\n  - Command: test.exe\n    Description: synthetic command\n    Category: Execution\n",
            {"filename": "Test.yml"},
        ),
        "owasp": (
            "document/4-Web_Application_Security_Testing/01-Information_Gathering/test.md",
            "# WSTG-INFO-01\n\n## Summary\nSynthetic summary.\n\n## Objective\nSynthetic objective.\n",
            {"filename": "test.md"},
        ),
        "exploitdb": (
            "900000",
            json.dumps({"id": "900000", "description": "Synthetic record", "date_published": "2026-01-01", "platform": "Linux", "type": "local", "port": "", "codes": "CVE-2026-0001"}, sort_keys=True),
            {},
        ),
        "nvd": (
            "CVE-2026-0001",
            json.dumps({"cve": {"id": "CVE-2026-0001", "descriptions": [{"lang": "en", "value": "Synthetic vulnerability"}], "metrics": {}, "configurations": [], "published": "2026-01-01T00:00:00.000Z"}}, sort_keys=True),
            {},
        ),
        "nuclei": (
            "http/test.yaml",
            "id: synthetic-test\ninfo:\n  name: Synthetic test\n  author: fixture\n  severity: low\n  description: Synthetic template description\nhttp:\n  - method: GET\n    path:\n      - '{{BaseURL}}/'\n",
            {"template_id": "synthetic-test"},
        ),
    }
    db_path = tmp_path / "fixture.sqlite3"
    with sqlite3.connect(db_path) as db:
        db.executescript("""
          CREATE TABLE kb_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
          CREATE TABLE source_documents(
            source_name TEXT, source_id TEXT, title TEXT, source_locator TEXT,
            metadata_json TEXT, content_sha256 TEXT, text TEXT,
            raw_source_sha256 TEXT, raw_source_text TEXT,
            PRIMARY KEY(source_name,source_id));
          CREATE TABLE chunks(
            chunk_id TEXT PRIMARY KEY, source_name TEXT, source_id TEXT, title TEXT,
            metadata_json TEXT, chunk_sha256 TEXT, text TEXT);
          INSERT INTO kb_meta VALUES('snapshot_id','synthetic-test-snapshot');
        """)
        for family, (source_id, raw, metadata) in samples.items():
            document_text = raw
            raw_source_text = raw if family in {"tool_docs", "exploitdb", "nvd"} else None
            if family == "tool_docs":
                document_text = "testtool — safe synthetic utility\n\nSyntax: `testtool --help`"
            if family in {"exploitdb", "nvd"}:
                raw_obj = json.loads(raw)
                if family == "exploitdb":
                    raw_obj.setdefault("port", "")
                    raw_obj.setdefault("codes", "")
                    raw_source_text = json.dumps(raw_obj, ensure_ascii=False, sort_keys=True)
                    document_text = (
                        "Synthetic record | Platform: Linux | Type: local | Published: 2026-01-01 "
                        "| Codes: CVE-2026-0001"
                    )
                    raw = raw_source_text
                else:
                    raw_source_text = raw
                    document_text = "Synthetic vulnerability"
            doc = {
                "source_id": source_id,
                "source_locator": source_id,
                "metadata": metadata,
                "text": document_text,
                "raw_source_text": raw_source_text,
            }
            selected_input, reason = tool._raw_input(family, doc)
            if family in {"tool_docs", "exploitdb", "nvd"}:
                assert reason is None and selected_input == raw_source_text
            generated = tool._upstream_chunks(family, doc, raw, clients[family], modules)
            assert generated, family
            upstream, _ = modules["redamon_filter_unchanged"](generated, {})
            if family == "gtfobins":
                assert len(generated) == 2 and len(upstream) == 1
                assert "last synthetic shell example" in upstream[0]["content"]
                assert "first synthetic shell example" not in upstream[0]["content"]
            # These fixture rows stand in for the transformed SQLite index
            # records and test ID/title/content hashing against actual upstream output.
            db.execute(
                "INSERT INTO source_documents VALUES(?,?,?,?,?,?,?,?,?)",
                (family, source_id, "fixture", source_id,
                 json.dumps(metadata, sort_keys=True), _hash(document_text), document_text,
                 _hash(raw_source_text) if raw_source_text is not None else None, raw_source_text),
            )
            for chunk in upstream:
                db.execute(
                    "INSERT INTO chunks VALUES(?,?,?,?,?,?,?)",
                    (chunk["chunk_id"], family, source_id, chunk["title"], "{}",
                     chunk["content_sha256"], chunk["content"]),
                )
        skipped_records = [
            ("gtfobins", "_gtfobins/skippedbin", "functions: [", {}),
            ("nuclei", "http/skipped.yaml", "info:\n  name: Missing ID\n", {}),
        ]
        for family, source_id, text, metadata in skipped_records:
            db.execute(
                "INSERT INTO source_documents VALUES(?,?,?,?,?,?,?,?,?)",
                (family, source_id, "expected skip fixture", source_id,
                 json.dumps(metadata, sort_keys=True), _hash(text), text, None, None),
            )

    before = _hash(db_path.read_bytes().hex())
    report = tool.compare(db_path, root, PIN)
    after = _hash(db_path.read_bytes().hex())
    assert before == after, "comparator must not write to its input index"
    assert report["summary"]["state"] == "MATCH", {
        name: {key: value for key, value in row.items() if key in {
            "state", "documents_unavailable", "missing_local_ids_count", "extra_local_ids_count",
            "mismatch_count", "uncomparable_local_chunks_count", "supplemental_fallback_rows_count",
            "documents_skipped_as_upstream_expected", "duplicate_upstream_chunk_id_count",
        }} for name, row in report["families"].items()
    }
    assert report["summary"]["documents_reconstructed"] == len(samples)
    assert report["summary"]["reference_chunks"] == len(samples)
    assert set(report["families"]) == set(samples)
    assert report["families"]["gtfobins"]["within_document_duplicate_occurrences"] == 1
    assert report["families"]["gtfobins"]["documents_skipped_as_upstream_expected"] == 1
    assert report["families"]["nuclei"]["documents_skipped_as_upstream_expected"] == 1
    assert report["summary"]["state"] == "MATCH", "upstream-deduped repeated IDs are diagnostic, not a parity failure"

    # A locally retained row deliberately excluded from RedAmon's default core
    # search remains visible in total counts, while exact upstream core parity
    # is reported without claiming the complete local corpus is identical.
    with sqlite3.connect(db_path) as db:
        extra = "supplemental retained nuclei row"
        db.execute(
            "INSERT INTO chunks VALUES(?,?,?,?,?,?,?)",
            ("nuclei:supplemental", "nuclei", "http/test.yaml", "Supplemental",
             json.dumps({"redamon_excluded_reason": "pinned_parser_guard"}), _hash(extra), extra),
        )
    with_supplemental = tool.compare(db_path, root, PIN)
    nuclei = with_supplemental["families"]["nuclei"]
    assert nuclei["state"] == "CORE_MATCH_WITH_SUPPLEMENTAL"
    assert nuclei["reference_chunks"] == nuclei["core_local_chunks"]
    assert nuclei["local_chunks"] == nuclei["core_local_chunks"] + 1
    assert nuclei["supplemental_excluded_rows_count"] == 1
    assert nuclei["supplemental_excluded_rows_by_reason"] == {
        "redamon_excluded:pinned_parser_guard": 1,
    }
    assert with_supplemental["summary"]["state"] == "CORE_MATCH_WITH_SUPPLEMENTAL"

    with sqlite3.connect(db_path) as db:
        extra = "unexpected core row"
        db.execute(
            "INSERT INTO chunks VALUES(?,?,?,?,?,?,?)",
            ("nuclei:unexpected-core", "nuclei", "http/test.yaml", "Unexpected", "{}", _hash(extra), extra),
        )
    unexpected_core = tool.compare(db_path, root, PIN)["families"]["nuclei"]
    assert unexpected_core["extra_local_ids_count"] == 1
    assert unexpected_core["state"] == "PARTIAL_OR_MISMATCH"
    with sqlite3.connect(db_path) as db:
        db.execute("DELETE FROM chunks WHERE chunk_id='nuclei:unexpected-core'")

    # A comparator that only reports counts could miss a broken local corpus.
    with sqlite3.connect(db_path) as db:
        db.execute("UPDATE chunks SET title='deliberately different' "
                   "WHERE chunk_id=(SELECT chunk_id FROM chunks WHERE source_name='tool_docs' LIMIT 1)")
    changed = tool.compare(db_path, root, PIN)
    assert changed["summary"]["mismatches"] == 1
    assert changed["summary"]["state"] == "PARTIAL_OR_MISMATCH"

    # Cross-document ID collisions are diagnostic only after last-wins has
    # selected the final upstream row. Its absence locally must still fail.
    duplicate_raw = samples["gtfobins"][1].replace(
        "last synthetic shell example", "final cross-document shell example"
    )
    duplicate_id = "_gtfobins/duplicate-testbin"
    with sqlite3.connect(db_path) as db:
        db.execute(
            "INSERT INTO source_documents VALUES(?,?,?,?,?,?,?,?,?)",
            ("gtfobins", duplicate_id, "duplicate", duplicate_id,
             json.dumps({"binary_name": "testbin"}), _hash(duplicate_raw), duplicate_raw, None, None),
        )
        gtfo_chunk_id = db.execute(
            "SELECT chunk_id FROM chunks WHERE source_name='gtfobins' LIMIT 1"
        ).fetchone()[0]
        db.execute("DELETE FROM chunks WHERE chunk_id=?", (gtfo_chunk_id,))
    collided_missing = tool.compare(db_path, root, PIN)["families"]["gtfobins"]
    assert collided_missing["cross_document_duplicate_ids_count"] == 1
    assert collided_missing["missing_local_ids_count"] == 1
    assert collided_missing["state"] == "PARTIAL_OR_MISMATCH"

    # A family with no input evidence cannot be silently counted as success.
    with sqlite3.connect(db_path) as db:
        db.execute("DELETE FROM source_documents WHERE source_name='lolbas'")
        db.execute("DELETE FROM chunks WHERE source_name='lolbas'")
    missing_family = tool.compare(db_path, root, PIN)
    assert missing_family["families"]["lolbas"]["state"] == "NO_EVIDENCE"
    assert missing_family["summary"]["state"] == "PARTIAL_OR_MISMATCH"


def test_refuses_wrong_upstream_commit(tmp_path: Path) -> None:
    root = Path(os.environ.get("REDAMON_SOURCE_ROOT", DEFAULT_REDAMON_ROOT))
    if not root.is_dir():
        pytest.skip("pinned RedAmon source checkout is not present")
    tool = _module()
    with pytest.raises(tool.CompareError, match="HEAD differs"):
        tool._verify_upstream(root, "0" * 40)


def test_index_refuses_nonempty_wal_without_opening_writable_connection(tmp_path: Path) -> None:
    tool = _module()
    db_path = tmp_path / "kb.sqlite3"
    db_path.touch()
    Path(str(db_path) + "-wal").write_bytes(b"pending")
    with pytest.raises(tool.CompareError, match="non-empty WAL"):
        tool._open_index(db_path)


def test_index_refuses_nonempty_rollback_journal(tmp_path: Path) -> None:
    tool = _module()
    db_path = tmp_path / "kb.sqlite3"
    db_path.touch()
    Path(str(db_path) + "-journal").write_bytes(b"pending")
    with pytest.raises(tool.CompareError, match="rollback journal"):
        tool._open_index(db_path)


def _pinned_fixture_checkout(root: Path) -> str:
    package = root / "services" / "knowledge_base"
    package.mkdir(parents=True)
    (package / "__init__.py").write_text("# fixture\n")
    (package / "kb_config.yaml").write_text("chunk_size: 100\n")
    subprocess.run(["git", "init", "-q", str(root)], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.email", "test@example.invalid"], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.name", "Test"], check=True)
    subprocess.run(["git", "-C", str(root), "add", "services/knowledge_base"], check=True)
    subprocess.run(["git", "-C", str(root), "commit", "-qm", "fixture"], check=True)
    return subprocess.run(
        ["git", "-C", str(root), "rev-parse", "HEAD"], check=True,
        capture_output=True, text=True,
    ).stdout.strip()


def test_verifies_all_upstream_python_and_config_blobs_before_import(tmp_path: Path) -> None:
    tool = _module()
    root = tmp_path / "redamon"
    commit = _pinned_fixture_checkout(root)
    assert tool._verify_tree_sources(root, commit) == [
        "services/knowledge_base/__init__.py",
        "services/knowledge_base/kb_config.yaml",
    ]

    # A dirty Python helper or config is rejected by the pre-import verifier.
    (root / "services/knowledge_base/__init__.py").write_text("raise RuntimeError('executed')\n")
    with pytest.raises(tool.CompareError, match="differs from pinned Git blob"):
        tool._verify_tree_sources(root, commit)


def test_imported_knowledge_base_module_must_not_come_from_outside_checkout(tmp_path: Path) -> None:
    tool = _module()
    root = tmp_path / "redamon"
    commit = _pinned_fixture_checkout(root)
    outside = tmp_path / "knowledge_base.py"
    outside.write_text("# wrong package\n")
    module = SimpleNamespace(__name__="knowledge_base.helper", __file__=str(outside))
    with pytest.raises(tool.CompareError, match="outside pinned checkout"):
        tool._verify_imported_sources(root, commit, [module])


def test_redamon_source_symlink_cannot_escape_checkout(tmp_path: Path) -> None:
    tool = _module()
    root = tmp_path / "redamon"
    commit = _pinned_fixture_checkout(root)
    outside = tmp_path / "outside.py"
    outside.write_text("# outside\n")
    (root / "services/knowledge_base/escape.py").symlink_to(outside)
    with pytest.raises(tool.CompareError, match="symlink escapes"):
        tool._verify_tree_sources(root, commit)
