# Primitive and chain layer — design draft

Status: **DRAFT, not approved, not implemented.** This document proposes the next
application layer on top of the existing behaviour graph. Nothing here is built yet.

## Why

The graph already answers "what was observed": operations, captures, field
occurrences, keyed value equality, navigation links, analyst annotations,
exact comparisons. It does not answer "what does this unlock", which is the
actual hunting question. Mook's chaining model names four primitive classes:
information, input-control, identity, execution. A chain is the composition of
primitives until impact is undeniable.

We already own the hardest part for the first two classes: **keyed value equality
across captures** proves that the same value appeared in another context, which
is the ground truth behind IDOR/BOLA and token-reuse chains. Most harnesses do
not have this.

## Scope of the first slice

In scope: information and input-control primitives derived deterministically
from existing graph records, plus a chain candidate that cites them.

Out of scope for this slice: execution primitives, any active probe against a
target, browser/JavaScript coverage, automatic impact claims.

## Data model (Neo4j, same store as the behaviour graph)

Nodes, all project-scoped and generation-stamped like existing entities:

| Node | Key fields |
|---|---|
| `Primitive` | `primitive_id`, `project_key`, `class` (`information` / `input_control` / `identity` / `execution`), `subtype`, `basis` (`deterministic` / `heuristic` / `analyst`), `status` (`candidate` / `accepted` / `rejected` / `tested`), `version`, `semantic_status` (`heuristic_candidate` for the first slice) |
| `Chain` | `chain_id`, `project_key`, `status`, `impact_hypothesis`, `impact_level` (`informational` … `critical`), `chain_severity`, `version` |
| `ChainStep` | `step_index`, `primitive_ref`, `unlocks` (free-text hypothesis, never a fact) |

Relationships:

- `Primitive --derived_from--> CaptureRevision | FieldOccurrence | Assertion` (evidence, mandatory)
- `ChainStep --cites--> Primitive`
- `Chain --has_step--> ChainStep`
- `Primitive --unlocks_candidate--> Primitive` (hypothesis edge, always labelled)

Hard rules: a primitive without at least one evidence reference is invalid; a
chain with any step lacking evidence is invalid; nothing in this layer may set
`status=tested` without an attached PoC reference.

## First three detectors

Each detector is a pure function over existing records, deterministic, with an
explicit `basis` and no inference beyond what the graph already proves.

1. **`value_reuse_across_contexts`** (information). Two captures in the same
   project, different operations, where the same `field-equality-v2` value
   reference appears on both sides. Evidence: both occurrences plus their
   source spans. This is the IDOR/BOLA substrate.
2. **`redirect_to_external_origin`** (input_control). A 3xx `Location` whose
   origin differs from the request origin, or a `referrer` link crossing origins,
   already present as navigation records. Evidence: the navigation assertion.
3. **`reflected_input_in_response`** (input_control). A request field value that
   appears verbatim inside the same capture's response body, with the occurrence
   span recorded. Evidence: both occurrences. This is the injection substrate.

Deliberately excluded from this slice: anything requiring HTML/JS parsing beyond
what the existing occurrence records already store.

## Chain candidates

The first chain builder is a bounded enumeration, not a search:

- `information → identity`: a value from detector 1 appears in a request that
  changed authentication state or actor context (analyst annotation or a
  different session label), which is exactly the token-reuse chain.
- `input_control → information`: detector 2 or 3 plus a later capture that reads
  the affected resource.

Each candidate carries `chain_severity` computed separately from per-primitive
severity, following the ChainGraph pattern: an isolated medium can become
critical once the path is materialised.

## Surfaces

- MCP: `bb_primitives_list`, `bb_primitive_show`, `bb_chains_list`,
  `bb_chain_propose` (write, operator-initiated, versioned).
- CLI: a read-only `primitives-preview` that prints counts and masked samples,
  never raw values.
- Web UI: a primitive list panel next to the operation catalog; no raw values.

## Acceptance for the slice

1. Detectors run over the existing registered project (1 400 captures, 131
   operations) and produce primitives with evidence references only.
2. Every produced primitive resolves to an exact capture revision and span.
3. A synthetic fixture proves each detector fires on a crafted positive and
   stays silent on a crafted negative.
4. Chain candidates appear only when both steps have evidence.
5. No raw values, credentials or bodies leave the local store; MCP output uses
   the existing redaction path.

## Open decisions

1. Do primitives need analyst accept/reject in this slice, or read-only first?
2. Is `chain_severity` in scope now, or after the first chain examples exist?
3. Should the first detector run over historical captures only, or also the
   live-tail projection?
