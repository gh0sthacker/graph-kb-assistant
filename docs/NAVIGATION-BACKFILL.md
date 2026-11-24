# Historical URL navigation backfill

New imports include project-keyed URL references. Previously accepted capture
projections remain immutable. Use this separate versioned operation to add URL
navigation to captures already retained in the selected project's encrypted vault.
It does not contact Caido, a model, or Neo4j.

## Preview and apply

The same private vault key used for intake must be available to the launcher.
Select exactly one registered project:

```sh
./scripts/bb-assistant graph-navigation-backfill-preview \
  --project-key <project-sha256> --data-root <private-data-root>

./scripts/bb-assistant graph-navigation-backfill-apply \
  --project-key <project-sha256> --data-root <private-data-root> \
  --candidate-sha256 <candidate-sha256-from-preview>
```

Preview authenticates locally retained request and response messages and pins
each candidate to its current capture revision and graph observation. It reports
counts and a candidate digest without serializing graph bundles or message bytes.
The digest covers the operation version, project, revision anchors and candidate
content. Resource limits do not change the digest when they select the same
complete candidate set.

Both commands hold the selected project's exclusive maintenance lease. Apply
regenerates and authenticates the complete candidate before committing. Stale,
modified, partial or differently scoped candidates are rejected. Errors report
an unavailable state; incomplete coverage reports `partial`.

Successful apply adds local graph bundles and a durable Neo4j outbox entry per
bundle. It leaves accepted projection jobs and capture revisions unchanged.
`neo4j_drain=not_attempted` means delivery to Neo4j still belongs to the regular
projection worker. Repeating the same accepted candidate reuses its bundle IDs
and does not enqueue duplicates. If a multi-bundle apply stops partway through,
already committed bundles remain valid and the same candidate can be retried.

## Limits and interpretation

- Defaults: 5,000 captures and 512 MiB combined decrypted request/response bytes.
  `--max-captures` allows 1–20,000; `--max-bytes` allows 1 MiB–1 GiB.
- Each individual message is limited to 1 MiB before decryption. Missing graph
  mappings, oversized messages, invalid targets, status conflicts, or an
  exhausted limit leave the preview partial and cannot be silently applied.
- A capture whose response was absent can still contribute its request URL and
  Referer. It supplies no redirect evidence.
- Exact URL equality connects every matching capture, including duplicates.
  These links do not establish an initiator, chronology, ownership or causality.
- Raw URLs are used in memory for project-keyed HMAC calculation, then message
  buffers are cleared. The additional graph records contain no raw URL values.
- Only current retained revisions are candidates. Superseded historical
  revisions and live-only captures without vault messages are not enriched by
  this operation.

Synthetic checks cover legacy graph enrichment, duplicate targets, stale
mapping rejection, candidate confirmation, bounded partial previews, preservation
of accepted job hashes, replay, safe CLI output, and maintenance locking. They
do not establish delivery to a running Neo4j instance or working-data readiness.
