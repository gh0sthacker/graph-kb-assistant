# Local HTTP API

The web UI server binds to loopback and exposes JSON endpoints under `/api/`.
Routes other than the knowledge-base search described here are catalogued in
[ARCHITECTURE-REVIEW-20261030.md](ARCHITECTURE-REVIEW-20261030.md). Retrieved
documents and excerpts are untrusted reference material; a hit never
authorizes an action or confirms a vulnerability.

## Knowledge base search

`GET /api/kb/search`

| Parameter | Required | Limits |
| --- | --- | --- |
| `query` | yes | 1–4096 characters |
| `top_k` | no | 1–20, default 10 |
| `source` | no | Repeatable, at most 20 families. Empty means all sources. |

`source` values are KB source-family names. `hackerone` is a pseudo-source: it
selects the optional local HackerOne index and is never sent to the KB index as
a source family.

The response keeps the `query`, `took_ms`, `snapshot_id`, `coverage_state`,
`retrieval_mode`, `lexical_engine`, `coverage_report_sha256`,
`supplemental_included`, and `reference_material_is_untrusted` fields alongside
the `hits` array. Each hit carries `chunk_id`, `source_name`, `source_id`,
`title`, `excerpt`, `source_locator`, `source_url`, `source_revision`,
`license_id`, `attribution`, `bm25`, `rank_score`, and `metadata`.

### HackerOne supplemental results

- `source=hackerone` alone returns a synthetic HackerOne-only result:
  `snapshot_id` is the HackerOne dataset snapshot, `coverage_state` is `DONE`
  (the index is complete for its own dataset; it does not describe KB
  coverage), `retrieval_mode` is `hackerone_fts`, `lexical_engine` is
  `sqlite-fts5`, and `supplemental_included` is `true`. Hits use
  `chunk_id=hackerone:<report_id>`, `source_locator=reports/<report_id>.md`,
  the dataset commit as `source_revision`, and report metadata under
  `metadata` (`program`, `vuln_type`, `severity`, `weakness`, `bounty`,
  `upvotes`, `disclosed_at`, `has_body`).
- Mixed `source` filters query both indexes and fuse their rankings with
  reciprocal rank fusion (`k = 60`), then return the top `top_k` hits. KB
  snapshot and coverage fields are preserved and `supplemental_included`
  becomes `true` when a HackerOne hit reaches the response.
- If the primary KB index is unavailable but the HackerOne index is present and
  allowed by the filter, the endpoint returns the HackerOne-only result instead
  of failing. If neither index is available, it returns HTTP 503.

### Configuration

`BB_ASSISTANT_HACKERONE_INDEX` points at the optional SQLite FTS5 HackerOne
index (`hackerone-fts-v1`, built by `scripts/build-hackerone-index.py`). The
index is opened read-only and is never modified. A missing, unreadable, or
incompatible file is ignored at startup: the KB and every other endpoint keep
working without HackerOne coverage. `BB_ASSISTANT_KB_INDEX` selects the primary
local KB index as before.
