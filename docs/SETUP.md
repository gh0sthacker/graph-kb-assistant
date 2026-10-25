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

The launcher also loads the optional private application file
`$BB_ASSISTANT_DATA_HOME/runtime.env`; by default this is
`$XDG_DATA_HOME/bb-graph-assistant/runtime.env` (or the
equivalent path under `~/.local/share`). Set `BB_ASSISTANT_RUNTIME_ENV_FILE` to
choose another file. Keep it owner-only (`0600`); it can contain the vault key
used by explicit local evidence-view actions. Values supplied directly in the
process environment take precedence. The key stays in the local Go process:
normal graph reads do not load message bytes, and evidence is not sent to
Hermes automatically.

## Use Hermes Agent

Hermes is the supported runtime harness for the local assistant. Install it and check the configured MCP server with the project launchers:

```sh
./scripts/install-hermes
./scripts/run-hermes mcp test bb_assistant
./scripts/run-hermes
```

The generated profile is stored outside the checkout with private file permissions. It enables only the local MCP server, skills, and clarification tools, and allowlists 15 named `bb_*` tools for local graph/KB inspection and the reviewed consultation workflow. Running `setup-hermes` or `run-hermes` reconciles the managed tool list to those 15 names: missing approved tools are added and stale names are removed, while the rest of the Hermes profile is preserved. Unmanaged profiles are left untouched. MCP resources and prompts are disabled; terminal, file, browser, web, computer-use, and code-execution toolsets are not enabled for this profile. `mcp test` should discover exactly the 15 allowlisted tools, including no raw-capture body tool. This checks tool discovery only; model-backed analysis still requires a separately selected and reviewed provider.

The harness and model provider are separate choices. Hermes can verify MCP connectivity before a provider is selected; configure one with `./scripts/run-hermes model` when ready. Review that provider's data handling before sending real capture content. The current workflow has not verified model-backed hypothesis generation or validation.

## Connect Caido

Caido operations use the Caido MCP interface. Select one Caido project and pass its identity explicitly to the CLI. For history import and reconciliation, provide the verified Caido MCP executable, its expected SHA-256, and the private token file using the command flags shown by the relevant CLI subcommand. Never store the token file or its contents in this repository.

Use a dedicated private local data directory. Historical import stores capture bytes in the encrypted vault; preserve the generated vault key outside Git and back it up securely. A reconciliation without a stable source snapshot token remains `PARTIAL`, even if the enumerated IDs match.

Run the Go live tail for one explicitly selected and authorized Caido project
from the repository checkout:

```sh
./scripts/bb-assistant caido-tail \
  --project-key <registered-project-sha256> \
  --mcp-command <verified-caido-mcp-executable> \
  --mcp-sha256 <verified-executable-sha256> \
  --token-file <private-caido-token-file> \
  --page-size 50 --max-pages 1 --watch --poll-interval 10s \
  --overlap-seconds 90 --body-limit 262144
```

Live intake imports every request in that Caido project during the overlap
window. Keep the project restricted to traffic authorized for collection. The
command persists its cursor and deduplicates previously indexed request IDs;
it does not declare historical capture coverage complete.

## Managed Go runtime (Linux/systemd)

The checked-in units in [`systemd/`](../systemd/) run the Go web UI, bounded
Caido live intake, and the durable projection worker. They are templates; this
repository does not install or start them. The live-tail unit reads every
request in its selected Caido project, so enable it only for a dedicated
project whose traffic is authorized for collection.

The examples assume a `bbassist` system account, an installation at
`/opt/bb-graph-assistant`, and private state at `/var/lib/bb-graph-assistant`.
Create the account if it does not already exist, then build and install the
executable and units:

```sh
if ! id bbassist >/dev/null 2>&1; then
  sudo useradd --system --user-group --home-dir /var/lib/bb-graph-assistant --create-home --shell /usr/sbin/nologin bbassist
fi
mkdir -p bin
go build -o bin/bb-assistant ./cmd/bb-assistant
sudo install -D -o root -g root -m 0755 bin/bb-assistant /opt/bb-graph-assistant/bin/bb-assistant
sudo install -d -o bbassist -g bbassist -m 0700 /var/lib/bb-graph-assistant
sudo install -d -o bbassist -g bbassist -m 0700 /var/lib/bb-graph-assistant/data
sudo install -d -o bbassist -g bbassist -m 0700 /var/lib/bb-graph-assistant/.caido-mcp
sudo install -d -o root -g root -m 0700 /etc/bb-graph-assistant
sudo install -m 0644 systemd/*.service /etc/systemd/system/
```

Prepare `/etc/bb-graph-assistant/runtime.env` with the same data root,
Neo4j connection, graph backend, and vault key used by the selected local
project. Prepare `/etc/bb-graph-assistant/caido-tail.env` with
`CAIDO_URL`, `BB_ASSISTANT_CAIDO_MCP_COMMAND`, the verified MCP executable
SHA-256, and `BB_ASSISTANT_CAIDO_TOKEN_FILE`. Keep both environment files
root-owned with mode `0600`; keep the token file owned by `bbassist` with mode
`0600`. Do not put token or vault-key values in the unit file, this repository,
or service arguments. Restarted services receive the key through the private
runtime environment; the backup archive does not contain it.

Before enabling services, list the registered project and run its local audit:

```sh
./scripts/bb-assistant projects --data-root /var/lib/bb-graph-assistant/data
./scripts/bb-assistant project-audit --project-key <registered-project-sha256> --data-root /var/lib/bb-graph-assistant/data
```

Resolve incompatible schemas or a missing vault key before enabling intake.
Then enable the loopback-only web UI and the project-specific worker/intake
units:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now bb-assistant-web-ui.service
sudo systemctl enable --now bb-assistant-projection-worker@<registered-project-sha256>.service
sudo systemctl enable --now bb-assistant-caido-tail@<registered-project-sha256>.service
```

The UI listens only on `127.0.0.1:8765`. Inspect current state and service logs
with:

```sh
./scripts/bb-assistant project-audit --project-key <registered-project-sha256> --data-root /var/lib/bb-graph-assistant/data
sudo journalctl -u 'bb-assistant-caido-tail@<registered-project-sha256>.service' -f
sudo journalctl -u 'bb-assistant-projection-worker@<registered-project-sha256>.service' -f
```

The project audit reports pending jobs split into due and retry-scheduled,
the age in seconds of the oldest pending job, expired worker leases, and
dead-letter counts. It omits job payloads and raw error codes. An unavailable
queue diagnostic means its schema or timestamps need operator review.

The live-tail checkpoint and request-ID index are persisted in the project
SQLite store. A synthetic test now closes and reopens the store between pages,
resumes the saved cursor, then replays known IDs and confirms no duplicate
captures. This verifies the local restart contract; it does not verify a
running systemd manager, the configured Caido MCP process, Neo4j delivery, or
the working project. Keep those runtime checks `PARTIAL` until observed on the
installed host.

## Knowledge-base tools

Install the Python extras and run the CLI:

```sh
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -e '.[kb,mcp]'
python -m bb_assistant.kb.cli --help
```

Source feeds are pinned and license-gated. A source listed in configuration is not proof that a complete corpus has been downloaded or that all terms have been cleared. Snapshots and indexes are runtime data and should remain outside the source checkout.
