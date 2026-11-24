# RedAmon KB rebuild

The transfer boundary is a canonical, provenance-rich corpus rather than the
RedAmon FAISS index or Neo4j database. RedAmon's index depends on its embedding
model and storage schema; this project rebuilds its own search indexes from
source documents.

The source manifest covers the seven families found in RedAmon's implementation:
local tool docs, GTFOBins, LOLBAS, OWASP WSTG, ExploitDB descriptions/metadata,
NVD, and Nuclei templates. Revisions and filters are written in
`src/bb_assistant/kb/sources.yaml`. They reflect pins found in the local
RedAmon checkout and must be revalidated before downloading because they were
recorded on 2026-07-10.

Here, `profile: full` means the full configured source-family set and the
RedAmon full-profile filters; it does **not** mean an all-time NVD archive.
RedAmon's `kb-rebuild-full` Make target explicitly sets the NVD publication
lookback to 730 days and the full profile disables the standard CVSS floor.
This project mirrors that rolling window and records it in the manifest.

`kb.canonical` defines source records and stable chunk IDs with character spans.
`kb.rebuild.export_snapshot()` writes `source-documents.jsonl`, `chunks.jsonl`
and a final `manifest.json` outside the source checkout. Each source record
retains its text, source locator/URL, revision, content hash, metadata and
license/attribution fields. Embeddings and local search indexes are separate
derived artifacts. The injected-provider contract and canonical adapter are in
[`redamon_sources.py`](../src/bb_assistant/kb/redamon_sources.py); it blocks
families without eligible terms, reconciles requested and applied filters,
and reports incomplete revisions/counts/notices. A source-pinned snapshot of
RedAmon's `agentic/skills/` tree is vendored under
[`references/redamon/agentic-skills/`](../references/redamon/agentic-skills/).
Its local provider verifies the source commit, exact file set, per-file hashes,
and MIT notice before producing ToolDocs records. A second provider now fetches
the pinned GTFOBins archive, reads source files in memory without extracting
paths to disk, verifies a bounded tar layout, and includes the pinned
LICENSE/NOTICE documents in the canonical records. A third provider handles
LOLBAS YAML records and preserves its pinned LICENSE and NOTICE.md, including
the embedded MITRE ATT&CK notice. The shared archive reader checks the
repository/commit root, rejects unsafe or non-regular entries, and does not
extract to disk. A fourth provider selects WSTG test-case Markdown and preserves
the root license notice; the exact pinned WSTG license is now verified. A fifth
provider preserves the selected Nuclei YAML templates and root license
document; its exact-pin MIT license is also verified. These sources still need
their downloaded snapshots to be rebuilt when their pins or filters change. A
sixth provider reads the pinned ExploitDB CSV, discards PoC/source
fields, keeps only the declared description/metadata allowlist, and preserves
the pinned `README.md` and `LICENSE.md`. The source gate is supported by the
official pinned GitLab README's GPLv2 statement; the provider reads no PoC
files. It also renders each search body with RedAmon's exact description,
platform, type, published-date, and codes formatting. A
seventh provider now pages the NVD CVE API using non-overlapping publication
windows, reconciles page totals and cursors, preserves each full CVE JSON
object, and binds the snapshot to response hashes and a fetch timestamp. It
has no cache fallback. Its manifest gate requires a composite terms status,
completed embedded-content review, a current-year review date, a NIST dataset
citation, and the CVE Program copyright designation. Every NVD snapshot
contains separate NIST and CVE terms notice records. The latest full 730-day
NVD build attempt did not produce a source batch: its coverage report records
`provider_failed:ValueError`, without retaining the underlying message. The
other four external families have since been fetched into the latest partial
snapshot; ExploitDB remains excluded by its unresolved dataset terms.

A separate, bounded one-day provider canary on 2026-10-22 fetched and parsed
all 531 expected CVE rows for the 2026-10-21/22 window and recorded two terms
notices. This diagnostic was not imported into the local index. It shows that
the narrow-window request path can succeed, but does not explain or repair the
730-day `ValueError`; NVD coverage therefore remains `partial`.

`kb.rebuild.import_snapshot(snapshot_dir, index)` imports a previously exported
version directory into the local SQLite FTS5 index. Before opening the index for
replacement, it checks manifest schema and snapshot identity, both JSONL SHA-256
digests and row/byte counts, each source document's content hash, and the exact
deterministic chunks, hashes, IDs, provenance and character spans regenerated
from those documents. It accepts only the version directory named by the
manifest's snapshot ID. Index replacement remains transactional, so a failed
insert does not leave a partially replaced index. For example:

```python
from pathlib import Path
from bb_assistant.kb.index import KBIndex
from bb_assistant.kb.rebuild import import_snapshot

stats = import_snapshot(Path("/path/to/snapshots/<snapshot-id>"), KBIndex())
```

The importer carries the validated coverage state and coverage-report hash into
`kb_meta`; it also stores the report's per-source rows. `KBIndex.stats()` returns
the overall state, report hash and per-source coverage rows. A `DONE` state
requires a matching coverage report whose state is `DONE`, whose per-source
rows are all `done`, and whose blocked/partial counts are zero. A missing report
is reported as `unverified`. Export completion by itself never marks source
coverage `DONE`.
Snapshot import creates the canonical FTS5 index. The pinned E5 vectors are a
separate derived build: `bb-assistant kb embed` persists completed batches so a
long build can resume after a process restart. It marks vectors ready only
after every content chunk for that snapshot/model has a matching vector.
Search uses vectors only when those IDs match the current imported snapshot.
The Go MCP server reads
this same SQLite index and calls the local pinned E5/BGE worker; it falls back
to lexical search if the model worker or vector generation is unavailable.

## Local retrieval models

Install the CPU-only E5/BGE runtime and download the exact RedAmon model
revisions with:

```sh
./scripts/setup-kb-models
```

The script stores its private virtual environment under the application's
runtime data directory, installs CPU PyTorch before Sentence Transformers,
and verifies both pinned models using synthetic input. It does not add model
weights to the source snapshot or Git. Rebuilding the local vector table after
an eligible snapshot import is a separate step:

```sh
./scripts/bb-assistant-kb embed
```

Model inference from the Go MCP server is local and offline; downloading model
weights is only done by the explicit setup command. The opt-in bridge check
exercises Go → Python → E5/BGE directly:

```sh
BB_ASSISTANT_KB_MODEL_SMOKE_PYTHON="$HOME/.local/share/bb-graph-assistant/kb-models-venv/bin/python" \
  go test ./internal/kb -run '^TestPythonRetrievalModelsSmoke$' -count=1 -v
```

## 2026-10-25 retrieval parity implementation checkpoint

### 2026-10-25 correction: parity is not complete

This correction supersedes any reading of the earlier checkpoint as claiming
that the active KB is already identical to RedAmon. It is not. The active web
service uses the full local snapshot
`ac9d1d2ac0fe0a3e80654180c81ac43f46a77f1e4481a7dc593fdfaed16ee049` with all
seven declared source families and 209,026 content chunks, but the last
authoritative stats read found only 9,824 matching E5 vectors. Embedding is
still running; vector retrieval is incomplete until all 209,026 content chunks
have vectors for the pinned model.

The deployed RedAmon corpus/index snapshot and same-query output fixtures are
not available in this environment, so byte-for-byte corpus parity and
rank/output parity have not been verified. Matching source-family names and
model revisions is not proof of identical source records, chunk boundaries,
or query results. The active assistant currently persists vectors and lexical
search in SQLite; RedAmon uses FAISS plus Neo4j Lucene full-text. SQLite FTS5
and Lucene do not have identical analyzers or scoring. Thus lexical candidate
parity remains incomplete even after the compatibility changes below.

The current uncommitted compatibility changes remove SQLite prefix-wildcard
expansion, build RedAmon-style sanitized OR/Lucene-operator terms, use equal
title/body BM25 weights, and apply source allowlists after the global lexical
and vector candidate pools. Focused Python and Go ranking/MCP tests pass.
These changes narrow known differences; they do not make SQLite FTS5 equivalent
to Neo4j Lucene or prove end-to-end result parity. Next steps are to finish the
full vector build, obtain a pinned RedAmon reference snapshot or fixture, and
compare representative queries before declaring parity.

The Go search path now uses both FTS5 and persisted E5 candidates, merges the
two lists with RedAmon's `RRF k=60`, applies the pinned per-source boosts,
reranks the first 30 candidates with pinned BGE and applies `sigmoid(logit)`
before the source boost, then returns up to five results with source/title MMR
at `lambda=0.65`. A Go integration smoke test has exercised the local model
worker. The real index still needs a newly rebuilt eligible source snapshot
and matching E5 vector generation before `vector_ready` becomes true.

The 730-day NVD full-profile fetch is being repeated after fixing the adapter
to interpret offset-less NVD publication timestamps as UTC, matching the
provider's own date-window semantics. No coverage is promoted while that fetch
is in progress. The pinned ExploitDB metadata-only GitLab feed is now eligible
under its repository license evidence, but its CSV has not yet been imported
into the installed snapshot. Therefore this checkpoint is retrieval-code
parity work, not proof that the local KB corpus is identical to RedAmon.

### Latest verified local snapshot

The latest snapshot was built on 2026-10-21 and reports `PARTIAL` coverage:
14,300 documents and 16,961 chunks across seven declared source families. Its
snapshot ID is
`d342e9df4123768a3419963799b5f9c3d47347cfa0ec5aac94a5b28171ea23a3`; its
source-manifest SHA-256 matches the current `sources.yaml` (`46ed4ed8…`).
ToolDocs (44 documents), GTFOBins (479), LOLBAS (244), WSTG (130), and Nuclei
(13,403) are `done`. NVD is `partial` with `provider_failed:ValueError`; the
diagnostic text was intentionally suppressed by the adapter. ExploitDB is
`blocked` by the unresolved CSV dataset terms. No source family was promoted
to `DONE` by inference.

The source snapshot was validated by the Go `kb-validate` command and imported
into the private runtime FTS5 index. On 2026-10-22, the missing snapshot files
were recovered from the persisted SQLite source and chunk records. Recovery
recreated the original coverage-report hash and snapshot ID exactly, checked
every source content hash, regenerated every chunk and span with the pinned
chunker, and passed the production snapshot importer against a disposable
index. The working index file remained byte-for-byte unchanged. The restored
package is at the private runtime path `kb/snapshots/<snapshot-id>`; it is not
part of Git. SQLite `integrity_check` returned `ok` and the read-only foreign-key
check returned no violations. Go search returned five citations with valid
source/chunk SHA-256 values and spans. This verifies the stored source package
and Python/Go FTS compatibility, not vector retrieval or complete source
coverage. The family state remains `PARTIAL`: NVD is partial and ExploitDB is
blocked.

The 46 Markdown documents in the pinned `agentic/skills/` source bundle, their
file sizes and SHA-256 hashes, and the MIT notice hash were checked against
`SOURCE-MANIFEST.json`; there were no missing, extra, or mismatched files.

This verifies the local ToolDocs snapshot and the FTS5 retrieval path. It does
not establish full RedAmon KB coverage or retrieval parity. The pinned RedAmon
checkout is `cf1b9e926a2af4420f5a894e7c8ddd225968c9d2`; its retrieval
implementation is in `services/knowledge_base/kb_orchestrator.py`, configuration
in `kb_config.yaml`, and chunking in `chunking.py`. RedAmon embeds with
`intfloat/e5-large-v2`, retrieves candidates from FAISS and Neo4j Lucene,
overfetches by 6, merges ranks with Reciprocal Rank Fusion (`k=60`), applies
per-source boosts, reranks up to 30 candidates with
`BAAI/bge-reranker-base`, then uses MMR (`lambda=0.65`) to return up to five
diverse results. Its chunker targets 256 tokens and caps at 480 using an
approximate chars-per-token estimate. These are the observed upstream settings,
not a recommendation to copy them unchanged. The new assistant currently has
only FTS5 retrieval over canonical chunks; embeddings, hybrid fusion, reranking,
and diversity ranking remain future work and need evaluation against a fixture.

On 2026-10-21, a bounded synthetic archive check exercised the GTFOBins and
LOLBAS providers through the canonical adapter, snapshot export/import, and
FTS5 search. Both fixture families reached `done`, their license/notice
documents were retained, and a mismatched repository/commit archive root was
rejected. The combined fixture snapshot remained `PARTIAL` because the other
four families were not included in that fixture. This verifies the local contracts;
it does not verify an upstream archive download, real source-file counts, or
full source coverage. Python compilation also succeeded for `src/`.

A separate synthetic WSTG archive check exercised the test-document filter and
root-license preservation. At that fixture run the exact pinned license
evidence had not yet been added, so the adapter kept the family `blocked`. The
later exact-pin recheck cleared this license gate. A Nuclei fixture selected
one template, excluded `.github/` and pre-commit configuration files, and
retained the root `LICENSE.md`; its license gate was likewise cleared by the
later exact-pin recheck. These fixtures are not permission to ingest either
real feed and do not prove complete upstream coverage.

An ExploitDB CSV fixture exercised the description/metadata allowlist, last-row
deduplication for duplicate IDs, malformed-row accounting, and pinned license
record retention. A field carrying synthetic PoC text was excluded. The
canonical adapter continued to mark ExploitDB `blocked` on the manifest's
unresolved repository-license and CSV dataset-terms gates.

These offline provider checks are now preserved in
[`tests/test_kb_source_providers.py`](../tests/test_kb_source_providers.py),
along with ToolDocs snapshot export/import, FTS citation provenance, tampered
snapshot rejection, and source-count/rights-gate cases. Run them with
`PYTHONPATH=src python -m unittest discover -s tests -p 'test_kb_source_providers.py' -v`.
On 2026-10-21 the complete offline suite passed 35 tests across KB providers,
offline snapshot import, multi-project Caido import/resume, MCP tool
isolation/facade policy, and NVD contracts. Run the complete suite with
`PYTHONPATH=src python -m unittest discover -s tests -v`. These fixtures verify
local code paths only; they do not establish the upstream archive/API contents,
exact-pin rights for blocked families, or full external-source coverage.

The local checkout does not contain a downloaded GTFOBins archive or the
RedAmon FAISS/Neo4j corpus. The project can build a partial snapshot from
ToolDocs and the latest persisted snapshot (`150a0dda…`) contains ToolDocs only.
The GTFOBins provider can fetch its pinned archive when network access is
available, but that provider was not configured for the latest snapshot build.
The pinned GTFOBins GPL-3.0 LICENSE was checked, and the provider preserves it
in the source records. The first fetch could not be completed in this
environment because shell DNS resolution to GitHub is unavailable. Coverage
must be measured from a finished snapshot manifest, not inferred from README
estimates.

The initial rights audit (before the exact-pin recheck below) found that the pinned GTFOBins revision declares
GPL-3.0 and the exact pinned LOLBAS `NOTICE.md` declares GPL-3.0 and carries an
MITRE ATT&CK license notice. External detection and analysis links in LOLBAS
remain links; their contents are not copied. The current
OWASP WSTG project states CC BY-SA 4.0, but the exact pinned notice still needs
verification. ProjectDiscovery's Nuclei template maintainer states that
contributed templates are covered by MIT; NIST's NVD documentation describes
the data as available for product use and appreciates credit. ExploitDB's
repository is GPL-2.0-or-later, while the exact terms attached to the CSV
description/metadata subset remain to be confirmed. Source-local license
and notice files must still be stored in each snapshot; labels are not a
substitute for preserving notices. See the upstream
[GTFOBins license](https://github.com/GTFOBins/GTFOBins.github.io/blob/acd524623f9c406acedd2754ebd9c2431f3675ad/LICENSE),
[LOLBAS notice](https://raw.githubusercontent.com/LOLBAS-Project/LOLBAS/a2784c79091cb282fefb68f0056a853cfafd7e3c/NOTICE.md),
[OWASP WSTG license statement](https://github.com/OWASP/wstg/blob/master/document/1-About/README.md),
[Nuclei templates license discussion](https://github.com/projectdiscovery/nuclei-templates/discussions/11739),
[NIST NVD reference](https://tsapps.nist.gov/publication/get_pdf.cfm?pub_id=150396),
and [ExploitDB repository](https://gitlab.com/exploit-database/exploitdb).

The read-only client audit found additional completeness constraints. ToolDocs
are pinned to the exact RedAmon source commit
`cf1b9e926a2af4420f5a894e7c8ddd225968c9d2` (2026-10-20); the 46 nonempty
Markdown files, file hashes, and MIT notice are present in the project-local
source bundle. ToolDocs and GTFOBins have pinned license notices. LOLBAS has
GPL-3.0 evidence and the embedded ATT&CK notice in its exact pinned `NOTICE.md`;
its provider preserves both `NOTICE.md` and `LICENSE`. Exact-pin WSTG and
Nuclei license files are now preserved in the manifest and required in their
archives; current-repository statements alone remain insufficient. ExploitDB is marked
`GPL-2.0-or-later` at repository level, but the CSV dataset's terms remain
unresolved. The NVD content review now accounts for mixed provenance: NIST
identifies the NVD dataset as public and requests attribution; CVE Program
terms permit use of CVE List records when the MITRE copyright designation and
terms are reproduced. NVD confirms that CVE descriptions are maintained by
CNAs, and the provider stores their origin with the complete CVE JSON. The
provider emits separate NIST attribution/citation and CVE terms notices, and
does not fetch linked third-party reference pages. Recent NVD schemas also
include CISA-ADP SSVC and CVE `affected` data; the provider retains their
source metadata within the CVE record. See the official [NIST NVD dataset
record](https://doi.org/10.18434/M3436), [NIST data licensing
statement](https://www.nist.gov/open/copyright-fair-use-and-licensing-statements-srd-data-software-and-technical-series-publications),
[NVD description guidance](https://nvd.nist.gov/vuln/Vulnerability-Detail-Pages),
[CVE Program terms](https://www.cve.org/legal/termsofuse), and [NVD schema update](https://www.nist.gov/itl/nvd).

Source clients can return partial or cached results after download failures.
The new providers must reject unverified cache fallback, preserve each
source's original documents separately from RedAmon's narrower normalized
fields, bind every item to the requested pin, and report fetch/parse/reject
counts. ToolDocs reads direct Markdown files only; GTFOBins and LOLBAS
normalizers omit some fields; WSTG selects test-case documents rather than the
whole repository; Nuclei normalizes only selected template fields; and the NVD
client's cache/window behavior does not prove the manifest's full 730-day
coverage. The local NVD provider adds strict page-total and identity checks,
but its live API run has not been exercised and NVD offers no transaction
snapshot token. These behaviors are recorded in the source client
implementations under `redamon-src/services/knowledge_base/curation/` and must
not be mistaken for verified upstream coverage. The local NVD provider's
one-day live API path was exercised successfully on 2026-10-22; the full
configured 730-day run still fails and is not verified.

### License gate recheck (2026-10-21)

The exact pinned [WSTG LICENSE](https://raw.githubusercontent.com/OWASP/wstg/78e6b6733ee071eb160fdddbacc1bc97f83a13e3/LICENSE)
states CC BY-SA 4.0. The exact pinned [Nuclei LICENSE.md](https://raw.githubusercontent.com/projectdiscovery/nuclei-templates/17315de39ed36ee2329e02d958b51fd315ad55fe/LICENSE.md)
states MIT and names ProjectDiscovery, Inc. The source manifest now records
these commit-specific files, and both providers require the corresponding
notice in the fetched archive before ingesting source documents. These exact-pin
checks clear the WSTG and Nuclei license gates; ExploitDB remains blocked until
the CSV dataset's reuse terms are resolved.

## Rebuild stages

1. **All seven source-family providers are present; the latest eligible feeds are built, but coverage is partial:**
   ToolDocs reads the pinned local bundle; GTFOBins, LOLBAS, OWASP, and Nuclei
   fetch immutable commit archives; ExploitDB reads one pinned CSV and license
   file; NVD pages the configured live API date range without cache fallback.
   The latest snapshot contains GTFOBins, LOLBAS, OWASP, and Nuclei. Their
   pinned licenses/notices are preserved. ExploitDB remains blocked on dataset
   terms; the NVD full-window attempt failed with a redacted `ValueError` and
   requires a controlled reproduction and coverage reconciliation. Its
   attribution and embedded-content notices are present in the manifest/provider.
2. Save eligible source documents, notices, pins, response hashes and fetch
   metadata into the immutable snapshot.
3. Produce `SourceDocument` records with exact locators and license metadata.
4. Export one pinned snapshot and reconcile per-source fetch, document and
   chunk counts; incomplete but ingestible data is `PARTIAL`, while a snapshot
   with no eligible source documents can be `BLOCKED`.
5. Import the validated snapshot into the local FTS5 index and inspect
   `KBIndex.stats()` for document/chunk counts and preserved coverage state. A
   future vector index is a separate derived artifact.

Use `pip install -e '.[kb]'`, then run `bb-assistant-kb build` to create and
import the currently available snapshot. The command returns exit status 3
when source coverage is `PARTIAL` or `BLOCKED`; this is expected until external
feeds have reviewed providers. Set `BB_ASSISTANT_DATA_HOME` to a private path
outside this checkout if the default user data root is unsuitable. Do not place
SQLite databases on the shared SMB workspace. If source snapshots must be
written there, use `--snapshot-root` for the share and a local `--index-path`.

An already-exported snapshot can be validated and imported offline, without
re-running any source provider:

```text
bb-assistant-kb --index-path /path/on/local-disk/kb.sqlite3 import-snapshot /path/to/snapshots/<snapshot-id>
```

This command also exits with status 3 for `PARTIAL` or `BLOCKED` coverage. It
does not change or fetch the original source documents.

Raw Caido requests, responses, cookies, tokens and target-specific data never
belong in this global technical KB.
