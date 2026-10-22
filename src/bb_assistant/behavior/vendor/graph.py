"""Local, derived behavior graph. Reachability never grants finding status.

The projection keeps immutable assertions and historical source versions. A
slice is a bounded inventory of compatible observations, not an executable
path or a claim that the application's behavior is completely known.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import re
import sqlite3
from typing import Any
from urllib.parse import unquote, urlsplit, parse_qsl

from .common import BehaviorError, entity_id
from .control import ArtifactSafetyError, assert_public, canonical_json, digest_json, file_sha256
from ...paths import data_root


KINDS = frozenset({"asset", "endpoint", "actor", "session_context", "object",
                   "observation", "parameter", "condition", "flow",
                   "classifier_signal", "hypothesis", "request_variant", "response_variant"})
PREDICATES = frozenset({"exposes", "has_parameter", "invokes", "references", "returns",
                        "owned_by", "visible_to", "value_used_in", "requires", "produces",
                        "supports", "contradicts", "contains", "precedes",
                        "uses_request_variant", "received_response",
                        "referrer_context_for", "redirects_to"})
ORIGIN_KINDS = frozenset({"parsed", "operator_asserted", "model_proposed", "check_result"})
EVIDENCE_STATES = frozenset({"proposed", "observed", "corroborated", "contested"})
_CONTEXT_KEYS = ("program", "origin", "actor", "session_version", "tenant", "role",
                 "object_state", "scenario", "scenario_step")
_UNKNOWN = (None, "", "unknown")
_SHA = re.compile(r"^[a-f0-9]{64}$")
_PROGRAM = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
_PRIVATE_METADATA = frozenset({"raw", "body", "headers", "header_values", "request_body",
                               "response_body", "field_values", "token", "session_id",
                               "session_cookie", "credential", "pii"})
_METADATA_SECRET = re.compile(
    r"(?i)(?:\b(?:password|passwd|api[_-]?key|access[_-]?token|refresh[_-]?token|"
    r"csrf[_-]?token|session[_-]?(?:id|cookie)|cookie|secret)\s*[=:]\s*"
    r"(?!\[REDACTED\]|<REDACTED>)[^\s&;,]+|\b[^\s/@]+@[^\s/]+\.[A-Za-z]{2,}\b)"
)


def _public(value: Any) -> None:
    """Reject secret/raw metadata as well as the common public-state blacklist."""
    try:
        assert_public(value)
        _safe_metadata(value)
    except (ArtifactSafetyError, TypeError, ValueError, RecursionError) as exc:
        raise BehaviorError("Unsafe or non-JSON graph material") from exc


def _safe_metadata(value: Any, parent: str = "") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if not isinstance(key, str):
                raise BehaviorError("Graph object keys must be strings")
            name = key.lower().replace("-", "_")
            if name in _PRIVATE_METADATA and child not in (None, "", "[REDACTED]", "<REDACTED>"):
                raise BehaviorError("Raw or private graph metadata is forbidden")
            if parent in {"fields", "header_metadata"} and name in {"value", "values"}:
                raise BehaviorError("Field and HTTP header values are forbidden")
            _safe_metadata(child, key)
    elif isinstance(value, (list, tuple)):
        for child in value:
            _safe_metadata(child, parent)
    elif isinstance(value, str):
        decoded = value
        for _ in range(3):
            decoded = unquote(decoded)
        if _METADATA_SECRET.search(decoded):
            raise BehaviorError("Secret-like metadata is forbidden")
        if decoded.startswith(("http://", "https://")):
            parsed = urlsplit(decoded)
            if parsed.username is not None or parsed.password is not None:
                raise BehaviorError("URL credentials are forbidden")
            if any(item not in ("", "[REDACTED]", "<REDACTED>")
                   for _name, item in parse_qsl(parsed.query, keep_blank_values=True)):
                raise BehaviorError("URL query values are forbidden")


def _exact_dict(value: Any, keys: set[str], label: str) -> dict:
    if not isinstance(value, dict) or set(value) != keys:
        raise BehaviorError("Invalid " + label + " shape")
    return value


def _list(value: Any, label: str) -> list:
    if not isinstance(value, list):
        raise BehaviorError(label + " must be a list")
    return value


def _identifier(value: Any, kind: str | None = None) -> str:
    if not isinstance(value, str) or "-" not in value:
        raise BehaviorError("Invalid typed graph ID")
    prefix, suffix = value.rsplit("-", 1)
    if not _SHA.fullmatch(suffix) or (kind is not None and prefix != kind):
        raise BehaviorError("Invalid typed graph ID")
    return value


def _source(ref: Any) -> dict:
    _exact_dict(ref, {"path", "sha256", "record_locator"}, "source reference")
    try:
        valid_path = (isinstance(ref["path"], str) and Path(ref["path"]).is_absolute()
                      and str(Path(ref["path"]).resolve()) == ref["path"])
    except (OSError, ValueError, RuntimeError):
        valid_path = False
    if (not valid_path or not isinstance(ref["sha256"], str) or not _SHA.fullmatch(ref["sha256"])
            or not isinstance(ref["record_locator"], str) or not ref["record_locator"].strip()):
        raise BehaviorError("Invalid source reference")
    return dict(ref)


def _source_pair(ref: dict) -> tuple[str, str]:
    return ref["path"], ref["sha256"]


def _sorted_refs(refs: list[dict]) -> list[dict]:
    return [json.loads(item) for item in sorted({canonical_json(ref) for ref in refs})]


def _refs(refs: Any, declared: set[tuple[str, str]]) -> list[dict]:
    _list(refs, "source_refs")
    if not refs:
        raise BehaviorError("Every graph record needs provenance")
    checked = [_source(ref) for ref in refs]
    if any(_source_pair(ref) not in declared for ref in checked):
        raise BehaviorError("Record references an undeclared source version")
    return checked


def _context(value: Any, program: str) -> dict:
    if not isinstance(value, dict):
        raise BehaviorError("Context must be an object")
    if value.get("program", program) != program:
        raise BehaviorError("Context belongs to another program")
    for key in _CONTEXT_KEYS:
        selected = value.get(key)
        if selected is not None and (not isinstance(selected, (str, int, bool))):
            raise BehaviorError("Context identity must be an opaque scalar")
    return dict(value)


def _http_status(value: Any) -> None:
    if isinstance(value, bool) or not isinstance(value, int) or not 100 <= value <= 599:
        raise BehaviorError("Observation status must be an HTTP status integer")


def _observation_states(value: Any) -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if key in {"status", "http_status", "status_code", "statusCode", "response_status"}:
                _http_status(child)
            elif key in {"evidence_state", "observation_state", "state"}:
                if not isinstance(child, str) or child not in EVIDENCE_STATES:
                    raise BehaviorError("Invented observation evidence state")
            elif key == "finding_state" and child is not None:
                raise BehaviorError("Observation cannot carry a finding state")
            _observation_states(child)
    elif isinstance(value, list):
        for child in value:
            _observation_states(child)


def _entity(record: Any, declared: set[tuple[str, str]], program: str) -> dict:
    _exact_dict(record, {"id", "kind", "data", "source_refs"}, "entity")
    if not isinstance(record["kind"], str) or record["kind"] not in KINDS or not isinstance(record["data"], dict):
        raise BehaviorError("Invalid entity kind or data")
    _identifier(record["id"], record["kind"])
    result = dict(record)
    result["source_refs"] = _sorted_refs(_refs(record["source_refs"], declared))
    data = record["data"]
    _context({key: data[key] for key in _CONTEXT_KEYS if key in data}, program)
    if "context" in data:
        _context(data["context"], program)
    if record["kind"] == "observation":
        _observation_states(data)
    if record["kind"] == "condition" and "value" in data:
        if not (type(data["value"]) is bool or data["value"] is None or data["value"] == "unknown"):
            raise BehaviorError("Condition value must be true, false or unknown")
    if record["kind"] == "classifier_signal":
        if (data.get("program") != program or not isinstance(data.get("capture_id"), str)
                or not data["capture_id"] or data.get("finding_status") != "candidate_only"
                or data.get("priority") not in {"critical", "high", "medium", "low"}
                or data.get("analysis_state") not in {"needs_review", "incomplete"}):
            raise BehaviorError("Invalid classifier signal or promoted status")
        categories = _list(data.get("categories"), "classifier categories")
        asvs_refs = _list(data.get("asvs_refs"), "classifier ASVS references")
        if (not categories or any(not isinstance(item, str) or not re.fullmatch(r"[a-z0-9_]{1,64}", item)
                                 for item in categories)
                or any(not isinstance(item, str) or not re.fullmatch(r"V[0-9V./]{1,32}", item)
                       for item in asvs_refs)):
            raise BehaviorError("Invalid classifier signal labels")
    if record["kind"] == "hypothesis":
        required = {"program", "packet_sha256", "analyst", "rank", "weakness_class", "claim",
                    "status", "evidence_strength", "impact_if_true", "missing_evidence",
                    "alternative_explanation", "plan_only"}
        if set(data) != required or data.get("program") != program:
            raise BehaviorError("Invalid offline hypothesis shape")
        if (not isinstance(data["packet_sha256"], str) or not _SHA.fullmatch(data["packet_sha256"])
                or not isinstance(data["analyst"], str) or not data["analyst"]
                or type(data["rank"]) is not int or data["rank"] < 1
                or not all(isinstance(data[key], str) and data[key].strip()
                           for key in ("weakness_class", "claim", "impact_if_true", "alternative_explanation"))
                or data["status"] != "candidate"
                or data["evidence_strength"] not in {"low", "medium", "high"}
                or type(data["plan_only"]) is not bool or data["plan_only"] is not True
                or not isinstance(data["missing_evidence"], list)
                or any(not isinstance(item, str) for item in data["missing_evidence"])):
            raise BehaviorError("Offline hypotheses must remain unverified candidates")
    return result


def _assertion(record: Any, declared: set[tuple[str, str]], program: str) -> dict:
    _exact_dict(record, {"id", "subject", "predicate", "object", "context", "origin_kind",
                         "evidence_state", "source_refs"}, "assertion")
    _identifier(record["id"], "assertion")
    _identifier(record["subject"])
    _identifier(record["object"])
    if (not isinstance(record["predicate"], str) or record["predicate"] not in PREDICATES
            or not isinstance(record["origin_kind"], str) or record["origin_kind"] not in ORIGIN_KINDS
            or not isinstance(record["evidence_state"], str) or record["evidence_state"] not in EVIDENCE_STATES):
        raise BehaviorError("Invalid assertion predicate, origin or evidence state")
    _context(record["context"], program)
    _refs(record["source_refs"], declared)
    identity = {key: value for key, value in record.items() if key != "id"}
    if record["id"] != entity_id("assertion", identity):
        raise BehaviorError("Assertion ID does not match its complete content")
    return dict(record)


def _card(record: Any, declared: set[tuple[str, str]], program: str) -> dict:
    _exact_dict(record, {"schema_version", "id", "capture_id", "program", "source_refs", "endpoint",
                         "context", "request_analysis", "response_analysis", "business_action",
                         "hypotheses", "missing_evidence", "analysis_state"}, "analysis card")
    if (type(record["schema_version"]) is not int or record["schema_version"] != 1
            or record["program"] != program or not isinstance(record["capture_id"], str)
            or not record["capture_id"] or not isinstance(record["analysis_state"], str)
            or record["analysis_state"] not in {"needs_review", "incomplete"}):
        raise BehaviorError("Invalid analysis card identity or state")
    _identifier(record["id"], "card")
    _context(record["context"], program)
    _refs(record["source_refs"], declared)
    if not isinstance(record["endpoint"], dict) or not isinstance(record["business_action"], dict):
        raise BehaviorError("Card endpoint and business action must be objects")
    if record["endpoint"].get("id") is not None:
        _identifier(record["endpoint"]["id"], "endpoint")
    for side in ("request_analysis", "response_analysis"):
        analysis = record[side]
        if (not isinstance(analysis, dict) or not isinstance(analysis.get("parse_state"), str)
                or analysis["parse_state"] not in {"complete", "incomplete"}):
            raise BehaviorError("Invalid card parse state")
        if side == "response_analysis" and analysis.get("status") is not None:
            _http_status(analysis["status"])
        if record["analysis_state"] != "incomplete" and analysis["parse_state"] == "incomplete":
            raise BehaviorError("Incomplete parsing cannot become a complete analysis card")
    for hypothesis in _list(record["hypotheses"], "hypotheses"):
        if not isinstance(hypothesis, dict) or hypothesis.get("outcome") != "candidate":
            raise BehaviorError("Graph import cannot promote a candidate")
    if any(not isinstance(item, str) for item in _list(record["missing_evidence"], "missing_evidence")):
        raise BehaviorError("Missing evidence must contain explicit text limitations")
    result = dict(record)
    result["source_refs"] = _sorted_refs(record["source_refs"])
    return result


def _integrity(sources: list[dict]) -> list[dict]:
    """Read only the explicitly imported source paths; never discover inputs."""
    by_path: dict[str, tuple[str, str | None]] = {}
    result = []
    for ref in sources:
        path = ref["path"]
        if path not in by_path:
            try:
                selected = Path(path)
                if not selected.exists():
                    by_path[path] = ("missing", None)
                elif not selected.is_file() or str(selected.resolve()) != path:
                    by_path[path] = ("unavailable", None)
                else:
                    by_path[path] = ("readable", file_sha256(selected))
            except OSError:
                by_path[path] = ("unavailable", None)
        state, actual_hash = by_path[path]
        state = ("current" if actual_hash == ref["sha256"] else "stale") if state == "readable" else state
        result.append({**ref, "integrity": state,
                       "freshness": "current" if state == "current" else
                       "stale" if state in {"stale", "missing"} else "unknown"})
    return result


def _record_freshness(record: dict, integrity: dict[tuple[str, str], str]) -> str:
    states = {integrity[_source_pair(ref)] for ref in record["source_refs"]}
    if states == {"current"}:
        return "current"
    if states.intersection({"stale", "missing"}):
        return "stale"
    return "unknown"


def _record_context(record: dict) -> dict:
    if record.get("kind") == "assertion":
        return record["context"]
    data = record.get("data", {})
    context = dict(data.get("context", {}))
    # Actor/object/session metadata may carry context directly rather than nested.
    for key in _CONTEXT_KEYS:
        if key in data and key not in context:
            context[key] = data[key]
    return context


def _compatible(candidate: dict, selected: dict) -> bool:
    return all(candidate.get(key) in _UNKNOWN or selected.get(key) in _UNKNOWN
               or type(candidate[key]) is type(selected[key]) and candidate[key] == selected[key]
               for key in _CONTEXT_KEYS)


def _bind(candidate: dict, selected: dict) -> None:
    for key in _CONTEXT_KEYS:
        if selected.get(key) in _UNKNOWN and candidate.get(key) not in _UNKNOWN:
            selected[key] = candidate[key]


class BehaviorGraph:
    """Transactional, idempotent local projection for one explicitly named program."""

    def __init__(self, db_path: str | Path | None, program: str):
        if not isinstance(program, str) or not _PROGRAM.fullmatch(program) or program in {".", ".."}:
            raise BehaviorError("Invalid program namespace")
        selected = (Path(db_path) if db_path is not None else
                    data_root() / "behavior" / program / "graph.sqlite3")
        self.db_path = selected.resolve()
        source_root = Path(__file__).resolve().parents[4]
        if (self.db_path == source_root or source_root in self.db_path.parents
                or str(selected).startswith("file:") or selected.is_dir()):
            raise BehaviorError("Behavior SQLite must be outside the source checkout")
        self.program = program
        self.db_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        self._db: sqlite3.Connection | None = sqlite3.connect(self.db_path, timeout=30)
        try:
            os.chmod(self.db_path, 0o600)
            self._db.execute("PRAGMA foreign_keys=ON")
            self._db.execute("PRAGMA journal_mode=DELETE")
            self._db.execute("BEGIN IMMEDIATE")
            self._db.execute("CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
            for table in ("sources", "entities", "assertions", "cards"):
                self._db.execute("CREATE TABLE IF NOT EXISTS " + table +
                                 " (id TEXT PRIMARY KEY, payload TEXT NOT NULL)")
            self._db.execute("CREATE TABLE IF NOT EXISTS imports (id TEXT PRIMARY KEY)")
            self._db.execute(
                """CREATE TABLE IF NOT EXISTS entity_source_refs (
                     entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
                     source_ref_sha256 TEXT NOT NULL,
                     payload TEXT NOT NULL,
                     PRIMARY KEY(entity_id,source_ref_sha256)
                   )"""
            )
            existing = dict(self._db.execute("SELECT key, value FROM metadata"))
            if existing:
                if (set(existing) != {"schema_version", "program"}
                        or existing.get("program") != program
                        or existing.get("schema_version") not in {"1", "2"}):
                    raise BehaviorError("Graph database belongs to another schema or program")
            else:
                self._db.executemany("INSERT INTO metadata VALUES (?, ?)",
                                     [("schema_version", "2"), ("program", program)])
            if existing.get("schema_version") == "1":
                self._migrate_entity_source_refs()
                self._db.execute("UPDATE metadata SET value='2' WHERE key='schema_version'")
            self._db.commit()
        except BaseException:
            if self._db is not None:
                self._db.rollback()
            self.close()
            raise

    def __enter__(self) -> "BehaviorGraph":
        self._connection()
        return self

    def __exit__(self, *_args: Any) -> None:
        self.close()

    def close(self) -> None:
        if self._db is not None:
            self._db.close()
            self._db = None

    def _connection(self) -> sqlite3.Connection:
        if self._db is None:
            raise BehaviorError("Graph is closed")
        return self._db

    def _migrate_entity_source_refs(self) -> None:
        """Move growing entity provenance arrays into a normalized table."""
        db = self._connection()
        rows = db.execute("SELECT id,payload FROM entities ORDER BY id").fetchall()
        for entity_id, payload in rows:
            record = json.loads(payload)
            has_refs = "source_refs" in record
            refs = record.pop("source_refs", [])
            if not isinstance(refs, list):
                raise BehaviorError("Legacy graph entity provenance is invalid")
            for ref in _sorted_refs(refs):
                encoded = canonical_json(ref)
                db.execute(
                    "INSERT OR IGNORE INTO entity_source_refs(entity_id,source_ref_sha256,payload) VALUES(?,?,?)",
                    (entity_id, digest_json(ref), encoded),
                )
            if has_refs:
                db.execute("UPDATE entities SET payload=? WHERE id=?", (canonical_json(record), entity_id))

    def _rows(self, table: str) -> list[dict]:
        if table == "entities":
            db = self._connection()
            refs_by_entity: dict[str, list[dict]] = {}
            for entity_id, payload in db.execute(
                "SELECT entity_id,payload FROM entity_source_refs "
                "ORDER BY entity_id,payload COLLATE BINARY"
            ):
                refs_by_entity.setdefault(entity_id, []).append(json.loads(payload))
            rows = []
            for entity_id, payload in db.execute("SELECT id,payload FROM entities ORDER BY id"):
                record = json.loads(payload)
                record["source_refs"] = refs_by_entity.get(entity_id, [])
                rows.append(record)
            return rows
        return [json.loads(row[0]) for row in self._connection().execute(
            "SELECT payload FROM " + table + " ORDER BY id")]

    def import_bundle(self, bundle: dict) -> dict:
        """Import one bundle using the same path as a one-item batch."""
        return self.import_bundles([bundle])

    def import_bundles(self, bundles: list[dict], *, include_snapshot: bool = True) -> dict:
        """Atomically import several compatible bundles and optionally snapshot.

        Bundles are normalized independently before merging. Entity provenance
        is unioned for identical entity content, while conflicting IDs in any
        other record type remain errors. The combined import has one idempotency
        key and one SQLite transaction. Callers that are importing many pages
        may skip the full snapshot digest and materialize a snapshot once when
        they actually need to query it.
        """
        if not isinstance(bundles, list) or not bundles:
            raise BehaviorError("At least one graph bundle is required")
        if type(include_snapshot) is not bool:
            raise BehaviorError("Snapshot option must be boolean")
        normalized_bundles = [self._normalize_bundle(bundle) for bundle in bundles]
        normalized = self._merge_normalized_bundles(normalized_bundles)
        return self._import_normalized_bundle(normalized, include_snapshot=include_snapshot)

    def _normalize_bundle(self, bundle: dict) -> dict:
        _public(bundle)
        _exact_dict(bundle, {"schema_version", "program", "sources", "entities", "assertions", "cards"}, "bundle")
        if type(bundle["schema_version"]) is not int or bundle["schema_version"] != 1 or bundle["program"] != self.program:
            raise BehaviorError("Bundle belongs to another schema or program")
        sources = [_source(ref) for ref in _list(bundle["sources"], "sources")]
        declared = {_source_pair(ref) for ref in sources}
        entities = [_entity(row, declared, self.program) for row in _list(bundle["entities"], "entities")]
        assertions = [_assertion(row, declared, self.program) for row in _list(bundle["assertions"], "assertions")]
        cards = [_card(row, declared, self.program) for row in _list(bundle["cards"], "cards")]
        normalized = {"schema_version": 1, "program": self.program,
                      "sources": _sorted_refs(sources)}
        for table, records in (("entities", entities), ("assertions", assertions), ("cards", cards)):
            unique = {}
            for record in records:
                if record["id"] in unique and unique[record["id"]] != record:
                    raise BehaviorError("Conflicting duplicate graph ID")
                unique[record["id"]] = record
            normalized[table] = [unique[key] for key in sorted(unique)]
        return normalized

    def _merge_normalized_bundles(self, bundles: list[dict]) -> dict:
        sources = []
        entities: dict[str, dict] = {}
        assertions: dict[str, dict] = {}
        cards: dict[str, dict] = {}
        for bundle in bundles:
            sources.extend(bundle["sources"])
            for record in bundle["entities"]:
                previous = entities.get(record["id"])
                if previous is None:
                    entities[record["id"]] = record
                    continue
                old_content = {key: value for key, value in previous.items() if key != "source_refs"}
                new_content = {key: value for key, value in record.items() if key != "source_refs"}
                if old_content != new_content:
                    raise BehaviorError("Entity identity collision; historical data cannot be overwritten")
                entities[record["id"]] = {
                    **previous,
                    "source_refs": _sorted_refs(previous["source_refs"] + record["source_refs"]),
                }
            for table, target in (("assertions", assertions), ("cards", cards)):
                for record in bundle[table]:
                    previous = target.get(record["id"])
                    if previous is not None and previous != record:
                        raise BehaviorError("Conflicting duplicate graph ID")
                    target[record["id"]] = record
        return {
            "schema_version": 1,
            "program": self.program,
            "sources": _sorted_refs(sources),
            "entities": [entities[key] for key in sorted(entities)],
            "assertions": [assertions[key] for key in sorted(assertions)],
            "cards": [cards[key] for key in sorted(cards)],
        }

    def _import_normalized_bundle(self, normalized: dict, *, include_snapshot: bool = True) -> dict:
        bundle_id = "bundle-" + digest_json(normalized)
        db = self._connection()
        inserted = {name: 0 for name in ("sources", "entities", "assertions", "cards")}
        updated_entities = 0
        replayed = False
        try:
            db.execute("BEGIN IMMEDIATE")
            if any(ref["integrity"] != "current" for ref in _integrity(normalized["sources"])):
                raise BehaviorError("Source changed or is unavailable before import")
            page_entities = {record["id"]: record for record in normalized["entities"]}
            page_entity_kinds = {identifier: record["kind"] for identifier, record in page_entities.items()}
            page_assertions = {record["id"] for record in normalized["assertions"]}
            referenced_ids = {
                record[key]
                for record in normalized["assertions"]
                for key in ("subject", "object")
            }
            referenced_ids.update(
                endpoint_id for card in normalized["cards"]
                if (endpoint_id := card["endpoint"].get("id")) is not None
            )
            external_ids = sorted(referenced_ids - set(page_entities) - page_assertions)
            stored_entities: dict[str, dict] = {}
            stored_assertions: set[str] = set()
            # Resolve only external IDs touched by this bounded page. Loading
            # and decoding every historical graph entity on each import made
            # incremental cost grow with the entire project history.
            for offset in range(0, len(external_ids), 400):
                batch = external_ids[offset:offset + 400]
                if not batch:
                    continue
                placeholders = ",".join("?" for _ in batch)
                stored_entities.update({
                    row[0]: json.loads(row[1])
                    for row in db.execute(
                        f"SELECT id,payload FROM entities WHERE id IN ({placeholders})", batch
                    )
                })
                stored_assertions.update(
                    row[0] for row in db.execute(
                        f"SELECT id FROM assertions WHERE id IN ({placeholders})", batch
                    )
                )
            known_entities = set(page_entities) | set(stored_entities)
            entity_kinds = {identifier: record["kind"] for identifier, record in stored_entities.items()}
            entity_kinds.update(page_entity_kinds)
            known_assertions = page_assertions | stored_assertions
            proposal_links = set()
            for record in normalized["assertions"]:
                for key in ("subject", "object"):
                    if record[key] not in known_entities and record[key] not in known_assertions:
                        raise BehaviorError("Dangling assertion reference")
                    if record[key] in known_assertions and record["predicate"] not in {"supports", "contradicts"}:
                        raise BehaviorError("Only evidence relations may reference assertions")
                subject_kind = entity_kinds.get(record["subject"])
                object_kind = entity_kinds.get(record["object"])
                if subject_kind == "hypothesis" or object_kind == "hypothesis":
                    hypothesis_id = record["subject"] if subject_kind == "hypothesis" else record["object"]
                    hypothesis = next((item for item in normalized["entities"]
                                       if item["id"] == hypothesis_id), stored_entities.get(hypothesis_id))
                    if (subject_kind != "hypothesis" or object_kind not in {
                            "observation", "endpoint", "parameter", "flow", "classifier_signal"}
                            or record["predicate"] not in {"supports", "contradicts"}
                            or record["origin_kind"] != "model_proposed"
                            or record["evidence_state"] != "proposed"
                            or record["context"].get("packet_sha256") != hypothesis["data"].get("packet_sha256")):
                        raise BehaviorError("Model hypothesis links must remain source-pinned proposals")
                    proposal_links.add(hypothesis_id)
            for record in normalized["entities"]:
                if record["kind"] == "hypothesis" and record["id"] not in proposal_links:
                    raise BehaviorError("Offline hypothesis requires an evidence link")
            for record in normalized["cards"]:
                endpoint_id = record["endpoint"].get("id")
                if endpoint_id is not None and endpoint_id not in known_entities:
                    raise BehaviorError("Dangling card endpoint reference")
            replayed = db.execute("SELECT 1 FROM imports WHERE id=?", (bundle_id,)).fetchone() is not None
            for table in ("sources", "entities", "assertions", "cards"):
                for record in normalized[table]:
                    if table == "entities":
                        identifier = record["id"]
                        base_record = {key: value for key, value in record.items() if key != "source_refs"}
                        previous = db.execute("SELECT payload FROM entities WHERE id=?", (identifier,)).fetchone()
                        if previous is None:
                            db.execute("INSERT INTO entities VALUES (?, ?)",
                                       (identifier, canonical_json(base_record)))
                            inserted["entities"] += 1
                        elif json.loads(previous[0]) != base_record:
                            raise BehaviorError("Entity identity collision; historical data cannot be overwritten")

                        added_refs = 0
                        for ref in record["source_refs"]:
                            encoded = canonical_json(ref)
                            result = db.execute(
                                "INSERT OR IGNORE INTO entity_source_refs(entity_id,source_ref_sha256,payload) "
                                "VALUES(?,?,?)",
                                (identifier, digest_json(ref), encoded),
                            )
                            added_refs += result.rowcount
                        if previous is not None and added_refs:
                            updated_entities += 1
                        continue

                    identifier = digest_json(record) if table == "sources" else record["id"]
                    previous = db.execute("SELECT payload FROM " + table + " WHERE id=?", (identifier,)).fetchone()
                    if previous is None:
                        db.execute("INSERT INTO " + table + " VALUES (?, ?)", (identifier, canonical_json(record)))
                        inserted[table] += 1
                    else:
                        existing = json.loads(previous[0])
                        if existing != record:
                            raise BehaviorError("Immutable graph record identity collision")
            db.execute("INSERT OR IGNORE INTO imports VALUES (?)", (bundle_id,))
            db.commit()
        except BaseException:
            db.rollback()
            raise
        return {"schema_version": 1, "program": self.program, "bundle_id": bundle_id,
                "replayed": replayed, "inserted": inserted, "updated_entities": updated_entities,
                "snapshot_id": self.snapshot()["snapshot_id"] if include_snapshot else None}

    def snapshot(self) -> dict:
        db = self._connection()
        # A single read transaction prevents another coordinator import from
        # exposing a mixture of old and new table contents.
        try:
            db.execute("BEGIN")
            raw = {name: self._rows(name) for name in ("sources", "entities", "assertions", "cards")}
            db.commit()
        except BaseException:
            db.rollback()
            raise
        sources = sorted(raw["sources"], key=canonical_json)
        integrity = _integrity(sources)
        state_by_pair = {_source_pair(ref): ref["integrity"] for ref in integrity}
        result = {"schema_version": 1, "program": self.program, "sources": sources,
                  "entities": raw["entities"], "assertions": raw["assertions"], "cards": raw["cards"],
                  "counts": {name: len(raw[name]) for name in raw}, "source_integrity": integrity,
                  "freshness": {name: {row["id"]: _record_freshness(row, state_by_pair)
                                        for row in raw[name]}
                                for name in ("entities", "assertions", "cards")},
                  "limitations": ["Source freshness describes local file integrity, not current server behavior.",
                                  "Record locators anchor provenance; their semantic applicability still needs review.",
                                  "Graph evidence states do not verify findings or authorize actions."]}
        result["snapshot_id"] = "snapshot-" + digest_json(result)
        return result

    def get_slice(self, seed_ids: list[str], *, max_nodes: int = 80, max_edges: int = 160,
                  max_depth: int = 3, context: dict | None = None, snapshot_id: str | None = None) -> dict:
        for value, minimum, maximum in ((max_nodes, 1, 10000), (max_edges, 0, 20000), (max_depth, 0, 100)):
            if type(value) is not int or not minimum <= value <= maximum:
                raise BehaviorError("Invalid slice budget")
        if not isinstance(seed_ids, (list, tuple)) or not seed_ids or any(not isinstance(seed, str) for seed in seed_ids):
            raise BehaviorError("Slice needs explicit graph seeds")
        seeds = sorted(set(seed_ids))
        if len(seeds) > max_nodes:
            raise BehaviorError("Seed count exceeds node budget")
        effective_context = _context(context, self.program) if context is not None else {}
        _public(effective_context)
        snapshot = self.snapshot()
        if snapshot_id is not None and snapshot_id != snapshot["snapshot_id"]:
            raise BehaviorError("Foreign or stale snapshot ID")
        entities = {row["id"]: row for row in snapshot["entities"]}
        assertions = {row["id"]: row for row in snapshot["assertions"]}
        nodes = {**entities, **{key: {**row, "kind": "assertion"} for key, row in assertions.items()}}
        if any(seed not in nodes for seed in seeds):
            raise BehaviorError("Missing slice seed")
        for seed in seeds:
            candidate = _record_context(nodes[seed])
            if not _compatible(candidate, effective_context):
                raise BehaviorError("Slice seeds have incompatible contexts")
            _bind(candidate, effective_context)
        selected_nodes = set(seeds)
        selected_edges: set[str] = set()
        depth = {seed: 0 for seed in seeds}
        omissions: dict[str, dict] = {}
        frontier = list(seeds)
        adjacency: dict[str, list[str]] = {}
        for identifier, assertion in assertions.items():
            for endpoint in (assertion["subject"], assertion["object"]):
                adjacency.setdefault(endpoint, []).append(identifier)
        # Conflicting evidence is considered before ordinary branches at each
        # frontier. All omitted contradictions are still explicitly reported.
        def priority(identifier: str) -> tuple:
            row = assertions[identifier]
            return (0 if row["predicate"] == "contradicts" or row["evidence_state"] == "contested" else
                    1 if row["predicate"] == "supports" else 2, identifier)

        while frontier:
            current = frontier.pop(0)
            incident = set(adjacency.get(current, []))
            if current in assertions:
                incident.add(current)
            for identifier in sorted(incident, key=priority):
                if identifier in selected_edges:
                    continue
                edge = assertions[identifier]
                candidates = (edge["context"], _record_context(nodes[edge["subject"]]),
                              _record_context(nodes[edge["object"]]))
                trial = dict(effective_context)
                compatible = True
                for candidate in candidates:
                    if not _compatible(candidate, trial):
                        compatible = False
                        break
                    _bind(candidate, trial)
                new_nodes = {edge["subject"], edge["object"]} - selected_nodes
                reason = None
                if not compatible:
                    reason = "context_incompatible"
                elif depth[current] >= max_depth:
                    reason = "max_depth"
                elif len(selected_edges) >= max_edges:
                    reason = "max_edges"
                elif len(selected_nodes) + len(new_nodes) > max_nodes:
                    reason = "max_nodes"
                if reason:
                    omissions[identifier] = {"assertion_id": identifier, "reason": reason,
                                             "mandatory_contradiction": edge["predicate"] == "contradicts"
                                             or edge["evidence_state"] == "contested"}
                    continue
                selected_edges.add(identifier)
                effective_context = trial
                omissions.pop(identifier, None)
                for endpoint in sorted(new_nodes):
                    selected_nodes.add(endpoint)
                    depth[endpoint] = depth[current] + 1
                    frontier.append(endpoint)
                # Assertions are evidence targets, not entities. A selected
                # relation still exposes its support/contradiction adjacency.
                if identifier not in depth:
                    depth[identifier] = depth[current]
                    frontier.append(identifier)

        # A budget may omit a base relationship before its assertion ID enters
        # the frontier. Evidence contradicting that relationship is still a
        # mandatory omission, rather than disappearing with the whole branch.
        evidence_frontier = selected_nodes | selected_edges | set(omissions)
        pending = True
        while pending:
            pending = False
            for identifier, edge in assertions.items():
                mandatory = edge["predicate"] == "contradicts" or edge["evidence_state"] == "contested"
                if (not mandatory or identifier in selected_edges or identifier in omissions
                        or not {edge["subject"], edge["object"]}.intersection(evidence_frontier)):
                    continue
                trial = dict(effective_context)
                compatible = True
                for candidate in (edge["context"], _record_context(nodes[edge["subject"]]),
                                  _record_context(nodes[edge["object"]])):
                    if not _compatible(candidate, trial):
                        compatible = False
                        break
                    _bind(candidate, trial)
                reason = "context_incompatible" if not compatible else "mandatory_evidence_branch_omitted"
                omissions[identifier] = {"assertion_id": identifier, "reason": reason,
                                         "mandatory_contradiction": True}
                evidence_frontier.add(identifier)
                pending = True

        selected_assertions = selected_edges | (selected_nodes & set(assertions))
        chosen_entities = [entities[key] for key in sorted(selected_nodes & set(entities))]
        chosen_edges = [assertions[key] for key in sorted(selected_edges)]
        assertion_nodes = [assertions[key] for key in sorted(selected_nodes & set(assertions))]
        observation_sources = {(entity["data"].get("capture_id"), _source_pair(ref))
                               for entity in chosen_entities if entity["kind"] == "observation"
                               for ref in entity["source_refs"]}
        selected_cards = []
        omitted_cards = []
        for card in snapshot["cards"]:
            endpoint_id = card["endpoint"].get("id")
            associated = (any((card["capture_id"], _source_pair(ref)) in observation_sources
                              for ref in card["source_refs"]) or endpoint_id in selected_nodes)
            if not associated:
                continue
            if not _compatible(card["context"], effective_context):
                omitted_cards.append({"card_id": card["id"], "reason": "context_incompatible"})
            elif len(selected_cards) >= max_nodes:
                omitted_cards.append({"card_id": card["id"], "reason": "max_cards"})
            else:
                selected_cards.append(card)
                _bind(card["context"], effective_context)
        used_pairs = {_source_pair(ref) for record in chosen_entities + chosen_edges + assertion_nodes + selected_cards
                      for ref in record["source_refs"]}
        source_integrity = [ref for ref in snapshot["source_integrity"] if _source_pair(ref) in used_pairs]
        omissions_list = [omissions[key] for key in sorted(omissions)]
        truncation = (any(row["reason"] in {"max_nodes", "max_edges", "max_depth", "mandatory_evidence_branch_omitted"}
                          for row in omissions_list) or any(row["reason"] == "max_cards" for row in omitted_cards))
        missing_evidence = sorted({item for card in selected_cards for item in card["missing_evidence"]
                                   if isinstance(item, str)})
        unknown_conditions = [row["id"] for row in chosen_entities
                              if row["kind"] == "condition" and row["data"].get("value") in (None, "unknown")]
        incomplete = bool(omissions_list or omitted_cards or missing_evidence or unknown_conditions
                          or any(card["analysis_state"] == "incomplete" for card in selected_cards)
                          or any(ref["integrity"] != "current" for ref in source_integrity))
        result = {"schema_version": 1, "program": self.program, "snapshot_id": snapshot["snapshot_id"],
                  "seed_ids": seeds, "context": effective_context, "entities": chosen_entities,
                  "assertions": chosen_edges, "cards": selected_cards,
                  "assertion_nodes": assertion_nodes,
                  "supporting_evidence": [row["id"] for row in chosen_edges if row["predicate"] == "supports"],
                  "conflicting_evidence": [row["id"] for row in chosen_edges
                                           if row["predicate"] == "contradicts" or row["evidence_state"] == "contested"],
                  "sources": [ref for ref in snapshot["sources"] if _source_pair(ref) in used_pairs],
                  "source_integrity": source_integrity,
                  "freshness": {name: {record["id"]: snapshot["freshness"][name][record["id"]]
                                        for record in records}
                                for name, records in (("entities", chosen_entities),
                                                       ("assertions", [assertions[key] for key in sorted(selected_assertions)]),
                                                       ("cards", selected_cards))},
                  "counts": {"nodes": len(selected_nodes), "entities": len(chosen_entities),
                             "edges": len(chosen_edges), "cards": len(selected_cards)},
                  "budgets": {"max_nodes": max_nodes, "max_edges": max_edges, "max_depth": max_depth,
                              "max_cards": max_nodes},
                  "truncated": truncation, "incomplete": incomplete,
                  "mandatory_contradiction_omitted": any(row["mandatory_contradiction"] for row in omissions_list),
                  "missing_branches": omissions_list, "omitted_cards": omitted_cards,
                  "missing_evidence": missing_evidence, "unknown_conditions": unknown_conditions,
                  "limitations": snapshot["limitations"] +
                                  ["Slice coverage is limited to the imported inventory; reachability is not data flow.",
                                   "Unknown context remains unknown; excluded contexts require separate slices."]}
        result["slice_id"] = "slice-" + digest_json(result)
        return result
