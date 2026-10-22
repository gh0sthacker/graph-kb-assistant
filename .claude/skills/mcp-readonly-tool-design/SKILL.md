---
name: mcp-readonly-tool-design
description: Design or change the project's local MCP facade for read-only graph, capture, KB, and coverage queries.
---

# Read-only MCP tool design

Use this skill when defining or changing the MCP tools exposed to a harness.

- Expose narrow operations over the project's coordinator APIs; do not expose
  arbitrary SQL, shell, filesystem, proxy replay, or target-send capabilities.
- Give each tool a strict input schema, bounded result size, explicit project
  selection, and application-side validation. Model-provided arguments are
  untrusted input, not authorization.
- Make read-only behavior explicit in tool descriptions and MCP annotations.
  A tool that writes state must be designed and reviewed separately; never label
  a mutating action as non-destructive.
- Return provenance with each result: Caido instance/project/request IDs,
  capture revision or source hash, parser/index version, truncation, and
  coverage gaps as applicable.
- Redact secrets and sensitive values from model-visible results by default.
  Retrieve raw bytes only through a narrow, project-scoped application path when
  a task requires them; do not make them globally searchable or log them.
- Keep the published tool reference generated from the source-of-truth schema,
  and check that descriptions, annotations, argument validation, and behavior
  agree.
- An MCP server's read access does not imply permission to make target requests.
  The initial facade only reads local Caido history, graph slices, KB sources,
  and coverage.

This project-local skill adapts API-contract lessons from the pinned RedAmon
`mcp-server-tools` source; see `docs/REDAMON-SKILLS-ADOPTION.md`.
