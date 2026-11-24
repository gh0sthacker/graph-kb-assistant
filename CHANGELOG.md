# Changelog

## 0.2.0-alpha — 2026-11-24

### Search and knowledge base

- Add an HTTP search API and a standalone `kb-search-api` service that serves `/api/kb/search` and `/api/health` from read-only SQLite indexes, so a KB host without a Go toolchain can run search as a separate process.
- Add an optional disclosed-report supplemental index (SQLite FTS5, built by `scripts/build-hackerone-index.py`) reachable as the `hackerone` pseudo-source, fused with primary KB hits through reciprocal rank fusion. A missing or incompatible index is disabled at startup instead of failing search, and a supplemental failure never hides a usable KB result.
- Align chunking, ranking stages, Lucene boolean query semantics, MMR title normalization, source-filter ordering and rerank tail order with the pinned reference corpus, so parity comparisons exercise equivalent retrieval paths.
- Align source parsing with the pinned revisions: NVD page size and text clipping, ExploitDB metadata-only records, ToolDocs category coverage and search normalization, GTFOBins binary suffixes, and retention of unparseable nuclei templates.
- Add a `lite` source profile (ToolDocs, GTFOBins, LOLBAS) alongside `full`, with source-specific vector readiness policy and partial vector coverage reporting.
- Add resumable vector builds with length-batched embeddings (up to 32 per batch), an optional thermal guard with spike tolerance, orphaned-vector pruning after rechunk, and exposed vector build progress.
- Add a fail-closed FAISS retrieval backend, a run-time selectable `sqlite-cosine` vector engine, and opt-in supplemental fallback search.
- Report stale source manifests in KB stats and coverage.

### Graph, ingest and analysis

- Add continuous Caido project lifecycle sync: new project IDs get stores, historical import resumes, new captures tail, and the projection queue drains without restarting the web UI or MCP process.
- Fix live-tail completion when an advancing history page is empty, stop historical backfill on repeated cursors, and align the live projection payload limit with graph bundles.
- Tighten attack-surface extraction: nested JSON parameters, strong-route grouping, and an attack-surface endpoint and CLI command.
- Add a persistent run coverage matrix exposed through both CLI and MCP, and a fail-closed run gate that refuses to proceed on missing readiness data instead of treating it as a skipped probe.
- Add bounded projection dead-letter replay with a dry-run-first, digest-validated attempt under the project maintenance lease.
- Fix graph continuation across zero-edge pages and yield project reads between projection deliveries.

### Web UI, MCP and consultation

- Add the remote Streamable HTTP MCP transport at `/mcp` beside the stdio adapter, with a shared bounded tool catalog.
- Make MCP tool arguments explicit and forgiving, mask short opaque values under bare sensitive keys, and keep operational tooling read-only.
- Clarify consultation session visibility: a created session keeps `content_withheld` set until the operator accepts it locally, and pre-acceptance comparison, proposal, validation and candidate operations stay rejected.
- Persist empty comparison lists consistently and enable reviewed local consultation writes.
- Expose the embedded build revision in the health version string.
- Fix the CLI default project data root and retain the revision when launching from a git source checkout.

### Packaging and CI

- Add an offline, fail-closed RedAmon KB parity receipt checker. It compares complete corpus/source/vector evidence and fixed-query ordered results; receipt provenance still requires external verification.
- Add `parity-export` to generate a local receipt from a matching read-only snapshot/index only after source and vector completeness checks pass.
- Publish a commit-pinned Linux runtime artifact from CI and verify its embedded revision and checksum.
- Scan pushes and pull requests for secrets with a digest-pinned Gitleaks image.
- Update GitHub Actions to Node 24 compatible releases.
- Update `golang.org/x/net` to v0.61.0, including the upstream HTML parser denial-of-service fix.

## 0.1.0-alpha — 2026-10-22

First public source snapshot. Includes the Go graph explorer and project-scoped Caido workflows, default read-only MCP tools, source-aware Python knowledge-base tooling, and explicit partial-coverage reporting. This alpha is not a claim of complete Caido compatibility or a finished end-to-end analysis workflow.
