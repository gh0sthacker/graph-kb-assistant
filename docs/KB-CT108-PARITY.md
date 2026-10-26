# CT108 KB status and RedAmon parity

**Status: PARTIAL.** Snapshot and vector counts below come from the latest
operator-provided read-only stats; service and single-sample temperature state
come from the operator's PVE check. No claim below means the installed KB is
identical to RedAmon.

## Parity matrix

| Area | Last recorded local evidence | RedAmon comparison | State / gap |
|---|---|---|---|
| Sources | Latest read-only Hermes `bb_kb_stats` reports snapshot `ac9d1d2ac0fe0a3e80654180c81ac43f46a77f1e4481a7dc593fdfaed16ee049`, source coverage `DONE`, seven families, 207,878 documents, and 209,026 content chunks. | Family names and filters are represented in `src/bb_assistant/kb/sources.yaml`; no deployed RedAmon snapshot manifest was supplied. | Local declared source coverage DONE; cross-system corpus parity PARTIAL. |
| Rights and notices | Runtime source coverage reports ExploitDB with 46,638 documents and `license_status=confirmed_at_pinned_revision`. The checked-in manifest instead has `dataset_terms=unresolved_pending_authoritative_dataset_terms_review` and `license_status=review_required`; `redamon_sources.py` treats both as blockers. Index search also excludes ExploitDB. This is a runtime snapshot/manifest inconsistency requiring reconciliation. | No authoritative deployed RedAmon rights receipt was supplied. | PARTIAL; runtime coverage DONE does not override the unresolved local manifest rights gate. |
| Extraction and canonicalization | Snapshot exporter/importer validates manifest, content hashes, provenance, deterministic chunks and spans. Source providers include fixed archive/API adapters and explicit coverage states. | No same-revision source document export to compare. | PARTIAL; extraction equivalence is unproven. |
| Chunking | Current code has deterministic canonical chunk IDs and span validation; latest stats report 209,026 content chunks for the active snapshot. | RedAmon source/chunk fixture and canonical digests unavailable. | PARTIAL; boundary/hash parity is unproven. |
| Lexical search | Local index uses SQLite FTS5 with RedAmon-inspired query cleanup, title/body BM25 weighting and candidate filtering. | RedAmon uses Neo4j Lucene; analyzer and score parity are not established. | PARTIAL; engines are not interchangeable. |
| Embedding model | Configured E5 model is `intfloat/e5-large-v2`, 1,024 dimensions. | Same model family is declared in the parity contract; deployed model revision receipt unavailable. | PARTIAL; exact model revision and runtime equivalence need receipts. |
| Vector coverage | Latest read-only Hermes `bb_kb_stats` reports 38,888 E5 vectors of 209,026 content chunks; `vector_ready=false`. The exact unembedded remainder is 170,138 chunks. | No deployed RedAmon vector-generation receipt to compare. | PARTIAL; vector coverage is incomplete and RedAmon vector parity is unproven. |
| Query behavior | Local retrieval combines lexical/vector candidates, reciprocal-rank fusion, BGE reranking and MMR; fixed query fixtures are checked in at `references/kb-parity-queries.json`. | No RedAmon query-result receipt for those fixture queries is present. | PARTIAL; no ranking or score parity claim. |
| Hermes API integration | The latest read-only `bb_kb_stats` call over Hermes SSH MCP succeeded. | No fixed-query search receipt or live model-inference check was supplied with this stats call. | PARTIAL; stats integration is observed, query and model paths remain unverified. |

## Safe embed configuration

The checked-in CT108 unit template at `deploy/graph-kb-assistant-kb-embed.service`
specifies one PyTorch thread, one BLAS/OMP thread, batch size 2, a 20% CPU quota,
no automatic restart, and a fail-closed `coretemp` Package id 0 sensor check
before every durable batch. The stop threshold is 70 C. The batch size and
worker lock preserve completed batches. The CLI waits for ten sampled
continuous minutes below 70 C before its first batch. A thermal stop leaves the
vector staging generation intact, waits for the same dwell, then resumes the
same process; non-thermal errors still terminate the service.

Do not start the worker until a fresh Proxmox temperature observation confirms
the package remains below 70 C for at least 10 continuous minutes. If a batch
check reaches 70 C, the CLI waits and resumes automatically after the cool
dwell. If the sensor cannot be read or another non-thermal error occurs, the
service exits nonzero and does not retry. This report does not establish that
the host currently meets the temperature condition.

## Latest operator check — 2026-10-26 11:51–12:02 UTC

The operator reports from an active root session on PVE that CT108's app
service is active, `/api/health` returned 200, and the embed service is
inactive. One-minute PVE package readings from 11:51:36 through 12:00:36 UTC
were 61, 62, 62, 62, 61, 65, 91, 62, 65, and 63 C. A subsequent check at
12:01:42 was 64 C. The 91 C reading breaks the required below-70 C dwell, so
no embedding service was started. The package sensor is visible inside CT108
and readable by its `max` service user. One CT read returned 84 C while a
PVE-side read shortly afterward returned 63 C; immediate reads of the same
sysfs path then matched at 61 C. A later read-only dwell attempt from the
PVE session started at 12:08:58 UTC and stopped on its second sample at 85 C.
Embedding remained inactive. Treat thermal dwell as unverified until a fresh,
consistent ten-minute series is observed. The same operator supplied the fresh
read-only Hermes stats summarized in the matrix; no RedAmon receipt was
available.

## Earlier coordinator read attempt — 2026-10-26

This run could not authenticate to Proxmox/CT108 over SSH (public-key access
was denied for both configured host aliases). The workspace `kb_stats` tool
also did not return within the bounded wait and was cancelled in this
coordinator session. No remote unit, database, or snapshot files were read or changed by this coordinator, and no
embedding, source fetch, database runtime, or target operation was started.
The separate operator stats now establish the active snapshot ID and vector
counts. A later live PVE session deployed the thermal-safe CLI and unit; the
unit is confirmed inactive and disabled, and a point sensor reading matched
between PVE and CT108. A fresh ten-minute cool dwell is still required before
starting the worker. Reconcile the ExploitDB source-coverage metadata with the
checked-in manifest and its rights gate before promoting source coverage as
policy-valid.

## Evidence required to close parity

1. A fresh, read-only CT108 snapshot/index report with matching snapshot ID,
   source coverage report, exact content-chunk count, vector count, model
   revision, dimensions, and readiness state.
2. A pinned RedAmon full-profile receipt covering source/document/chunk hashes,
   licenses/notices, model revision and vector generation.
3. Search receipts from both implementations for every query in
   `references/kb-parity-queries.json`, including ordered chunk IDs and score
   components.
4. A separately recorded authenticated read-only Hermes stats/search check
   against CT108 after the candidate index is complete.

Until all four are available and matching, report the corpus and retrieval
parity as `PARTIAL`.
