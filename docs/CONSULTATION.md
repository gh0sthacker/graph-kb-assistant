# Local consultation workflow

Consultation sessions are stored in an owner-only SQLite database inside the
selected project's local data directory. Each session currently belongs to one
project. It contains the analyst's redacted question, selected exact historical
revision spans, deterministic comparison records, proposal receipts, and
candidate versions. It does not import Caido traffic or call a model.

## Review boundary

The local web UI first builds a bounded redacted preview. Creating a session
stores that packet, but its question and excerpts remain hidden from Hermes.
Before review, assistant-visible session JSON sets `content_withheld` to `true`
and lists hidden paths in `withheld_fields`. The path names are
`question`, `evidence.question`, `evidence.question_redactions`,
`evidence.evidence[].excerpt`, `evidence.evidence[].sha256`,
`evidence.evidence[].redactions`, `evidence_references[].sha256`,
`context_annotations`, `comparisons`, `proposals`, `validation_receipts`, and
`candidates`. The question and evidence remain withheld; these markers do not
change the review boundary. After acceptance, `content_withheld` is `false` and
`withheld_fields` is an empty array. MCP responses also omit evidence hashes and
redaction spans before review; comparison, proposal, validation, and candidate
operations are rejected.

The operator must inspect the saved packet in the local UI and explicitly
accept it. That local-only transition is not an MCP tool. A session response
exposes the same opaque identifier as both `consultation_id` (the existing name)
and `session_id`. The consultation MCP tools accept either name; if a caller
supplies both, they must have the same value. After acceptance, Hermes can
resume the session with `bb_consult_resume` using `project_key` and either ID
name. This allows access to the saved redacted packet; it does not send the
packet automatically or select a model provider.

The synthetic MCP lifecycle test injects a fixture analyst adapter at the
model boundary. It receives only the accepted resume result and sanitized
deterministic comparison, derives a proposal and exact citation IDs, then sends
that proposal through the same MCP persistence and validation path. The fixture
rejects unreviewed sessions, unavailable comparisons, and raw excerpt markers.
It is test-only. Hermes is configured with `gpt-6-luna`, and a model-backed
synthetic consultation/resume has passed. No captured project packet has been
sent to the model; its handling of real capture content remains unverified.

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
Cross-project sessions, live Caido fetches, actor-aware comparisons, and a
model-backed evaluation on reviewed real capture content remain unimplemented
or unverified. The Hermes provider itself is configured and its synthetic
workflow passed. Overall coverage stays `PARTIAL` until the
project-specific synchronization, runtime compatibility, KB, and full
consultation gates are verified.
