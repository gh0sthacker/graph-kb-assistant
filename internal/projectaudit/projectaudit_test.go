package projectaudit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestAuditProjectRequiresRegisteredProject(t *testing.T) {
	_, err := AuditProject(context.Background(), t.TempDir(), strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("AuditProject error = %v; want registry error", err)
	}
}

func TestAuditProjectReportsCompatibleStoresAndPartialCoverage(t *testing.T) {
	root, ref, key := newAuditFixture(t, true)
	insertCapture(t, root, ref, key)

	report, err := AuditProject(context.Background(), root, key)
	if err != nil {
		t.Fatalf("AuditProject() error = %v", err)
	}
	if report.CaptureStore.State != "compatible" || report.CaptureStore.SchemaVersion != store.CurrentSchemaVersion {
		t.Errorf("capture store = %+v; want compatible schema %d", report.CaptureStore, store.CurrentSchemaVersion)
	}
	if report.BehaviorGraph.State != "compatible" || report.BehaviorGraph.SchemaVersion != 2 {
		t.Errorf("BehaviorGraph = %+v; want compatible schema 2", report.BehaviorGraph)
	}
	if report.Coverage.CaptureCount != 1 || report.Coverage.UnmappedCaptureCount != 1 {
		t.Errorf("coverage = %+v; want one unmapped capture", report.Coverage)
	}
	if report.Coverage.HistoricalSyncState != "done" {
		t.Errorf("historical sync state = %q; want done checkpoint", report.Coverage.HistoricalSyncState)
	}
	if report.CaptureStore.RevisionCount != 1 || report.RequestWireFidelity.ReconstructedText != 1 || report.ResponseWireFidelity.Unknown != 1 {
		t.Errorf("fidelity summary = capture:%+v request:%+v response:%+v", report.CaptureStore, report.RequestWireFidelity, report.ResponseWireFidelity)
	}
	if report.OverallState != "partial" || report.HistoryReconciled || report.Coverage.CoverageState != "partial" {
		t.Errorf("audit overstated completeness: %+v", report)
	}
}

func TestAuditProjectReportsProjectionQueueCountsWithoutPayloads(t *testing.T) {
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "queue-audit-instance", ProjectID: "queue-audit-project"}
	projectStore, err := store.CreateProject(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	key := projectStore.ProjectKey
	for index, state := range []string{"pending", "pending", "leased", "applied", "applied", "applied", "dead_letter"} {
		revisionID := strings.Repeat(strconv.FormatInt(int64(index+1), 16), 64)
		if _, err := projectStore.Database.DB().ExecContext(context.Background(), `
			INSERT INTO capture_revision_registry(revision_id,observation_id,source_kind,first_seen_at)
			VALUES(?,?,?,?)`, revisionID, "observation-"+strconv.Itoa(index), "historical", "2026-10-23T00:00:00Z"); err != nil {
			_ = projectStore.Close()
			t.Fatalf("insert revision registry row: %v", err)
		}
		var leaseToken, leaseUntil, appliedAt any
		if state == "leased" {
			leaseToken, leaseUntil = strings.Repeat("a", 32), "2026-10-23T00:01:00Z"
		}
		if state == "applied" {
			appliedAt = "2026-10-23T00:01:00Z"
		}
		if _, err := projectStore.Database.DB().ExecContext(context.Background(), `
			INSERT INTO projection_jobs(job_id,revision_id,payload_json,payload_sha256,state,attempt_count,
			 next_attempt_at,lease_token,lease_until,created_at,updated_at,applied_at)
			VALUES(?,?,?,? ,?,0,?,?,?,?,?,?)`,
			"job-"+strconv.Itoa(index), revisionID, `{"private":"payload-must-not-be-returned"}`, strings.Repeat("b", 64),
			state, "2026-10-23T00:00:00Z", leaseToken, leaseUntil, "2026-10-23T00:00:00Z", "2026-10-23T00:00:00Z", appliedAt,
		); err != nil {
			_ = projectStore.Close()
			t.Fatalf("insert projection job: %v", err)
		}
	}
	if err := projectStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	report, err := AuditProject(context.Background(), root, key)
	if err != nil {
		t.Fatalf("AuditProject() error = %v", err)
	}
	want := ProjectionQueueStatus{State: "compatible", Pending: 2, Leased: 1, Applied: 3, DeadLetter: 1}
	if report.ProjectionQueue != want {
		t.Fatalf("projection queue = %+v, want %+v", report.ProjectionQueue, want)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal audit report: %v", err)
	}
	if strings.Contains(string(encoded), "payload-must-not-be-returned") {
		t.Fatal("audit report exposed projection payload content")
	}
}

func TestAuditProjectReportsUnsupportedCaptureSchemaWithoutMutation(t *testing.T) {
	root, ref, key := newAuditFixture(t, false)
	setCaptureSchemaVersion(t, root, ref, 99)
	databasePath := filepath.Join(root, "projects", key, "graph.sqlite3")
	before := mustModTime(t, databasePath)

	report, err := AuditProject(context.Background(), root, key)
	if err != nil {
		t.Fatalf("AuditProject() error = %v", err)
	}
	if report.CaptureStore.State != "incompatible" || report.OverallState != "partial" {
		t.Errorf("audit = %+v; want incompatible capture store and partial overall state", report)
	}
	if got := mustModTime(t, databasePath); !got.Equal(before) {
		t.Errorf("capture database mtime changed during read-only audit: before %s after %s", before, got)
	}
}

func TestAuditProjectReportsUnsupportedBehaviorGraphSchema(t *testing.T) {
	root, _, key := newAuditFixture(t, false)
	createBehaviorFixture(t, filepath.Join(root, "projects", key, "behavior.sqlite3"), key, 99)

	report, err := AuditProject(context.Background(), root, key)
	if err != nil {
		t.Fatalf("AuditProject() error = %v", err)
	}
	if report.CaptureStore.State != "compatible" || report.BehaviorGraph.State != "incompatible" {
		t.Errorf("audit = %+v; want compatible capture store and incompatible BehaviorGraph", report)
	}
	if report.OverallState != "partial" || report.HistoryReconciled {
		t.Errorf("audit overstated completeness: %+v", report)
	}
}

func TestAuditProjectRejectsIncompleteBehaviorGraphSchema(t *testing.T) {
	mutations := []struct {
		name  string
		query string
	}{
		{name: "missing v2 imports table", query: "DROP TABLE imports"},
		{name: "missing payload column", query: "DROP TABLE sources; CREATE TABLE sources (id TEXT PRIMARY KEY)"},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			root, _, key := newAuditFixture(t, false)
			behaviorPath := filepath.Join(root, "projects", key, "behavior.sqlite3")
			createBehaviorFixture(t, behaviorPath, key, 2)
			db, err := sql.Open("sqlite", behaviorPath)
			if err != nil {
				t.Fatalf("open BehaviorGraph fixture: %v", err)
			}
			if _, err := db.Exec(mutation.query); err != nil {
				_ = db.Close()
				t.Fatalf("make BehaviorGraph fixture incomplete: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close BehaviorGraph fixture: %v", err)
			}

			report, err := AuditProject(context.Background(), root, key)
			if err != nil {
				t.Fatalf("AuditProject() error = %v", err)
			}
			if report.BehaviorGraph.State != "incompatible" {
				t.Errorf("BehaviorGraph state = %q; want incompatible", report.BehaviorGraph.State)
			}
		})
	}
}

func TestAuditProjectDoesNotModifyCompatibleDatabases(t *testing.T) {
	root, _, key := newAuditFixture(t, true)
	capturePath := filepath.Join(root, "projects", key, "graph.sqlite3")
	behaviorPath := filepath.Join(root, "projects", key, "behavior.sqlite3")
	captureBefore := mustSHA256(t, capturePath)
	behaviorBefore := mustSHA256(t, behaviorPath)

	if _, err := AuditProject(context.Background(), root, key); err != nil {
		t.Fatalf("AuditProject() error = %v", err)
	}
	if got := mustSHA256(t, capturePath); got != captureBefore {
		t.Errorf("capture database bytes changed: before %x after %x", captureBefore, got)
	}
	if got := mustSHA256(t, behaviorPath); got != behaviorBefore {
		t.Errorf("BehaviorGraph bytes changed: before %x after %x", behaviorBefore, got)
	}
}

func newAuditFixture(t *testing.T, withBehaviorGraph bool) (string, project.CaidoProjectRef, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectStore, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	key := projectStore.ProjectKey
	if err := projectStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if withBehaviorGraph {
		createBehaviorFixture(t, filepath.Join(root, "projects", key, "behavior.sqlite3"), key, 2)
	}
	return root, ref, key
}

func createBehaviorFixture(t *testing.T, path, key string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open BehaviorGraph fixture: %v", err)
	}
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE TABLE sources (id TEXT PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE entities (id TEXT PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE assertions (id TEXT PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE cards (id TEXT PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE imports (id TEXT PRIMARY KEY)",
		"CREATE TABLE entity_source_refs (entity_id TEXT NOT NULL, source_ref_sha256 TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(entity_id,source_ref_sha256))",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create BehaviorGraph fixture schema: %v", err)
		}
	}
	if _, err := db.Exec("INSERT INTO metadata(key,value) VALUES('schema_version',?),('program',?)", version, key); err != nil {
		t.Fatalf("insert BehaviorGraph fixture metadata: %v", err)
	}
}

func insertCapture(t *testing.T, root string, ref project.CaidoProjectRef, key string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "projects", key, "graph.sqlite3"))
	if err != nil {
		t.Fatalf("open capture fixture: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO captures (
		observation_id,caido_instance_id,caido_project_id,request_id,request_sha256,
		request_size,request_vault_ref,parse_state
	) VALUES ('observation-fixture',?,?,?,'request-digest',1,'vault://request','partial')`, ref.InstanceID, ref.ProjectID, "request-fixture")
	if err != nil {
		t.Fatalf("insert capture fixture: %v", err)
	}
	_, err = db.Exec(`INSERT INTO capture_revisions (
		revision_id,observation_id,request_sha256,request_size,request_vault_ref,
		request_wire_fidelity,response_wire_fidelity,first_seen_at
	) VALUES ('revision-fixture','observation-fixture','request-digest',1,'vault://request','reconstructed_text','unknown','fixture-time')`)
	if err != nil {
		t.Fatalf("insert capture revision fixture: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sync_state (source_key,state,updated_at)
		VALUES ('caido-http-history-v1','done','fixture-time')`)
	if err != nil {
		t.Fatalf("insert completed sync checkpoint fixture: %v", err)
	}
}

func setCaptureSchemaVersion(t *testing.T, root string, ref project.CaidoProjectRef, version int) {
	t.Helper()
	projectStore, err := store.OpenProjectForWrite(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("open writable capture fixture: %v", err)
	}
	defer projectStore.Close()
	// Fixture-only version mutation. Production audit code must never use this path.
	if _, err := projectStore.Database.DB().Exec("PRAGMA user_version=" + strconv.Itoa(version)); err != nil {
		t.Fatalf("set unsupported schema version: %v", err)
	}
}

func mustModTime(t *testing.T, path string) (gotTime time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", filepath.Base(path), err)
	}
	return info.ModTime()
}

func mustSHA256(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture database: %v", err)
	}
	return sha256.Sum256(data)
}
