#!/usr/bin/env python3
"""Export report bodies from the official HackerOne disclosed-reports dataset.

The Hacker0x01/hackerone_disclosed_reports dataset is published as three
parquet splits (train/test/validation).  This script flattens the rows that
carry a non-empty ``vulnerability_information`` field into a JSONL supplement
consumed by ``build-hackerone-index.py --supplement``.

Requires pyarrow, so run it with a venv that has pyarrow installed:

    /home/max/.hermes/cache/scratch/parquet-venv/bin/python \
        scripts/hackerone_hf_supplement.py \
        --output ~/.local/share/bb-graph-assistant/hackerone/hf-supplement.jsonl

The output is deterministic: rows are deduplicated by report id (train wins
over test, test over validation) and sorted numerically, so re-running the
export over the same inputs yields byte-identical JSONL and SHA-256.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

SOURCE = "Hacker0x01/hackerone_disclosed_reports"
DATASET_LAST_MODIFIED = "2024-05-17"

DEFAULT_OUTPUT = "/home/max/.local/share/bb-graph-assistant/hackerone/hf-supplement.jsonl"
DEFAULT_PARQUETS = (
    "/tmp/hf_train.parquet",
    "/tmp/hf_test.parquet",
    "/tmp/hf_validation.parquet",
)

COLUMNS = (
    "id",
    "title",
    "vulnerability_information",
    "team",
    "weakness",
    "disclosed_at",
    "vote_count",
)


class SupplementError(RuntimeError):
    """Raised when the parquet inputs are missing or unreadable."""


def _text(value: Any) -> str:
    return "" if value is None else str(value).strip()


def _struct_text(value: Any, key: str) -> str:
    """Read one string field out of a parquet struct column."""
    if isinstance(value, dict):
        return _text(value.get(key))
    return ""


def _vote_count(value: Any) -> int | None:
    if value is None:
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def read_parquet(path: Path) -> list[dict[str, Any]]:
    """Return supplement records from one parquet split, in file order."""
    try:
        import pyarrow.parquet as pq
    except ImportError as exc:  # pragma: no cover - depends on the interpreter
        raise SupplementError(
            "pyarrow is required; run this script with the parquet venv"
        ) from exc

    if not path.is_file():
        raise SupplementError(f"parquet file not found: {path}")

    table = pq.read_table(str(path), columns=list(COLUMNS))
    records: list[dict[str, Any]] = []
    for row in table.to_pylist():
        report_id = _text(row.get("id"))
        body = _text(row.get("vulnerability_information"))
        if not report_id or not body:
            continue
        records.append(
            {
                "report_id": report_id,
                "title": _text(row.get("title")),
                "body": body,
                "weakness": _struct_text(row.get("weakness"), "name"),
                "program": _struct_text(row.get("team"), "handle"),
                "disclosed_at": _text(row.get("disclosed_at")),
                "upvotes": _vote_count(row.get("vote_count")),
                "source": SOURCE,
                "dataset_last_modified": DATASET_LAST_MODIFIED,
            }
        )
    return records


def collect(
    parquet_paths: Iterable[Path],
) -> tuple[list[dict[str, Any]], list[dict[str, Any]], int]:
    """Deduplicate records across splits, first split wins.

    Returns ``(records, per_file_stats, duplicates)`` where records are sorted
    by numeric report id for deterministic output.
    """
    merged: dict[str, dict[str, Any]] = {}
    stats: list[dict[str, Any]] = []
    duplicates = 0
    for path in parquet_paths:
        path = Path(path).expanduser()
        records = read_parquet(path)
        kept = 0
        for record in records:
            report_id = record["report_id"]
            if report_id in merged:
                duplicates += 1
                continue
            merged[report_id] = record
            kept += 1
        stats.append(
            {
                "path": str(path.resolve()),
                "rows_with_body": len(records),
                "kept": kept,
            }
        )
    ordered = sorted(merged.values(), key=lambda item: int(item["report_id"]))
    return ordered, stats, duplicates


def write_jsonl(records: list[dict[str, Any]], output: Path) -> str:
    """Write JSONL atomically and return the SHA-256 of the file bytes."""
    output = Path(output).expanduser().absolute()
    output.parent.mkdir(parents=True, exist_ok=True)
    tmp_path = output.with_name(output.name + ".tmp")
    digest = hashlib.sha256()
    try:
        with tmp_path.open("wb") as handle:
            for record in records:
                line = json.dumps(record, ensure_ascii=False, sort_keys=False).encode("utf-8")
                handle.write(line + b"\n")
                digest.update(line + b"\n")
        tmp_path.replace(output)
    except BaseException:
        try:
            tmp_path.unlink()
        except OSError:
            pass
        raise
    return digest.hexdigest()


def build_meta(
    records: list[dict[str, Any]],
    stats: list[dict[str, Any]],
    duplicates: int,
    sha256: str,
    output: Path,
) -> dict[str, Any]:
    return {
        "source": SOURCE,
        "dataset_last_modified": DATASET_LAST_MODIFIED,
        "generated_at": datetime.now(timezone.utc)
        .isoformat(timespec="seconds")
        .replace("+00:00", "Z"),
        "output": str(output),
        "rows": len(records),
        "sha256": sha256,
        "duplicates_dropped": duplicates,
        "files": stats,
    }


def run(parquet_paths: Iterable[Path], output: Path, meta_path: Path | None = None) -> dict[str, Any]:
    started = time.monotonic()
    records, stats, duplicates = collect(parquet_paths)
    if not records:
        raise SupplementError("no rows with vulnerability_information found")
    output = Path(output).expanduser().absolute()
    sha256 = write_jsonl(records, output)
    meta_path = Path(meta_path).expanduser().absolute() if meta_path else output.with_suffix(".meta.json")
    meta = build_meta(records, stats, duplicates, sha256, output)
    meta_path.write_text(
        json.dumps(meta, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    summary = {
        "output": str(output),
        "meta": str(meta_path),
        "rows": len(records),
        "sha256": sha256,
        "duplicates_dropped": duplicates,
        "size_bytes": output.stat().st_size,
        "elapsed_seconds": round(time.monotonic() - started, 3),
    }
    return summary


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Export HackerOne parquet report bodies to a JSONL supplement."
    )
    parser.add_argument(
        "--parquet",
        action="append",
        dest="parquets",
        help="parquet split in precedence order (repeatable); defaults to the "
        "downloaded train/test/validation files",
    )
    parser.add_argument(
        "--output",
        default=DEFAULT_OUTPUT,
        help="destination JSONL file",
    )
    parser.add_argument(
        "--meta",
        default=None,
        help="destination meta JSON (default: <output>.meta.json)",
    )
    args = parser.parse_args(argv)

    parquets = args.parquets or list(DEFAULT_PARQUETS)
    try:
        summary = run([Path(p) for p in parquets], Path(args.output), args.meta)
    except SupplementError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    print(json.dumps(summary, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
