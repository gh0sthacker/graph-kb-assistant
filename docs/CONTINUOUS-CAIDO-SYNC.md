# Continuous Caido Sync and Proxmox SSD Plan

**Status:** the Go `caido-sync` manager is implemented in the repository but
has not yet been installed or enabled on the server. The earlier CT 108
read-only `caido_list_projects` call returned HTTP 403; that old failure means
the deployed worker could not sync and is not evidence that the connected
Caido instance currently has no projects. Do not purge server data until the
new manager can authenticate and obtains repeated successful inventories.

## What we reuse from RedAmon

RedAmon separates the full HTTP corpus from its derived graph. Its own capture
proxy writes request/response transactions to PostgreSQL through a durable
spool and background ingest worker. Its Neo4j graph stores merged entities and
relationships, such as BaseURL → Endpoint → Parameter; `MERGE` keys include
the project/user namespace. The agent has different read paths for graph
questions and raw traffic. See the [traffic capture design](https://github.com/samugit83/redamon/blob/master/docs/readmes/README.TRAFFIC.md),
[graph DB design](https://github.com/samugit83/redamon/blob/master/docs/readmes/README.GRAPH_DB.md),
and [endpoint/parameter writes](https://github.com/samugit83/redamon/blob/master/graph_db/mixins/recon/resource_mixin.py).

We should keep that separation, but Caido already owns our full traffic corpus,
so we do not need to add RedAmon's proxy or copy every raw body into another
store. Caido is the canonical source; our local graph is a fast, compact
projection with links back to Caido request IDs.

## Caido capabilities verified so far

- The connected MCP exposes `caido_list_requests(limit, after, httpql)` and
  `caido_get_request(ids, include, bodyLimit, bodyOffset)`. The request list is
  capped at 100 rows and exposes an opaque pagination cursor.
- Caido HTTPQL supports `req.created_at.gt` time filtering. A read-only live
  query using that filter returned a page and cursor on 2026-10-21. This proves
  the filter is accepted by the connected server; it does not prove snapshot
  isolation or lossless pagination while captures are arriving.
- The current connected MCP interface does not expose a request-created event
  subscription. Caido's Workflow SDK can inspect the request/response attached
  to a workflow run, but a reliable local push into our service has not yet
  been verified.
- The model-facing live project-list check exposed one project. The service
  deployed in CT 108 uses its own pinned Caido MCP connection, so its project
  inventory is checked separately before claiming all-project coverage.

References: [Caido HTTPQL fields and timestamps](https://docs.caido.io/app/reference/httpql.html),
[Caido workflow JavaScript SDK](https://docs.caido.io/app/guides/workflows_javascript),
[Caido GraphQL stability note](https://docs.caido.io/app/concepts/graphql.html).

## Implemented continuous sync path

Use `bb-assistant caido-sync`, a bounded poller that discovers every project
in one configured Caido instance, processes projects sequentially, and drains
their durable graph projection queues. It runs independently of graph queries
and analysis.

1. Read the complete project inventory. Newly seen projects get a namespaced
   local store, resumable historical backfill, and live tail. Existing projects
   continue from their durable import and tail checkpoints.
2. A successful project-list response that omits a registered project records
   one missing confirmation. Three successful inventory responses (30 seconds
   apart by default) report the project as absent from Caido. Its local project
   registration, graph, and evidence remain available as history. A Caido
   MCP/API error or malformed inventory never counts as an omission. A project
   that reappears resets its pending missing count and resumes synchronization.
   Deleting retained history requires a separate explicit operator purge.
3. Query a fixed RFC3339 interval using `req.created_at.gt` and
   `req.created_at.lt`. The default interval overlaps the last checkpoint by 90
   seconds; a fixed upper bound makes each cursor chain finite.
4. Read only request-list metadata first. For historical backfill, fetch each
   page's details and let the durable capture index deduplicate replayed pages.
   The live tail checks request IDs against the existing capture indexes before
   fetching details, so overlap scans do not refetch already stored messages.
5. Read at most 256 KiB of each request and response body by default. Larger
   messages are marked incomplete; their prefix signature is explicitly marked
   partial and is not presented as an exact whole-response match.
6. Import bounded historical pages (five 50-row pages per 30-second cycle by
   default) and one live-tail page in a single
   BehaviorGraph transaction and one compact immutable receipt. If the cycle
   reaches its page budget, persist the current window and cursor and resume it
   on the next cycle.
7. Advance the watermark only after the bounded window reaches its terminal
   cursor. Re-query the overlap and deduplicate by instance, project, and
   request ID so delayed writes can be picked up.
8. Drain at most 1,000 durable projection jobs per project cycle in batches of
   100. If the queue is still pending or contains a failed job, the cycle is
   reported as incomplete; intake is not described as graph projection.

Caido's HTTPQL `created_at` field accepts RFC3339 date/time values. The page
metadata itself exposes ID/method/URL/status, not a stable request timestamp;
therefore the local watermark records the completed query-window boundary and
is not a frozen source snapshot. See [Caido HTTPQL](https://docs.caido.io/app/reference/httpql.html).

## What gets written locally

- Store one project-scoped reference/index row per new Caido ID. This supports
  deduplication and exact lookup without rewriting large entity payloads.
- Reuse the existing BehaviorGraph: endpoint and parameter entities merge by
  normalized identity; observations keep provenance; exact request variants
  and exact response variants are deduplicated by project-keyed HMAC signature.
  A truncated message gets a partial-prefix signature and explicit parse state.
- Each response variant retains safe searchable metadata (status, content type,
  body size, header names, field paths and parser characteristics). Full body
  values and sensitive header values stay in Caido. This makes graph search
  self-contained for discovery and response-variant lookup; a selected response
  body is fetched from Caido by project and request ID only when analysis needs
  its content.
- Connect an observation to its request and response variant. Add
  `referrer_context_for` edges when a recorded `Referer` matches another
  captured request and `redirects_to` edges when a 3xx `Location` matches a
  captured request. Every edge records whether the evidence was an exact
  normalized URL match or only a normalized route-template match. Exact URL
  matching uses a project-keyed HMAC, so query values are not stored. These are
  navigation hints, not proof of application execution causality. Caido's
  current MCP list metadata omits a stable capture timestamp and initiator ID,
  so exact browser call order/initiator remains unknown; the graph must not
  label these links as proven causes.
- Process message bytes in memory and discard them after parsing. The live path
  does not duplicate raw bodies in the local vault; it returns the Caido
  instance/project/request ID so the harness can fetch one selected exchange
  from Caido when needed. The legacy explicit backfill keeps its encrypted
  vault behavior.
- Graph search reads the local structural index and returns graph seeds and
  Caido IDs. It can filter by either a keyed request-variant signature or a
  keyed unique-response signature; `bb_graph_slice` follows an observation to
  the deduplicated request/response nodes and their graph relationships.
  Ordinary graph queries do not call Caido.

This still reads each unseen exchange once to detect new fields and unique
request/response variants. Repeated overlap scans check metadata only and do
not refetch bodies. One page receipt replaces one receipt file per request.

## Proxmox and SSD placement

Target all MVP hot-path state on Proxmox storage backed by local SSD: assistant
service, project registry, per-project SQLite databases, graph/search indexes,
sync checkpoints, and any temporary queue/spool. Do not run the live SQLite
databases from the SMB share or another network filesystem. Keep backups on a
separate storage target; do not put the active DB there.

The assistant guest is CT 108. Its root volume is on Proxmox `ssd-lvm` (64 GB),
and Caido resolves as `caido.myzone.su`; a TCP connection from CT 108 to its
MCP API port 8080 succeeded. Runtime databases remain in
`/var/lib/bb-graph-assistant/data` inside the CT, not on the SMB source share.
Moving the Caido application itself is a separate task.

## Removal semantics and remaining deployment gaps

Absence from Caido is not deletion from the assistant. After three successful
inventory omissions the manager emits one `project_absent_from_caido` event and
retains the project's registry entry, local store, graph records, and evidence
as history. A returning project clears the omission counter and resumes sync.
Only a separate explicit operator purge may remove retained local history.

The unit template is `systemd/graph-kb-assistant-caido-sync.service`; its
private configuration example is
`systemd/graph-kb-assistant-caido-sync.env.example`. In the latest inspected
CT 108 state it had not been installed or enabled. Deployment still requires
working MCP authentication from the server, confirmation that the service uses
the same private data root and Neo4j credentials as the UI, and a smoke run
showing successful project inventories, backfill/tail, and graph projection.
The source API has no stable snapshot token or push event, so this is eventual
synchronization; it cannot promise atomic lockstep or a provably complete
historical snapshot.
