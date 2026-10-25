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

Backfill replaces only that project's Neo4j graph snapshot in one transaction.
The current SQLite graph remains available as the recovery copy. This command
does not update Caido checkpoints or mark Caido history complete.

The Compose profile reserves a 2 GiB initial / 8 GiB maximum heap, 1 GiB
page cache, and up to 6 GiB for an individual or total transaction. These
limits support atomic replacement of a large project graph; they are ceilings,
not preallocated reservations. Allow roughly 12 GiB of host memory for Neo4j
and keep swap available during a large backfill. If the host has less memory,
reduce the transaction limits and heap together; a backfill that exceeds the
transaction cap rolls back and leaves the prior project snapshot intact.

Run the application through `scripts/bb-assistant` (or `scripts/bb-assistant-mcp`) so graph reads and new graph imports use Neo4j and the durable SQLite outbox. `neo4j-sync --project-key <registered-project-sha256>` retries pending outbox events. If Neo4j is unavailable, SQLite keeps the committed events for retry and sync remains `partial`.
