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

- The Go runtime opened the working capture and graph stores read-only and reported both schemas compatible. Caido enumeration reached its terminal cursor with the current local IDs matched; coverage remains `PARTIAL` because the source provides no stable snapshot token or expected total.
- The imported KB snapshot passed validation and SQLite integrity checks. Go search returned provenance hashes and spans from the Python-built FTS5 index; the local MCP `bb_kb_stats` tool reported the same `PARTIAL` coverage.
- Hermes discovered the eight configured read-only tools. Its model provider is not configured, so the model-backed analysis and hypothesis cycle has not been verified.

Contributors should preserve explicit `PARTIAL` and `BLOCKED` states whenever data, coverage, or source provenance is missing.
