"""Explicitly project-scoped, bounded graph slices with capture provenance."""
from __future__ import annotations

import json
import re
from typing import Any, Sequence

from .behavior import BehaviorGraph
from .storage import ProjectStore


def search_captures(
    stores: Sequence[ProjectStore],
    *,
    text: str | None = None,
    method: str | None = None,
    host: str | None = None,
    route: str | None = None,
    status: int | None = None,
    field_name: str | None = None,
    request_signature: str | None = None,
    response_signature: str | None = None,
    since: str | None = None,
    until: str | None = None,
    limit: int = 100,
) -> dict[str, Any]:
    """Search structural metadata across only the explicitly selected stores."""
    if not stores:
        raise ValueError("Select at least one Caido project before searching")
    keys = [store.project_key for store in stores]
    if len(set(keys)) != len(keys):
        raise ValueError("A Caido project may appear only once in the selection")
    if not (1 <= limit <= 500):
        raise ValueError("limit must be between 1 and 500")
    if text is not None and len(text) > 4096:
        raise ValueError("Search text exceeds the 4096-character limit")
    if status is not None and not (100 <= status <= 599):
        raise ValueError("status must be an HTTP status code")
    for label, signature in (("request_signature", request_signature), ("response_signature", response_signature)):
        if signature is not None and re.fullmatch(r"[a-f0-9]{64}", signature) is None:
            raise ValueError(f"{label} must be a keyed 64-character digest")
    terms = re.findall(r"[\w-]{2,64}", text or "", flags=re.UNICODE)[:32]
    match_query = " OR ".join('"' + term.replace('"', '""') + '"*' for term in terms)
    hits: list[dict[str, Any]] = []
    truncated = False
    for store in stores:
        sql = """SELECT x.observation_id,x.request_id,x.captured_at,x.request_sha256,x.response_sha256,x.parse_state,
                        x.current_revision_id,
                        cr.request_truncated,cr.response_truncated,
                        cr.request_wire_fidelity,cr.response_wire_fidelity,
                        s.method,s.host,s.port,s.route,s.response_status,s.request_content_type,s.response_content_type,
                        s.field_names_json,s.header_names_json,m.graph_observation_id
                 FROM capture_search s JOIN captures x ON x.observation_id=s.observation_id
                 LEFT JOIN capture_revisions cr ON cr.revision_id=x.current_revision_id
                 LEFT JOIN graph_capture_map m ON m.observation_id=x.observation_id
                       AND m.revision_id=x.current_revision_id"""
        params: list[object] = []
        if match_query:
            sql += " JOIN capture_search_fts f ON f.observation_id=s.observation_id"
        clauses = []
        if request_signature is not None or response_signature is not None:
            # Historical imports have unkeyed message hashes; do not compare
            # those with the project-keyed live graph signatures.
            clauses.append("0")
        if match_query:
            clauses.append("capture_search_fts MATCH ?")
            params.append(match_query)
        if method:
            clauses.append("s.method=?")
            params.append(method.upper())
        if host:
            clauses.append("instr(lower(s.host),lower(?))>0")
            params.append(host)
        if route:
            clauses.append("instr(lower(s.route),lower(?))>0")
            params.append(route.lower())
        if status is not None:
            clauses.append("s.response_status=?")
            params.append(status)
        if field_name:
            clauses.append("instr(lower(s.field_names_json),lower(?))>0")
            params.append(field_name.lower())
        if since:
            clauses.append("x.captured_at>=?")
            params.append(since)
        if until:
            clauses.append("x.captured_at<=?")
            params.append(until)
        if clauses:
            sql += " WHERE " + " AND ".join(clauses)
        sql += " ORDER BY x.captured_at DESC,x.request_id LIMIT ?"
        params.append(limit + 1)
        with store.connect() as connection:
            rows = connection.execute(sql, params).fetchall()
        if len(rows) > limit:
            truncated = True
            rows = rows[:limit]
        for row in rows:
            hits.append({
                "project_key": store.project_key,
                "observation_id": row["observation_id"],
                "graph_seed_id": row["graph_observation_id"],
                "request_id": row["request_id"],
                "captured_at": row["captured_at"],
                "method": row["method"], "host": row["host"], "port": row["port"],
                "route": row["route"], "status": row["response_status"],
                "request_content_type": row["request_content_type"],
                "response_content_type": row["response_content_type"],
                "field_names": json.loads(row["field_names_json"]),
                "header_names": json.loads(row["header_names_json"]),
                "request_sha256": row["request_sha256"], "response_sha256": row["response_sha256"],
                "capture_revision": row["current_revision_id"],
                "parse_state": row["parse_state"],
                "request_truncated": None if row["request_truncated"] is None else bool(row["request_truncated"]),
                "response_truncated": None if row["response_truncated"] is None else bool(row["response_truncated"]),
                "request_wire_fidelity": row["request_wire_fidelity"] or "unknown",
                "response_wire_fidelity": row["response_wire_fidelity"] or "unknown",
                "record_source": "encrypted_local_capture",
                "raw_stored_locally": True,
            })

        # Live-tail records are structural graph indexes only; full messages
        # remain in Caido and are opened by exact request ID when needed.
        live_sql = "SELECT l.* FROM live_capture_index l"
        live_params: list[object] = []
        live_clauses = []
        if request_signature is not None:
            live_clauses.append("l.request_signature=?")
            live_params.append(request_signature)
        if response_signature is not None:
            live_clauses.append("l.response_signature=?")
            live_params.append(response_signature)
        if match_query:
            live_sql += " JOIN live_capture_fts f ON f.observation_id=l.observation_id"
            live_clauses.append("live_capture_fts MATCH ?")
            live_params.append(match_query)
        for clause, value in (
            ("l.method=?", method.upper() if method else None),
            ("instr(lower(l.host),lower(?))>0", host),
            ("instr(lower(l.route),lower(?))>0", route.lower() if route else None),
            ("l.response_status=?", status),
            ("instr(lower(l.field_names_json),lower(?))>0", field_name.lower() if field_name else None),
            ("l.synced_at>=?", since),
            ("l.synced_at<=?", until),
        ):
            if value is not None:
                live_clauses.append(clause)
                live_params.append(value)
        if live_clauses:
            live_sql += " WHERE " + " AND ".join(live_clauses)
        live_sql += " ORDER BY l.synced_at DESC,l.request_id LIMIT ?"
        live_params.append(limit + 1)
        with store.connect() as connection:
            live_rows = connection.execute(live_sql, live_params).fetchall()
        if len(live_rows) > limit:
            truncated = True
            live_rows = live_rows[:limit]
        for row in live_rows:
            hits.append({
                "project_key": store.project_key,
                "observation_id": row["observation_id"],
                "graph_seed_id": row["graph_observation_id"],
                "request_id": row["request_id"],
                "captured_at": None,
                "synced_at": row["synced_at"],
                "method": row["method"], "host": row["host"], "port": row["port"],
                "route": row["route"], "status": row["response_status"],
                "request_content_type": row["request_content_type"],
                "response_content_type": row["response_content_type"],
                "field_names": json.loads(row["field_names_json"]),
                "header_names": json.loads(row["header_names_json"]),
                "request_signature": row["request_signature"],
                "response_signature": row["response_signature"],
                "response_size": row["response_size"],
                "parse_state": row["parse_state"],
                "record_source": "caido_live_tail",
                "caido_request_reference": {
                    "instance_id": store.project.instance_id,
                    "project_id": store.project.project_id,
                    "request_id": row["request_id"],
                },
                "raw_stored_locally": False,
            })
    hits.sort(
        key=lambda item: (item.get("captured_at") or item.get("synced_at") or "", item["request_id"]),
        reverse=True,
    )
    deduplicated: dict[tuple[str, str], dict[str, Any]] = {}
    for hit in hits:
        deduplicated.setdefault((hit["project_key"], hit["request_id"]), hit)
    hits = list(deduplicated.values())
    if len(hits) > limit:
        truncated = True
        hits = hits[:limit]
    return {"hits": hits, "truncated": truncated, "selected_project_keys": keys}


def read_raw_capture(store: ProjectStore, request_id: str, side: str, *, key: bytes | None = None) -> bytes | None:
    """Read a raw request/response only by exact ID inside one selected project."""
    if side not in {"request", "response"}:
        raise ValueError("side must be request or response")
    with store.connect() as connection:
        row = connection.execute(
            f"SELECT observation_id,{side}_sha256 FROM captures WHERE request_id=?",
            (request_id,),
        ).fetchone()
    if row is None:
        raise KeyError("Caido request ID was not found in the selected project")
    digest = row[f"{side}_sha256"]
    if digest is None:
        return None
    return store.read_blob(row["observation_id"], side, digest, key=key)


def graph_slice(
    store: ProjectStore,
    seed_ids: Sequence[str],
    *,
    max_nodes: int = 80,
    max_edges: int = 160,
    max_depth: int = 3,
) -> dict[str, Any]:
    if not seed_ids or len(seed_ids) > max_nodes:
        raise ValueError("Provide between one and max_nodes seed IDs")
    if not (1 <= max_nodes <= 500 and 0 <= max_edges <= 2000 and 0 <= max_depth <= 12):
        raise ValueError("Invalid graph slice budget")
    with BehaviorGraph(store.behavior_graph_path, store.project_key, read_only=store.read_only) as graph:
        result = graph.get_slice(
            list(seed_ids),
            max_nodes=max_nodes,
            max_edges=max_edges,
            max_depth=max_depth,
        )
    graph_observation_ids = sorted({
        row["id"] for row in result["entities"] if row.get("kind") == "observation"
    })
    capture_provenance: list[dict[str, Any]] = []
    for offset in range(0, len(graph_observation_ids), 400):
        batch = graph_observation_ids[offset:offset + 400]
        placeholders = ",".join("?" for _ in batch)
        with store.connect() as connection:
            rows = connection.execute(
                    f"""SELECT gr.graph_observation_id,cr.revision_id,c.observation_id,
                           c.request_id,cr.request_sha256,cr.response_sha256,
                           cr.request_size,cr.response_size,cr.first_seen_at,
                           cr.request_truncated,cr.response_truncated,
                           cr.request_wire_fidelity,cr.response_wire_fidelity
                    FROM graph_capture_revisions gr
                    JOIN capture_revisions cr ON cr.revision_id=gr.revision_id
                    JOIN captures c ON c.observation_id=cr.observation_id
                    WHERE gr.graph_observation_id IN ({placeholders})
                    ORDER BY cr.first_seen_at,cr.revision_id""",
                batch,
            ).fetchall()
        capture_provenance.extend(dict(row) for row in rows)
        with store.connect() as connection:
            live_rows = connection.execute(
                f"""SELECT observation_id,request_id,graph_observation_id,synced_at,
                           request_signature,response_signature,request_size,response_size,
                           parse_state,caido_instance_id,caido_project_id
                    FROM live_capture_index WHERE graph_observation_id IN ({placeholders})
                    ORDER BY synced_at,request_id""",
                batch,
            ).fetchall()
        capture_provenance.extend({
            **dict(row),
            "record_source": "caido_live_tail",
            "raw_stored_locally": False,
        } for row in live_rows)
    result["project_key"] = store.project_key
    result["capture_provenance"] = capture_provenance
    return result
