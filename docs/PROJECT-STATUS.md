# Project status and known limitations

This repository is an early public alpha. The following boundaries are deliberate and should remain visible in documentation and user-facing output:

- Caido history and local capture indexes have been exercised in one development environment, but full compatibility across Caido versions and workspaces is not established.
- Caido does not currently provide a stable history snapshot token to this workflow. A completed metadata enumeration therefore cannot prove that the source was unchanged throughout the read; reconciliation reports `PARTIAL`.
- Some imported exchanges have incomplete parse or response-body coverage. HTTP status or endpoint presence alone is not a complete behavioral analysis.
- Browser walkthrough and JavaScript endpoint coverage are not yet part of a complete end-to-end application-map workflow.
- The knowledge-base snapshot is incomplete. Some external source families remain partial or blocked pending provider diagnostics or source-specific terms review. No compiled full corpus is included.
- Hermes is an experimental harness integration; it is not a supported, end-to-end hypothesis-analysis loop.
- Candidate hypotheses and graph relationships are not confirmed findings. Evidence validation and human review remain necessary.

Contributors should preserve explicit `PARTIAL` and `BLOCKED` states whenever data, coverage, or source provenance is missing.
