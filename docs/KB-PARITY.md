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
written with owner-only permissions. It records the selected lexical engine,
SQLite cosine backend and score components, so a receipt can expose a mismatch instead of
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

## Snapshot-isolated Lucene migration

RedAmon's default Neo4j Lucene analyzer and raw sanitized query are now
available to both the Go runtime and the Python search/receipt path. Set
`BB_ASSISTANT_KB_LEXICAL_ENGINE=neo4j-lucene` to select this implementation;
an unavailable or mismatched generation fails visibly rather than silently
substituting SQLite. Search responses identify the lexical engine. The legacy
`bm25` hit field carries the selected engine's raw lexical score; new parity
receipts call that component `lexical_score`.

## FAISS vector backend

Python retrieval can use the same `IndexFlatIP` engine as RedAmon when the
optional `kb-ml` dependencies (including `faiss-cpu`) are installed. Select it
with `BB_ASSISTANT_KB_VECTOR_ENGINE=faiss`; the default remains
`sqlite-cosine`. FAISS mode builds one in-memory index per published snapshot,
model, dimension and supplemental-content policy, adding vectors in the pinned
RedAmon family/document/chunk order and bounded batches. It fails closed if the
vector snapshot is incomplete, the pinned query model or FAISS package is
unavailable, or the index cannot be built. Do not select it for deployment
until the active corpus has complete E5 vectors and parity receipts can be
generated. This setting affects the Python KB runtime and receipt exporter;
the Go runtime continues using its separately configured vector implementation.

## Pinned RedAmon runtime backend

The Go MCP server can delegate `bb_kb_search` and `bb_kb_stats` to the pinned
RedAmon `PentestKnowledgeBase` implementation directly, instead of reproducing
its ranking stages. Enable it with `BB_ASSISTANT_KB_BACKEND=redamon` and set:

```text
BB_ASSISTANT_KB_PYTHON=<RedAmon virtualenv Python>
BB_ASSISTANT_REDAMON_ROOT=<clean checkout at 4585b3442cf3c6e075fca5d3b512a8f47be501b6>
BB_ASSISTANT_REDAMON_KB_PATH=<active RedAmon knowledge_base/data directory>
NEO4J_URI=<RedAmon KB Neo4j URI>
NEO4J_USER=<read-only KB account>
NEO4J_PASSWORD=<protected service secret>
```

The worker verifies the pinned source revision, full-profile marker, required
retrieval settings, seven source families, and matching FAISS/Neo4j counts. It
forces offline model loading and fails closed; it never falls back to the local
SQLite retriever. This mode queries the RedAmon KB itself, so it proves the
query path is shared, not that a separate Graph Assistant corpus is an
identical copy. Use the parity receipt exporter and same-query comparison to
prove a local mirror. `bb_kb_get_source` still requires the local provenance
index and is unavailable in RedAmon-only mode.

Install the optional `kb-neo4j` dependency for Python administration. KB
publishing and application retrieval require a separate private endpoint in
`BB_ASSISTANT_KB_NEO4J_*`; they never fall back to the application's graph
`BB_ASSISTANT_NEO4J_*` settings. Keep the endpoint file owner-only. Publish an
offline, read-only SQLite snapshot with:

```sh
BB_ASSISTANT_KB_NEO4J_ENV_FILE=/private/kb/kb-neo4j.env \
  ./scripts/bb-assistant-kb --index-path /private/kb/index.sqlite3 publish-lucene --batch-size 200
```

Publishing first validates chunk text hashes, then imports only KB projection
nodes into generation-specific labels and core/all full-text indexes. It
re-reads every imported record, compares canonical content digests and counts,
and waits for both indexes to become online before marking the generation
ready. Search requires that generation's snapshot ID to equal the SQLite
snapshot ID; old generations cannot consume the new generation's limited
candidate pool. Supplemental raw Nuclei fallbacks and normally rendered cards
rejected by the pinned upstream YAML guards stay available through the explicit
all-content selection. The latter carry `redamon_excluded_reason`; default
lexical and vector candidates exclude both metadata classes. The original
documents and their text remain retained. This does not modify captured HTTP graphs.
Do not run the publisher against an actively written SQLite file; take a
consistent SQLite backup first. This migration has not yet selected Lucene in
the deployed application's environment or promoted the incomplete candidate.
Set `BB_ASSISTANT_KB_NEO4J_ENV_FILE` to the private KB endpoint file when
launching the application or publisher. The admin launcher loads it directly.
The application launcher also loads it. The default is
`$BB_ASSISTANT_DATA_HOME/kb-neo4j.env`; the file must define all four
`BB_ASSISTANT_KB_NEO4J_URI`, `USERNAME`, `PASSWORD`, and `DATABASE` values.
Use a genuinely separate Neo4j instance for staging and runtime KB reads. The
graph endpoint can remain on the application's existing `neo4j.env`.

The separate `scripts/compare-redamon-corpus` tool verifies a pinned RedAmon
Git source/config tree before executing its actual parsers and chunkers against
retained public source records. It reads the input SQLite file immutably and
uses a private temporary scratch database to compare ID, title, and content
digests without loading the full corpus into RAM. The `kb-reference` extra
supplies the upstream parser dependencies. For example:

```sh
./scripts/compare-redamon-corpus --index /private/kb/offline-copy.sqlite3 \
  --redamon-root /private/redamon-reference \
  --redamon-commit 4585b3442cf3c6e075fca5d3b512a8f47be501b6 \
  --output /private/kb/corpus-comparison.json
```

The output directory must already be private. A real transformation comparison
does not prove independent source-fetch completeness, a deployed RedAmon
snapshot match, complete E5 vectors, or full hybrid-query equivalence. Those
requirements still need their own observed evidence.

The comparison reports total retained chunks separately from
`core_local_chunks`. `CORE_MATCH_WITH_SUPPLEMENTAL` means that the core matches
the pinned transformations while additional material remains explicitly
excluded from default retrieval. It does not mean the entire retained corpus
is an identical RedAmon copy. Unexpected core IDs, changed title/content hashes,
or unavailable source evidence still fail the comparison. Within-document
duplicate IDs follow the pinned ingestion deduper's last-occurrence rule;
duplicate counts remain visible in the report.

## Receipt contract

Receipts use schema version 3; older receipts are rejected because they can
claim `profile=full` with an incomplete family set, omit retrieval-policy
settings, or pass with empty query results and arbitrary score fields. `PASS`
still compares supplied claims and does not authenticate the producer. The
full profile must contain exactly `tool_docs`, `gtfobins`, `lolbas`, `owasp`,
`exploitdb`, `nvd`, and `nuclei`. Every fixed query must return exactly
`top_k` unique results and every result must include the same four finite score
components: `lexical_score`, `vector_cosine`, `reranker_logit`, and
`combined_rank`. A result not contributed by one candidate channel records zero
for that channel's score. Query filters are restricted to applied
`source_names` filters.
Receipts are pinned to E5-large-v2 (revision and 1024 dimensions), BGE-reranker-base
(revision), the full-profile candidate/RRF/rerank/MMR settings and source boosts,
and a 1920-character limit per reranker input. A RedAmon receipt must also
identify its Neo4j Lucene and FAISS engines. The assistant receipt reports its
actual engines; an SQLite-backed receipt therefore fails parity against RedAmon.
Internal snapshot IDs and source-manifest digests are retained as provenance,
not compared across implementations. Parity compares `runtime_inventory`, a
canonical hash of each default-search chunk's stable ID, source family, title,
and text digest, plus per-family hashes and counts. This lets two systems prove
the same retrievable chunk corpus even when their private build manifests use
different formats.

The RedAmon exporter is [`scripts/export_redamon_kb_receipt.py`](../scripts/export_redamon_kb_receipt.py).
Run it from RedAmon's service environment only when the KB host is within its
thermal operating limit and the full RedAmon runtime is available. It refuses
any checkout other than the pinned commit, reads FAISS and Neo4j without
calling ingestion or schema-creation methods, requires their complete chunk-ID
sets to match, and runs the fixed query fixture against the configured hybrid
retriever. Querying may load the pinned E5 and BGE models if they are not cached;
therefore do not run this receipt step while the host is thermally constrained.
It writes a mode-0600 receipt. Keep Neo4j credentials in the process environment
or the service's protected environment file; do not put them on the command
line. The receipt is the RedAmon side of the comparison, not evidence by itself
that Graph Assistant is at parity.

Example, from a checkout containing this project and the pinned RedAmon tree:

```sh
PYTHONPATH=/path/to/graph-kb-assistant/src \
  /path/to/graph-kb-assistant/scripts/export_redamon_kb_receipt.py \
  --redamon-root /path/to/redamon-src \
  --kb-path /path/to/redamon-src/services/knowledge_base/data \
  --output /restricted/path/redamon-kb-receipt.json
```

Both files use the same shape; `producer` is respectively
`redamon` or `graph-kb-assistant`.

```json
{
  "schema_version": 3,
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
    "runtime_inventory": {
      "chunk_count": 209392,
      "sha256": "<sha256-of-canonical-runtime-chunk-rows>",
      "sources": [
        {"name": "tool_docs", "chunk_count": 1, "sha256": "<sha256>"}
      ]
    },
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
      "mmr_lambda": 0.65,
      "mmr_enabled": true,
      "source_boosts": {
        "tool_docs": 1.20, "gtfobins": 1.15, "lolbas": 1.15,
        "owasp": 1.05, "nuclei": 1.00, "nvd": 0.90, "exploitdb": 0.85
      },
      "reranker_enabled": true,
      "reranker_max_chars_per_side": 1920
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
      "score_components": {
        "lexical_score": 0.1,
        "vector_cosine": 0.2,
        "reranker_logit": 0.3,
        "combined_rank": 0.4
      }
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

Long E5 builds run on a dedicated build host, never on the analyst workstation. `bb-assistant-kb embed`
requires a host-identified, read-only thermal sensor source; the hwmon
directory mode resolves the uniquely labelled `coretemp` `Package id 0`
sensor on each check. It checks before each durable batch, fails closed on
unreadable sensor data, and stops at the configured package limit (currently
80 C on the i3-N305 build host).
The conservative invocation is `--batch-size 2 --threads 1`; the worker also
takes an exclusive process lock so a second embedding process cannot overlap.
The deployment unit is [`graph-kb-assistant-kb-embed.service`](../deploy/graph-kb-assistant-kb-embed.service)
and caps CPU at 40% of one core, disables automatic restarts, and is enabled for boot so
the same guarded job resumes after a host restart. The CLI still waits for the
ten-minute thermal gate before model loading. The embedding worker waits until the
package temperature has remained below 80 C for at least 10 continuous minutes.
A single cool reading does not satisfy this precondition.
Before enabling the unit, create the root-owned
`/etc/graph-kb-assistant/kb-embed.env` with
`BB_ASSISTANT_KB_INDEX=/var/lib/bb-graph-assistant/kb/candidates/<candidate-id>/index.sqlite3`.
The CLI refuses missing indexes, symlink targets, and any index outside that
candidate layout before opening a writable database; the live application
index is not an embedding destination.
Thermal stops preserve completed batches but do not publish the vector-ready
marker. `vector_ready` requires both complete snapshot-bound vectors and an
available NumPy search backend; `vector_storage_complete` reports the durable
generation state separately. Readiness remains false until every content
chunk has its pinned E5 vector, the completion marker is written, and the
runtime backend is available.
The embed CLI waits for a sampled ten-minute dwell below its configured limit
before starting and after a `ThermalLimitReached` batch stop, then resumes the
same staged job.
Unreadable sensors and all non-thermal errors fail closed without retry.
