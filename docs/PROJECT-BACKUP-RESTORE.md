# Project backup and restore

`project-backup` creates a bounded archive for one registered local project.
`project-restore` validates and restores that archive under a new data root.
Both commands are offline: they do not contact Caido, Neo4j, the KB, or a model
provider. The current implementation requires Linux for the project's
maintenance lease and atomic no-replace restore publication.

## Create an archive

Get the project key from `./scripts/bb-assistant projects`, then choose an
archive path whose parent directory already exists and is outside the project
directory:

```sh
./scripts/bb-assistant project-backup \
  --project-key <registered-project-sha256> \
  --output /private/backup/juice-shop.tar \
  --max-bytes 1073741824 \
  --max-files 50000
```

The positive byte and file limits are required. The byte limit covers the sum
of uncompressed project files. Backup takes the project's exclusive
maintenance lease, records file hashes in a versioned manifest, writes a
temporary archive with mode `0600`, and publishes it without replacing an
existing path. The process lock file and global project registry are omitted.

To inspect the exact file list and byte total before writing an archive, run
`project-backup --dry-run` with the same project and limits. `--output` is
optional in dry-run mode; if supplied, its destination is validated and shown
but not created. The JSON plan lists included file paths and sizes and explains
that the live lock, external project registry, and directory entries are
excluded. Dry-run holds a shared read lease, does not create the lock file, and
does not write an archive.

```sh
./scripts/bb-assistant project-backup \
  --project-key <registered-project-sha256> \
  --max-bytes 1073741824 \
  --max-files 50000 \
  --dry-run
```

## Restore

Restore into a new private runtime root. The project identity is retained and
the local registry is rebuilt after all files have passed their integrity
checks:

```sh
./scripts/bb-assistant project-restore \
  --input /private/backup/juice-shop.tar \
  --data-root /private/restored-bb-data \
  --max-bytes 1073741824 \
  --max-files 50000
```

Restore refuses an already registered identity or an existing project
directory. It rejects symlinks, unsafe archive paths, duplicate or unexpected
members, integrity mismatches, and archives exceeding the supplied budgets.
The project directory is published atomically without replacement. The JSON
result includes a fresh `capability_summary`; that summary can remain
`partial` when local stores are incompatible or coverage is not reconciled.
If the read-only audit itself cannot run, the result marks the summary
`unavailable` and directs the operator to `project-audit`.

## Data handling and verification

The archive preserves project-local files, including encrypted vault files,
graph stores, receipt files, and analyst material. Treat the archive as
sensitive, keep it in a private directory, and do not publish it. The archive
is not encrypted as a whole; vault encryption remains unchanged. Restore sets
directories to `0700` and files to `0600`, and does not restore the process
maintenance lock.

The archive never includes `BB_ASSISTANT_VAULT_KEY`. Configure the same vault
key through the existing secret-management path on the restore host; without
it, encrypted captures remain stored but cannot be opened for evidence review.

The automated backup/restore verification uses synthetic projects only. After
restoring a real project, inspect the emitted capability summary and run
`./scripts/bb-assistant project-audit --project-key <sha256> --data-root
<restored-root>` before resuming service. Project backup does not by itself
prove Caido-history completeness, Neo4j parity, KB coverage, or Hermes provider
readiness.
