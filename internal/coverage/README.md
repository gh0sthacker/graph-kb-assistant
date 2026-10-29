# Run coverage matrix

Coverage is local run state in `<data-root>/coverage.sqlite3`, using the existing
SQLite driver (no new dependency). Run IDs are lookup values, never file paths.

```sh
bb-assistant coverage --run RUN_ID [--json] [--data-root PRIVATE_ROOT]
```

Without `--data-root`, the normal `BB_ASSISTANT_DATA_HOME` / application data-root
resolution applies. CLI and MCP reads open an existing database read-only. Missing
databases and runs return errors, not successful empty/100%-covered reports.

MCP: `bb_coverage_matrix`, input `{"run":"RUN_ID"}` (required string, 1–256
characters, not blank; no additional properties). The shared catalog exposes it
to stdio and Streamable HTTP with read-only/local annotations. No allowlist change.

## Recording and persistence

```go
m := &coverage.Matrix{Run: "RUN_ID"}
m.SetBudget("xss", 2) // creates an untested cell
m.Record("xss", coverage.Confirmed, "capture:123")

s, err := coverage.Open(filepath.Join(dataRoot, coverage.DatabaseName))
// Handle err, defer s.Close(). Load an existing run before updating it.
err = s.Save(ctx, m) // atomic upsert by (run, class)
m, err = s.Load(ctx, "RUN_ID")
```

`coverage_cells(run, class, status, tests, budget, evidence_json)` has a composite
primary key `(run, class)` and constraints on statuses and nonnegative counters.
Save preserves other cells/runs and rejects empty or invalid matrices. New files
are mode 0600. Evidence is an array of reference strings, not raw traffic/secrets.
There is no automatic coupling to scanners or existing report generators: callers
record outcomes and must consult `CanFinish()` when deciding whether to finish.

## Semantics and reporting boundary

- Status: `confirmed`, `ruled_out`, `untested`, `blocked_first_party`.
- Each valid `Record` counts one attempt, including blocked/untested attempts,
  and appends nonempty evidence. A confirmed finding cannot be erased by a later
  negative/blocked attempt. Invalid outcomes/empty classes are ignored.
- `SetBudget` declares a class; negative budgets are clamped to zero.
- `CanFinish` is false if any cell has `tests < budget`, or is `untested` with a
  positive budget. `open` is a sorted list of these classes. Invalid/nil cells
  also fail closed. A blocked cell does **not** bypass an unmet budget.
- `coverage_pct` is the percentage of declared classes with a confirmed or
  ruled-out outcome; blocked and untested classes are not covered. It measures
  outcomes, independently of budget exhaustion. Empty matrices have 0% coverage.
- `FindingsPerCall(calls)` is confirmed classes / calls; nonpositive calls yield 0.
- An empty in-memory matrix has no open declared budgets, but cannot be saved or
  returned as a successful missing run. Completion covers only declared classes.
- If `can_finish=false`, a report must not claim there are no vulnerabilities.
  **Even `can_finish=true` is not proof of absence:** blocked classes can exhaust
  their attempt budget and zero-budget untested classes remain coverage gaps.

## JSON contract and three-class example

The top-level fields are `run`, `cells` (array sorted by class), `coverage_pct`,
`can_finish`, and `open`. Each cell has exactly `class`, `status`, `tests`, `budget`,
and `evidence` (array). Empty arrays are `[]`, never `null`.

```json
{
  "run": "demo",
  "cells": [
    {"class":"idor","status":"untested","tests":0,"budget":2,"evidence":[]},
    {"class":"sqli","status":"ruled_out","tests":1,"budget":1,"evidence":["req-2"]},
    {"class":"xss","status":"confirmed","tests":1,"budget":1,"evidence":["req-1"]}
  ],
  "coverage_pct": 66.66666666666667,
  "can_finish": false,
  "open": ["idor"]
}
```
