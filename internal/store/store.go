// Package store opens the existing project-scoped SQLite capture database.
// It deliberately separates inspection from writes during the Go migration.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const CurrentSchemaVersion = 5

var ErrUnsupportedSchema = errors.New("unsupported graph database schema")

type Store struct {
	db       *sql.DB
	path     string
	readonly bool
}

// OpenReadOnly inspects an existing database without running migrations,
// creating tables, or changing SQLite pragmas. Use it first on production data.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, true)
}

// OpenForWrite opens an existing, already compatible database. It never
// initializes or upgrades the schema; callers should use a copied database
// until Go write parity has been checked.
func OpenForWrite(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, false)
}

func open(ctx context.Context, path string, readonly bool) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("database path is invalid")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, errors.New("database file is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("database path must be a regular file, not a symbolic link")
	}
	if !readonly && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("writable database permissions must be owner-only")
	}

	fileURL := (&url.URL{Scheme: "file", Path: absolute}).String()
	if readonly {
		fileURL += "?mode=ro"
	}
	db, err := sql.Open("sqlite", fileURL)
	if err != nil {
		return nil, errors.New("database could not be opened")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	store := &Store{db: db, path: absolute, readonly: readonly}
	if err := store.validate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) validate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return errors.New("database schema version could not be read")
	}
	if version != CurrentSchemaVersion {
		return fmt.Errorf("%w: found %d, Go supports %d; database was not changed", ErrUnsupportedSchema, version, CurrentSchemaVersion)
	}
	for _, table := range []string{
		"captures", "capture_revisions", "capture_search", "live_capture_index",
		"live_sync_state", "sync_state", "graph_capture_map",
	} {
		var found int
		err := s.db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", table,
		).Scan(&found)
		if err != nil || found != 1 {
			return errors.New("database is missing a required project table")
		}
	}
	if !s.readonly {
		if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
			return errors.New("database foreign-key mode could not be enabled")
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA busy_timeout=30000"); err != nil {
			return errors.New("database busy timeout could not be configured")
		}
	}
	return nil
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Path() string { return s.path }

func (s *Store) ReadOnly() bool { return s.readonly }

func (s *Store) Close() error { return s.db.Close() }
