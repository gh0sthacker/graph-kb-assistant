# Local Neo4j graph store

The graph projection uses Neo4j Community in its default `neo4j` database.
Capture metadata, sync checkpoints, encrypted exchanges, and the existing
BehaviorGraph rollback copy remain in the private assistant data root.

Create a private environment file outside this checkout with these variables. The application launcher reads the same file and selects Neo4j as the graph backend; it may also set `BB_ASSISTANT_DATA_HOME` to the existing private project-data root:

```text
NEO4J_AUTH=neo4j/<private-random-password>
BB_ASSISTANT_DATA_HOME=<private-data-root>/graph-kb-assistant
BB_ASSISTANT_GRAPH_BACKEND=neo4j
BB_ASSISTANT_NEO4J_URI=bolt://127.0.0.1:7687
BB_ASSISTANT_NEO4J_USERNAME=neo4j
BB_ASSISTANT_NEO4J_PASSWORD=<same-private-random-password>
BB_ASSISTANT_NEO4J_DATABASE=neo4j
BB_ASSISTANT_NEO4J_DATA_DIR=<private-data-root>/neo4j/data
```

Restrict the file and data directory to the local owner (`0600` and `0700`).
Do not put the file under the repository. The Compose file binds Bolt and the
browser UI to loopback only and mounts the data directory for persistence.

The variables above configure the application's BehaviorGraph database only.
If KB Lucene is enabled, keep its connection in a separate private file
`$BB_ASSISTANT_DATA_HOME/kb-neo4j.env` (or set
`BB_ASSISTANT_KB_NEO4J_ENV_FILE` to another private path):

```text
BB_ASSISTANT_KB_NEO4J_URI=bolt+s://<private-kb-host>:7687
BB_ASSISTANT_KB_NEO4J_USERNAME=<kb-only-user>
BB_ASSISTANT_KB_NEO4J_PASSWORD=<private-random-password>
BB_ASSISTANT_KB_NEO4J_DATABASE=neo4j
```

The KB endpoint must be an independently provisioned Neo4j instance; it must
not point back to this BehaviorGraph database. The launcher loads the optional
KB environment file and fails closed if `BB_ASSISTANT_KB_LEXICAL_ENGINE` is
`neo4j-lucene` but that file is absent or incomplete.

Start and inspect the local service:

```sh
docker compose --env-file "$NEO4J_ENV_FILE" -f deploy/neo4j/compose.yaml up -d
BB_ASSISTANT_NEO4J_ENV_FILE="$NEO4J_ENV_FILE" ./scripts/bb-assistant neo4j-check
```

The application does not print credentials. Backfill a selected project from
its validated SQLite BehaviorGraph and require a read-back digest match:

```sh
./scripts/bb-assistant neo4j-backfill --project-key <registered-project-sha256>
```

Backfill marks only the selected project's graph unavailable to readers,
deletes and writes it in bounded transactions, reads it back, and publishes the
new generation only after its digest matches. If interrupted, that project's
graph stays unavailable and the command can be rerun from the SQLite recovery
copy. Other project namespaces remain readable. This command does not update
Caido checkpoints or mark Caido history complete.

The Compose profile reserves a 2 GiB initial / 8 GiB maximum heap, 1 GiB
page cache, and up to 6 GiB for an individual or total transaction. These
limits support atomic replacement of a large project graph; they are ceilings,
not preallocated reservations. Allow roughly 12 GiB of host memory for Neo4j
and keep swap available during a large backfill. If the host has less memory,
reduce the transaction limits and heap together; a backfill that exceeds the
transaction cap rolls back and leaves the prior project snapshot intact.

Run the application through `scripts/bb-assistant` (or `scripts/bb-assistant-mcp`) so graph reads and new graph imports use Neo4j and the durable SQLite outbox. `neo4j-sync --project-key <registered-project-sha256>` retries pending outbox events. If Neo4j is unavailable, SQLite keeps the committed events for retry and sync remains `partial`.
