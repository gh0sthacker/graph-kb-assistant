# BB Graph Assistant

A local-first, human-led assistant for exploring captured application behavior, graph relationships, and evidence. It uses Neo4j for the analytical graph, SQLite for capture metadata and durable sync state, and keeps raw exchanges in the encrypted local vault.

## Run locally

Start Neo4j using the private configuration described in [deploy/neo4j](deploy/neo4j/README.md), then run:

```sh
./scripts/bb-assistant neo4j-check
./scripts/bb-assistant neo4j-backfill --project-key <registered-project-sha256>
./scripts/bb-assistant web-ui
```

Open `http://127.0.0.1:8765/`. Use `./scripts/bb-assistant-mcp` to launch the read-only MCP server. Runtime data and credentials stay outside this repository.

Caido history intake and graph coverage are measured independently. Missing captures, incomplete parsing, or absent source snapshot guarantees remain `PARTIAL`; graph relationships are evidence navigation, not vulnerability verdicts.

See [docs/SETUP.md](docs/SETUP.md), [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), and [docs/PROJECT-STATUS.md](docs/PROJECT-STATUS.md) for setup, data boundaries, and verified coverage.

## Development

```sh
go test ./...
go vet ./...
```
