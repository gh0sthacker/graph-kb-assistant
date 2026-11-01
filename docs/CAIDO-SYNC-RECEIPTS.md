# Caido sync cycle receipts

`bb-assistant caido-sync` emits one JSON `project_sync_cycle_receipt` to stderr
for each project cycle that starts processing. The record summarizes bounded
work and makes the limits of the MCP source explicit. It is an operational log
record, not a cryptographic or immutable source receipt; deployment must retain
the service journal if operators need this history.

The receipt deliberately separates `cycle_state` from `coverage_status`:

- `cycle_state: processed` means all configured origin-scope stages returned
  successfully and the bounded graph projection drain completed. It does not
  mean all source history was imported.
- Each configured origin is present in `scopes` with its own `cycle_state`:
  `not_started`, `partial`, or `processed`. If a scope fails, later configured
  origins remain visible as `not_started` instead of disappearing from the
  receipt.
- `coverage_status` is always `unreconciled` and `coverage_complete` is always
  `false` for the current MCP path. Caido does not provide this adapter a stable
  snapshot token or an expected history total against which to reconcile the
  imported records.
- `backfill_state: done` and `backfill_cursor_state: complete` mean the cursor
  chain reported a terminal page. They do not prove that the chain represented
  an atomic, exhaustive snapshot. `backfill_limit_reached` identifies a bounded
  run that must resume.
- `live_tail_window_boundary` is the adapter's local time-window boundary, not
  a Caido source watermark. `live_tail_limit_reached` and
  `response_refresh_pending` expose bounded or unfinished work.
- `selection_binding` says project identity is checked before and after each
  MCP history call. This detects many project switches and aborts on mismatch,
  but cannot lock Caido's shared selection between those checks; concurrent
  project selection can still interrupt a cycle.
- `wire_fidelity` states that fidelity is recorded per exchange. The MCP text
  adapter marks reconstructed messages as `reconstructed_text`, and absent or
  unverifiable sides as `unknown`; it does not claim original wire bytes.

On a project-cycle failure the receipt is emitted with `cycle_state: partial`
before the generic sanitized error event. It includes the current scope's last
known backfill/tail status where those stages produced one. No cursor, request
ID, URL, body, credential, or raw MCP error is included in this operational
record.

The `--once` option performs exactly one inventory cycle and returns a
sanitized nonzero error when inventory enumeration or any project cycle fails.
Without `--once`, the manager continues polling and records errors while it
keeps running. `--once` is intended for bounded operator preflight, not as
evidence of full-history completion.

To claim complete coverage in a future adapter, require source evidence that
can be reconciled: at minimum a stable snapshot boundary and authoritative
expected total (or equivalent immutable source manifest), plus explicit
accounting for pagination, project binding, truncation, and byte fidelity. A
terminal cursor alone is insufficient.
