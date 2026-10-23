package appquery

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
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
)

var fullCursorTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type fullCursorBinding struct {
	ProjectKey string
	SeedHash   string
	Budget     GraphBudget
}

type fullCursorLoader func([]string, map[string]any, int) (map[string]any, []string, int64, error)
type fullCursorFinalizer func(map[string]any) (map[string]any, error)

type fullCursorSession struct {
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
}

func newFullCursorStore() *fullCursorStore {
	return &fullCursorStore{sessions: make(map[string]*fullCursorSession), now: time.Now}
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
	now := s.now()
	s.mu.Lock()
	s.pruneExpiredLocked(now)
	session := s.sessions[token]
	s.mu.Unlock()
	if session == nil {
		return nil, "", 0, queryError(ErrorStaleCursor, "full graph continuation cursor expired or is unknown; reload the first page", nil)
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	now = s.now()
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
		if !entitySeedPattern.MatchString(value) || !strings.HasPrefix(value, "assertion-") {
			return nil, nil, errors.New("full graph continuation branch is malformed")
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
