# Architecture overview

BB Graph Assistant is a local-first, human-led analysis companion. It helps an analyst navigate captured application behavior, compare evidence, and form testable hypotheses. Go is the primary runtime; Python tooling ingests and validates knowledge sources.

## Data flow

1. The Caido adapter reads an explicitly selected project through Caido's MCP interface.
2. Import code normalizes bounded pages and writes project-scoped structural indexes. Historical message bytes are stored in an encrypted local vault; live-tail message bodies are handled in memory.
3. The BehaviorGraph relates typed observations, endpoints, parameter shapes, response variants, actor/session labels, object references, and provenance.
4. The web explorer and read-only MCP tools expose bounded graph slices and links to the underlying captured exchanges.
5. The analyst or assistant inspects only the relevant exchanges, retrieves focused KB sources, and records a candidate with alternatives and missing evidence.

The graph is the navigation layer for the investigation, not a vulnerability verdict or a causal trace. The assistant does not send target requests in the current workflow.

## Evidence graph and model boundary

Keep three kinds of information distinct:

- **Observed facts:** parsed from a specific Caido request/revision and response, with parser version and completeness.
- **Derived relationships:** deterministic or heuristic links such as normalized endpoint matches, referer links, or observed flow order. Store the match basis and ambiguity with each link.
- **Hypotheses:** analyst or model interpretations that reference observations and KB sources. They must not be represented as observed relationships or promoted by graph reachability alone.

Query the graph progressively: begin with an endpoint or behavior family, then expand a bounded neighborhood or path for parameters, response variants, actor/object context, and flow. Return source IDs, match basis, parse completeness, and coverage with each slice. Short deterministic annotations can describe method, field location/type, response class, or parser warnings. Any future model-generated annotation must remain versioned and visibly marked as interpretation. Do not send the whole graph or all capture bodies to a model.

The [Positive Technologies article on graph-guided code auditing](https://habr.com/ru/companies/pt/articles/1084780/) discusses code property graphs (CPGs). Its code-specific AST/CFG/data-flow structures do not directly apply to HTTP history. The transferable idea is to use a formal graph to locate a small, relevant subgraph before asking a language model to interpret it; graph structure supplies navigation and evidence, while the model supplies a reviewable explanation.

## Graph storage

Neo4j is the primary analytical graph backend. Every graph node and relationship carries the selected `project_key`; queries and writes stay within that namespace. The application reads Neo4j through the shared graph Reader, which applies the same bounded-slice and evidence validation used by the SQLite implementation.

SQLite remains the durable capture catalog, Caido sync/checkpoint store, normalized BehaviorGraph recovery copy, and transactional outbox. A graph import first commits normalized records and an outbox event together in SQLite, then applies the event idempotently to Neo4j. If Neo4j is unavailable, the event remains retryable and graph sync reports `partial`. The `neo4j-backfill` command replaces one project's graph snapshot atomically and verifies a read-back digest before cutover. The encrypted vault remains the only store for historical raw exchanges; Neo4j stores sanitized structural properties and provenance links, never request or response bodies.

Use `scripts/bb-assistant` to run the CLI, web UI, or MCP server with the private Neo4j runtime configuration. `scripts/bb-assistant-mcp` is the MCP-specific entry point. SQLite is still available as a migration/recovery backend by setting `BB_ASSISTANT_GRAPH_BACKEND=sqlite` for a process. A successful graph backfill or Neo4j read does not imply complete Caido history coverage: Caido reconciliation and source coverage continue to report their measured state independently.

## Boundaries

- Caido is accessed through its public MCP integration; the application does not read Caido's internal database directly.
- Project selection is explicit for operations that import or reconcile history.
- The web explorer accepts loopback requests only.
- Runtime databases, vault keys, credentials, and capture data belong in a private data directory outside the checkout.
- Incomplete parsing, missing source watermarks, and incomplete source collections remain visible as partial coverage.

See [SETUP.md](SETUP.md) for a local setup path and [PROJECT-STATUS.md](PROJECT-STATUS.md) for current limits.
