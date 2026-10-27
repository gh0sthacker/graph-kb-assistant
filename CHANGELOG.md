# Changelog

## Unreleased

- Add an offline, fail-closed RedAmon KB parity receipt checker. It compares complete corpus/source/vector evidence and fixed-query ordered results; receipt provenance still requires external verification.
- Add `parity-export` to generate a local receipt from a matching read-only snapshot/index only after source and vector completeness checks pass.
- Update GitHub Actions to Node 24 compatible releases.
- Update `golang.org/x/net` to v0.61.0, including the upstream HTML parser denial-of-service fix.

## 0.1.0-alpha — 2026-10-22

First public source snapshot. Includes the Go graph explorer and project-scoped Caido workflows, default read-only MCP tools, source-aware Python knowledge-base tooling, and explicit partial-coverage reporting. This alpha is not a claim of complete Caido compatibility or a finished end-to-end analysis workflow.
