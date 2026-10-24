# Project status and known limitations

## ECC projection-batch correction and runtime check (2026-10-24 05:32Z) — PARTIAL

- ECC review found that the worker CLI applied one deadline to the entire
  `--max-jobs` batch even though the flag is a per-job lease/execution limit.
  The CLI now passes the shutdown context through and lets each delivery keep
  its own lease-bounded projection context. A review also found that watch-mode
  events were buffered until the batch ended and could be lost on a later job
  error. Watch mode now emits each completed job immediately and includes the
  stage timings; regression coverage verifies the callback fires before the
  next projection starts.
- A worker batch opens and validates one BehaviorGraph writer lazily, then
  reuses it for each job while the exclusive project maintenance lease is
  held. Per-job claim, payload digest, lease token, mapping transaction and
  acknowledgment remain independent. The two-job runtime batch applied both
  jobs on attempt 1. Job one spent 141,827 ms opening the graph writer, then
  67 ms importing and 806 ms syncing Neo4j; job two reused the writer (0 ms
  open), importing in 54 ms and syncing Neo4j in 304 ms. Projection totals were
  142,702 ms and 359 ms respectively. Imports and Neo4j syncs are still issued
  per job rather than coalesced.
- Full `TMPDIR=/var/tmp go test -count=1 ./...`, `TMPDIR=/var/tmp go vet ./...`,
  and `TMPDIR=/var/tmp go test -race -count=1 ./internal/ingest
  ./cmd/bb-assistant` passed. Targeted progress/timing tests and
  `git diff --check` also passed.
- Fresh status at 2026-10-24 05:32:23Z reports capture schema v7 and
  BehaviorGraph schema v2 compatible; 1,400 captures, 1,369 mapped, 31
  unmapped, 1,378 pending projection jobs, 22 applied, zero leased, and zero
  dead-lettered. `neo4j-check` returned `ready`. Overall status remains
  `PARTIAL`; history is unreconciled and browser/JavaScript/source coverage is
  incomplete.
- Caido 0.58.3 still has Juice Shop `READY`, but `sperpravo` is the selected
  project. No project switch, request enumeration or body fetch occurred. The
  latest completed global KB audit remains the 04:55Z check: 15,517 compatible
  files and 68,878 chunks/vectors with structural integrity `OK`, while source
  and vector content validation and corpus completeness remain unverified. The
  local immutable snapshot was validated at 04:55Z with 14,300 documents and
  16,961 chunks, but its source coverage remains `PARTIAL`. No KB reindex or
  source change was started.

## Caido whole-project and exact-scope reconciliation (2026-10-24) — PARTIAL

- Installed the pinned Caido MCP v4.3.0 binary in user-local app data after its
  downloaded Linux amd64 artifact matched the SHA-256 published on the pinned
  GitHub release and reported `v4.3.0`. The read-only Go reconciler uses only
  project-list and request-list metadata tools; no request details or bodies
  were fetched. The existing private token file passed ownership/mode checks.
- The read-only reconciliation reached Caido's terminal cursor for the whole
  selected Juice Shop project: 1,969 pages and 196,809 distinct request IDs.
  The local inventory contained 1,400 IDs; all 1,400 matched, 195,409 Caido IDs
  were absent locally, and there were no local-only IDs or duplicates. This is
  an all-project inventory, not a host-scoped result. Caido provides no stable
  snapshot token, so `coverage_reconciled` remains false.
- Caido's configured scope allowlists host `192.168.1.149`; its scope name says
  port 3000 is enforced by the run gate. Added `--scope-host` and `--scope-port`
  to `caido-reconcile`: Caido applies an exact-host HTTPQL filter, the Go client
  verifies each metadata URL's port, and local IDs come from host/port indexes.
  Unparseable Caido URLs and local historical rows without host/port index data
  are counted as unclassified rather than silently treated as outside scope.
- A scoped read-only run reached the terminal cursor in 4 pages: Caido 355,
  local 355, matched 355, Caido-only 0, local-only 0, duplicates 0, and
  unclassified 0. `local_inventory_complete=true`; the prior Caido project
  selection was restored. This is exact ID equality for one enumeration, not
  point-in-time proof: Caido has no stable snapshot token, so
  `coverage_reconciled=false` and project coverage remains `PARTIAL`.
- ECC TDD fixtures cover exact-host filter construction, host/port comparison,
  wrong-port exclusion, malformed source URLs, local unclassified rows,
  invalid scope values, and a database digest check proving the reconciler is
  read-only. `go test -count=1 ./...`, `go vet ./...`, and
  `TMPDIR=/var/tmp go test -race -count=1 ./internal/caido
  ./internal/historyreconcile ./cmd/bb-assistant ./internal/store` all passed.
- Neither reconciliation wrote local database rows or fetched request bodies.
  The 195,409 all-project difference above cannot be attributed to this host;
  31 captures remain unmapped and broader source/browser/JavaScript coverage is
  still incomplete.

## Scoped reconciliation hardening and fresh runtime audit (2026-10-24 04:55Z) — PARTIAL

- ECC regression review found that equivalent IPv6 host strings could be
  missed locally, `unknown` and malformed indexed hosts could be omitted from
  the unclassified count, and scoped zero counters disappeared from JSON. The
  read-only local scan now walks the historical/live project indexes in
  request-ID order under a row limit, canonicalizes IPv6 in Go, and classifies
  missing or malformed host/port records. It does not use `GROUP BY`; the
  composite project/request indexes plus `LIMIT` bound rows read. Unique-ID
  overflow and a capped local scan have distinct stop reasons, and both stop
  before Caido enumeration. All aggregate fields are emitted, including zeroes.
- The same review showed production projection rejected IPv6 host headers.
  Exchange projection, endpoint extraction, URL normalization and historical
  origin construction now canonicalize IPv6. Regression fixtures cover valid
  expanded/bracketed IPv6, malformed DNS/IPv6 hosts, `unknown` sentinels,
  unrelated IPv6 candidates, duplicate request IDs across historical/live
  indexes, unique-ID limits, bounded scans, explicit zero fields, and unchanged
  local database bytes.
- The updated implementation passed `go test -count=1 ./...`, `go vet ./...`,
  and `TMPDIR=/var/tmp go test -race -count=1 ./internal/caido ./internal/graph
  ./internal/historyreconcile ./cmd/bb-assistant ./internal/store`; `git diff
  --check` passed. A fresh static follow-up review found no actionable issue;
  it accessed no runtime store or Caido traffic.
- Fresh `bb-assistant status` at 2026-10-24 04:55:57Z reports capture schema
  v7 and BehaviorGraph schema v2 compatible; 1,400 captures, 1,369 mapped,
  31 unmapped, 1,382 pending projection jobs, 18 applied, zero leased, and
  zero dead-lettered. `neo4j-check` returned `ready`; the web UI returned HTTP
  200. Overall coverage remains `PARTIAL`.
- The project-local KB snapshot `d342e9df…` validated (14,300 documents,
  16,961 chunks), with source coverage still `PARTIAL`. Global `kb_stats`
  returned 15,517 compatible files and 68,878 chunks/vectors, with no missing
  vectors and structural integrity `OK`; the index timestamp is
  `2026-10-24T03:58:19Z`. Source and vector content validation are false, so
  corpus completeness remains unverified. The redundant local stats fallback
  was stopped after 42 seconds of sustained CPU use. No reindex or source
  change was started.
- Caido 0.58.3 reports the Juice Shop project `READY`, but `sperpravo` is
  currently selected. The updated exact-scope command was not rerun against
  Juice Shop because it requires that project to already be selected; the
  preceding 355/355 scoped result predates this correction. No request bodies
  were fetched. The local vault key is still absent, so encrypted historical
  import is unavailable.

## ECC writer-reuse slice and bounded runtime validation (2026-10-24) — PARTIAL

- The historical projection path now reuses one selected-project BehaviorGraph
  writer across local bundle import and targeted Neo4j outbox confirmation,
  including ordered outbox pages. Writer reuse checks the project key and exact
  graph database, BehaviorGraph, and receipt paths. This avoids reopening and
  fully validating the same graph between the import and its outbox ack.
- Synthetic tests cover writer/store path mismatch and verify that SQLite-only
  mode leaves the imported outbox event pending rather than falsely confirming
  Neo4j readiness. Full `go test -count=1 ./...`, `go vet ./...`, and
  `git diff --check` passed before the bounded runtime check; a subsequent
  targeted `go test -race -count=1 ./internal/graph ./internal/graphsync
  ./internal/ingest ./internal/projectlock` also passed. An ECC static security
  review found no actionable issue in the changed files; it did not access
  runtime stores or traffic.
- `neo4j-check` returned `ready`. One bounded `projection-worker --max-jobs 1`
  run applied one job with no retry or dead letter (the recovered lease had
  attempt count 2). A fresh status then reported 1,382 pending, 18 applied,
  zero leased, and zero dead-lettered. This single job confirms the local
  projection → Neo4j outbox → acknowledgment path and took about two minutes;
  the one-job sample is not enough to claim sustained bulk-drain throughput or
  queue parity.
- The same status reported capture schema v7 and BehaviorGraph schema v2 as
  compatible; 1,400 captures, 1,369 graph mappings, and 31 unmapped captures.
  Overall status remains `PARTIAL`: history is unreconciled, source coverage is
  incomplete, and Caido intake was not contacted. No capture bodies were read.

## Stage 6 current runtime and KB recheck (2026-10-24) — PARTIAL

- `bb-assistant status` at 2026-10-24 04:01:52Z reported capture schema v7
  and BehaviorGraph schema v2 compatible; 1,400 captures, 1,369 graph mappings,
  31 unmapped captures, 1,382 pending projection jobs, 1 leased job, 17 applied,
  and 0 dead-lettered. History reconciliation and coverage remain `PARTIAL`.
  Neo4j connection, Caido intake, and model execution were not checked by this
  command. No projection worker was running after the earlier bounded attempt.
- Caido 0.58.3 reported the registered Juice Shop project `READY`, but it was
  not the selected project. No project switch, request enumeration, or body
  fetch was performed. `BB_ASSISTANT_VAULT_KEY` was absent from the process
  environment and the default private runtime environment file; encrypted
  historical import remains blocked on that key.
- The project-local immutable KB snapshot passed `bb-assistant kb-validate`:
  snapshot `d342e9df…`, 14,300 documents and 16,961 chunks. A local
  `kb-search` query returned three cited results. Snapshot source coverage is
  still `PARTIAL`; this validates the current snapshot and lookup path, not
  corpus completeness or a model-backed consultation.
- The separate global KB statistics fallback did not finish during this
  check. Its two launched read-only processes were interrupted after both used
  sustained CPU, and process inspection confirmed they had exited. No KB
  reindex or source change was started; current global KB statistics remain
  unverified by this attempt.

## Stage 6 graph backlog runtime attempt (2026-10-23) — PARTIAL

- After the earlier status snapshot, the bounded projection worker applied 2
  jobs, moving the queue from 15 to 17 applied. The batch was stopped after
  measuring very low throughput; current status reports 1,382 pending, 1
  expired lease, and 17 applied. No worker process is active. The expired job
  remains recoverable by the next worker invocation.
- A read-only `projection-reconcile` confirmed identical local and Neo4j
  snapshot digests (`6b121586e8b437953019ddb93d50d4e06c00a4bad5fd3b7d0015258899b43747`).
  Its ten-minute limit expired after 109 jobs / 54,401,457 payload bytes; all
  109 conflicted with the current snapshot, with no exact matches, missing
  records, or invalid jobs in that scanned prefix. It did not apply anything,
  and the rest of the queue remains unclassified by this run. The queue cannot
  be called reconciled from these results.
- The tested worker batch took about 13 minutes to apply 2 jobs, which is too
  little throughput for a bulk drain. Code inspection shows that each job opens
  and fully validates BehaviorGraph for import and opens it again for targeted
  outbox synchronization; outbox pages can reopen it further. Profiling has
  not yet isolated the time spent in each stage. Optimize reuse while preserving
  per-job leases and acknowledgments, then measure with synthetic data before
  retrying a larger runtime batch.
- The final read-only status passed structural compatibility: schema v7/v2,
  1,400 captures, 1,369 mapped, 31 unmapped, and partial history coverage. No
  Caido import, body read, graph snapshot replacement, or reconciliation ack
  occurred in this attempt.

## Stage 6 maintenance barrier and current runtime recheck (2026-10-23) — PARTIAL

- ECC-guided TDD reproduced a consultation write succeeding while the project
  held its exclusive maintenance lease. All six state-changing consultation
  operations now hold a shared lease from before opening/accessing their
  project database through completion: session creation, review acceptance,
  comparison, proposal, citation validation, and candidate save. A maintenance
  snapshot therefore waits for in-flight consultation writes and prevents new
  ones on Linux. Platforms without the lock primitive keep consultation
  behavior; maintenance operations must fail closed when they cannot acquire
  the exclusive lease.
- Synthetic tests verified the timeout under the exclusive lease and retry
  using the same idempotency key after release. `go test -count=1 ./...`,
  `go test -race -count=1 ./internal/consultation ./internal/projectlock`,
  `go vet ./...`, and `git diff --check` passed. This is only the consistency
  barrier; no backup or restore command exists yet.
- A fresh `bb-assistant status` read 1,400 captures, 1,369 graph mappings, 31
  unmapped captures, and 1,385 pending projection jobs (15 applied, none
  leased or dead-lettered). Capture schema v7 and BehaviorGraph schema v2 were
  compatible. Coverage and history reconciliation remain `PARTIAL`.
- `bb-assistant neo4j-check` returned `ready` for the configured Neo4j database
  and ensured the application's idempotent schema. This verifies connection
  and schema availability, but not queue parity or drain. The status command
  intentionally continues to report connectivity as `not_checked`.
- The local web UI process is running and returned HTTP 200. User-service
  supervision could not be inspected because this shell has no systemd user
  bus, so managed startup is unverified. Caido's Juice Shop project is ready
  but not selected; no project switch, request enumeration, or body fetch was
  done. The local vault key is absent, so encrypted historical import remains
  unavailable until that runtime prerequisite is supplied.
- The local KB fallback reports 15,517 compatible files, 68,878 chunks and
  vectors, zero missing vectors, and `semantic_degraded=false`. Structural
  integrity passed, while source and vector content validation are still
  unverified. MCP `kb_stats` timed out after 300 seconds; the documented local
  fallback completed successfully. KB remains outside the approved feature
  scope except for this health audit.
- Stage 6 remains `PARTIAL`: backup/restore, managed startup, projection queue
  drain, Caido live intake/restart replay, and restored receipt verification
  are outstanding. No live Caido request bodies were read.

## Stage 6 local capability status slice (2026-10-23) — PARTIAL

- Added `bb-assistant status`, a read-only JSON report for graph configuration,
  registered project schema/coverage, local live/historical counters, and
  projection-queue states. It labels Neo4j connectivity as unchecked, Caido as
  not contacted, and model execution as not invoked. It does not read message
  bodies, vault objects, display names, Caido IDs, or credentials. Overall
  status stays `partial` while source reconciliation and coverage gaps remain.
- `project-audit` now includes pending, leased, applied, and dead-letter
  projection counts from the same read-only SQLite snapshot. The shared Go
  launcher can run `status` before Neo4j credentials are configured; normal
  Neo4j-backed commands still require the private runtime file.
- Added `docs/CURRENT-CAPABILITIES.md` as the current report guide and linked it
  from the local setup/runbook. The machine-specific JSON remains outside Git
  and must be regenerated with `./scripts/bb-assistant status` when checking a
  runtime.
- Synthetic checks cover exact queue-state counts, private-field omission,
  partial status, and launcher behavior with or without Neo4j configuration.
  The CLI was also run once as a read-only audit of the selected local store;
  it confirmed supported capture/BehaviorGraph schemas and partial coverage.
  The report omitted per-machine identifiers from committed docs and did not
  inspect message bodies or contact Caido, Neo4j, or a model provider.
- Stage 6 remains `PARTIAL`: supervised Go service cutover, backup/restore,
  runtime live intake, and restart/replay against the selected data still need
  separate evidence. See [CURRENT-CAPABILITIES.md](CURRENT-CAPABILITIES.md).

## Stage 5 consultation implementation (2026-10-23) — PARTIAL

- Added a project-scoped SQLite consultation store with schema-v1 migration,
  explicit operator-review events, immutable comparisons/proposals/receipts,
  and stable candidate IDs with per-candidate version sequences. The migration
  preserves older candidate history and restores append-only application
  triggers. These triggers are not a tamper-evident protection against direct
  local filesystem edits.
- The local UI now creates a session from the reviewed packet preview, shows
  the saved redacted question and excerpts, and requires a separate operator
  acceptance action. Before acceptance Hermes sees neither the question nor
  excerpts, hashes, or redaction spans; MCP has no acceptance tool and all
  assistant write operations remain blocked. After acceptance the user can
  copy the project/session arguments for `bb_consult_resume`.
- MCP now exposes create, exact compare, proposal, citation validation,
  candidate save, and resume operations. Compare summaries bind their nested
  request/response sides to the selected revision IDs and validate bounded
  hashes/sizes. Unavailable comparisons cannot claim equality or carry
  malformed partial metadata; valid oversized-source metadata remains visible.
  Citation validation rereads exact selected spans and reruns cited
  comparisons; a receipt proves reference integrity only, not semantic support
  or impact. Sessions are currently limited to one project.
- Migrated version-1 sessions remain unreviewed. Until an operator accepts the
  saved packet, Hermes responses also hide any legacy comparisons, proposals,
  receipts and candidate rows, and all assistant mutations reject before
  idempotency replay. Legacy candidate-save replays resolve the stable ID from
  the canonical append-only candidate row.
- Synthetic tests ran the complete create → review → accept → compare →
  proposal → revalidate → candidate → resume cycle. A separate encrypted
  fixture used the real appquery resolver: exact evidence validated, then
  validation failed after the fixture's exact vault blob was removed. A v1→v2
  migration fixture resumes an unreviewed legacy session, checks its operator
  preview, then validates canonical candidate-ID recovery on an old idempotent
  save. These checks use synthetic data and do not validate the existing
  runtime vault, local Caido databases, or Caido synchronization.
- `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`,
  `PYTHONPATH=src python3 -m pytest -q` (110 passed, 8 subtests), the four
  Hermes setup tests, JS/shell syntax checks and `git diff --check` pass. The
  private Hermes profile was reconfigured and `./scripts/run-hermes mcp test
  bb_assistant` discovered 15 tools. No provider/model is set, no model call
  occurred, and no real captured content was sent to Hermes.
- Overall remains `PARTIAL`: current runtime database compatibility, complete
  Caido synchronization, KB integrity, authenticated actor/object analysis,
  model-backed evaluation, and managed startup/recovery are unverified. See
  [CONSULTATION.md](CONSULTATION.md) for the workflow and data boundary.

## Latest verified checkpoint (2026-10-23)

This checkpoint supersedes conflicting runtime measurements in the older
same-day sections below. The overall runtime, source coverage, and analysis
loop remain `PARTIAL`.

- A fresh local read-only `project-audit` passed after verified zero-byte WAL
  and inactive SHM sidecars were cleared under the exclusive project lock.
  Capture schema v7 and BehaviorGraph v2 are compatible. It found 1,400
  captures and search-index entries, 1,369 graph mappings, and 31 unmapped
  captures. Parsing is complete for 444 and partial for 956; response fidelity
  is unknown for 37. Request text is reconstructed for 1,400 captures,
  response text for 1,363, and no source-byte fidelity was established.
  Historical synchronization and overall coverage are `PARTIAL`.
- The running loopback web UI returned HTTP 200 for `/api/projects` and the
  selected project's `/api/surface` API. The surface catalog reports 131 local
  operations, `coverage_state=partial`, and a continuation cursor. This
  confirms the current local catalog can be served; it does not establish full
  Caido coverage or complete browser/JavaScript mapping. A direct graph query
  for one sampled seed returned 3 entities, 2 assertions, and 1 source; its
  slice is `incomplete=true` with 2 omitted edges and `truncated=false`.
  The bounded full-view query returned 4 entities, 3 assertions, 1 source, and
  1 card; it is also marked incomplete without truncation. These results verify
  that graph data is served, not that the selected operation has complete
  evidence closure.
- A read-only `neo4j-backfill --verify-only`, rerun after reconciliation,
  reports `verified`: local and Neo4j snapshot digests both equal
  `6b121586e8b437953019ddb93d50d4e06c00a4bad5fd3b7d0015258899b43747`, with
  33,366 entities, 33,987 assertions, 1,369 cards, and 1,369 sources. This
  verifies graph parity for the mapped local snapshot only; it does not prove
  Caido history completeness or source coverage.
- ECC added `projection-reconcile`, a dry-run-first CLI that requires full
  local/Neo4j snapshot parity and only acknowledges exact normalized projection
  bundles. The live dry-run checked all 1,393 pending jobs (251,157,396 payload
  bytes): 8 were exact, 31 had missing records, and 1,354 conflicted with
  current records. Apply acknowledged only the 8 exact jobs; 1,385 remain
  pending. It also persists their revision mappings. No graph records or Caido
  data were changed. The local behavior outbox has 45 applied entries and none
  pending. Neo4j parity is not proof that source history is complete.
- Synthetic tests cover dry-run non-mutation, exact-only acknowledgement,
  cursor bounds, parity mismatch, project-lock enforcement, and digest-fenced
  state changes. `go test -count=1 ./...`, `go test -race -count=1 ./...`,
  `go vet ./...`, and `git diff --check` passed for this implementation. The
  queue remains `PARTIAL`; the 1,385 remaining jobs require separate handling.
- Caido 0.58.3 lists the Juice Shop project as `READY` but inactive; the
  selected project is the unrelated `sperpravo` project. No project switch,
  request enumeration, body read, replay, or target traffic was performed.
  Caido reconciliation remains unverified.
- The latest local KB fallback snapshot reports 15,536 semantic files and
  77,471 vectors; 14,471 files and 61,380 vectors are compatible. Semantic
  integrity remains degraded (`ok=false`) with 1,065 wrong-version and
  unhashed rows; the stored index version is null and last semantic index is
  2026-10-07. Lexical search is available with 15,438 files and 67,991
  passages; its last refresh was 2026-10-13. The incremental
  `kb_reindex(force=false)` call timed out after 300 seconds, but the KB
  database continues to grow and compatible-file counts rose by 82 since the
  prior snapshot. A live indexer still holds the exclusive index lock with
  four worker connections to the configured Ollama endpoint; `/api/tags`
  returned HTTP 200. A duplicate local start was rejected by the lock before
  changing data. Completion is unconfirmed; KB readiness remains `PARTIAL`.
- `./scripts/run-hermes mcp test bb_assistant` passed and discovered the
  nine-tool read-only local MCP allowlist. The private Hermes profile is mode
  0600 and has no provider/model selection. A model-backed analysis and
  hypothesis-validation cycle remains unverified.
- ECC fixed the Neo4j analytical-slice omission: the bounded frontier now
  follows both entity and assertion records, so linked assertion-to-assertion
  `contradicts` evidence is included. The neighborhood integration fixture now
  seeds the entity-only API with an entity and checks its assertion dependency.
  The regression reproduced before the production change. Afterward,
  `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`,
  and the full opt-in `go test -count=1 ./internal/neo4jstore` all passed.
  This verifies the tested graph-closure behavior, not full source coverage or
  end-to-end Caido synchronization.
- Browser walkthrough, JavaScript endpoint coverage, and the end-to-end
  Hermes analysis loop are still absent. Keep product and coverage claims
  `PARTIAL` until those gaps are closed and verified.

## Fresh compatibility and KB recheck (2026-10-23 22:22 -0400)

- A new read-only `project-audit` reports capture schema v7 and BehaviorGraph
  schema v2 compatible. The selected local Juice Shop store has 1,400 captures
  and search entries: 1,369 are graph-mapped, 31 are unmapped, 444 parse
  complete, 956 partial, and none unparsed. The BehaviorGraph contains 33,366
  entities, 33,987 assertions, 1,369 sources, and 1,369 cards. Historical
  synchronization is `partial`; `history_reconciled=false`. The audit reads
  structural metadata only, not captured bodies.
- A fresh `neo4j-backfill --verify-only` returned `verified` for the selected
  local BehaviorGraph and Neo4j snapshot, with 33,366 entities, 33,987
  assertions, 1,369 sources, and 1,369 cards. Its coverage field remains
  `source_sync_not_assessed`; parity does not reconcile imported Caido history.
  Verify-only did not replace the snapshot.
- Caido 0.58.3 lists the Juice Shop project as `READY` but inactive; the
  selected project is `sperpravo`. Only the project list was read. No project
  switch, history enumeration, body read, replay, or target request occurred.
- The connected `kb_stats` request timed out after 300 seconds. The documented
  local `query-kb.sh stats` fallback completed read-only: 15,591 semantic files,
  79,376 chunks/vectors, 15,381 compatible files and 67,382 compatible
  vectors. Semantic integrity is degraded: 210 files have a wrong index
  version and no source hash, 210 file chunk counts are incomplete, and the
  stored index version is null. The semantic index timestamp is 2026-10-07.
  Lexical search is available with 15,438 files and 67,991 passages, last
  refreshed 2026-10-13. KB readiness remains `PARTIAL`.
- The packet builder now accumulates up to four exact spans from multiple
  historical captures in one project (4 KiB per span, 8 KiB total), clears on
  project switch, and renders only the redacted preview locally. It still does
  not call a model; the Hermes/provider cycle is Stage 5. Synthetic Go, race,
  and vet checks passed for this implementation; all 23 browser-state tests
  passed. Runtime decryption still lacks a configured vault key.

## Stage 4 local evidence preview (2026-10-23 21:31 -0400)

- Added a typed Go evidence-range use case backed by the authenticated local
  encrypted-vault reader. It can resolve the current historical revision or
  read a specific immutable revision by ID; callers can page without mixing
  bytes if a newer revision arrives. It does not contact Caido.
- The loopback web UI now has an explicit **Open stored HTTP** action on
  locally encrypted historical provenance. Request and response tabs read at
  most 1 MiB per range and show no more than 2 MiB per side. The dialog reports
  revision, full-message SHA-256, size, capture time, wire fidelity, parser
  state, and truncation; it marks raw values as sensitive. Content uses
  `textContent`, responses are `no-store`, and the evidence method is absent
  from MCP dispatch and the Hermes tool catalog. Live-only or unavailable raw
  messages stay unavailable in this viewer.
- New tests cover reading an older exact revision after the current capture
  changes, invalid bounds/project selection, API query validation/cache headers,
  bounded JSON/base64 fallback, exact revision pinning in the browser, project
  clearing, and live-only source refusal. Sequential UTF-8 pages now end on
  rune boundaries while preserving
  the exact byte sequence; arbitrary offsets still return exact requested
  bytes. `go test -count=1 ./...`, `go test -race -count=1 ./...`,
  `go vet ./...`, the 19 browser state tests, `node --check`, and
  `git diff --check` pass. These checks validate local historical evidence
  handling only; they do not establish complete response inspection or a
  two-capture comparison.
- A browser smoke test with the configured project root and Neo4j settings
  loaded the local surface (131 operations), opened a focused Juice Shop graph,
  and displayed the explicit evidence action. The selected stored message could
  not be decrypted: the existing runtime environment has no
  `BB_ASSISTANT_VAULT_KEY`. The dialog failed closed, cleared its contents, and
  now identifies that the local vault key is unavailable or invalid.
  This confirms the UI path and safe refusal, not successful runtime access to
  the existing encrypted vault; a local key must be configured before that
  can be verified.
- Fresh read-only `project-audit` confirmed capture schema v7 and BehaviorGraph
  v2 compatible: 1,400 captures, 1,369 graph mappings, 31 unmapped, 444 complete
  parses, 956 partial parses, and 37 unknown response fidelities. Neo4j
  `--verify-only` matched the selected local graph digest
  `6b121586e8b437953019ddb93d50d4e06c00a4bad5fd3b7d0015258899b43747` at
  33,366 entities, 33,987 assertions, 1,369 cards, and 1,369 sources. This is
  parity for the mapped local graph, not Caido source-history completeness.
- A sequential fresh check repeated the same graph digest and counts. The
  project audit again reports 1,400 captures, 1,369 mapped, 31 unmapped, zero
  live-indexed rows, `history_reconciled=false`, and overall `PARTIAL`; its
  report explicitly covers structural metadata and aggregates only.
- Caido 0.58.3 lists the Juice Shop project `READY` but inactive; the currently
  selected project is `sperpravo`. No project switch, history read, body read,
  replay, or target traffic was performed. Hermes v0.22.4 connected and
  discovered the configured nine read-only tools; no provider/model is set.
- The KB MCP `kb_stats` request timed out after 300 seconds, but the existing
  read-only local fallback completed at 21:19. It reported 15,536 semantic
  files and 77,864 vectors; 14,660 files and 62,677 vectors match the configured
  version and source hashes. Integrity is still degraded: 876 rows have a
  wrong index version and no source hash, and source/vector contents were not
  validated. The lexical tier has 15,438 files and 67,991 passages, last
  refreshed 2026-10-13; semantic indexing last completed 2026-10-07. The live
  KB server/indexer process remains active, so counts are a point-in-time
  snapshot rather than a completion signal. No competing reindex was started;
  KB integrity and readiness remain `PARTIAL`.
- Stage 4 remains `PARTIAL`: the UI only reads bounded local historical ranges.
  Deterministic two-capture comparison, full evidence coverage, and bounded
  model packets are still outstanding. Overall project coverage also remains
  `PARTIAL` because source reconciliation, browser/JavaScript coverage, the
  model-backed consultation cycle, and managed startup/recovery are not proven.

## Stage 4 deterministic local capture comparison (2026-10-23 21:40 -0400)

- The graph inspector can now pin an exact locally encrypted historical
  revision as a baseline and compare another exact revision from the same
  project. The local API decrypts request and response bytes read-only, compares
  at most 1 MiB per message side, and reports equality or the first differing
  byte with bounded escaped context. It does not contact Caido or expose a
  comparison tool to Hermes/MCP; the UI renders results with `textContent` and
  uses the existing sensitive-values warning.
- Both revisions' request IDs, revision IDs, hashes, sizes, capture times,
  parser state, wire fidelity, and truncation flags are shown when available,
  including when stored bytes match. A byte match therefore does not imply a
  complete capture. Missing bytes and messages above the limit are marked
  unavailable. Baselines are cleared when the selected project changes.
- Synthetic Go and browser-state coverage verifies exact revision parameters,
  strict query validation, no-store responses, equality/difference metadata,
  first-difference context, unavailability, truncation visibility, project
  switch cleanup, and UI text rendering. `go test -count=1 ./...`,
  `go test -race -count=1 ./...`, `go vet ./...`, `node --check`, the 20
  browser-state tests, and `git diff --check` pass.
- Runtime decryption of the existing vault is still unverified because the
  running local service has no configured `BB_ASSISTANT_VAULT_KEY`; no key was
  recovered or copied. Runtime source reconciliation, complete browser/JS
  coverage, bounded model packets, and the Hermes consultation cycle remain
  outstanding. Stage 4 and overall coverage stay `PARTIAL`.

## Stage 4 bounded evidence packet preview (2026-10-23)

- Added an explicit local preview for an analyst question plus byte spans from
  exact encrypted historical revisions. The API is capped at four spans, 4 KiB
  per span, and 8 KiB selected in total. It retains revision IDs, source hashes,
  absolute byte offsets, capture/parser/fidelity/truncation metadata, and
  reports omission totals as unknown when the exact source is unavailable.
  For messages up to 1 MiB, redaction scans the complete stored message before
  extracting a selected span. Larger messages are rejected from indexed size
  before the vault file is opened, then omitted with their known size. Their
  indexed digest is labeled `indexed_only`; only a successfully decrypted and
  hash-checked message is labeled `verified`.
- The question and selected UTF-8 excerpts pass through heuristic redaction for
  common credential headers and assignments, bearer/JWT/private-key material,
  prefixed secret keys, quoted multiword values, email addresses, and labeled
  PII fields. A partial selection overlapping a detected secret is replaced
  in full. A final public-data validation gate rejects unsafe questions and
  omits excerpts that still fail validation. Redaction remains heuristic and
  each preview is marked for operator review.
- The loopback API accepts only bounded JSON from a same-origin POST, disables
  caching, and renders the result as text in the local UI. This operation does
  not call a model and is not registered in MCP/Hermes. The provider remains
  unset. The UI can accumulate up to four exact spans across historical
  captures in the selected project, remove individual spans, and preview the
  redacted packet. It clears the draft on project switch and blocks additions
  beyond 8 KiB total before posting.
- Synthetic unit and service tests exercise encrypted fixture reads, exact
  revision binding, source offsets, redaction, missing-capture accounting,
  request limits, multi-capture packet assembly, project-switch invalidation,
  and local rendering. Runtime decryption of the existing
  project vault is still unverified because the running service lacks
  `BB_ASSISTANT_VAULT_KEY`; no real capture was read or sent to a model.
  Stage 4 and overall coverage remain `PARTIAL` pending configured vault
  access and the later Hermes consultation cycle.

## Fresh local verification (2026-10-23, after `b4f460e`)

This checkpoint supersedes earlier runtime claims below where they conflict;
older audit entries describe their own historical snapshots.

## Live service recheck (2026-10-23)

- Caido 0.58.3 still lists the Juice Shop project as `READY` but inactive;
  `sperpravo` remains selected. No project switch or history request was made.
- The local web UI is running and `/api/projects` returns the registered Juice
  Shop identity. A fresh `project-audit` fails closed with
  `database has a SQLite sidecar; read-only open refused`; the selected
  project's `/api/surface` returns HTTP 503 (`selected project is unavailable
  for graph reads`). SQLite WAL/SHM sidecars are present. Current working-DB
  compatibility and Neo4j parity were therefore not re-established in this
  recheck; use the prior verified snapshot only as dated evidence. Runtime
  coverage remains `PARTIAL`.
- The connected `kb_stats` call timed out after 300 seconds. The documented
  local `query-kb.sh stats` fallback did not finish within two minutes and was
  stopped without requesting a refresh or changing the KB. The completed
  same-day snapshot below remains the latest available coverage evidence;
  current semantic integrity is unverified and KB readiness remains `PARTIAL`.

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

## Stage 2 CLI graph continuation (2026-10-23)

- The graph-slice CLI now uses the shared typed appquery service and accepts
  a full-view continuation cursor. To resume across short-lived CLI processes,
  it stores the bounded cursor session in a separate query-state SQLite
  database under the private data root. The directory and database are
  owner-only (0700 and 0600); the project capture and graph databases stay
  read-only. Cursor state expires after 15 minutes, is capped at 16 sessions,
  expired rows are pruned when the store opens, and SQLite secure-delete is
  enabled. The store preserves idempotent replay of the latest accepted page.
- Synthetic tests cover service-store restart, continuation after restart,
  replay after another restart, expired-state cleanup, concurrent advancement
  from two stores, and cursor forwarding through two fresh CLI invocations.
  The CLI fixture also confirms query-state permissions and context forwarding.
  No working Neo4j continuation was exercised here; Stage 2 and runtime
  coverage remain `PARTIAL`.

## Stage 2 signature-filtered capture search (2026-10-23)

- Historical request/response signature filters now use the caller-owned
  `graphaccess.Service`. Neo4j reads exact signatures through project-scoped
  variant indexes and follows only matching assertion links to observation
  IDs; the query is capped at the requested maximum plus one row and does not
  transfer `payload_json` or reconstruct a full-project Reader. MCP passes its
  own graph service into capture search; the CLI compatibility entry point
  uses the process service.
- Snapshot projection now promotes keyed request/response signatures to Neo4j
  properties and creates composite project/kind/signature indexes. A
  `signature_index_version` plus `signature_index_generation` marker binds the
  index to the current graph generation. Signature search fails with a
  specific reproject-required message if the marker is absent or stale,
  preventing false empty results after a graph update from an older writer.
  Incremental outbox writes advance the marker only when it matched the prior
  graph generation; they cannot revalidate an index made stale by an older
  writer. The opt-in Neo4j integration fixture exercises both a successful
  indexed match and a stale-marker transition, but it was not run in this
  environment.
- Focused synthetic Go tests passed for the native-query contract, signature
  property projection, service ownership, and avoidance of full snapshots.
  Working Neo4j query plans and the existing project snapshot were not checked
  or reprojected; capture search against the live graph remains unverified and
  Stage 2 remains `PARTIAL`. Caido was not queried for this code slice.

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
