# Projection queue reconciliation

`projection-reconcile` checks whether pending durable projection jobs are
already represented by the current SQLite BehaviorGraph and Neo4j snapshot.
It does not contact Caido, inspect encrypted request/response bodies, or write
graph records.

## Dry run

```sh
./scripts/bb-assistant projection-reconcile \
  --project-key <registered-project-sha256>
```

The command acquires the selected project's exclusive maintenance lock, reads
both full graph snapshots, requires their snapshot digests to match, validates
each bounded queued payload, and compares every source, entity, assertion and
card with the current local snapshot. It reports exact matches, missing or
conflicting records, invalid jobs, queue size, and the snapshot digests. The
default limits are 5,000 jobs and 512 MiB of normalized payload bytes.

## Apply exact matches

```sh
./scripts/bb-assistant projection-reconcile \
  --project-key <registered-project-sha256> \
  --apply
```

`--apply` changes only jobs whose entire normalized payload is an exact subset
of the parity-verified snapshots. It writes revision-to-observation mappings
and marks those jobs applied in bounded SQLite transactions. A mismatched
snapshot stops before scanning or changing jobs. Missing, conflicting, or
invalid jobs remain pending for ordinary worker handling. An exact match may
also clear a stale lease as part of acknowledgement; the exclusive project
lock prevents the normal projection worker from running concurrently. The
queue is reported `partial` while any pending jobs remain.

Scans can be limited with `--max-jobs` and `--max-bytes`. If the output reports
`truncated: true`, resume after `next_sequence_id`:

```sh
./scripts/bb-assistant projection-reconcile \
  --project-key <registered-project-sha256> \
  --after-sequence-id <next_sequence_id>
```

The project must already pass Go schema compatibility checks, and the Neo4j
backend must be available. Reconciliation establishes the presence of graph
projection records at the verified snapshot; it does not prove that Caido's
history was completely imported, nor does it change browser, JavaScript, or
analysis coverage.
