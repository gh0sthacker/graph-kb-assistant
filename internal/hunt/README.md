# Evidence-backed hunting surface (P1 + P2)

`attack-surface` is a local read, not a scanner or a finding verifier. The CLI,
HTTP handler and MCP tool share `appquery.Service.AttackSurface` and the JSON
contract in `attack-surface.schema.json`. Existing consumers are unchanged;
this is an additive API. HTTP inherits the existing local UI host/origin guards;
MCP inherits the registered-project allowlist. Neither surface sends captured
bytes to an LLM. The query never starts a Caido process.

```sh
bb-assistant attack-surface --project-key KEY --classes sqli,xss --limit 50 --json
# GET /api/attack-surface?project_key=KEY&classes=sqli,xss&limit=50
# MCP bb_attack_surface: {"project_key":"KEY","classes":["sqli","xss"],"limit":50}
```

Default limit is 50, range 1–1000. Classes are exact identifiers documented by
`hunt.Classes`; unknown classes, malformed keys and invalid limits are errors.
CLI exits nonzero; HTTP uses the existing error envelope (400 invalid input,
404 unregistered project, 503 unavailable project/evidence); MCP returns a tool
error. No raw payloads or token values are returned in errors. Empty evidence
produces zero candidates, never inferred execution sinks. `uncovered` describes
classes with no supported candidates, **not** absence of vulnerabilities. Stats
cover the complete projected graph before class filtering/limit.

## Package and extraction

- `extract.go`: bounded in-memory HTTP/JSON/form/query/header/path/hash parsing,
  scalar types, nonsecret samples (<32 bytes, up to five), exact distinct counts
  using digests, reflection contexts, fixed error signatures, opaque session
  references and fully redacted disclosed information.
- `graph.go`: merges observations, creates evidence-bearing edges and candidates,
  computes scores and sorts descending score / ascending stable candidate ID.
- `sqlite.go`: private project-local `hunt.sqlite3` cache, transactional upsert
  and replacement of derived graph tables. No graph/KB/model dependencies.
- `ingest/hunt.go`: import hooks, revision-bound read-only cache joins, legacy
  historical extraction and an explicit legacy-live backfill.

A signature co-occurring with a parameter is a reachability **hypothesis**, not
causal taint analysis. `why` makes this limitation explicit. Candidate identity
is project/host/port/method/route/parameter-location/name/class, independent of
score, samples or capture ordering. `why` contains exact capture, observation
and (where present) revision locators. UNLOCKS needs both anonymous 401/403 and
authenticated 200 on the same operation. Different opaque sessions are observed
identities, not independently verified application roles.

Plain error text scores confidence 0.6; a stack frame scores 0.9. HTML text,
attribute and JS reflection produce XSS hypotheses; JSON-only reflection is
`input-validation`, not executable XSS evidence. Known parameter names can also
produce lower-confidence `input_control`/`authorization` hypotheses, explicitly
**not** SQL/SSRF/filesystem execution sinks. SSRF execution is not inferred from
a URL-shaped parameter or an ordinary 200 response: this source has no server
initiator/OOB evidence. Timing/differential evidence is likewise not fabricated
from the structural index. Browser fragments are absent from ordinary HTTP;
only fragments actually present in a captured target can be extracted.

### Score policy

The exact ADR product is implemented without additive scores:

```
sink_confidence * (0.5 + 0.5 * reflected_bonus) * class_fit
                * identity_bonus * novelty * state_changing_bonus
```

The ADR does not specify numeric factors for fit/identity/state changes. This
implementation makes those choices explicit: reflection 0/1; name fit 1 for
ADR table matches, 0.5 otherwise; identity multiplier 1.5 for idor/bfla/bopla
when at least two identities are observed, 1 otherwise; state multiplier 1.5
for state-changing business-logic/bfla, 1 otherwise. `role -> bopla/bfla` is the
acceptance-example extension to the ADR name table. Signature-free name-fit
hypotheses use confidence 0.3. Novelty supports untested=1 and tested=0.3;
P1/P2 observations are untested (no finding/verdict lifecycle is invented).
Playbook paths are class identifiers for downstream hunter-tools; this package
does not claim those external files or executed checks exist.

## Storage contract / migration

`PRAGMA user_version=1`; unsupported versions fail closed. Tables:

| Table | Keys and indexed fields | Payload |
|---|---|---|
| observations | capture_id PK, observation_id, revision_id | sanitized extracted observation JSON |
| endpoints | id PK, method, route_template, host, port, auth_required, state_changing | complete endpoint JSON |
| parameters | id PK, endpoint_id, name, location CHECK | types, samples, value digests, distinct_count, reflection JSON |
| sinks | id PK, kind, confidence CHECK 0..1 | engine, evidence kind, fixed signature JSON |
| identities | id PK, label, token_ref | capabilities JSON (never tokens) |
| infos | id PK, kind, redacted CHECK=1 | where and `[REDACTED]` value JSON |
| edges | (candidate_id, from, to, kind, status) PK | nonempty evidence_json CHECK |
| candidates | id PK, class, score; score DESC/id index | nonempty why_json CHECK, full candidate JSON |

Expansion creates only the new cache; backfill upserts at most 100 observations
per batch; query switches to a cache observation only when its imported current
revision matches. No old tables are dropped. Rollback is reverting the binary
and ignoring/removing only the derived hunt cache, never the source database.
Import caches are committed before the authoritative import checkpoint; failed
cache writes roll that checkpoint back. A cache ahead of a failed source commit
is rejected by the revision join. Queries remain compatible with older projects.

Short nonsecret samples now intentionally exist **only** in hunt.sqlite3. The
existing structural graph/search stores still discard values. Authorization,
cookies, passwords, secret-like assignments, JWTs, hash values and common PII
are excluded from samples. Only opaque token references and distinct-value
digests persist. Info values are always redacted; no SQL error snippets or
response bodies are stored, only fixed signature names. Redaction remains
heuristic; this is private data, not a safe-to-publish export.

## Legacy data and bounded recovery

Historical vault messages are authenticated and pinned to an exact revision,
with a 1 MiB per-side limit. Live imports predating P1 have no local bodies:
queries report those rows as `coverage_gaps`. They do not invent signature,
reflection or identity evidence from value-free metadata.

For an operator-authorized **copy** of a legacy live project:

```sh
bb-assistant hunt-backfill --project-key KEY --data-root COPY_ROOT --read-existing-caido
```

This optional migration reads only IDs already in the copied capture index,
checks source identity and full revision digest, clears message buffers, writes
only hunt.sqlite3, and reports missing/mismatched revisions. It does not list
new target requests, send HTTP probes, change capture tables, or touch indexes.
It uses the existing read-oriented Caido adapter and its selection restoration
contract. Do not run against a source project when project writes are forbidden.
A full project copy needs `graph.sqlite3`, `vault/`, `behavior-receipts/`, the
maintenance lock and a matching project registry; BehaviorGraph is not read by
this use case. Source bytes missing in Caido cannot be recovered from metadata.

P3 chain traversal, verified findings, sweep wiring and coverage-verdict reporting
belong to separate work; a prepared candidate is not an executed test.
