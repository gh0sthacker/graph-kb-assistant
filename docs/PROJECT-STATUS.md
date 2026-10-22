# Project status and known limitations

This repository is an early public alpha. The following boundaries are deliberate and should remain visible in documentation and user-facing output:

- Caido 0.58.3 with the pinned community MCP bridge v4.3.0 and the current Go capture schema v5 / BehaviorGraph schema v2 have been exercised in one development environment. A read-only metadata reconciliation matched the current local history inventory, but full compatibility across Caido versions and workspaces is not established.
- Caido does not currently provide a stable history snapshot token to this workflow. A completed metadata enumeration therefore cannot prove that the source was unchanged throughout the read; reconciliation reports `PARTIAL`.
- Some imported exchanges have incomplete parse or response-body coverage. HTTP status or endpoint presence alone is not a complete behavioral analysis.
- Browser walkthrough and JavaScript endpoint coverage are not yet part of a complete end-to-end application-map workflow.
- The private local KB index currently has 14,300 documents and 16,961 chunks across five complete source families. NVD remains `PARTIAL` after a provider failure; ExploitDB remains `BLOCKED` pending dataset-terms review. The FTS5 snapshot was validated and imported locally; no compiled corpus is included in Git.
- Hermes is an experimental harness integration; it is not a supported, end-to-end hypothesis-analysis loop.
- Candidate hypotheses and graph relationships are not confirmed findings. Evidence validation and human review remain necessary.

## Latest local integration checks (2026-10-22)

- The Go runtime opened the working capture and graph stores read-only and reported capture schema v5 and BehaviorGraph schema v2 compatible. The project audit counted 377 captures, all 377 search-indexed and graph-mapped; parsing is complete for 143, partial for 234, and unparsed for 0.
- A bounded read-only Caido reconciliation enumerated 377 IDs in 8 pages. All 377 matched the local history, with no Caido-only, local-only, or duplicate IDs. Coverage remains `PARTIAL`: Caido provides no stable snapshot token or expected total, so a terminal cursor cannot prove a point-in-time complete history.
- One bounded live-tail cycle completed one page with `idle` and 0 new requests. The live index remains empty. This verifies the no-new-data path only; ingestion of new traffic and a persistent background worker are not verified by this check.
- The imported KB snapshot passed validation and SQLite integrity checks. A fresh read-only Go MCP run against the private Python-built FTS5 index matched Python SQLite stats by snapshot ID, coverage report hash, five source families, 14,300 documents, and 16,961 chunks; both reported `PARTIAL`. `bb_kb_search` returned three hits, each with valid source and chunk SHA-256 provenance. The check did not read or print source excerpts.
- Go KB regression tests now build a synthetic database with the Python FTS5 schema and verify stats, filtered search, source retrieval, truncation and content-hash behavior. They also assert that read operations leave the index bytes unchanged and that a missing index is not created.
- Hermes discovered the eight configured read-only tools. Its model provider is not configured, so the model-backed analysis and hypothesis cycle has not been verified.

Contributors should preserve explicit `PARTIAL` and `BLOCKED` states whenever data, coverage, or source provenance is missing.
