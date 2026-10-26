# RedAmon KB parity gate

`bb-assistant-kb parity-check` compares two operator-produced JSON receipts. It
is an offline gate: it does not fetch RedAmon data, inspect a deployed RedAmon
instance, rebuild the local KB, or authenticate who produced the receipts.
`PASS` means the supplied claims match; it is not proof of deployment identity.

Create the Graph Assistant side directly from a completed local snapshot and
read-only index:

```sh
bb-assistant-kb --index-path /private/kb/index.sqlite3 parity-export \
  --snapshot-dir /private/kb/snapshots/<snapshot-id> \
  --profile full \
  --fixture references/kb-parity-queries.json \
  --output /private/kb/graph-assistant-receipt.json
```

The exporter refuses an active SQLite WAL, maintenance marker, snapshot/index
mismatch, incomplete source coverage, unpublished or incomplete E5 vectors,
missing pinned-query results, or unavailable vector/reranker score components.
It never fetches sources or writes to the index. The output file is atomically
written with owner-only permissions. It records the actual SQLite FTS5/cosine
backend and score components, so a receipt can expose a mismatch instead of
claiming that the local backend is RedAmon. It is still a local assertion and
does not authenticate the deployed RedAmon reference.

```sh
bb-assistant-kb parity-check \
  --redamon /restricted/path/redamon-kb-receipt.json \
  --assistant /restricted/path/graph-assistant-kb-receipt.json \
  --fixture references/kb-parity-queries.json
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
    "reranker": {"model": "BAAI/bge-reranker-base", "revision": "2cfc18c9415c912f9d8155881c133215df768a70"},
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

Both receipts must contain exactly the query IDs in the checked-in fixture;
each query's text, `top_k`, and filters must match it. The fixture contains
ten generic public security-technique queries and can be revised only by a
reviewed code change. This prevents passing the gate with a cherry-picked query
set that happens to match.

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

## Operational and rights gates

The pinned official ExploitDB GitLab README states that the repository project
and SearchSploit are released under GPLv2. The local manifest therefore permits
the self-hosted provider's allowlisted `files_exploits.csv` descriptions and
metadata at that pinned revision, while retaining the pinned README and
LICENSE notices. It never downloads or copies PoC/source files. This scoped
decision does not claim separate licensing for third-party PoCs or authorize
redistribution of the assembled corpus; any public distribution needs its own
rights review. The provider fails closed if its URL, pinned revision, file
allowlist, or metadata-only filter changes.

Long E5 builds run only on Proxmox, never on Kali. `bb-assistant-kb embed`
requires a host-identified, read-only thermal sensor source; the hwmon
directory mode resolves the uniquely labelled `coretemp` `Package id 0`
sensor on each check. It checks before
each durable batch, fails closed on unreadable sensor data, and stops at 70 C.
The conservative invocation is `--batch-size 2 --threads 1`; the worker also
takes an exclusive process lock so a second embedding process cannot overlap.
The deployment unit is [`graph-kb-assistant-kb-embed.service`](../deploy/graph-kb-assistant-kb-embed.service)
and caps CPU at 20%, disables automatic restarts, and is enabled for boot so
the same guarded job resumes after a CT restart. The CLI still waits for the
ten-minute thermal gate before model loading. Do not start or resume until the package temperature has remained
below 70 C for at least 10 continuous minutes. A single cool reading does not
satisfy this precondition.
Before enabling the unit, create the root-owned
`/etc/graph-kb-assistant/kb-embed.env` with
`BB_ASSISTANT_KB_INDEX=/home/max/.local/share/bb-graph-assistant/kb/candidates/<candidate-id>/index.sqlite3`.
The CLI refuses missing indexes, symlink targets, and any index outside that
candidate layout before opening a writable database; the live application
index is not an embedding destination.
Thermal stops preserve completed batches but do not publish the vector-ready
marker. `vector_ready` requires both complete snapshot-bound vectors and an
available NumPy search backend; `vector_storage_complete` reports the durable
generation state separately. Readiness remains false until every content
chunk has its pinned E5 vector, the completion marker is written, and the
runtime backend is available.
The embed CLI waits for a sampled ten-minute dwell below 70 C before starting
and after a `ThermalLimitReached` batch stop, then resumes the same staged job.
Unreadable sensors and all non-thermal errors fail closed without retry.
