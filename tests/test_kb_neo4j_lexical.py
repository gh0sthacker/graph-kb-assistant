from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from bb_assistant.kb.canonical import CanonicalChunk, SourceDocument, chunk_document
from bb_assistant.kb.index import KBIndex
from bb_assistant.kb.neo4j_lexical import (
    Neo4jLexicalProvider, _driver_config_from_env, generation_names, publish_generation,
    sanitize_redamon_query,
)


SNAPSHOT = "a" * 64
READY = {"snapshot_id": SNAPSHOT, "state": "ready", "schema_version": 1,
         "core_count": 1, "total_count": 1, "core_sha256": "b" * 64, "total_sha256": "b" * 64}


class Result(list):
    def single(self):
        return self[0] if self else None

    def consume(self):
        return None


class Driver:
    def __init__(self, *, marker=None, hits=(), corrupt_readback=False):
        self.marker = marker
        self.hits = hits
        self.calls = []
        self.rows = []
        self.corrupt_readback = corrupt_readback

    def session(self, **kwargs):
        return self

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return None

    def run(self, cypher, **params):
        self.calls.append((cypher, params))
        if "RETURN g.state AS state" in cypher:
            return Result([self.marker] if self.marker else [])
        if "RETURN properties(g) AS generation" in cypher:
            return Result([{"generation": self.marker}] if self.marker else [])
        if "fulltext.queryNodes" in cypher:
            return Result(self.hits)
        if "g.state='building'" in cypher:
            self.marker = {"state": "building", "schema_version": 1}
        if "AS removed" in cypher:
            return Result([{"removed": 0}])
        if cypher.startswith("UNWIND $rows"):
            self.rows.extend(dict(row) for row in params["rows"])
        if "AS core ORDER BY chunk_id" in cypher:
            return Result([{**row, "content": "corrupt" if self.corrupt_readback else row["content"],
                            "core": not row["supplemental"]} for row in self.rows])
        if cypher.startswith("SHOW INDEXES"):
            return Result([{"state": "ONLINE"}])
        if "g += $evidence" in cypher:
            self.marker = {**params["evidence"], "state": "ready"}
        return Result()


class LexicalTests(unittest.TestCase):
    def make_index(self, path, *, lexical_provider=None):
        document = SourceDocument.create(source_name="fixture", source_id="public-card", title="Public card",
                                         text="Public corpus text that does not contain the query.")
        chunk = CanonicalChunk.from_document(document, 0, 0, len(document.text), document.text,
                                             redamon_unique_key="public-card")
        index = KBIndex(path, lexical_provider=lexical_provider)
        index.replace_snapshot((chunk,), SNAPSHOT, sources=(document,))
        return index, chunk

    def test_sanitizer_preserves_upstream_raw_lucene_semantics(self):
        self.assertEqual(sanitize_redamon_query('alpha+beta AND "gamma" foo:bar && a/b || end*'),
                         "alpha beta AND  gamma  foo bar   a b   end")
        self.assertEqual(sanitize_redamon_query("alpha beta"), "alpha beta")

    def test_kb_driver_requires_explicit_dedicated_endpoint(self):
        graph_only = {
            "BB_ASSISTANT_NEO4J_URI": "bolt://127.0.0.1:7687",
            "BB_ASSISTANT_NEO4J_USERNAME": "graph-user",
            "BB_ASSISTANT_NEO4J_PASSWORD": "graph-secret",
            "BB_ASSISTANT_NEO4J_DATABASE": "neo4j",
        }
        with self.assertRaisesRegex(ValueError, "dedicated KB Neo4j"):
            _driver_config_from_env(graph_only)

    def test_kb_driver_reads_separate_connection_namespace(self):
        env = {
            "BB_ASSISTANT_NEO4J_URI": "bolt://127.0.0.1:7687",
            "BB_ASSISTANT_KB_NEO4J_URI": "bolt+s://kb.example.internal:7687",
            "BB_ASSISTANT_KB_NEO4J_USERNAME": "kb-user",
            "BB_ASSISTANT_KB_NEO4J_PASSWORD": "kb-secret",
            "BB_ASSISTANT_KB_NEO4J_DATABASE": "knowledge",
        }
        self.assertEqual(_driver_config_from_env(env), (
            "bolt+s://kb.example.internal:7687", "kb-user", "kb-secret", "knowledge",
        ))

    def test_unpublished_generation_cannot_query_fulltext(self):
        for marker in (None, {"state": "building", "schema_version": 1}):
            driver = Driver(marker=marker)
            with self.assertRaisesRegex(RuntimeError, "not published"):
                Neo4jLexicalProvider(driver).search(SNAPSHOT, "query", limit=30)
            self.assertFalse(any("fulltext.queryNodes" in call[0] for call in driver.calls))

    def test_snapshot_and_duplicate_hits_are_rejected(self):
        good = {"chunk_id": "1" * 16, "snapshot_id": SNAPSHOT, "score": 2.3}
        for hits in ([{**good, "snapshot_id": "b" * 64}], [good, good], [{**good, "score": float("nan")}]):
            driver = Driver(marker=READY, hits=hits)
            with self.assertRaisesRegex(RuntimeError, "invalid snapshot provenance"):
                Neo4jLexicalProvider(driver).search(SNAPSHOT, "query", limit=30)

    def test_generation_is_selected_before_lucene_limit(self):
        driver = Driver(marker=READY,
                        hits=[{"chunk_id": "1" * 16, "snapshot_id": SNAPSHOT, "score": 4.0}])
        provider = Neo4jLexicalProvider(driver)
        self.assertEqual(provider.search(SNAPSHOT, "one two", limit=30), (("1" * 16, 4.0),))
        params = driver.calls[-1][1]
        self.assertEqual(params, {"index_name": f"kb_{SNAPSHOT}", "q_text": "one two", "limit": 30})
        provider.search(SNAPSHOT, "one two", limit=30, include_supplemental=True)
        self.assertEqual(driver.calls[-1][1]["index_name"], f"kb_{SNAPSHOT}_all")

    def test_generation_name_rejects_identifier_injection(self):
        with self.assertRaises(ValueError):
            generation_names("a` MATCH (n) DETACH DELETE n")

    def test_python_search_uses_lucene_scores_and_real_16hex_ids(self):
        class Stub:
            engine = "neo4j-lucene"

            def search(self, snapshot, query, **kwargs):
                self.received = (snapshot, query, kwargs)
                return ((self.chunk_id, 7.5),)

        stub = Stub()
        with tempfile.TemporaryDirectory() as temporary:
            index, chunk = self.make_index(Path(temporary) / "index.sqlite3", lexical_provider=stub)
            stub.chunk_id = chunk.chunk_id
            hits = index.search("zzzzunmatched", top_k=5)
            self.assertEqual([hit.chunk_id for hit in hits], [chunk.chunk_id])
            self.assertEqual(hits[0].bm25, 7.5)
            self.assertEqual(stub.received, (SNAPSHOT, "zzzzunmatched", {"limit": 30, "include_supplemental": False}))
            stub.chunk_id = "f" * 16
            with self.assertRaisesRegex(RuntimeError, "differ from the SQLite snapshot"):
                index.search("zzzzunmatched")

    def test_redamon_excluded_nuclei_card_is_supplemental_in_search_and_publish(self):
        wildcards = "*" * 1001
        excluded = SourceDocument.create(
            source_name="nuclei", source_id="http/large-literal.yaml", title="large-literal",
            text=("id: large-literal\ninfo:\n  name: Large literal payload\n"
                  "  description: needlemarker\nhttp:\n  - method: GET\n"
                  f'    path: ["{{{{BaseURL}}}}/{wildcards}"]\n'),
            metadata={"template_id": "large-literal"},
        )
        core = SourceDocument.create(
            source_name="nuclei", source_id="http/core.yaml", title="core",
            text="id: core\ninfo:\n  name: Core template\n  description: ordinary core text\n",
            metadata={"template_id": "core"},
        )
        supplemental_chunk = chunk_document(excluded)[0]
        core_chunk = chunk_document(core)[0]
        self.assertEqual(supplemental_chunk.metadata["redamon_excluded_reason"], "yaml_alias_character_limit")
        self.assertNotIn("canonicalization_fallback", supplemental_chunk.metadata)
        chunks = (supplemental_chunk, core_chunk)

        with tempfile.TemporaryDirectory() as temporary:
            fts_index = KBIndex(Path(temporary) / "fts.sqlite3")
            fts_index.replace_snapshot(chunks, SNAPSHOT, sources=(excluded, core))
            self.assertEqual(fts_index.search("needlemarker"), ())
            self.assertEqual([hit.chunk_id for hit in fts_index.search("needlemarker", include_supplemental=True)],
                             [supplemental_chunk.chunk_id])

            class Stub:
                engine = "neo4j-lucene"

                def search(self, snapshot, query, *, limit, include_supplemental=False):
                    return ((supplemental_chunk.chunk_id, 3.25),) if include_supplemental else ()

            lucene_index = KBIndex(Path(temporary) / "lucene.sqlite3", lexical_provider=Stub())
            lucene_index.replace_snapshot(chunks, SNAPSHOT, sources=(excluded, core))
            self.assertEqual(lucene_index.search("needlemarker"), ())
            hits = lucene_index.search("needlemarker", include_supplemental=True)
            self.assertEqual([hit.chunk_id for hit in hits], [supplemental_chunk.chunk_id])

            readonly = KBIndex(fts_index.path, read_only=True)
            driver = Driver()
            receipt = publish_generation(readonly, driver)
            self.assertEqual((receipt["core_count"], receipt["total_count"]), (1, 2))
            projected = {row["chunk_id"]: row for row in driver.rows}
            self.assertTrue(projected[supplemental_chunk.chunk_id]["supplemental"])
            self.assertFalse(projected[core_chunk.chunk_id]["supplemental"])

    def test_publish_checks_remote_content_before_ready_marker(self):
        with tempfile.TemporaryDirectory() as temporary:
            index, _ = self.make_index(Path(temporary) / "index.sqlite3")
            readonly = KBIndex(index.path, read_only=True)
            driver = Driver(corrupt_readback=True)
            with self.assertRaisesRegex(RuntimeError, "re-read differs"):
                publish_generation(readonly, driver)
            self.assertEqual(driver.marker["state"], "building")
            self.assertFalse(any("g += $evidence" in call[0] for call in driver.calls))
            good = Driver()
            receipt = publish_generation(readonly, good)
            self.assertEqual(receipt["state"], "ready")
            self.assertEqual(receipt["core_count"], 1)
            self.assertEqual(good.marker["total_sha256"], receipt["total_sha256"])


if __name__ == "__main__":
    unittest.main()
