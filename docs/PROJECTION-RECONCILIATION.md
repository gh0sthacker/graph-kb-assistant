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

## Inspect dead-letter jobs

Dead-letter jobs are excluded by default. Add `--include-dead-letter` to scan
them in the same dry-run report. This does not retry or change them. With
`--apply --include-dead-letter`, only jobs whose complete normalized payload
is an exact subset of both parity-matched snapshots can be acknowledged as
applied. Missing, conflicting, or invalid dead-letter jobs remain in
`dead_letter`; reconciliation never moves them back to the worker queue.

```sh
./scripts/bb-assistant projection-reconcile \
  --project-key <registered-project-sha256> \
  --include-dead-letter
```

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

## Explicitly replay selected dead letters

When reconciliation reports a conflict that should be repaired by the normal
projector, inspect and replay only the named revisions. The command validates
the stored normalized payload and SHA-256 before changing anything; it never
prints or reads encrypted HTTP bodies and does not contact Caido. Dry-run is
the default:

```sh
./scripts/bb-assistant projection-replay \
  --project-key <registered-project-sha256> \
  --revision-id <dead-letter-revision-sha256>
```

Apply is explicit and bounded to the listed revisions:

```sh
./scripts/bb-assistant projection-replay \
  --project-key <registered-project-sha256> \
  --revision-id <dead-letter-revision-sha256> \
  --apply
```

Apply holds the project's exclusive maintenance lease through requeue and
worker delivery. It refuses the operation if any unrelated pending or leased
job exists, requires all selected revisions to be valid dead letters with the
same attempt count, preserves the cumulative attempt count, and gives each
listed job exactly one additional attempt. A failed replay returns to
`dead_letter`; a successful replay becomes `applied`. Reconcile again after a
replay to confirm queue state and snapshot parity.
