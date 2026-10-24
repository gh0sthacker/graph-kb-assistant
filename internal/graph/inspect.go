package graph

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

var (
	ErrUnsupportedDatabase = errors.New("BehaviorGraph database is incompatible")
	ErrDatabaseMissing     = errors.New("BehaviorGraph database is missing")
)

// DatabaseStats contains structural metadata only. InspectReadOnly never reads
// entity, assertion, source, or card payloads.
type DatabaseStats struct {
	SchemaVersion  int   `json:"schema_version"`
	EntityCount    int64 `json:"entity_count"`
	AssertionCount int64 `json:"assertion_count"`
	SourceCount    int64 `json:"source_count"`
	CardCount      int64 `json:"card_count"`
	SourceRefCount int64 `json:"source_ref_count"`
}

// InspectReadOnly checks the supported BehaviorGraph schema and returns only
// bounded aggregate table counts. It opens SQLite with mode=ro, does not load
// row payloads or perform migrations, and limits each table count query to
// maxGraphRows+1 rows rather than treating the database file size as a schema
// compatibility limit.
func InspectReadOnly(ctx context.Context, path, program string) (DatabaseStats, error) {
	if !graphDigestPattern.MatchString(program) {
		return DatabaseStats{}, errors.New("selected graph project key is invalid")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return DatabaseStats{}, errors.New("BehaviorGraph database path is invalid")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DatabaseStats{}, ErrDatabaseMissing
		}
		return DatabaseStats{}, errors.New("BehaviorGraph database is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return DatabaseStats{}, errors.New("BehaviorGraph database is unavailable")
	}
	dbURL := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro"
	db, err := sql.Open("sqlite", dbURL)
	if err != nil {
		return DatabaseStats{}, errors.New("BehaviorGraph database could not be opened")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if !hasColumns(ctx, db, "metadata", "key", "value") {
		return DatabaseStats{}, ErrUnsupportedDatabase
	}
	metadata, err := db.QueryContext(ctx, "SELECT key,value FROM metadata LIMIT 3")
	if err != nil {
		return DatabaseStats{}, ErrUnsupportedDatabase
	}
	values := make(map[string]string, 2)
	for metadata.Next() {
		var key, value string
		if err := metadata.Scan(&key, &value); err != nil {
			_ = metadata.Close()
			return DatabaseStats{}, ErrUnsupportedDatabase
		}
		if key != "schema_version" && key != "program" {
			_ = metadata.Close()
			return DatabaseStats{}, ErrUnsupportedDatabase
		}
		if _, exists := values[key]; exists {
			_ = metadata.Close()
			return DatabaseStats{}, ErrUnsupportedDatabase
		}
		values[key] = value
	}
	if err := metadata.Err(); err != nil {
		_ = metadata.Close()
		return DatabaseStats{}, ErrUnsupportedDatabase
	}
	if err := metadata.Close(); err != nil || len(values) != 2 || values["program"] != program {
		return DatabaseStats{}, ErrUnsupportedDatabase
	}
	schemaVersion, err := strconv.Atoi(values["schema_version"])
	if err != nil || (schemaVersion != 1 && schemaVersion != 2) {
		return DatabaseStats{}, ErrUnsupportedDatabase
	}
	stats := DatabaseStats{SchemaVersion: schemaVersion}
	tables := []struct {
		name    string
		columns []string
		count   *int64
	}{
		{name: "entities", columns: []string{"id", "payload"}, count: &stats.EntityCount},
		{name: "assertions", columns: []string{"id", "payload"}, count: &stats.AssertionCount},
		{name: "sources", columns: []string{"id", "payload"}, count: &stats.SourceCount},
		{name: "cards", columns: []string{"id", "payload"}, count: &stats.CardCount},
	}
	if schemaVersion == 2 {
		tables = append(tables, struct {
			name    string
			columns []string
			count   *int64
		}{name: "imports", columns: []string{"id"}})
		tables = append(tables, struct {
			name    string
			columns []string
			count   *int64
		}{name: "entity_source_refs", columns: []string{"entity_id", "source_ref_sha256", "payload"}, count: &stats.SourceRefCount})
	}
	for _, table := range tables {
		if !hasColumns(ctx, db, table.name, table.columns...) {
			return DatabaseStats{}, ErrUnsupportedDatabase
		}
		if table.count == nil {
			continue
		}
		boundedCount := "SELECT count(*) FROM (SELECT 1 FROM " + table.name + " LIMIT ?)"
		if err := db.QueryRowContext(ctx, boundedCount, maxGraphRows+1).Scan(table.count); err != nil {
			return DatabaseStats{}, ErrUnsupportedDatabase
		}
		if *table.count > maxGraphRows {
			return DatabaseStats{}, ErrUnsupportedDatabase
		}
	}
	return stats, nil
}

func hasColumns(ctx context.Context, db *sql.DB, table string, required ...string) bool {
	var found int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", table,
	).Scan(&found); err != nil || found != 1 {
		return false
	}
	// Table names come only from InspectReadOnly's fixed allowlist.
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false
	}
	defer rows.Close()
	columns := make(map[string]struct{}, len(required))
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false
		}
		columns[name] = struct{}{}
	}
	if rows.Err() != nil {
		return false
	}
	for _, name := range required {
		if _, ok := columns[name]; !ok {
			return false
		}
	}
	return true
}
