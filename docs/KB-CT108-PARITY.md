# CT108 KB status and RedAmon parity

**Status: PARTIAL.** Snapshot and vector counts below come from the latest
operator-provided read-only stats; service and single-sample temperature state
come from the operator's PVE check. No claim below means the installed KB is
identical to RedAmon.

## Parity matrix

| Area | Last recorded local evidence | RedAmon comparison | State / gap |
|---|---|---|---|
| Sources | Read-only CT108 `bb_kb_stats` at 2026-10-26 16:17 UTC reports snapshot `ac9d1d2ac0fe0a3e80654180c81ac43f46a77f1e4481a7dc593fdfaed16ee049`, source coverage `DONE`, seven families, 207,878 documents, and 209,026 content chunks. Snapshot `source_manifest_sha256` is null; current manifest SHA-256 is `95336ab4a4f6109d93f515d9b465740e035b3bc277197926edbd167d617a0b10`. The source-manifest match is unknown until a new snapshot is built. | Family names and filters match RedAmon’s configured seven-family `full` profile, but no deployed RedAmon corpus receipt was supplied. | Prior local snapshot is present; current-source validity and cross-system corpus parity PARTIAL. |
| Rights and notices | Runtime snapshot contains ExploitDB coverage from its prior build. The current manifest now permits only the pinned GitLab repository’s allowlisted descriptions/metadata and preserves README/LICENSE; PoC files are never fetched. The active snapshot has no manifest digest and predates this policy record. | No deployed RedAmon corpus or rights receipt was supplied. | PARTIAL; current local source gate is scoped and enabled, but active snapshot provenance is stale/unknown. |
| Extraction and canonicalization | Snapshot exporter/importer validates manifest, content hashes, provenance, deterministic chunks and spans. Source providers include fixed archive/API adapters and explicit coverage states. | No same-revision source document export to compare. | PARTIAL; extraction equivalence is unproven. |
| Chunking | Current code has deterministic canonical chunk IDs and span validation; latest stats report 209,026 content chunks for the active snapshot. | RedAmon source/chunk fixture and canonical digests unavailable. | PARTIAL; boundary/hash parity is unproven. |
| Lexical search | Local index uses SQLite FTS5 with RedAmon-inspired query cleanup, title/body BM25 weighting and candidate filtering. | RedAmon uses Neo4j Lucene; analyzer and score parity are not established. | PARTIAL; engines are not interchangeable. |
| Embedding model | Configured E5 model is `intfloat/e5-large-v2`, 1,024 dimensions. | Same model family is declared in the parity contract; deployed model revision receipt unavailable. | PARTIAL; exact model revision and runtime equivalence need receipts. |
| Vector coverage | Read-only `bb_kb_stats` reports 38,888 E5 vectors of 209,026 content chunks; `vector_ready=false`, leaving 170,138 content chunks without vectors. The embed unit is inactive. | No deployed RedAmon vector-generation receipt to compare. | PARTIAL; vector coverage is incomplete and RedAmon vector parity is unproven. |
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
starting the worker. The checked-in ExploitDB policy is now scoped to the pinned metadata-only
feed; rebuild the active snapshot before treating its source coverage as current.

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

## Go service manifest-freshness reporting

The production `/mcp` server calls the Go `bb_kb_stats` implementation. Its
local branch now reports `source_manifest_sha256`,
`current_source_manifest_sha256`, and `source_manifest_matches_current` when
the pinned snapshot and checked-in `sources.yaml` are readable. Older indexes
fall back to the immutable snapshot manifest; unavailable hashes remain
`null`. The systemd templates configure the manifest path explicitly. This is
only a source-definition freshness signal: it does not establish source
coverage validity, vector completeness, or RedAmon parity. This version is deployed on CT108. A live read-only MCP call at 2026-10-26
16:17 UTC returned the current source hash above, but the old snapshot hash was
null, so the match remains unknown. This freshness signal does not establish
source coverage validity, vector completeness, or RedAmon parity.


## Latest coordinator check — 2026-10-26 16:14–16:18 UTC

Authenticated read-only SSH to PVE/CT108 succeeded. The deployed application
reports `0.1.0-go-dev+b1541985fc49`; `/api/health` returned `ok`; an MCP
initialize and `bb_kb_stats` call both succeeded. The current manifest was
copied to CT108 and its SHA-256 matched the local file above. The active DB
still reports the older snapshot and null snapshot-manifest hash. Its full
Python stats report 209,036 stored chunks including notice records; the Go
API’s 209,026 is the content-chunk count used for vector readiness. The embed
service remains inactive. `coretemp` Package id 0 ranged from 74–85 C in subsequent checks, above
the 70 C worker stop threshold. At 16:20 UTC, PVE load average was 3.71 and
the KVM process for VM 112 (`agent-hub`) showed about 60% CPU in a point
sample; the Graph Assistant embedding unit was inactive. No source rebuild or
embedding was started. That unrelated VM was left untouched. CT108 has 89 GiB free on its data filesystem. No RedAmon runtime or
reference receipt was available from PVE.

## Coordinator deployment check — 2026-10-26 16:32 UTC

CI run `40358810695` completed successfully for PR head
`625eb0fde07ac011f98efb0e86a992eb9b9db142`. The verified Linux artifact was
built from the PR merge revision `d0efdb63e31f`; its SHA-256 matched the
workflow checksum before and after transfer to CT108. The binary is installed
as `/usr/local/bin/bb-assistant`, and its reported version is
`0.1.0-go-dev+d0efdb63e31f`. The previous binary and systemd unit were backed
up before replacement. The only unit difference was the explicit
`BB_ASSISTANT_KB_PYTHON` path to the existing pinned-model virtual environment.
`systemd-analyze verify` passed, the app service is active, and
`/api/health` returned `ok` with the new version. The KB embed service remains
inactive.

A fresh read-only `bb_kb_stats` MCP call against CT108 confirmed the active
snapshot is still `ac9d1d2ac0fe0a3e80654180c81ac43f46a77f1e4481a7dc593fdfaed16ee049`:
7 families, 207,878 documents, 209,026 content chunks, and 38,888 embedded
content chunks (18.6%). `vector_ready=false`; the snapshot manifest hash is
still null while the current source-manifest hash is
`95336ab4a4f6109d93f515d9b465740e035b3bc277197926edbd167d617a0b10`. The
active ToolDocs coverage remains 434 chunks versus 355 expected from the
pinned RedAmon-derived fixture. The snapshot therefore needs a controlled
source rebuild before vector completion can be meaningful.

The pinned E5 and BGE model cache directories and Python package are present.
An idle model-worker import/exit check passed without loading either model or
running inference. At 16:32 UTC the PVE package sensor reported 69 C once;
this is not the required continuous ten-minute below-70 C window. No source
rebuild or embedding was started, and the vector count is unchanged. The
current blockers are the stale active snapshot, incomplete vectors, absent
RedAmon query receipts, and the unverified thermal dwell.

The pinned RedAmon source checkout was checked directly before deciding the
rebuild filters. Its `full` profile includes the same seven families; its
`kb-build-full`/`kb-rebuild-full` targets set NVD lookback to 730 days, and the
RedAmon NVD client deliberately ignores the CVSS cutoff for the `full`
profile. The local manifest's 730-day, 0.0 CVSS filter is therefore correct
for that profile. The pinned E5/BGE model revisions, top-k, overfetch, RRF,
reranker pool, MMR lambda, and source boosts also match the declared RedAmon
settings. This is configuration parity only; it does not prove the active
RedAmon snapshot's contents or exact search ordering.

Before a source rebuild, the two CT108 Python files that write/report snapshot
manifest hashes were updated from the CI-passing branch files with backups.
Hashes now match for all 21 tracked `src/bb_assistant/kb` files. This ensures
the next imported snapshot records the current source-manifest hash instead of
leaving it null.
