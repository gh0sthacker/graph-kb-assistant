# Go runtime cutover and recovery

**Status:** staged, not an assertion that Caido intake is deployed. The Go web
application is active on CT108; the Go Caido sync unit and its private
configuration were absent at the 2026-10-27 read-only check. The old Python
tail is disabled. Recheck live state before using this runbook.

This is the canonical operator path for CT108. The older installation examples
in `SETUP.md` and `MVP-RUNBOOK.md` describe earlier layouts and must not be used
to create a second production service. Python KB tooling remains a separate
maintenance path; production HTTP intake, graph projection and web/MCP serving
belong to the Go application.

## Runtime contract

- Service account: `max`.
- Checkout: `/home/max/bb/graph-kb-assistant`.
- Binary: `/usr/local/bin/bb-assistant`.
- Private runtime root: `/home/max/.local/share/bb-graph-assistant/graph-kb-assistant`.
- Neo4j credentials and graph backend: private `neo4j.env`, mode `0600`.
- Caido token and sync settings: private `caido-sync.env`, token file under
  `<runtime-root>/caido-mcp/`, both mode `0600`.
- The web service stays on loopback. Nginx remains the authenticated remote
  access boundary; do not expose the application port directly.
- Never enable a sync unit until its exact `scheme://host:port` scopes have
  been reviewed against the Caido project scopes and authorized lab/target
  origins. Host-only allowlists are insufficient to construct those origins.

## Preflight

1. Identify the successful CI artifact for the exact commit being deployed.
   Verify its checksum and `bb-assistant version`; do not build from a dirty
   checkout on CT108.
2. Read, but do not print, the current unit definitions, service account,
   private environment-file paths, data root, selected project keys, and
   Caido MCP executable checksum. Confirm the old Python tail is not active.
3. Verify schema compatibility and current state with `bb-assistant status`,
   `projects`, and `project-audit`. Record pending projection jobs, dead
   letters, cursor state, and the age of the oldest pending job.
4. Ensure a recent private project backup exists. Backup and restore are
   separate from Caido reconciliation; a backup does not establish source
   completeness or Neo4j parity.
5. Create the private sync environment from
   `systemd/graph-kb-assistant-caido-sync.env.example`. Replace the placeholder
   origin; include each reviewed origin as a separate comma-separated entry in
   `BB_ASSISTANT_CAIDO_SCOPE_ORIGINS`. Do not put credentials or target data in
   Git. The service fails closed if the list is empty or malformed.
6. Verify the unit offline with `systemd-analyze verify`. Confirm its user,
   working directory, executable, data root and `ReadWritePaths` match the
   active web service before installation.

Do not infer a complete Caido snapshot from an empty queue, a terminal cursor,
or matching counts. Caido does not provide the stable snapshot watermark
needed to prove there were no concurrent changes during enumeration. Report
history state as eventual/reconciled-at-checkpoint, never atomic or permanently
complete.

## Bounded first sync and cutover

Keep the recurring service disabled during the first check. Install the
verified binary atomically while retaining the immediately previous binary.
Install/reload the reviewed unit, then run one bounded inventory/backfill
cycle under the same service account and private environment. The CLI's
`--once` mode performs one inventory and exits nonzero on any inventory or
project-sync failure; it does not sleep and retry indefinitely. For the first
smoke, use one item per page and one backfill page, then inspect receipts before
increasing throughput:

```sh
sudo -u max bash -c '
  set -a
  . /home/max/.local/share/bb-graph-assistant/neo4j.env
  . /home/max/.local/share/bb-graph-assistant/caido-sync.env
  set +a
  exec /home/max/bb/graph-kb-assistant/scripts/bb-assistant caido-sync \
    --data-root "$BB_ASSISTANT_DATA_HOME" \
    --instance-id "$BB_ASSISTANT_CAIDO_INSTANCE_ID" \
    --mcp-command "$BB_ASSISTANT_CAIDO_MCP_COMMAND" \
    --mcp-sha256 "$BB_ASSISTANT_CAIDO_MCP_SHA256" \
    --token-file "$BB_ASSISTANT_CAIDO_TOKEN_FILE" \
    --backfill-page-size 1 --backfill-body-limit 65536 --backfill-pages 1 \
    --page-size 1 --tail-pages 1 --body-limit 65536 --once
'
```

Do not paste token contents into this command. The sync process reads the token
from its protected file. The interactive check is read-only against Caido but
does persist accepted captures, cursors and projection work in the selected
local project; use it only after confirming the project and exact scopes.

Confirm:

- the expected project inventory is seen without changing Caido's selected
  project;
- each origin has an independent checkpoint and reports page/request counts;
- truncated, absent or text-reconstructed message sides remain explicitly
  partial/unknown and are not described as exact source bytes;
- committed captures have durable SQLite projection jobs;
- Neo4j projection succeeds or the jobs remain visible and retryable;
- the service reports partial coverage when any origin, page, body or graph
  projection is incomplete.

Only after those checks should the operator enable
`graph-kb-assistant-caido-sync.service`. The unit is bounded per poll and
resource-limited; watch queue age, failures and CT resource use after enabling.
Do not run the legacy Python tail at the same time. Keep its unit disabled
until a Go sync acceptance record shows stable cursor advancement and replay
after service restart.

The Go sync manager currently polls; it is not an atomic push subscription.
Caido project selection can change between MCP calls because the read API does
not bind every call to an immutable project handle. The worker aborts on
observed selection changes, but cannot eliminate the race. Raw-byte access for
all projects via the official SDK is still an implementation/evidence gap.

## Restart, replay and rollback

- For a service restart, retain the SQLite checkpoints and durable projection
  jobs. Restarting must resume from the saved cursor and deduplicate already
  accepted request IDs; do not reset the cursor to force recovery.
- If Neo4j is unavailable, keep accepted source captures and pending projection
  jobs. Restore Neo4j from the SQLite projection/rebuild path; do not discard
  the intake ledger or dead-letter history.
- For a binary rollback, stop Go sync first, retain the new binary and logs for
  diagnosis, restore the previous binary atomically, then run `version`,
  `status`, and `project-audit`. Do not automatically re-enable Python tail.
  If schema migrations have advanced beyond the old binary's reader contract,
  rollback the code only after restoring a compatible backup or using a
  verified forward-fix; never point an old binary at an unknown newer schema.
- For data restoration, use `project-backup` and `project-restore` into a new
  private root, restore the separately managed vault key, run `project-audit`,
  and compare receipt references/capabilities before switching the service.
  Do not overwrite the live root in place.

## Acceptance evidence still required

This runbook becomes a verified production cutover only after a dated record
shows an installed Go sync unit, exact private origin configuration, bounded
live inventory/backfill/tail, restart/replay behavior, projection drain or
visible retry state, backup/restore on a working project, and current
capability status. Even then, Caido's missing snapshot watermark means global
atomic completeness remains unprovable; preserve that limitation in the UI.
