# Current capability summary

This document describes the live local status command, not a claim that every
runtime integration has been exercised. Run it from the project checkout for a
fresh, machine-specific report:

```sh
./scripts/bb-assistant status
```

The command returns JSON with a generation timestamp, graph-backend
configuration state, each registered project's capture and BehaviorGraph
schema state, local coverage counts, history reconciliation state, and
projection-queue counts. It does not include project display names, Caido IDs,
connection endpoints, credentials, capture payloads, or encrypted vault data.

`overall_state` stays `partial` because the local index cannot establish a
complete Caido history snapshot or browser/JavaScript coverage. A configured
Neo4j connection is not the same as a verified connection: the command checks
configuration syntax but does not connect. Caido intake and model execution
are reported as unverified/not invoked. The report is a read-only point-in-time
snapshot; it does not migrate stores or drain projection jobs.

## Implemented in the Go application

- Loopback web interface, stdio MCP adapter, project-scoped graph and evidence
  queries, and structural local project audits.
- Durable local projection jobs with bounded retry, dead-letter states, and a
  separate worker. Queue counts are visible in `status`; draining remains an
  explicit worker operation.
- A reviewed consultation workflow with session persistence, evidence
  comparison, citation checks, candidate versioning, and resume. Its synthetic
  workflow is verified; model-backed use is not.
- Hermes profile setup and MCP tool discovery. Discovery does not invoke a
  model or send captured content.

## Runtime items still unverified

- Caido project access, new live intake, and restart/replay against the current
  runtime databases.
- Neo4j connectivity and projection-queue drain on the selected working data.
- Backup/restore preserving capture revisions, encrypted vault objects,
  BehaviorGraph receipts, consultation state, and required key references.
- Full KB integrity, authenticated actor/object analysis, model-backed
  evaluation, and browser/JavaScript application-map coverage.

Use [PROJECT-STATUS.md](PROJECT-STATUS.md) for dated implementation evidence
and known gaps. Older checkpoints in that file describe their own state and
must not be read as current runtime measurements.
