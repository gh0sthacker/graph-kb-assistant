"""Tests for scripts/build-hackerone-index.py."""

from __future__ import annotations

import hashlib
import importlib.util
import json
import sqlite3
import stat
import subprocess
import sys
from pathlib import Path

import pytest

SCRIPT_PATH = Path(__file__).with_name("build-hackerone-index.py")


def _load_module():
    spec = importlib.util.spec_from_file_location("build_hackerone_index", SCRIPT_PATH)
    assert spec is not None
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


builder = _load_module()


MD_1001 = """# SSRF in image fetcher

**Report:** https://hackerone.com/reports/1001
**Severity:** high (7.5)
**Weakness:** Server-Side Request Forgery (SSRF)
**Bounty:** $500

The image proxy fetched arbitrary URLs, enabling server-side request forgery
against internal metadata endpoints.
"""

MD_1002 = """# IDOR on invoice export

**Report:** https://hackerone.com/reports/1002
**Severity:** medium (5.3)

An authorization flaw let any user download invoices belonging to other
tenants. The authorization checks were missing entirely.
"""

MD_1003 = """# GraphQL authorization bypass

**Report:** https://hackerone.com/reports/1003
**Severity:** critical
**Weakness:** Improper Authorization

A crafted query bypassed the permission layer and exposed admin-only fields.
"""

CSV_TEXT = (
    "program,title,link,upvotes,bounty,vuln_type\n"
    "Acme,SSRF in image fetcher,hackerone.com/reports/1001,42,500.0,"
    "Server-Side Request Forgery (SSRF)\n"
    "Acme,IDOR on invoice export,hackerone.com/reports/1002,17,0.0,Insecure Direct Object Reference (IDOR)\n"
    "Globex,GraphQL authorization bypass,hackerone.com/reports/1003,99,1500.0,Improper Authorization\n"
    "Initech,Stored XSS in profile bio,hackerone.com/reports/1004,5,0.0,Cross-site Scripting (XSS) - Stored\n"
)


SUPPLEMENT_TEXT = (
    json.dumps(
        {
            "report_id": "1001",
            "title": "HF title that must lose",
            "body": "HF BODY MUST NOT REPLACE THE MARKDOWN BODY",
            "weakness": "Wrong Weakness",
            "program": "Wrong Program",
            "disclosed_at": "2020-01-01T00:00:00.000Z",
            "upvotes": 999,
            "source": "Hacker0x01/hackerone_disclosed_reports",
            "dataset_last_modified": "2024-05-17",
        }
    )
    + "\n"
    + json.dumps(
        {
            "report_id": "1004",
            "title": "HF title for 1004",
            "body": "Filled from the official dataset: graphql deserialization notes.",
            "weakness": "Deserialization of Untrusted Data",
            "program": "Wrong Program",
            "disclosed_at": "2021-02-03T04:05:06.000Z",
            "upvotes": 777,
            "source": "Hacker0x01/hackerone_disclosed_reports",
            "dataset_last_modified": "2024-05-17",
        }
    )
    + "\n"
    + json.dumps(
        {
            "report_id": "5001",
            "title": "HF-only report",
            "body": "HF-only body describing a clickhouse injection.",
            "weakness": "SQL Injection",
            "program": "hf-program",
            "disclosed_at": "2022-01-01T00:00:00.000Z",
            "upvotes": 11,
            "source": "Hacker0x01/hackerone_disclosed_reports",
            "dataset_last_modified": "2024-05-17",
        }
    )
    + "\n"
    # Duplicate id: the first occurrence wins.
    + json.dumps(
        {
            "report_id": "5001",
            "title": "duplicate row",
            "body": "SECOND DUPLICATE BODY",
            "weakness": "",
            "program": "",
            "disclosed_at": "",
            "upvotes": 0,
        }
    )
    + "\n"
    + json.dumps({"report_id": "5002", "title": "no body", "body": "   "})
    + "\n"
)


@pytest.fixture()
def dataset(tmp_path: Path) -> Path:
    root = tmp_path / "HackerOne-Reports"
    reports = root / "reports"
    reports.mkdir(parents=True)
    (reports / "1001.md").write_text(MD_1001, encoding="utf-8")
    (reports / "1002.md").write_text(MD_1002, encoding="utf-8")
    (reports / "1003.md").write_text(MD_1003, encoding="utf-8")
    (root / "data.csv").write_text(CSV_TEXT, encoding="utf-8")
    return root


@pytest.fixture()
def supplement(tmp_path: Path) -> Path:
    path = tmp_path / "hf-supplement.jsonl"
    path.write_text(SUPPLEMENT_TEXT, encoding="utf-8")
    return path


@pytest.fixture()
def built(dataset: Path):
    output = dataset.parent / "out" / "index.sqlite3"
    summary = builder.build_index(dataset, output)
    return summary, output


def _fts_rows(connection: sqlite3.Connection, match: str, limit: int = 5):
    return connection.execute(
        """
        SELECT reports.report_id, reports.title, reports.has_body,
               bm25(reports_fts) AS score
        FROM reports_fts
        JOIN reports ON reports.rowid = reports_fts.rowid
        WHERE reports_fts MATCH ?
        ORDER BY bm25(reports_fts)
        LIMIT ?
        """,
        (match, limit),
    ).fetchall()


def test_counts_and_meta(built):
    summary, output = built
    assert output.is_file()
    assert summary["report_count"] == 4
    assert summary["with_body_count"] == 3
    assert summary["csv_rows"] == 4
    assert summary["md_files"] == 3
    assert summary["dataset_commit"] == "unknown"
    assert summary["size_bytes"] == output.stat().st_size
    assert summary["supplement_path"] == ""
    assert summary["supplement_rows"] == 0

    with sqlite3.connect(f"file:{output}?mode=ro", uri=True) as connection:
        meta = dict(connection.execute("SELECT key, value FROM meta").fetchall())
        assert meta["schema_version"] == "hackerone-fts-v1"
        assert meta["dataset_commit"] == "unknown"
        assert Path(meta["dataset_path"]).name == "HackerOne-Reports"
        assert Path(meta["dataset_path"]).is_absolute()
        assert meta["report_count"] == "4"
        assert meta["with_body_count"] == "3"
        expected = hashlib.sha256(b"unknown:4:3").hexdigest()
        assert meta["snapshot_id"] == expected
        assert len(meta["snapshot_id"]) == 64
        int(meta["snapshot_id"], 16)  # lowercase hex
        assert meta["built_at"].endswith("Z")

        tables = {
            row[0]
            for row in connection.execute(
                "SELECT name FROM sqlite_master WHERE type IN ('table','trigger')"
            )
        }
        assert {"reports", "reports_fts", "meta", "reports_ai", "reports_ad", "reports_au"} <= tables


def test_has_body_and_csv_only_row(built):
    _, output = built
    with sqlite3.connect(f"file:{output}?mode=ro", uri=True) as connection:
        row = connection.execute(
            "SELECT title, body, has_body, program, vuln_type, severity, weakness, url "
            "FROM reports WHERE report_id = '1004'"
        ).fetchone()
        assert row is not None
        assert row[0] == "Stored XSS in profile bio"
        assert row[1] == ""
        assert row[2] == 0
        assert row[3] == "Initech"
        assert row[4].startswith("Cross-site Scripting")
        assert row[5] == ""
        assert row[6] == ""
        assert row[7] == "https://hackerone.com/reports/1004"

        body_row = connection.execute(
            "SELECT has_body, severity, weakness, bounty FROM reports WHERE report_id = '1001'"
        ).fetchone()
        assert body_row == (1, "high (7.5)", "Server-Side Request Forgery (SSRF)", 500.0)

        # CSV-only rows are still searchable through their title.
        hits = _fts_rows(connection, "profile")
        assert hits and hits[0][0] == "1004"


def test_fts_body_and_title_ranking(built):
    _, output = built
    with sqlite3.connect(f"file:{output}?mode=ro", uri=True) as connection:
        body_hits = _fts_rows(connection, "tenants")
        assert body_hits and body_hits[0][0] == "1002"

        title_hits = _fts_rows(connection, "authorization")
        ids = [row[0] for row in title_hits]
        assert "1002" in ids and "1003" in ids
        # Title match in 1003 must outrank the body-only matches in 1002.
        assert title_hits[0][0] == "1003"
        scores = [row[3] for row in title_hits]
        assert scores == sorted(scores)

        # External-content table returns the title from the content table.
        title = connection.execute(
            "SELECT title FROM reports_fts WHERE reports_fts MATCH 'graphql'"
        ).fetchone()
        assert title == ("GraphQL authorization bypass",)


def test_parser_title_fallback():
    parsed = builder.parse_report_markdown("body without a heading\n", "777")
    assert parsed["title"] == ""
    assert parsed["body"] == "body without a heading"


def test_title_precedence_and_fallbacks(tmp_path: Path):
    root = tmp_path / "ds"
    (root / "reports").mkdir(parents=True)
    (root / "reports" / "9001.md").write_text(
        "**Severity:** low\n\nbody text\n", encoding="utf-8"
    )
    (root / "reports" / "9002.md").write_text(
        "# Markdown title wins\n\n**Severity:** high\n\nmore text\n", encoding="utf-8"
    )
    (root / "data.csv").write_text(
        "program,title,link,upvotes,bounty,vuln_type\n"
        "Acme,CSV fallback title,hackerone.com/reports/9001,1,0.0,\n"
        "Acme,CSV title loses,hackerone.com/reports/9002,2,0.0,\n"
        "Acme,,hackerone.com/reports/9003,3,0.0,\n",
        encoding="utf-8",
    )
    output = tmp_path / "out" / "index.sqlite3"
    builder.build_index(root, output)
    with sqlite3.connect(f"file:{output}?mode=ro", uri=True) as connection:
        titles = dict(connection.execute("SELECT report_id, title FROM reports"))
        assert titles["9001"] == "CSV fallback title"
        assert titles["9002"] == "Markdown title wins"
        assert titles["9003"] == "9003"
        assert connection.execute(
            "SELECT has_body FROM reports WHERE report_id = '9001'"
        ).fetchone() == (1,)


def test_output_is_private_and_atomic(dataset, tmp_path):
    output = tmp_path / "nested" / "dir" / "index.sqlite3"
    builder.build_index(dataset, output)
    mode = stat.S_IMODE(output.stat().st_mode)
    assert mode == 0o600
    leftovers = [p.name for p in output.parent.iterdir() if p.name != output.name]
    assert leftovers == []


def test_cli_emits_json_summary(dataset, tmp_path):
    output = tmp_path / "cli" / "index.sqlite3"
    result = subprocess.run(
        [
            sys.executable,
            str(SCRIPT_PATH),
            "--dataset",
            str(dataset),
            "--output",
            str(output),
            # Do not pick up the machine-wide supplement during the test.
            "--supplement",
            "",
        ],
        capture_output=True,
        text=True,
        timeout=120,
    )
    assert result.returncode == 0, result.stderr
    summary = json.loads(result.stdout)
    assert set(
        [
            "output",
            "size_bytes",
            "report_count",
            "with_body_count",
            "csv_rows",
            "md_files",
            "dataset_commit",
            "supplement_path",
            "supplement_rows",
            "filled_bodies",
            "added_reports",
            "elapsed_seconds",
        ]
    ) <= set(summary)
    assert summary["report_count"] == 4
    assert summary["output"] == str(output)
    assert summary["supplement_path"] == ""
    assert summary["supplement_rows"] == 0


def test_supplement_fills_adds_and_preserves(dataset, supplement, tmp_path):
    output = tmp_path / "sup" / "index.sqlite3"
    summary = builder.build_index(dataset, output, supplement)
    assert summary["report_count"] == 5
    assert summary["with_body_count"] == 5  # 3 markdown + 1 filled + 1 added
    assert summary["supplement_path"] == str(supplement.absolute())
    assert summary["supplement_rows"] == 3  # empty body and duplicate are skipped
    assert summary["filled_bodies"] == 1
    assert summary["added_reports"] == 1

    with sqlite3.connect(f"file:{output}?mode=ro", uri=True) as connection:
        # Existing markdown body and metadata win over the supplement.
        preserved = connection.execute(
            "SELECT title, body, has_body, weakness, program, upvotes, disclosed_at "
            "FROM reports WHERE report_id = '1001'"
        ).fetchone()
        assert preserved[0] == "SSRF in image fetcher"
        assert preserved[1].startswith("The image proxy fetched")
        assert "MUST NOT REPLACE" not in preserved[1]
        assert preserved[2] == 1
        assert preserved[3] == "Server-Side Request Forgery (SSRF)"
        assert preserved[4] == "Acme"
        assert preserved[5] == 42
        # disclosed_at was empty locally, so the supplement may fill it.
        assert preserved[6] == "2020-01-01T00:00:00.000Z"

        # CSV-only row gets the body, keeps its own metadata, fills gaps.
        filled = connection.execute(
            "SELECT body, has_body, weakness, program, upvotes, disclosed_at, "
            "vuln_type, bounty, content_sha256 FROM reports WHERE report_id = '1004'"
        ).fetchone()
        assert filled[0].startswith("Filled from the official dataset")
        assert filled[1] == 1
        assert filled[2] == "Deserialization of Untrusted Data"
        assert filled[3] == "Initech"
        assert filled[4] == 5
        assert filled[5] == "2021-02-03T04:05:06.000Z"
        assert filled[6].startswith("Cross-site Scripting")
        assert filled[7] == 0.0
        assert filled[8] == hashlib.sha256(filled[0].encode("utf-8")).hexdigest()

        # Unknown id becomes a new report row.
        added = connection.execute(
            "SELECT title, url, program, vuln_type, severity, weakness, bounty, "
            "upvotes, disclosed_at, body, has_body, content_sha256 "
            "FROM reports WHERE report_id = '5001'"
        ).fetchone()
        assert added is not None
        assert added[0] == "HF-only report"
        assert added[1] == "https://hackerone.com/reports/5001"
        assert added[2] == "hf-program"
        assert added[3] == "SQL Injection"
        assert added[4] == ""
        assert added[5] == "SQL Injection"
        assert added[6] is None
        assert added[7] == 11
        assert added[8] == "2022-01-01T00:00:00.000Z"
        assert added[9] == "HF-only body describing a clickhouse injection."
        assert added[10] == 1
        assert added[11] == hashlib.sha256(added[9].encode("utf-8")).hexdigest()

        # Duplicate supplement rows never create extra reports.
        assert connection.execute(
            "SELECT COUNT(*) FROM reports WHERE body LIKE '%SECOND DUPLICATE%'"
        ).fetchone() == (0,)
        assert connection.execute(
            "SELECT COUNT(*) FROM reports WHERE report_id = '5002'"
        ).fetchone() == (0,)

        # The new body is searchable through FTS.
        assert [row[0] for row in _fts_rows(connection, "deserialization")] == ["1004"]
        assert [row[0] for row in _fts_rows(connection, "clickhouse")] == ["5001"]

        meta = dict(connection.execute("SELECT key, value FROM meta").fetchall())
        assert meta["schema_version"] == "hackerone-fts-v1"
        assert meta["report_count"] == "5"
        assert meta["with_body_count"] == "5"
        assert meta["supplement_path"] == str(supplement.absolute())
        assert meta["supplement_rows"] == "3"
        assert meta["filled_bodies"] == "1"
        assert meta["added_reports"] == "1"
        expected = hashlib.sha256(b"unknown:5:5").hexdigest()
        assert meta["snapshot_id"] == expected


def test_supplement_missing_raises(dataset, tmp_path):
    with pytest.raises(builder.DatasetError):
        builder.build_index(dataset, tmp_path / "x.sqlite3", tmp_path / "nope.jsonl")

