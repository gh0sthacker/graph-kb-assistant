package hunt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const DatabaseName = "hunt.sqlite3"
const schema = `
 CREATE TABLE observations(capture_id TEXT PRIMARY KEY, observation_id TEXT NOT NULL, revision_id TEXT NOT NULL, payload_json TEXT NOT NULL);
 CREATE TABLE endpoints(id TEXT PRIMARY KEY,method TEXT NOT NULL,route_template TEXT NOT NULL,host TEXT NOT NULL,port INTEGER NOT NULL,auth_required INTEGER NOT NULL,state_changing INTEGER NOT NULL,payload_json TEXT NOT NULL);
 CREATE TABLE parameters(id TEXT PRIMARY KEY,endpoint_id TEXT NOT NULL,name TEXT NOT NULL,location TEXT NOT NULL CHECK(location IN ('query','body','header','path','hash')),payload_json TEXT NOT NULL);
 CREATE TABLE sinks(id TEXT PRIMARY KEY,kind TEXT NOT NULL,confidence REAL NOT NULL CHECK(confidence BETWEEN 0 AND 1),payload_json TEXT NOT NULL);
 CREATE TABLE identities(id TEXT PRIMARY KEY,label TEXT NOT NULL,token_ref TEXT NOT NULL,payload_json TEXT NOT NULL);
 CREATE TABLE infos(id TEXT PRIMARY KEY,kind TEXT NOT NULL,redacted INTEGER NOT NULL CHECK(redacted=1),payload_json TEXT NOT NULL);
 CREATE TABLE edges(candidate_id TEXT NOT NULL,"from" TEXT NOT NULL,"to" TEXT NOT NULL,kind TEXT NOT NULL,status INTEGER NOT NULL,evidence_json TEXT NOT NULL CHECK(json_array_length(evidence_json)>0),PRIMARY KEY(candidate_id,"from","to",kind,status));
 CREATE TABLE candidates(id TEXT PRIMARY KEY,class TEXT NOT NULL,score REAL NOT NULL,why_json TEXT NOT NULL CHECK(json_array_length(why_json)>0),payload_json TEXT NOT NULL);
 CREATE INDEX candidate_rank ON candidates(score DESC,id);
 PRAGMA user_version=1;`

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error)         { return open(path, false) }
func OpenReadOnly(path string) (*Store, error) { return open(path, true) }
func open(path string, readOnly bool) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil || path == "" {
		return nil, errors.New("hunt database path is required")
	}
	if !readOnly {
		f, e := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil && !errors.Is(e, os.ErrExist) {
			return nil, e
		}
		if e == nil {
			if e = f.Close(); e != nil {
				return nil, e
			}
		}
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("hunt database must be a private regular file")
	}
	q := url.Values{"_pragma": {"busy_timeout(5000)"}}
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Set("_txlock", "immediate")
	}
	uri := url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(e error) (*Store, error) { db.Close(); return nil, e }
	var v int
	if err = db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fail(err)
	}
	if v == 0 && !readOnly {
		tx, e := db.Begin()
		if e != nil {
			return fail(e)
		}
		if _, e = tx.Exec(schema); e != nil {
			tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
		v = 1
	}
	if v != 1 {
		return fail(errors.New("unsupported hunt schema version"))
	}
	return &Store{db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func loadObservations(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]Observation, error) {
	rows, err := q.QueryContext(ctx, "SELECT payload_json FROM observations ORDER BY capture_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	obs := []Observation{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var o Observation
		if err = json.Unmarshal([]byte(raw), &o); err != nil {
			return nil, err
		}
		obs = append(obs, o)
	}
	return obs, rows.Err()
}
func (s *Store) Observations(ctx context.Context) ([]Observation, error) {
	return loadObservations(ctx, s.db)
}
func (s *Store) Load(ctx context.Context) (Graph, error) {
	obs, err := s.Observations(ctx)
	if err != nil {
		return Graph{}, err
	}
	return Build(obs), nil
}

// Project atomically replaces the derived graph after upserting current
// capture observations. Replaying a page never inflates distinct counts.
// This independent cache is recoverable from captures; it never writes them.
func (s *Store) Project(ctx context.Context, observations []Observation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, o := range observations {
		if len(evidence(o)) == 0 || o.Endpoint.ID == "" {
			continue
		}
		b, e := json.Marshal(o)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO observations VALUES(?,?,?,?) ON CONFLICT(capture_id) DO UPDATE SET observation_id=excluded.observation_id,revision_id=excluded.revision_id,payload_json=excluded.payload_json`, o.CaptureID, o.ObservationID, o.RevisionID, string(b)); e != nil {
			return e
		}
	}
	obs, err := loadObservations(ctx, tx)
	if err != nil {
		return err
	}
	g := Build(obs)
	for _, name := range []string{"endpoints", "parameters", "sinks", "identities", "infos", "edges", "candidates"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+name); err != nil {
			return err
		}
	}
	insert := func(query string, value any, args ...any) error {
		b, e := json.Marshal(value)
		if e != nil {
			return e
		}
		args = append(args, string(b))
		_, e = tx.ExecContext(ctx, query, args...)
		return e
	}
	for _, v := range g.Endpoints {
		if err = insert("INSERT INTO endpoints VALUES(?,?,?,?,?,?,?,?)", v, v.ID, v.Method, v.Route, v.Host, v.Port, v.AuthRequired, v.StateChanging); err != nil {
			return err
		}
	}
	for _, v := range g.Parameters {
		if err = insert("INSERT INTO parameters VALUES(?,?,?,?,?)", v, v.ID, v.EndpointID, v.Name, v.Location); err != nil {
			return err
		}
	}
	for _, v := range g.Sinks {
		if err = insert("INSERT INTO sinks VALUES(?,?,?,?)", v, v.ID, v.Kind, v.Confidence); err != nil {
			return err
		}
	}
	for _, v := range g.Identities {
		if err = insert("INSERT INTO identities VALUES(?,?,?,?)", v, v.ID, v.Label, v.TokenRef); err != nil {
			return err
		}
	}
	for _, v := range g.Infos {
		if err = insert("INSERT INTO infos VALUES(?,?,?,?)", v, v.ID, v.Kind, v.Redacted); err != nil {
			return err
		}
	}
	for _, v := range g.Edges {
		if err = insert(`INSERT INTO edges VALUES(?,?,?,?,?,?)`, v.Evidence, v.CandidateID, v.From, v.To, v.Kind, v.Status); err != nil {
			return err
		}
	}
	for _, v := range g.Result.Candidates {
		why, _ := json.Marshal(v.Why)
		if err = insert("INSERT INTO candidates VALUES(?,?,?,?,?)", v, v.ID, v.Class, v.Score, string(why)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
