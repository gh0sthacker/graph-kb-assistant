"""Shared, dependency-free integrity and public-artifact helpers."""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import tempfile
from typing import Any


class ArtifactSafetyError(ValueError):
    """A rejected artifact operation; never includes secret payloads."""


def canonical_json(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False)


def digest_json(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode()).hexdigest()


def file_sha256(path: Path) -> str:
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def atomic_json(path: Path, value: Any) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix="." + path.name, dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            stream.write(canonical_json(value) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


_PRIVATE_KEYS = {"authorization", "cookie", "set-cookie", "password", "passwd", "access_token", "refresh_token", "csrf_token", "api_key", "apikey", "private_key", "raw_body", "raw_request", "raw_response", "private_oracle", "ground_truth", "known_vulnerabilities", "session_store", "credentials"}
_SECRET = re.compile(r"(?i)(?:Bearer\s+[A-Za-z0-9._~+/=-]{8,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|(?:password|passwd|api[_-]?key|access[_-]?token|secret)\s*[=:]\s*[\"']?(?!\[REDACTED\]|<REDACTED>)[A-Za-z0-9._~+/=-]{8,}|\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b)")


def assert_public(value: Any) -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if str(key).lower().replace("-", "_") in {k.replace("-", "_") for k in _PRIVATE_KEYS} and child not in (None, "", "[REDACTED]", "<REDACTED>"):
                raise ArtifactSafetyError("Sensitive or private field is not allowed in a public evidence artifact")
            assert_public(child)
    elif isinstance(value, (list, tuple)):
        for child in value:
            assert_public(child)
    elif isinstance(value, str) and _SECRET.search(value):
        raise ArtifactSafetyError("Secret-like material is not allowed in a public evidence artifact")
    canonical_json(value)
