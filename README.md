# BB Graph Assistant

[![CI](https://github.com/gh0sthacker/graph-kb-assistant/actions/workflows/ci.yml/badge.svg)](https://github.com/gh0sthacker/graph-kb-assistant/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
`Go 1.27+`

BB Graph Assistant is a local-first analysis companion for captured application behavior. It reads an explicitly selected Caido project through Caido's MCP interface, writes a project-scoped behavior graph to Neo4j while keeping capture metadata, sync checkpoints and the projection outbox in SQLite, and keeps historical request/response bytes in an encrypted local vault. A loopback web explorer, a standalone search service and a bounded MCP tool catalog let an analyst follow endpoint, parameter, variant and actor relationships, open the exact evidence behind a node, ground a hypothesis in pinned knowledge sources, and record a candidate with alternatives and missing evidence. The graph is a navigation layer over observed behavior: it is not a vulnerability verdict, and the analysis workflow does not send requests to the target.

> **Project status: alpha.** This is an early source snapshot. Coverage is reported as `partial` wherever the local index cannot prove a complete Caido history snapshot or browser/JavaScript coverage, and the supported workflow is report-only. Use it on projects you are authorized to test, and expect interfaces to change between alpha releases.

## What it does

- **Project-scoped capture graph.** Caido intake registers each project under a SHA-256 project key derived from the configured instance key and Caido project ID. Every graph node and relationship carries that `project_key`, and queries and writes stay inside the namespace. There is no implicit multi-project search.
- **Read-only web graph explorer.** `serve` binds to `127.0.0.1:8765` and rejects wildcard or non-loopback listeners. The explorer returns bounded graph slices with match basis, parse completeness and coverage alongside each result, exposes linked evidence ranges and request/response comparisons, and offers a *Focus on direct relationships* view that narrows the already-loaded slice without rereading Caido (`internal/webui`).
- **Standalone search service.** `cmd/kb-search-api` is a small HTTP service that only opens its SQLite indexes read-only, so it can be cross-built for hosts without a Go toolchain. It serves `GET /api/kb/search` (`query` required, 1–4096 characters; `top_k` 1–20, default 10; repeatable `source` filters, at most 20) and `GET /api/health`; anything other than `GET` or `HEAD` is rejected. Listen address comes from `--addr` or `KB_SEARCH_ADDR` (default `127.0.0.1:18766`), the primary index from `BB_ASSISTANT_KB_INDEX`, and the optional disclosed-report index from `BB_ASSISTANT_HACKERONE_INDEX`. A missing or incompatible HackerOne index is logged and disabled instead of failing the service.
- **Knowledge base with pinned, license-checked sources.** `src/bb_assistant/kb/sources.yaml` records a revision, license identifier, license evidence URL and license status for each source family: the vendored ToolDocs snapshot, GTFOBins, LOLBAS, OWASP WSTG, ExploitDB descriptions and metadata only, NVD, and nuclei templates. Two profiles are defined (`lite`: ToolDocs, GTFOBins, LOLBAS; `full`: all seven). Retrieval is lexical over SQLite FTS5 by default, with an optional Neo4j Lucene index for a separately provisioned KB endpoint, and optional vector search (`sqlite-cosine` or FAISS, installed through the `kb-ml` extra). Every hit carries its source name, locator, revision, license and attribution, and is flagged as untrusted reference material.
- **Bounded MCP server.** `./scripts/bb-assistant-mcp` launches the stdio MCP server over the same runtime configuration as the CLI. Its catalog covers project listing, capture search, surface map, bounded graph slices and neighborhoods, coverage, KB stats/search/source reads, and redacted capture and stored-message reads, plus the consultation tools described below. The managed `serve` process also exposes the same catalog at `/mcp` over Streamable HTTP for deployments that put it behind the authenticated reverse proxy described in [docs/SETUP.md](docs/SETUP.md).
- **Consultation packets gated by explicit local acceptance.** A consultation session is created from the web UI as a bounded, redacted packet. Until the operator inspects it and accepts it in the local UI, MCP session JSON reports `content_withheld: true` with the hidden field paths listed, and comparison, proposal, validation and candidate operations are rejected. Acceptance is a local-only transition, not an MCP tool; after it, an assistant client can resume the session and see the saved redacted packet (`internal/consultation`, `internal/webui`).
- **Encrypted vault for raw exchanges.** Historical message bytes are encrypted with AES-256-GCM before they are written to the vault, with the additional authenticated data binding each object to its observation ID and side. The key comes from `BB_ASSISTANT_VAULT_KEY` and is never persisted by the application, so it has to be backed up separately. Raw bodies stay out of the repository, the graph and the sync receipts, which carry hashes, byte counts, truncation state and wire-fidelity labels only. Live tail parses messages in memory and retains only redacted structure and keyed signatures.

## How it fits together

```text
 capture pipeline

   Caido project --MCP API--> ingest / adapter --+--> encrypted vault
                                                 |    (AES-256-GCM, raw exchanges)
                                                 |
                                                 +--> SQLite capture store
                                                      (metadata, sync checkpoints,
                                                       graph copy, outbox)
                                                            |
                                                            v
                                                      projection worker
                                                            |
                                                            v
                                                      Neo4j behavior graph
                                                      (project_key namespace)

 read-only surfaces

   Neo4j behavior graph --+--> web UI           http://127.0.0.1:8765
                          +--> MCP server        stdio (scripts/bb-assistant-mcp), /mcp
   KB index ----------------+--> kb-search-api   http://127.0.0.1:18766
   (pinned sources)        |
                           +--> local assistant client (web UI, HTTP search, or MCP)
```

A graph import commits normalized records and an outbox event together in SQLite, then applies the event idempotently to Neo4j; if Neo4j is unavailable the event stays retryable and graph sync reports `partial`. See [docs/DATA-MODEL.md](docs/DATA-MODEL.md) and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full data flow and boundary rules.

## Quick start

Requirements: Go 1.27 or newer. The launcher runs the binary from source with `go run` when no prebuilt binary is configured through `BB_ASSISTANT_BINARY`.

```sh
# 1. Optional Neo4j backend (the default graph backend)
export NEO4J_ENV_FILE=/path/outside/the/checkout/neo4j.env   # see deploy/neo4j/README.md
docker compose --env-file "$NEO4J_ENV_FILE" -f deploy/neo4j/compose.yaml up -d
BB_ASSISTANT_NEO4J_ENV_FILE="$NEO4J_ENV_FILE" ./scripts/bb-assistant neo4j-check

# 2. Backfill one registered project. Without --ensure-schema, neo4j-check is read-only.
BB_ASSISTANT_NEO4J_ENV_FILE="$NEO4J_ENV_FILE" \
  ./scripts/bb-assistant neo4j-backfill --project-key <registered-project-sha256>

# 3. Serve the web UI. Add --project-key to also run the projection worker.
./scripts/bb-assistant serve
```

Open `http://127.0.0.1:8765/`. `serve` accepts `--project-key` (repeatable, up to 16 projects), `--data-root`, `--kb-index`, `--addr`, `--poll-interval`, `--lease-duration`, `--max-jobs` and `--max-attempts`; without project keys it runs the web UI only. The Neo4j environment file is required unless `BB_ASSISTANT_GRAPH_BACKEND=sqlite` is set for the process.

The `deploy/neo4j/compose.yaml` service binds Bolt and the browser UI to loopback only and mounts a private data directory; `deploy/neo4j/README.md` lists the environment variables the launcher reads.

MCP clients launch the stdio server instead:

```sh
./scripts/bb-assistant-mcp
```

The Python compatibility path builds and queries the same knowledge base:

```sh
pip install -e ".[kb]"
./scripts/bb-assistant-kb --index-path /path/to/kb/index.sqlite3 build
./scripts/bb-assistant-kb --index-path /path/to/kb/index.sqlite3 search "request smuggling"
```

`build`, `stats`, `rechunk`, `import`, `embed`, `search`, `models`, `publish-lucene`, `parity-check` and `parity-export` are the subcommands. Vector retrieval needs the `kb-ml` extra (`sentence-transformers`, `faiss-cpu`); `scripts/setup-kb-models` prepares the pinned retrieval models.

## Security and privacy posture

- **Loopback only.** The web UI validates its listen address and refuses wildcard or non-loopback hosts; the Neo4j compose file publishes `127.0.0.1:7474` and `127.0.0.1:7687` only. Remote browser or MCP access is a deliberate reverse-proxy deployment, documented in [docs/SETUP.md](docs/SETUP.md), not a bind-address change.
- **Data outside the checkout.** Runtime roots resolve from `BB_ASSISTANT_DATA_HOME`, then `$XDG_DATA_HOME/bb-graph-assistant`, then `~/.local/share/bb-graph-assistant`, and the store enforces the boundary when it opens a project database. Credentials, vault keys and capture data belong in that private root, not in the repository.
- **No capture data leaves the machine.** There is no hosted-service or telemetry component. Egress is the Caido instance you configure, the optional `gate` readiness probes against an explicitly authorized target origin, and, when you build the knowledge base, the pinned upstream feeds named in `sources.yaml`.
- **Consultation gating.** A packet's question and excerpts stay hidden from assistant clients until an operator accepts the session locally. Raw captured messages are never copied into the consultation database or MCP results, and redaction is heuristic, so each question and excerpt is meant to be inspected before acceptance.
- **Explicit project selection.** Import and reconciliation operations require named projects; the MCP facade exposes only projects present in the local registry.
- **Secrets and findings.** CI runs Gitleaks over the checkout; see [docs/SECRET-SCANNING.md](docs/SECRET-SCANNING.md) and [SECURITY.md](SECURITY.md) for reporting a vulnerability with synthetic data only.

## Repository layout

```text
cmd/bb-assistant/        CLI, web UI host, projection worker, MCP stdio entry
cmd/kb-search-api/       standalone read-only KB search service
internal/                caido, ingest, graph, neo4jstore, store, webui,
                         mcpserver, consultation, kb, hackerone, gate, coverage
src/bb_assistant/        Python KB tooling, CLI and compatibility path
scripts/                 launchers plus KB build, model and parity scripts
deploy/                  Neo4j compose, systemd units, nginx vhost, env templates
docs/                    architecture, setup, API, data model, operating notes
references/              vendored RedAmon reference documents and parity fixtures
tests/                   Python test suite
```

## Documentation

| Document | Contents |
| --- | --- |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | Data flow, evidence boundary, graph storage and known limits |
| [API.md](docs/API.md) | Local HTTP API, including `/api/kb/search` parameters and responses |
| [SETUP.md](docs/SETUP.md) | Local setup, systemd deployment, reverse proxy and MCP transports |
| [CONSULTATION.md](docs/CONSULTATION.md) | Review boundary, session state, evidence comparison, candidates |
| [DATA-MODEL.md](docs/DATA-MODEL.md) | Project isolation, vault encryption, variants, privacy boundary |
| [CURRENT-CAPABILITIES.md](docs/CURRENT-CAPABILITIES.md) | What the status command reports and which runtime items are unverified |
| [GRAPH-GUIDED-ANALYSIS.md](docs/GRAPH-GUIDED-ANALYSIS.md) | The analysis loop from project selection to a recorded candidate |
| [CONTINUOUS-CAIDO-SYNC.md](docs/CONTINUOUS-CAIDO-SYNC.md) | Design and current state of the continuous Caido sync manager |
| [CAIDO_PROJECT_SYNC.md](docs/CAIDO_PROJECT_SYNC.md) | Project inventory sync, checkpoints and removal semantics |
| [KB-PARITY.md](docs/KB-PARITY.md) | Offline parity gate against the pinned reference corpus |
| [PROJECT-BACKUP-RESTORE.md](docs/PROJECT-BACKUP-RESTORE.md) | Offline bounded backup and restore of one project |
| [SECRET-SCANNING.md](docs/SECRET-SCANNING.md) | CI secret scanning and the exception policy |

## Third-party notices

The reference documents under `references/redamon/` are from [samugit83/redamon](https://github.com/samugit83/redamon), pinned at commit `cf1b9e9` and retained under their upstream MIT license. See [NOTICE.md](NOTICE.md) for the manifest and attribution details; Go and Python dependencies keep their own licenses.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development workflow, [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for community expectations, [ROADMAP.md](ROADMAP.md) for planned work, and [SECURITY.md](SECURITY.md) for vulnerability reports.

## License

MIT — see [LICENSE](LICENSE).
