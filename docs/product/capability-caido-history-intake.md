# Capability: Caido history into a local behavior graph

## Capability

The workspace owner can select one Caido project, import its saved HTTP history
into a private local project store, and inspect request/response relationships
in the graph explorer. This makes their existing Caido traffic useful to the
personal bug-bounty assistant without sending new traffic to a target or
exposing raw captures to a model.

## Constraints

- A Caido project is an explicit input; import exactly one registered project
  per run and keep its store, graph, receipts, and capture references isolated.
- Caido's documented read API is the only capture source. Never read Caido's
  private database or send replay, fuzz, or other active requests.
- Keep historical raw request/response messages only in that project's
  encrypted local vault. Keep the continuous live-tail path shape-only.
- Store runtime data and the vault key outside Git on a private local
  filesystem. Do not store raw captures, credentials, or user data in graph
  labels, logs, receipts, the global KB, or model-visible default tools.
- Preserve request IDs, page cursors, capture revisions, and graph provenance.
  Retry safely; never erase an existing graph before a replacement is
  validated.
- A terminal cursor is `terminal_unreconciled`, not proof of a stable complete
  snapshot. Body completeness and reconstructed-text fidelity are separate
  coverage dimensions.
- The model/provider remains unset. Real capture values do not go to a model
  until its retention and data boundary are explicitly known.

## Implementation contract

### Actors and surfaces

- Operator: selects the Caido project and starts a bounded import or live-tail
  cycle from the local CLI.
- Caido MCP read bridge: lists projects and reads HTTP history only.
- Local assistant: registers project identity, encrypts historical messages,
  parses exchanges into BehaviorGraph, indexes structural metadata, and serves
  the read-only graph explorer and MCP facade.
- Graph explorer: shows selected-project requests, endpoints, fields, responses,
  and observed edges with exact Caido request provenance and coverage notes.

### States

`unregistered -> registered -> importing -> partial | terminal_unreconciled`

An interrupted/failed page remains partial and resumes from the last committed
cursor. `terminal_unreconciled` cannot be promoted to complete until a stable
source inventory is independently reconciled. A live-tail run is separately
reported by its watermark/cursor and bounded-page coverage.

### Interfaces and data

- Project identity: stable local Caido instance key + opaque Caido project ID;
  the project name is display metadata only.
- Import input: exactly one registered project key, page size (1–100), bounded
  page count, maximum per-side body size, and page delay.
- Source calls: project listing, project selection, paginated HTTP-history
  metadata, and batched request/response detail reads. The bridge rejects all
  other Caido tool names.
- Import output: per-project run ID, pages and request IDs committed, exchanges
  seen/saved this invocation, cursor state, parse/body/fidelity coverage, and
  sanitized errors.
- Local effects: project registry, SQLite graph/search data, immutable
  encrypted capture objects, and resumable sync receipts. Caido's project
  selection is restored after reads when its API permits.

## Non-goals

- Sending or modifying target requests, active scanning, fuzzing, or replay.
- Importing every Caido project implicitly or merging project graphs.
- Treating imported historical traffic as evidence for a different authorized
  pentest run.
- Sending capture contents to an assistant or model provider, or to a remote service.
- Claiming byte-exact fidelity, stable snapshot completeness, or application
  coverage from history alone.

## Open questions

- Caido MCP exposes a mutable global project selection and no snapshot token;
  concurrent project switching and point-in-time completeness cannot be
  proven. The importer must fail closed on observed selection changes and keep
  coverage unreconciled otherwise.
- Caido's MCP message shape is text. Binary bodies and original HTTP framing
  cannot be recovered from reconstructed text.
- The operator remains responsible for preserving the generated vault key;
  loss of that key makes encrypted historical raw captures unreadable.

## Handoff

- **Implementation:** complete for selected-project registration, resumable
  history import, encrypted local storage, graph explorer, and one-cycle
  live-tail integration.
- **Verified:** imported 355 exchanges across 8 pages from the explicitly
  selected Caido project; the source cursor reached its terminal page. The
  one-cycle live tail connected and returned idle with no new exchanges. The
  graph UI displayed 80 nodes and 96 relationships for the selected route;
  loading another endpoint page preserved the selected graph. The MCP
  smoke test connected and exposed the 8 allowlisted read-only tools. Go tests,
  `go vet`, and the Python suite passed.
- **Coverage:** `terminal_unreconciled` / `PARTIAL`. Caido exposes no stable
  snapshot token or expected total, so terminal cursor does not verify that
  this import is a complete point-in-time inventory. MCP text reconstruction
  is not byte-exact, oversized bodies may be incomplete, and the full
  model-backed scenario remains unverified because no provider is configured.
- **Runtime:** local graph UI is available at `http://127.0.0.1:8765/`.
  Encrypted captures and the vault key are in a private runtime directory
  outside Git.
- **Architecture review:** not required; the project already fixes the
  read-only bridge, encrypted vault, per-project namespace, and graph contracts.
- **Product clarification:** not required for this selected-project import.
- **Next ECC lane:** `verification-loop` for future integration changes.
