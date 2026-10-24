# Local consultation workflow

Consultation sessions are stored in an owner-only SQLite database inside the
selected project's local data directory. Each session currently belongs to one
project. It contains the analyst's redacted question, selected exact historical
revision spans, deterministic comparison records, proposal receipts, and
candidate versions. It does not import Caido traffic or call a model.

## Review boundary

The local web UI first builds a bounded redacted preview. Creating a session
stores that packet, but its question and excerpts remain hidden from Hermes.
Before review, MCP responses also omit the evidence hashes and redaction spans;
comparison, proposal, validation, and candidate operations are rejected.

The operator must inspect the saved packet in the local UI and explicitly
accept it. That local-only transition is not an MCP tool. After acceptance,
Hermes can resume the session with `bb_consult_resume` using the copied
`project_key` and `session_id`. This allows access to the saved redacted packet;
it does not send the packet automatically or select a model provider.

Version-1 sessions migrate as unaccepted. Before operator acceptance, Hermes
also receives no prior comparison, proposal, receipt, or candidate records from
the session, and assistant mutation retries cannot replay pre-migration results.
If a legacy candidate-save retry is accepted after review, its missing candidate
ID is recovered from the matching stored candidate version.

Redaction is heuristic. The operator must inspect each question and excerpt
before accepting because a detector may miss sensitive values. Raw captured
messages are not copied into the consultation database or MCP results.

## Evidence and candidate state

Comparison records bind nested request and response summaries to the exact
request and revision IDs. Proposal validation rereads the selected spans from
the local encrypted source and reruns each cited comparison. If exact source
bytes are missing or changed, validation fails closed. A valid receipt proves
reference identity and integrity only; it does not prove semantic support,
impact, or a vulnerability.

Each candidate has a stable opaque ID and its own immutable version sequence.
Candidate state starts as `candidate`; saving never promotes it to
`reproducing`, `reproduced`, or `verified`. Candidate records are append-only
through the application API. SQLite triggers enforce that API contract, but
the local database is not a tamper-evident ledger against a user with direct
filesystem access.

Schema version 2 adds the operator-review event and candidate IDs. The
migration preserves existing version-1 candidate history by assigning its
prior session-wide version chain one stable candidate ID. Existing sessions
remain unaccepted until an operator reviews them under the new boundary.

The first implementation is single-project and uses local historical data.
Cross-project sessions, live Caido fetches, actor-aware comparisons, a
configured Hermes model/provider, and a model-backed evaluation remain
unimplemented or unverified. Overall coverage stays `PARTIAL` until the
project-specific synchronization, runtime compatibility, KB, and full
consultation gates are verified.
