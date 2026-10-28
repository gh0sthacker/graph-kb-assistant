from __future__ import annotations

import hashlib
import types
import unittest
from pathlib import Path
import sqlite3
import tempfile
from unittest.mock import patch

from bb_assistant.kb.ranking import (
    RankedCandidate, rank_candidates, reciprocal_rank_fusion, source_title_similarity,
)
from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.index import KBIndex, _commit_with_busy_retry, _redamon_fulltext_expression
from bb_assistant.project_lock import project_maintenance_read_lock


class _FixtureEmbedder:
    model_name = "intfloat/e5-large-v2"

    def embed_documents_batch(self, texts, *, batch_size=64):
        return [[1.0, 0.0] if "alpha" in text else [0.0, 1.0] for text in texts]

    def embed_query(self, text):
        return [0.0, 1.0]


class _FixtureReranker:
    def score(self, query, texts):
        return [1.0 if "alpha" in text else 0.0 for text in texts]


class _RecordingEmbedder(_FixtureEmbedder):
    def __init__(self):
        self.batches = []

    def embed_documents_batch(self, texts, *, batch_size=64):
        self.batches.append(tuple(texts))
        return super().embed_documents_batch(texts, batch_size=batch_size)


class _FailOnSecondBatchEmbedder(_FixtureEmbedder):
    def __init__(self):
        self.calls = 0

    def embed_documents_batch(self, texts, *, batch_size=64):
        self.calls += 1
        if self.calls == 2:
            raise RuntimeError("synthetic interrupted embedding run")
        return super().embed_documents_batch(texts, batch_size=batch_size)


class _RecordingEmbedder(_FixtureEmbedder):
    def __init__(self):
        self.texts = []
        self.batches = []

    def embed_documents_batch(self, texts, *, batch_size=64):
        self.texts.extend(texts)
        self.batches.append(tuple(texts))
        return super().embed_documents_batch(texts, batch_size=batch_size)


class _TransientLockConnection:
    def __init__(self, failures):
        self.failures = failures
        self.attempts = 0

    def commit(self):
        self.attempts += 1
        if self.attempts <= self.failures:
            raise sqlite3.OperationalError("database is locked")


class RetrievalRankingTests(unittest.TestCase):
    def test_mmr_title_normalization_matches_redamon_lower_not_casefold(self) -> None:
        first = RankedCandidate("a", "nvd", "Straße vulnerability")
        second = RankedCandidate("b", "nuclei", "Strasse vulnerability")
        self.assertAlmostEqual(source_title_similarity(first, second), 0.2)

    def test_supplemental_chunks_are_opt_in_and_remain_searchable(self) -> None:
        canonical_document = SourceDocument.create(
            source_name="fixture", source_id="canonical.yaml", title="Canonical template",
            text="Nuclei template describes a safe synthetic test.", metadata={"document_role": "content"},
        )
        supplemental_document = SourceDocument.create(
            source_name="fixture", source_id="wordpress-plugin-detect.yaml", title="Raw template fallback",
            text="WordPress plugin version detection synthetic template.",
            metadata={"document_role": "content", "canonicalization_fallback": "yaml_size_limit"},
        )
        documents = (canonical_document, supplemental_document)
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-supplemental-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3", reranker=_FixtureReranker())
            index.replace_snapshot(chunks, "a" * 64, sources=documents)

            default_hits = index.search("WordPress plugin version detection", top_k=5)
            supplemental_hits = index.search(
                "WordPress plugin version detection", top_k=5, include_supplemental=True,
            )

        self.assertEqual(default_hits, ())
        self.assertEqual([hit.source_id for hit in supplemental_hits], ["wordpress-plugin-detect.yaml"])

    def test_lexical_query_terms_match_redamon_without_prefix_expansion(self) -> None:
        self.assertEqual(
            _redamon_fulltext_expression("auth sql-injection"),
            '"auth" OR "sql" OR "injection"',
        )
        self.assertEqual(_redamon_fulltext_expression("x"), '"x"')
        self.assertEqual(
            _redamon_fulltext_expression("needle AND body"),
            '"needle" AND "body"',
        )
        long_query = " ".join(f"term{index}" for index in range(40)) + " AND final"
        expression = _redamon_fulltext_expression(long_query)
        self.assertIn('"term39"', expression)
        self.assertTrue(expression.endswith('AND "final"'))

        documents = (
            SourceDocument.create(
                source_name="tool_docs", source_id="auth.md", title="Auth guide",
                text="auth flow", metadata={"document_role": "content"},
            ),
            SourceDocument.create(
                source_name="tool_docs", source_id="authentication.md", title="Authentication guide",
                text="authentication flow", metadata={"document_role": "content"},
            ),
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-lucene-query-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3", reranker=_FixtureReranker())
            index.replace_snapshot(chunks, "e" * 64, sources=documents)
            hits = index.search("auth", top_k=5)
        self.assertEqual([hit.source_id for hit in hits], ["auth.md"])

    def test_source_allowlist_does_not_backfill_global_lexical_pool(self) -> None:
        documents = [
            SourceDocument.create(
                source_name="nvd", source_id=f"CVE-2026-{index + 1000}", title=f"Needle guide {index}",
                text="needle reference", metadata={"document_role": "content"},
            )
            for index in range(30)
        ]
        documents.append(SourceDocument.create(
            source_name="tool_docs", source_id="allowed.md", title="Sparse guide",
            text="needle " + "background " * 80, metadata={"document_role": "content"},
        ))
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-source-pool-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3", reranker=_FixtureReranker())
            index.replace_snapshot(chunks, "f" * 64, sources=documents)
            hits = index.search("needle", top_k=1, source_names=("tool_docs",))
        self.assertEqual(hits, ())

    def test_lexical_bm25_weights_title_and_body_equally(self) -> None:
        documents = (
            SourceDocument.create(
                source_name="nvd", source_id="title-hit", title="needle guide",
                text="alpha beta", metadata={"document_role": "content"},
            ),
            SourceDocument.create(
                source_name="nvd", source_id="body-hit", title="sample guide",
                text="needle beta", metadata={"document_role": "content"},
            ),
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-bm25-parity-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3", reranker=_FixtureReranker())
            index.replace_snapshot(chunks, "1" * 64, sources=documents)
            hits = index.search("needle", top_k=2)
        self.assertEqual(len(hits), 2)
        self.assertAlmostEqual(hits[0].bm25, hits[1].bm25, places=7)

    def test_embedding_batches_process_shorter_chunks_first(self) -> None:
        documents = tuple(
            SourceDocument.create(
                source_name="nvd", source_id=f"CVE-2026-{index:05d}", title=f"CVE-{index}",
                text=text, metadata={"document_role": "content"},
            )
            for index, text in enumerate(("long " * 100, "tiny", "medium " * 10))
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-embed-order-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3")
            index.replace_snapshot(chunks, "c" * 64, sources=documents)
            embedder = _RecordingEmbedder()
            self.assertEqual(index.rebuild_embeddings(embedder, batch_size=2), 3)
        self.assertEqual(embedder.texts, sorted((chunk.text for chunk in chunks), key=len))

    def test_embedding_inference_releases_project_maintenance_lock(self) -> None:
        documents = (
            SourceDocument.create(
                source_name="tool_docs", source_id="alpha.md", title="Alpha",
                text="alpha authentication flow", metadata={"document_role": "content"},
            ),
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-vector-lock-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3")
            index.replace_snapshot(chunks, "d" * 64, sources=documents)

            class LockCheckingEmbedder(_FixtureEmbedder):
                def embed_documents_batch(self, texts, *, batch_size=64):
                    with project_maintenance_read_lock(index.path.parent, timeout=0.1):
                        return super().embed_documents_batch(texts, batch_size=batch_size)

            self.assertEqual(index.rebuild_embeddings(LockCheckingEmbedder(), batch_size=1), 1)
            self.assertTrue(index.stats()["vector_ready"])

    def test_embedding_batch_commit_retries_transient_sqlite_lock(self) -> None:
        connection = _TransientLockConnection(failures=2)
        with patch("bb_assistant.kb.index.time.sleep"):
            _commit_with_busy_retry(connection)
        self.assertEqual(connection.attempts, 3)

    def test_embedding_batch_commit_does_not_retry_non_lock_errors(self) -> None:
        class BrokenConnection:
            def commit(self):
                raise sqlite3.OperationalError("disk I/O error")

        with self.assertRaisesRegex(sqlite3.OperationalError, "disk I/O error"):
            _commit_with_busy_retry(BrokenConnection())

    def test_embedding_queue_batches_shorter_chunks_first(self) -> None:
        documents = (
            SourceDocument.create(
                source_name="tool_docs", source_id="long.md", title="Long",
                text="long " * 300, metadata={"document_role": "content"},
            ),
            SourceDocument.create(
                source_name="nvd", source_id="CVE-2026-0099", title="Short",
                text="short", metadata={"document_role": "content"},
            ),
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-embed-order-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3")
            index.replace_snapshot(chunks, "c" * 64, sources=documents)
            embedder = _RecordingEmbedder()
            index.rebuild_embeddings(embedder, batch_size=1)
        self.assertEqual(len(embedder.batches[0][0]), min(len(chunk.text) for chunk in chunks))

    def test_persisted_vectors_retrieve_semantic_candidate_without_lexical_match(self) -> None:
        documents = (
            SourceDocument.create(
                source_name="tool_docs", source_id="alpha.md", title="Alpha",
                text="alpha authentication flow", metadata={"document_role": "content"},
            ),
            SourceDocument.create(
                source_name="nvd", source_id="CVE-2026-0002", title="Bravo",
                text="bravo credential renewal", metadata={"document_role": "content"},
            ),
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-vector-test-") as temporary:
            index = KBIndex(
                Path(temporary) / "index.sqlite3", embedder=_FixtureEmbedder(),
                reranker=_FixtureReranker(),
            )
            index.replace_snapshot(chunks, "a" * 64, sources=documents)
            self.assertEqual(index.rebuild_embeddings(_FixtureEmbedder(), batch_size=1), 2)
            self.assertTrue(index.stats()["vector_ready"])
            hits = index.search("semantic concept", top_k=2)
            self.assertEqual({hit.source_id for hit in hits}, {"alpha.md", "CVE-2026-0002"})
            self.assertIsNotNone(next(hit for hit in hits if hit.source_id == "alpha.md").rerank_score)

    def test_faiss_backend_uses_snapshot_cache_and_fails_closed(self) -> None:
        import numpy as np

        class FakeIndexFlatIP:
            def __init__(self, dimensions):
                self.dimensions = dimensions
                self.vectors = np.empty((0, dimensions), dtype=np.float32)
                self.ntotal = 0

            def add(self, vectors):
                self.vectors = np.vstack((self.vectors, vectors))
                self.ntotal = len(self.vectors)

            def search(self, queries, count):
                scores = queries @ self.vectors.T
                order = np.argsort(scores[0])[::-1][:count]
                return scores[:, order], order.reshape(1, -1)

        fake_faiss = types.SimpleNamespace(IndexFlatIP=FakeIndexFlatIP)
        documents = (
            SourceDocument.create(source_name="tool_docs", source_id="a", title="Alpha", text="alpha details"),
            SourceDocument.create(source_name="tool_docs", source_id="b", title="Beta", text="beta details"),
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-faiss-test-") as temporary:
            with patch.dict("os.environ", {"BB_ASSISTANT_KB_VECTOR_ENGINE": "faiss"}), \
                    patch.dict("sys.modules", {"faiss": fake_faiss}):
                index = KBIndex(Path(temporary) / "index.sqlite3", embedder=_FixtureEmbedder())
                index.replace_snapshot(chunks, "b" * 64, sources=documents)
                index.rebuild_embeddings(_FixtureEmbedder(), batch_size=2)
                ranked_ids, scores = index._vector_search("semantic-only", top_k=2)
                cache_before = index._faiss_cache
                second_ids, _ = index._vector_search("semantic-only", top_k=2)

            self.assertEqual(index.vector_engine, "faiss")
            self.assertEqual(ranked_ids, [chunks[1].chunk_id, chunks[0].chunk_id])
            self.assertGreater(scores[chunks[1].chunk_id], scores[chunks[0].chunk_id])
            self.assertEqual(second_ids, ranked_ids)
            self.assertIs(index._faiss_cache, cache_before)

        with tempfile.TemporaryDirectory(prefix="bb-kb-faiss-incomplete-test-") as temporary:
            with patch.dict("os.environ", {"BB_ASSISTANT_KB_VECTOR_ENGINE": "faiss"}), \
                    patch.dict("sys.modules", {"faiss": fake_faiss}):
                index = KBIndex(Path(temporary) / "index.sqlite3", embedder=_FixtureEmbedder())
                with self.assertRaisesRegex(RuntimeError, "complete, published"):
                    index._vector_search("query", top_k=1)

    def test_vector_ready_requires_the_runtime_vector_backend(self) -> None:
        document = SourceDocument.create(
            source_name="tool_docs", source_id="alpha.md", title="Alpha",
            text="alpha authentication flow", metadata={"document_role": "content"},
        )
        chunks = tuple(chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-vector-backend-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3")
            index.replace_snapshot(chunks, "f" * 64, sources=(document,))
            index.rebuild_embeddings(_FixtureEmbedder(), batch_size=1)
            with patch("bb_assistant.kb.index.importlib.util.find_spec", return_value=None):
                stats = index.stats()
        self.assertTrue(stats["vector_storage_complete"])
        self.assertFalse(stats["vector_backend_available"])
        self.assertFalse(stats["vector_ready"])

    def test_kb_stats_reports_snapshot_source_manifest_freshness(self) -> None:
        document = SourceDocument.create(
            source_name="tool_docs", source_id="alpha.md", title="Alpha",
            text="alpha authentication flow", metadata={"document_role": "content"},
        )
        chunks = tuple(chunk_document(document))
        manifest_path = Path(__file__).resolve().parents[1] / "src/bb_assistant/kb/sources.yaml"
        current_manifest_sha256 = hashlib.sha256(manifest_path.read_bytes()).hexdigest()
        with tempfile.TemporaryDirectory(prefix="bb-kb-manifest-freshness-test-") as temporary:
            index = KBIndex(Path(temporary) / "index.sqlite3")
            index.replace_snapshot(chunks, "c" * 64, source_manifest_sha256="a" * 64)
            stale = index.stats()
            self.assertEqual(stale["source_manifest_sha256"], "a" * 64)
            self.assertEqual(stale["current_source_manifest_sha256"], current_manifest_sha256)
            self.assertFalse(stale["source_manifest_matches_current"])

            index.replace_snapshot(chunks, "d" * 64, source_manifest_sha256=current_manifest_sha256)
            current = index.stats()
            legacy_index = KBIndex(Path(temporary) / "legacy.sqlite3")
            legacy_index.replace_snapshot(chunks, "e" * 64)
            snapshot_dir = Path(temporary) / "snapshots" / ("e" * 64)
            snapshot_dir.mkdir(parents=True)
            (snapshot_dir / "manifest.json").write_text(
                '{"source_manifest_sha256":"' + "f" * 64 + '"}', encoding="utf-8",
            )
            legacy = legacy_index.stats()
        self.assertTrue(current["source_manifest_matches_current"])
        self.assertEqual(legacy["source_manifest_sha256"], "f" * 64)
        self.assertFalse(legacy["source_manifest_matches_current"])

    def test_embedding_batches_resume_after_interruption_without_publishing_partial_vectors(self) -> None:
        documents = (
            SourceDocument.create(
                source_name="tool_docs", source_id="alpha.md", title="Alpha",
                text="alpha authentication flow", metadata={"document_role": "content"},
            ),
            SourceDocument.create(
                source_name="nvd", source_id="CVE-2026-0002", title="Bravo",
                text="bravo credential renewal", metadata={"document_role": "content"},
            ),
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-vector-resume-test-") as temporary:
            path = Path(temporary) / "index.sqlite3"
            index = KBIndex(path)
            index.replace_snapshot(chunks, "b" * 64, sources=documents)
            with self.assertRaisesRegex(RuntimeError, "synthetic interrupted"):
                index.rebuild_embeddings(_FailOnSecondBatchEmbedder(), batch_size=1)

            partial = index.stats()
            self.assertEqual(partial["embedded_chunks"], 1)
            self.assertFalse(partial["vector_ready"])

            resumed = KBIndex(path)
            embedder = _FixtureEmbedder()
            self.assertEqual(resumed.rebuild_embeddings(embedder, batch_size=1), 2)
            self.assertTrue(resumed.stats()["vector_ready"])

    def test_partial_vectors_respect_policy_and_search_only_nonempty_vectors(self) -> None:
        documents = (
            SourceDocument.create(
                source_name="tool_docs", source_id="alpha.md", title="Alpha",
                text="alpha", metadata={"document_role": "content"},
            ),
            SourceDocument.create(
                source_name="nvd", source_id="CVE-2026-0002", title="Bravo",
                text="bravo " * 20, metadata={"document_role": "content"},
            ),
        )
        chunks = tuple(chunk for document in documents for chunk in chunk_document(document))
        with tempfile.TemporaryDirectory(prefix="bb-kb-partial-vector-test-") as temporary:
            path = Path(temporary) / "index.sqlite3"
            all_policy = KBIndex(path, embedder=_FixtureEmbedder())
            all_policy.replace_snapshot(chunks, "a" * 64, sources=documents)
            with self.assertRaisesRegex(RuntimeError, "synthetic interrupted"):
                all_policy.rebuild_embeddings(_FailOnSecondBatchEmbedder(), batch_size=1)
            self.assertFalse(all_policy.stats()["vector_ready"])

            with all_policy.connect() as connection:
                meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
                connection.execute(
                    "INSERT INTO chunk_embeddings(chunk_id,model_name,dimensions,vector) VALUES(?,?,?,?)",
                    (chunks[1].chunk_id, meta["vector_staging_model"], 2, b""),
                )

            with patch.dict("os.environ", {"KB_VECTOR_POLICY": "min_share=0.5"}):
                partial_policy = KBIndex(path, embedder=_FixtureEmbedder())
            stats = partial_policy.stats()
            ranked_ids, scores = partial_policy._vector_search("semantic-only", top_k=2)
            with partial_policy.connect() as connection:
                meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())

        self.assertTrue(stats["vector_ready"])
        self.assertTrue(stats["vector_partial"])
        self.assertEqual(stats["vector_coverage_by_source"]["tool_docs"], {"chunks": 1, "vectors": 1})
        self.assertEqual(stats["vector_coverage_by_source"]["nvd"], {"chunks": 1, "vectors": 0})
        self.assertEqual(ranked_ids, [chunks[0].chunk_id])
        self.assertEqual(set(scores), {chunks[0].chunk_id})
        self.assertEqual(meta["vector_partial"], "true")
        self.assertEqual(
            meta["vector_coverage_by_source"],
            '{"nvd":{"chunks":1,"vectors":0},"tool_docs":{"chunks":1,"vectors":1}}',
        )

    def test_rrf_combines_ranks_and_ignores_duplicate_ids_within_one_list(self) -> None:
        scores = reciprocal_rank_fusion(
            ["a", "b", "a"], ["b", "a", "c"], rrf_k=60,
        )
        self.assertAlmostEqual(scores["a"], 1 / 61 + 1 / 62)
        self.assertAlmostEqual(scores["b"], 1 / 62 + 1 / 61)
        self.assertAlmostEqual(scores["c"], 1 / 63)

    def test_source_boost_rerank_sigmoid_and_mmr_are_applied(self) -> None:
        candidates = (
            RankedCandidate("a", "tool_docs", "OWASP Authentication Guide"),
            RankedCandidate("b", "tool_docs", "OWASP Authentication Guide duplicate"),
            RankedCandidate("c", "gtfobins", "Linux privilege escalation"),
        )
        results = rank_candidates(
            candidates,
            vector_ids=["a", "b", "c"],
            fulltext_ids=["a", "b", "c"],
            top_k=2,
            rerank_scores={"a": 0.0, "b": 0.0, "c": 0.0},
        )
        self.assertEqual([item.chunk_id for item in results], ["a", "c"])
        self.assertAlmostEqual(results[0].score, 0.5 * 1.20)
        self.assertAlmostEqual(results[1].score, 0.5 * 1.15)
        self.assertEqual(results[0].rerank_score, 0.0)

    def test_unreranked_tail_stays_after_reranked_pool_when_logits_are_low(self) -> None:
        pool_ids = [f"pool-{index:02d}" for index in range(30)]
        tail_ids = ["tail-a", "tail-b"]
        candidates = tuple(
            RankedCandidate(chunk_id, "nvd", chunk_id)
            for chunk_id in (*pool_ids, *tail_ids)
        )
        # All pool logits are far below the RRF score of the untouched tail.
        # RedAmon still keeps the reranked pool first and appends the tail in
        # its previous fused order.
        rerank_scores = {chunk_id: -20.0 - index for index, chunk_id in enumerate(pool_ids)}

        results = rank_candidates(
            candidates,
            vector_ids=pool_ids + tail_ids,
            top_k=32,
            rerank_scores=rerank_scores,
            mmr_enabled=False,
        )

        self.assertEqual([item.chunk_id for item in results[:30]], pool_ids)
        self.assertEqual([item.chunk_id for item in results[30:]], tail_ids)

    def test_fulltext_only_ranker_uses_redamon_defaults_and_top_five(self) -> None:
        candidates = tuple(RankedCandidate(str(i), "nvd", f"CVE-{i}") for i in range(8))
        results = rank_candidates(
            candidates,
            fulltext_ids=[item.chunk_id for item in candidates],
        )
        self.assertEqual(len(results), 5)
        self.assertEqual(results[0].chunk_id, "0")


if __name__ == "__main__":
    unittest.main()
