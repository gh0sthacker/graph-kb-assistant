"""Local, path-free registry of Caido projects known to the assistant."""
from __future__ import annotations

import json
import os
from pathlib import Path
import tempfile
from typing import Any

from .domain import CaidoProjectRef
from .paths import data_root, ensure_private_directory
from .project_lock import project_maintenance_lock


_REGISTRY_VERSION = 1


def _projects_root(root: Path | None = None) -> Path:
    base = data_root() if root is None else root.expanduser().resolve()
    return base / "projects"


def registry_path(root: Path | None = None) -> Path:
    return _projects_root(root) / "registry.json"


def load_project_refs(root: Path | None = None) -> tuple[CaidoProjectRef, ...]:
    """Load validated project identities; registry values never become paths."""
    path = registry_path(root)
    if path.parent.is_symlink():
        raise ValueError("Project registry directory must not be a symbolic link")
    if not path.parent.exists():
        return ()
    if not path.parent.is_dir():
        raise ValueError("Project registry directory is not a directory")
    if path.is_symlink():
        raise ValueError("Project registry must not be a symbolic link")
    if not path.exists():
        return ()
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ValueError("Project registry cannot be read") from exc
    if (
        not isinstance(document, dict)
        or document.get("schema_version") != _REGISTRY_VERSION
        or not isinstance(document.get("projects"), list)
        or set(document) != {"schema_version", "projects"}
    ):
        raise ValueError("Project registry has an unsupported format")

    refs: list[CaidoProjectRef] = []
    seen: set[str] = set()
    for row in document["projects"]:
        if (
            not isinstance(row, dict)
            or set(row) != {"project_key", "instance_id", "project_id", "display_name"}
            or not isinstance(row.get("project_key"), str)
            or not isinstance(row.get("instance_id"), str)
            or not isinstance(row.get("project_id"), str)
            or (row.get("display_name") is not None and not isinstance(row.get("display_name"), str))
        ):
            raise ValueError("Project registry contains a malformed row")
        try:
            ref = CaidoProjectRef(row["instance_id"], row["project_id"], row["display_name"])
        except (TypeError, ValueError) as exc:
            raise ValueError("Project registry contains an invalid project identity") from exc
        if ref.storage_key != row["project_key"] or ref.storage_key in seen:
            raise ValueError("Project registry identity hash is inconsistent")
        seen.add(ref.storage_key)
        refs.append(ref)
    return tuple(sorted(refs, key=lambda ref: (ref.display_name or "", ref.storage_key)))


def register_project(ref: CaidoProjectRef, root: Path | None = None) -> None:
    """Atomically add/update one project identity in the local catalog."""
    path = registry_path(root)
    ensure_private_directory(path.parent)
    # Projects have independent maintenance directories, but share this
    # registry file. Serialize the complete read/modify/replace transaction at
    # the catalog directory so concurrent initializations cannot lose rows.
    with project_maintenance_lock(path.parent):
        rows: dict[str, dict[str, Any]] = {
            item.storage_key: {
                "project_key": item.storage_key,
                "instance_id": item.instance_id,
                "project_id": item.project_id,
                "display_name": item.display_name,
            }
            for item in load_project_refs(root)
        }
        rows[ref.storage_key] = {
            "project_key": ref.storage_key,
            "instance_id": ref.instance_id,
            "project_id": ref.project_id,
            "display_name": ref.display_name,
        }
        payload = {
            "schema_version": _REGISTRY_VERSION,
            "projects": sorted(rows.values(), key=lambda row: (row["display_name"] or "", row["project_key"])),
        }
        encoded = json.dumps(payload, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
        fd, temporary_name = tempfile.mkstemp(prefix=".registry-", suffix=".tmp", dir=path.parent)
        temporary = Path(temporary_name)
        try:
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
                handle.write(encoded)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, path)
            try:
                path.chmod(0o600)
            except OSError:
                pass
        finally:
            try:
                temporary.unlink()
            except FileNotFoundError:
                pass
