"""Synthetic end-to-end contract checks for the local read-only MCP facade."""
from __future__ import annotations

import base64
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from bb_assistant.caido import HistoryPage
from bb_assistant.domain import CaidoExchangeRef, CaidoProjectRef, HttpExchange
from bb_assistant.ingest import backfill_all_projects, sync_project
from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.index import KBIndex
from bb_assistant.storage import ProjectStore
from bb_assistant.tools import ReadOnlyAssistantTools, ToolCallError


class FixtureCaidoSource:
    def __init__(self, projects: tuple[CaidoProjectRef, ...], exchanges: tuple[HttpExchange, ...]):
        self._projects = projects
        self._exchanges = {exchange.ref.project.storage_key: exchange for exchange in exchanges}
        self.list_projects_calls = 0
        self.history_project_keys: list[str] = []

    def list_projects(self) -> tuple[CaidoProjectRef, ...]:
        self.list_projects_calls += 1
        return self._projects

    def list_history(
        self,
        project: CaidoProjectRef,
        *,
        cursor: str | None = None,
        page_size: int = 100,
    ) -> HistoryPage:
        if cursor is not None or page_size < 1:
            raise AssertionError("fixture has exactly one terminal history page")
        self.history_project_keys.append(project.storage_key)
        return HistoryPage((self._exchanges[project.storage_key],), None, False)

    def get_exchange(self, ref: CaidoExchangeRef) -> HttpExchange | None:
        exchange = self._exchanges.get(ref.project.storage_key)
        return exchange if exchange is not None and exchange.ref.request_id == ref.request_id else None


def fixture_exchange(project: CaidoProjectRef, request_id: str, marker: str) -> HttpExchange:
    target = f"/api/products/123?category=hardware&api_key={marker}"
    request = (
        f"GET {target} HTTP/1.1\r\n"
        f"Host: {project.project_id}.fixture.test\r\n"
        f"Authorization: Bearer {marker}\r\n"
        "Accept: application/json\r\n\r\n"
    ).encode()
    response = (
        "HTTP/1.1 200 OK\r\n"
        "Content-Type: application/json\r\n"
        "\r\n"
        f'{{"id":"123","access_token":"{marker}","owner_id":"actor-1"}}'
    ).encode()
    return HttpExchange(
        ref=CaidoExchangeRef(project, request_id),
        captured_at="2026-10-21T12:00:00Z",
        request_raw=request,
        response_raw=response,
        method="GET",
        scheme="https",
        host=f"{project.project_id}.fixture.test",
        port=443,
        target=target,
        response_status=200,
        request_truncated=False,
        response_truncated=False,
        request_wire_fidelity="source_bytes",
        response_wire_fidelity="source_bytes",
    )


class ReadOnlyMCPFacadeContractTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="bb-mcp-contract-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.vault_key = b"synthetic-vault-key-32-bytes-long!"[:32]
        self.env = patch.dict(
            os.environ,
            {"BB_ASSISTANT_VAULT_KEY": base64.urlsafe_b64encode(self.vault_key).decode("ascii")},
        )
        self.env.start()
        self.addCleanup(self.env.stop)

        self.projects = (
            CaidoProjectRef("fixture-instance", "project-a", "Fixture A"),
            CaidoProjectRef("fixture-instance", "project-b", "Fixture B"),
        )
        self.markers = ("SyntheticSecret-A9q3", "SyntheticSecret-B4z8")
        self.exchanges = tuple(
            fixture_exchange(project, f"request-{index}", marker)
            for index, (project, marker) in enumerate(zip(self.projects, self.markers, strict=True), 1)
        )
        self.stores = {
            project.storage_key: ProjectStore(project, root=self.root)
            for project in self.projects
        }
        source = FixtureCaidoSource(self.projects, self.exchanges)
        self.sync_receipts = tuple(
            sync_project(source, project, store=self.stores[project.storage_key], page_size=10)
            for project in self.projects
        )

        self.kb = KBIndex(self.root / "kb" / "index.sqlite3")
        document = SourceDocument.create(
            source_name="fixture_kb",
            source_id="authz/object-boundary",
            title="Synthetic object authorization note",
            text=(
                "Object authorization must be enforced for each actor and object. "
                "Compare responses using two isolated test actors and preserve the "
                "request IDs, response evidence, and alternative explanations."
            ),
            source_url="https://kb.invalid/fixture",
            source_locator="fixture.md#object-authorization",
            source_revision="fixture-1",
            license_id="CC0-1.0",
            attribution="Synthetic fixture",
        )
        report = {
            "coverage_state": "PARTIAL",
            "source_count": 1,
            "blocked_source_count": 0,
            "partial_source_count": 1,
            "sources": [{"source_name": "fixture_kb", "state": "partial"}],
        }
        report_hash = hashlib.sha256(
            json.dumps(report, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8") + b"\n"
        ).hexdigest()
        self.kb.replace_snapshot(
            chunk_document(document),
            "a" * 64,
            sources=[document],
            source_coverage_state="PARTIAL",
            source_coverage_report_sha256=report_hash,
            source_coverage_report=report,
        )
        readonly_stores = {
            project.storage_key: ProjectStore(project, root=self.root, read_only=True)
            for project in self.projects
        }
        self.tools = ReadOnlyAssistantTools(readonly_stores, KBIndex(self.kb.path, read_only=True))

    def test_import_graph_search_and_kb_citations_stay_project_scoped_and_redacted(self) -> None:
        self.assertEqual([receipt.state for receipt in self.sync_receipts], ["done", "done"])
        projects = self.tools.call("bb_list_projects", {})
        self.assertEqual(projects["project_count"], 2)
        self.assertTrue(all(row["coverage_reconciled"] is False for row in projects["projects"]))
        self.assertTrue(all(row["import_state"] == "terminal_cursor_unreconciled" for row in projects["projects"]))

        selected = self.projects[0]
        search = self.tools.call(
            "bb_search_captures",
            {"project_keys": [selected.storage_key], "method": "GET", "host": "project-a.fixture.test"},
        )
        self.assertEqual(search["selected_project_keys"], [selected.storage_key])
        self.assertEqual([hit["request_id"] for hit in search["hits"]], ["request-1"])
        self.assertEqual(len(search["hits"]), 1)
        seed = search["hits"][0]["graph_seed_id"]
        self.assertIsInstance(seed, str)

        capture = self.tools.call(
            "bb_get_capture",
            {"project_key": selected.storage_key, "request_id": "request-1"},
        )
        self.assertFalse(capture["raw_values_returned"])
        self.assertEqual(capture["graph_observation_id"], seed)
        self.assertEqual(capture["request_wire_fidelity"], "source_bytes")

        graph = self.tools.call(
            "bb_graph_slice",
            {"project_key": selected.storage_key, "seed_ids": [seed], "max_nodes": 50, "max_edges": 100, "max_depth": 4},
        )
        self.assertTrue(any(entity["kind"] == "observation" for entity in graph["entities"]))
        self.assertEqual([row["request_id"] for row in graph["capture_provenance"]], ["request-1"])
        self.assertEqual(graph["project_key"], selected.storage_key)

        kb_stats = self.tools.call("bb_kb_stats", {})
        self.assertEqual(kb_stats["coverage_state"], "PARTIAL")
        kb_search = self.tools.call("bb_kb_search", {"query": "object authorization actor", "top_k": 2})
        self.assertEqual(kb_search["coverage_state"], "PARTIAL")
        self.assertEqual(len(kb_search["hits"]), 1)
        hit = kb_search["hits"][0]
        self.assertEqual(hit["source_name"], "fixture_kb")
        self.assertRegex(hit["source_sha256"], r"^[a-f0-9]{64}$")
        self.assertRegex(hit["chunk_sha256"], r"^[a-f0-9]{64}$")
        self.assertLess(hit["span"]["start_char"], hit["span"]["end_char"])
        source = self.tools.call(
            "bb_kb_get_source",
            {"source_name": "fixture_kb", "source_id": "authz/object-boundary"},
        )
        self.assertTrue(source["reference_material_is_untrusted"])

        visible_result = json.dumps([projects, search, capture, graph, kb_stats, kb_search, source])
        for marker in self.markers:
            self.assertNotIn(marker, visible_result)
        self.assertNotIn("request-2", visible_result)

    def test_unknown_projects_tools_and_out_of_bounds_queries_are_rejected(self) -> None:
        with self.assertRaises(ToolCallError):
            self.tools.call("bb_search_captures", {"project_keys": ["b" * 64]})
        with self.assertRaises(ToolCallError):
            self.tools.call("bb_get_capture", {"project_key": self.projects[0].storage_key, "request_id": "request-2"})
        with self.assertRaises(ToolCallError):
            self.tools.call("bb_graph_slice", {
                "project_key": self.projects[0].storage_key,
                "seed_ids": ["observation-" + "0" * 64],
                "max_depth": 7,
            })
        with self.assertRaises(ToolCallError):
            self.tools.call("caido_send_request", {})

    def test_raw_tool_is_not_advertised_or_callable_by_default(self) -> None:
        names = {spec["name"] for spec in self.tools.list_tools()}
        self.assertNotIn("bb_get_capture_raw", names)
        self.assertTrue(all(name.startswith("bb_") for name in names))
        self.assertTrue(all(spec["input_schema"].get("additionalProperties") is False for spec in self.tools.list_tools()))
        with self.assertRaises(ToolCallError):
            self.tools.call("bb_get_capture_raw", {
                "project_key": self.projects[0].storage_key,
                "request_id": "request-1",
                "side": "request",
            })

    def test_project_and_kb_database_contexts_close_connections(self) -> None:
        with self.stores[self.projects[0].storage_key].connect() as project_connection:
            project_connection.execute("SELECT 1").fetchone()
        with self.assertRaises(sqlite3.ProgrammingError):
            project_connection.execute("SELECT 1")

        with self.kb.connect() as kb_connection:
            kb_connection.execute("SELECT 1").fetchone()
        with self.assertRaises(sqlite3.ProgrammingError):
            kb_connection.execute("SELECT 1")

    def test_stale_graph_seed_is_hidden_after_capture_revision_changes(self) -> None:
        selected = self.projects[0]
        initial = self.tools.call(
            "bb_search_captures",
            {"project_keys": [selected.storage_key], "method": "GET"},
        )["hits"][0]
        self.assertIsNotNone(initial["graph_seed_id"])

        old = self.exchanges[0]
        updated = HttpExchange(
            ref=old.ref,
            captured_at=old.captured_at,
            request_raw=old.request_raw,
            response_raw=old.response_raw + b" ",
            method=old.method,
            scheme=old.scheme,
            host=old.host,
            port=old.port,
            target=old.target,
            response_status=old.response_status,
            request_truncated=False,
            response_truncated=False,
            request_wire_fidelity="source_bytes",
            response_wire_fidelity="source_bytes",
        )
        self.stores[selected.storage_key].save_exchange(updated, key=self.vault_key)

        current = self.tools.call(
            "bb_search_captures",
            {"project_keys": [selected.storage_key], "method": "GET"},
        )["hits"][0]
        capture = self.tools.call(
            "bb_get_capture",
            {"project_key": selected.storage_key, "request_id": "request-1"},
        )
        self.assertNotEqual(current["capture_revision"], initial["capture_revision"])
        self.assertIsNone(current["graph_seed_id"])
        self.assertIsNone(capture["graph_observation_id"])

    def test_all_project_backfill_is_sequential_and_keeps_namespaces_separate(self) -> None:
        runtime_root = self.root / "all-project-import"
        source = FixtureCaidoSource(self.projects, self.exchanges)
        with patch.dict(os.environ, {"BB_ASSISTANT_DATA_HOME": str(runtime_root)}):
            receipts = backfill_all_projects(source, page_size=10)
            stores = [ProjectStore(project, root=runtime_root) for project in self.projects]

        self.assertEqual([receipt.state for receipt in receipts], ["done", "done"])
        self.assertEqual([receipt.distinct_request_ids for receipt in receipts], [1, 1])
        self.assertEqual(source.history_project_keys, [project.storage_key for project in self.projects])
        self.assertEqual(source.list_projects_calls, 2)  # Inventory checked before and after the run.
        self.assertNotEqual(stores[0].db_path, stores[1].db_path)
        for store in stores:
            with store.connect() as connection:
                self.assertEqual(connection.execute("SELECT COUNT(*) FROM captures").fetchone()[0], 1)

    def test_partial_project_sync_resumes_at_last_committed_cursor(self) -> None:
        project = self.projects[0]
        first = fixture_exchange(project, "resume-request-1", "ResumeSecret-A1")
        second = fixture_exchange(project, "resume-request-2", "ResumeSecret-B2")
        store = ProjectStore(project, root=self.root / "resume")

        class InterruptedSource:
            def __init__(self) -> None:
                self.fail_second_page = True
                self.cursors: list[str | None] = []

            def list_history(self, selected_project, *, cursor=None, page_size=100):
                if not selected_project.same_project(project) or page_size != 10:
                    raise AssertionError("unexpected project or page size")
                self.cursors.append(cursor)
                if cursor is None:
                    return HistoryPage((first,), "fixture-cursor-1", True)
                if cursor != "fixture-cursor-1":
                    raise AssertionError("unexpected resume cursor")
                if self.fail_second_page:
                    raise RuntimeError("synthetic page failure")
                return HistoryPage((second,), None, False)

        source = InterruptedSource()
        with self.assertRaisesRegex(RuntimeError, "synthetic page failure"):
            sync_project(source, project, page_size=10, store=store)
        partial = store.sync_progress("caido-http-history-v1")
        self.assertEqual(partial["state"], "partial")
        self.assertEqual(partial["cursor"], "fixture-cursor-1")
        first_run_id = partial["run_id"]

        source.fail_second_page = False
        receipt = sync_project(source, project, page_size=10, store=store)
        self.assertEqual(receipt.state, "done")
        self.assertEqual(receipt.run_id, first_run_id)
        self.assertEqual(receipt.pages_completed, 2)
        self.assertEqual(receipt.distinct_request_ids, 2)
        self.assertEqual(source.cursors, [None, "fixture-cursor-1", "fixture-cursor-1"])


if __name__ == "__main__":
    unittest.main()
