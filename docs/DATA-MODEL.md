# Data model and privacy boundary

## Project isolation

Each Caido project has a project metadata/search SQLite database, a separate
BehaviorGraph SQLite database, immutable redacted source receipts, and an
encrypted vault. Runtime SQLite databases must live on a local filesystem with
working file locks; a shared workspace directory mounted over SMB is not a
suitable SQLite data root. The storage directory is named by SHA-256 of the locally
configured Caido `instance_key` and project ID, not by a human-supplied project name. A multi-
project query must receive an explicit list of selected projects; there is no
implicit global search.

The private `projects/registry.json` records the known project key, stable
instance key, Caido project ID, and display name. It is an identity catalog,
not a path catalog: none of its values are interpreted as file paths. The
read-only MCP facade exposes only projects present in this registry and pages
the list by the stable project hash. A project store registers its identity
when it is initialized.

One observation represents one Caido request ID within one instance/project.
Equal URL/body content does not merge observations. A response can be absent for
an in-flight or dropped request and remains distinct from an empty response.

## Stored messages and source fidelity

Message bytes from the explicit historical backfill are encrypted with AES-256-GCM before writing to the vault. Each
request and response revision records `wire_fidelity`: `source_bytes`,
`reconstructed_text`, or `unknown`. The current Caido MCP adapter reconstructs
the start line and headers and re-encodes returned text bodies as UTF-8, so its
records are labeled `reconstructed_text`; truncation state does not imply wire
fidelity. Only an adapter that receives the original message byte sequence may
use `source_bytes`. The key comes from `BB_ASSISTANT_VAULT_KEY`, is not
persisted by the application, and must be backed up separately by the
operator. AAD binds each encrypted object to its observation ID and side.
Files are written atomically with owner-only permissions on POSIX systems.

Metadata and graph records are held in SQLite. The BehaviorGraph contains
normalized entities, evidence assertions, and analysis cards. Historical
backfill receipts contain hashes, byte counts, truncation state, wire-fidelity
labels, and hashed project/observation identities; they contain no message
values. Live-tail receipts group one bounded page into one immutable file.

The live tail does not copy raw traffic into the local vault. It reads unseen
request/response bytes from Caido, parses them in memory, then retains only
redacted structure and project-keyed HMAC signatures. Unique `request_variant`
and `response_variant` nodes deduplicate equal complete messages inside that
project; truncated captures are marked as `partial_prefix` and are not
represented as exact whole-message matches. Each observation links to its
request/response variants and its Caido project/request IDs are kept in the
local lookup index. Agents can search either keyed variant signature and then
use an observation seed to traverse endpoint, request-variant, response-variant,
and navigation-evidence links. The signature keys derive from
`BB_ASSISTANT_VAULT_KEY`; rotating that key changes signatures for newly
ingested messages.

The Go historical and live importers now materialize these variant nodes and
observation assertions in BehaviorGraph. Historical signature filters resolve
the graph variants back to the current capture index; live signature filters
use the existing live index. Variant attributes are derived from each side's
message bytes and contain only the keyed signature, extent/parse state, method
or response status, media type, byte count, safe header names, and safe field
names. They omit wire-fidelity metadata so equal keyed bytes can merge even
when two source adapters described the bytes differently.

The Go mapper creates request `parameter` nodes for query names, normalized
path segments, JSON, form-urlencoded, and multipart/form-data body fields, and
HTML/XHTML forms, inputs, and links. Multipart file nodes retain only the
decoded byte count, normalized media type, and a source-bound opaque filename
label; file content and source filenames are never copied. The analysis card
retains sanitized form-action and link origin, path, and query-name metadata.
Response body fields use `returns` links from the observation. JSON nodes
retain structural type; form fields use `string`; HTML inputs retain only a
normalized control type. Body values and visible page text are never copied.
Names that fail the Python mapper's safe-name rules become source-bound opaque
aliases. Unsupported body media, missing or truncated messages, and parser
limits remain explicit gaps on the analysis card; these nodes do not claim
complete semantic coverage.

Python and Go use the same project-scoped variant identity. When both parsers
observe the same signature, graph import keeps the first safe parser projection
and merges source references; it rejects conflicts in signature extent, media
type, body size, request method, or response status. Signature search scans
candidate observations in bounded batches, applies the selected capture filters
before the global result limit, and orders ties consistently by request ID.

Never store header values such as Cookie or Authorization, body values,
credentials, or personal data in graph attributes, receipts, logs, KB, or
default indexes. MCP reconstructed records are not represented as exact wire
data.

## Search strategy for captured traffic

The default searchable projection contains method, host, normalized route,
status, content types, header names, and parsed parameter/field names. These
features feed structural search and the BehaviorGraph. Search results lead to
specific Caido IDs and graph observations; they do not duplicate message
values. Exact request and response variants remain addressable by keyed
signature.

Historical backfill request/response bodies stay encrypted in the vault. Live
tail bodies stay in Caido. Broad plaintext FTS over bodies would make secrets
and personal data searchable in a second, unencrypted database, so it is not
part of the default index. The graph-guided path first narrows to a project,
endpoint family, and bounded capture set; the harness can then request the
selected message from Caido by its returned project and request IDs.

If larger histories show that this is too slow, measure an on-demand scan that
decrypts only graph-selected captures before adding a persistent content
index. An encrypted searchable index is a later option: it can improve broad
search speed, but adds key rotation/rebuild work and can leak term equality or
frequency depending on its design. It needs an explicit threat model and a
performance benchmark before becoming the default. Search output should return
capture IDs, sides, offsets, and coverage state; raw matched text remains
operator-gated.

The MCP facade returns structural capture summaries by default. Its separate
raw range tool reads only records retained by the historical backfill and is
hidden unless `BB_ASSISTANT_ALLOW_RAW_CAPTURE_TOOL=1`; it requires one project,
request ID, side, and byte range, and returns at most 16 KiB. Live-tail records
return the Caido reference instead; they are fetched from Caido when selected.
The facade does not log returned bytes.

The historical-capture store encrypts message objects and uses project-scoped
databases. The live index records one Caido reference, keyed signatures, graph
seed, structural metadata, and local sync time per unseen request. Search
returns those references and graph seeds without asking Caido on ordinary
queries. Receipts must be retained at least as long as graph rows: the graph
checks their path and hash when producing slices. There is no automatic receipt
cleanup. Retention policy, key rotation, backups, and live-load measurements
remain operational work. Persistent full-text body indexing is not enabled.

## Knowledge base

The KB is global technical reference material, kept separate from per-program
captures. Canonical chunks retain source ID, upstream revision, original hash,
license/attribution metadata, chunk schema and chunk hash. Rebuilding an index
must not silently change source content or source filters. Feed data is not
downloaded in this initial scaffold, and each source remains marked for a
source-specific license review.
