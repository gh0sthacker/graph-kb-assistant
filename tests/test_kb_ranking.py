from __future__ import annotations

import unittest
from pathlib import Path
import tempfile

from bb_assistant.kb.ranking import RankedCandidate, rank_candidates, reciprocal_rank_fusion
from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.index import KBIndex


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


class RetrievalRankingTests(unittest.TestCase):
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
