from __future__ import annotations

import unittest

from bb_assistant.kb.ranking import RankedCandidate, rank_candidates, reciprocal_rank_fusion


class RetrievalRankingTests(unittest.TestCase):
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
