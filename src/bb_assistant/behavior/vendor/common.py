"""Shared, local-only identities and source receipts for behavior analysis."""
from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .control import digest_json, file_sha256


class BehaviorError(ValueError):
    """Invalid or incomplete local behavior-analysis contract."""


def entity_id(kind: str, identity: Any) -> str:
    if not isinstance(kind, str) or not kind or not kind.replace("_", "").isalnum():
        raise BehaviorError("Invalid entity kind")
    return kind + "-" + digest_json({"kind": kind, "identity": identity})


def source_reference(path: str | Path, locator: str = "$") -> dict:
    selected = Path(path).resolve(strict=True)
    if not selected.is_file() or not isinstance(locator, str) or not locator:
        raise BehaviorError("Source must identify a regular file and record")
    return {"path": str(selected), "sha256": file_sha256(selected), "record_locator": locator}


def read_json(path: str | Path) -> Any:
    def pairs(items: list[tuple[str, Any]]) -> dict:
        result = {}
        for key, value in items:
            if key in result:
                raise BehaviorError("Duplicate JSON key")
            result[key] = value
        return result

    def nonfinite(_value: str) -> None:
        raise BehaviorError("Non-finite JSON value")

    try:
        return json.loads(Path(path).read_text(encoding="utf-8"), object_pairs_hook=pairs,
                          parse_constant=nonfinite)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise BehaviorError("Unable to read the selected JSON source") from exc
