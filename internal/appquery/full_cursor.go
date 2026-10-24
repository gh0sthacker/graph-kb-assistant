package appquery

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"database/sql"

	_ "modernc.org/sqlite"
)

const (
	fullCursorTTL                    = 15 * time.Minute
	maxFullCursorSessions            = 16
	maxFullCursorPendingBranches     = 5_000
	maxFullCursorProcessedBranches   = 5_000
	maxFullCursorBranchAttempts      = 4
	fullCursorPageSeeds              = 20
	maxFullCursorReplayPayloadBytes  = 3 << 20
	maxFullCursorSessionPayloadBytes = 8 << 20
	maxPersistedCursorPayloadBytes   = maxFullCursorSessionPayloadBytes + maxFullCursorReplayPayloadBytes + 1<<20
)

var fullCursorTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type fullCursorBinding struct {
	ProjectKey  string
	SeedHash    string
	ContextHash string
	Budget      GraphBudget
}

type fullCursorLoader func([]string, map[string]any, int) (map[string]any, []string, int64, error)
type fullCursorFinalizer func(map[string]any) (map[string]any, error)

type fullCursorSession struct {
	id                  string
	mu                  sync.Mutex
	binding             fullCursorBinding
	generation          int64
	expiresAt           time.Time
	pending             []string
	branchListTruncated bool
	queueTruncated      bool
	queueDropped        int
	attempts            map[string]int
	processed           int
	cumulative          map[string]any
	currentToken        string
	previousToken       string
	previousResult      map[string]any
	page                int
	complete            bool
	activeCounted       bool
}

type fullCursorStore struct {
	mu       sync.Mutex
	sessions map[string]*fullCursorSession
	active   int
	now      func() time.Time
	db       *sql.DB
}

func newFullCursorStore() *fullCursorStore {
	return &fullCursorStore{sessions: make(map[string]*fullCursorSession), now: time.Now}
}

func openPersistentFullCursorStore(stateDir string) (*fullCursorStore, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("full graph cursor state directory is required")
	}
	absolute, err := filepath.Abs(filepath.Clean(stateDir))
	if err != nil {
		return nil, errors.New("full graph cursor state directory is invalid")
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, errors.New("full graph cursor state directory could not be created")
	}
	dirInfo, err := os.Lstat(absolute)
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return nil, errors.New("full graph cursor state directory must be a real directory")
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, errors.New("full graph cursor state directory permissions could not be secured")
	}

	path := filepath.Join(absolute, "full-cursors.sqlite3")
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.New("full graph cursor state file must be a regular file")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, errors.New("full graph cursor state file permissions could not be secured")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, errors.New("full graph cursor state file could not be inspected")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("full graph cursor state file could not be created")
	}
	if err := file.Close(); err != nil {
		return nil, errors.New("full graph cursor state file could not be initialized")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("full graph cursor state file is not a regular file")
	}

	fileURL := (&url.URL{Scheme: "file", Path: path}).String() + "?_txlock=immediate"
	db, err := sql.Open("sqlite", fileURL)
	if err != nil {
		return nil, errors.New("full graph cursor state database could not be opened")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	fail := func() (*fullCursorStore, error) {
		_ = db.Close()
		return nil, errors.New("full graph cursor state database could not be initialized")
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		return fail()
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return fail()
	}
	if _, err := db.Exec("PRAGMA secure_delete = ON"); err != nil {
		return fail()
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version > 1 {
		return fail()
	}
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS full_cursor_sessions (session_id TEXT PRIMARY KEY, state_json BLOB NOT NULL, expires_at_ns INTEGER NOT NULL, active INTEGER NOT NULL CHECK (active IN (0, 1))); " +
		"CREATE TABLE IF NOT EXISTS full_cursor_tokens (token TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES full_cursor_sessions(session_id) ON DELETE CASCADE); " +
		"CREATE INDEX IF NOT EXISTS full_cursor_sessions_expiry_idx ON full_cursor_sessions(expires_at_ns)"); err != nil {
		return fail()
	}
	if version == 0 {
		if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
			return fail()
		}
	}
	if _, err := db.Exec("DELETE FROM full_cursor_sessions WHERE expires_at_ns <= ?", time.Now().UnixNano()); err != nil {
		return fail()
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, errors.New("full graph cursor state file permissions could not be secured")
	}
	return &fullCursorStore{sessions: make(map[string]*fullCursorSession), now: time.Now, db: db}, nil
}

func (s *fullCursorStore) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

type persistedFullCursorSession struct {
	ID                  string
	Binding             fullCursorBinding
	Generation          int64
	ExpiresAt           time.Time
	Pending             []string
	BranchListTruncated bool
	QueueTruncated      bool
	QueueDropped        int
	Attempts            map[string]int
	Processed           int
	Cumulative          map[string]any
	CurrentToken        string
	PreviousToken       string
	PreviousResult      map[string]any
	Page                int
	Complete            bool
	ActiveCounted       bool
}

func encodePersistentFullCursorSession(session *fullCursorSession) ([]byte, error) {
	if session == nil {
		return nil, errors.New("cursor session is missing")
	}
	state := persistedFullCursorSession{
		ID: session.id, Binding: session.binding, Generation: session.generation,
		ExpiresAt: session.expiresAt, Pending: session.pending,
		BranchListTruncated: session.branchListTruncated, QueueTruncated: session.queueTruncated,
		QueueDropped: session.queueDropped, Attempts: session.attempts, Processed: session.processed,
		Cumulative: session.cumulative, CurrentToken: session.currentToken, PreviousToken: session.previousToken,
		PreviousResult: session.previousResult, Page: session.page, Complete: session.complete,
		ActiveCounted: session.activeCounted,
	}
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) > maxPersistedCursorPayloadBytes {
		return nil, errors.New("cursor session exceeds its persistence limit")
	}
	return encoded, nil
}

func decodePersistentFullCursorSession(encoded []byte) (*fullCursorSession, error) {
	if len(encoded) == 0 || len(encoded) > maxPersistedCursorPayloadBytes {
		return nil, errors.New("cursor state is empty or exceeds its persistence limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var state persistedFullCursorSession
	if err := decoder.Decode(&state); err != nil {
		return nil, errors.New("cursor state could not be decoded")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("cursor state contains trailing data")
	}
	pending, _, pendingErr := uniqueContinuationSeeds(state.Pending)
	if pendingErr != nil || len(pending) != len(state.Pending) {
		return nil, errors.New("cursor state contains invalid pending branches")
	}
	if !fullCursorTokenPattern.MatchString(state.ID) || !validFullCursorBinding(state.Binding) || state.Generation < 1 ||
		state.ExpiresAt.IsZero() || state.Processed < 0 || state.Processed > maxFullCursorProcessedBranches ||
		state.Page < 0 || state.Page > maxFullCursorProcessedBranches || state.QueueDropped < 0 ||
		state.QueueDropped > maxFullCursorProcessedBranches*maxFullCursorPendingBranches ||
		len(state.Attempts) > maxFullCursorProcessedBranches ||
		(state.CurrentToken != "" && !fullCursorTokenPattern.MatchString(state.CurrentToken)) ||
		(state.PreviousToken != "" && !fullCursorTokenPattern.MatchString(state.PreviousToken)) ||
		state.CurrentToken == state.PreviousToken || state.Complete != (state.CurrentToken == "") ||
		state.ActiveCounted == state.Complete ||
		(state.PreviousToken == "") != (state.PreviousResult == nil) ||
		(!state.Complete && (len(pending) == 0 || state.Cumulative == nil)) ||
		(state.Complete && (len(pending) != 0 || state.Cumulative != nil || len(state.Attempts) != 0)) {
		return nil, errors.New("cursor state violates its bounds")
	}
	for id, attempts := range state.Attempts {
		if _, _, err := uniqueContinuationSeeds([]string{id}); err != nil || attempts < 0 || attempts > maxFullCursorBranchAttempts {
			return nil, errors.New("cursor state contains invalid branch attempts")
		}
	}
	if state.Cumulative != nil {
		cumulative, err := json.Marshal(state.Cumulative)
		if err != nil || len(cumulative) > maxFullCursorSessionPayloadBytes {
			return nil, errors.New("cursor state contains an oversized graph snapshot")
		}
	}
	if state.PreviousResult != nil {
		result, err := json.Marshal(state.PreviousResult)
		if err != nil || len(result) > maxFullCursorReplayPayloadBytes {
			return nil, errors.New("cursor state contains an oversized replay response")
		}
	}
	if state.CurrentToken == "" && state.PreviousToken == "" {
		return nil, errors.New("cursor state has no valid token")
	}
	return &fullCursorSession{
		id: state.ID, binding: state.Binding, generation: state.Generation, expiresAt: state.ExpiresAt,
		pending: pending, branchListTruncated: state.BranchListTruncated, queueTruncated: state.QueueTruncated,
		queueDropped: state.QueueDropped, attempts: state.Attempts, processed: state.Processed,
		cumulative: state.Cumulative, currentToken: state.CurrentToken, previousToken: state.PreviousToken,
		previousResult: state.PreviousResult, page: state.Page, complete: state.Complete,
		activeCounted: state.ActiveCounted,
	}, nil
}

func (s *fullCursorStore) persistCreate(session *fullCursorSession) error {
	if s == nil || s.db == nil || session == nil {
		return errors.New("persistent cursor store is unavailable")
	}
	encoded, err := encodePersistentFullCursorSession(session)
	if err != nil {
		return queryError(ErrorOutputTooLarge, "full graph continuation state exceeds its persistence limit", err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return queryError(ErrorGraphUnavailable, "full graph continuation state could not be saved", err)
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := tx.Exec("DELETE FROM full_cursor_sessions WHERE expires_at_ns <= ?", now.UnixNano()); err != nil {
		return queryError(ErrorGraphUnavailable, "expired graph continuation state could not be pruned", err)
	}
	var active, total int
	if err := tx.QueryRow("SELECT count(*), coalesce(sum(active), 0) FROM full_cursor_sessions").Scan(&total, &active); err != nil {
		return queryError(ErrorGraphUnavailable, "graph continuation capacity could not be checked", err)
	}
	if active >= maxFullCursorSessions {
		return queryError(ErrorOutputTooLarge, "full graph continuation capacity is reached", nil)
	}
	if total >= maxFullCursorSessions {
		if _, err := tx.Exec("DELETE FROM full_cursor_sessions WHERE session_id = (SELECT session_id FROM full_cursor_sessions WHERE active = 0 ORDER BY expires_at_ns ASC LIMIT 1)"); err != nil {
			return queryError(ErrorGraphUnavailable, "completed graph continuation state could not be evicted", err)
		}
		var remaining int
		if err := tx.QueryRow("SELECT count(*) FROM full_cursor_sessions").Scan(&remaining); err != nil || remaining >= maxFullCursorSessions {
			return queryError(ErrorOutputTooLarge, "full graph continuation capacity is reached", err)
		}
	}
	if _, err := tx.Exec(
		"INSERT INTO full_cursor_sessions(session_id, state_json, expires_at_ns, active) VALUES(?, ?, ?, 1)",
		session.id, encoded, session.expiresAt.UnixNano(),
	); err != nil {
		return queryError(ErrorGraphUnavailable, "full graph continuation state could not be saved", err)
	}
	if _, err := tx.Exec("INSERT INTO full_cursor_tokens(token, session_id) VALUES(?, ?)", session.currentToken, session.id); err != nil {
		return queryError(ErrorGraphUnavailable, "full graph continuation token could not be saved", err)
	}
	if err := tx.Commit(); err != nil {
		return queryError(ErrorGraphUnavailable, "full graph continuation state could not be committed", err)
	}
	return nil
}

func (s *fullCursorStore) loadPersistent(token string) (*fullCursorSession, error) {
	if s == nil || s.db == nil {
		return nil, queryError(ErrorGraphUnavailable, "full graph continuation store is unavailable", nil)
	}
	var encoded []byte
	err := s.db.QueryRow("SELECT sessions.state_json FROM full_cursor_tokens AS tokens JOIN full_cursor_sessions AS sessions ON sessions.session_id = tokens.session_id WHERE tokens.token = ?", token).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, queryError(ErrorStaleCursor, "full graph continuation cursor expired or is unknown; reload the first page", nil)
	}
	if err != nil {
		return nil, queryError(ErrorGraphUnavailable, "full graph continuation state could not be read", err)
	}
	session, err := decodePersistentFullCursorSession(encoded)
	if err != nil {
		return nil, queryError(ErrorGraphUnavailable, "full graph continuation state is invalid", err)
	}
	if session.currentToken != token && session.previousToken != token {
		return nil, queryError(ErrorStaleCursor, "full graph continuation cursor is no longer current; reload the first page", nil)
	}
	if s.now().After(session.expiresAt) {
		s.deletePersistentSession(session.id)
		return nil, queryError(ErrorStaleCursor, "full graph continuation cursor expired; reload the first page", nil)
	}
	return session, nil
}

type fullCursorReplay struct {
	payload map[string]any
	token   string
	page    int
}

func (s *fullCursorStore) persistAdvance(expectedToken string, session *fullCursorSession) (*fullCursorReplay, error) {
	encoded, err := encodePersistentFullCursorSession(session)
	if err != nil {
		return nil, queryError(ErrorOutputTooLarge, "full graph continuation state exceeds its persistence limit", err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, queryError(ErrorGraphUnavailable, "full graph continuation state could not be updated", err)
	}
	defer tx.Rollback()
	var latestEncoded []byte
	err = tx.QueryRow("SELECT sessions.state_json FROM full_cursor_tokens AS tokens JOIN full_cursor_sessions AS sessions ON sessions.session_id = tokens.session_id WHERE tokens.token = ?", expectedToken).Scan(&latestEncoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, queryError(ErrorStaleCursor, "full graph continuation cursor was invalidated while loading; reload the first page", nil)
	}
	if err != nil {
		return nil, queryError(ErrorGraphUnavailable, "full graph continuation state could not be checked", err)
	}
	latest, err := decodePersistentFullCursorSession(latestEncoded)
	if err != nil {
		return nil, queryError(ErrorGraphUnavailable, "full graph continuation state is invalid", err)
	}
	if latest.id != session.id {
		return nil, queryError(ErrorStaleCursor, "full graph continuation cursor is no longer current; reload the first page", nil)
	}
	if latest.currentToken != expectedToken {
		if latest.previousToken == expectedToken && latest.previousResult != nil {
			replay, cloneErr := cloneGraphPayload(latest.previousResult)
			if cloneErr != nil {
				return nil, queryError(ErrorGraphUnavailable, "full graph continuation replay could not be read", cloneErr)
			}
			return &fullCursorReplay{payload: replay, token: stringValue(replay["next_cursor"]), page: latest.page}, nil
		}
		return nil, queryError(ErrorStaleCursor, "full graph continuation cursor is no longer current; reload the first page", nil)
	}
	if latest.expiresAt.Before(s.now()) {
		if _, err := tx.Exec("DELETE FROM full_cursor_sessions WHERE session_id = ?", latest.id); err != nil {
			return nil, queryError(ErrorGraphUnavailable, "expired graph continuation state could not be removed", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, queryError(ErrorGraphUnavailable, "expired graph continuation state could not be removed", err)
		}
		return nil, queryError(ErrorStaleCursor, "full graph continuation cursor expired while loading; reload the first page", nil)
	}
	if _, err := tx.Exec("UPDATE full_cursor_sessions SET state_json = ?, expires_at_ns = ?, active = ? WHERE session_id = ?",
		encoded, session.expiresAt.UnixNano(), boolInt(session.activeCounted), session.id); err != nil {
		return nil, queryError(ErrorGraphUnavailable, "full graph continuation state could not be updated", err)
	}
	if _, err := tx.Exec("DELETE FROM full_cursor_tokens WHERE session_id = ?", session.id); err != nil {
		return nil, queryError(ErrorGraphUnavailable, "old graph continuation tokens could not be replaced", err)
	}
	if session.previousToken != "" {
		if _, err := tx.Exec("INSERT INTO full_cursor_tokens(token, session_id) VALUES(?, ?)", session.previousToken, session.id); err != nil {
			return nil, queryError(ErrorGraphUnavailable, "previous graph continuation token could not be retained", err)
		}
	}
	if session.currentToken != "" {
		if _, err := tx.Exec("INSERT INTO full_cursor_tokens(token, session_id) VALUES(?, ?)", session.currentToken, session.id); err != nil {
			return nil, queryError(ErrorGraphUnavailable, "next graph continuation token could not be saved", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, queryError(ErrorGraphUnavailable, "full graph continuation state could not be committed", err)
	}
	return nil, nil
}

func (s *fullCursorStore) deletePersistentSession(id string) {
	if s == nil || s.db == nil || id == "" {
		return
	}
	_, _ = s.db.Exec("DELETE FROM full_cursor_sessions WHERE session_id = ?", id)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *fullCursorStore) create(binding fullCursorBinding, generation int64, seeds []string, cumulative map[string]any) (string, error) {
	if s == nil || !validFullCursorBinding(binding) || generation < 1 || cumulative == nil {
		return "", errors.New("full graph continuation binding is invalid")
	}
	pending, _, err := uniqueContinuationSeeds(seeds)
	if err != nil || len(pending) == 0 || len(pending) > maxFullCursorPendingBranches {
		return "", errors.New("full graph continuation branch set is invalid or exceeds its limit")
	}
	encoded, err := json.Marshal(cumulative)
	if err != nil || len(encoded) > maxFullCursorSessionPayloadBytes {
		return "", errors.New("full graph continuation snapshot exceeds its session limit")
	}
	initial, err := cloneGraphPayload(cumulative)
	if err != nil {
		return "", err
	}
	token, err := newFullCursorToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(now)
	if s.activeSessionCountLocked() >= maxFullCursorSessions {
		return "", errors.New("full graph continuation capacity is reached")
	}
	s.makeRoomForNewSessionLocked()
	session := &fullCursorSession{
		binding: binding, generation: generation, expiresAt: now.Add(fullCursorTTL),
		pending: pending, branchListTruncated: graphPayloadBool(cumulative["missing_branches_truncated"]),
		attempts: make(map[string]int), cumulative: initial, currentToken: token, activeCounted: true,
	}
	if s.db != nil {
		session.id = token
		if err := s.persistCreate(session); err != nil {
			return "", err
		}
		return token, nil
	}
	s.sessions[token] = session
	s.active++
	return token, nil
}

// next runs one bounded page callback. It advances the session only after the
// graph query and response preparation succeed. A retry of the just-consumed
// token replays the exact wire-safe response without another database read.
func (s *fullCursorStore) next(token string, binding fullCursorBinding, load fullCursorLoader, finalize fullCursorFinalizer) (map[string]any, string, int, error) {
	if s == nil || !fullCursorTokenPattern.MatchString(token) {
		return nil, "", 0, queryError(ErrorInvalidInput, "full graph continuation cursor is malformed", nil)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || !validFullCursorBinding(binding) {
		return nil, "", 0, queryError(ErrorInvalidInput, "full graph continuation cursor is malformed", nil)
	}
	var session *fullCursorSession
	if s.db != nil {
		session, err = s.loadPersistent(token)
		if err != nil {
			return nil, "", 0, err
		}
	} else {
		now := s.now()
		s.mu.Lock()
		s.pruneExpiredLocked(now)
		session = s.sessions[token]
		s.mu.Unlock()
	}
	if session == nil {
		return nil, "", 0, queryError(ErrorStaleCursor, "full graph continuation cursor expired or is unknown; reload the first page", nil)
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	now := s.now()
	if now.After(session.expiresAt) {
		s.remove(session)
		return nil, "", 0, queryError(ErrorStaleCursor, "full graph continuation cursor expired; reload the first page", nil)
	}
	if session.binding != binding {
		return nil, "", 0, queryError(ErrorInvalidInput, "full graph continuation cursor does not match this project, seed set, or budget", nil)
	}
	if token == session.previousToken {
		if session.previousResult != nil {
			copy, cloneErr := cloneGraphPayload(session.previousResult)
			if cloneErr == nil {
				return copy, stringValue(copy["next_cursor"]), session.page, nil
			}
		}
		return nil, "", 0, queryError(ErrorStaleCursor, "full graph continuation page was already consumed; use its latest cursor", nil)
	}
	if token != session.currentToken || session.complete || load == nil {
		return nil, "", 0, queryError(ErrorStaleCursor, "full graph continuation cursor is no longer current; reload the first page", nil)
	}

	remainingBudget := maxFullCursorProcessedBranches - session.processed
	if remainingBudget <= 0 {
		return nil, "", 0, queryError(ErrorOutputTooLarge, "full graph continuation branch processing limit was reached", nil)
	}
	count := min(fullCursorPageSeeds, binding.Budget.MaxNodes, remainingBudget, len(session.pending))
	pageSeeds := append([]string(nil), session.pending[:count]...)
	accumulated, cloneErr := cloneGraphPayload(session.cumulative)
	if cloneErr != nil {
		return nil, "", 0, queryError(ErrorGraphUnavailable, "full graph continuation state could not be read", cloneErr)
	}
	pageNo := session.page + 1
	page, discovered, generation, err := load(pageSeeds, accumulated, pageNo)
	if err != nil {
		var queryErr *QueryError
		if errors.As(err, &queryErr) && queryErr.Code == ErrorStaleCursor {
			s.remove(session)
		}
		return nil, "", 0, err
	}
	if generation != session.generation {
		s.remove(session)
		return nil, "", 0, queryError(ErrorStaleCursor, "Neo4j project changed during graph continuation; reload the first page", nil)
	}
	if page == nil {
		return nil, "", 0, queryError(ErrorGraphUnavailable, "full graph continuation returned an empty page", nil)
	}
	newSeeds, _, err := uniqueContinuationSeeds(discovered)
	if err != nil {
		return nil, "", 0, queryError(ErrorGraphUnavailable, "full graph continuation returned invalid branch identities", err)
	}
	if encoded, marshalErr := json.Marshal(page); marshalErr != nil || len(encoded) > maxFullCursorSessionPayloadBytes {
		return nil, "", 0, queryError(ErrorOutputTooLarge, "cumulative graph continuation exceeded its session limit", marshalErr)
	}
	newAttempts := make(map[string]int, len(session.attempts)+len(pageSeeds))
	for id, attempts := range session.attempts {
		newAttempts[id] = attempts
	}
	for _, id := range pageSeeds {
		newAttempts[id]++
	}
	// Keep the unprocessed tail before adding newly discovered branches. A page
	// must never silently drop work simply because its batch size is smaller
	// than the initial or accumulated queue.
	queued := make(map[string]struct{}, len(session.pending)-count+len(newSeeds))
	candidates := make([]string, 0, len(session.pending)-count+len(newSeeds))
	for _, id := range append(append([]string(nil), session.pending[count:]...), newSeeds...) {
		if _, exists := queued[id]; exists {
			continue
		}
		queued[id] = struct{}{}
		candidates = append(candidates, id)
	}
	sort.Strings(candidates)
	newPending := make([]string, 0, min(len(candidates), maxFullCursorPendingBranches))
	unresolved := 0
	queueDropped := 0
	for _, id := range candidates {
		if newAttempts[id] >= maxFullCursorBranchAttempts {
			unresolved++
			continue
		}
		if len(newPending) >= maxFullCursorPendingBranches {
			queueDropped++
			continue
		}
		newPending = append(newPending, id)
	}

	branchListTruncated := session.branchListTruncated || graphPayloadBool(page["missing_branches_truncated"])
	queueTruncated := session.queueTruncated || queueDropped > 0
	queueDroppedTotal := session.queueDropped + queueDropped
	page["continuation_page"] = pageNo
	if branchListTruncated {
		page["continuation_branches_truncated"] = true
	}
	if queueTruncated {
		page["continuation_branch_queue_truncated"] = true
		page["continuation_dropped_branch_count"] = queueDroppedTotal
	}
	nextToken := ""
	if len(newPending) > 0 {
		nextToken, err = newFullCursorToken()
		if err != nil {
			return nil, "", 0, queryError(ErrorGraphUnavailable, "full graph continuation cursor could not be generated", err)
		}
		page["next_cursor"] = nextToken
		page["continuation_state"] = "available"
		if branchListTruncated {
			page["continuation_state"] = "available_with_truncated_branch_list"
		}
		if queueTruncated {
			page["continuation_state"] = "available_with_truncated_branch_queue"
		}
		if unresolved > 0 {
			page["continuation_state"] = "available_with_unresolved_branches"
			page["continuation_unresolved_branch_count"] = unresolved
		}
	} else if unresolved > 0 {
		page["continuation_state"] = "budget_exhausted"
		page["continuation_unresolved_branch_count"] = unresolved
	} else if queueTruncated {
		page["continuation_state"] = "branch_queue_truncated"
	} else if branchListTruncated {
		page["continuation_state"] = "branch_list_truncated"
	} else {
		page["continuation_state"] = "branches_complete"
		page["continuation_branches_complete"] = true
	}
	if session.processed+len(pageSeeds) >= maxFullCursorProcessedBranches && len(newPending) > 0 {
		delete(page, "next_cursor")
		nextToken = ""
		page["continuation_state"] = "branch_budget_reached"
		page["continuation_unresolved_branch_count"] = len(newPending) + unresolved + queueDropped
	}

	finalPage, err := cloneGraphPayload(page)
	if err != nil {
		return nil, "", 0, queryError(ErrorGraphUnavailable, "full graph continuation response could not be prepared", err)
	}
	if finalize != nil {
		finalPage, err = finalize(finalPage)
		if err != nil {
			return nil, "", 0, err
		}
	}
	if finalPage == nil {
		return nil, "", 0, queryError(ErrorGraphUnavailable, "full graph continuation response is empty", nil)
	}
	encodedFinal, marshalErr := json.Marshal(finalPage)
	if marshalErr != nil {
		return nil, "", 0, queryError(ErrorGraphUnavailable, "full graph continuation response could not be encoded", marshalErr)
	}
	if len(encodedFinal) > maxFullCursorReplayPayloadBytes {
		return nil, "", 0, queryError(ErrorOutputTooLarge, "full graph continuation response exceeds the shared wire limit; reload with a narrower seed set", nil)
	}
	replay, err := cloneGraphPayload(finalPage)
	if err != nil {
		return nil, "", 0, queryError(ErrorGraphUnavailable, "full graph continuation response could not be cached", err)
	}
	storedCumulative, err := cloneGraphPayload(page)
	if err != nil {
		return nil, "", 0, queryError(ErrorGraphUnavailable, "full graph continuation state could not be stored", err)
	}
	delete(storedCumulative, "next_cursor")
	if s.db != nil {
		session.previousToken = token
		session.previousResult = replay
		session.pending = newPending
		session.branchListTruncated = branchListTruncated
		session.queueTruncated = queueTruncated
		session.queueDropped = queueDroppedTotal
		session.attempts = newAttempts
		session.processed += len(pageSeeds)
		session.cumulative = storedCumulative
		session.page = pageNo
		session.currentToken = nextToken
		session.complete = nextToken == ""
		if session.complete {
			session.pending = nil
			session.attempts = nil
			session.cumulative = nil
			session.activeCounted = false
		}
		replay, persistErr := s.persistAdvance(token, session)
		if persistErr != nil {
			return nil, "", 0, persistErr
		}
		if replay != nil {
			return replay.payload, replay.token, replay.page, nil
		}
		return finalPage, nextToken, pageNo, nil
	}
	s.mu.Lock()
	// The loader and response preparation can outlive the cursor TTL, and a
	// service shutdown can clear the store while they run. Revalidate both
	// expiry and ownership before committing any state or publishing a token.
	commitTime := s.now()
	s.pruneExpiredLocked(commitTime)
	if s.sessions[token] != session || !session.activeCounted || session.currentToken != token || commitTime.After(session.expiresAt) {
		s.removeSessionLocked(session)
		s.mu.Unlock()
		return nil, "", 0, queryError(ErrorStaleCursor, "full graph continuation cursor expired or was invalidated while loading; reload the first page", nil)
	}
	session.previousToken = token
	session.previousResult = replay
	session.pending = newPending
	session.branchListTruncated = branchListTruncated
	session.queueTruncated = queueTruncated
	session.queueDropped = queueDroppedTotal
	session.attempts = newAttempts
	session.processed += len(pageSeeds)
	session.cumulative = storedCumulative
	session.page = pageNo
	session.currentToken = nextToken
	session.complete = nextToken == ""
	if session.complete {
		session.pending = nil
		session.attempts = nil
		session.cumulative = nil
	}
	// Keep only the current and immediately previous opaque token mapped to a
	// session. The previous mapping enables bounded retry replay without growing
	// one map entry per page.
	for oldToken, candidate := range s.sessions {
		if candidate == session && oldToken != token && oldToken != nextToken {
			delete(s.sessions, oldToken)
		}
	}
	if nextToken != "" {
		s.sessions[nextToken] = session
	}
	if session.complete && session.activeCounted {
		s.active--
		session.activeCounted = false
	}
	s.mu.Unlock()
	return finalPage, nextToken, pageNo, nil
}

func (s *fullCursorStore) remove(expected *fullCursorSession) {
	if s != nil && s.db != nil {
		if expected != nil {
			s.deletePersistentSession(expected.id)
		}
		return
	}
	s.mu.Lock()
	s.removeSessionLocked(expected)
	s.mu.Unlock()
}

func (s *fullCursorStore) removeSessionLocked(expected *fullCursorSession) {
	for key, candidate := range s.sessions {
		if candidate == expected {
			delete(s.sessions, key)
		}
	}
	if expected.activeCounted {
		s.active--
		expected.activeCounted = false
	}
}

func (s *fullCursorStore) discard(token string) {
	if s == nil {
		return
	}
	if s.db != nil {
		var id string
		if err := s.db.QueryRow("SELECT session_id FROM full_cursor_tokens WHERE token = ?", token).Scan(&id); err == nil {
			s.deletePersistentSession(id)
		}
		return
	}
	s.mu.Lock()
	session := s.sessions[token]
	if session != nil {
		for key, candidate := range s.sessions {
			if candidate == session {
				delete(s.sessions, key)
			}
		}
		if session.activeCounted {
			s.active--
			session.activeCounted = false
		}
	}
	s.mu.Unlock()
}

func (s *fullCursorStore) pruneExpiredLocked(now time.Time) {
	removed := make(map[*fullCursorSession]struct{})
	for token, session := range s.sessions {
		if now.After(session.expiresAt) {
			delete(s.sessions, token)
			removed[session] = struct{}{}
		}
	}
	for session := range removed {
		if session.activeCounted {
			s.active--
			session.activeCounted = false
		}
	}
}

func (s *fullCursorStore) activeSessionCountLocked() int {
	return s.active
}

func (s *fullCursorStore) makeRoomForNewSessionLocked() {
	unique := make(map[*fullCursorSession]struct{}, len(s.sessions))
	for _, session := range s.sessions {
		unique[session] = struct{}{}
	}
	if len(unique) < maxFullCursorSessions {
		return
	}
	var oldest *fullCursorSession
	for session := range unique {
		if !session.activeCounted && (oldest == nil || session.expiresAt.Before(oldest.expiresAt)) {
			oldest = session
		}
	}
	if oldest == nil {
		return
	}
	for token, session := range s.sessions {
		if session == oldest {
			delete(s.sessions, token)
		}
	}
	if oldest.activeCounted {
		s.active--
		oldest.activeCounted = false
	}
}

func (s *fullCursorStore) clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	for _, session := range s.sessions {
		session.activeCounted = false
	}
	s.sessions = make(map[string]*fullCursorSession)
	s.active = 0
	s.mu.Unlock()
}

func validFullCursorBinding(binding fullCursorBinding) bool {
	return projectKeyPattern.MatchString(binding.ProjectKey) && projectKeyPattern.MatchString(binding.SeedHash) &&
		(binding.ContextHash == "" || projectKeyPattern.MatchString(binding.ContextHash)) &&
		binding.Budget.MaxNodes >= 1 && binding.Budget.MaxNodes <= 100 &&
		binding.Budget.MaxEdges >= 0 && binding.Budget.MaxEdges <= 300 &&
		binding.Budget.MaxDepth >= 0 && binding.Budget.MaxDepth <= 6
}

func uniqueContinuationSeeds(values []string) ([]string, map[string]struct{}, error) {
	if len(values) > maxFullCursorPendingBranches {
		return nil, nil, errors.New("full graph continuation contains too many branches")
	}
	seen := make(map[string]struct{}, len(values))
	ids := make([]string, 0, len(values))
	for _, value := range values {
		if !entitySeedPattern.MatchString(value) || strings.HasPrefix(value, "source-") || strings.HasPrefix(value, "card-") {
			return nil, nil, errors.New("full graph continuation seed is malformed")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		ids = append(ids, value)
	}
	sort.Strings(ids)
	return ids, seen, nil
}

func newFullCursorToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate full graph cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

func cloneGraphPayload(input map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var output map[string]any
	if err := decoder.Decode(&output); err != nil {
		return nil, err
	}
	return output, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func graphPayloadBool(value any) bool {
	flag, _ := value.(bool)
	return flag
}
