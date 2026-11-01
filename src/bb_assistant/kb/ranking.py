"""Deterministic retrieval ranking stages shared with RedAmon's KB policy."""
from __future__ import annotations

from dataclasses import dataclass, replace
import math
from typing import Mapping, Sequence


DEFAULT_TOP_K = 5
DEFAULT_OVERFETCH_FACTOR = 6
DEFAULT_RRF_K = 60
DEFAULT_RERANK_POOL_SIZE = 30
DEFAULT_MMR_LAMBDA = 0.65
DEFAULT_SOURCE_BOOSTS: dict[str, float] = {
    "tool_docs": 1.20,
    "gtfobins": 1.15,
    "lolbas": 1.15,
    "owasp": 1.05,
    "nuclei": 1.00,
    "nvd": 0.90,
    "exploitdb": 0.85,
}


@dataclass(frozen=True, slots=True)
class RankedCandidate:
    chunk_id: str
    source_name: str
    title: str
    lexical_score: float | None = None
    vector_score: float | None = None
    rerank_score: float | None = None
    score: float = 0.0


def reciprocal_rank_fusion(
    vector_ids: Sequence[str], fulltext_ids: Sequence[str], *, rrf_k: int = DEFAULT_RRF_K,
) -> dict[str, float]:
    """Fuse vector/full-text ranks without comparing incompatible raw scores."""
    if rrf_k < 1:
        raise ValueError("rrf_k must be positive")
    result: dict[str, float] = {}
    for ranked_ids in (vector_ids, fulltext_ids):
        seen: set[str] = set()
        for rank, chunk_id in enumerate(ranked_ids, start=1):
            if chunk_id in seen:
                continue
            seen.add(chunk_id)
            result[chunk_id] = result.get(chunk_id, 0.0) + 1.0 / (rrf_k + rank)
    return result


def source_title_similarity(a: RankedCandidate, b: RankedCandidate) -> float:
    """RedAmon's inexpensive source/title overlap proxy used by MMR."""
    similarity = 0.4 if a.source_name and a.source_name == b.source_name else 0.0
    # RedAmon uses str.lower(), not casefold(); keep Unicode tokenization
    # identical because the similarity score can change MMR result order.
    tokens_a = set(a.title.lower().split())
    tokens_b = set(b.title.lower().split())
    if tokens_a or tokens_b:
        similarity += 0.6 * len(tokens_a & tokens_b) / max(len(tokens_a | tokens_b), 1)
    return min(similarity, 1.0)


def rank_candidates(
    candidates: Sequence[RankedCandidate],
    *,
    vector_ids: Sequence[str] = (),
    fulltext_ids: Sequence[str] = (),
    top_k: int = DEFAULT_TOP_K,
    rrf_k: int = DEFAULT_RRF_K,
    source_boosts: Mapping[str, float] = DEFAULT_SOURCE_BOOSTS,
    rerank_scores: Mapping[str, float] | None = None,
    mmr_enabled: bool = True,
    mmr_lambda: float = DEFAULT_MMR_LAMBDA,
) -> tuple[RankedCandidate, ...]:
    """Apply RRF, RedAmon source boosts, optional cross-encoder scores, MMR."""
    if top_k < 1 or not 0.0 <= mmr_lambda <= 1.0:
        raise ValueError("top_k must be positive and mmr_lambda must be in [0, 1]")
    rrf_scores = reciprocal_rank_fusion(vector_ids, fulltext_ids, rrf_k=rrf_k)
    rescored = []
    fused_scores: dict[str, float] = {}
    for candidate in candidates:
        if candidate.chunk_id not in rrf_scores:
            continue
        boost = float(source_boosts.get(candidate.source_name, 1.0))
        fused_scores[candidate.chunk_id] = rrf_scores[candidate.chunk_id] * boost
        if rerank_scores is None or candidate.chunk_id not in rerank_scores:
            score = fused_scores[candidate.chunk_id]
            rerank_score = None
        else:
            rerank_score = float(rerank_scores[candidate.chunk_id])
            # RedAmon converts cross-encoder logits with sigmoid before boosts.
            probability = 1.0 / (1.0 + math.exp(-max(-60.0, min(60.0, rerank_score))))
            score = probability * boost
        rescored.append(replace(candidate, rerank_score=rerank_score, score=score))
    # Establish the fused order before reordering the reranker pool. RedAmon
    # reranks only the head of this list, then appends the untouched tail in
    # its original fused order. Do not compare tail RRF scores with the
    # cross-encoder probabilities: they are different score scales and a low
    # logit must not promote the tail above the reranked pool.
    rescored.sort(key=lambda item: fused_scores[item.chunk_id], reverse=True)
    if rerank_scores is not None:
        reranked = [item for item in rescored if item.rerank_score is not None]
        unreranked = [item for item in rescored if item.rerank_score is None]
        reranked.sort(key=lambda item: item.score, reverse=True)
        rescored = reranked + unreranked
    if not mmr_enabled or len(rescored) <= top_k:
        return tuple(rescored[:top_k])

    selected = [rescored[0]]
    remaining = rescored[1:]
    while len(selected) < top_k and remaining:
        best_index = max(
            range(len(remaining)),
            key=lambda index: mmr_lambda * remaining[index].score
            - (1.0 - mmr_lambda) * max(source_title_similarity(remaining[index], item) for item in selected),
        )
        selected.append(remaining.pop(best_index))
    return tuple(selected)
