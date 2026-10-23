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

	"bb-graph-assistant/internal/projectlock"

	_ "modernc.org/sqlite"
)

const CurrentSchemaVersion = 6

var ErrUnsupportedSchema = errors.New("unsupported graph database schema")

type Store struct {
	db        *sql.DB
	path      string
	readonly  bool
	readLease *projectlock.Lock
}

// OpenReadOnly inspects an existing database without running migrations,
// creating tables, or changing SQLite pragmas. Use it first on production data.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, true, true)
}

// OpenReadOnlyUnderMaintenance opens a project database read-only when the
// caller already holds that project's exclusive maintenance lease. The caller
// must keep the lease until the returned Store is closed.
func OpenReadOnlyUnderMaintenance(ctx context.Context, path string, maintenance *projectlock.Lock) (*Store, error) {
	if maintenance == nil {
		return nil, errors.New("project maintenance lease is required for this read-only open")
	}
	absolute, err := filepath.Abs(path)
	if err != nil || !maintenance.CoversDirectory(filepath.Dir(absolute)) {
		return nil, errors.New("project maintenance lease does not match the database directory")
	}
	return open(ctx, path, true, false)
}

// OpenForWrite opens an existing, already compatible database. It never
// initializes or upgrades the schema; callers should use a copied database
// until Go write parity has been checked.
func OpenForWrite(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, false, false)
}

func open(ctx context.Context, path string, readonly bool, acquireReadLease bool) (_ *Store, retErr error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("database path is invalid")
	}
	var readLease *projectlock.Lock
	if readonly && acquireReadLease {
		readLease, err = projectlock.AcquireRead(ctx, filepath.Dir(absolute))
		if err != nil {
			return nil, errors.New("project read lease could not be acquired")
		}
		defer func() {
			if readLease != nil {
				retErr = errors.Join(retErr, readLease.Close())
			}
		}()
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
	if readonly {
		if err := rejectSQLiteSidecars(absolute); err != nil {
			return nil, err
		}
	}

	fileURL := (&url.URL{Scheme: "file", Path: absolute}).String()
	if readonly {
		fileURL += "?mode=ro&immutable=1"
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
		return nil, errors.Join(err, db.Close())
	}
	store.readLease = readLease
	readLease = nil
	return store, nil
}

func rejectSQLiteSidecars(databasePath string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Lstat(databasePath + suffix)
		if err == nil {
			return errors.New("database has a SQLite sidecar; read-only open refused")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("SQLite sidecar state could not be checked; read-only open refused")
		}
	}
	return nil
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
		"capture_revision_registry", "projection_jobs",
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

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var databaseErr, leaseErr error
	if s.db != nil {
		databaseErr = s.db.Close()
	}
	if s.readLease != nil {
		leaseErr = s.readLease.Close()
	}
	return errors.Join(databaseErr, leaseErr)
}
