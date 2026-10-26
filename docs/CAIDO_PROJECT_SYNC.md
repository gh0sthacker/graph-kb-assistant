# Caido Project Inventory Sync

`bb-assistant caido-sync` is the continuous Caido-to-graph manager. It polls
the complete project inventory, creates stores for new project IDs, resumes
historical import, tails new captures, and drains the durable graph projection
queue. Caido remains the source of truth; the graph is a derived view.

An omitted project is purged only after three successful project-list
responses by default. API errors, timeouts, malformed responses, and duplicate
IDs do not count. The inventory confirmation state is persisted across service
restarts, and a project that returns resets its pending removal count. After
confirmation, the manager takes the project maintenance lock, deletes only the
matching Neo4j `project_key` in bounded batches, removes local capture,
index, and consultation files, and unregisters the project. Deleted projects
are not archived and are not retained as graph history. If Neo4j is
unavailable, it retains the local store and registry row and retries later.

The sync is eventual, not atomic: Caido MCP provides no stable inventory
snapshot or push event stream. Each successful poll reconciles project IDs;
per-project checkpoints and overlap scans handle request updates. Historical
backfill and live tail apply the configured exact origin scope before fetching
message details. A scope is required by the supplied systemd unit.

The system unit is intended for the graph server VM/container, under the same
`bbassist` account, Neo4j environment, and `/var/lib/bb-graph-assistant/data`
root as the web UI. The web UI and MCP processes refresh their registered
project catalog before API requests, so additions and removals become visible
without restarting those processes. The unit template is
[`../systemd/graph-kb-assistant-caido-sync.service`](../systemd/graph-kb-assistant-caido-sync.service).
The private config template is
[`../systemd/graph-kb-assistant-caido-sync.env.example`](../systemd/graph-kb-assistant-caido-sync.env.example).
Deployment is not complete until the server can authenticate to Caido MCP, the
sync process and UI use the same data root and Neo4j settings, and a smoke run
confirms inventory, backfill/tail, graph projection, and deletion behavior.
