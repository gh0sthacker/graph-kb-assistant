# Project status and known limitations

## Stage 3 project URL references across import pages — PARTIAL (2026-10-25)

- New historical and live projections include versioned `url_reference`
  nodes keyed by a project HMAC of the normalized URL. `uses_request_url`,
  `referrer_url_matches`, and `redirect_url_matches` assertions cite only the
  originating capture's receipt. Shared nodes connect captures from separate
  pages and preserve every duplicate match without selecting an initiator.
  Raw URL values are absent from these records.
- Relative navigation headers resolve against the request's full URL,
  including its directory and query. An unresolved response-status mismatch
  cannot generate a redirect match. Writer validation checks URL-reference
  identities, project boundaries, version, and exact receipt locators.
- Focused graph navigation exposes URL matches and their request/Referer/
  redirect roles. The inspector explains duplicate matches and unknown order
  and causality; URL signatures are omitted from UI labels and properties.
  URL nodes are hidden in the overview alongside other capture-level details.
- Synthetic acceptance imports a redirect source and matching captures in
  separate historical pages, then projects the real local graph and queries
  the shared application graph API. Both duplicate targets are returned; a
  different query value is excluded. Project separation, replay identity,
  raw-value absence, relative resolution, and native Neo4j row preparation
  are also checked. Go graph/ingest/graphaccess/search/webui tests and 78/78
  Node tests passed. Native row preparation used unit tests only.
- Existing accepted projections are immutable and have not been rewritten;
  adding these references to older captures still requires an explicit
  versioned backfill. Reimporting an unchanged historical capture preserves
  its registered canonical payload even when the current parser can emit
  additional records. Synthetic pending and applied-job scenarios verify that
  replay keeps the original payload hash, delivery state, and single capture
  revision. Live-first revisions retain their existing source/mapping checks.
  Runtime Neo4j, Caido, and model verification remain
  outside this synthetic checkpoint. Stage 3 remains `PARTIAL`.

## Stage 3 exact HTTP navigation links during page import — PARTIAL (2026-10-25)

- The Go historical and live import paths now add request-to-request
  `referrer_context_for` links when a request's unique, valid `Referer` URL
  exactly matches one request in the same imported page. They add
  `redirects_to` links when a unique 3xx response `Location` exactly matches
  another request in that page. Ambiguous duplicate matches, duplicate
  headers, malformed URLs, and credential-bearing URLs are skipped without
  failing the surrounding capture import.
- Live-tail import also stores project-keyed HMAC identities for request,
  Referer, and redirect URLs in its existing index and source receipt. It can
  connect an exact unique Referer/redirect match across already imported live
  pages. The graph job carries the matched observation and both capture
  receipts so asynchronous projection remains correct if jobs run out of
  order. Raw matched URL values are not persisted. Historical imports across
  page boundaries and retroactive re-evaluation when a later duplicate URL is
  discovered remain incomplete.
- Assertions retain both exchange receipt references and record the exact
  match basis as `referrer_exact_url` or `redirect_exact_url`, with
  `causality=navigation_evidence_only`. The predecessor observation is
  included in the same projection bundle, so an out-of-order worker does not
  lose the endpoint before applying the assertion. URL values are used only in
  memory for matching and are not added to graph context. No `precedes`
  assertion is inferred.
- Each navigation assertion cites only its two endpoints' receipts, while
  the enclosing bundle retains the union needed by all its records. Replaying
  links in a different order now preserves their IDs and does not create
  duplicate assertions. A synthetic integration scenario imports three
  related captures, reopens the store, projects the dependent job first,
  applies the remaining jobs, and replays the page in reverse order. The real
  local graph writer and application graph query resolve both links and all
  three exact capture revisions. Neo4j readiness is a test seam in this
  scenario; it does not verify a Neo4j runtime.
- Graph labels now render these predicates as `referer matches` and
  `redirects to`. Synthetic tests passed with
  `TMPDIR=/var/tmp go test -count=1 ./internal/graph ./internal/ingest` and
  77/77 Node UI tests; `git diff --check` passed. No Caido, runtime project
  database, Neo4j service, or model was accessed.
- Direct observation matching across historical page boundaries and retroactive re-evaluation
  when later duplicate URLs are discovered remain incomplete; runtime Neo4j
  projection remains unverified.
  Stage 3 remains `PARTIAL`.

## Stage 4 analyst-defined flow order — PARTIAL (2026-10-25)

- Before accepting the consultation review, an analyst can select evidence
  references for a `flow` context label and move the selected references up or
  down. The exact selected order is saved in `evidence_reference_ids` and is
  restored as numbered flow steps when the review is reopened.
- Review, graph-inspector, and exact-comparison displays label this order
  `analyst-defined order; not observed causality`. It does not create a graph
  `precedes` assertion or imply that one exchange initiated another.
- `node --test internal/webui/graph_focus_test.cjs
  internal/webui/surface_requests_test.cjs` passed 76/76 synthetic UI tests;
  `git diff --check` passed. No Go build/tests, Neo4j, containers, runtime
  database, Caido, or live browser verification was run. Runtime coverage
  remains `PARTIAL`; no live data or graph was changed.

## Stage 3 request ordering in observation inspector — PARTIAL (2026-10-25)

- The observation inspector shows direction only for a current-slice
  `precedes` assertion whose exact subject and object both resolve to
  observation entities and which carries complete source references with an
  observed or corroborated evidence state. It displays the subject/object IDs,
  assertion ID, origin/evidence basis, and source SHA-256 plus record locator.
- Missing edges, omitted endpoints, missing/invalid source references, and
  proposed or contested assertions leave the initiating request unknown in the
  current slice. Timestamps and graph proximity are never used to infer order
  or causality. No graph schema or working data changed.
- Pure Node UI tests cover incoming and outgoing edges, no edge/incomplete
  slices, and missing provenance. Runtime and live graph coverage remain
  `PARTIAL`.

## Stage 4 accepted context in exact capture comparison — PARTIAL (2026-10-25)

- Exact capture comparison now shows accepted analyst context in separate
  Baseline (A) and Selected (B) sections. It resolves each side through the
  graph's exact observation provenance and the shared context matcher, which
  requires the selected project's loaded accepted consultation, exact request
  and revision IDs, and the annotation's selected evidence-reference ID.
- The comparison view shows analyst-provided labels, consultation and
  annotation versions, and evidence-reference provenance. It adds no excerpts
  or raw HTTP values to the context display and sends no annotation fields to
  the comparison endpoint or Hermes packet. Mismatched revisions and
  unaccepted reviews show no labels.
- `node --test internal/webui/graph_focus_test.cjs
  internal/webui/surface_requests_test.cjs` passed 69/69 synthetic UI tests;
  `git diff --check` passed. No Go build/tests, Neo4j, containers, runtime,
  working databases, Caido, or live browser were used. Runtime coverage remains
  `PARTIAL`.

## Stage 3 accepted analyst context in graph inspector — PARTIAL (2026-10-25)

- The observation inspector can display context labels from the currently
  selected consultation only after its review is accepted and loaded for the
  exact selected project. Each displayed label must reference an evidence ID
  whose Caido request ID and revision ID match that graph observation's capture
  provenance. The UI shows the consultation and annotation versions, evidence
  reference ID, side, request ID, and revision ID.
- The overlay exposes no evidence excerpts or raw HTTP values. It identifies
  labels as analyst-provided context and states that they do not establish
  actor identity, ownership, causality, or effect. Unaccepted reviews, other
  projects, other observations, and mismatched revisions produce no overlay.
- `node --test internal/webui/graph_focus_test.cjs
  internal/webui/surface_requests_test.cjs` passed 68/68 synthetic UI tests.
  `git diff --check` passed. No Go build, Neo4j, containers, runtime database,
  Caido, or live browser verification was run; no graph data was reprojected.
  Overall Stage 3 and runtime coverage remain `PARTIAL`.

## Stage 3 revision-bound query and body occurrences — PARTIAL (2026-10-25)

- The Go historical and live projections now emit capture-specific query
  `field_occurrence` records and request/response JSON scalar,
  URL-encoded-form, and simple multipart text-part body occurrences. Response
  HTML controls (`input`, `select`, `textarea`, and `button`) also receive
  occurrences whose source span covers the exact raw start-tag token. HTML
  control occurrences have no value span and do not index attribute values.
  All source spans include message-relative byte ranges, wire fidelity, source
  receipt, and capture revision identity. Body spans are emitted only for
  complete, byte-preserving messages with explicit non-truncated metadata and
  a consistent message extent. Project-keyed `value_reference` links are
  created only for non-sensitive strings; query, form, JSON, and supported
  multipart text strings share the `field-equality-v2` decoded UTF-8
  normalization. Neither the key nor plaintext values enter graph records.
  Legacy v1 references remain readable; new projections use v2.
- Multipart source spans currently cover only unambiguous top-level
  `form-data` text parts with no filename or transfer encoding. The raw-source
  scanner requires strict CRLF boundaries and exact alignment with parsed
  fields. File, nested, encoded, ambiguous, or unbound parts remain structural
  and add an explicit occurrence-coverage gap. Sensitive field names receive
  opaque aliases and never produce value-equality tokens.
- HTML response controls remain structural-only beyond their raw start-tag
  spans: values inside attributes, textarea bodies and selected options are not
  extracted. Form actions and links remain structural entries without control
  occurrences. Request-side HTML controls and incomplete/unsupported response
  bodies leave an explicit occurrence-coverage gap.
- Equality relations are labeled `keyed_value_equality` / `correlation_only`.
  They establish a same-project keyed value match and do not establish
  causality, object identity, or ownership. If a source span or revision cannot
  be bound, the occurrence and equality link are omitted and the analysis card
  records the gap.
- Synthetic Go graph-import tests cover repeated query/form/multipart order,
  multipart request/response source spans, safe equality correlation, file and
  transfer-encoded/nested exclusions, HTML response control token spans,
  repeated control order, sensitive-name masking, raw-value absence, and
  writer validation. They also cover URL-encoded source spans, request/response
  JSON scalar spans (including escaped values), same-project correlation across
  query/form/JSON/multipart locations, separation by project key, sensitive-name
  omission, and omission of spans for truncated, malformed, or extent-mismatched
  bodies.
  Historical and live builders use the same projection path.
  Synthetic traversal tests cover same-project correlation across captures.
  `GraphNeighborhoodPage`
  now keeps the `has_occurrence`, `occurrence_of`, and `value_used_in` path and
  can expand a bounded observation → field → project-value match → second
  capture slice. The Neo4j writer promotes only the keyed signature into an
  indexed property; its lookup is project-scoped, generation-checked, and
  returns IDs only. Targeted Go packages and 37 synthetic Web UI tests passed.
- The UI labels field/value nodes, shows source path, occurrence number and
  byte spans, and explains that equality is correlation only. It does not
  render raw parameter values or equality signatures. These nodes stay hidden in
  the overview until their filter is enabled; focused observation traversal
  opens the bounded related path.
- Stage 3 remains `PARTIAL`: the opt-in ephemeral Neo4j Cypher integration was
  added but not run, and existing graph snapshots need a full reprojection
  before v2 field-value references and the field-value index marker become
  current. Multipart file, transfer-encoded, nested, ambiguous, or unbound parts
  remain structural-only with explicit coverage gaps. Complete capture-shape
  coverage, actor/object annotations, and complete application-map coverage
  remain unfinished. Caido and active working databases were not checked for
  this code slice.

## Stage 5 local UI resume after reload — PARTIAL

- The consultation panel can reopen a saved session from its ID after the
  evidence selection that created it is gone. A persistent top-bar action
  opens the operator review panel, scopes the lookup to the currently selected
  project, and restores the saved version, redacted packet, context labels and
  review/acceptance state. The ID remains available after the dialog closes;
  changing projects clears it.
- A late resume response cannot replace state after the selected project has
  changed. Open sessions remain unaccepted and keep the explicit Hermes review
  gate; accepted sessions reopen as accepted. Session contents continue to be
  loaded through the existing local operator-review endpoint.
- Synthetic verification: `node --check internal/webui/assets/app.js`, all 37
  `surface_requests_test.cjs` cases, and `TMPDIR=/var/tmp go test -count=1
  ./internal/webui` passed. The JavaScript transport is mocked; no running
  application, project data, Caido, graph database, KB or model was accessed.
- Stage 5 remains `PARTIAL`: Hermes/provider selection, actual browser-level
  verification and model/data-boundary review are not part of this slice.

## Stage 5 graph comparison handoff to consultation — PARTIAL (2026-10-25)

- The graph comparison dialog now keeps consultation resume and operator-review
  controls available. An accepted session can receive a deterministic
  comparison only when its reviewed evidence references contain both exact,
  hash-verified capture revisions in the same project. The UI sends the pinned
  IDs, session version, and idempotency key to the existing consultation
  compare route; version conflicts require resuming the session again.
- The saved consultation exposes the comparison record ID and bounded
  comparison metadata to Hermes. The consultation service removes displayed
  raw byte excerpts before persistence; it does not infer causality or impact.
- Synthetic verification passed: the UI test resumed an accepted session and
  attached an exact graph comparison; a loopback HTTP integration test used
  two encrypted temporary captures, persisted the comparison, resumed the
  accepted session, and confirmed excerpts were absent. `node --check`, all
  38 `surface_requests_test.cjs` cases, and
  `TMPDIR=/var/tmp go test -count=1 ./internal/webui -run
  TestConsultationOperatorReviewUsesRealHTTPAndEncryptedEvidence` passed.
- Stage 5 remains `PARTIAL`: the user must first create and accept a session
  containing both captures, then resume it in the comparison dialog. Hermes
  provider selection, an actual model-backed run, and real-browser verification
  remain unconfirmed.

## Stage 5 consultation candidate workflow and recovery — PARTIAL (2026-10-25)

- The accepted consultation review now supports a manually written candidate
  proposal with bounded fields. The UI offers only hash-verified evidence
  references and comparisons already stored in that same session. It calls
  proposal, citation validation, and candidate-save routes in sequence with
  the latest session version and separate idempotency keys. The resulting
  record is labeled `candidate`; the UI does not present it as a finding or
  verified impact.
- On version conflict the UI disables the stale workflow and requires a fresh
  session resume. A resumed session lists saved proposals, matching valid
  receipts, and candidates. It can continue by validating a saved proposal or
  by saving against its matching valid receipt, so an interrupted HTTP step
  does not require creating a duplicate proposal. Proposal fields contain only
  operator-entered text and reference IDs; displayed HTTP excerpts are not
  copied into the proposal.
- Synthetic verification passed: `node --check`, all 42
  `surface_requests_test.cjs` cases, and the loopback
  `TestConsultationOperatorReviewUsesRealHTTPAndEncryptedEvidence` test using
  temporary encrypted captures and a temporary consultation store. The test
  covers proposal/receipt/candidate/resume, recovery after proposal and receipt
  persistence, rejection of an out-of-session citation at validation, and
  stale-version conflicts. No running application, Caido, Neo4j, KB, Hermes
  process, or model provider was used.
- Stage 5 remains `PARTIAL`: this is an operator-authored local candidate
  workflow. Hermes/provider selection, model-backed analysis, and actual
  browser verification remain unconfirmed.

## Stage 4 synthetic HTTP evidence acceptance (2026-10-24) — PARTIAL

- A new end-to-end synthetic fixture writes two encrypted HTTP exchanges into
  a disposable project vault, then reads an exact request revision and
  compares both request and response revisions through the real appquery
  service and local HTTP handlers. It verifies deterministic difference
  excerpts, exact revision IDs, truncation flags, and `no-store` responses.
- `go test ./internal/webui -run
  TestEvidenceAndComparisonHTTPUseExactEncryptedCaptureRevisions -count=1`
  passed. All data and vault keys are generated in the test; no existing
  project, Caido, Neo4j, KB, or model provider is used.
- This proves the application path against a synthetic encrypted vault only.
  Decryption of the pre-existing vault and full Stage 4 source coverage remain
  unverified; Stage 4 remains `PARTIAL`.

## Stage 5 encrypted-evidence consultation cycle (2026-10-24) — PARTIAL

- A second synthetic project now exercises the actual appquery evidence packet
  builder and exact-revision comparator inside the MCP consultation lifecycle.
  Two encrypted captures flow through operator review, MCP resume and compare,
  a test-only model adapter, proposal citation validation, candidate save, and
  a fresh service/MCP server restart. The test verifies that unreviewed MCP
  output withholds question/excerpts, the synthetic bearer value does not cross
  the accepted model boundary, exact source citations validate, and the saved
  candidate resumes at the expected version while remaining `candidate`.
- The loopback web handler also now has an end-to-end synthetic approval test:
  create returns a redacted/withheld session, the review route reveals only the
  redacted exact packet, a same-origin context-annotation POST binds a
  pseudonymous actor label to its exact evidence reference with
  `origin=analyst`, the acceptance POST records the explicit operator
  transition, and the session resume route returns the approved redacted
  evidence and context. This goes through the actual consultation service and
  encrypted temporary vault.
- `go test ./internal/mcpserver -run
  TestMCPConsultationUsesEncryptedEvidenceAndResumesSavedCandidate -count=1`
  passed using only `t.TempDir()` project and vault files. No external model or
  existing project data is used.
- `go test ./internal/webui -run
  TestConsultationOperatorReviewUsesRealHTTPAndEncryptedEvidence -count=1`
  also passed with a temporary project and a generated test key.
- Hermes/provider selection and real model-backed evaluation remain
  intentionally unresolved; the complete synthetic consultation contract is
  covered, while Stage 5 stays `PARTIAL` pending provider/data-boundary review.

## Stage 2 full-view provenance cursor round trip (2026-10-24) — PARTIAL

- A synthetic end-to-end regression test now traverses the shared
  `appquery.Service`, HTTP graph endpoint, and MCP `bb_graph_slice` tool. It
  starts with 64 source references, follows the generation-pinned cursor to
  retrieve the remaining two, verifies the cumulative result has all 66, and
  confirms replay returns the same accepted page.
- The test exposed that analytical graph responses add `integrity` and
  `freshness` to source references, while the BehaviorGraph snapshot reader
  accepts only the canonical `path`/`sha256`/`record_locator` shape. Continuation
  now identifies sources by that canonical triple, rejects inconsistent
  integrity states across pages, and strips response-only status fields before
  reopening the merged snapshot.
- The acceptance uses a synthetic in-memory graph store and a temporary local
  project. It does not contact Neo4j, Caido, the KB, a model provider, or a
  target. Full Neo4j query compatibility and runtime coverage remain
  unverified; Stage 2 remains `PARTIAL`.

## Stage 2 assertion/frontier candidate continuation (2026-10-24) — PARTIAL

- The Neo4j analytical slice no longer applies a separate 100-assertion cap
  to each frontier node. It shares the remaining global 1,000-assertion budget
  across the current frontier, so a focused high-degree seed can use the
  available budget while broader frontiers receive a bounded proportional
  share. The global cap and explicit truncation status remain in place.
- Omitted frontier IDs are now added to the existing generation-pinned graph
  continuation queue. The server merges each selected branch into one
  cumulative graph view; the queue accepts validated entity and assertion IDs,
  preserves its unprocessed tail, and marks capped branch lists incomplete.
  The UI displays the returned cumulative page instead of merging partial
  graphs client-side.
- The per-query 1,000-assertion cap now returns bounded omitted assertion IDs
  to that same queue. Continuation passes already-materialized assertion IDs
  back as exclusions so the next native read advances instead of selecting
  the same page. Exclusions are capped at 10,000 IDs; exceeding that bound is
  reported and keeps the result `PARTIAL`.
- Synthetic unit tests cover budget sharing, omitted frontier seeds, cursor
  persistence validation, branch deduplication, and UI page replacement.
  `TMPDIR=/var/tmp go test -count=1 ./internal/neo4jstore ./internal/search
  ./internal/appquery`, `node --test internal/webui/surface_requests_test.cjs`,
  `go vet` for those Go packages, and `git diff --check` passed. Three opt-in
  Neo4j integration scenarios, including global-cap continuation, passed
  against ephemeral `neo4j:2026.10.0` containers using synthetic fixtures;
  the containers were removed afterward. Working project data and its query
  plan were not touched.
  The 1,000-assertion cap still applies to each native query page. Card and
  provenance caps still lack continuation pages, and all continuation queues
  are bounded; these conditions can leave an explicit `PARTIAL` result.
  Stage 2 remains `PARTIAL`.

## Stage 2 analysis-card candidate continuation (2026-10-24) — PARTIAL

- Neo4j analytical reads now return bounded owner branches when the 200-card
  candidate page is truncated. Generation-pinned full-view continuation passes
  the already materialized card IDs back as exclusions, then merges the next
  card page into the cumulative graph. The page limit flag now clears when the
  latest page resolves it instead of remaining sticky from the first page.
- Card exclusions are capped at 10,000 IDs and omitted owner branches at 5,000;
  queue/attempt limits still leave explicit truncation when exceeded. Provenance
  sampling still has no continuation and keeps Stage 2 `PARTIAL`.
- Targeted synthetic unit tests for exclusion validation, bounded owner seeds,
  continuation branch decoding, and truncation state passed. An opt-in Neo4j
  fixture for advancing from a full 200-card page to the final two cards was
  added, but was not run in this check. No working graph database or request
  bodies were read for the implementation check.

## Fresh local runtime acceptance (2026-10-24) — PARTIAL

- Reconciled the selected Juice Shop Caido history against the local index for
  `192.168.1.149:3000`: 355 IDs matched, with zero Caido-only, local-only,
  duplicate, or unclassified IDs; the terminal cursor was reached in four
  pages. Caido was restored to `sperpravo` and the selection was verified.
  Since the source has no stable snapshot token, history coverage remains
  `PARTIAL`. A fresh read-only repeat confirmed 355 historical IDs, zero live
  index rows, four Caido pages, and the same exact-origin match; the other rows
  in the 1,400-record project index were outside that origin and excluded from
  this comparison. No request/response bodies were read and no target requests
  were sent.
- The running web UI returned HTTP 200 for projects, surface, and direct graph
  reads on the registered Juice Shop project. The surface read covered 1,400
  observations and 131 operations. A one-seed direct view returned three
  entities and two edges at graph generation 1375, with `truncated=false` and
  `coverage_state=partial`. Across 10 repeated requests, latency was 114 ms
  minimum, 166 ms median, 261 ms p95, and 442 ms maximum. This was a small
  mixed warm-cache smoke sample on Linux x86_64, 8 vCPU, and 15.3 GiB RAM; it
  is not a broad performance benchmark or complete graph validation.
- `./scripts/run-hermes mcp test bb_assistant` connected over stdio and
  discovered all 15 managed tools. No model request was made; the real
  model-backed analysis loop remains `PARTIAL` until provider selection and
  data-boundary review.
- A fresh read-only KB `stats` attempt did not return within 95 seconds while
  checking current source hashes and was interrupted. It made no index or
  configuration changes; current KB coverage was not revalidated.

## Projection worker startup and local import follow-up (2026-10-24) — PARTIAL

- A bounded run against the registered project spent 491 seconds opening the
  existing BehaviorGraph because `OpenWriter` used the full snapshot reader
  just to check the schema. After SIGINT, one leased job returned to pending
  as `worker_interrupted`; zero jobs were applied.
- `OpenWriter` now checks only the project namespace and required schema tables
  through a read-only SQLite connection. A regression fixture proves unrelated
  malformed records are not loaded by writer startup while unsupported schema
  versions still fail closed. `internal/graph` and `internal/ingest` tests,
  `go vet ./...`, and the full `go test -count=1 ./...` passed.
- A live one-job follow-up reduced graph-writer startup from 491 seconds to
  2 ms. The job then failed during local graph import in 33 ms, before Neo4j
  sync, with the existing safe `projection_unavailable` code. Zero jobs were
  applied and the queue remains `PARTIAL`; the root cause within that import
  still needs investigation. No more than these two single-job attempts ran.

## Stage 5 injectable synthetic analyst adapter (2026-10-24) — PARTIAL

- The MCP consultation lifecycle fixture now injects an explicit model-boundary
  adapter. It receives only an operator-accepted resume result and the
  sanitized deterministic comparison, derives an unverified proposal with
  exact evidence/comparison references, and submits it through MCP citation
  validation, candidate save, and restart/resume.
- The adapter rejects unreviewed sessions, unavailable comparison sides, and
  raw excerpt markers. This validates the harness contract on synthetic data;
  it is test-only and does not configure Hermes or call a model provider. Stage
  5 remains `PARTIAL` pending a selected provider, data-boundary review, and
  real model-backed evaluation.

## Exact evidence range provenance checks (2026-10-24) — PARTIAL

- The local evidence viewer now requires an exact 64-character revision ID and
  rejects API ranges unless project, request, revision, side, offset, byte
  counts, and continuation metadata match the pinned selection. Mismatched
  responses fail closed and are not rendered. This keeps evidence snippets
  and later consultation references bound to the selected capture revision.
- Synthetic UI tests first reproduced wrong-project/revision/side/range display
  and unversioned fallback, then passed after the fix. No real capture was
  opened and no runtime data was changed; Stage 4 remains `PARTIAL` pending
  working evidence verification.

## Hermes managed tool allowlist reconciliation (2026-10-24) — PARTIAL

- `setup-hermes` now removes obsolete MCP tool names from its managed Hermes
  allowlist as well as adding newly approved tools. It preserves settings
  outside the managed allowlist and still refuses malformed or duplicate
  entries. This prevents a prior profile from retaining stale tool grants.
- A regression test first reproduced the stale-tool behavior, then passed with
  the fix. Six Hermes profile tests pass, including a synthetic check that all
  15 managed names exist in the Go MCP tool catalog and the raw-capture tool is
  excluded. The installed Hermes MCP discovery check and model-backed workflow
  were not run; Stage 5 remains `PARTIAL` and the model provider remains an
  operator decision.

## Evidence vault readiness indicator (2026-10-24) — PARTIAL

- The local web UI now shows whether the evidence vault key is missing/invalid,
  syntactically configured but unverified, or unavailable to inspect. The
  read-only capability endpoint returns only this state and disables caching;
  it never returns key material or reads stored vault objects. Runtime audit
  output reports the same state and explicitly says that key syntax does not
  verify decryption.
- Synthetic checks passed: Go tests for `internal/ingest`,
  `internal/runtimeaudit`, and `internal/webui`; all 49 web UI tests; JavaScript
  syntax checks; `go vet` for those Go packages; and `git diff --check`.
- The status indicator does not establish that existing evidence can be
  decrypted. The original vault key and stored-object verification remain
  unresolved; Stage 4 and overall project readiness remain `PARTIAL`.

## Stage 2 bounded analysis cards in the direct view (2026-10-24) — PARTIAL

- The Neo4j one-hop query now loads up to 24 matching analysis cards in the
  same generation-pinned read. It closes referenced endpoint and provenance
  dependencies, reports when the candidate cap was reached, and the inspector
  shows parse state, known-effect metadata, hypothesis count, and bounded
  missing-evidence notes for the selected capture/endpoint.
- Added an opt-in Neo4j integration assertion for card selection and a UI
  identity-resolution test. The assertion passed against an ephemeral
  `neo4j:2026.10.0` container on a random loopback port with synthetic fixtures;
  the temporary container was removed afterward. This verifies the Neo4j query
  behavior for the fixture, but existing project data and production query
  behavior remain unverified. Stage 2 remains `PARTIAL`.

## Stage 6 synthetic live restart/replay and Go service templates (2026-10-24) — PARTIAL

- Extended live-tail coverage to commit one metadata page, close the store and
  writer, reopen them from the project path, resume the saved cursor, then
  replay both known request IDs and verify that no duplicate captures are
  added. The test takes the same exclusive project lease used by the CLI and
  confirms raw fixture markers are not written in plaintext.
- Added Linux/systemd templates for the loopback web UI, per-project live
  intake, and per-project projection worker, plus a setup/runbook path. Static
  `systemd-analyze verify` passed on copies with the install-time executable
  path replaced by `/usr/bin/true`. The real install path is not present in
  this checkout; no systemd service was installed or started.
- `TMPDIR=/var/tmp go test -count=1 -run
  TestRunLiveTailResumesFromCommittedCursorAfterStoreRestart ./internal/ingest`
  passed with synthetic data. No Caido instance, existing project,
  production queue, or Neo4j was accessed; test state stayed in a temporary
  synthetic project. Managed runtime and live source parity remain `PARTIAL`.
- The read-only `project-audit` queue summary now reports due versus delayed
  pending jobs, the age of the oldest pending job, and expired leases alongside
  existing applied/dead-letter counts. Malformed queue timestamps leave queue
  diagnostics unavailable; payloads and raw error codes are not returned.
  Synthetic audit tests cover due/retry/expired/age calculations and secret
  omission.
- Built the Go executable and launched the web UI against an empty temporary
  data root on an ephemeral loopback port. `GET /` and `GET /api/projects`
  both returned HTTP 200; the API correctly reported zero registered
  projects. The temporary server was stopped after the check. This verifies
  binary startup and the empty-state route only, not the installed service or
  a real project.

## Stage 6 bounded project backup and restore (2026-10-24) — SYNTHETICALLY VERIFIED

- Added offline `project-backup` and `project-restore` CLI commands. They use
  mandatory byte/file budgets, per-file SHA-256 checks, private `0600` archive
  files, `0700` restore directories, no-overwrite publication, and rebuild the
  local project registry only after full archive validation. Restored output
  includes a fresh read-only capability summary; a partial summary stays
  `PARTIAL`.
- The versioned tar manifest preserves the project identity and all regular
  project files, including encrypted vault payloads and evidence/receipt data.
  Symlinks, special files, unsafe paths, duplicate members, tampering, and
  budget overruns are rejected. The global registry and `.maintenance.lock`
  are reconstructed or omitted rather than copied.
- Synthetic tests verify round-trip identity and file preservation,
  read-only capture-store open, tamper/traversal/duplicate rejection,
  symlink rejection, explicit budgets, and refusal to overwrite existing
  archives or projects. The CLI round-trip also confirms that restore emits a
  fresh capability summary without exposing vault contents. No working
  project was backed up or restored. Run `project-audit` on an
  operator-restored project before resuming service.
- Stage 6 remains `PARTIAL`: managed startup/restart replay, live intake,
  working-project restore, and real runtime parity are still unverified.

See [Project backup and restore](PROJECT-BACKUP-RESTORE.md) for operator steps.

## Approved Caido ID recheck and synthetic MCP consultation cycle (2026-10-24) — PARTIAL

- With the user's approval, Caido was temporarily switched from `sperpravo` to
  the registered Juice Shop project. An unfiltered metadata pass reached its
  10,000-ID/100-page bound with more records available. A host-filtered pass
  for `192.168.1.149` reached its terminal cursor in four pages: 355 Caido IDs
  matched 355 local historical/live IDs, with zero Caido-only, local-only,
  duplicate, or unclassified IDs. Request and response bodies were not read,
  and no target traffic was sent. Caido was restored to `sperpravo` and the
  selection result confirmed. The comparison remains `PARTIAL`: the source has
  no stable snapshot token, and the 10,000-ID unfiltered pass was bounded.
- Added a synthetic MCP JSON-RPC integration test for the consultation cycle:
  create a session, keep content hidden until operator acceptance, resume,
  compare exact revisions, record a proposal, validate citations, save a
  `candidate`, then recreate the service and resume. It checks excerpt
  redaction and persistence across the real MCP dispatch path. No Hermes model
  call or provider was used; real model-backed evaluation remains pending.
- This check did not read the KB, alter a working graph/queue/database, or
  perform active target operations. Overall coverage remains `PARTIAL`.

## Stage 3/4 annotated evidence preview (2026-10-24) — PARTIAL

- Expanding an actor/session/object/flow label now shows the exact byte span
  and the redacted excerpt from the saved evidence packet for each linked
  reference. The excerpt is displayed only when request ID, revision, side,
  and span all match; unavailable or mismatched packet evidence stays hidden.
  Rendering is bounded to 4 KiB of UTF-8 text and uses text nodes.
- Synthetic UI tests cover source binding, unavailable and mismatched evidence,
  duplicate references, redaction-only display, and the UTF-8 cap. This slice
  does not inspect the working project, open raw message bodies, change graph
  data, or verify a Hermes run. Stage 3/4 runtime and overall coverage remain
  `PARTIAL`.

## Stage 3 context annotation traceability (2026-10-24) — PARTIAL

- Saved actor/session/object/flow labels in the consultation review now expand
  to show the exact evidence number, HTTP side, and Caido request ID attached
  to each label. Unresolvable references are called out; opaque reference IDs
  and raw HTTP bytes are not rendered.
- Synthetic UI tests cover reference ordering, unknown IDs, duplicate reference
  IDs, and ensure the display helper returns only safe metadata. Project data,
  graph, Caido history, and queues were not changed by this code slice.
- Coverage remains `PARTIAL`: the UI makes analyst-provided context auditable,
  but it does not establish actor identity, ownership, or causality.

## Stage 3 historical operation backfill preview (2026-10-24) — PARTIAL

- Added `graph-operation-backfill-preview`, a read-only CLI preview for legacy
  projects. It inspects current historical revisions under the project's
  exclusive maintenance lease, decrypts request messages locally with a 1 MiB
  per-message cap and a default 512 MiB aggregate cap, then builds candidate
  operation nodes and `belongs_to_operation` links in memory. It does not call
  Caido, read responses, write receipts, update SQLite/Neo4j, or drain either
  projection queue. The output contains coverage counts, a candidate digest,
  and at most ten method/route-family samples; candidate bundles and raw
  requests are omitted.
- Limits are explicit: 5,000 captures/512 MiB by default, at most
  20,000 captures/1 GiB. Unmapped captures, oversized requests, unsupported
  route targets, endpoint-ID mismatches, and capped scans make the preview
  `PARTIAL`. A synthetic historical-projection fixture, origin/target guards,
  bounded bundle batching/provenance tests, and JSON redaction test pass with
  `TMPDIR=/var/tmp go test -count=1 -p 1 ./internal/ingest ./cmd/bb-assistant`.
- Added `graph-operation-backfill-apply`. It recomputes the bounded preview
  under the exclusive project lease and requires the exact candidate digest
  from the preview command. Only a complete ready candidate is accepted; its
  bundles are imported idempotently to the local BehaviorGraph and durable
  outbox. The command does not drain Neo4j. Synthetic end-to-end coverage checks
  exact-digest rejection, local operation/link materialization, queued outbox
  state, and idempotent replay. A fixture also found and fixed preview
  compatibility with current flat endpoint cards and legacy nested cards.
- Neither preview nor apply has been run against the saved Juice Shop graph.
  Applying a candidate to working data still requires an explicit operator
  invocation with its exact preview digest. No live graph or queue was changed
  for this code slice.

## Stage 3 project-local operation aliases and live wiring (2026-10-24 12:41Z) — PARTIAL

- The graph inspector can now assign an analyst-readable alias to an operation.
  It is stored per project in consultation SQLite schema v4 as append-only,
  versioned metadata with idempotent writes; stale edits return a conflict.
  The API confirms the selected ID is an operation node in that project's
  graph before saving. Display aliases do not rewrite route families, graph
  entity IDs, or historical evidence.
- Existing consultation stores migrate transactionally from v1, v2, or v3.
  Tests cover migration, project isolation, validation, replay, version
  conflicts, API graph-node validation, and the browser save/display flow.
  `TMPDIR=/var/tmp go test -p 1 ./...`, `TMPDIR=/var/tmp go vet ./...`,
  JavaScript syntax validation, and all 40
  `node --test internal/webui/*.cjs` checks passed. The live alias-list API
  returns HTTP 200; the editor cannot be exercised against this project's
  active graph until operation nodes exist.
- The loopback web-ui was restarted. `/api/projects`, `/api/surface`,
  `/api/consultation/operation-aliases`, and sampled `/api/graph` reads return
  HTTP 200. The API adapter is wired through the live `Tools` implementation.
  The current BehaviorGraph still contains 1,317 endpoints and 1,369
  observations but zero operation nodes, so the alias editor is not yet
  reachable from this project's graph. There is no durable production
  graph-rebuild/cutover path; the previously tested larger projection remains
  a shadow result and the active graph was not replaced.
- A bounded 10-job delivery batch applied 5 jobs and retried 5 as
  `projection_unavailable`; none were dead-lettered. The queue now has 55
  pending, 0 leased, 1,345 applied, 0 dead-letter. No further worker is
  running. The stale SQLite sidecar was checkpointed under the exclusive
  project lock and the database integrity check passed, restoring read access.
- Overall coverage remains `PARTIAL`: 31 captures remain unmapped, the Caido
  inventory has no stable snapshot token, and browser/JavaScript mapping and
  Hermes model-backed analysis are incomplete. No alias was written to the
  real project during runtime checks.

## Stage 3 analyst context annotations (2026-10-24 11:04Z) — PARTIAL

- Consultation sessions now accept versioned, append-only analyst annotations
  (`actor`, `session`, `tenant`, `role`, `object`, `flow`, `scenario`, and
  `scenario_step`) attached to exact selected evidence-reference IDs. Labels
  are constrained to a short type-prefixed alias such as `actor-a` or
  `object-1`; the UI says these labels are analyst context, not proof of
  ownership, equality, or causality. Accepted sessions are immutable. Hermes
  receives the annotations only after the operator accepts the updated
  session; pre-accept resume responses omit them.
- Consultation SQLite schema v3 adds an append-only annotation table and
  migrates v1/v2 session stores transactionally. Tests cover selected-reference
  binding, alias validation, idempotent retry, version changes, accepted-state
  rejection, pre/post-review visibility, and persistence after service restart.
  ECC review found and the UI now fixes stale-review recovery after a version
  conflict.
- `TMPDIR=/var/tmp go test -count=1 ./...`, `TMPDIR=/var/tmp go vet ./...`,
  `node --check internal/webui/assets/app.js`, 37 graph/surface UI tests, and
  `git diff --check` passed. This code slice did not access Caido, inspect raw
  traffic, migrate the live consultation store, or reproject graph data.
- The local application and overall data coverage remain `PARTIAL`: live
  consultation startup with this schema, complete Caido synchronization,
  graph reprojection, and Hermes model-backed analysis still need runtime
  verification.

## Stage 3 route-family uncertainty in graph inspector (2026-10-24 10:47Z) — PARTIAL

- Operation and endpoint nodes now identify route grouping as heuristic. The
  inspector counts `{value}` and legacy `{parameter}` path segments and explains
  that each could be a variable value or an unrecognized static route word; it
  does not claim these are confirmed parameters. Routes without masks still
  disclose the static-word vocabulary limitation.
- Added focused UI helper coverage. `node --test
  internal/webui/graph_focus_test.cjs internal/webui/surface_requests_test.cjs`
  passed 36/36 tests; `git diff --check` passed. Existing operation graph data
  was not reprojected, so historical graph data still has no new operation
  nodes. The local `web-ui` was rebuilt and restarted; `/assets/app.js` serves
  the updated inspector. Route grouping remains heuristic; route aliases and
  analyst annotations remain outstanding.

## Stage 3 multipart occurrences and current health check (2026-10-24 10:44Z) — PARTIAL

- Go multipart mapping now retains repeated same-name parts as indexed field
  occurrences (`[0]`, `[1]`) and keeps singleton paths on their existing `[]`
  identity. Sensitive multipart field names use source-local aliases; values
  remain absent from graph bundles. A fixture exposed the prior collapse to
  `[]`; the regression test now covers occurrence identity and value/name
  redaction.
- The local Go capture store and BehaviorGraph schemas are compatible (v7 and
  v2). The project has 1,400 captures, 1,369 mapped observations, and 31
  unmapped captures; history and coverage remain `PARTIAL`. The loopback UI
  serves `/api/projects` and `/api/surface`; the surface reports 131 operations
  and partial coverage. A read-only direct graph request through the live UI
  returned 3 entities and 2 edges at generation 982. Its integrity scope is
  selected records only, with analysis cards and full-project integrity not
  assessed. The running projection predates this code, so existing data does
  not yet show the new route-family operation nodes.
- Caido project metadata shows Juice Shop `READY` but inactive and
  `sperpravo` current. No project switch or request enumeration was performed.
- A direct, read-only SQLite aggregate found 15,517 KB file rows, 68,878 chunks,
  and non-empty source hashes/current index-version metadata on all file rows;
  stored index metadata names `nomic-embed-text` at 768 dimensions. Full
  `kb_stats` source-freshness/vector-integrity checks did not complete within
  the bounded wait, so vector coverage and current source hashes remain
  unverified. No KB index was written.
- Projection jobs were not drained or reconciled. Caido coverage, complete
  source history, KB integrity, browser/JavaScript application coverage, and
  the Hermes model-backed consultation cycle remain unverified or partial.

## Stage 3 versioned operation grouping (2026-10-24 10:28Z) — PARTIAL

- Historical imports now add a separate `operation` node keyed by project,
  origin, method, `route-family-v1`, and its canonical route family. An
  endpoint links to that operation with `belongs_to_operation`; existing
  endpoint, capture, and card identities remain unchanged. Dynamic values are
  masked, while recognized static paths and HTTP methods stay distinct.
- The graph reader, writer, bounded slice, and inspector accept and display
  the new node and relation. Synthetic tests cover equivalent dynamic routes,
  static-route separation, method separation, provenance linkage, and absence
  of concrete path values in the grouped bundle. `TMPDIR=/var/tmp go test
  ./internal/graph`, both graph/surface browser-state suites, and
  `git diff --check` passed.
- This is a code/projection slice only: the local graph was not reprojected or
  restarted, and Caido was not queried. `route-family-v1` uses a static-word
  vocabulary, so unfamiliar static segments can still group as dynamic;
  analyst route aliases and actor/session/object/flow annotations remain
  outstanding. Stage 3 and source coverage remain `PARTIAL`.

## Caido recheck and graph viewport/capture-state fixes (2026-10-24 10:02Z) — PARTIAL

- With the user's approval, Caido was switched temporarily from `sperpravo` to
  the registered Juice Shop project for a read-only reconciliation of
  `192.168.1.149:3000`. Four metadata pages contained 355 Caido IDs and 355
  local historical/live IDs; all 355 matched, with zero unmatched,
  duplicate, or unclassified IDs. Enumeration reached the terminal cursor and
  the local inventory was complete. The active Caido project was restored to
  `sperpravo` and verified. No request details, bodies, or target traffic were
  used. Coverage remains `PARTIAL` because Caido supplies no stable snapshot
  token.
- Fixed a graph/UI labeling defect: `analysis_state` had been used as a
  fallback for `parse_state`, making an unfinished analyst card look like an
  incomplete HTTP parse. New graph projections now store parser state
  separately; the inspector labels analysis state separately and reports
  parser completeness as unknown when older graph data lacks that field.
- A live browser check on `GET /rest/products/search` showed the existing
  graph record as **Parse state: unknown** and **Analysis state: incomplete**,
  with a note that parser completeness is not recorded. Existing graph rows
  have not been reprojected with the new parse-state field, so they remain
  unknown until regenerated.
- The same browser check found that the desktop shell expanded to 2,892 px in
  a 720 px viewport, pushing graph nodes below the visible area. The shell now
  uses a fixed viewport-height grid with scrolling inside panels. After the
  fix, the shell measured 720 px, the graph canvas 405 px, and all 100 nodes
  were visible. The selected capture stayed inside the graph canvas at 300%
  zoom.
- `TMPDIR=/var/tmp go test ./...`, `TMPDIR=/var/tmp go vet ./...`, and all 32
  browser-state tests passed. Overall coverage remains `PARTIAL`.

## Stage 3 searchable operation fields (2026-10-24 09:32Z) — PARTIAL

- The operation catalog filter now performs a case-insensitive substring
  search over host, method, route, observed field names, and header names.
  When a field/header name matches, that operation's counts and graph seeds
  include only matching observations, so selecting the operation opens the
  relevant capture set. Host/method/route matches continue to include the full
  matching operation. The UI and Hermes tool description expose the expanded
  search.
- Field-filter pagination remains bound to the same project, filter, and
  structural-index fingerprint. Invalid or truncated field/header-name lists
  can hide matches; the API marks that limitation in coverage gaps. The query
  reads structural metadata only and never reads request/response values.
- Synthetic Go fixtures cover case-insensitive field/header matches, matched
  observation counts/graph seeds, and continuation across matching operations.
  `TMPDIR=/var/tmp go test -count=1 ./...`, `TMPDIR=/var/tmp go vet ./...`,
  and all 25 browser-state tests passed. The restarted local UI returns HTTP
  200 and serves the updated field-search controls.
- Stage 3 remains `PARTIAL`: versioned route-family grouping and analyst
  actor/session/object/flow annotations are still outstanding. This feature
  does not promote current source coverage or graph data beyond `PARTIAL`.

## Graph node zoom focus (2026-10-24) — PARTIAL

- The selected-node inspector now offers **Center and zoom to node**. It moves
  the selected node to the center of the graph and raises zoom to at least
  300%, while preserving a higher current zoom. The existing zoom controls can
  then increase magnification further.
- Pure transform tests cover centering and invalid geometry. A local browser
  click on the Juice Shop graph kept the selected capture in the inspector and
  set zoom to 300%. Graph data coverage and the overall application remain
  `PARTIAL`.

## High-degree graph layout (2026-10-24) — PARTIAL

- Graph layers with more than ten nodes now wrap into columns, keeping the
  number of rows bounded instead of placing every neighbor in one tall column.
  The root and each layer remain distinct; node and evidence IDs are unchanged.
- In the local browser, `GET /rest/products/search` displayed 100 loaded nodes
  and 99 edges with additional neighbors available. The new layout spanned
  2,250 by 1,152 layout units; centering the selected capture retained the
  inspector selection at 300% zoom. No HTTP bodies were opened.
- All 30 browser-state tests pass. This one-route UI check does not establish
  complete graph coverage or a project-wide layout/performance result.

## Local API and graph runtime smoke (2026-10-24) — PARTIAL

- Read-only calls to the actual local Juice Shop project returned one
  registered project and a surface catalog with 131 operations; the catalog
  correctly reports `partial` coverage.
- A direct graph request for one catalog seed returned 3 entities and 2
  assertions with `incomplete=true` and `truncated=false`. This verifies the
  UI/API/local graph-backend path for one neighborhood, not full graph or
  Caido-history completeness. The smoke did not read HTTP bodies or contact
  Caido.
- Direct graph responses deliberately remain `incomplete=true` because they
  assess a bounded selected neighborhood, not full-project integrity. The UI
  now explains that coverage flag separately from a capture's parser state,
  and explains `incomplete`, unknown context, and missing timestamp on the
  selected observation. All 29 browser-state tests pass; live browser clicks
  are now verified on one local route; this remains a focused interaction
  check rather than full browser/JavaScript coverage.

## Current Go and KB readiness (2026-10-24) — PARTIAL

- The local Go status report generated at 09:41:05Z found one registered
  project, compatible capture schema v7 and graph schema v2, 1,400 captures,
  1,369 graph-mapped captures and 31 unmapped. Historical sync remains
  `partial`; live sync is idle; the projection queue has 448 pending and 952
  applied jobs. Status did not contact Caido or check Neo4j connectivity.
  Focused local UI graph queries did read the configured backend, but source
  history and overall coverage remain `PARTIAL`.
- The local KB stats fallback completed after a slow read. It reports 15,517
  indexed files, 68,878 chunks and vectors, no orphan chunks/vectors, and no
  missing vectors; hybrid semantic/lexical search is available. The index was
  last refreshed at 2026-10-24 03:58Z. The current-source check counted 15,515
  compatible files; two indexed files were not counted as current-compatible,
  and their cause was not isolated. Full vector and source-integrity
  validation was not run. No KB refresh or reindex was performed; KB readiness
  remains `PARTIAL` pending that validation.

## Local application and Hermes runtime check (2026-10-24 09:26Z) — PARTIAL

- The Go web UI was restarted at `http://127.0.0.1:8765/` through the updated
  launcher; `/` returned HTTP 200. A read-only Neo4j check returned `ready`.
  This confirms the local UI and graph backend start, not complete graph or
  Caido coverage.
- Fresh Go status at 09:10:34Z reported capture schema v7 and BehaviorGraph
  schema v2 compatible, 1,400 captures, 1,369 graph-mapped, 31 unmapped, and
  queue counts of 448 pending, 0 leased, 952 applied and 0 dead-lettered.
  `neo4j-check` returned `ready`; overall coverage remains `PARTIAL`.
- The shared launcher now reads the optional private `runtime.env` file and
  passes its vault key to local Go processes. A synthetic regression test
  confirmed the key reaches the app process; the updated launcher and setup
  guidance make the existing exact-message preview usable when that key
  decrypts the selected local revision. The running UI was restarted to load
  the file. No real capture body was opened during this check.
- Route-mask help now explains that the graph omits dynamic path values and
  points to the explicit local **Open stored HTTP** action for historical
  captures; the inspector no longer implies that only Caido can show them.
  The UI was restarted after the copy change. This documents the safe
  inspection path; the exact local vault bytes were not opened in this check.
- A 20-minute local projection-worker run applied 612 jobs. Its JSON progress
  contained all 612 successful deliveries, matching the durable applied-count
  increase, while the summary showed only 600 because the final partial batch
  was dropped when shutdown interrupted the next claim. The CLI now accounts
  for completed partial-batch results before handling cancellation and does
  not emit an error result when shutdown occurs before a job is claimed. ECC
  regression tests cover both cases.
- The installed Hermes profile passed `./scripts/run-hermes mcp test
  bb_assistant` and discovered all 15 allowlisted tools. Hermes reports model
  unset, provider `Auto`, with GitHub Copilot available; no model call or real
  capture-content transfer was performed. The web graph and Hermes MCP are
  verified as separate local entry points; the model-backed consultation
  cycle is not yet runtime-verified.
- An approved metadata-only Juice Shop reconciliation enumerated 355 IDs for
  `192.168.1.149:3000` in 4 pages; all 355 matched the local index, with no
  unclassified IDs. No request/response bodies or target traffic were used.
  Caido was restored to the `sperpravo` selection and verified. Since Caido
  offers no stable snapshot token, `history_reconciled=false` and coverage
  remains `PARTIAL`.
- `PYTHONPATH=src python3 -m pytest -q tests/test_runtime_launcher.py`
  (4 passed), `bash -n scripts/bb-assistant`, the UI HTTP smoke check,
  `./scripts/run-hermes mcp test bb_assistant` (15 tools), and
  `node --test internal/webui/*_test.cjs` (25 passed), and `git diff --check`
  passed. Runtime state is still `PARTIAL`: 448 projections remain queued, 31
  captures are unmapped, and browser/JavaScript application coverage and
  model-backed consultation remain unverified.

## ECC graph audit correction and projection runtime (2026-10-24 08:21Z) — PARTIAL

- A regression fixture reproduced a false `incompatible` result when a valid
  BehaviorGraph schema-v2 database exceeded the 256 MiB graph-payload limit.
  The local BehaviorGraph file is 483,246,080 bytes and its bounded structural
  counts remain below the supported row limits. The read-only schema audit no
  longer treats the database file size as a schema-compatibility limit; its
  table checks and row-bounded counts remain in place. The fixture extends a
  valid schema-v2 database beyond 256 MiB and verifies that audit does not
  change its size or modification time.
- The fresh local status now reports capture schema v7 and BehaviorGraph
  schema v2 as `compatible`; 1,400 captures, 1,369 mapped, 31 unmapped, 1,183
  pending projection jobs, 217 applied, zero leased, and zero dead-lettered.
  `neo4j-check` returned `ready`. Overall coverage remains `PARTIAL`.
- ECC full Go tests, `go vet`, and race tests for graph/audit passed. The
  projection hot-path change reuses claim-validated payload data: a comparable
  three-job sample went from decode times of 8.5 seconds/job to zero and ack
  times of 7.9–26.7 seconds to 13–17 ms after the cold job. These timings are
  local samples, not a benchmark guarantee.
- A single watch worker ran for about 20 minutes and applied 179 jobs with no
  retries, dead letters, or worker errors. It stopped at its supervisor limit
  with no leased jobs. Remaining queue: 1,183 pending. Across the run, average
  claim/decode/projection/graph-import/Neo4j-sync/ack durations were
  1.67s/0/4.99s/1.18s/2.96s/27ms; the first cold graph-writer open took about
  152s. No Caido request bodies were read and no project was switched.
- History reconciliation still lacks a stable Caido snapshot token; 31
  captures remain unmapped; some captures remain partial or unparsed; browser
  walkthrough and JavaScript endpoint coverage are not present. Model
  execution and Caido live intake were not checked. Continue with the bounded
  projection backlog, then address source reconciliation and coverage gaps;
  do not report the project complete.

## ECC worker runtime evaluation (2026-10-24 07:28Z) — PARTIAL

- A local watch run reopened the configured project through the normal writable
  path and applied six projection jobs. The cold graph-writer open took about
  2.5 minutes; the run applied six jobs in roughly five minutes. The capture
  store file `graph.sqlite3` grew from 252 MB to 254 MB; the separate
  BehaviorGraph file `behavior.sqlite3` was 231 MB at the final snapshot, with
  no pre-run size recorded. No request bodies were read.
- A first monitoring wrapper polled `status` while the worker held its
  exclusive maintenance lease. The read timed out, the wrapper exited, and its
  child remained running until it was interrupted. A read-only audit then
  refused the capture store `graph.sqlite3` while its WAL sidecars were present.
  The normal worker writer reopened the capture store; the subsequent audit
  again reported capture schema v7 and BehaviorGraph v2 as compatible. Sidecars
  were gone after the worker stopped. This was a supervisor failure, not
  evidence of data corruption; do not remove SQLite sidecars manually.
- The post-run audit at 2026-10-24 07:28Z reported 1,400 captures, 1,369 graph
  mappings, 31 unmapped captures, and queue counts of 1,366 pending, 2 leased,
  32 applied, and 0 dead-lettered. The two leases remain visible after stop
  and must expire before those jobs can be reclaimed. The watch process is no
  longer running.
- `neo4j-check` returned `ready`. Caido remained on `sperpravo`; the Juice Shop
  project is ready but not selected. Caido was not contacted for captures and
  no project was switched. Historical reconciliation, complete queue drain,
  and browser/JavaScript coverage remain unverified, so runtime coverage stays
  `PARTIAL`.
- Next: measure per-job timing events with a supervisor that never audits
  during the maintenance lease, resume after the two leases expire, and keep
  the source-history and UI/JavaScript coverage gaps explicit.

## ECC watch-runner writer reuse and bounded runtime check (2026-10-24 05:46Z) — PARTIAL

- The CLI now owns one `ProjectionWorkerRunner` for the lifetime of a drain or
  watch invocation. It binds to one project key and storage directory, serializes
  calls, and keeps the path-only graph writer while releasing and reacquiring
  the project maintenance lease between watch batches. No SQLite connection or
  transaction is retained across batches; each graph import still validates
  the selected schema. A synthetic regression test runs two one-job batches
  under separate leases and confirms only one writer open.
- A bounded runtime watch run (`--max-jobs 1 --poll-interval 100ms`) applied
  four consecutive jobs on attempt 1. The first opened the graph writer in
  136,337 ms; the next three reported 0 ms open time and projection totals of
  400 ms, 414 ms and 6,108 ms. The process was interrupted after those four
  results. Post-run status confirmed zero leased jobs; the watch command's
  `go run` wrapper returned exit 1 on Ctrl-C after printing its summary.
- Full `TMPDIR=/var/tmp go test -count=1 ./...`, `TMPDIR=/var/tmp go vet ./...`,
  `TMPDIR=/var/tmp go test -race -count=1 ./internal/ingest
  ./cmd/bb-assistant`, and `git diff --check` passed. The regression test was
  first run red before the runner API was added, then passed.
- Fresh status at 2026-10-24 05:46:33Z reports schema v7/v2 compatible; 1,400
  captures, 1,369 mapped, 31 unmapped, 1,374 pending projection jobs, 26
  applied, zero leased and zero dead-lettered. `neo4j-check` returned `ready`.
  Overall coverage remains `PARTIAL`; history reconciliation and
  browser/JavaScript/source coverage are still incomplete. Caido remained on
  `sperpravo`; no project switch or request-body read occurred.

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
- Repeated the approved exact-host reconciliation on 2026-10-24 at 09:20 UTC:
  4 pages, 355 Caido IDs, 355 local IDs, all 355 matched, with no Caido-only,
  local-only, duplicate, or unclassified IDs. The run read metadata only; no
  request or response bodies were fetched and no target traffic was sent. The
  Caido project was switched back to `sperpravo` immediately afterward and its
  active selection was verified. Snapshot completeness remains `PARTIAL` for
  the same missing stable-snapshot-token limitation.
- Repeated the exact-host check through the Go `caido-reconcile` command at
  2026-10-24 15:50 UTC: 4 pages, 355 Caido IDs, 355 local historical/live IDs,
  all 355 matched, and zero Caido-only, local-only, duplicate, or unclassified
  IDs. No request or response details were fetched and no target traffic was
  sent. Caido was restored to `sperpravo` and verified. The enumeration still
  reports `coverage_reconciled=false` because Caido provides no stable snapshot
  token.
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
- Revalidated the same private snapshot on 2026-10-24 with
  `bb-assistant kb-validate`: `validated=true`, 14,300 documents,
  16,961 chunks, `source_coverage_state=PARTIAL`. The source-family gaps above
  remain unresolved; validation does not promote them.
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

- The full analytical Neo4j graph view builds a selected-query snapshot from a generation-rechecked native frontier instead of loading the whole project snapshot. It bounds each hop to 128 frontier entities, distributes the remaining share of a global 1,000-assertion budget across those frontier nodes, and caps cards at 200; card endpoint entities are loaded as dependencies, and entity provenance is read in batches of at most 100 owners. Exact-limit probes distinguish a full page from omitted candidates. Its generation-pinned continuation resumes renderer-omitted branches, up to 5,000 native assertion candidates, and up to 5,000 omitted native frontier nodes by merging selected records and rerunning the shared Reader from the original seeds; already-loaded assertions are excluded from each continuation query up to a 10,000-ID bound. Unprocessed queue tails survive each page, while capped queues and exclusion lists stay explicitly incomplete. Card and provenance candidate caps still lack continuation; bounded closure remains explicitly `PARTIAL`. Truncated omission catalogs remain marked as truncated and cannot report branch completion. Prepared full-page responses are capped at the MCP graph result limit (3 MiB), cached exactly for retry, and cursor sessions are bounded by active-session count, completed-session eviction, branch count, and a 15-minute TTL. Expiry during a read, generation changes, and service clearing invalidate the session before it can publish a new cursor. Synthetic Go and JS state tests and Neo4j integration fixtures passed against disposable Neo4j 2026.10.0 containers; they were removed afterward. Working project data and its query plan were not accessed, and the proposed focused-view p95 remains unmeasured. Caido and the working project database were not accessed for this code slice.

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

## Projection-writer diagnosis (2026-10-24) — PARTIAL

- The writer returns fixed, sanitized import-stage codes to the projection queue; raw database error text and record values are not persisted. Synthetic regression coverage now exercises reprojecting an existing card, batch merging duplicate card identities, preserving prior analysis, refreshing parser projection, updating the checksummed outbox, and idempotent replay. `TMPDIR=/var/tmp go test -count=1 ./internal/graph ./internal/ingest`, `TMPDIR=/var/tmp go vet ./internal/graph ./internal/ingest`, and `git diff --check` passed.
- Earlier in the same runtime diagnosis, a `--max-jobs 55` worker was interrupted after about 491 seconds opening the existing graph; one leased job was returned to `pending` as `worker_interrupted`, with `applied=0`. Two later single-job retries reported `projection_unavailable` and `graph_records_write_failed`; neither applied a projection. These worker attempts are not a full queue drain.
- Two bounded `projection-worker --max-jobs 1` invocations touched the registered Juice Shop queue during diagnosis. The first retried one pending job with `graph_cards_write_failed` at attempt 2; the second retried that job with `graph_immutable_record_conflict` at attempt 3. Both reported `applied=0` and `dead_letter=0`; graph imports failed inside their transaction and were not acknowledged. Neo4j sync did not run. No later queue audit was performed, so these figures are the last observed results, not a current queue inventory.
- The first attempt localized the failure to card insertion; the second classified it as an immutable-record conflict. A synthetic regression reproduced this conflict. The writer now refreshes deterministic endpoint/context/request/response projection fields, preserves existing business-action and hypothesis data, preserves `needs_review` only while the new request and response parse states are complete, and otherwise downgrades the card to `incomplete`. Missing-evidence strings are unioned so a reproject does not erase previously recorded gaps. The reconciled card is committed and checksummed in the Neo4j outbox in the same SQLite transaction; replay is idempotent. Since the last observed `applied=0` runtime result, there were **0 additional worker retries**. A later read-only Go project audit, Caido project list, and KB stats batch was canceled before returning results; it produced no new metrics. The fix is synthetic-verified only; runtime compatibility and queue status remain `PARTIAL`.
- A subsequent CLI link attempt failed because `/tmp` had only 26 MiB free. The active web UI process still owns its temporary executable, so no temporary files or running services were removed. Use `/var/tmp` for future Go build work and check disk capacity before another runtime command.

## Worker-to-graph and retry synthetic acceptance (2026-10-24) — PARTIAL

- A synthetic `PageWriter → projection worker → BehaviorGraph` scenario now
  accepts a request without a response, then a later complete JSON response.
  The actual local SQLite graph writer materializes both source-pinned
  revisions, the worker acknowledges both projection jobs, the current graph
  mapping points at the later revision, and its observation has
  `parse_state=complete`. The projector's remote-ready result is injected by
  the fixture; no Neo4j client, Caido, or working project database is used.
- A second scenario temporarily removes a source receipt in `t.TempDir()`. The
  real graph import returns the fixed `receipt_validation_failed` stage, while
  the accepted revision and retryable job remain committed. After closing and
  reopening the project store, the pending state is still present; restoring
  the receipt lets the same revision import and acknowledge on attempt two.
  The test projector injects remote readiness, so this verifies local graph
  materialization and durable retry only.
- The targeted race tests for both scenarios, uncached `internal/ingest` and
  `internal/graph` package tests, `go vet` for those packages, and
  `git diff --check` passed. Runtime queue health, Neo4j delivery, and Caido
  coverage remain `PARTIAL`.
- The opt-in multi-page provenance continuation subsequently passed against a
  disposable Neo4j database, as recorded below. Next local acceptance is a full
  HTTP/MCP cursor round trip with exact-boundary fixtures. Focused-view
  performance on the working dataset remains unverified.

## Entity provenance continuation (2026-10-24) — PARTIAL

- Full analytical graph continuation now pages entity source references past
  the per-read 64-reference bound. It excludes already returned owner/source
  pairs, retains one validated source anchor per owner so each continuation
  remains a closed graph snapshot, merges provenance pages only when the
  entity's non-provenance fields agree, and emits another branch while more
  references remain. The cursor is bounded at 10,000 previous source-reference
  keys and 5,000 anchors; exceeding either cap remains explicitly truncated.
- Focused synthetic Go tests passed for Neo4j page selection and validation,
  provenance-key construction and merge, branch scheduling, and page-scoped
  truncation flags. `go vet` passed for `internal/neo4jstore`,
  `internal/search`, and `internal/appquery` with `TMPDIR=/var/tmp`;
  `git diff --check` passed. An opt-in 66-reference Neo4j integration fixture
  passed against Neo4j Community 2026.10.0 in an isolated disposable
  container: page one returned 64 references, page two returned the retained
  anchor and both remaining references without truncation or repeats. The
  container was stopped and removed.
- Caido and the working project database were not accessed for this code
  slice. Runtime graph performance, HTTP/MCP continuation, Caido completeness,
  and KB coverage remain `PARTIAL`.
- The HTTP graph handler preserves provenance continuation flags, owner IDs,
  branches, and opaque cursors; the MCP `bb_graph_slice` description now
  documents resumable provenance branches and explicit cursor caps. Targeted
  appquery, MCP contract, and HTTP handler tests passed. These verify adapter
  contracts only; a full cursor round trip through appquery and the HTTP/MCP
  entry points remains outstanding.

## WAL-backed read-only surface access (2026-10-24) — PARTIAL

- The project read-only store previously refused every SQLite sidecar, so a
  valid project database left in WAL mode made `/api/surface` return HTTP 503
  and blocked `project-audit`. Read-only open now accepts only a regular,
  non-symlink `-wal`/`-shm` pair, uses SQLite `mode=ro` so committed WAL pages
  are visible, and retains the shared project lease for the read. A rollback
  journal, incomplete pair, or unsafe sidecar still fails closed. Databases
  without sidecars retain the immutable read-only path.
- Synthetic regression fixtures prove committed WAL data is visible through
  the appquery service and HTTP `GET /api/surface` (200), while a synthetic
  rollback journal remains unchanged and produces 503. The prior deduplication
  test fixture was updated to include valid source provenance required by the
  current graph contract.
- `TMPDIR=/var/tmp go test ./...`, `TMPDIR=/var/tmp go vet ./...`, and
  `git diff --check` pass. This slice was accepted on synthetic fixtures only;
  no live project database, running UI, Caido history, Neo4j service, or model
  was used for acceptance. Runtime verification remains `PARTIAL`, as do graph
  API/Neo4j response, Caido import coverage, queue delivery, KB readiness, and
  Hermes consultation.

Contributors should preserve explicit `PARTIAL` and `BLOCKED` states whenever data, coverage, or source provenance is missing.
