# Local setup

## Requirements

- Go 1.27 or newer for the primary application.
- Python 3.12 or newer for knowledge-base tooling and compatibility workflows.
- Caido and its MCP integration only if you want to import or reconcile captured history.

## Run the graph explorer

From the repository root:

```sh
./scripts/bb-assistant web-ui
```

Open `http://127.0.0.1:8765/`. The interface starts with an empty project list until local project data is registered. Use `--addr` only with a loopback host. Set `BB_ASSISTANT_DATA_HOME` to choose the private runtime-data directory; by default the application uses a user data directory outside the checkout.

The same launcher provides the current local runtime summary:

```sh
./scripts/bb-assistant status
```

`status` can run before Neo4j credentials are configured. It reports project
schema/coverage and projection-queue counts without reading capture bodies or
contacting Caido, Neo4j, or a model provider. Its overall state remains
`partial` until source reconciliation and the other coverage gaps are verified.
See [CURRENT-CAPABILITIES.md](CURRENT-CAPABILITIES.md) for what the report
measures and how to read its limits.

## Connect an assistant over MCP

The project exposes a stdio MCP server. The included launcher is:

```sh
./scripts/bb-assistant-mcp
```

Configure your MCP client to run that executable from the repository checkout. Keep the data directory and any environment variables private. The default server exposes read-only local project, search, graph, and knowledge-base tools. An opt-in raw-capture tool can return sensitive request or response bytes; keep it disabled unless you need it and understand the data exposure.

The launcher reads the private Neo4j settings from `$XDG_DATA_HOME/bb-graph-assistant/neo4j.env` (or `~/.local/share/bb-graph-assistant/neo4j.env`) and selects Neo4j for graph reads. Set `BB_ASSISTANT_NEO4J_ENV_FILE` to use another private file. Operational commands require that file when Neo4j is selected. `status` and `version` work without it; explicit SQLite compatibility mode also does not require it. The same launcher runs other commands, for example `./scripts/bb-assistant neo4j-check`.

## Use Hermes Agent

Hermes is the supported runtime harness for the local assistant. Install it and check the configured MCP server with the project launchers:

```sh
./scripts/install-hermes
./scripts/run-hermes mcp test bb_assistant
./scripts/run-hermes
```

The generated profile is stored outside the checkout with private file permissions. It enables only the local MCP server, skills, and clarification tools, and allowlists the nine read-only `bb_*` tools. Running `setup-hermes` or `run-hermes` adds the graph neighborhood tool to an existing managed profile while preserving its other settings; unmanaged profiles are left untouched. MCP resources and prompts are disabled; terminal, file, browser, web, computer-use, and code-execution toolsets are not enabled for this profile. `mcp test` should discover exactly nine tools, including no raw-capture body tool.

The harness and model provider are separate choices. Hermes can verify MCP connectivity before a provider is selected; configure one with `./scripts/run-hermes model` when ready. Review that provider's data handling before sending real capture content. The current workflow has not verified model-backed hypothesis generation or validation.

## Connect Caido

Caido operations use the Caido MCP interface. Select one Caido project and pass its identity explicitly to the CLI. For history import and reconciliation, provide the verified Caido MCP executable, its expected SHA-256, and the private token file using the command flags shown by the relevant CLI subcommand. Never store the token file or its contents in this repository.

Use a dedicated private local data directory. Historical import stores capture bytes in the encrypted vault; preserve the generated vault key outside Git and back it up securely. A reconciliation without a stable source snapshot token remains `PARTIAL`, even if the enumerated IDs match.

## Knowledge-base tools

Install the Python extras and run the CLI:

```sh
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -e '.[kb,mcp]'
python -m bb_assistant.kb.cli --help
```

Source feeds are pinned and license-gated. A source listed in configuration is not proof that a complete corpus has been downloaded or that all terms have been cleared. Snapshots and indexes are runtime data and should remain outside the source checkout.
