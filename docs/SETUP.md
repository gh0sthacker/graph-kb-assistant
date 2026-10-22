# Local setup

## Requirements

- Go 1.27 or newer for the primary application.
- Python 3.12 or newer for knowledge-base tooling and compatibility workflows.
- Caido and its MCP integration only if you want to import or reconcile captured history.

## Run the graph explorer

From the repository root:

```sh
go run ./cmd/bb-assistant web-ui
```

Open `http://127.0.0.1:8765/`. The interface starts with an empty project list until local project data is registered. Use `--addr` only with a loopback host. Set `BB_ASSISTANT_DATA_HOME` to choose the private runtime-data directory; by default the application uses a user data directory outside the checkout.

## Connect an assistant over MCP

The project exposes a stdio MCP server. The included launcher is:

```sh
./scripts/bb-assistant-mcp
```

Configure your MCP client to run that executable from the repository checkout. Keep the data directory and any environment variables private. The default server exposes read-only local project, search, graph, and knowledge-base tools. An opt-in raw-capture tool can return sensitive request or response bytes; keep it disabled unless you need it and understand the data exposure.

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
