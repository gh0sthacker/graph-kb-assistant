from __future__ import annotations

import unittest
from pathlib import Path
import sqlite3
import tempfile
from unittest.mock import patch

from bb_assistant.kb.ranking import RankedCandidate, rank_candidates, reciprocal_rank_fusion
from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.index import KBIndex, _commit_with_busy_retry
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
            self.assertEqual(index.rebuild_embeddings(embedder, batch_size=3), 3)
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
                source_name="gtfobins", source_id="short.md", title="Short",
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
                source_name="gtfobins", source_id="bravo.md", title="Bravo",
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
            self.assertEqual({hit.source_id for hit in hits}, {"alpha.md", "bravo.md"})
            self.assertIsNotNone(next(hit for hit in hits if hit.source_id == "alpha.md").rerank_score)

    def test_embedding_batches_resume_after_interruption_without_publishing_partial_vectors(self) -> None:
        documents = (
            SourceDocument.create(
                source_name="tool_docs", source_id="alpha.md", title="Alpha",
                text="alpha authentication flow", metadata={"document_role": "content"},
            ),
            SourceDocument.create(
                source_name="gtfobins", source_id="bravo.md", title="Bravo",
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
