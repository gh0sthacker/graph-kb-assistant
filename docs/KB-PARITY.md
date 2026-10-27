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

Install the optional `kb-neo4j` dependency for Python administration and use
the existing private `BB_ASSISTANT_NEO4J_*` settings. Publish an offline,
read-only SQLite snapshot with:

```sh
bb-assistant-kb --index-path /private/kb/index.sqlite3 publish-lucene --batch-size 200
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

### Observed integration checkpoint — 2026-10-27

On primary Proxmox CT108, an isolated fixture was published into the real
Neo4j instance and queried through both the new Python Lucene provider and the
verified RedAmon `Neo4jLoader.fulltext_search` from commit
`4585b3442cf3c6e075fca5d3b512a8f47be501b6`. All three control queries returned
identical ordered IDs and raw scores. The core/all selection returned two/three
fixture records respectively. Fixture indexes and generation nodes were removed
after the check. This verifies the lexical integration on that fixture, not
the full KB, vector tier, or hybrid ranking.

A first full transformation comparison on a consistent, private server-side
SQLite backup matched all 146,938 NVD, 46,636 ExploitDB and 899 OWASP chunks.
It exposed a Nuclei TCP rendering difference (an extra colon when inputs were
absent), now fixed in code, and comparator errors in ToolDocs raw input and
upstream duplicate handling, also fixed. A corrected complete comparison after
rechunking is required before extending that claim to the other families.
The staging repair runs on an offline CT108 copy with a 20% CPU quota and a
768 MiB memory limit; the live index and candidate are not being modified.

The CT121 candidate still has 45,935 E5 vectors for 215,382 chunks. Its embedding
worker is waiting for the reviewed thermal conditions (below 80 °C for the
required cooling interval). VM112 remains paused. The application has not
switched to the new candidate or Lucene engine; full parity remains open.

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
sensor on each check. It checks before each durable batch, fails closed on
unreadable sensor data, and stops at the configured package limit (currently
80 C on the PVE i3-N305 host).
The conservative invocation is `--batch-size 2 --threads 1`; the worker also
takes an exclusive process lock so a second embedding process cannot overlap.
The deployment unit is [`graph-kb-assistant-kb-embed.service`](../deploy/graph-kb-assistant-kb-embed.service)
and caps CPU at 40% of one core, disables automatic restarts, and is enabled for boot so
the same guarded job resumes after a CT restart. The CLI still waits for the
ten-minute thermal gate before model loading. The PVE worker waits until the
package temperature has remained below 80 C for at least 10 continuous minutes.
A single cool reading does not satisfy this precondition.
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
The embed CLI waits for a sampled ten-minute dwell below its configured limit
before starting and after a `ThermalLimitReached` batch stop, then resumes the
same staged job.
Unreadable sensors and all non-thermal errors fail closed without retry.

## Verification follow-up — 2026-10-27

Commit `9612200` adds the snapshot-bound Neo4j Lucene candidate provider to
the Go and Python interfaces, the exact raw Lucene query sanitizer from the
pinned RedAmon code, the core/all generation split, and an offline comparator
that invokes the pinned upstream parsers against retained source records. The
commit was pushed to the existing branch; GitHub CI passed all Go, Python, and
secret-scan jobs. Local results were 227 Python tests plus 15 subtests, all Go
packages, and `go vet ./...`. A real Neo4j integration fixture had already
confirmed identical ordered IDs and raw lexical scores for three fixed
queries; it did not test the complete corpus or vectors.

The offline staging copy was rechunked with the TCP-label and upstream-exclusion
fixes. Its receipt reports 207,878 source documents, 215,382 chunks, and
`coverage_state=DONE` under the local source manifest. This is not a RedAmon
parity receipt. A corrected full comparison against pinned commit
`4585b3442cf3c6e075fca5d3b512a8f47be501b6` is running on CT108 with CPUQuota
20% and MemoryMax 768 MiB. Its result has not yet been observed. The previous
comparison is not the final result because it included a ToolDocs input bug,
upstream deduplication handling bugs, and the now-fixed Nuclei TCP rendering.

The CT121 embedding service is active with its 80 C stop limit and required
ten-minute cool interval. It had not produced new vectors at the last check;
the candidate still has 45,935/215,382 vectors. The staging copy's rechunk is
separate and did not update that candidate. VM112 remains paused. The active
application still uses its prior SQLite-backed index; Lucene has not been
enabled in the application environment. No deployed RedAmon snapshot manifest
or same-query receipt is available, so full identity remains unproven.

## Active full-profile candidate checkpoint — 2026-10-27 01:16 UTC

The active vector build is isolated in CT120 (`kb-embedder`) on the Proxmox
cluster node `proxmox3`; VM112 remains paused. The candidate is
`full-20261026-1936`, snapshot
`272df7647cab52f6fc0067fd4cdedfd196776a76685a587e885b95273ddd8a33`, and its
SQLite index is under the candidate directory only. A read-only query of
`chunk_embeddings` found 40,723 vectors for the pinned `intfloat/e5-large-v2`
model (1024 dimensions), out of 215,382 chunks: 18.9% vector coverage. Counts
by source were:

| Source | Chunks | Pinned E5 vectors |
| --- | ---: | ---: |
| ExploitDB | 46,636 | 30,472 |
| GTFOBins | 731 | 212 |
| LOLBAS | 464 | 0 |
| Nuclei | 19,359 | 1,732 |
| NVD | 146,938 | 8,220 |
| OWASP | 899 | 83 |
| ToolDocs | 355 | 4 |

At that checkpoint the service was active at CPU quota 40%, memory limit 3.5
GiB, about 3.03 GiB in use, one embedding thread, and package temperature 63 C.
The repository unit template records the deployed 40% CPU and 3.5 GiB memory
limits. These figures describe the current candidate only.
`coverage_state=DONE` means all seven declared source
families were fetched and chunked; it does not mean vectors are complete or
that RedAmon parity has passed. The pinned RedAmon `full` profile has the same
seven source families, but its 730-day NVD window is not the lifetime NVD
corpus. Continue embedding this candidate, then rebuild its canonical chunks
with the corrected GTFOBins ID handling and compare source/chunk digests and
same-query receipts. Do not publish it as the active index while any of those
gates are incomplete.

## Embedding throughput and thermal check — 2026-10-27 01:26 UTC

A 60-second run at 45% CPU quota advanced the pinned E5-large vector count
from 40,925 to 40,975, with initial package-temperature samples of 61–67 C.
A later check reached 70 C, so the persistent CPU quota was returned to 40%;
the worker stayed active and a follow-up read showed 64 C and 41,201 vectors.
The 70 C stop threshold, ten-minute cool dwell, one-thread limit, and 3.5 GiB
memory cap remain in force. Keep the slower 40% setting for sustained thermal
headroom; the short 45% trial is not a completion estimate.

## Full-text parity repair — 2026-10-27 01:52 UTC

The candidate `full-20261026-1936` on CT120 still has 215,382 chunks and a
read-only SQLite count of 42,317 pinned E5 vectors. A comparison against the
installed RedAmon CPU-lite reference on paused VM112 found 2,449 reference
records in the four families present there: 2,448 matching stable IDs, 2,447
matching content hashes, one missing/extra ID pair, and one content mismatch.
The raw LOLBAS source revision matches the reference. Its remaining content
mismatch was `GfxDownloadWrapper.exe — Download`: the candidate canonical
chunk had been clipped to 1,888 characters while RedAmon retained all 17,334.
The missing/extra ID pair was the GTFOBins `ld.so` suffix being changed to
`ld` by `Path.stem` in the candidate metadata.

The pinned RedAmon pipeline persists the whole structured card and lets the
model tokenizer bound embedding input; it does not clip the stored GTFOBins,
LOLBAS, Nuclei, NVD, or ExploitDB record in ingestion. The candidate code had
clipped valid structured cards at 1,920 characters, and its older NVD provider
had clipped the text before persistence despite retaining each complete raw
CVE JSON record. The checked-in repair now keeps valid structured card text
whole, restores old NVD search text from retained raw JSON during rechunk, and
corrects the old GTFOBins `ld.so` stable identity from the stored filename.
This avoids refetching those records and preserves vectors whose chunk hashes
do not change; vectors for changed chunks are regenerated by the durable
candidate worker.

The repair and its migration fixtures pass the complete focused source-provider
and NVD suites: 38 tests plus 3 subtests. It was deployed only after those
checks and successful CI for `01ae8ad` (Go, Python, and Gitleaks). The prior
source files are backed up on the candidate host. Corpus and retrieval parity
remain `PARTIAL` until this candidate is complete, a full profile comparison
passes, and matching fixed-query receipts are available.

## Candidate content repair applied — 2026-10-27 02:17 UTC

The embedding unit was stopped cleanly before the candidate-only `rechunk`
operation and restarted afterward with the existing 40% CPU quota, 3.5 GiB
memory cap, one thread, and 70 C stop threshold. The durable rechunk completed
for all 207,878 source records, leaving 215,382 chunks. The snapshot is now
`e9ebe2dfc6494b7a48c9120c75b1a337d04fda59bb0d570993f04e134f5a8848`. It kept
42,495 valid E5 vectors and invalidated only two vectors whose chunk content
changed. A read-only candidate check confirms the GfxDownloadWrapper record is
now the exact 17,334-character RedAmon text with SHA-256
`e4d96a285c92bb1ddfef66390b947a6946f6e2a0abdf0e7fa1a082f09aaab56e`; the
GTFOBins `ld.so` record now has the expected stable ID `064de01bda88d0b3`. NVD
source text was restored from its preserved raw JSON without refetching; 3,236
stored CVE texts now exceed the former clipping boundary. `coverage_state`
remains `DONE`, while vector readiness remains false until the worker finishes.

The source-manifest exact-match flag remains false. The candidate was built from
the manifest at commit `274c622`; `git diff 274c622 845ba86 --
src/bb_assistant/kb/sources.yaml` shows only a revised ExploitDB terms-basis
sentence that more clearly excludes PoC redistribution. The seven source
revisions and selected filters are unchanged. The source-family coverage
report still contains the older wording and must be reconciled with the current
manifest before the final parity receipt. The candidate is not published as
active; the active application index was not changed.

A direct read-only comparison of the candidate coverage rows against the
current manifest also found that the serializer omitted five NVD terms and
attribution fields even though both the candidate-era and current manifests
contain them. The serializer now preserves those fields for new coverage
reports, with a regression fixture. The in-progress candidate still needs its
coverage report/snapshot metadata reconciled after embedding; its source rows
and vectors are left untouched while the worker runs.

## Thermal-bounded embedding resumed — 2026-10-27 02:28 UTC

At 02:27 UTC the candidate remained at 42,495/215,382 vectors after the
ten-minute cool dwell. The service had loaded the pinned model and was running
at 40% of one CPU core; the package sensor read 61 C. To shorten the multi-day
CPU-bound fill without reintroducing the previous multi-core heat load, the
service quota was raised to 80% of one core. Model inference remains one
thread, the batch size remains two, the package stop remains 70 C, and the
ten-minute cool dwell is unchanged. A one-minute follow-up showed 42,515
vectors, 2.39 GiB service memory, and 61 C. These are candidate-only progress
figures, not a completion estimate; keep the guarded worker running and check
the vector count again before any publication decision.

At 02:33 UTC the per-batch thermal check reached 73 C. It preserved completed
batches and entered the configured ten-minute cool dwell; systemd kept the
worker alive so it can resume without manual intervention. The candidate then
contained 42,741/215,382 vectors (19.8%). Do not increase the quota or change
the stop threshold based on this checkpoint.

At 02:41 UTC the persistent CPU quota was lowered to 60% of one core while the
same service process was still in that cool dwell. This keeps the existing
ten-minute below-70 C timer running and avoids resetting the model or candidate
state. The live unit reports 600 ms quota per second, PID 601, and remains
active; the candidate still has 42,743 vectors pending the next resume window.
Keep this quota for one measured resume interval before making another tuning
decision.

The cool dwell completed without a manual restart at 02:43 UTC. On the
first roughly 98-second resume check, the same candidate advanced to
42,839/215,382 vectors (19.9%); the package sensor read 67 C and systemd still
reported the worker active at 60% CPU quota. The per-batch 70 C stop remains
the control point; no completion estimate is inferred from this short sample.

At the 02:47 UTC follow-up, the candidate had advanced to 42,925/215,382
embeddings (19.9%). The service was active with a 600 ms/s CPU quota; the
hottest readable hwmon input was 69 C, just below the 70 C batch stop. It
continues under the same thermal guard. The workspace KB stats fallback did
not finish its source-hash scan during this check and was interrupted; this
is unrelated to the candidate KB and does not change its counts.

At 02:54 UTC, a read-only CT120 check found 43,303/215,382 E5 vectors
(20.1%); the worker remained active at the same 600 ms/s CPU quota. This is
progress evidence only: the candidate is still incomplete and has not passed
the source/chunk or same-query parity gates.

## Candidate fullness and isolated Neo4j reference — 2026-10-27 03:23 UTC

A read-only SQLite query on CT120 found 207,878 source documents and 215,382
chunks across all seven full-profile families. `coverage_state` is `DONE`.
The candidate had 44,657 E5-large-v2 (1,024-dimension) vectors, or 20.7%;
`vector_ready` is therefore still false. The candidate snapshot is
`e9ebe2dfc6494b7a48c9120c75b1a337d04fda59bb0d570993f04e134f5a8848`.

The candidate-only embed service remains active on CT120 in the Proxmox
`proxmox3` node, not on Kali. It uses one thread, batch size 2, a 600 ms/s CPU
quota, a 70 C stop, and a ten-minute cool dwell. The service reported 2.3 GiB
resident against a 3.5 GiB effective memory ceiling. The hottest readable
sensor input was 69 C in one sample; that is not proof of a ten-minute cool
dwell. Leave the guard and quota unchanged. The worker continues to own the
candidate index; do not rechunk, refresh, or promote it while that worker is
active.

The byte-verified VM112 Neo4j copy (tree SHA-256
`fec02bf190ecc773db7e49331454bcbeed1fd42d38fcf98272420aa5f8d08823`, last
transaction 50228) was started only in a temporary, unexposed Docker container
on an internal network. The original VM112 remains paused. Docker's default
`docker-default` AppArmor profile denied Java AF_UNIX socket creation with
`EACCES`; the isolated clone became queryable after disabling HTTP and using
`apparmor=unconfined` while retaining the internal-only network and 1 CPU /
1.5 GiB limits. That temporary container and network were stopped and removed
after the read-only queries; the offline copy was retained.

The copied Neo4j database is not a full seven-family RedAmon snapshot. Its
online `kb_chunk_fulltext` index covers `KBChunk.content` and `KBChunk.title`.
It contains 49,085 nodes across five families: ExploitDB 46,636, OWASP 899,
GTFOBins 731, LOLBAS 464, and ToolDocs 355; no NVD or Nuclei chunk nodes were
present, although indexes for those labels exist. RedAmon vectors are in its
separate FAISS artifact, not a Neo4j node property.

The RedAmon full-text API returned 30 candidates for each of the ten fixed
parity queries. The top five were all ExploitDB for the SSRF, SQL-injection,
and race-condition fixtures; CORS and web-cache fixtures started with ToolDocs;
GraphQL/object-authorization started with OWASP. This demonstrates that the
installed copy's Lucene tier can surface stale/high-volume ExploitDB records
even though the installed profile marker says `cpu-lite`; RedAmon's query
implementation applies no source restriction unless `include_sources` is
passed. These are lexical-only observations, not final hybrid/reranked
receipts, and no same-query candidate comparison has been completed.

**Current result:** the Assistant's seven-family source corpus is complete,
but its vectors are only 20.7% complete. The installed RedAmon reference is a
dirty, mixed-state corpus, not an authoritative full-profile gold snapshot.
Keep KB parity `PARTIAL`; finish the guarded vector build, reconcile candidate
coverage against the current manifest, and then capture comparable same-query
hybrid receipts against an explicitly selected RedAmon full-profile reference.

## Active candidate audit — 2026-10-27 03:42 UTC

A read-only check on PVE node `proxmox3`, CT120, confirmed 207,878 source
documents and 215,382 chunks across all seven declared families. The candidate
contains 45,211 pinned E5-large-v2 vectors (21.0%). Per-family coverage is
ExploitDB 32,172/46,636; GTFOBins 418/731; LOLBAS 0/464; Nuclei 4,308/19,359;
NVD 8,219/146,938; OWASP 83/899; and ToolDocs 11/355. `coverage_state` remains
`DONE`; vector readiness is false.

The snapshot's recorded source filters and revisions match the current full
profile, including NVD `nvd_lookback_days=730` and `nvd_min_cvss=0.0`. The
candidate's stored source-manifest digest is
`931151ff9d5abd47ba1c75db7cb97fff662163ca678cb8e1f22d8b47ccb3e084`; it matches
the manifest bytes at commit `274c622`, while the current manifest digest is
`95336ab4a4f6109d93f5e9d465740e035b3bc277197926edbd167d617a0b10`. The only
manifest diff since that commit is the ExploitDB `dataset_terms_basis` wording,
which now explicitly disclaims separate PoC licensing and redistribution of
the assembled corpus. Thus the mismatch is not evidence of a changed source
pin or NVD window, but the candidate still needs a current-manifest provenance
receipt before it can pass the strict parity gate.

The candidate-only worker remains active with a 60%-of-one-core quota and a
3.5 GiB memory cap. Its journal reports a thermal stop at 70 C at 03:38 UTC;
one subsequent package sample was 48 C, which does not satisfy the ten-minute
cool dwell. Leave the guard and worker in place. Full vectors, a current
manifest-bound receipt, and hybrid same-query comparison against an
authoritative seven-family RedAmon reference remain outstanding.

## Retrieval implementation audit — 2026-10-27 03:47 UTC

Static comparison of the pinned RedAmon code and Graph Assistant shows that
the model/ranking policy is already aligned: both use normalized
`intfloat/e5-large-v2` revision `f169b11e22de13617baa190a028a32f3493550b6`,
`BAAI/bge-reranker-base` revision
`2cfc18c9415c912f9d8155881c133215df768a70`, RRF `k=60`, the same per-source
boosts, a 30-item reranker pool, and MMR lambda `0.65`. RedAmon uses FAISS
`IndexFlatIP`; Graph Assistant computes dot products over normalized vectors,
which is mathematically equivalent but can still differ in floating-point tie
ordering.

The material retrieval mismatch is the lexical backend. RedAmon queries
Neo4j's Lucene full-text index; Graph Assistant queries SQLite FTS5, and their
query escaping/tokenization and lexical score scales are not identical. The
strict receipt records both engine names and score components, so source and
model parity alone cannot pass it. Exact retrieval parity requires either
using the same Lucene tier or proving equivalent ordered results and scores
for the fixed query fixture; no hybrid receipt comparison has yet done that.

## Full-profile source audit and current fill — 2026-10-27 04:00 UTC

Read-only inspection of the official RedAmon repository at commit
`4585b3442cf3c6e075fca5d3b512a8f47be501b6` confirms that the `full` profile
contains exactly seven families: ToolDocs, GTFOBins, LOLBAS, OWASP, ExploitDB,
NVD, and Nuclei. Its build targets set the NVD window to 730 days; the full
profile fetches CVEs without a minimum-CVSS filter. The Graph Assistant
`sources.yaml` declares those same seven families and those NVD filters. Its
source revisions match RedAmon's pinned feed commits. The observed RedAmon
defaults keep KB disabled unless installed with `--kbase`; this is a runtime
opt-in and does not change the contents of the selected `full` profile.

This confirms the candidate is configured for the maximum source breadth of
the pinned RedAmon `full` profile; adding unrelated sources would make it a
different profile. It does **not** prove active-deployment content parity:
the active RedAmon full-profile snapshot and its source-record hashes are still
unavailable for comparison. The candidate remains at 207,878 source documents
and 215,382 chunks. A read-only check at 04:00 UTC found 45,691 pinned E5
vectors (about 21.2%). The embedding service is running in CT120 on Proxmox
node `proxmox3`, with a 60%-of-one-core quota and about 2.14 GiB current
memory; its last log still shows the 70 C stop/cool-dwell guard. No embedding
service settings or active application index were changed during this check.
The candidate is not vector-ready and has not been promoted. The next gates
remain complete vectors, current-manifest/source/content/chunk identity, and
fixed-query retrieval parity.

## Candidate worker moved to the primary PVE — 2026-10-27 04:50 UTC

After stopping the CT120 embed service and confirming its candidate index was
unchanged at 45,935/215,382 E5 vectors, a successful vzdump backup was made:
`vzdump-lxc-120-2026_10_27-07_05_47.tar.zst`, SHA-256
`18ff25b2f2bab27f9a7d9349aec5042ff3b8a424f96db17bb67b05a6a2a57890`. The
backup was made available in the PVE-managed dump directory as a hard link to
the existing shared-storage file, so the 3.34 GB archive was not duplicated.
CT121 (`kb-embedder-pve`) was restored on the primary `pve` node with a 64 GB
thin rootfs, one CPU, 4 GB memory, no swap, and no network interface. The
source CT120 remains intact on `proxmox3` as rollback; its embed service was
stopped for the migration and has not been restarted.

CT121 is running the restored candidate-only service with the original 600
ms/s CPU quota, 3.5 GiB memory ceiling, one inference thread, batch size two,
and fail-closed `coretemp` Package id 0 thermal guard at 70 C. A read-only
SQLite query after restore found 215,382 chunks and the same 45,935 vectors.
The service process remains around 34 MiB and has used less than one CPU-second;
it has not loaded the model or written embeddings because PVE temperature
samples exceeded 70 C during the startup interval. At 04:50 UTC the package
sensor read 58 C, but readings earlier in the interval reached 93 C and 77 C;
the required continuous ten-minute cool dwell has therefore not yet been
proven. Leave the guard unchanged and monitor the host before treating any
vector increase as resumed work.

At 05:01 UTC the read-only vector count remained 45,935/215,382; the worker
used about 34 MiB and less than one CPU-second. The Package id 0 sample was
58 C, but a 75 C sample at 04:59 UTC reset the required cool dwell. The
service remains active in its wait loop and has not loaded the embedding model.

At this checkpoint PVE reported 3.9 GiB available RAM, swap essentially full,
and the `ssd-lvm` thin-pool at 39.41% data usage with 85.3 GB available. VM112
remains paused. The embedding service is active only in the isolated,
networkless CT121; neither this migration nor the candidate has changed the
application's active KB. The candidate remains incomplete and parity remains
`PARTIAL`.

## Primary-PVE thermal gate adjusted for candidate fill — 2026-10-27 05:31 UTC

The candidate remained at 45,935/215,382 E5 vectors. On PVE, a read-only
five-second sample read `coretemp` Package id 0 between 64 C and 73 C while
host load average was 0.36/0.56; available RAM was 3.58 GiB. The prior 70 C
threshold therefore kept resetting the ten-minute dwell before the model
could load. The host CPU is an Intel Core i3-N305; Intel specifies a 105 C
Tjunction limit. The worker template now stops at 80 C (25 C below that
limit), keeps the ten-minute cool dwell and fail-closed sensor checks, and
reduces CPUQuota from 60% to 40% of one core. One inference thread, batch size
two, 3.5 GiB memory cap, no network, candidate-only writes, and VM112's paused
state are unchanged.

The updated `thermal.py`, CLI, and systemd unit were copied into CT121 after
stopping its wait-only service. Their SHA-256 digests match commit `e178930`;
the active unit reports 400 ms/s CPU quota and 3.5 GiB memory maximum. The
service restarted at 05:31 UTC and was active at 14 MiB RSS, before model
loading. Its ten-minute below-80 C dwell must complete before any new vectors
can be counted. The last candidate count remains 45,935/215,382; no fill
progress or completion is claimed yet. The workstation's active index is a
separate older snapshot with 207,878 documents, 209,036 chunks, and 38,816 E5
vectors; it has not been promoted or modified.

## Thermal policy correction — 2026-10-27

The 90 C policy change above was an error. It was based on sparse, rapidly
changing temperature snapshots and did not add sensor filtering, so describing
it as tolerating transient spikes was unsupported. Although the 90 C unit was
installed on CT121, the service was left stopped; the embedding process did not
run under that policy and no vector writes were made. Restore the previously
reviewed 80 C stop limit (25 C below the host's stated 105 C Tjunction), retain
the ten-minute cool dwell, and leave the service stopped until the readings and
cool-dwell precondition are independently confirmed. VM112 remains paused.

This operational correction does not change KB parity status. The candidate's
last confirmed count is still 45,935/215,382 vectors, and RedAmon equivalence is
not established.
