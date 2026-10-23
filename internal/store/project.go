package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
)

type ProjectStore struct {
	Project             project.CaidoProjectRef
	ProjectKey          string
	Directory           string
	VaultDirectory      string
	BehaviorGraphPath   string
	BehaviorReceiptPath string
	Database            *Store
}

// OpenProjectReadOnly opens an existing v7 project store without creating
// directories, registering a project, or changing SQLite state.
func OpenProjectReadOnly(ctx context.Context, ref project.CaidoProjectRef, dataRoot string) (*ProjectStore, error) {
	return openProject(ctx, ref, dataRoot, true, nil)
}

// OpenProjectReadOnlyUnderMaintenance opens a read-only snapshot while the
// caller already holds the matching exclusive project lease. Keep the lease
// until ProjectStore.Close. It is intended for read/modify workflows that
// must prevent concurrent intake while they operate on a consistent snapshot.
func OpenProjectReadOnlyUnderMaintenance(
	ctx context.Context,
	ref project.CaidoProjectRef,
	dataRoot string,
	maintenance *projectlock.Lock,
) (*ProjectStore, error) {
	if maintenance == nil {
		return nil, errors.New("project maintenance lease is required for this read-only open")
	}
	return openProject(ctx, ref, dataRoot, true, maintenance)
}

// OpenProjectForWrite opens an existing v7 database. It does not migrate or
// initialize the database; use a shadow copy until parity has been established.
func OpenProjectForWrite(ctx context.Context, ref project.CaidoProjectRef, dataRoot string) (*ProjectStore, error) {
	return openProject(ctx, ref, dataRoot, false, nil)
}

// ProjectDirectory resolves an existing private directory for a validated
// project identity. Callers use it to acquire the maintenance lease before
// opening a read-only snapshot for a maintenance operation.
func ProjectDirectory(ref project.CaidoProjectRef, dataRoot string) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", errors.New("project identity is invalid")
	}
	root, err := resolveDataRoot(dataRoot)
	if err != nil {
		return "", err
	}
	if err := rejectSourceCheckoutPath(root); err != nil {
		return "", err
	}
	projectKey, err := ref.StorageKey()
	if err != nil {
		return "", errors.New("project identity is invalid")
	}
	directory := filepath.Join(root, "projects", projectKey)
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("project data directory is unavailable")
	}
	return directory, nil
}

// CreateProject creates a new project namespace and version-7 SQLite database.
// It refuses to overwrite any existing namespace and does not touch Caido.
func CreateProject(ctx context.Context, ref project.CaidoProjectRef, dataRoot string) (*ProjectStore, error) {
	if err := ref.Validate(); err != nil {
		return nil, errors.New("project identity is invalid")
	}
	root, err := resolveDataRoot(dataRoot)
	if err != nil {
		return nil, err
	}
	if err := rejectSourceCheckoutPath(root); err != nil {
		return nil, err
	}
	projectsDirectory, err := project.EnsurePrivateDirectory(filepath.Join(root, "projects"))
	if err != nil {
		return nil, err
	}
	projectKey, err := ref.StorageKey()
	if err != nil {
		return nil, errors.New("project identity is invalid")
	}
	directory, err := project.EnsurePrivateDirectory(filepath.Join(projectsDirectory, projectKey))
	if err != nil {
		return nil, err
	}
	vault, err := project.EnsurePrivateDirectory(filepath.Join(directory, "vault"))
	if err != nil {
		return nil, err
	}
	receipts, err := project.EnsurePrivateDirectory(filepath.Join(directory, "behavior-receipts"))
	if err != nil {
		return nil, err
	}
	databasePath := filepath.Join(directory, "graph.sqlite3")
	file, err := os.OpenFile(databasePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("refusing to overwrite an existing project database")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, errors.New("project database permissions could not be restricted")
	}
	if err := file.Close(); err != nil {
		return nil, errors.New("project database could not be created")
	}
	if err := initializeEmptyDatabase(ctx, databasePath, projectKey); err != nil {
		return nil, err
	}
	database, err := OpenForWrite(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	if err := project.RegisterProject(ref, root); err != nil {
		_ = database.Close()
		return nil, errors.New("project registry could not be updated")
	}
	return &ProjectStore{
		Project: ref, ProjectKey: projectKey, Directory: directory,
		VaultDirectory: vault, BehaviorGraphPath: filepath.Join(directory, "behavior.sqlite3"),
		BehaviorReceiptPath: receipts, Database: database,
	}, nil
}

func openProject(
	ctx context.Context,
	ref project.CaidoProjectRef,
	dataRoot string,
	readonly bool,
	maintenance *projectlock.Lock,
) (*ProjectStore, error) {
	if err := ref.Validate(); err != nil {
		return nil, errors.New("project identity is invalid")
	}
	root, err := resolveDataRoot(dataRoot)
	if err != nil {
		return nil, err
	}
	if err := rejectSourceCheckoutPath(root); err != nil {
		return nil, err
	}
	projectKey, err := ref.StorageKey()
	if err != nil {
		return nil, errors.New("project identity is invalid")
	}
	directory := filepath.Join(root, "projects", projectKey)
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("project data directory is unavailable")
	}
	for _, name := range []string{"vault", "behavior-receipts"} {
		child := filepath.Join(directory, name)
		childInfo, childErr := os.Lstat(child)
		if childErr != nil || childInfo.Mode()&os.ModeSymlink != 0 || !childInfo.IsDir() {
			return nil, errors.New("project data layout is incomplete")
		}
	}
	databasePath := filepath.Join(directory, "graph.sqlite3")
	var database *Store
	if readonly {
		if maintenance != nil {
			database, err = OpenReadOnlyUnderMaintenance(ctx, databasePath, maintenance)
		} else {
			database, err = OpenReadOnly(ctx, databasePath)
		}
	} else {
		database, err = OpenForWrite(ctx, databasePath)
	}
	if err != nil {
		return nil, err
	}
	return &ProjectStore{
		Project: ref, ProjectKey: projectKey, Directory: directory,
		VaultDirectory:      filepath.Join(directory, "vault"),
		BehaviorGraphPath:   filepath.Join(directory, "behavior.sqlite3"),
		BehaviorReceiptPath: filepath.Join(directory, "behavior-receipts"),
		Database:            database,
	}, nil
}

func (s *ProjectStore) Close() error { return s.Database.Close() }

func initializeEmptyDatabase(ctx context.Context, path, projectKey string) error {
	db, err := sql.Open("sqlite", (&urlForSQLite{path: path}).String())
	if err != nil {
		return errors.New("new project database could not be opened")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return errors.New("new project database could not enable foreign keys")
	}
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=30000"); err != nil {
		return errors.New("new project database could not set a busy timeout")
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return errors.New("new project database could not enable WAL mode")
	}
	if _, err := db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		return errors.New("new project database could not set durable writes")
	}
	for _, statement := range strings.Split(schemaV7, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return errors.New("new project database schema could not be initialized")
		}
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", CurrentSchemaVersion)); err != nil {
		return errors.New("new project database version could not be recorded")
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO project_meta(key,value) VALUES('project_key',?)", projectKey,
	); err != nil {
		return errors.New("new project identity could not be recorded")
	}
	return nil
}

type urlForSQLite struct{ path string }

func (u *urlForSQLite) String() string {
	absolute, err := filepath.Abs(u.path)
	if err != nil {
		return u.path
	}
	return "file://" + filepath.ToSlash(absolute)
}

func resolveDataRoot(configured string) (string, error) {
	root := configured
	if root == "" {
		var err error
		root, err = project.DataRoot()
		if err != nil {
			return "", err
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("private data root is invalid")
	}
	return filepath.Clean(root), nil
}

func rejectSourceCheckoutPath(path string) error {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil
	}
	sourceRoot := filepath.Dir(filepath.Dir(filepath.Dir(sourceFile)))
	sourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return errors.New("could not validate private data root")
	}
	rel, err := filepath.Rel(sourceRoot, path)
	if err != nil {
		return errors.New("could not validate private data root")
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("runtime data must remain outside the source checkout")
	}
	return nil
}
