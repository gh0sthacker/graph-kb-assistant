# Architecture overview

BB Graph Assistant is a local-first application with Go as its primary runtime and Python tooling for knowledge-source ingestion and compatibility workflows.

## Data flow

1. The Caido adapter reads an explicitly selected project through Caido's MCP interface.
2. Import code normalizes bounded pages and writes project-scoped structural indexes. Historical message bytes, when imported, are stored in an encrypted local vault; live-tail message bodies are handled in memory.
3. The BehaviorGraph records typed observations, request/response variants, and provenance links. It represents observed evidence, not a causal execution trace or a confirmed vulnerability.
4. The local web explorer and read-only MCP tools query the selected project's indexes and graph.
5. Knowledge-base tools build source-attributed snapshots from pinned source inputs. The compiled corpus is kept outside the source repository.

## Boundaries

- Caido is accessed through its public MCP integration; the application does not read Caido's internal database directly.
- Project selection is explicit for operations that import or reconcile history.
- The web explorer accepts loopback requests only.
- Runtime databases, vault keys, credentials, and capture data belong in a private data directory outside the checkout.
- Incomplete parsing, missing source watermarks, and incomplete source collections remain visible as partial coverage.

See [SETUP.md](SETUP.md) for a local setup path and [PROJECT-STATUS.md](PROJECT-STATUS.md) for current limits.
