# Project status and known limitations

## Fresh local verification (2026-10-23, after `b4f460e`)

This checkpoint supersedes earlier runtime claims below where they conflict;
older audit entries describe their own historical snapshots.

## Runtime follow-up (2026-10-23, after `c93b1b0`)

- A resumed bounded Caido-history probe processed 27 pages: 31 captures were
  new and 1,319 were replays. The run remains resumable and `PARTIAL`; the
  preceding 196,809-ID inventory still has no stable snapshot token, so it is
  not a point-in-time completeness guarantee.
- A fresh read-only audit using the configured private data root found capture
  schema v7 and BehaviorGraph v2 compatible. The store has 1,400 captures and
  search-index entries, 1,369 graph mappings, 31 unmapped captures, 444
  complete parses and 956 partial parses. Response fidelity is unknown for 37
  captures. Historical sync and overall coverage remain `PARTIAL`.
- The projection ledger has 1,400 pending jobs with 251,263,436 bytes of
  normalized bundle JSON. The encrypted vault contains 2,763 objects totaling
  33,124,199 bytes. The filesystem had 17 GB free at the last check. These
  measurements show that another unbounded history import is not yet sized for
  the available disk; do not scale it up until asynchronous projection and the
  durable payload/storage plan account for the backlog. The import checkpoint
  remains untouched and resumable.
- `neo4j-backfill --verify-only` read the working Neo4j snapshot without
  replacing it. The local snapshot digest was
  `6b121586e8b437953019ddb93d50d4e06c00a4bad5fd3b7d0015258899b43747`; Neo4j
  returned `dc1b5523144061fd1deb0dfc19a536896bf355f7832c4a3b15b4d24f2c9388f8`.
  Parity is therefore `PARTIAL`. Source-history completeness remains a
  separate, unassessed property.
- A fresh local KB stats snapshot reports 9,878 semantic files and 67,505
  chunks/vectors, of which 2,852 files and 41,087 vectors match the current
  version and source hashes. Semantic coverage is degraded: 7,026 rows are
  wrong-version and unhashed, and integrity is not `ok`. The lexical tier is
  available with 15,438 files and 67,991 passages; its last refresh was
  2026-10-13. Semantic indexing last completed 2026-10-07. The MCP stats call
  did not complete in the earlier audit, so this snapshot came from the local
  CLI fallback. KB readiness remains `PARTIAL`.
- Caido import progress now emits sanitized page counters and cumulative stage
  timings to stderr. Millisecond values preserve sub-millisecond totals; graph
  projection time is explicitly a breakdown included in page-processing time.
  Full Go tests, race tests, `go vet`, and `git diff --check` passed for this
  change.

- The working Juice Shop capture database was migrated from schema v5 to v7
  using the existing Python migration path under the exclusive project lock.
  A private pre-migration backup outside the checkout was verified before the
  change. Post-migration SQLite integrity passed and capture/search/revision/
  graph-map counts remained stable. The zero-byte WAL and inactive SHM runtime
  sidecars were removed only after confirming no process held the files.
- `project-audit` now opens both stores successfully: capture schema v7 and
  BehaviorGraph schema v2 are compatible. It reports `PARTIAL`: 1,369
  captures, 1,369 search-indexed and graph-mapped, 414 complete parses and 955
  partial parses. Request text fidelity is reconstructed for all 1,369;
  response text is reconstructed for 1,332 and unknown for 37. The live index
  is empty and no source-byte fidelity was established.
- `neo4j-backfill --verify-only` reports `state=verified` with matching local
  and Neo4j digest
  `dc1b5523144061fd1deb0dfc19a536896bf355f7832c4a3b15b4d24f2c9388f8` and
  matching counts (25,064 entities, 25,685 assertions, 1,369 cards and 1,369
  sources). Its `source_sync_not_assessed` coverage remains separate from
  graph parity.
- Caido 0.58.3 lists the Juice Shop project as ready but inactive; `sperpravo`
  is selected. No project switch or history call was made. Caido reconciliation
  against Juice Shop therefore remains unverified; do not query the selected
  unrelated project.
- The loopback UI responds with HTTP 200. Its surface API reports 127 local
  operations and 1,369 observations with `PARTIAL` coverage. ECC `benchmark`
  measurements used the running x86_64 process (8 logical CPUs), graph
  generation 21, and the local 1,369-observation dataset. For two operation
  shapes, 100 sequential direct-view requests returned HTTP 200 and stable
  payload sizes; p95 was 77.6 ms and 98.69 ms. Ten concurrent requests had
  maximum latencies of 273.26 ms and 197.13 ms. The selected responses held 3
  entities/2 assertions and 4 entities/3 assertions respectively, both with
  `incomplete=true` and `truncated=false`. These are repeated direct-view API
  measurements on an already-running process, not cold-start or full analytical
  slice benchmarks. The proposed 2-second focused-view p95 was met for these
  two shapes; Stage 2 remains `PARTIAL` for source coverage and other gaps.
- The application KB remains `PARTIAL`. A newer local read-only audit found
  degraded semantic-index freshness and an active background reindex; exact
  snapshot and integrity counts are recorded below. NVD remains partial and
  ExploitDB remains blocked pending dataset-terms review; the source corpus
  has not been expanded.
- `./scripts/run-hermes mcp test bb_assistant` connects over local stdio and
  discovers the 9-tool read-only allowlist. The current private Hermes profile
  has mode 0600; it contains no model/provider selection. Model-backed analysis
  remains unverified and no provider call was made.
- Remaining material gaps: Caido history is not currently reconciled against
  the inactive Juice Shop project; 955 captures have partial parsing, 37
  response fidelities are unknown, browser walkthrough and JavaScript endpoint
  coverage are absent, and live intake has no current records. Overall runtime
  and source coverage therefore remain `PARTIAL`.

## Stage 2 operation catalog update (2026-10-23)

- Added an indexed-operation catalog shared by the Go application-query layer,
  HTTP UI, and MCP. It groups exact `host:port + method + route` operations,
  deduplicates historical/live rows by Caido Request ID (historical wins),
  computes exact local-index counts, and samples field/header names and graph
  seed IDs with fixed limits. It does not read request/response bodies or claim
  local counts equal Caido totals.
- Search now runs on the server. The opaque signed continuation cursor is bound
  to project, normalized filter, and a structural-index fingerprint; filter or
  index changes invalidate it. The UI resets pagination on filter changes,
  rejects late responses from older project/filter requests, and reloads from
  page one after a stale-cursor conflict, including cursors invalidated by a
  service restart; stale recovery also clears the old graph selection and
  inspector.
- Fixture tests cover whole-operation grouping, exact counts, historical/live
  deduplication, ordering, filter-before-page behavior, bounded field samples,
  cursor tampering/binding/staleness/restart, HTTP validation, MCP schema
  limits, UI filter races, and cursor recovery. Each page currently scans the
  full local structural index to recompute exact filtered totals and its
  fingerprint; operation materialization and field samples are bounded, but
  scan work scales with all indexed records on every page. The local SQLite
  query plan uses project identity indexes for the historical and live sources
  plus a temporary sort B-tree for operation order. Working-dataset latency and
  large-index behavior have not been measured; a reusable catalog generation
  or aggregate is follow-up work before claiming large-dataset performance.
- The initial catalog verification for this commit was synthetic and local;
  runtime compatibility and the working-dataset p95 were not yet checked at
  that checkpoint. The fresh follow-up measurements and current database state
  are recorded above. Caido history completeness remains unverified, so Stage
  2 stays `PARTIAL`.

## Stage 3 field-occurrence slice (2026-10-23)

- The Go historical graph mapper now keeps repeated query and
  `application/x-www-form-urlencoded` parameters, plus repeated JSON array
  fields, as separate occurrences with indexed paths such as
  `query["id"][0]`, `request.body["id"][0]`, and
  `$["items"][1]["id"]`. A singleton retains its established wildcard path
  and parameter ID; endpoint and capture identities do not change. Values are
  parsed only to validate input and classify field type; literal values are
  discarded before graph construction.
- Synthetic regression tests confirm separate IDs for repeated occurrences,
  stable singleton and endpoint identities, and absence of raw query/form
  values in the graph bundle. `go test -count=1 ./...`,
  `go test -race -count=1 ./...`, `go vet ./...`, and `git diff --check`
  passed.
- The current local graph was not reprojected, so existing captures still have
  the prior collapsed representation until a separately validated rebuild or
  replay. This first slice covers query fields, form fields and repeated JSON
  array fields; multipart occurrences, route-family grouping,
  actor/session/object context and flow annotations remain outstanding. Stage
  3 stays `PARTIAL`.

## Stage 3 full-data shadow reproject check (2026-10-23)

- Added a read-only, authenticated full-message recovery path bounded to 1 MiB
  per request or response and a shadow graph writer that requires an absent
  graph file in an owner-only local directory outside the selected project.
  The one-time check held the selected Juice Shop project's exclusive
  maintenance lease and read only its local capture index, encrypted vault,
  graph and receipts. It did not call Caido, switch projects, or replace the
  active graph. The temporary shadow graph and plaintext message buffers were
  removed/cleared after the check.
- All 1,369 indexed captures were reprojected. Recomputed request and response
  HMACs matched all stored signatures; all card IDs stayed stable, and the
  active graph's digest stayed unchanged. The new projection contains 112,135
  entities (107,137 parameters) and 112,753 assertions, compared with 25,064
  entities (20,069 parameters) and 25,685 assertions in the active graph.
  The temporary SQLite graph was 655,867,904 bytes versus 135,913,472 bytes
  for the active graph. A follow-up request-only identity audit reconciled the
  five non-parameter ID changes: one old `POST /` endpoint had exactly four
  linked captures whose recovered request lines point to separate
  `/…/export/…` paths. The old history summary had recorded all four as `/`;
  the new endpoint paths retain only source-keyed opaque segment aliases. The
  four new endpoint IDs therefore correct route attribution; capture,
  observation and card identities remain stable.
- The active graph was not replaced. No durable production cutover command or
  rollback journal currently coordinates SQLite replacement with the Neo4j
  snapshot, and the expanded graph is materially larger. The Neo4j snapshot
  record-count ceiling is now 300,000 so it can cover the measured 227,626
  records with headroom; the 256 MiB aggregate serialized-payload ceiling is
  unchanged. This is a bounded serializer/read-back limit change only: preparing
  that actual snapshot, Neo4j transaction capacity, outbox/import-marker
  reconciliation, cutover and rollback have not been verified. Build and
  verify that cutover before applying the occurrence-complete projection.
  Current captures therefore still use the prior collapsed representation.
  Stage 3 remains `PARTIAL`; multipart occurrences, route-family grouping,
  actor/session/object context and flow annotations remain outstanding.
- The graph-rebuild message reader now verifies encrypted-file size against
  the indexed plaintext size plus the fixed AES-GCM envelope before reading;
  the per-message plaintext cap remains 1 MiB. Aggregate capture, byte and
  time budgets still need to be enforced by a durable rebuild coordinator. A
  fixture regression covers an oversized ciphertext object. The bounded read
  API, shadow-path checks, full Go test suite, race suite, `go vet ./...`, and
  `git diff --check` passed at their recorded checkpoints. The shadow check is
  a local feasibility result, not a Caido completeness or browser/JavaScript
  coverage result.

## KB read-only audit (2026-10-23)

- The local fallback `tools/kb-mcp/query-kb.sh stats` reported a semantic index
  of 9,878 files and 65,969 chunks/vectors. Only 2,535 files and 34,621
  chunks/vectors match the current semantic index version and source hashes;
  `semantic_degraded=true`, with 7,343 legacy/incomplete file rows. There are
  no orphan chunks/vectors or missing vectors in this committed snapshot, but
  integrity is not `ok` because those legacy rows lack current hashes/version.
- The lexical FTS tier is available in hybrid mode and reports 15,438 files
  and 67,991 passages; its last recorded refresh was 2026-10-13. Empty search
  results still cannot establish absence. The MCP `kb_stats` call did not
  return within roughly one minute, so the configured local CLI fallback was
  used; it returned after about one minute with a Pydantic warning.
- At measurement time a background indexer already held the KB writer lock
  and the database/WAL timestamps were advancing. During this continuation the
  compatible semantic count grew from 2,463 files / 33,181 vectors to 2,535 /
  34,621. The MCP `kb_stats` call timed out after 300 seconds; the local CLI
  fallback returned a consistent committed snapshot after about 78 seconds
  with the existing Pydantic warning. No second reindex was started. These
  statistics do not represent the eventual result of the in-progress job.
  Re-run `kb_stats` after the writer releases its lock and validate updated
  source hashes before claiming KB readiness. KB coverage remains `PARTIAL`.

This repository is an early public alpha. The following boundaries are deliberate and should remain visible in documentation and user-facing output:

- Architecture v2 now defines project schema v7, with a bounded revision-keyed projection-job ledger and an independent cursor for late live-response refresh. Go ingestion commits accepted capture metadata, source checkpoints, and projection jobs in one SQLite transaction. The bounded `projection-worker` command retries jobs and records revision-specific graph mappings; Python owns upgrades of existing databases. A post-commit projection is acknowledged only after local mapping and Neo4j sync report ready. The working runtime database was not migrated or read during the v7 change, so runtime compatibility and the live refresh path remain unverified; Stage 1 remains `PARTIAL`.

- Stage 2 has a shared typed application-query service in `internal/appquery`: `direct` neighborhoods and bounded `full` slices use the same project-selection, seed/budget validation, read-only project open, evidence provenance, local-path/unsafe-locator redaction, and stable result fingerprinting. HTTP calls the typed `Graph` use case directly; MCP maps it to the existing tool wire shape. Successful graph payload fields remain in the existing JSON shape. The web API maps typed failures to safe HTTP statuses without returning internal causes. `bb_graph_neighborhood` remains bounded to 20 unique entity IDs and rejects `assertion-*`, `source-*` and `card-*`; full slices retain their existing budget limits. `setup-hermes` and `run-hermes` add the MCP tools to older managed profiles while preserving unrelated settings; unmanaged profiles are not rewritten. The graph access layer keeps one lazily initialized Neo4j driver per HTTP or MCP process, shares it between direct neighborhoods and full slices, waits for in-flight reads at shutdown, and uses short-lived operation sessions. Neo4j is the default graph backend; SQLite is an explicit compatibility setting. Direct Neo4j reads page candidate assertions so the one-hop slice can fit within its 100-node limit; the opaque cursor is bound to the selected project, entity seed set, and graph generation, and a changed generation returns a stale-cursor conflict. HTTP and MCP accept the optional cursor. The UI merges direct-neighborhood pages with overlap deduplication, while full analytical pages replace UI state with the cumulative server result; a stale cursor triggers a fresh first-page request. SQLite compatibility mode does not issue continuation cursors and returns an explicit unsupported response if given one. Stage 2 remains `PARTIAL`: live-project compatibility, Caido completeness, and focused-view latency acceptance remain unverified. Synthetic Go and JS state tests pass; the opt-in Neo4j test for multiple pages and stale generations was added but not run because `BB_ASSISTANT_NEO4J_TEST` is disabled. No working project database or Caido was used for this code slice.

- The full analytical Neo4j graph view builds a selected-query snapshot from a generation-rechecked native frontier instead of loading the whole project snapshot. It bounds each hop to 128 frontier entities, 100 assertions per entity, 1,000 assertions overall and 200 cards; card endpoint entities are loaded as dependencies, and entity provenance is read in batches of at most 100 owners. Exact-limit probes distinguish a full page from omitted candidates. Its generation-pinned continuation resumes renderer-omitted assertion branches by merging selected records and rerunning the shared Reader from the original seeds; unprocessed queue tails survive each page, while a capped queue stays explicitly incomplete. It does not page native assertion, frontier, card, or provenance candidate caps; these limits and bounded closure remain explicitly `PARTIAL`. Truncated omission catalogs remain marked as truncated and cannot report branch completion. Prepared full-page responses are capped at the MCP graph result limit (3 MiB), cached exactly for retry, and cursor sessions are bounded by active-session count, completed-session eviction, branch count, and a 15-minute TTL. Expiry during a read, generation changes, and service clearing invalidate the session before it can publish a new cursor. Synthetic Go and JS state tests pass; the opt-in Neo4j query/integration checks were not run, and Neo4j query-plan cost and the proposed focused-view p95 remain unmeasured. Caido and the working project database were not accessed for this code slice.

- The last live-data compatibility check exercised Caido 0.58.3, the pinned community MCP bridge v4.3.0, capture schema v5, and BehaviorGraph schema v2 in one development environment. That import enumerated 1,369 historical requests, and a separate read-only reconciliation matched all 1,369 Caido IDs to the local index. The source now defines capture schema v6; the working database remains at v5 and was not migrated during this change. Coverage remains `PARTIAL` because Caido supplies no stable snapshot token; compatibility across Caido versions and workspaces is not established.
- Caido does not currently provide a stable history snapshot token to this workflow. A completed metadata enumeration therefore cannot prove that the source was unchanged throughout the read; reconciliation reports `PARTIAL`.
- Some imported exchanges have incomplete parse or response-body coverage. HTTP status or endpoint presence alone is not a complete behavioral analysis.
- Browser walkthrough and JavaScript endpoint coverage are not yet part of a complete end-to-end application-map workflow.
- The project-local KB is an FTS5-only `PARTIAL` snapshot with 14,300 documents and 16,961 chunks across five populated families. Its source documents, chunk JSONL, and manifest were recovered from the runtime SQLite index on 2026-10-22; the original coverage-report hash and snapshot ID matched exactly, source hashes and deterministic chunk spans were revalidated, and the production importer passed against a disposable index. The working index was unchanged. NVD remains `PARTIAL` after a full-window provider failure. ExploitDB remains `BLOCKED`: the pinned repository provides a GPL-2.0-or-later license, while the separate official site Terms restrict reproduction of service content; the rights applying to the CSV metadata subset have not been resolved ([pinned license](https://gitlab.com/exploit-database/exploitdb/-/blob/6a57361db10b709c4b4f339c09ee250acc2d8aac/LICENSE.md), [Terms of Service](https://www.exploit-db.com/terms)). Snapshot files remain private and outside Git; no compiled corpus is included in Git.
- Hermes is an experimental harness integration; it is not a supported, end-to-end hypothesis-analysis loop.
- Candidate hypotheses and graph relationships are not confirmed findings. Evidence validation and human review remain necessary.

## Latest local integration checks (2026-10-22–23)

- Neo4j Community is running locally in Docker with loopback-only Bolt/Browser ports and data outside the checkout. The selected project graph was backfilled from the validated SQLite BehaviorGraph and read back with matching SHA-256 `9cdbb3c21112782005040a267a1b5310a6a5fee81e605ebe3abbda56f7f77d87` (13,223 entities, 13,551 assertions, 477 cards and 477 sources). The `scripts/bb-assistant` and MCP launchers now select Neo4j; SQLite remains the local capture catalog, recovery graph, and durable outbox. A read-only web graph request through Neo4j returned 13 entities, 12 assertion edges, and 2 cards for the selected seed; the slice explicitly reported `incomplete=true`. A live Neo4j integration test verified outbox idempotence, merged provenance, and preservation of SQLite's canonical entity projection. That earlier backfill checkpoint preceded the latest Caido history import below.
- Before the latest import, the Go runtime opened the working capture and graph stores read-only and reported capture schema v5 and BehaviorGraph schema v2 compatible. That earlier audit counted 477 captures, all search-indexed and graph-mapped; parsing was complete for 155 and partial for 322. Request-message fidelity was `reconstructed_text` for all 477 captures; response fidelity was reconstructed for 463 and unknown for 14. The imported text is therefore not a byte-exact copy of the original Caido wire messages.
- An initial bounded read-only Caido reconciliation using page size 50 enumerated 1,369 IDs in 28 pages and found 477 matched, 892 Caido-only, 0 local-only, and 0 duplicate IDs. It established the import gap later closed by the historical backfill below. Coverage still cannot be promoted based on a terminal cursor alone.
- Before the subsequent backfill, a read-only HTTP History enumeration found 1,369 IDs over 14 pages and no duplicates; the local audit at that checkpoint still had 477 captures.
- A failed backfill resume exposed a 9,073-byte Caido request line above the former 8 KiB bound. The adapter limit was raised to 64 KiB with a regression test. The full import and current reconciliation results are recorded below.
- On 2026-10-23, historical import resumed from its durable checkpoint and reached a terminal cursor after 34 pages. It stored 892 previously missing requests and retained the original 477, for 1,369 captures total. The Go project audit found compatible capture schema v5 / BehaviorGraph v2; all 1,369 captures are search-indexed and graph-mapped, with 414 complete and 955 partial parses. Request text is reconstructed for all captures; response text is reconstructed for 1,332 and unknown for 37. The read-only Caido reconciliation enumerated 1,369 IDs in 28 pages: 1,369 matched, 0 Caido-only, 0 local-only, and 0 duplicates. Both enumerations reached terminal cursors, but `coverage_reconciled=false` because Caido exposes no stable snapshot token or expected history total. A printable non-token header name is preserved in reconstructed text and causes the graph parser to retain `partial` parsing; when history-list status differs from the detailed response line, the detailed status is retained and the mismatch is recorded as missing evidence. Overall coverage remains `PARTIAL`.
- A fresh read-only inventory on 2026-10-23 confirmed the registered Juice Shop project was already selected in the connected Caido app, enumerated 1,369 request IDs in 14 pages, and matched them against local SQLite: 1,369 matched, 0 Caido-only, 0 local-only, and 0 duplicates. No stable snapshot token or expected total was exposed, so `coverage_reconciled=false` and coverage remains `PARTIAL`. An attempted Go CLI `caido-reconcile` did not connect because its runtime configuration lacked the expected SHA-256 for the pinned Caido MCP executable; the app-tool enumeration is separate evidence and does not verify that CLI bridge startup.
- An earlier read-only `neo4j-backfill --verify-only` returned `state=verified` with matching local/Neo4j digest `9cdbb3c21112782005040a267a1b5310a6a5fee81e605ebe3abbda56f7f77d87`, 13,223 entities, 13,551 assertions, 477 cards, and 477 sources. `neo4j-sync` then reported `ready`, with 0 events applied and 0 pending.
- After the latest import, read-only `neo4j-backfill --verify-only` returned `state=verified` with matching local/Neo4j snapshot digest `dc1b5523144061fd1deb0dfc19a536896bf355f7832c4a3b15b4d24f2c9388f8`: 25,064 entities, 25,685 assertions, 1,369 cards, and 1,369 sources. This confirms graph parity; it does not establish Caido snapshot completeness.
- A repeat read-only `neo4j-backfill --verify-only` on 2026-10-23 revalidated the same digest and record counts in about 64 seconds. This is the end-to-end local snapshot/parity check duration, not an isolated Neo4j query benchmark; `coverage=source_sync_not_assessed` remains unchanged.
- The local historical index contains 1,369 captures; the live index is empty. A prior bounded live-tail cycle completed one page with `idle` and 0 new requests. This verifies only the no-new-data path; ingestion of new live traffic and a persistent background worker are not verified.
- The project-local KB snapshot has 5 populated source families, 14,300 documents, and 16,961 FTS5 chunks and reports `PARTIAL`. Its private `source-documents.jsonl`, `chunks.jsonl`, and manifest were reconstructed from SQLite and passed exact snapshot-ID, coverage-report, source-hash, deterministic chunk-span, Python production-importer, and Go `kb-validate` checks; the latter returned `validated=true`. SQLite returned `integrity_check=ok` and zero foreign-key violations, and the working index bytes were unchanged. NVD coverage remains `PARTIAL` after `provider_failed:ValueError`; ExploitDB is `BLOCKED` pending dataset-terms review. The generated Hermes profile carries the configured private data root and optional KB index path; a read-only MCP `bb_kb_stats` call returned the expected snapshot ID and counts. Source text was processed only inside the local validation process and was not printed.
- A separate one-day NVD API canary for the 2026-10-21/22 window fetched and parsed all 531 expected CVE rows and recorded two notices. A later read-only 730-day diagnostic parsed 12,000 records in its first 120-day window, then received an HTTP 200 body that was not valid UTF-8 JSON at offset 12,000. A separate request for that offset in the current rolling window returned a valid 500-record page; the original exact timestamp parameters were not retained, so this was not an exact replay. No full-window reconciliation or KB import completed; NVD remains `PARTIAL`.
- Go KB regression tests now build a synthetic database with the Python FTS5 schema and verify stats, filtered search, source retrieval, truncation and content-hash behavior. They also assert that read operations leave the index bytes unchanged and that a missing index is not created.
- A fresh local web runtime check returned HTTP 200 for the UI and project API. A bounded surface request returned 2 rows and `partial` coverage. A graph slice returned 80 nodes and 90 edges with `incomplete=true`; the first cold request after the one-minute graph-reader cache expired took 50.96 seconds, while the same warm-cache request took 0.245 seconds. On a cache miss the current Neo4j reader loads and validates the full project snapshot before slicing; this is a measured local latency, not a general benchmark.
- A repeated read-only probe of the live `/api/graph` endpoint returned the same 80-node/90-edge slice with `incomplete=true` and `truncated=true`; the cold-or-expired-cache request took 51.256 seconds and the warm request took 0.256 seconds. The associated surface API still reports `partial` coverage. This confirms the cold-path latency is reproducible on the current local dataset.
- A fresh loopback-only `/api/graph` probe on 2026-10-23 took 102.377 seconds on a cache miss and 0.342 seconds on the immediate repeat. Both responses returned 80 entities and 90 assertions with `incomplete=true` and `truncated=true`; the surface remained `partial`. The cache-miss latency is now observed above 100 seconds on the current local dataset.
- A current read-only `neo4j-backfill --verify-only` completed with `state=verified` in 35.763 seconds. It matched the local BehaviorGraph snapshot at 13,223 entities, 13,551 assertions, 477 cards, and 477 sources. Its result still reports `coverage=source_sync_not_assessed`; Neo4j parity does not establish complete Caido synchronization.
- On 2026-10-23 the previously stopped loopback UI was started with `scripts/bb-assistant web-ui`. `/`, `/api/projects`, and `/api/surface` all returned HTTP 200; the selected project surface still reports `partial` coverage. The first graph request after server start took 50.649 seconds and returned 80 entities, 90 edges, and 77 cards with `incomplete=true` and `truncated=true`; repeating the same request from the warm cache took 0.251 seconds. The UI is available at `http://127.0.0.1:8765/` while its local process remains running.
- Uncached `go test -count=1 ./...`, full race-enabled `go test -race -count=1 ./...`, and `go vet ./...` pass. The opt-in Neo4j integration test passes against the configured local database (`BB_ASSISTANT_NEO4J_TEST=1 go test -count=1 ./internal/neo4jstore`, 1.135 seconds) using an isolated generated project namespace. This verifies the Go driver, project snapshot, and outbox read-back path; it does not verify Caido history completeness or remove the cold graph-read latency.
- Hermes Agent v0.22.4 passed `./scripts/run-hermes mcp test bb_assistant` on 2026-10-23 and discovered exactly the 9 configured read-only tools; `bb_get_capture` returns a redacted structural summary and the sensitive `bb_get_capture_raw` tool is absent. The profile is mode 0600, limits access to the `bb_assistant` MCP server, and requires approval for skill writes. No model/provider is configured, so the model-backed analysis and hypothesis cycle remains unverified. No real capture content was sent to a model.
- A browser walkthrough of the local Graph Explorer on 2026-10-23 confirmed the graph UI reaches 1,000% zoom, but this does not make the current selected-route graph easy to inspect: the `/{parameter}` route group matched 10 requests and rendered 80 nodes/92 edges. Clicking **Open relations from this node** for one selected capture still returned 80 nodes/89 edges with partial coverage; the UI remained in its loading state at 20 seconds and had completed when checked around 50 seconds later. This is consistent with the one-minute cache expiring between sessions and the current reader loading the full Neo4j project snapshot before slicing. Path labels remain masked as `{parameter}`. Graph focusing and readable route grouping remain open usability work.
- On 2026-10-23 the approved [Architecture v2 plan](ARCHITECTURE-V2-PROPOSAL.md) entered implementation. Stage 1 remains `PARTIAL`: project-scoped maintenance locking now serializes Go graph intake/drain/snapshot replacement with the legacy Python historical and live writers on Linux. Python and Go registry writes share the exclusive catalog-directory lease; project writers use an exclusive directory lease and retained `.maintenance.lock`. Go and Python read-only SQLite access holds a shared project lease and uses immutable connections; Go fails closed on SQLite WAL/SHM/journal sidecars, while Python refuses non-empty WAL and rollback journals. Python schema initialization, KB snapshot replacement, and registry updates are protected by the writer lease. Maintenance commands acquire the exclusive lease before opening their capture snapshot; the under-maintenance open path verifies that the lease belongs to the database directory and avoids recursive locking. Go `caido-tail --watch` closes its writable store before polling and retries lease contention on the next poll interval. Python `tail_all_projects` returns a `PARTIAL` receipt for only the busy project and continues to the others; the continuous CLI retries during its next poll cycle. The MCP facade rejects writable project or KB stores. The surface project-switch race is fixed. Historical and live Go ingestion now enqueue revision-keyed jobs in the same transaction as accepted capture metadata and checkpoint advancement, then attempt projection after commit. Applied acknowledgment is project-scoped and is recorded only after projection reports local materialization and Neo4j readiness; a projection failure leaves the job pending while source progress stays committed. If historical replay encounters the same revision first registered by live intake, it reuses the validated live job payload and graph ID; Python history accepts the same registry provenance. Backfill and live-tail receipts expose `projection_pending_pages`. The background consumer and live pending-job retry path, queue lease/retry/backoff, dead-letter inspection/replay, and live response revisions/refresh remain outstanding. Synthetic Go/Python tests cover these failure boundaries plus the existing lock, sidecar, migration, and cross-process cases. No Caido operation or runtime database was used for this slice.
- The interactive Neo4j graph implementation now reads a bounded one-hop neighborhood instead of loading the full project snapshot. On 2026-10-23 a loopback API check against the selected local project returned HTTP 200, 3 entities and 2 assertions for one selected capture, with `coverage_state=partial` and `incomplete=true`. A synthetic Neo4j fixture with more than 33,000 entity provenance links returned the same small graph topology, kept assertion provenance complete, sampled entity provenance deterministically to at most 64 references per entity, and exposed the sampled entity IDs and limitation. The UI code includes capture selection, focus/expand, back navigation, zoom and deeper full-slice navigation; interactive browser review of this revision could not be completed because no browser target was connected. The direct result still does not establish full-project integrity, complete Caido coverage, or an application-level analysis conclusion.
- ECC security review found and closed a projection-ledger integrity gap: queued and stored jobs now require exactly one observation entity whose `capture_id` matches the registered capture. Historical graph mappings and live capture mappings are checked against the canonical queued job in the same transaction before acknowledgment. Regression fixtures reject empty and mismatched bundles, detect a checksummed stored job for another capture, and keep a live job pending if its persisted graph mapping changes after acceptance. Follow-up verification passed `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`, `PYTHONPATH=src python3 -m pytest -q` (105 tests, 8 subtests), and `git diff --check`. This is synthetic local verification; Caido and the working runtime database were not used.
- The revision-keyed projection queue now has a bounded Go consumer and CLI (`projection-worker`) with random lease fencing, payload-digest checks, retry backoff/jitter, and dead-letter handling. Each invocation requires the selected project's active exclusive maintenance lease, holds no capture transaction during graph/Neo4j I/O, and acknowledges only after local mapping and the specific Neo4j outbox bundle are confirmed. Ordered outbox backlog is drained page by page without consuming the target job's retry budget; a later unrelated page failure cannot undo an already-applied target. When Neo4j is disabled the worker reports `backend_disabled` without leasing or incrementing attempts. ECC security and data reviews found no remaining actionable findings after fixes. Verification on 2026-10-23 passed `TMPDIR=/var/tmp go test -count=1 ./...`, `TMPDIR=/var/tmp go test -race -count=1 ./...`, `TMPDIR=/var/tmp go vet ./...`, `TMPDIR=/var/tmp PYTHONPATH=src python3 -m pytest -q` (105 tests, 8 subtests), and `git diff --check`. Tests use synthetic fixtures. Runtime databases and Caido were not accessed; current data compatibility and projection backlog status therefore remain unverified.
- The next Architecture v2 slice adds project schema v7 live-response refresh. Go independently enumerates bounded Caido metadata pages and fetches details only for live captures whose response is still absent; the sweep cursor advances in the same transaction as accepted response revisions and stops at the terminal page. Known pending requests in the live overlap are eligible for refresh in the same cycle, while newly fetched requests are not duplicated. Response changes append revision-keyed graph jobs and update structural search metadata without persisting raw live bodies. Graph slices now resolve live provenance through revision-specific graph mappings, preserving Caido request IDs for older graph nodes and avoiding current-response metadata on older revisions. Python migration from schema v6 backfills the current graph mapping and preserves pre-registry live graph references as explicitly marked legacy mappings. ECC review identified overlap, terminal-page, provenance fallback, and legacy-reference edge cases; regression coverage was added. Final synthetic verification passed `TMPDIR=/var/tmp go test -count=1 ./...`, `TMPDIR=/var/tmp go test -race -count=1 ./...`, `TMPDIR=/var/tmp go vet ./...`, `TMPDIR=/var/tmp PYTHONPATH=src python3 -m pytest -q` (108 tests, 8 subtests), and `git diff --check`. No live Caido operation or working runtime database was used, so Stage 1 and runtime compatibility remain `PARTIAL`.
- An earlier read-only Go audit of copied working-project SQLite files on 2026-10-23 found capture schema v5 while this Go build expected v7. That check did not migrate the working database. After confirming an empty WAL and no rollback journal under the shared project lease, a private temporary shadow copy was upgraded by the existing Python migration path to v7 and discarded after validation.
- A fresh read-only `project-audit` on 2026-10-23 now confirms the selected working project is compatible at capture schema v7 and BehaviorGraph v2: 1,369 captures, all search-indexed and graph-mapped, 25,064 entities, 25,685 assertions, 1,369 sources and cards, and 27,054 source references. Coverage remains `PARTIAL`: 414 captures parse completely, 955 partially, 37 responses have unknown fidelity, and browser/JavaScript coverage is absent.
- A fresh Go `caido-reconcile` run used the verified Caido MCP v4.3.0 binary and read only project/request metadata. It reached the former 100,000-ID ceiling after 1,000 pages: 1,369 matched, 98,631 Caido-only, 0 local-only, 0 duplicates; enumeration was nonterminal and `stop_reason=request_limit`. This proved the selected Caido history exceeded the old bound. With limits extended to 10,000 pages / 1,000,000 IDs, the repeated enumeration reached a terminal page after 1,969 pages and 196,809 Caido request IDs: 1,369 matched, 195,440 Caido-only, 0 local-only, 0 duplicates. Terminal enumeration is not a point-in-time completeness guarantee because Caido exposes no stable snapshot token. No request bodies were fetched or target traffic sent.
- The fresh KB MCP `kb_stats` call timed out after 300 seconds. The documented local CLI fallback completed: 9,878 files, 66,240 chunks/vectors; 2,672 files and 36,267 vectors are compatible with the configured semantic index. It reports 7,206 incomplete, unhashed or wrong-version semantic rows, 0 orphan chunks/vectors, `semantic_degraded=true`, and structural integrity `ok=false`. The lexical tier covers 15,438 files and 67,991 passages. Semantic and lexical last-index timestamps are 2026-10-07 and 2026-10-13. A KB writer and changing DB/WAL were still active; no competing reindex was started. KB status remains `PARTIAL`.
- The approved asynchronous-projection slice removes graph/Neo4j projection from historical and live intake: accepted capture metadata, source checkpoints, and revision-keyed jobs commit before intake returns `pending`; a separate worker owns graph materialization and acknowledgment. `projection-worker --watch --poll-interval 1s` can drain continuously, retries project-lock contention at the poll interval, and emits sanitized state events. A synthetic restart test closes and reopens the project store before processing a pending job. Historical import still holds its exclusive project lease for the full invocation, so the worker cannot drain during that run; bounded import invocations can drain between runs. A managed service and a runtime queue drain are not established. This code slice used synthetic fixtures only; Caido, the working runtime DB, and KB were not accessed. Overall Stage 1 and runtime compatibility remain `PARTIAL`.

Contributors should preserve explicit `PARTIAL` and `BLOCKED` states whenever data, coverage, or source provenance is missing.
