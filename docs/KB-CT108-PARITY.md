# CT108 KB status and RedAmon parity

**Status: PARTIAL.** Snapshot and vector counts below come from the latest
operator-provided read-only stats; service and single-sample temperature state
come from the operator's PVE check. No claim below means the installed KB is
identical to RedAmon.

## Coordinator verification — 2026-10-26 22:53 UTC

- Installed the checksum-verified Go CI artifact for PR head `3c19db6`
  (artifact version `0.1.0-go-dev+348adf896e38`) on CT108, retaining the
  previous binary at `/usr/local/bin/bb-assistant.backup-44c6468`. Restarted
  `graph-kb-assistant.service`; `/api/health` returned `status=ok` and the new
  version.
- Updated the four KB Python runtime files (`cli.py`, `index.py`, `ranking.py`,
  and `tools.py`) from pinned commit `3c19db60b371e5650f8253b49fe16825905aa117`.
  Their SHA-256 values match the clean local checkout; `py_compile` succeeded.
  The source backup is under the private data directory
  `deploy-backups/python-pre-3c19db`.
- Read-only SQLite verification: the isolated `full` candidate has 215,382
  content chunks, 38,775 E5-large-v2 vectors, and 176,607 missing vectors;
  source coverage is `DONE`, but vector readiness and parity remain incomplete.
  The active application index has 209,026 content chunks and 38,888 vectors;
  it was not modified. No embedding job or database rebuild was run by this
  deployment.
- The single candidate embed service remains active behind its fail-closed
  thermal gate. At this check it held about 15 MB, with 2.76 CPU seconds
  accumulated; `coretemp` `Package id 0` read 81 C, above its 70 C threshold.
  The required ten-minute cool dwell has not been established, so no vectors
  were added. VM112 remains `paused`.
- The public RedAmon wiki page returned by web search is stale for current
  profile details. The pinned upstream source and the existing local audit at
  commit `4585b3442cf3c6e075fca5d3b512a8f47be501b6` remain the comparison
  basis; they confirm the seven source families and full-profile NVD settings
  recorded below. Exact installed-instance corpus and query-result parity is
  still unverified.

## Latest coordinator verification — 2026-10-26 22:09 UTC

- Read-only CT108 checks confirmed `VM112` is `paused`. The candidate-only
  embed unit is still active with PID 83922, but had consumed 1.103 CPU seconds
  after about 26 minutes and its process showed 0.0% CPU. No vector progress
  occurred: the candidate remains at 38,775/215,382 content vectors; the active
  application index remains at 38,888/209,026. The configured `coretemp`
  `Package id 0` sensor read 84 C; `thermal_zone0` read 27.8 C and is not the
  package sensor. The 70 C / ten-minute gate therefore remains closed. No
  model inference or index write was observed in this check.
- Candidate snapshot ID is
  `272df7647cab52f6fc0067fd4cdedfd196776a76685a587e885b95273ddd8a33` and its
  stored source-manifest digest is
  `931151ff9d5abd47ba1c75db7cb97fff662163ca678cb8e1f22d8b47ccb3e084`.
  The current workspace manifest digest is
  `95336ab4a4f6109d93f515d9b465740e035b3bc277197926edbd167d617a0b10`. A
  direct comparison with commit `274c622` confirms that the only manifest
  change is a more precise ExploitDB dataset-terms statement; feed URLs,
  revisions, profile filters, and source-family membership did not change.
- Inspected the current official RedAmon source at commit
  [`4585b3442cf3c6e075fca5d3b512a8f47be501b6`](https://github.com/samugit83/redamon/tree/4585b3442cf3c6e075fca5d3b512a8f47be501b6)
  (2026-10-22). Its [`full` profile config](https://github.com/samugit83/redamon/blob/4585b3442cf3c6e075fca5d3b512a8f47be501b6/services/knowledge_base/kb_config.yaml)
  lists `tool_docs`, `gtfobins`, `lolbas`, `owasp`, `exploitdb`, `nvd`, and
  `nuclei`; [`kb-build-full`](https://github.com/samugit83/redamon/blob/4585b3442cf3c6e075fca5d3b512a8f47be501b6/services/knowledge_base/Makefile)
  sets NVD lookback to 730 days, and the [NVD full profile](https://github.com/samugit83/redamon/blob/4585b3442cf3c6e075fca5d3b512a8f47be501b6/services/knowledge_base/curation/nvd_client.py)
  ignores the CVSS threshold. These settings match the workspace's declared
  full-profile manifest. The config file's Git blob ID is
  `3599de4c7fdd4e75d5c7ff1144ec3d0dec135207`.
- Compared all ToolDocs Markdown paths and Git blob hashes in the candidate's
  vendored source against the same upstream commit's `agentic/skills/` tree:
  46 upstream files, 46 local files, no missing/extra paths, and no differing
  blobs. This proves the vendored ToolDocs source is current at that commit;
  it does not prove that the installed RedAmon VM has the same KB snapshot.
- Candidate source aggregates (content documents/chunks): ExploitDB
  46,636/46,636; GTFOBins 458/731; LOLBAS 242/464; Nuclei 13,401/19,359; NVD
  146,938/146,938; OWASP 129/899; ToolDocs 43/355. They sum to the candidate's
  207,847 searchable source documents and 215,382 content chunks. Source
  membership and profile-filter parity are now evidenced against upstream
  source, but the installed RedAmon snapshot and cross-system query results
  remain unverified because VM112 is paused. Exact corpus/retrieval parity is
  still `PARTIAL`.

## Latest coordinator check — 2026-10-26 21:53 UTC

CT108's candidate index was read through SQLite `-readonly`: 215,382 chunk
rows and 38,775 E5-large-v2 vectors (1,024 dimensions). No vector progress was
observed. The retained service `graph-kb-assistant-kb-embed.service` is active
and enabled with MainPID 83922, but has consumed only 0.48 CPU seconds after
about ten minutes and holds about 16 MB; it is waiting before model loading.
The exact configured sensor (`coretemp`, label `Package id 0`) read 87 C, so
the 70 C limit and ten-minute cool dwell correctly prevent embedding. An
earlier 27.8 C read from `/sys/class/thermal/thermal_zone0/temp` was a
different thermal zone and is not valid evidence that the CPU package is
cool. Other hwmon zones also differed from the CPU package sensor.

Two active services were found targeting the same candidate. Both were
sleeping behind the thermal gate and the embed code has an exclusive per-index
lock, so only one could write; the redundant legacy
`graph-kb-assistant-kb-embed-candidate.service` was stopped and disabled. The
repository-managed `graph-kb-assistant-kb-embed.service` remains the single
enabled worker. Its installed unit and environment select the isolated
candidate, not the live index. This cleanup changed no KB rows. The candidate
still lacks 176,607 content vectors, and `vector_ready` remains false. VM112
was not resumed.

At 22:02 UTC, a narrower read-only query showed all 5,960
`yaml_size_limit` fallback chunks come from exactly two source documents:
`http/technologies/wordpress/plugins/wordpress-plugin-detect.yaml` (10,079,081
characters) and `http/technologies/wordpress/themes/wordpress-theme-detect.yaml`
(1,251,736 characters). No `yaml_complexity_limit`, `yaml_scan_failed`, or
`yaml_parse_failed` fallback chunks were present. These two templates exceed
RedAmon's 1 MiB YAML bound and are skipped by its provider. At this check the
candidate still had 215,382 chunks and 38,775 vectors; the exact CPU package
sensor read 86 C. This narrows the corpus mismatch to two large Nuclei
documents, but does not prove the remaining source documents and chunks are
identical.

## Embedding destination correction — 2026-10-26 21:36 UTC

A fresh read-only PVE check confirmed VM112 is paused. In CT108,
`graph-kb-assistant-kb-embed.service` is inactive and disabled with `MainPID=0`;
the package sensor read 75 C. The candidate index path recorded below exists.
Inspection of the installed unit's expanded `ExecStart` showed it targets the
live `kb/index.sqlite3`, not `kb/candidates/full-20261026-1936/index.sqlite3`.
Therefore the earlier entries describing this installed unit as a running
candidate-only worker were incorrect; no worker was running at this check, and
the live index was not modified by the inactive unit.

The checked-in CLI now fails closed unless embedding points to an existing
`kb/candidates/<id>/index.sqlite3`; the systemd unit reads that path from
`/etc/graph-kb-assistant/kb-embed.env` instead of hard-coding the live index.
The focused CLI tests pass locally. At 21:36 UTC the correction had not yet
been deployed or started. The 75 C sample was above the 70 C threshold; see
the follow-up below for the deployment and gated start.

## Candidate-only worker deployment — 2026-10-26 21:41 UTC

The pushed `a1eff94` CLI and systemd unit were deployed to CT108; their
SHA-256 hashes match the workspace files. The root-owned environment file now
selects only `kb/candidates/full-20261026-1936/index.sqlite3`. The deployed
CLI was invoked against the live `kb/index.sqlite3` as a negative safety
check and rejected it before opening the database. The corrected unit was
started manually and enabled for boot so it resumes the same gated candidate
job after a CT restart. At the read-only check,
MainPID 83922 was active at 0.2% CPU and 0.4% memory, while the package sensor
read 77 C. Candidate counts were 215,382 chunks and 38,775 vectors, unchanged
from the preceding read; the thermal gate is waiting, not embedding. VM112
remains paused. No live index rows were changed.

## Latest candidate state — 2026-10-26 21:13 UTC (supersedes older counts below)

- Commits `2c86ced`, `d6340ba`, `6f1ea2e`, and `5eb90f4` implement
  RedAmon-compatible source chunk identities, parser-token YAML limits, and a
  bounded raw-text fallback for Nuclei templates that cannot be parsed. The
  latest GitHub CI run (`40400476932`) passed. Local focused KB verification
  passed: 65 tests and 3 subtests, plus compileall and `git diff --check`.
- The tested `canonical.py` from `5eb90f4` is deployed to CT108. The isolated
  candidate `kb/candidates/full-20261026-1936/index.sqlite3` completed its
  re-chunk: 207,878 source documents from seven families and 215,382 content
  chunks. Its metadata reports `coverage_state=DONE`, but
  `source_manifest_matches_current=false`; this is not a parity or publication
  receipt. Comparing stored-manifest revision `274c622` with current
  `sources.yaml` found exactly one change: a stricter clarification of the
  ExploitDB `dataset_terms_basis` wording. Source families, URLs, revisions,
  and filters are unchanged in that diff; the historical digest is retained.
  A read-only aggregate found 38,775 E5 vectors and 176,607 content
  chunks without vectors. Candidate `vector_ready=false`. The active index was
  not changed; its last observed count remains 209,036 stored chunks (209,026
  content chunks) and 38,888 E5 vectors.
- The bounded Nuclei fallback marked 5,960 chunks with
  `canonicalization_fallback=yaml_size_limit`; oversized source text is
  retained in bounded chunks instead of aborting the family rebuild. This
  preserves source material but is not proof that these records have the same
  normalized text/chunk boundaries as RedAmon.
- A direct source-code comparison confirms this is a corpus difference, not
  merely an unverified implementation detail: RedAmon's pinned
  inspected RedAmon `file_cache.py` caps YAML at 1 MiB, and its
  `NucleiClient.fetch()` catches
  `YAMLTooComplexError` and skips the template. The Assistant candidate instead
  retains these oversized templates as searchable raw chunks. A read-only
  group count on CT108 found 5,960 such chunks and 13,399 normally rendered
  Nuclei chunks. Thus the candidate is fuller, but its default Nuclei corpus is
  not byte/chunk-identical to RedAmon; strict parity needs a separate
  supplemental path or RedAmon-side matching behavior, plus same-query
  receipts.
- A read-only `PRAGMA quick_check` was started on the candidate, but no result
  returned within the check window. Candidate database integrity therefore
  remains unverified; no success is claimed. The
  candidate-only embed service in CT108 is active and enabled. Its process is
  sleeping at 0.0% CPU, and the CT-visible thermal sensor read 82 C. The
  service's 70 C / ten-minute cool-dwell gate is holding model loading and
  embedding; no duplicate worker was started. VM112 remains paused.
- Next: keep the candidate isolated, let its existing thermal-gated worker
  resume only after its configured cool dwell, then verify database integrity,
  complete vectors, source-manifest provenance, and fixed-query receipts
  before considering promotion. Exact RedAmon corpus and ranking parity remain
  `PARTIAL`.

At 2026-10-26 21:16 UTC a fresh CT108 sample read 93 C. The candidate unit
remains active/running, while its process is sleeping at 0.0% CPU. The
thermal gate continues to prevent model loading; the earlier 82 C sample
above is superseded by this reading.

## VM112 thermal follow-up — 2026-10-26 21:18 UTC

A fresh PVE check found VM112 running, contrary to the earlier recorded state.
Its `ps` CPU percentage was a lifetime average, not an instantaneous sample.
Following the user's existing instruction to pause VM112 temporarily, `qm
suspend 112` completed and `qm status 112` confirmed `paused`. CT108's sensor
then read 87 C and 82 C on subsequent samples. These readings are trending
down but do not establish a cool dwell below 70 C. The candidate embed service
remains active behind its thermal gate; its process is at 0.0% CPU. No data
disks or KB indexes were changed by the pause.

At 21:22 UTC the CT-visible sensor read 84 C and `qm status 112` still
reported `paused`. A brief PVE `top` sample showed 93.2% CPU idle, 940 MiB
free RAM, and 8,182/8,192 MiB swap in use. Do not start the separate Neo4j
parity clone or another embedder under this thermal/memory state.

At 21:28 UTC, with VM112 still paused and the candidate service still gated,
the CT-visible sensor read 85 C. The required dwell below 70 C has not begun.

## User goal: maximum RedAmon full-profile coverage — 2026-10-26

The requested target is the pinned RedAmon `full` source profile, not the
smaller `cpu-lite` profile most recently ingested by the installed VM112
instance. The isolated Graph Assistant candidate has the seven full-profile
families and the 730-day NVD window: 207,878 source documents and 208,950
content chunks (208,960 stored rows including 10 notice rows). It has 38,882
E5-large vectors (18.6%) and 170,068 content chunks still missing vectors.
The active application index remains unchanged; candidate vector readiness is
false. Thus the immediate completeness gap is semantic coverage, not a missing
RedAmon full-profile family.

### Workspace code-side work (not yet applied to CT108)

The workspace now emits RedAmon-compatible stable 16-hex source/record IDs
and source-specific normalized cards for GTFOBins, LOLBAS, and Nuclei. ToolDocs
and OWASP use section identities; NVD and ExploitDB use their CVE/EDB IDs.
License and notice documents remain stored as provenance but no longer create
search chunks. YAML parsing uses pre-scan complexity bounds and PyYAML's safe
loader. Rechunking remaps an E5 vector only when the exact text hash matches;
changed chunk text invalidates the old vector. All `tests/test_kb_*.py` tests
pass locally (60 tests and 3 subtests). Selected fixture output was compared
with the checked-out RedAmon client code. This does not establish whole-corpus
or deployed RedAmon equality; CT108 has not been re-chunked with this code.

At 20:39 UTC the CT108 candidate embedding unit was inactive and disabled;
VM112 remained paused. The candidate is unchanged at 38,882/208,950 vectors
(18.6%), with 170,068 content vectors missing. Active application data was not
changed.

The current CT108 full-profile index was inspected read-only before work was
resumed. All seven stored source revisions match their current pinned source
revisions. The source-manifest digest is stale because the present manifest
contains later legal/notice metadata, and its `DONE` report has inaccurate
chunk counts for ToolDocs (434 reported versus 432 stored) and OWASP (913
reported versus 900 stored). A focused code change now makes `rechunk_index`
recompute per-source document/chunk counts from the resulting SQLite tables;
its 17 provider/rechunk tests pass. It does not silently change the active
source manifest or claim RedAmon parity.

At 2026-10-26 20:04 UTC a candidate-only resumable worker was started on
Proxmox CT108. Its unit is bounded to one thread, batch size 2, 20% CPU and
3 GiB memory. It waits for a continuous 10-minute period below 70 C before
loading the model and checks temperature before every durable batch. At
20:23 UTC `coretemp Package id 0` inside CT108 read 89 C; the worker remained
active in its thermal gate with under one second CPU and had added no vectors.
A single later 20:28 UTC sample read 55 C, five minutes after the 89 C sample;
this does not establish the required ten-minute dwell. The candidate worker
preserves completed batches and only publishes vector readiness after all
content chunks are embedded. A later read-only CT108 check at 20:39 UTC found
the candidate embed unit inactive and disabled. The original Kali embed unit
is disabled and VM112 remains paused.

Current parity is still `PARTIAL`: the active RedAmon installation is `cpu-lite`
with E5-base-v2 while this max-fill job uses the Assistant's pinned E5-large-v2
on `full`. The target remains broad full-profile coverage first; exact
installed-RedAmon equality requires separate source and query receipts.

## Parity matrix

| Area | Last recorded local evidence | RedAmon comparison | State / gap |
|---|---|---|---|
| Sources | Read-only CT108 `bb_kb_stats` at 2026-10-26 16:17 UTC reports snapshot `ac9d1d2ac0fe0a3e80654180c81ac43f46a77f1e4481a7dc593fdfaed16ee049`, seven families, 207,878 documents, and 209,026 content chunks; its snapshot digest was null. Direct Python stats at 19:35 UTC found stored digest `931151ff9d5abd47ba1c75db7cb97fff662163ca678cb8e1f22d8b47ccb3e084`, not matching current manifest SHA-256 `95336ab4a4f6109d93f515d9b465740e035b3bc277197926edbd167d617a0b10`. | Read-only inspection of VM112's installed RedAmon tree and index found `.last_ingest` profile `cpu-lite` (2026-10-19), configured as `tool_docs`, `gtfobins`, and `lolbas`. Its FAISS index has 2,449 IDs. The VM's repository is dirty, so this identifies the installed reference state, not a reproducible clean checkout. | **Known profile mismatch.** The seven-family Assistant snapshot is not the installed RedAmon `cpu-lite` corpus. Exact document/chunk equality remains unmeasured. |
| Rights and notices | Runtime snapshot contains ExploitDB coverage from its prior build. The current manifest permits only the pinned GitLab repository’s allowlisted descriptions/metadata and preserves README/LICENSE; PoC files are never fetched. The stored manifest digest predates current legal/notice metadata. | No deployed RedAmon corpus or rights receipt was supplied. | PARTIAL; current local source gate is scoped and enabled, but active snapshot provenance is stale. |
| Extraction and canonicalization | Snapshot exporter/importer validates manifest, content hashes, provenance, deterministic chunks and spans. Source providers include fixed archive/API adapters and explicit coverage states. | No same-revision source document export to compare. | PARTIAL; extraction equivalence is unproven. |
| Chunking | Workspace code now uses RedAmon source identities and source-specific renderers; offline fixtures pass. Candidate not re-chunked. | Selected renderer fixtures match the checked-out client source; no deployed full-profile receipt. | PARTIAL; whole-corpus boundary/hash parity is unproven. |
| Lexical search | Local index uses SQLite FTS5 with RedAmon-inspired query cleanup, title/body BM25 weighting and candidate filtering. | RedAmon uses Neo4j Lucene; analyzer and score parity are not established. | PARTIAL; engines are not interchangeable. |
| Embedding model | The Assistant snapshot uses `intfloat/e5-large-v2`, 1,024 dimensions. | The installed RedAmon config has an uncommitted change to `intfloat/e5-base-v2`; its pinned revision is `f52bf8ec8c7124536f0efb74aca902b2995e5bcd`. The installed FAISS artifact's size and ID count are consistent with 2,449 768-d float vectors. | **Known model/dimension mismatch.** A model rebuild is required for parity. |
| Vector coverage | Candidate count read at 20:34 UTC: 38,882 E5 vectors of 208,950 content chunks; `vector_ready=false`, leaving 170,068 without vectors. Unit inactive/disabled at 20:39 UTC; VM112 paused. | The installed RedAmon FAISS index and `chunk_ids.json` each contain 2,449 entries; index manifest SHA-256 is `20fa1849164b537a6b5068b19b90d7a58370f73ab868f6fd0df3aaf0752ea030`. | Both stores have vector data, but they cover different profiles/models. No same-corpus vector comparison has been made. |
| Query behavior | Local retrieval combines lexical/vector candidates, reciprocal-rank fusion, BGE reranking and MMR; fixed query fixtures are checked in at `references/kb-parity-queries.json`. | Installed RedAmon source config sets top-k 5, overfetch 6, score threshold 0.80, RRF k 60, reranker pool 30, and MMR lambda 0.65. RedAmon uses Neo4j Lucene plus FAISS; no same-query result receipt has been produced. | PARTIAL; some settings are comparable, but corpus, embedding model, and backend differ; output/rank parity is untested. |
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

## Read-only RedAmon reference found on VM112 — 2026-10-26

The earlier statement that no RedAmon deployment was available is superseded:
the Proxmox host contains VM112 with the installed RedAmon checkout and its
Neo4j volume. At the user's request, VM112 remains **paused**. Its qcow2 disk
was attached read-only (`qemu-nbd`, ext4 `ro,noload`) for inspection and was
unmounted/disconnected afterward; no VM process or original database was
started or changed.

The installed source checkout is at `e1dfdff671b7e76d3a002eeba37b01d92cb949e1`
on `master`, with four local modifications. In particular, its active
embedder config is changed to `intfloat/e5-base-v2`, and its local pin file
contains revision `f52bf8ec8c7124536f0efb74aca902b2995e5bcd`. The on-disk
`.last_ingest` records `cpu-lite` at `2026-10-19T18:32:53.497301+00:00`;
that profile explicitly contains `tool_docs`, `gtfobins`, and `lolbas`. Other
source caches present on disk are not evidence that those sources were
ingested into this active profile.

Inspection of the installed `ToolDocsClient.SKILL_CATEGORIES` confirms that
its importer enumerates 15 categories and omits `post_exploitation`, even
though that directory has three files in the repository manifest. The local
provider's same 15-category allowlist and its 43-document/355-chunk golden
fixture therefore match the installed client behavior. Do not add those three
files merely because they exist in the checkout; parity follows the deployed
client's category filter.

The installed FAISS index is `IndexFlatIP`; its ID list has 2,449 entries and
the index file is 7,523,373 bytes, consistent with 2,449 normalized 768-d
float vectors. The sidecar manifest SHA-256
`20fa1849164b537a6b5068b19b90d7a58370f73ab868f6fd0df3aaf0752ea030` matches
the index file. The source config also records top-k 5, overfetch 6, score
threshold 0.80, RRF k 60, reranker pool 30, and MMR lambda 0.65. These are
reference configuration/artifact facts, not equivalent search-result evidence.

A byte-verified copy of the RedAmon Neo4j data directory was made on the PVE
host (source/copy tree SHA-256
`fec02bf190ecc773db7e49331454bcbeed1fd42d38fcf98272420aa5f8d08823`).
`neo4j-admin database info` on the copy reported store format
`record-aligned-1.1`, last committed transaction `50228`, and no recovery
required. A network-isolated, CPU/memory-limited Neo4j 5.26 clone was then
attempted on that copy, but it exited with code 3 before becoming queryable;
the only server log says shutdown was requested, and Docker reported no OOM.
Therefore no Cypher corpus export or same-query result has been obtained yet.

**Conclusion:** actual installed-reference evidence now proves the active
RedAmon profile, model/configuration, and vector artifact. It also proves the
current seven-family E5-large Assistant snapshot is not a matching profile or
model. Exact corpus hashes and same-query ranking remain unverified. Keep
parity `PARTIAL`; fill the checked full-profile index under the constrained
worker while the separately verified snapshot/rechunk work proceeds.

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

The vendored ToolDocs source was compared file-by-file with the checked-out
RedAmon `agentic/skills` tree at commit
`cf1b9e926a2af4420f5a894e7c8ddd225968c9d2`: both contain 46 Markdown files,
with identical relative paths and SHA-256 hashes. A deterministic local
comparison of RedAmon's ToolDocs client and the assistant adapter produced the
same 355 searchable chunks, with exact title and content hashes. The assistant
also retains one separate MIT license notice, which is deliberately excluded
from search chunks and embeddings. This proves ToolDocs source and searchable
content parity at that pin, not parity with RedAmon's deployed corpus or its
other six families. The active assistant snapshot's 434 ToolDocs chunks are
stale; a candidate rebuild should replace them with the verified 355 content
chunks plus the separate notice after full-profile coverage passes.

Before a source rebuild, the two CT108 Python files that write/report snapshot
manifest hashes were updated from the CI-passing branch files with backups.
Hashes now match for all 21 tracked `src/bb_assistant/kb` files. This ensures
the next imported snapshot records the current source-manifest hash instead of
leaving it null.

## Live read-only index integrity check — 2026-10-26 18:36–18:40 UTC

Authenticated read-only inspection of PVE confirmed CT108's
`graph-kb-assistant.service` is active and `/api/health` returns `ok` at
`0.1.0-go-dev+d0efdb63e31f`; the embed unit is inactive. VM112 (`agent-hub`)
remains paused. PVE had 3.8 GiB available memory, 7.8/8 GiB swap used, and one
66 C package-temperature sample. No guest was resumed, and no rebuild or
embedding was started.

The CT108 SQLite index was opened using SQLite URI `mode=ro` and
`PRAGMA query_only=ON`. Its active snapshot remains
`ac9d1d2ac0fe0a3e80654180c81ac43f46a77f1e4481a7dc593fdfaed16ee049`, with
207,878 source documents, 209,036 stored chunks, and 38,888 E5 vector rows.
The published vector model metadata is absent; a staging marker targets this
snapshot, so vectors are not ready. The index's coverage metadata says `DONE`,
but per-source counts do not match either its immutable `chunks.jsonl` or the
active `chunks` table: ToolDocs coverage says 434 versus 432 actual chunks;
OWASP says 913 versus 900. All other source-family chunk counts match. The
snapshot-level total of 209,036 matches the file and database, while the
per-source coverage rows sum to 209,051. A streaming read of the 513 MB chunk
file confirmed the per-source counts in SQLite. Therefore the source-level
`DONE` assertion is not trustworthy, and the current data cannot produce a
valid parity receipt.

The parity exporter and snapshot importer now reject `DONE` coverage whose
per-source document or chunk counts disagree with the snapshot records. Go KB
stats and search also downgrade such a snapshot to `PARTIAL` while preserving
search results. At the time of this integrity check, these code changes were
not yet deployed. The new checks passed their focused local Go and Python
tests. The next safe data action is to reconcile the coverage rows from already
stored documents, then produce a new immutable snapshot before any vector work.

## Coverage integrity fix deployed — 2026-10-26 18:46 UTC

CI run `40380086566` passed all jobs for PR head
`8251a859d519806afe710944b45e97521bf1564b`. The tested PR-merge Linux amd64
artifact reports version `0.1.0-go-dev+44c6468fd07f` and SHA-256
`8861baa0366d888f603048d9c27d49f1edf49e470edf1b8b0dd45c82cb7dd065`. It is
installed on CT108 as `/usr/local/bin/bb-assistant`; the prior binary is backed
up as `/usr/local/bin/bb-assistant.before-8251a85`. The application service is
active after restart, loopback `/api/health` returns `ok`, and a live read-only
MCP `bb_kb_stats` call reports the existing snapshot as `PARTIAL`. It still has
207,878 documents, 209,026 content chunks, and 38,888 E5 vectors; vector
readiness remains false. Search is preserved. No snapshot rebuild or embedding
was started, and VM112 remains paused. The existing coverage rows still need
reconciliation from stored source documents before an immutable replacement
snapshot can be built; RedAmon corpus and query parity remain `PARTIAL`.

## Candidate source refresh and vector continuation — 2026-10-26 19:52–20:05 UTC

Code commits `017343e`, `511b426`, and `0b67b91` were pushed to the active
development branch; the focused KB provider/rechunk suite passed (18 tests),
and CI run `40391962579` passed. The deployed Python files on CT108 have
SHA-256 hashes matching their pushed revisions.

The active application index was left unchanged. An isolated candidate at
`kb/candidates/full-20261026-1936/index.sqlite3` was refreshed from the pinned
vendored ToolDocs source and re-chunked under 20% CPU / 2 GiB memory limits.
All 44 ToolDocs source hashes now match the current local provider. Coverage
reports 44 documents and 356 chunks (43 source documents plus the retained
notice). The candidate contains seven `DONE` source families, 207,878
documents, and 208,960 stored chunks (208,950 content chunks). Per-family
counts: ToolDocs 356, GTFOBins 479, LOLBAS 244, OWASP 900, ExploitDB 46,638,
NVD 146,940, and Nuclei 13,403.

Six embeddings for chunk IDs removed by the ToolDocs refresh were pruned, so
all 38,882 remaining vectors join to current chunks. A regression now makes
future re-chunk operations prune orphan vectors automatically. The candidate
records the original snapshot source-manifest digest `931151ff…`, which differs
from the current manifest `95336ab4…`; source-definition freshness is false
and parity remains `PARTIAL`. This preserves provenance honestly and does not
claim equality with the installed RedAmon or its full-profile corpus.

A candidate-only E5 worker is running on CT108 with one model thread, batch
size 2, 20% CPU quota, 3 GiB memory cap, and a 70 C stop / ten-minute cool
dwell gate. At the last check the package sensor read 75 C; the worker was
waiting before model load (about 16 MiB resident, under one second CPU), with
no new vectors reported. The worker targets the candidate, not the active
application index. Promotion remains pending until embeddings finish and the
candidate passes final integrity and retrieval checks. Graph API and Neo4j
remain active; VM112 remains paused.

## VM112 installed-index ID reconciliation — 2026-10-26 23:08 UTC

The RedAmon reference disk was rechecked while VM112 was paused, using a
read-only qcow2/NBD attachment and ext4 `ro,noload`; the mount was then removed
and `/dev/nbd0` disconnected. A checksum-verified temporary copy of
`chunk_ids.json` on CT108 was removed after comparison. VM112 remains paused.

The installed index contains 2,449 IDs. Its artifact hashes are:

| Artifact | SHA-256 |
| --- | --- |
| `index.faiss` | `20fa1849164b537a6b5068b19b90d7a58370f73ab868f6fd0df3aaf0752ea030` |
| `chunk_ids.json` | `41793591519064947966fe877bd3d2f8e79710086d0993739fdf1604c8539ef1` |
| `index.faiss.manifest.json` | `12674436584b2aa5376817e0f2d3b8f8c56b12508a03390b544e82e3b617a1f6` |

The candidate's four RedAmon-compatible families have 2,449 IDs in total and
overlap the installed ID list on 2,448 IDs: ToolDocs 355/355, LOLBAS 464/464,
OWASP 899/899, and GTFOBins 730/731. The only difference is the `ld.so` shell
card. RedAmon's key `gtfobins:ld.so:shell` produces
`064de01bda88d0b3`; our old importer used `Path.stem`, changed the key to
`gtfobins:ld:shell`, and produced `3c3199b415f18567`. The importer now strips
only a trailing `.md`, matching RedAmon's filename handling and preserving
`.so` as part of a real binary name. This corrects stable-ID parity; the
candidate database has not yet been refreshed with the fix because its
embedding worker is active.

This ID-level result is much stronger than the earlier profile-marker-only
assessment, but it does not prove chunk-text hashes or same-query ranking. The
installed `.last_ingest` still says `cpu-lite`, while the four-family ID set
matches the candidate subset; treat that marker as inconsistent with the
artifact until the installed source/text manifest is independently reconciled.
The installed RedAmon config uses `intfloat/e5-base-v2` (768 dimensions); our
candidate uses E5-large-v2 (1024 dimensions), so its vectors cannot be compared
directly. The candidate currently has 215,382 chunks and 38,775 vectors; its
worker is active but has used only about 3.45 CPU seconds and 14.7 MiB RAM,
consistent with waiting before model load. A single 60 C PVE temperature
sample is not proof that the worker's ten-minute cool-dwell gate has passed.
Do not bypass that gate. VM112 stays paused, and exact corpus-text and retrieval
parity remain `PARTIAL`.

After CI run `40417806079` passed Go, Python, and secret-scan jobs, commit
`6d9277f`'s `gtfobins.py` was installed in CT108 at the source path with the
previous file backed up under
`/home/max/.local/share/bb-graph-assistant/deploy-backups/python-pre-6d9277f/`.
The installed file SHA-256 matches the checked-in file. No application or
embedding service was restarted and no candidate/active KB data was changed;
the correction will be used by the next controlled source refresh after the
current embedding worker releases the candidate.

## Thermal gate recheck — 2026-10-26 23:18–23:21 UTC

The worker process `83922` is still live in `hrtimer_nanosleep`; its CPU time
rose only from 3.734 to 3.866 seconds while elapsed time advanced about ten
minutes. CT108's application and embedding services are both active. A
read-only candidate query still returns 215,382 chunks and 38,775 vectors, so
no embedding progress occurred during this interval.

A 60-second, one-second-sample read of the same `coretemp` Package id 0 sensor
used by the worker ranged from 58 C to 92 C; 6 of 60 samples were at or above
the 70 C stop threshold. This explains why the worker's ten-minute cool-dwell
timer keeps resetting; do not raise or bypass the threshold. VM112's QMP state
is `paused` and its QMP CPU metric is zero. The `qm list`/top-level API can
still show a running QEMU process for a paused VM, so that process's lifetime
average from `ps` is not current CPU usage. A `top` snapshot showed most
current host CPU load in VM920, but that single sample does not prove it caused
the temperature peaks. VM920 was left unchanged. The candidate remains
incomplete, and vector/retrieval parity remains `PARTIAL`.

## Pinned RedAmon full-profile model/source contract — 2026-10-26 23:24 UTC

Read-only Git object inspection of the installed checkout's pinned commit
`e1dfdff671b7e76d3a002eeba37b01d92cb949e1` confirms that the clean source
config defaults to `intfloat/e5-large-v2`, pinned to
`f169b11e22de13617baa190a028a32f3493550b6`. The assistant candidate uses the
same model name and exact revision. The installed VM's working-tree override
to `e5-base-v2` is dirty state, not the pinned source default; its 2,449-vector
index therefore does not establish the model used by a clean full-profile
build.

The same pinned config defines full as
`tool_docs, gtfobins, lolbas, owasp, exploitdb, nvd, nuclei`, matching the
candidate's seven families. RedAmon's full Make target overrides NVD lookback
to 730 days, and the full profile ignores the standard profile's CVSS cutoff.
This confirms source-level agreement on the full-family set and embedding
model. It still does not prove the installed dirty index contains all seven
families, that every chunk's text/hash matches, or that the two retrievers
rank identically. Corpus/retrieval parity remains `PARTIAL` until a full
candidate refresh, complete vectors, and same-query receipts are available.
