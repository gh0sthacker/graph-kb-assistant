from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest


_SCRIPT = Path(__file__).parents[1] / "scripts" / "export_redamon_kb_receipt.py"
_SPEC = importlib.util.spec_from_file_location("export_redamon_kb_receipt", _SCRIPT)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(_MODULE)


class _Session:
    def __init__(self, rows: list[dict[str, str]]) -> None:
        self.rows = rows

    def __enter__(self) -> _Session:
        return self

    def __exit__(self, *args: object) -> None:
        return None

    def run(self, query: str, **kwargs: object) -> list[dict[str, str]]:
        assert "ORDER BY c.chunk_id" in query
        assert not kwargs
        return self.rows


class _Driver:
    def __init__(self, rows: list[dict[str, str]]) -> None:
        self.rows = rows

    def session(self, **kwargs: object) -> _Session:
        assert kwargs["default_access_mode"] == "READ"
        return _Session(self.rows)


def _rows() -> list[dict[str, str]]:
    return sorted(({
        "chunk_id": f"chunk-{name}",
        "source_name": name,
        "title": f"Title {name}",
        "content": f"Content {name}",
        "source_path": f"source/{name}",
        "cve_id": f"CVE-{name}" if name == "nvd" else None,
        "edb_id": f"EDB-{name}" if name == "exploitdb" else None,
    } for name in _MODULE.SOURCE_FAMILIES), key=lambda row: row["chunk_id"])


def test_redamon_loader_driver_forces_read_access_mode() -> None:
    class InnerDriver:
        def session(self, **kwargs: object) -> dict[str, object]:
            return kwargs

    readonly = _MODULE._ReadOnlyDriver(InnerDriver(), "READ")

    assert readonly.session(fetch_size=1000) == {
        "fetch_size": 1000, "default_access_mode": "READ",
    }


def test_redamon_inventory_is_canonical_and_covers_every_family() -> None:
    rows = _rows()
    inventory, family_chunks, family_docs = _MODULE._runtime_inventory(
        _Driver(rows), [row["chunk_id"] for row in rows],
    )

    assert inventory["chunk_count"] == len(_MODULE.SOURCE_FAMILIES)
    assert [row["name"] for row in inventory["sources"]] == sorted(_MODULE.SOURCE_FAMILIES)
    assert all(count == 1 for count in family_chunks.values())
    assert all(count == 1 for count in family_docs.values())


def test_redamon_inventory_rejects_faiss_neo4j_drift() -> None:
    rows = _rows()
    ids = [row["chunk_id"] for row in rows]
    with pytest.raises(_MODULE.ReceiptError, match="FAISS IDs do not exactly match"):
        _MODULE._runtime_inventory(_Driver(rows), ids[:-1])


def test_redamon_inventory_rejects_non_profile_chunks() -> None:
    rows = _rows()
    rows.append({
        "chunk_id": "extra", "source_name": "personal_notes", "title": "Private",
        "content": "not in the public full profile", "source_path": "private/note",
        "cve_id": None, "edb_id": None,
    })

    with pytest.raises(_MODULE.ReceiptError, match="missing identity or provenance"):
        _MODULE._runtime_inventory(_Driver(rows), [row["chunk_id"] for row in rows])


def test_redamon_document_counts_use_logical_nvd_and_exploitdb_records() -> None:
    rows = _rows()
    rows.extend([
        {
            "chunk_id": "chunk-nvd-2", "source_name": "nvd", "title": "CVE-NVD-2",
            "content": "second NVD item", "source_path": "source/nvd", "cve_id": "CVE-NVD-2", "edb_id": None,
        },
        {
            "chunk_id": "chunk-exploitdb-2", "source_name": "exploitdb", "title": "EDB-EXP-2",
            "content": "second exploit", "source_path": "source/exploitdb", "cve_id": None, "edb_id": "EDB-EXP-2",
        },
    ])
    rows.sort(key=lambda row: row["chunk_id"])

    _inventory, _family_chunks, family_docs = _MODULE._runtime_inventory(
        _Driver(rows), [row["chunk_id"] for row in rows],
    )

    assert family_docs["nvd"] == 2
    assert family_docs["exploitdb"] == 2


def test_absent_candidate_channel_score_is_encoded_as_finite_zero() -> None:
    assert _MODULE._score_or_zero(None, "vector cosine") == 0.0
    assert _MODULE._score_or_zero(0.25, "vector cosine") == 0.25
    with pytest.raises(_MODULE.ReceiptError, match="finite vector cosine"):
        _MODULE._score_or_zero(float("nan"), "vector cosine")
