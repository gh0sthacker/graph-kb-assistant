#!/usr/bin/env python3
"""Build a standalone read-only SQLite FTS5 index of the HackerOne reports dataset.

The index is a supplemental, self-contained artifact: the main knowledge-base
index and the Go service schema are untouched.  The Go reader only needs the
stable schema defined in ``SCHEMA_SQL`` below.

Usage:
    python3 scripts/build-hackerone-index.py \
        --dataset "/path/to/HackerOne-Reports" \
        --output ~/.local/share/bb-graph-assistant/hackerone/index.sqlite3 \
        [--supplement ~/.local/share/bb-graph-assistant/hackerone/hf-supplement.jsonl]

``--supplement`` points at the JSONL produced by
``scripts/hackerone_hf_supplement.py``.  It fills bodies that the local dataset
only has as headers, and inserts reports that the local dataset is missing; a
non-empty local body or metadata value is never overwritten.  The default is
``$HACKERONE_SUPPLEMENT``, then the conventional file next to the index when it
exists.

The build is atomic: rows are written to a temporary file in the destination
directory and moved into place with ``os.replace`` only after the database has
been fully populated, optimized and closed.  The resulting file is chmod 0600.
"""

from __future__ import annotations

import argparse
import csv
import hashlib
import json
import os
import re
import sqlite3
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

SCHEMA_VERSION = "hackerone-fts-v1"

DEFAULT_DATASET = "/home/max/bb/База знаний/External/HackerOne-Reports"
DEFAULT_OUTPUT = "/home/max/.local/share/bb-graph-assistant/hackerone/index.sqlite3"
DEFAULT_SUPPLEMENT = "/home/max/.local/share/bb-graph-assistant/hackerone/hf-supplement.jsonl"

SCHEMA_SQL = """
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE reports(
    report_id TEXT PRIMARY KEY,
    program TEXT NOT NULL,
    title TEXT NOT NULL,
    url TEXT NOT NULL,
    vuln_type TEXT NOT NULL,
    severity TEXT NOT NULL,
    weakness TEXT NOT NULL,
    bounty REAL,
    upvotes INTEGER,
    disclosed_at TEXT NOT NULL,
    body TEXT NOT NULL,
    has_body INTEGER NOT NULL,
    content_sha256 TEXT NOT NULL
);
CREATE VIRTUAL TABLE reports_fts USING fts5(
    title,
    body,
    program,
    vuln_type,
    weakness,
    content='reports',
    content_rowid='rowid',
    tokenize="porter unicode61 remove_diacritics 2"
);
CREATE TRIGGER reports_ai AFTER INSERT ON reports BEGIN
    INSERT INTO reports_fts(rowid, title, body, program, vuln_type, weakness)
    VALUES (new.rowid, new.title, new.body, new.program, new.vuln_type, new.weakness);
END;
CREATE TRIGGER reports_ad AFTER DELETE ON reports BEGIN
    INSERT INTO reports_fts(reports_fts, rowid, title, body, program, vuln_type, weakness)
    VALUES ('delete', old.rowid, old.title, old.body, old.program, old.vuln_type, old.weakness);
END;
CREATE TRIGGER reports_au AFTER UPDATE ON reports BEGIN
    INSERT INTO reports_fts(reports_fts, rowid, title, body, program, vuln_type, weakness)
    VALUES ('delete', old.rowid, old.title, old.body, old.program, old.vuln_type, old.weakness);
    INSERT INTO reports_fts(rowid, title, body, program, vuln_type, weakness)
    VALUES (new.rowid, new.title, new.body, new.program, new.vuln_type, new.weakness);
END;
"""

INSERT_SQL = """
INSERT INTO reports(
    report_id, program, title, url, vuln_type, severity, weakness,
    bounty, upvotes, disclosed_at, body, has_body, content_sha256
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
"""

# ``**Severity:** high (7.5)`` style metadata lines in the report header block.
HEADER_RE = re.compile(r"^\*\*(Report|Severity|Weakness|Bounty):\*\*\s*(.*?)\s*$")
H1_RE = re.compile(r"^#\s+(.*\S)\s*$")
REPORT_ID_RE = re.compile(r"^(\d+)\.md$")
LINK_ID_RE = re.compile(r"/reports/(\d+)")
ANY_ID_RE = re.compile(r"(\d+)\s*$")
AMOUNT_RE = re.compile(r"[-+]?\d+(?:\.\d+)?")


class DatasetError(RuntimeError):
    """Raised when the dataset layout is unusable."""


def _text(value: Any) -> str:
    return "" if value is None else str(value).strip()


def _parse_float(value: Any) -> float | None:
    """Parse a CSV/header money or count field; return None when absent."""
    if value is None:
        return None
    if isinstance(value, (int, float)):
        return float(value)
    raw = str(value).strip().replace(",", "").replace("$", "")
    if not raw:
        return None
    match = AMOUNT_RE.search(raw)
    if not match:
        return None
    try:
        return float(match.group(0))
    except ValueError:
        return None


def _parse_int(value: Any) -> int | None:
    number = _parse_float(value)
    return None if number is None else int(number)


def parse_report_markdown(text: str, report_id: str) -> dict[str, Any]:
    """Split a report markdown file into title, header metadata and body.

    The generated files start with ``# <title>`` followed by a header block of
    ``**Report:**`` / ``**Severity:**`` / ``**Weakness:**`` / ``**Bounty:**``
    lines and a blank line before the report body.  Only that leading block is
    stripped; anything later in the document is body text.
    """
    lines = text.splitlines()
    title = ""
    index = 0
    if lines:
        match = H1_RE.match(lines[0])
        if match:
            title = match.group(1).strip()
            index = 1

    severity = ""
    weakness = ""
    bounty: float | None = None
    while index < len(lines):
        line = lines[index]
        if not line.strip():
            index += 1
            continue
        match = HEADER_RE.match(line)
        if not match:
            break
        key, value = match.group(1), match.group(2)
        if key == "Severity":
            severity = value
        elif key == "Weakness":
            weakness = value
        elif key == "Bounty":
            bounty = _parse_float(value)
        index += 1

    body = "\n".join(lines[index:]).strip()
    return {
        "title": title,
        "severity": severity,
        "weakness": weakness,
        "bounty": bounty,
        "body": body,
    }


def _resolve_git_commit(dataset: Path) -> str:
    """Return the dataset commit hash by reading git metadata, or 'unknown'."""
    git_dir = dataset / ".git"
    try:
        if git_dir.is_file():
            payload = git_dir.read_text(encoding="utf-8", errors="replace").strip()
            if payload.lower().startswith("gitdir:"):
                git_dir = (dataset / payload[len("gitdir:"):].strip()).resolve()
        if not git_dir.is_dir():
            return "unknown"

        head = (git_dir / "HEAD").read_text(encoding="utf-8", errors="replace").strip()
        if head.startswith("ref:"):
            ref = head[len("ref:"):].strip()
            ref_file = git_dir / ref
            if ref_file.is_file():
                value = ref_file.read_text(encoding="utf-8", errors="replace").strip()
                if re.fullmatch(r"[0-9a-f]{40}", value):
                    return value
            packed = git_dir / "packed-refs"
            if packed.is_file():
                for line in packed.read_text(encoding="utf-8", errors="replace").splitlines():
                    line = line.strip()
                    if not line or line.startswith("#") or line.startswith("^"):
                        continue
                    parts = line.split()
                    if len(parts) == 2 and parts[1] == ref:
                        if re.fullmatch(r"[0-9a-f]{40}", parts[0]):
                            return parts[0]
            return "unknown"
        if re.fullmatch(r"[0-9a-f]{40}", head):
            return head
    except OSError:
        return "unknown"
    return "unknown"


def _metadata_fingerprint(
    report_id: str,
    program: str,
    title: str,
    vuln_type: str,
    bounty: float | None,
    upvotes: int | None,
) -> str:
    """Stable digest for rows that have no markdown body on disk."""
    parts = [
        report_id,
        program,
        title,
        vuln_type,
        "" if bounty is None else f"{bounty:.2f}",
        "" if upvotes is None else str(upvotes),
    ]
    payload = "\x1f".join(parts).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()


def _read_csv(csv_path: Path) -> tuple[dict[str, dict[str, Any]], int]:
    rows: dict[str, dict[str, Any]] = {}
    if not csv_path.is_file():
        return rows, 0
    count = 0
    with csv_path.open("r", encoding="utf-8-sig", newline="") as handle:
        reader = csv.DictReader(handle)
        for raw in reader:
            count += 1
            link = _text(raw.get("link"))
            match = LINK_ID_RE.search(link) or ANY_ID_RE.search(link)
            if not match:
                continue
            report_id = match.group(1)
            rows[report_id] = {
                "program": _text(raw.get("program")),
                "title": _text(raw.get("title")),
                "vuln_type": _text(raw.get("vuln_type")),
                "bounty": _parse_float(raw.get("bounty")),
                "upvotes": _parse_int(raw.get("upvotes")),
            }
    return rows, count


def _iter_markdown(reports_dir: Path) -> Iterable[tuple[str, Path]]:
    if not reports_dir.is_dir():
        return
    entries = []
    for entry in os.scandir(reports_dir):
        if not entry.is_file():
            continue
        match = REPORT_ID_RE.match(entry.name)
        if match:
            entries.append((match.group(1), Path(entry.path)))
    entries.sort(key=lambda item: int(item[0]))
    yield from entries


def _build_rows(dataset: Path) -> tuple[dict[str, dict[str, Any]], int, int]:
    """Read the local dataset into a report-id keyed record mapping."""
    csv_rows, csv_count = _read_csv(dataset / "data.csv")
    records: dict[str, dict[str, Any]] = {}
    md_count = 0

    for report_id, path in _iter_markdown(dataset / "reports"):
        md_count += 1
        raw = path.read_bytes()
        digest = hashlib.sha256(raw).hexdigest()
        parsed = parse_report_markdown(raw.decode("utf-8", errors="replace"), report_id)
        csv_row = csv_rows.get(report_id, {})
        body = parsed["body"]
        records[report_id] = {
            "program": csv_row.get("program", ""),
            "title": parsed["title"] or _text(csv_row.get("title")) or report_id,
            "vuln_type": csv_row.get("vuln_type", ""),
            "severity": parsed["severity"],
            "weakness": parsed["weakness"],
            "bounty": csv_row.get("bounty") if csv_row.get("bounty") is not None else parsed["bounty"],
            "upvotes": csv_row.get("upvotes"),
            "disclosed_at": "",
            "body": body,
            # Header-only markdown files exist; keep has_body tied to real text.
            "has_body": 1 if body else 0,
            "content_sha256": digest,
        }

    for report_id, csv_row in csv_rows.items():
        if report_id in records:
            continue
        records[report_id] = {
            "program": csv_row["program"],
            "title": csv_row["title"] or report_id,
            "vuln_type": csv_row["vuln_type"],
            "severity": "",
            "weakness": "",
            "bounty": csv_row["bounty"],
            "upvotes": csv_row["upvotes"],
            "disclosed_at": "",
            "body": "",
            "has_body": 0,
            "content_sha256": _metadata_fingerprint(
                report_id,
                csv_row["program"],
                csv_row["title"] or report_id,
                csv_row["vuln_type"],
                csv_row["bounty"],
                csv_row["upvotes"],
            ),
        }

    return records, csv_count, md_count


def _read_supplement(path: Path) -> list[dict[str, Any]]:
    """Load the JSONL supplement; first occurrence of a report id wins."""
    rows: list[dict[str, Any]] = []
    seen: set[str] = set()
    if not path.is_file():
        raise DatasetError(f"supplement file not found: {path}")
    with path.open("r", encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, 1):
            line = line.strip()
            if not line:
                continue
            try:
                payload = json.loads(line)
            except json.JSONDecodeError as exc:
                raise DatasetError(
                    f"invalid supplement JSON at {path}:{line_number}: {exc}"
                ) from exc
            report_id = _text(payload.get("report_id"))
            body = _text(payload.get("body"))
            if not report_id or not body or report_id in seen:
                continue
            if not report_id.isdigit():
                raise DatasetError(
                    f"invalid report id at {path}:{line_number}: {report_id!r}"
                )
            seen.add(report_id)
            upvotes = payload.get("upvotes")
            rows.append(
                {
                    "report_id": report_id,
                    "title": _text(payload.get("title")),
                    "body": body,
                    "weakness": _text(payload.get("weakness")),
                    "program": _text(payload.get("program")),
                    "disclosed_at": _text(payload.get("disclosed_at")),
                    "upvotes": _parse_int(upvotes) if upvotes is not None else None,
                }
            )
    return rows


def _merge_supplement(
    records: dict[str, dict[str, Any]], supplement_rows: Iterable[dict[str, Any]]
) -> tuple[int, int]:
    """Merge supplement rows into the local records.

    Existing rows are only enriched when their body/metadata is empty; a
    non-empty local value always wins.  Unknown report ids become new rows.

    Returns ``(filled_bodies, added_reports)``.
    """
    filled = 0
    added = 0
    for row in supplement_rows:
        report_id = row["report_id"]
        record = records.get(report_id)
        if record is None:
            body = row["body"]
            records[report_id] = {
                "program": row["program"],
                "title": row["title"] or report_id,
                "vuln_type": row["weakness"],
                "severity": "",
                "weakness": row["weakness"],
                "bounty": None,
                "upvotes": row["upvotes"],
                "disclosed_at": row["disclosed_at"],
                "body": body,
                "has_body": 1,
                "content_sha256": hashlib.sha256(body.encode("utf-8")).hexdigest(),
            }
            added += 1
            continue

        if not record["body"].strip():
            body = row["body"]
            record["body"] = body
            record["has_body"] = 1
            # The digest describes the body text once it comes from the supplement.
            record["content_sha256"] = hashlib.sha256(body.encode("utf-8")).hexdigest()
            filled += 1
        if not record["weakness"]:
            record["weakness"] = row["weakness"]
        if not record["program"]:
            record["program"] = row["program"]
        if not record["disclosed_at"]:
            record["disclosed_at"] = row["disclosed_at"]
        if record["upvotes"] is None:
            record["upvotes"] = row["upvotes"]
    return filled, added


def _materialize_rows(
    records: dict[str, dict[str, Any]]
) -> tuple[list[tuple[Any, ...]], int]:
    """Sort records by numeric id and flatten them into insert tuples."""
    ordered = sorted(records.items(), key=lambda item: int(item[0]))
    rows: list[tuple[Any, ...]] = []
    with_body = 0
    for report_id, record in ordered:
        if record["has_body"]:
            with_body += 1
        rows.append(
            (
                report_id,
                record["program"],
                record["title"],
                f"https://hackerone.com/reports/{report_id}",
                record["vuln_type"],
                record["severity"],
                record["weakness"],
                record["bounty"],
                record["upvotes"],
                record["disclosed_at"],
                record["body"],
                record["has_body"],
                record["content_sha256"],
            )
        )
    return rows, with_body


def build_index(
    dataset: Path, output: Path, supplement: Path | str | None = None
) -> dict[str, Any]:
    """Build the FTS5 index and return a summary dictionary."""
    started = time.monotonic()
    dataset = Path(dataset).expanduser().resolve()
    output = Path(output).expanduser().absolute()
    supplement_path = Path(supplement).expanduser().absolute() if supplement else None

    if not dataset.is_dir():
        raise DatasetError(f"dataset directory not found: {dataset}")

    output.parent.mkdir(parents=True, exist_ok=True)
    records, csv_count, md_count = _build_rows(dataset)
    supplement_rows = 0
    filled_bodies = 0
    added_reports = 0
    if supplement_path is not None:
        supplement_records = _read_supplement(supplement_path)
        supplement_rows = len(supplement_records)
        filled_bodies, added_reports = _merge_supplement(records, supplement_records)
    rows, with_body = _materialize_rows(records)
    commit = _resolve_git_commit(dataset)
    report_count = len(rows)
    if report_count == 0:
        raise DatasetError(f"no reports found under {dataset}")

    snapshot_id = hashlib.sha256(
        f"{commit}:{report_count}:{with_body}".encode("utf-8")
    ).hexdigest()
    built_at = datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")

    meta = {
        "schema_version": SCHEMA_VERSION,
        "dataset_path": str(dataset),
        "dataset_commit": commit,
        "built_at": built_at,
        "report_count": str(report_count),
        "with_body_count": str(with_body),
        "snapshot_id": snapshot_id,
        "supplement_path": str(supplement_path) if supplement_path else "",
        "supplement_rows": str(supplement_rows),
        "filled_bodies": str(filled_bodies),
        "added_reports": str(added_reports),
    }

    fd, tmp_name = tempfile.mkstemp(
        prefix=".hackerone-index-", suffix=".sqlite3.tmp", dir=str(output.parent)
    )
    os.close(fd)
    tmp_path = Path(tmp_name)
    try:
        connection = sqlite3.connect(str(tmp_path))
        try:
            # Rollback journal: the service opens the file read-only, and
            # WAL would need a writable -shm alongside it.
            connection.execute("PRAGMA journal_mode=DELETE")
            connection.execute("PRAGMA synchronous=OFF")
            connection.executescript(SCHEMA_SQL)
            connection.executemany(INSERT_SQL, rows)
            connection.commit()

            connection.execute("INSERT INTO reports_fts(reports_fts) VALUES('optimize')")
            connection.executemany(
                "INSERT INTO meta(key, value) VALUES (?, ?)", list(meta.items())
            )
            connection.commit()

            # VACUUM is intentionally skipped: it may renumber the implicit
            # rowids of a table without an INTEGER PRIMARY KEY, which would
            # desynchronise the external-content FTS index.
            connection.execute("ANALYZE")
            connection.execute("PRAGMA optimize")
            connection.commit()
            connection.execute("PRAGMA journal_mode=DELETE")
        finally:
            connection.close()

        os.chmod(tmp_path, 0o600)
        os.replace(tmp_path, output)
    except BaseException:
        for suffix in ("", "-journal", "-wal", "-shm"):
            try:
                Path(str(tmp_path) + suffix).unlink()
            except OSError:
                pass
        raise

    elapsed = time.monotonic() - started
    return {
        "output": str(output),
        "size_bytes": output.stat().st_size,
        "report_count": report_count,
        "with_body_count": with_body,
        "csv_rows": csv_count,
        "md_files": md_count,
        "dataset_commit": commit,
        "supplement_path": str(supplement_path) if supplement_path else "",
        "supplement_rows": supplement_rows,
        "filled_bodies": filled_bodies,
        "added_reports": added_reports,
        "elapsed_seconds": round(elapsed, 3),
    }


def _resolve_supplement(value: str | None) -> Path | None:
    """Pick the supplement path: CLI flag, env var, then the default file."""
    if value is not None:
        value = value.strip()
        if value in ("", "-", "none", "off"):
            return None
        return Path(value).expanduser()
    env_value = os.environ.get("HACKERONE_SUPPLEMENT", "").strip()
    if env_value:
        if env_value in ("-", "none", "off"):
            return None
        return Path(env_value).expanduser()
    default = Path(DEFAULT_SUPPLEMENT).expanduser()
    return default if default.is_file() else None


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Build a standalone SQLite FTS5 index of HackerOne reports."
    )
    parser.add_argument(
        "--dataset",
        default=os.environ.get("HACKERONE_DATASET", DEFAULT_DATASET),
        help="dataset directory containing data.csv and reports/",
    )
    parser.add_argument(
        "--output",
        default=os.environ.get("HACKERONE_INDEX", DEFAULT_OUTPUT),
        help="destination SQLite file",
    )
    parser.add_argument(
        "--supplement",
        default=None,
        help="JSONL report-body supplement (default: $HACKERONE_SUPPLEMENT or "
        f"{DEFAULT_SUPPLEMENT} when it exists; pass '' to disable)",
    )
    args = parser.parse_args(argv)

    try:
        summary = build_index(
            Path(args.dataset), Path(args.output), _resolve_supplement(args.supplement)
        )
    except DatasetError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    print(json.dumps(summary, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
