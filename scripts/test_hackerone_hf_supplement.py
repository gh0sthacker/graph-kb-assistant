"""Tests for scripts/hackerone_hf_supplement.py.

The parquet reader needs pyarrow, which is not available to the plain test
interpreter, so these tests stub ``read_parquet`` and cover the export logic:
split precedence, deduplication, deterministic ordering and the meta/hash file.
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
from pathlib import Path

SCRIPT_PATH = Path(__file__).with_name("hackerone_hf_supplement.py")


def _load_module():
    spec = importlib.util.spec_from_file_location("hackerone_hf_supplement", SCRIPT_PATH)
    assert spec is not None
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


supplement = _load_module()


def _record(report_id: str, body: str, program: str = "acme") -> dict:
    return {
        "report_id": report_id,
        "title": f"title {report_id}",
        "body": body,
        "weakness": "SQL Injection",
        "program": program,
        "disclosed_at": "2020-01-01T00:00:00.000Z",
        "upvotes": 1,
        "source": supplement.SOURCE,
        "dataset_last_modified": supplement.DATASET_LAST_MODIFIED,
    }


def test_collect_dedupes_first_split_wins_and_sorts(tmp_path, monkeypatch):
    paths = [tmp_path / "train.parquet", tmp_path / "test.parquet"]
    payload = {
        paths[0]: [_record("1003", "train body"), _record("9001", "train only")],
        paths[1]: [_record("1003", "test body"), _record("1002", "test only")],
    }
    monkeypatch.setattr(supplement, "read_parquet", lambda path: payload[path])

    records, stats, duplicates = supplement.collect(paths)

    assert [row["report_id"] for row in records] == ["1002", "1003", "9001"]
    assert [row["body"] for row in records] == ["test only", "train body", "train only"]
    assert duplicates == 1
    assert stats[0]["kept"] == 2
    assert stats[1]["kept"] == 1


def test_run_writes_jsonl_and_meta(tmp_path, monkeypatch):
    paths = [tmp_path / "train.parquet"]
    monkeypatch.setattr(
        supplement,
        "read_parquet",
        lambda path: [_record("42", "body text"), _record("7", "other body", "globex")],
    )
    output = tmp_path / "hf-supplement.jsonl"

    summary = supplement.run(paths, output)

    raw = output.read_bytes()
    lines = [json.loads(line) for line in raw.decode("utf-8").splitlines()]
    assert [row["report_id"] for row in lines] == ["7", "42"]
    assert set(lines[0]) == {
        "report_id",
        "title",
        "body",
        "weakness",
        "program",
        "disclosed_at",
        "upvotes",
        "source",
        "dataset_last_modified",
    }
    assert lines[0]["source"] == "Hacker0x01/hackerone_disclosed_reports"
    assert lines[0]["dataset_last_modified"] == "2024-05-17"
    assert summary["rows"] == 2
    assert summary["sha256"] == hashlib.sha256(raw).hexdigest()

    meta_path = output.with_suffix(".meta.json")
    meta = json.loads(meta_path.read_text(encoding="utf-8"))
    assert meta["rows"] == 2
    assert meta["sha256"] == summary["sha256"]
    assert meta["source"] == supplement.SOURCE
    assert meta["files"][0]["rows_with_body"] == 2


def test_run_is_byte_identical_on_rerun(tmp_path, monkeypatch):
    paths = [tmp_path / "train.parquet"]
    monkeypatch.setattr(supplement, "read_parquet", lambda path: [_record("5", "stable")])
    output = tmp_path / "out.jsonl"
    first = supplement.run(paths, output)["sha256"]
    second = supplement.run(paths, output)["sha256"]
    assert first == second
