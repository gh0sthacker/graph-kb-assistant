# BB Graph Assistant

A local-first, human-led assistant for exploring captured application behavior, graph relationships, and evidence. It uses Neo4j for the analytical graph, SQLite for capture metadata and durable sync state, and keeps raw exchanges in the encrypted local vault.

## Run locally

Start Neo4j using the private configuration described in [deploy/neo4j](deploy/neo4j/README.md), then run:

```sh
./scripts/bb-assistant neo4j-check
./scripts/bb-assistant neo4j-backfill --project-key <registered-project-sha256>
./scripts/bb-assistant serve
```

Open `http://127.0.0.1:8765/`. To run the durable projection worker for selected registered projects, pass each project key explicitly with `--project-key <sha256>`. Without project keys, `serve` runs the web UI only. MCP clients launch the separate stdio server with `./scripts/bb-assistant-mcp`; both launchers read the same private runtime configuration. The managed server does not start Caido intake or call a model. Runtime data and credentials stay outside this repository.

In the graph inspector, **Focus on direct relationships** narrows the display to the selected node and its immediate neighbors from the already-loaded graph. **All nodes** restores that graph slice. Focusing changes only the view; it does not reread Caido or change graph coverage.

Caido history intake and graph coverage are measured independently. Missing captures, incomplete parsing, or absent source snapshot guarantees remain `PARTIAL`; graph relationships are evidence navigation, not vulnerability verdicts.

To inspect pending projection jobs that may already be materialized in both graph stores, use [`projection-reconcile`](docs/PROJECTION-RECONCILIATION.md). It defaults to a read-only dry run; `--apply` acknowledges only exact matches after full snapshot parity.

Add URL navigation to previously retained captures using the separate
[`graph-navigation-backfill-preview` and `graph-navigation-backfill-apply`](docs/NAVIGATION-BACKFILL.md)
commands. They preserve accepted projection jobs and require an exact candidate digest.

Create and restore bounded, private project archives with [`project-backup` and `project-restore`](docs/PROJECT-BACKUP-RESTORE.md). Restore emits a fresh local capability summary; it does not establish Caido-history completeness or Neo4j parity.

See [docs/SETUP.md](docs/SETUP.md), [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), and [docs/PROJECT-STATUS.md](docs/PROJECT-STATUS.md) for setup, data boundaries, and the current verified coverage. The checked local runtime has served the selected project's operation graph and exact evidence/comparison APIs through Neo4j; Hermes MCP discovery and a model-backed synthetic consultation/resume are also verified. Real capture content remains outside the model boundary pending a separate retention/data-boundary review, and full source/browser/JavaScript coverage is still partial.

## Development

```sh
go test ./...
go vet ./...
```
