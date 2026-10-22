---
name: caido-history-graph
description: Design or implement the project's read-only Caido history intake, per-project graph import, search, and coverage reporting.
---

# Caido history to behavior graph

Use this skill when changing how the assistant imports, stores, parses, or
searches Caido HTTP history.

- Keep each Caido project in its own namespace and only search user-selected
  projects.
- Include the Caido instance and project identity in graph keys and capture
  references. Natural endpoint/request keys alone must never merge observations
  across projects.
- Make page/batch writes idempotent and revision-aware. Do not clear existing
  graph state before a new import has been validated; preserve the last known
  good revision and report partial imports as partial.
- Use the Caido MCP read API first. Do not query Caido's private database or
  issue requests to target hosts.
- The explicit historical backfill may preserve raw capture bytes only in the
  encrypted project vault. Continuous live-tail processing keeps Caido as the
  canonical raw store: parse unseen messages in memory, discard raw bytes, and
  retain only graph shapes, keyed signatures, and Caido references. Graph
  facts, logs, receipts, and output must never contain raw values or secrets.
- Reuse the existing BehaviorGraph parser/importer instead of introducing a
  parallel graph schema.
- Track enumeration, body completeness, parse status, and graph coverage
  separately. `done` enumeration alone does not mean complete content coverage.
- Keep project selection and restoration sequential because Caido's active
  project is shared mutable state.
- Report unknown/truncated exchanges explicitly; never imply that metadata-only
  enumeration is a complete import.
