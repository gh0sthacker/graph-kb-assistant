package coverage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DatabaseName is relative to the application's private data root.
const DatabaseName = "coverage.sqlite3"

// ErrRunNotFound distinguishes missing coverage from a completed run.
var ErrRunNotFound = errors.New("coverage run not found")

// Store persists coverage cells using the project's existing SQLite driver.
type Store struct{ db *sql.DB }

// Open creates or opens a writable coverage database. New directories and
// database files are private. No extra dependencies or external services are used.
func Open(path string) (*Store, error) { return openDatabase(path, false) }

// OpenReadOnly never creates the database, its directory, or the schema.
func OpenReadOnly(path string) (*Store, error) { return openDatabase(path, true) }

func openDatabase(path string, readOnly bool) (*Store, error) {
	if path == "" {
		return nil, errors.New("coverage database path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !readOnly {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if err == nil {
			if err := file.Close(); err != nil {
				return nil, err
			}
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("coverage database must be a regular file")
	}
	query := url.Values{"_pragma": {"busy_timeout(5000)"}}
	if readOnly {
		query.Set("mode", "ro")
	} else {
		query.Set("_txlock", "immediate")
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(err error) (*Store, error) { _ = db.Close(); return nil, err }
	if err := db.Ping(); err != nil {
		return fail(err)
	}
	if !readOnly {
		_, err = db.Exec(`CREATE TABLE IF NOT EXISTS coverage_cells (
   run TEXT NOT NULL CHECK(length(run) > 0),
   class TEXT NOT NULL CHECK(length(class) > 0),
   status TEXT NOT NULL CHECK(status IN ('confirmed','ruled_out','untested','blocked_first_party')),
   tests INTEGER NOT NULL CHECK(tests >= 0),
   budget INTEGER NOT NULL CHECK(budget >= 0),
   evidence_json TEXT NOT NULL,
   PRIMARY KEY(run, class)
  )`)
		if err != nil {
			return fail(err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Save atomically upserts the supplied cells by (run, class). Other runs and
// classes are preserved. Load the run before updating to retain existing state.
// Empty matrices are rejected: missing coverage must not masquerade as complete.
func (s *Store) Save(ctx context.Context, m *Matrix) error {
	if m == nil || m.Run == "" || len(m.Cells) == 0 {
		return errors.New("nonempty coverage run and cells are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for class, cell := range m.Cells {
		if cell == nil || class == "" || cell.Class != class || !validStatus(cell.Status) || cell.Tests < 0 || cell.Budget < 0 {
			return errors.New("coverage cell is invalid")
		}
		evidence := cell.Evidence
		if evidence == nil {
			evidence = []string{}
		}
		encoded, err := json.Marshal(evidence)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO coverage_cells (run, class, status, tests, budget, evidence_json)
   VALUES (?, ?, ?, ?, ?, ?)
   ON CONFLICT(run, class) DO UPDATE SET status=excluded.status, tests=excluded.tests,
    budget=excluded.budget, evidence_json=excluded.evidence_json`,
			m.Run, class, cell.Status, cell.Tests, cell.Budget, string(encoded))
		if err != nil {
			return fmt.Errorf("save coverage cell: %w", err)
		}
	}
	return tx.Commit()
}

// Load returns all cells for exactly one run, or ErrRunNotFound. It rejects
// corrupt stored evidence instead of silently discarding coverage information.
func (s *Store) Load(ctx context.Context, run string) (*Matrix, error) {
	if run == "" {
		return nil, errors.New("coverage run is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT class, status, tests, budget, evidence_json
  FROM coverage_cells WHERE run = ? ORDER BY class`, run)
	if err != nil {
		return nil, fmt.Errorf("load coverage cells: %w", err)
	}
	defer rows.Close()
	m := &Matrix{Run: run, Cells: make(map[string]*Cell)}
	for rows.Next() {
		var cell Cell
		var encoded string
		if err := rows.Scan(&cell.Class, &cell.Status, &cell.Tests, &cell.Budget, &encoded); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &cell.Evidence); err != nil {
			return nil, fmt.Errorf("decode coverage evidence: %w", err)
		}
		if cell.Class == "" || !validStatus(cell.Status) || cell.Tests < 0 || cell.Budget < 0 {
			return nil, errors.New("stored coverage cell is invalid")
		}
		if cell.Evidence == nil {
			cell.Evidence = []string{}
		}
		m.Cells[cell.Class] = &cell
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(m.Cells) == 0 {
		return nil, ErrRunNotFound
	}
	return m, nil
}
