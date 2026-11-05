// Package hackerone reads an optional local SQLite FTS5 index of disclosed
// HackerOne reports. The index is reference material only: it is opened
// read-only and never modified by this service.
package hackerone

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

const (
	// SchemaVersion is the only metadata schema this reader understands.
	SchemaVersion = "hackerone-fts-v1"

	defaultSearchLimit = 10
	maximumSearchLimit = 200
	maximumQueryRunes  = 4096
)

var (
	// ErrIndexUnavailable reports that no usable HackerOne index is open.
	ErrIndexUnavailable = errors.New("HackerOne index is unavailable")
	// ErrInvalidQuery reports a query that has no searchable terms.
	ErrInvalidQuery = errors.New("HackerOne query must contain searchable terms")
	// ErrSearchFailed reports a failed read against an open index.
	ErrSearchFailed = errors.New("HackerOne index search failed")
)

var snapshotPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Meta is the validated metadata row set written by the index builder.
type Meta struct {
	SchemaVersion string
	DatasetCommit string
	BuiltAt       string
	ReportCount   int64
	WithBodyCount int64
	SnapshotID    string
}

// Index is a read-only handle to a HackerOne FTS5 index.
type Index struct {
	db   *sql.DB
	meta Meta
	path string
}

// Hit is one ranked HackerOne search result. Bounty and Upvotes are pointers
// because the source reports may not carry either value.
type Hit struct {
	ReportID    string
	Program     string
	Title       string
	URL         string
	VulnType    string
	Severity    string
	Weakness    string
	Bounty      *float64
	Upvotes     *int
	DisclosedAt string
	HasBody     bool
	Excerpt     string
	BM25        float64
}

const searchSQL = `SELECT r.report_id,COALESCE(r.program,''),COALESCE(r.title,''),COALESCE(r.url,''),
	COALESCE(r.vuln_type,''),COALESCE(r.severity,''),COALESCE(r.weakness,''),r.bounty,r.upvotes,
	COALESCE(r.disclosed_at,''),COALESCE(r.has_body,0),
	COALESCE(snippet(reports_fts,1,'','','…',40),'') AS excerpt,
	bm25(reports_fts,8.0,1.0,2.0,3.0,2.0) AS score
	FROM reports_fts JOIN reports r ON r.rowid=reports_fts.rowid
	WHERE reports_fts MATCH ? ORDER BY score LIMIT ?`

// Open opens an existing HackerOne index read-only. An empty path is a no-op
// that returns a nil index and no error, so callers can treat the supplemental
// index as genuinely optional.
func Open(path string) (*Index, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, errors.New("HackerOne index path is invalid")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("HackerOne index must be an existing regular file")
	}
	// mode=ro keeps the index immutable from this process. immutable=1 is
	// deliberately not used because the builder may leave the database in WAL
	// mode, where immutable readers can observe stale or incomplete content.
	fileURL := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro"
	db, err := sql.Open("sqlite", fileURL)
	if err != nil {
		return nil, errors.New("HackerOne index could not be opened read-only")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	meta, err := loadMeta(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Index{db: db, meta: meta, path: absolute}, nil
}

func loadMeta(ctx context.Context, db *sql.DB) (Meta, error) {
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type IN ('table','view') AND name IN ('meta','reports','reports_fts')`).Scan(&tables); err != nil || tables != 3 {
		return Meta{}, errors.New("HackerOne index schema is incomplete")
	}
	rows, err := db.QueryContext(ctx, `SELECT key,value FROM meta`)
	if err != nil {
		return Meta{}, errors.New("HackerOne index metadata is unavailable")
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return Meta{}, errors.New("HackerOne index metadata is malformed")
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return Meta{}, errors.New("HackerOne index metadata is malformed")
	}
	for _, required := range []string{"schema_version", "dataset_commit", "built_at", "report_count", "with_body_count", "snapshot_id"} {
		if _, ok := values[required]; !ok {
			return Meta{}, errors.New("HackerOne index metadata is incomplete")
		}
	}
	if values["schema_version"] != SchemaVersion {
		return Meta{}, errors.New("HackerOne index schema version is unsupported")
	}
	if !snapshotPattern.MatchString(values["snapshot_id"]) {
		return Meta{}, errors.New("HackerOne index snapshot identity is invalid")
	}
	reportCount, err := strconv.ParseInt(strings.TrimSpace(values["report_count"]), 10, 64)
	if err != nil || reportCount < 0 {
		return Meta{}, errors.New("HackerOne index report count is invalid")
	}
	withBodyCount, err := strconv.ParseInt(strings.TrimSpace(values["with_body_count"]), 10, 64)
	if err != nil || withBodyCount < 0 || withBodyCount > reportCount {
		return Meta{}, errors.New("HackerOne index body count is invalid")
	}
	return Meta{
		SchemaVersion: values["schema_version"],
		DatasetCommit: values["dataset_commit"],
		BuiltAt:       values["built_at"],
		ReportCount:   reportCount,
		WithBodyCount: withBodyCount,
		SnapshotID:    values["snapshot_id"],
	}, nil
}

// Meta returns validated index metadata, or a zero value for a nil index.
func (i *Index) Meta() Meta {
	if i == nil {
		return Meta{}
	}
	return i.meta
}

// Close releases the read-only handle. It is safe on a nil index.
func (i *Index) Close() error {
	if i == nil || i.db == nil {
		return nil
	}
	db := i.db
	i.db = nil
	return db.Close()
}

// Search runs one bounded FTS5 query. Every user token is quoted before it is
// placed in the MATCH expression so operators and punctuation cannot change
// the query grammar; explicit quoted phrases keep phrase semantics.
func (i *Index) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	if i == nil || i.db == nil {
		return nil, ErrIndexUnavailable
	}
	expression := buildMatchExpression(query)
	if expression == "" {
		return nil, ErrInvalidQuery
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maximumSearchLimit {
		limit = maximumSearchLimit
	}
	rows, err := i.db.QueryContext(ctx, searchSQL, expression, limit)
	if err != nil {
		return nil, ErrSearchFailed
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit)
	for rows.Next() {
		var hit Hit
		var bounty sql.NullFloat64
		var upvotes sql.NullInt64
		if err := rows.Scan(&hit.ReportID, &hit.Program, &hit.Title, &hit.URL, &hit.VulnType,
			&hit.Severity, &hit.Weakness, &bounty, &upvotes, &hit.DisclosedAt, &hit.HasBody,
			&hit.Excerpt, &hit.BM25); err != nil {
			return nil, ErrSearchFailed
		}
		if bounty.Valid {
			value := bounty.Float64
			hit.Bounty = &value
		}
		if upvotes.Valid {
			value := int(upvotes.Int64)
			hit.Upvotes = &value
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrSearchFailed
	}
	return hits, nil
}

func buildMatchExpression(query string) string {
	runes := []rune(query)
	if len(runes) > maximumQueryRunes {
		runes = runes[:maximumQueryRunes]
	}
	terms := make([]string, 0, 8)
	for index := 0; index < len(runes); {
		for index < len(runes) && unicode.IsSpace(runes[index]) {
			index++
		}
		if index >= len(runes) {
			break
		}
		if runes[index] == '"' {
			index++
			var phrase strings.Builder
			for index < len(runes) {
				if runes[index] == '"' {
					if index+1 < len(runes) && runes[index+1] == '"' {
						phrase.WriteRune('"')
						index += 2
						continue
					}
					index++
					break
				}
				phrase.WriteRune(runes[index])
				index++
			}
			value := strings.TrimSpace(phrase.String())
			if value == "" {
				continue
			}
			terms = append(terms, quoteToken(value))
			continue
		}
		start := index
		for index < len(runes) && !unicode.IsSpace(runes[index]) {
			index++
		}
		terms = append(terms, quoteToken(string(runes[start:index])))
	}
	return strings.Join(terms, " ")
}

func quoteToken(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
