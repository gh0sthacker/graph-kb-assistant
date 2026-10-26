# RedAmon KB parity gate

`bb-assistant-kb parity-check` compares two operator-produced JSON receipts. It
is an offline gate: it does not fetch RedAmon data, inspect a deployed RedAmon
instance, rebuild the local KB, or authenticate who produced the receipts.
`PASS` means the supplied claims match; it is not proof of deployment identity.

```sh
bb-assistant-kb parity-check \
  --redamon /restricted/path/redamon-kb-receipt.json \
  --assistant /restricted/path/graph-assistant-kb-receipt.json
```

The command writes a JSON report. It exits `0` only when every criterion
matches, `3` when evidence differs, and a non-zero error for a missing or
malformed receipt. Score comparison is exact by default. An explicit
`--score-tolerance 0.000001` may be used only when both systems document a
known numeric serialization difference; it never relaxes result ordering.

## Receipt contract

Both files use schema version 1 and the same shape; `producer` is respectively
`redamon` or `graph-kb-assistant`.

```json
{
  "schema_version": 1,
  "producer": "redamon",
  "profile": "full",
  "corpus": {
    "snapshot_id": "<sha256>",
    "source_manifest_sha256": "<sha256>",
    "source_documents_sha256": "<sha256>",
    "chunks_sha256": "<sha256>",
    "source_count": 7,
    "document_count": 123,
    "chunk_count": 456,
    "coverage_state": "DONE",
    "vectors": {
      "model": "intfloat/e5-large-v2",
      "revision": "<pinned-revision>",
      "dimensions": 1024,
      "embedded_chunks": 456,
      "total_content_chunks": 456,
      "complete": true
    },
    "reranker": {"model": "BAAI/bge-reranker-v2-m3", "revision": "<pinned-revision>"},
    "retrieval": {
      "lexical_engine": "neo4j-lucene",
      "vector_engine": "faiss",
      "candidate_pool": 30,
      "rrf_k": 60,
      "rerank_pool_size": 30,
      "mmr_lambda": 0.65
    }
  },
  "sources": [{
    "name": "owasp",
    "revision": "<effective-source-revision>",
    "filters": {"profile": "full"},
    "document_count": 123,
    "chunk_count": 456,
    "content_sha256": "<canonical-family-content-digest>",
    "chunk_sha256": "<canonical-family-chunk-digest>",
    "complete": true
  }],
  "queries": [{
    "id": "authorization-boundary",
    "query": "authorization boundary",
    "top_k": 5,
    "filters": {"source_names": []},
    "results": [{
      "chunk_id": "<stable-chunk-id>",
      "score_components": {"lexical": 0.1, "vector": 0.2, "rerank": 0.3}
    }]
  }]
}
```

The source list must account for all corpus source/document/chunk counts; every
source must be complete and include its effective revision, filters, and
canonical content/chunk digests. Corpus coverage must be `DONE`, vector evidence
must explicitly cover every content chunk, and model revisions must be pinned.
Both receipts must identify the lexical/vector engines and ranking recipe.
For family digests, both exporters must serialize the same canonical
`SourceDocument`/`CanonicalChunk` records as compact UTF-8 JSON with sorted keys,
one record per line, ordered by `(source_name, source_id)` for documents and
`(source_name, source_id, ordinal)` for chunks, then SHA-256 the complete byte
stream. Do not hash local database pages or backend-specific index files.
Query fixtures must be non-empty and use unique IDs; the checker compares the
fixture text, filters, `top_k`, ordered chunk IDs, score component names, and
numeric values. A missing reference, incomplete corpus or vector generation, changed source,
different ranking, or changed score fails closed.

Do not put credentials, raw traffic, or target data in receipts. Use public KB
corpus metadata and fixed public technique queries. Until a deployed RedAmon
profile export and matching query receipt exist, KB parity remains unproven.
