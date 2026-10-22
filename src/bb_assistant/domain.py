"""Harness-neutral data contracts for project and Caido capture identity."""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import re
from typing import Any


_SAFE_ID = re.compile(r"^[\x21-\x7e]{1,512}$")
_WIRE_FIDELITY = {"source_bytes", "reconstructed_text", "unknown"}


def _stable_json(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


@dataclass(frozen=True, slots=True)
class CaidoProjectRef:
    instance_id: str
    project_id: str
    display_name: str | None = None

    def __post_init__(self) -> None:
        if not _SAFE_ID.fullmatch(self.instance_id) or not _SAFE_ID.fullmatch(self.project_id):
            raise ValueError("Caido instance and project IDs must be non-empty opaque identifiers")

    @property
    def storage_key(self) -> str:
        identity = {"instance_id": self.instance_id, "project_id": self.project_id}
        return hashlib.sha256(_stable_json(identity)).hexdigest()

    def same_project(self, other: "CaidoProjectRef") -> bool:
        return self.instance_id == other.instance_id and self.project_id == other.project_id


@dataclass(frozen=True, slots=True)
class CaidoExchangeRef:
    project: CaidoProjectRef
    request_id: str

    def __post_init__(self) -> None:
        if not _SAFE_ID.fullmatch(self.request_id):
            raise ValueError("Caido request ID must be a non-empty opaque identifier")

    @property
    def observation_id(self) -> str:
        identity = {
            "instance_id": self.project.instance_id,
            "project_id": self.project.project_id,
            "request_id": self.request_id,
        }
        return hashlib.sha256(_stable_json(identity)).hexdigest()


@dataclass(frozen=True, slots=True)
class HttpExchange:
    """One Caido row with stored message bytes and explicit wire-fidelity labels."""

    ref: CaidoExchangeRef
    captured_at: str | None
    request_raw: bytes
    response_raw: bytes | None
    method: str | None = None
    scheme: str | None = None
    host: str | None = None
    port: int | None = None
    target: str | None = None
    response_status: int | None = None
    request_truncated: bool | None = None
    response_truncated: bool | None = None
    source_cursor: str | None = None
    request_wire_fidelity: str = "unknown"
    response_wire_fidelity: str = "unknown"

    def __post_init__(self) -> None:
        if not isinstance(self.request_raw, bytes):
            raise TypeError("request_raw must be bytes")
        if self.response_raw is not None and not isinstance(self.response_raw, bytes):
            raise TypeError("response_raw must be bytes or None")
        if not isinstance(self.request_wire_fidelity, str) or self.request_wire_fidelity not in _WIRE_FIDELITY:
            raise ValueError("request_wire_fidelity is unsupported")
        if not isinstance(self.response_wire_fidelity, str) or self.response_wire_fidelity not in _WIRE_FIDELITY:
            raise ValueError("response_wire_fidelity is unsupported")
