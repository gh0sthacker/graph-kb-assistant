package store

// schemaV5 mirrors the established Python schema. Keep changes additive and versioned.
const schemaV5 = `

PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS project_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sync_state (
  source_key TEXT PRIMARY KEY,
  cursor TEXT,
  last_request_id TEXT,
  run_id TEXT,
  state TEXT NOT NULL CHECK (state IN ('new','running','partial','done','failed')),
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sync_runs (
  run_id TEXT PRIMARY KEY,
  source_key TEXT NOT NULL,
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  page_count INTEGER NOT NULL DEFAULT 0 CHECK (page_count >= 0),
  distinct_request_count INTEGER NOT NULL DEFAULT 0 CHECK (distinct_request_count >= 0),
  state TEXT NOT NULL CHECK (state IN ('running','partial','done'))
);
CREATE INDEX IF NOT EXISTS sync_runs_source_idx ON sync_runs(source_key, started_at);
CREATE TABLE IF NOT EXISTS sync_run_cursor_digests (
  run_id TEXT NOT NULL REFERENCES sync_runs(run_id),
  cursor_digest TEXT NOT NULL,
  PRIMARY KEY (run_id, cursor_digest)
);
CREATE TABLE IF NOT EXISTS sync_run_request_digests (
  run_id TEXT NOT NULL REFERENCES sync_runs(run_id),
  request_id_digest TEXT NOT NULL,
  PRIMARY KEY (run_id, request_id_digest)
);
CREATE TABLE IF NOT EXISTS captures (
  observation_id TEXT PRIMARY KEY,
  caido_instance_id TEXT NOT NULL,
  caido_project_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  captured_at TEXT,
  request_sha256 TEXT NOT NULL,
  response_sha256 TEXT,
  request_size INTEGER NOT NULL,
  response_size INTEGER,
  current_revision_id TEXT,
  request_vault_ref TEXT NOT NULL,
  response_vault_ref TEXT,
  parse_state TEXT NOT NULL DEFAULT 'not_parsed',
  imported_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE (caido_instance_id, caido_project_id, request_id)
);
CREATE TABLE IF NOT EXISTS graph_capture_map (
  observation_id TEXT PRIMARY KEY REFERENCES captures(observation_id),
  revision_id TEXT NOT NULL,
  graph_observation_id TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS capture_revisions (
  revision_id TEXT PRIMARY KEY,
  observation_id TEXT NOT NULL REFERENCES captures(observation_id),
  request_sha256 TEXT NOT NULL,
  response_sha256 TEXT,
  request_size INTEGER NOT NULL,
  response_size INTEGER,
  request_vault_ref TEXT NOT NULL,
  response_vault_ref TEXT,
  captured_at TEXT,
  request_truncated INTEGER CHECK (request_truncated IS NULL OR request_truncated IN (0,1)),
  response_truncated INTEGER CHECK (response_truncated IS NULL OR response_truncated IN (0,1)),
  request_wire_fidelity TEXT NOT NULL DEFAULT 'unknown'
    CHECK (request_wire_fidelity IN ('source_bytes','reconstructed_text','unknown')),
  response_wire_fidelity TEXT NOT NULL DEFAULT 'unknown'
    CHECK (response_wire_fidelity IN ('source_bytes','reconstructed_text','unknown')),
  first_seen_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS graph_capture_revisions (
  revision_id TEXT PRIMARY KEY REFERENCES capture_revisions(revision_id),
  graph_observation_id TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS captures_request_id_idx ON captures(request_id);
CREATE INDEX IF NOT EXISTS captures_time_idx ON captures(captured_at);
CREATE TABLE IF NOT EXISTS capture_search (
  observation_id TEXT PRIMARY KEY REFERENCES captures(observation_id),
  method TEXT NOT NULL,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  route TEXT NOT NULL,
  response_status INTEGER,
  request_content_type TEXT NOT NULL,
  response_content_type TEXT NOT NULL,
  field_names_json TEXT NOT NULL,
  header_names_json TEXT NOT NULL,
  captured_at TEXT
);
CREATE TABLE IF NOT EXISTS live_sync_state (
  source_key TEXT PRIMARY KEY,
  watermark TEXT,
  window_since TEXT,
  window_until TEXT,
  cursor TEXT,
  state TEXT NOT NULL CHECK (state IN ('new','running','partial','idle','failed')),
  last_polled_at TEXT,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS live_capture_index (
  observation_id TEXT PRIMARY KEY,
  caido_instance_id TEXT NOT NULL,
  caido_project_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  graph_observation_id TEXT NOT NULL,
  synced_at TEXT NOT NULL,
  method TEXT NOT NULL,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  route TEXT NOT NULL,
  response_status INTEGER,
  request_content_type TEXT NOT NULL,
  response_content_type TEXT NOT NULL,
  request_size INTEGER NOT NULL,
  response_size INTEGER,
  request_signature TEXT NOT NULL,
  response_signature TEXT,
  request_url_key TEXT NOT NULL,
  referer_url_key TEXT,
  redirect_url_key TEXT,
  field_names_json TEXT NOT NULL,
  header_names_json TEXT NOT NULL,
  parse_state TEXT NOT NULL CHECK (parse_state IN ('complete','partial')),
  UNIQUE (caido_instance_id, caido_project_id, request_id)
);
CREATE INDEX IF NOT EXISTS live_capture_host_idx ON live_capture_index(host);
CREATE INDEX IF NOT EXISTS live_capture_route_idx ON live_capture_index(route);
CREATE INDEX IF NOT EXISTS live_capture_status_idx ON live_capture_index(response_status);
CREATE INDEX IF NOT EXISTS live_capture_synced_idx ON live_capture_index(synced_at);
CREATE INDEX IF NOT EXISTS live_capture_request_url_idx ON live_capture_index(request_url_key);
CREATE INDEX IF NOT EXISTS live_capture_redirect_url_idx ON live_capture_index(redirect_url_key);
CREATE VIRTUAL TABLE IF NOT EXISTS live_capture_fts USING fts5(
  observation_id UNINDEXED, method, host, route, fields, headers,
  tokenize='unicode61 remove_diacritics 2'
);
CREATE INDEX IF NOT EXISTS capture_search_method_idx ON capture_search(method);
CREATE INDEX IF NOT EXISTS capture_search_host_idx ON capture_search(host);
CREATE INDEX IF NOT EXISTS capture_search_route_idx ON capture_search(route);
CREATE INDEX IF NOT EXISTS capture_search_endpoint_idx ON capture_search(host,port,route);
CREATE INDEX IF NOT EXISTS capture_search_status_idx ON capture_search(response_status);
CREATE VIRTUAL TABLE IF NOT EXISTS capture_search_fts USING fts5(
  observation_id UNINDEXED, method, host, route, fields, headers,
  tokenize='unicode61 remove_diacritics 2'
);
`

// schemaV6 adds the authoritative, per-project projection job ledger. It is
// applied when creating a new database; Python owns upgrades of existing files.
const schemaV6 = schemaV5 + `
CREATE TABLE IF NOT EXISTS capture_revision_registry (
  revision_id TEXT PRIMARY KEY CHECK (length(revision_id) = 64),
  observation_id TEXT NOT NULL,
  source_kind TEXT NOT NULL CHECK (source_kind IN ('historical','live')),
  first_seen_at TEXT NOT NULL,
  UNIQUE (source_kind, observation_id, revision_id)
);
CREATE TABLE IF NOT EXISTS projection_jobs (
  sequence_id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id TEXT NOT NULL UNIQUE,
  revision_id TEXT NOT NULL UNIQUE REFERENCES capture_revision_registry(revision_id),
  payload_json TEXT NOT NULL,
  payload_sha256 TEXT NOT NULL CHECK (length(payload_sha256) = 64),
  state TEXT NOT NULL DEFAULT 'pending'
    CHECK (state IN ('pending','leased','applied','dead_letter')),
  attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
  next_attempt_at TEXT NOT NULL,
  lease_token TEXT,
  lease_until TEXT,
  last_error_code TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  applied_at TEXT,
  CHECK ((state = 'leased' AND lease_token IS NOT NULL AND lease_until IS NOT NULL)
      OR (state <> 'leased' AND lease_token IS NULL AND lease_until IS NULL))
);
CREATE INDEX IF NOT EXISTS projection_jobs_due_idx
  ON projection_jobs(state, next_attempt_at, sequence_id);
`
