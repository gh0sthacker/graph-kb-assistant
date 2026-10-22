---
name: evidence-triage-lifecycle
description: Keep observed evidence, model assessments, candidate hypotheses, and verified findings separate.
---

# Evidence and triage lifecycle

Use this skill when designing graph observations, hypotheses, review records, or
finding status transitions.

- Keep immutable observations separate from interpretations. Every interpretation
  points to the exact capture/source IDs and content hashes it used.
- Separate deterministic metadata/rules, model review, and user decisions into
  distinct records. A model may propose or challenge an assessment; it cannot
  overwrite source evidence or a user's decision.
- Bind a model review to a hash of the evidence bundle and to the prompt/skill
  version. If evidence, redaction, or instructions change, mark the review stale
  and rerun it before relying on it.
- Preserve explicit states such as `candidate`, `reproducing`, `reproduced`, and
  `verified`. A score, graph path, KB match, or model confidence never promotes a
  candidate to `verified` by itself.
- Keep contrasting explanations, missing context, and the smallest safe next
  check visible in the candidate record.
- Use project-scoped IDs and access controls for every stored record. Never let
  one Caido project's evidence or review leak into another project's search.

This project-local skill adapts triage-layering lessons from RedAmon's pinned
`priority-board-triage` source; see `docs/REDAMON-SKILLS-ADOPTION.md`.
