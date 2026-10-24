// Package consultation owns durable, project-scoped analyst sessions and
// candidate versions. It stores redacted evidence packets, never raw traffic.
package consultation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/publicdata"
	projectstore "bb-graph-assistant/internal/store"
)

const (
	SessionOpen                = "open"
	CandidateStateCandidate    = "candidate"
	ValidationReferencesValid  = "references_validated"
	maximumProposalBytes       = 12 << 10
	maximumProposals           = 32
	maximumValidationReceipts  = 512
	maximumCandidateVersions   = 32
	maximumSessionComparisons  = 16
	maximumContextAnnotations  = 32
	maximumOperationAliasRunes = 96
	maximumAnnotationEvidence  = 4
	maximumEvidenceReferences  = appquery.MaximumEvidencePacketSpans
	maximumEvidenceCitations   = 8
	maximumComparisonCitations = 16
	maximumSessionIDBytes      = 43
)

var (
	ErrInvalidInput                  = errors.New("consultation input is invalid")
	ErrProjectNotRegistered          = errors.New("consultation project is not registered")
	ErrSessionNotFound               = errors.New("consultation session was not found")
	ErrStoreUnavailable              = errors.New("consultation store is unavailable")
	ErrEvidenceUnavailable           = errors.New("consultation evidence is unavailable")
	ErrComparisonUnavailable         = errors.New("consultation comparison is unavailable")
	ErrCitationInvalid               = errors.New("proposal citation is not part of the exact selected evidence")
	ErrReceiptInvalid                = errors.New("proposal has no matching valid citation receipt")
	ErrReviewNotAccepted             = errors.New("operator review is required before assistant use")
	ErrLimitReached                  = errors.New("consultation storage limit was reached")
	ErrSessionVersionConflict        = errors.New("consultation session version is stale")
	ErrSessionAlreadyAccepted        = errors.New("consultation session is already accepted and immutable")
	ErrOperationAliasVersionConflict = errors.New("operation alias version is stale")
	ErrIdempotencyConflict           = errors.New("idempotency key was reused for a different operation")
	sessionIDPattern                 = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	idempotencyKeyPattern            = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	contextAliasSuffixPattern        = regexp.MustCompile(`^(?:[a-z]|[0-9]{1,3})$`)
	operationIDPattern               = regexp.MustCompile(`^operation-[a-f0-9]{64}$`)
	projectKeyPattern                = regexp.MustCompile(`^[a-f0-9]{64}$`)
	revisionPattern                  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// EvidenceBuilder returns the same redacted, bounded packet shown to the
// analyst. A session is never created from a raw or caller-supplied packet.
type EvidenceBuilder interface {
	BuildEvidencePacket(context.Context, appquery.EvidencePacketRequest) (appquery.EvidencePacket, error)
}

// ComparisonBuilder compares exact local revisions. The service strips message
// excerpts before comparison metadata can be persisted or returned to Hermes.
type ComparisonBuilder interface {
	CompareCaptureMessages(context.Context, appquery.CaptureComparisonRequest) (appquery.CaptureComparison, error)
}

type Service struct {
	dataRoot    string
	projects    map[string]project.CaidoProjectRef
	evidence    EvidenceBuilder
	comparisons ComparisonBuilder
	mu          sync.Mutex
	dbs         map[string]*sessionDB
	closed      bool
}

type Session struct {
	ID                    string                  `json:"consultation_id"`
	ProjectKey            string                  `json:"project_key"`
	Version               int                     `json:"version"`
	Question              string                  `json:"question"`
	State                 string                  `json:"state"`
	ReviewAccepted        bool                    `json:"review_accepted"`
	ReviewAcceptedVersion int                     `json:"review_accepted_version,omitempty"`
	ReviewAcceptedAt      time.Time               `json:"review_accepted_at,omitempty"`
	Evidence              appquery.EvidencePacket `json:"evidence"`
	References            []EvidenceReference     `json:"evidence_references"`
	ContextAnnotations    []ContextAnnotation     `json:"context_annotations"`
	Comparisons           []ComparisonRecord      `json:"comparisons"`
	Proposals             []ProposalRecord        `json:"proposals"`
	Receipts              []ValidationReceipt     `json:"validation_receipts"`
	Candidates            []CandidateVersion      `json:"candidates"`
	CreatedAt             time.Time               `json:"created_at"`
	UpdatedAt             time.Time               `json:"updated_at"`
}

// ContextAnnotation records an analyst-assigned pseudonymous label for exact
// evidence references. It is contextual metadata, not an observed fact.
type ContextAnnotation struct {
	ID                   string    `json:"context_annotation_id"`
	SessionVersion       int       `json:"session_version"`
	Kind                 string    `json:"kind"`
	Label                string    `json:"label"`
	EvidenceReferenceIDs []string  `json:"evidence_reference_ids"`
	Origin               string    `json:"origin"`
	EvidenceState        string    `json:"evidence_state"`
	CreatedAt            time.Time `json:"created_at"`
}

type RecordContextAnnotationRequest struct {
	ProjectKey           string   `json:"project_key"`
	SessionID            string   `json:"session_id"`
	ExpectedVersion      int      `json:"expected_version"`
	IdempotencyKey       string   `json:"idempotency_key"`
	Kind                 string   `json:"kind"`
	Label                string   `json:"label"`
	EvidenceReferenceIDs []string `json:"evidence_reference_ids"`
}

// OperationAlias is a project-local analyst label for an immutable graph
// operation. It never changes route-family data or evidence identifiers.
type OperationAlias struct {
	ID          string    `json:"operation_alias_id"`
	ProjectKey  string    `json:"project_key"`
	OperationID string    `json:"operation_id"`
	Version     int       `json:"version"`
	Alias       string    `json:"alias"`
	Origin      string    `json:"origin"`
	CreatedAt   time.Time `json:"created_at"`
}

type SaveOperationAliasRequest struct {
	ProjectKey      string `json:"project_key"`
	OperationID     string `json:"operation_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	Alias           string `json:"alias"`
}

type EvidenceReference struct {
	ID                    string `json:"evidence_reference_id"`
	EvidenceIndex         int    `json:"evidence_index"`
	RequestID             string `json:"request_id"`
	RevisionID            string `json:"revision_id"`
	Side                  string `json:"side"`
	SpanStart             int64  `json:"span_start"`
	SpanEnd               int64  `json:"span_end"`
	SHA256                string `json:"sha256,omitempty"`
	SourceIntegrityStatus string `json:"source_integrity_status"`
	PolicyVersion         string `json:"policy_version"`
}

type ComparisonRecord struct {
	ID             string                     `json:"comparison_id"`
	SessionVersion int                        `json:"session_version"`
	Result         appquery.CaptureComparison `json:"result"`
	CreatedAt      time.Time                  `json:"created_at"`
}

type CandidateProposal struct {
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	Rationale    string   `json:"rationale"`
	Impact       string   `json:"impact"`
	Alternatives []string `json:"alternatives"`
	NextChecks   []string `json:"next_checks"`
	Limitations  []string `json:"limitations"`
}

type RecordProposalRequest struct {
	ProjectKey           string            `json:"project_key"`
	SessionID            string            `json:"session_id"`
	ExpectedVersion      int               `json:"expected_version"`
	IdempotencyKey       string            `json:"idempotency_key"`
	Proposal             CandidateProposal `json:"proposal"`
	EvidenceReferenceIDs []string          `json:"evidence_reference_ids"`
	ComparisonIDs        []string          `json:"comparison_ids"`
}

type ProposalRecord struct {
	ID                   string            `json:"proposal_id"`
	SessionVersion       int               `json:"session_version"`
	Proposal             CandidateProposal `json:"proposal"`
	EvidenceReferenceIDs []string          `json:"evidence_reference_ids"`
	ComparisonIDs        []string          `json:"comparison_ids"`
	Digest               string            `json:"proposal_digest"`
	CreatedAt            time.Time         `json:"created_at"`
}

type ValidateProposalRequest struct {
	ProjectKey      string `json:"project_key"`
	SessionID       string `json:"session_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	ProposalID      string `json:"proposal_id"`
}

type ValidationReceipt struct {
	ID                   string    `json:"receipt_id"`
	SessionVersion       int       `json:"session_version"`
	ProposalID           string    `json:"proposal_id"`
	ProposalDigest       string    `json:"proposal_digest"`
	EvidenceReferenceIDs []string  `json:"evidence_reference_ids"`
	ComparisonIDs        []string  `json:"comparison_ids"`
	PolicyVersion        string    `json:"policy_version"`
	Status               string    `json:"status"`
	ValidatedAt          time.Time `json:"validated_at"`
}

type EvidenceCitation struct {
	EvidenceReferenceID string `json:"evidence_reference_id"`
	RequestID           string `json:"request_id"`
	RevisionID          string `json:"revision_id"`
	Side                string `json:"side"`
	SpanStart           int64  `json:"span_start"`
	SpanEnd             int64  `json:"span_end"`
	SHA256              string `json:"sha256"`
}

type SaveCandidateRequest struct {
	ProjectKey      string `json:"project_key"`
	SessionID       string `json:"session_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	ProposalID      string `json:"proposal_id"`
	ReceiptID       string `json:"receipt_id"`
	CandidateID     string `json:"candidate_id,omitempty"`
}

type CandidateVersion struct {
	CandidateID    string             `json:"candidate_id"`
	Version        int                `json:"candidate_version"`
	SessionVersion int                `json:"session_version"`
	State          string             `json:"state"`
	ProposalID     string             `json:"proposal_id"`
	ReceiptID      string             `json:"receipt_id"`
	Proposal       CandidateProposal  `json:"proposal"`
	Citations      []EvidenceCitation `json:"citations"`
	ComparisonIDs  []string           `json:"comparison_ids"`
	CreatedAt      time.Time          `json:"created_at"`
}

func NewService(dataRoot string, refs []project.CaidoProjectRef, evidence EvidenceBuilder, comparisons ComparisonBuilder) (*Service, error) {
	if strings.TrimSpace(dataRoot) == "" || evidence == nil || comparisons == nil {
		return nil, errors.New("consultation data root and evidence/comparison services are required")
	}
	projects := make(map[string]project.CaidoProjectRef, len(refs))
	for _, ref := range refs {
		key, err := ref.StorageKey()
		if err != nil {
			return nil, errors.New("consultation project catalog contains an invalid identity")
		}
		if _, exists := projects[key]; exists {
			return nil, errors.New("consultation project catalog contains duplicate identities")
		}
		projects[key] = ref
	}
	return &Service{dataRoot: dataRoot, projects: projects, evidence: evidence, comparisons: comparisons, dbs: make(map[string]*sessionDB)}, nil
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var result error
	for key, db := range s.dbs {
		if err := db.close(); err != nil {
			result = errors.Join(result, err)
		}
		delete(s.dbs, key)
	}
	return result
}

// acquireProjectReadLease coordinates consultation writes with project-wide
// maintenance. Maintenance takes the exclusive project lease before taking
// consistent snapshots, so no session, comparison, receipt, or candidate can
// change between the project's database snapshots. Platforms without a
// supported project lease retain the existing consultation behavior; any
// maintenance operation must fail closed when its exclusive lease is absent.
func (s *Service) acquireProjectReadLease(ctx context.Context, projectKey string) (*projectlock.Lock, error) {
	ref, ok := s.projects[projectKey]
	if !ok {
		return nil, ErrProjectNotRegistered
	}
	directory, err := projectstore.ProjectDirectory(ref, s.dataRoot)
	if err != nil {
		return nil, errors.Join(ErrStoreUnavailable, err)
	}
	lease, err := projectlock.AcquireRead(ctx, directory)
	if errors.Is(err, projectlock.ErrUnsupported) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Join(ErrStoreUnavailable, err)
	}
	return lease, nil
}

// CreateSession stores the exact bounded packet produced by the local evidence
// service. The idempotency key makes retries safe after a lost MCP response.
func (s *Service) CreateSession(ctx context.Context, request appquery.EvidencePacketRequest, idempotencyKey string) (_ Session, retErr error) {
	if s == nil || ctx == nil || !projectKeyPattern.MatchString(request.ProjectKey) || strings.TrimSpace(request.Question) == "" || !validIdempotencyKey(idempotencyKey) {
		return Session{}, ErrInvalidInput
	}
	if _, ok := s.projects[request.ProjectKey]; !ok {
		return Session{}, ErrProjectNotRegistered
	}
	if !utf8.ValidString(request.Question) || len(request.Question) > appquery.MaximumEvidencePacketQuestion {
		return Session{}, ErrInvalidInput
	}
	sanitizedQuestion, _, err := publicdata.RedactText(request.Question)
	if err != nil || strings.TrimSpace(sanitizedQuestion) == "" {
		return Session{}, ErrInvalidInput
	}
	canonicalRequest := request
	canonicalRequest.Question = sanitizedQuestion
	hash, err := digestJSON(canonicalRequest)
	if err != nil {
		return Session{}, ErrInvalidInput
	}
	lease, err := s.acquireProjectReadLease(ctx, request.ProjectKey)
	if err != nil {
		return Session{}, err
	}
	defer func() {
		if lease != nil {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	db, err := s.database(request.ProjectKey, true)
	if err != nil {
		return Session{}, errors.Join(ErrStoreUnavailable, err)
	}
	if encoded, found, err := db.lookupMutation(ctx, request.ProjectKey, "", "create_session", idempotencyKey, hash); err != nil {
		return Session{}, err
	} else if found {
		var replay Session
		if decodeSafe(encoded, &replay) != nil {
			return Session{}, ErrStoreUnavailable
		}
		return assistantVisible(replay), nil
	}
	packet, err := s.evidence.BuildEvidencePacket(ctx, request)
	if err != nil {
		return Session{}, errors.Join(ErrEvidenceUnavailable, err)
	}
	if err := validatePacket(request, packet); err != nil {
		return Session{}, errors.Join(ErrEvidenceUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return Session{}, errors.Join(ErrEvidenceUnavailable, err)
	}
	id, err := newID()
	if err != nil {
		return Session{}, ErrStoreUnavailable
	}
	references := make([]EvidenceReference, 0, len(packet.Evidence))
	for index, item := range packet.Evidence {
		refID, idErr := newID()
		if idErr != nil {
			return Session{}, ErrStoreUnavailable
		}
		references = append(references, EvidenceReference{
			ID: refID, EvidenceIndex: index, RequestID: item.RequestID, RevisionID: item.RevisionID, Side: item.Side,
			SpanStart: item.SpanStart, SpanEnd: item.SpanEnd, SHA256: item.SHA256,
			SourceIntegrityStatus: item.SourceIntegrityStatus, PolicyVersion: packet.PolicyVersion,
		})
	}
	now := time.Now().UTC()
	session := Session{
		ID: id, ProjectKey: request.ProjectKey, Version: 1, Question: packet.Question, State: SessionOpen,
		Evidence: packet, References: references, ContextAnnotations: []ContextAnnotation{}, Comparisons: []ComparisonRecord{}, Proposals: []ProposalRecord{},
		Receipts: []ValidationReceipt{}, Candidates: []CandidateVersion{}, CreatedAt: now, UpdatedAt: now,
	}
	created, err := db.createSession(ctx, session, idempotencyKey, hash)
	if err != nil {
		return Session{}, err
	}
	return assistantVisible(created), nil
}

func (s *Service) Resume(ctx context.Context, projectKey, sessionID string) (Session, error) {
	session, err := s.loadSession(ctx, projectKey, sessionID)
	if err != nil {
		return Session{}, err
	}
	return assistantVisible(session), nil
}

// ReviewSession is a loopback-UI-only use case. It returns the stored redacted
// preview so the operator can inspect it before allowing assistant access.
func (s *Service) ReviewSession(ctx context.Context, projectKey, sessionID string) (Session, error) {
	return s.loadSession(ctx, projectKey, sessionID)
}

func (s *Service) loadSession(ctx context.Context, projectKey, sessionID string) (Session, error) {
	if s == nil || ctx == nil || !projectKeyPattern.MatchString(projectKey) || !sessionIDPattern.MatchString(sessionID) {
		return Session{}, ErrInvalidInput
	}
	if _, ok := s.projects[projectKey]; !ok {
		return Session{}, ErrProjectNotRegistered
	}
	db, err := s.database(projectKey, false)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, errors.Join(ErrStoreUnavailable, err)
	}
	return db.readSession(ctx, projectKey, sessionID)
}

// AcceptReview records the operator's explicit approval for the redacted
// packet to become visible to Hermes. This transition is only exposed by the
// loopback web UI, never by MCP.
func (s *Service) AcceptReview(ctx context.Context, projectKey, sessionID string, expectedVersion int, idempotencyKey string) (_ Session, retErr error) {
	if s == nil || ctx == nil || !validSessionInput(projectKey, sessionID, expectedVersion, idempotencyKey) {
		return Session{}, ErrInvalidInput
	}
	lease, err := s.acquireProjectReadLease(ctx, projectKey)
	if err != nil {
		return Session{}, err
	}
	defer func() {
		if lease != nil {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	db, err := s.database(projectKey, false)
	if err != nil {
		return Session{}, errors.Join(ErrStoreUnavailable, err)
	}
	hash, err := digestJSON(struct {
		ProjectKey string `json:"project_key"`
		SessionID  string `json:"session_id"`
		Version    int    `json:"expected_version"`
	}{projectKey, sessionID, expectedVersion})
	if err != nil {
		return Session{}, ErrInvalidInput
	}
	if encoded, found, lookupErr := db.lookupMutation(ctx, projectKey, sessionID, "accept_review", idempotencyKey, hash); lookupErr != nil {
		return Session{}, lookupErr
	} else if found {
		var replay Session
		if decodeSafe(encoded, &replay) != nil {
			return Session{}, ErrStoreUnavailable
		}
		return replay, nil
	}
	session, err := s.loadSession(ctx, projectKey, sessionID)
	if err != nil {
		return Session{}, err
	}
	if session.Version != expectedVersion {
		return Session{}, ErrSessionVersionConflict
	}
	if session.ReviewAccepted {
		return Session{}, ErrSessionVersionConflict
	}
	session, err = db.appendReview(ctx, projectKey, sessionID, expectedVersion, idempotencyKey, hash, session)
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

// RecordContextAnnotation appends a pseudonymous analyst label to exact
// evidence references before the operator accepts the consultation packet.
// The annotation is not an observed fact and cannot be changed after review.
func (s *Service) RecordContextAnnotation(ctx context.Context, request RecordContextAnnotationRequest) (_ ContextAnnotation, retErr error) {
	if s == nil || ctx == nil || !validSessionInput(request.ProjectKey, request.SessionID, request.ExpectedVersion, request.IdempotencyKey) ||
		!validContextKind(request.Kind) || !validContextLabel(request.Kind, request.Label) ||
		len(request.EvidenceReferenceIDs) < 1 || len(request.EvidenceReferenceIDs) > maximumAnnotationEvidence || !validOpaqueIDs(request.EvidenceReferenceIDs) {
		return ContextAnnotation{}, ErrInvalidInput
	}
	canonical := request
	canonical.Label = strings.TrimSpace(request.Label)
	hash, err := digestJSON(canonical)
	if err != nil {
		return ContextAnnotation{}, ErrInvalidInput
	}
	lease, err := s.acquireProjectReadLease(ctx, request.ProjectKey)
	if err != nil {
		return ContextAnnotation{}, err
	}
	defer func() {
		if lease != nil {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	db, err := s.database(request.ProjectKey, false)
	if err != nil {
		return ContextAnnotation{}, errors.Join(ErrStoreUnavailable, err)
	}
	if encoded, found, lookupErr := db.lookupMutation(ctx, request.ProjectKey, request.SessionID, "context_annotation", request.IdempotencyKey, hash); lookupErr != nil {
		return ContextAnnotation{}, lookupErr
	} else if found {
		var replay ContextAnnotation
		if decodeSafe(encoded, &replay) != nil {
			return ContextAnnotation{}, ErrStoreUnavailable
		}
		return replay, nil
	}
	session, err := s.loadSession(ctx, request.ProjectKey, request.SessionID)
	if err != nil {
		return ContextAnnotation{}, err
	}
	if session.ReviewAccepted {
		return ContextAnnotation{}, ErrSessionAlreadyAccepted
	}
	if session.Version != request.ExpectedVersion {
		return ContextAnnotation{}, ErrSessionVersionConflict
	}
	refs := referenceMap(session)
	for _, referenceID := range request.EvidenceReferenceIDs {
		if _, selected := refs[referenceID]; !selected {
			return ContextAnnotation{}, ErrCitationInvalid
		}
	}
	if len(session.ContextAnnotations) >= maximumContextAnnotations {
		return ContextAnnotation{}, ErrLimitReached
	}
	id, err := newID()
	if err != nil {
		return ContextAnnotation{}, ErrStoreUnavailable
	}
	record := ContextAnnotation{
		ID: id, SessionVersion: request.ExpectedVersion + 1, Kind: request.Kind, Label: canonical.Label,
		EvidenceReferenceIDs: append([]string(nil), request.EvidenceReferenceIDs...),
		Origin:               "analyst", EvidenceState: "analyst_provided", CreatedAt: time.Now().UTC(),
	}
	return db.appendContextAnnotation(ctx, request.ProjectKey, request.SessionID, request.ExpectedVersion, request.IdempotencyKey, hash, record)
}

func validContextKind(kind string) bool {
	switch kind {
	case "actor", "session", "tenant", "role", "object", "flow", "scenario", "scenario_step":
		return true
	default:
		return false
	}
}

func validContextLabel(kind, label string) bool {
	if !utf8.ValidString(label) || len(label) == 0 || len(label) > 64 || strings.TrimSpace(label) != label {
		return false
	}
	prefix := strings.ReplaceAll(kind, "_", "-") + "-"
	return strings.HasPrefix(label, prefix) && contextAliasSuffixPattern.MatchString(strings.TrimPrefix(label, prefix))
}

// ListOperationAliases returns the latest analyst label for each operation in
// one project. Alias records are contextual metadata, never graph evidence.
func (s *Service) ListOperationAliases(ctx context.Context, projectKey string) (_ []OperationAlias, retErr error) {
	if s == nil || ctx == nil || !projectKeyPattern.MatchString(projectKey) {
		return nil, ErrInvalidInput
	}
	lease, err := s.acquireProjectReadLease(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	defer func() {
		if lease != nil {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	db, err := s.database(projectKey, false)
	if errors.Is(err, ErrSessionNotFound) {
		return []OperationAlias{}, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := db.db.QueryContext(ctx, `SELECT a.alias_id, a.project_key, a.operation_id,
		a.alias_version, a.alias, a.origin, a.created_at_ns
		FROM consultation_operation_aliases a
		JOIN (SELECT operation_id, max(alias_version) AS alias_version
			FROM consultation_operation_aliases WHERE project_key = ? GROUP BY operation_id) latest
		ON latest.operation_id = a.operation_id AND latest.alias_version = a.alias_version
		WHERE a.project_key = ? ORDER BY a.operation_id`, projectKey, projectKey)
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	defer rows.Close()
	aliases := make([]OperationAlias, 0)
	for rows.Next() {
		var alias OperationAlias
		var createdAt int64
		if err := rows.Scan(&alias.ID, &alias.ProjectKey, &alias.OperationID, &alias.Version, &alias.Alias, &alias.Origin, &createdAt); err != nil {
			return nil, ErrStoreUnavailable
		}
		alias.CreatedAt = time.Unix(0, createdAt).UTC()
		aliases = append(aliases, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrStoreUnavailable
	}
	return aliases, nil
}

// SaveOperationAlias appends a versioned, project-local analyst label while
// preserving both the source operation ID and every previous label version.
func (s *Service) SaveOperationAlias(ctx context.Context, request SaveOperationAliasRequest) (_ OperationAlias, retErr error) {
	if s == nil || ctx == nil || !projectKeyPattern.MatchString(request.ProjectKey) ||
		!operationIDPattern.MatchString(request.OperationID) || request.ExpectedVersion < 0 || request.ExpectedVersion >= 1_000_000 ||
		!validIdempotencyKey(request.IdempotencyKey) || !validOperationAlias(request.Alias) {
		return OperationAlias{}, ErrInvalidInput
	}
	request.Alias = strings.TrimSpace(request.Alias)
	hash, err := digestJSON(request)
	if err != nil {
		return OperationAlias{}, ErrInvalidInput
	}
	lease, err := s.acquireProjectReadLease(ctx, request.ProjectKey)
	if err != nil {
		return OperationAlias{}, err
	}
	defer func() {
		if lease != nil {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	db, err := s.database(request.ProjectKey, true)
	if err != nil {
		return OperationAlias{}, errors.Join(ErrStoreUnavailable, err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return OperationAlias{}, ErrStoreUnavailable
	}
	defer tx.Rollback()
	if encoded, found, err := lookupMutationTx(ctx, tx, request.ProjectKey, request.OperationID, "operation_alias", request.IdempotencyKey, hash); err != nil {
		return OperationAlias{}, err
	} else if found {
		var replay OperationAlias
		if decodeSafe(encoded, &replay) != nil {
			return OperationAlias{}, ErrStoreUnavailable
		}
		return replay, nil
	}
	var currentVersion int
	err = tx.QueryRowContext(ctx, `SELECT coalesce(max(alias_version), 0) FROM consultation_operation_aliases
		WHERE project_key = ? AND operation_id = ?`, request.ProjectKey, request.OperationID).Scan(&currentVersion)
	if err != nil {
		return OperationAlias{}, ErrStoreUnavailable
	}
	if currentVersion != request.ExpectedVersion {
		return OperationAlias{}, ErrOperationAliasVersionConflict
	}
	id, err := newID()
	if err != nil {
		return OperationAlias{}, ErrStoreUnavailable
	}
	record := OperationAlias{ID: id, ProjectKey: request.ProjectKey, OperationID: request.OperationID,
		Version: currentVersion + 1, Alias: request.Alias, Origin: "analyst", CreatedAt: time.Now().UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO consultation_operation_aliases
		(alias_id, project_key, operation_id, alias_version, alias, origin, idempotency_key, request_hash, created_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.ID, record.ProjectKey, record.OperationID, record.Version,
		record.Alias, record.Origin, request.IdempotencyKey, hash, record.CreatedAt.UnixNano()); err != nil {
		return OperationAlias{}, ErrStoreUnavailable
	}
	encoded, err := encodeSafe(record)
	if err != nil {
		return OperationAlias{}, ErrStoreUnavailable
	}
	if err := insertMutation(ctx, tx, request.ProjectKey, request.OperationID, "operation_alias", request.IdempotencyKey, hash, encoded, record.CreatedAt); err != nil {
		return OperationAlias{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperationAlias{}, ErrStoreUnavailable
	}
	return record, nil
}

func validOperationAlias(alias string) bool {
	if !utf8.ValidString(alias) {
		return false
	}
	trimmed := strings.TrimSpace(alias)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > maximumOperationAliasRunes {
		return false
	}
	for _, char := range trimmed {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

// Compare appends a deterministic comparison only when both exact revisions
// are represented by available, hash-verified references in the session.
func (s *Service) Compare(ctx context.Context, projectKey, sessionID string, expectedVersion int, idempotencyKey string, request appquery.CaptureComparisonRequest) (_ ComparisonRecord, retErr error) {
	if s == nil || ctx == nil || request.ProjectKey != projectKey || !validSessionInput(projectKey, sessionID, expectedVersion, idempotencyKey) {
		return ComparisonRecord{}, ErrInvalidInput
	}
	op := struct {
		ProjectKey string                            `json:"project_key"`
		SessionID  string                            `json:"session_id"`
		Version    int                               `json:"expected_version"`
		Request    appquery.CaptureComparisonRequest `json:"request"`
	}{projectKey, sessionID, expectedVersion, request}
	hash, err := digestJSON(op)
	if err != nil {
		return ComparisonRecord{}, ErrInvalidInput
	}
	lease, err := s.acquireProjectReadLease(ctx, projectKey)
	if err != nil {
		return ComparisonRecord{}, err
	}
	defer func() {
		if lease != nil {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	db, err := s.database(projectKey, false)
	if err != nil {
		return ComparisonRecord{}, errors.Join(ErrStoreUnavailable, err)
	}
	session, err := s.loadSession(ctx, projectKey, sessionID)
	if err != nil {
		return ComparisonRecord{}, err
	}
	if !session.ReviewAccepted {
		return ComparisonRecord{}, ErrReviewNotAccepted
	}
	if encoded, found, err := db.lookupMutation(ctx, projectKey, sessionID, "compare", idempotencyKey, hash); err != nil {
		return ComparisonRecord{}, err
	} else if found {
		var replay ComparisonRecord
		if decodeSafe(encoded, &replay) != nil {
			return ComparisonRecord{}, ErrStoreUnavailable
		}
		return replay, nil
	}
	if session.Version != expectedVersion {
		return ComparisonRecord{}, ErrSessionVersionConflict
	}
	if !hasAvailableEvidence(session, request.LeftRequestID, request.LeftRevisionID) || !hasAvailableEvidence(session, request.RightRequestID, request.RightRevisionID) {
		return ComparisonRecord{}, ErrCitationInvalid
	}
	if len(session.Comparisons) >= maximumSessionComparisons {
		return ComparisonRecord{}, ErrLimitReached
	}
	result, err := s.comparisons.CompareCaptureMessages(ctx, request)
	if err != nil {
		return ComparisonRecord{}, errors.Join(ErrComparisonUnavailable, err)
	}
	result, err = safeComparison(result, request)
	if err != nil {
		return ComparisonRecord{}, ErrComparisonUnavailable
	}
	id, err := newID()
	if err != nil {
		return ComparisonRecord{}, ErrStoreUnavailable
	}
	record := ComparisonRecord{ID: id, SessionVersion: expectedVersion + 1, Result: result, CreatedAt: time.Now().UTC()}
	return db.appendComparison(ctx, projectKey, sessionID, expectedVersion, idempotencyKey, hash, record)
}

// RecordProposal stores a redacted, untrusted interpretation. Citation IDs are
// not trusted or validated until ValidateProposal creates a separate receipt.
func (s *Service) RecordProposal(ctx context.Context, request RecordProposalRequest) (_ ProposalRecord, retErr error) {
	if s == nil || ctx == nil || !validSessionInput(request.ProjectKey, request.SessionID, request.ExpectedVersion, request.IdempotencyKey) {
		return ProposalRecord{}, ErrInvalidInput
	}
	proposal, err := sanitizeProposal(request.Proposal)
	if err != nil {
		return ProposalRecord{}, ErrInvalidInput
	}
	if len(request.EvidenceReferenceIDs) > maximumEvidenceCitations || len(request.ComparisonIDs) > maximumComparisonCitations ||
		!validOpaqueIDs(request.EvidenceReferenceIDs) || !validOpaqueIDs(request.ComparisonIDs) {
		return ProposalRecord{}, ErrInvalidInput
	}
	normalized := request
	normalized.Proposal = proposal
	hash, err := digestJSON(normalized)
	if err != nil {
		return ProposalRecord{}, ErrInvalidInput
	}
	lease, err := s.acquireProjectReadLease(ctx, request.ProjectKey)
	if err != nil {
		return ProposalRecord{}, err
	}
	defer func() {
		if lease != nil {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	db, err := s.database(request.ProjectKey, false)
	if err != nil {
		return ProposalRecord{}, errors.Join(ErrStoreUnavailable, err)
	}
	session, err := s.loadSession(ctx, request.ProjectKey, request.SessionID)
	if err != nil {
		return ProposalRecord{}, err
	}
	if !session.ReviewAccepted {
		return ProposalRecord{}, ErrReviewNotAccepted
	}
	if encoded, found, err := db.lookupMutation(ctx, request.ProjectKey, request.SessionID, "record_proposal", request.IdempotencyKey, hash); err != nil {
		return ProposalRecord{}, err
	} else if found {
		var replay ProposalRecord
		if decodeSafe(encoded, &replay) != nil {
			return ProposalRecord{}, ErrStoreUnavailable
		}
		return replay, nil
	}
	if session.Version != request.ExpectedVersion {
		return ProposalRecord{}, ErrSessionVersionConflict
	}
	if len(session.Proposals) >= maximumProposals {
		return ProposalRecord{}, ErrLimitReached
	}
	id, err := newID()
	if err != nil {
		return ProposalRecord{}, ErrStoreUnavailable
	}
	proposalDigest, err := digestJSON(struct {
		Proposal    CandidateProposal `json:"proposal"`
		EvidenceIDs []string          `json:"evidence_reference_ids"`
		CompareIDs  []string          `json:"comparison_ids"`
		Policy      string            `json:"policy_version"`
	}{proposal, request.EvidenceReferenceIDs, request.ComparisonIDs, session.Evidence.PolicyVersion})
	if err != nil {
		return ProposalRecord{}, ErrInvalidInput
	}
	record := ProposalRecord{
		ID: id, SessionVersion: request.ExpectedVersion + 1, Proposal: proposal,
		EvidenceReferenceIDs: append([]string(nil), request.EvidenceReferenceIDs...), ComparisonIDs: append([]string(nil), request.ComparisonIDs...),
		Digest: proposalDigest, CreatedAt: time.Now().UTC(),
	}
	return db.appendProposal(ctx, request.ProjectKey, request.SessionID, request.ExpectedVersion, request.IdempotencyKey, hash, record)
}

// ValidateProposal re-resolves every cited exact span and recomputes cited
// comparisons. It proves citation identity and integrity only; it does not
// establish semantic support, impact, or vulnerability.
func (s *Service) ValidateProposal(ctx context.Context, request ValidateProposalRequest) (_ ValidationReceipt, retErr error) {
	if s == nil || ctx == nil || !validSessionInput(request.ProjectKey, request.SessionID, request.ExpectedVersion, request.IdempotencyKey) || !sessionIDPattern.MatchString(request.ProposalID) {
		return ValidationReceipt{}, ErrInvalidInput
	}
	hash, err := digestJSON(request)
	if err != nil {
		return ValidationReceipt{}, ErrInvalidInput
	}
	lease, err := s.acquireProjectReadLease(ctx, request.ProjectKey)
	if err != nil {
		return ValidationReceipt{}, err
	}
	defer func() {
		if lease != nil {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	db, err := s.database(request.ProjectKey, false)
	if err != nil {
		return ValidationReceipt{}, errors.Join(ErrStoreUnavailable, err)
	}
	session, err := s.loadSession(ctx, request.ProjectKey, request.SessionID)
	if err != nil {
		return ValidationReceipt{}, err
	}
	if !session.ReviewAccepted {
		return ValidationReceipt{}, ErrReviewNotAccepted
	}
	if encoded, found, err := db.lookupMutation(ctx, request.ProjectKey, request.SessionID, "validate_proposal", request.IdempotencyKey, hash); err != nil {
		return ValidationReceipt{}, err
	} else if found {
		var replay ValidationReceipt
		if decodeSafe(encoded, &replay) != nil {
			return ValidationReceipt{}, ErrStoreUnavailable
		}
		return replay, nil
	}
	if session.Version != request.ExpectedVersion {
		return ValidationReceipt{}, ErrSessionVersionConflict
	}
	proposal, ok := findProposal(session, request.ProposalID)
	if !ok {
		return ValidationReceipt{}, ErrInvalidInput
	}
	if err := s.validateProposalReferences(ctx, session, proposal); err != nil {
		return ValidationReceipt{}, err
	}
	id, err := newID()
	if err != nil {
		return ValidationReceipt{}, ErrStoreUnavailable
	}
	receipt := ValidationReceipt{
		ID: id, SessionVersion: request.ExpectedVersion + 1, ProposalID: proposal.ID, ProposalDigest: proposal.Digest,
		EvidenceReferenceIDs: append([]string(nil), proposal.EvidenceReferenceIDs...), ComparisonIDs: append([]string(nil), proposal.ComparisonIDs...),
		PolicyVersion: session.Evidence.PolicyVersion, Status: ValidationReferencesValid, ValidatedAt: time.Now().UTC(),
	}
	return db.appendReceipt(ctx, request.ProjectKey, request.SessionID, request.ExpectedVersion, request.IdempotencyKey, hash, receipt)
}

// SaveCandidate inserts one immutable candidate version after an exact
// proposal/receipt match. New candidates always start as "candidate".
func (s *Service) SaveCandidate(ctx context.Context, request SaveCandidateRequest) (_ CandidateVersion, retErr error) {
	if s == nil || ctx == nil || !validSessionInput(request.ProjectKey, request.SessionID, request.ExpectedVersion, request.IdempotencyKey) ||
		!sessionIDPattern.MatchString(request.ProposalID) || !sessionIDPattern.MatchString(request.ReceiptID) ||
		(request.CandidateID != "" && !sessionIDPattern.MatchString(request.CandidateID)) {
		return CandidateVersion{}, ErrInvalidInput
	}
	hash, err := digestJSON(request)
	if err != nil {
		return CandidateVersion{}, ErrInvalidInput
	}
	lease, err := s.acquireProjectReadLease(ctx, request.ProjectKey)
	if err != nil {
		return CandidateVersion{}, err
	}
	defer func() {
		if lease != nil {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	db, err := s.database(request.ProjectKey, false)
	if err != nil {
		return CandidateVersion{}, errors.Join(ErrStoreUnavailable, err)
	}
	session, err := s.loadSession(ctx, request.ProjectKey, request.SessionID)
	if err != nil {
		return CandidateVersion{}, err
	}
	if !session.ReviewAccepted {
		return CandidateVersion{}, ErrReviewNotAccepted
	}
	if encoded, found, err := db.lookupMutation(ctx, request.ProjectKey, request.SessionID, "save_candidate", request.IdempotencyKey, hash); err != nil {
		return CandidateVersion{}, err
	} else if found {
		var replay CandidateVersion
		if decodeSafe(encoded, &replay) != nil {
			return CandidateVersion{}, ErrStoreUnavailable
		}
		return canonicalCandidateReplay(session, replay)
	}
	if session.Version != request.ExpectedVersion {
		return CandidateVersion{}, ErrSessionVersionConflict
	}
	proposal, ok := findProposal(session, request.ProposalID)
	if !ok {
		return CandidateVersion{}, ErrReceiptInvalid
	}
	receipt, ok := findReceipt(session, request.ReceiptID)
	if !ok || receipt.ProposalID != proposal.ID || receipt.ProposalDigest != proposal.Digest || receipt.PolicyVersion != session.Evidence.PolicyVersion ||
		receipt.Status != ValidationReferencesValid || !reflect.DeepEqual(receipt.EvidenceReferenceIDs, proposal.EvidenceReferenceIDs) ||
		!reflect.DeepEqual(receipt.ComparisonIDs, proposal.ComparisonIDs) {
		return CandidateVersion{}, ErrReceiptInvalid
	}
	references := referenceMap(session)
	citations := make([]EvidenceCitation, 0, len(receipt.EvidenceReferenceIDs))
	for _, refID := range receipt.EvidenceReferenceIDs {
		ref, exists := references[refID]
		if !exists || ref.SourceIntegrityStatus != "verified" || !revisionPattern.MatchString(ref.SHA256) {
			return CandidateVersion{}, ErrReceiptInvalid
		}
		citations = append(citations, EvidenceCitation{
			EvidenceReferenceID: ref.ID, RequestID: ref.RequestID, RevisionID: ref.RevisionID, Side: ref.Side,
			SpanStart: ref.SpanStart, SpanEnd: ref.SpanEnd, SHA256: ref.SHA256,
		})
	}
	if len(citations) == 0 {
		return CandidateVersion{}, ErrReceiptInvalid
	}
	candidateID := request.CandidateID
	if candidateID == "" {
		candidateID, err = newID()
		if err != nil {
			return CandidateVersion{}, ErrStoreUnavailable
		}
	} else if !candidateExists(session, candidateID) {
		return CandidateVersion{}, ErrInvalidInput
	}
	return db.appendCandidate(ctx, request.ProjectKey, request.SessionID, request.ExpectedVersion, request.IdempotencyKey, hash, candidateID, proposal, receipt, citations)
}

func (s *Service) validateProposalReferences(ctx context.Context, session Session, proposal ProposalRecord) error {
	if len(proposal.EvidenceReferenceIDs) == 0 || len(proposal.EvidenceReferenceIDs) > maximumEvidenceReferences ||
		len(proposal.ComparisonIDs) > maximumComparisonCitations {
		return ErrCitationInvalid
	}
	references := referenceMap(session)
	selections := make([]appquery.EvidencePacketSelection, 0, len(proposal.EvidenceReferenceIDs))
	selectedRefs := make([]EvidenceReference, 0, len(proposal.EvidenceReferenceIDs))
	seen := make(map[string]struct{}, len(proposal.EvidenceReferenceIDs))
	for _, refID := range proposal.EvidenceReferenceIDs {
		ref, ok := references[refID]
		if !ok || ref.PolicyVersion != session.Evidence.PolicyVersion || ref.SourceIntegrityStatus != "verified" || !revisionPattern.MatchString(ref.SHA256) ||
			ref.SpanEnd <= ref.SpanStart || ref.SpanEnd-ref.SpanStart > appquery.MaximumEvidencePacketSpanBytes {
			return ErrCitationInvalid
		}
		if _, duplicate := seen[refID]; duplicate {
			return ErrCitationInvalid
		}
		seen[refID] = struct{}{}
		selectedRefs = append(selectedRefs, ref)
		selections = append(selections, appquery.EvidencePacketSelection{
			RequestID: ref.RequestID, RevisionID: ref.RevisionID, Side: ref.Side,
			Offset: ref.SpanStart, Length: int(ref.SpanEnd - ref.SpanStart),
		})
	}
	packet, err := s.evidence.BuildEvidencePacket(ctx, appquery.EvidencePacketRequest{
		ProjectKey: session.ProjectKey, Question: session.Question, Selections: selections,
	})
	if err != nil || validatePacket(appquery.EvidencePacketRequest{ProjectKey: session.ProjectKey, Question: session.Question, Selections: selections}, packet) != nil {
		return errors.Join(ErrCitationInvalid, err)
	}
	for index, item := range packet.Evidence {
		ref := selectedRefs[index]
		if !item.Available || item.SourceIntegrityStatus != "verified" || item.RequestID != ref.RequestID || item.RevisionID != ref.RevisionID ||
			item.Side != ref.Side || item.SpanStart != ref.SpanStart || item.SpanEnd != ref.SpanEnd || item.SHA256 != ref.SHA256 || packet.PolicyVersion != ref.PolicyVersion {
			return ErrCitationInvalid
		}
	}
	comparisons := comparisonMap(session)
	seenComparisons := make(map[string]struct{}, len(proposal.ComparisonIDs))
	for _, comparisonID := range proposal.ComparisonIDs {
		record, ok := comparisons[comparisonID]
		if !ok {
			return ErrCitationInvalid
		}
		if _, duplicate := seenComparisons[comparisonID]; duplicate {
			return ErrCitationInvalid
		}
		seenComparisons[comparisonID] = struct{}{}
		old := record.Result
		fresh, compareErr := s.comparisons.CompareCaptureMessages(ctx, appquery.CaptureComparisonRequest{
			ProjectKey: session.ProjectKey, LeftRequestID: old.LeftRequestID, LeftRevisionID: old.LeftRevisionID,
			RightRequestID: old.RightRequestID, RightRevisionID: old.RightRevisionID,
		})
		if compareErr != nil {
			return errors.Join(ErrCitationInvalid, compareErr)
		}
		fresh, compareErr = safeComparison(fresh, appquery.CaptureComparisonRequest{
			ProjectKey: session.ProjectKey, LeftRequestID: old.LeftRequestID, LeftRevisionID: old.LeftRevisionID,
			RightRequestID: old.RightRequestID, RightRevisionID: old.RightRevisionID,
		})
		if compareErr != nil || !reflect.DeepEqual(fresh, old) {
			return ErrCitationInvalid
		}
	}
	return nil
}

func validatePacket(request appquery.EvidencePacketRequest, packet appquery.EvidencePacket) error {
	if !utf8.ValidString(request.Question) || len(request.Question) > appquery.MaximumEvidencePacketQuestion ||
		len(request.Selections) < 1 || len(request.Selections) > appquery.MaximumEvidencePacketSpans {
		return errors.New("evidence packet request exceeds its bounds")
	}
	expectedQuestion, _, err := publicdata.RedactText(request.Question)
	if err != nil || strings.TrimSpace(expectedQuestion) == "" || packet.Question != expectedQuestion {
		return errors.New("evidence packet question does not match the redacted request")
	}
	if packet.Version != "1" || packet.PolicyVersion != appquery.EvidencePacketPolicyVersion || packet.ProjectKey != request.ProjectKey ||
		!packet.PreviewOnly || !packet.OperatorReviewNeeded || len(packet.Evidence) != len(request.Selections) ||
		len(packet.Evidence) == 0 || len(packet.Evidence) > appquery.MaximumEvidencePacketSpans || packet.SelectedBytes < 0 || packet.SelectedBytes > appquery.MaximumEvidencePacketBytes {
		return errors.New("evidence packet identity or bounds do not match the request")
	}
	if err := publicdata.Validate(packet); err != nil {
		return err
	}
	selected := 0
	for index, item := range packet.Evidence {
		selection := request.Selections[index]
		if item.RequestID != selection.RequestID || item.RevisionID != selection.RevisionID || item.Side != selection.Side ||
			item.SpanStart != selection.Offset || item.SpanEnd != selection.Offset+int64(selection.Length) || item.SelectedBytes != selection.Length {
			return errors.New("evidence packet does not preserve the selected revision and span")
		}
		selected += item.SelectedBytes
		if item.Available {
			if item.SourceIntegrityStatus != "verified" || !revisionPattern.MatchString(item.SHA256) || item.Excerpt == "" {
				return errors.New("available evidence is missing verified source integrity")
			}
		} else if item.Excerpt != "" {
			return errors.New("unavailable evidence cannot include an excerpt")
		}
	}
	if selected != packet.SelectedBytes {
		return errors.New("evidence packet selected-byte total is inconsistent")
	}
	return nil
}

func safeComparison(result appquery.CaptureComparison, request appquery.CaptureComparisonRequest) (appquery.CaptureComparison, error) {
	if result.ProjectKey != request.ProjectKey || result.LeftRequestID != request.LeftRequestID || result.LeftRevisionID != request.LeftRevisionID ||
		result.RightRequestID != request.RightRequestID || result.RightRevisionID != request.RightRevisionID {
		return appquery.CaptureComparison{}, ErrComparisonUnavailable
	}
	if !comparisonSideMatches(result.Request, request) || !comparisonSideMatches(result.Response, request) {
		return appquery.CaptureComparison{}, ErrComparisonUnavailable
	}
	result.Request.LeftExcerpt = ""
	result.Request.RightExcerpt = ""
	result.Response.LeftExcerpt = ""
	result.Response.RightExcerpt = ""
	if err := publicdata.Validate(result); err != nil {
		return appquery.CaptureComparison{}, err
	}
	return result, nil
}

func comparisonSideMatches(side appquery.CaptureSideComparison, request appquery.CaptureComparisonRequest) bool {
	if !comparisonSummaryMatches(side.Left, request.LeftRequestID, request.LeftRevisionID) ||
		!comparisonSummaryMatches(side.Right, request.RightRequestID, request.RightRevisionID) {
		return false
	}
	if !side.Available {
		return strings.TrimSpace(side.Reason) != "" && !side.Equal && side.ComparedBytes == 0 &&
			side.FirstDifferenceOffset == nil && side.LeftExcerpt == "" && side.RightExcerpt == ""
	}
	if side.Left.Size < 0 || side.Right.Size < 0 || side.Left.Size > appquery.MaximumComparisonMessageBytes ||
		side.Right.Size > appquery.MaximumComparisonMessageBytes || !revisionPattern.MatchString(side.Left.SHA256) ||
		!revisionPattern.MatchString(side.Right.SHA256) || side.ComparedBytes != max(side.Left.Size, side.Right.Size) {
		return false
	}
	if side.Equal {
		return side.Left.Size == side.Right.Size && side.Left.SHA256 == side.Right.SHA256 && side.FirstDifferenceOffset == nil
	}
	return (side.Left.Size != side.Right.Size || side.Left.SHA256 != side.Right.SHA256) &&
		side.FirstDifferenceOffset != nil && *side.FirstDifferenceOffset >= 0 && *side.FirstDifferenceOffset < side.ComparedBytes
}

func comparisonSummaryMatches(summary appquery.CaptureRevisionSummary, requestID, revisionID string) bool {
	if summary.RequestID != requestID || summary.RevisionID != revisionID || summary.Size < 0 {
		return false
	}
	if summary.SHA256 == "" {
		return summary.Size == 0 && summary.CapturedAt == nil && summary.ParseState == "" && summary.WireFidelity == "" && summary.Truncated == nil
	}
	return revisionPattern.MatchString(summary.SHA256)
}

func hasAvailableEvidence(session Session, requestID, revisionID string) bool {
	for _, ref := range session.References {
		if ref.RequestID == requestID && ref.RevisionID == revisionID && ref.SourceIntegrityStatus == "verified" && revisionPattern.MatchString(ref.SHA256) {
			return true
		}
	}
	return false
}

func assistantVisible(session Session) Session {
	if session.ReviewAccepted {
		return session
	}
	session.Question = ""
	session.Evidence.Question = ""
	session.Evidence.QuestionRedactions = []publicdata.TextRedactionSpan{}
	for index := range session.Evidence.Evidence {
		session.Evidence.Evidence[index].Excerpt = ""
		session.Evidence.Evidence[index].SHA256 = ""
		session.Evidence.Evidence[index].Redactions = []publicdata.TextRedactionSpan{}
	}
	for index := range session.References {
		session.References[index].SHA256 = ""
	}
	session.ContextAnnotations = []ContextAnnotation{}
	session.Comparisons = []ComparisonRecord{}
	session.Proposals = []ProposalRecord{}
	session.Receipts = []ValidationReceipt{}
	session.Candidates = []CandidateVersion{}
	return session
}

// canonicalCandidateReplay resolves legacy idempotency results that predate
// CandidateID. The session's append-only candidate rows are the source of truth.
func canonicalCandidateReplay(session Session, replay CandidateVersion) (CandidateVersion, error) {
	var match *CandidateVersion
	for index := range session.Candidates {
		candidate := &session.Candidates[index]
		if candidate.Version != replay.Version || candidate.ProposalID != replay.ProposalID || candidate.ReceiptID != replay.ReceiptID {
			continue
		}
		if replay.CandidateID != "" && candidate.CandidateID != replay.CandidateID {
			continue
		}
		if replay.SessionVersion > 0 && candidate.SessionVersion != replay.SessionVersion {
			continue
		}
		if replay.State != "" && candidate.State != replay.State {
			continue
		}
		if match != nil {
			return CandidateVersion{}, ErrStoreUnavailable
		}
		match = candidate
	}
	if match == nil {
		return CandidateVersion{}, ErrStoreUnavailable
	}
	return *match, nil
}

func candidateExists(session Session, candidateID string) bool {
	for _, candidate := range session.Candidates {
		if candidate.CandidateID == candidateID {
			return true
		}
	}
	return false
}

func sanitizeProposal(input CandidateProposal) (CandidateProposal, error) {
	proposal := CandidateProposal{}
	var total int
	copyField := func(name, value string, maximum int, required bool) (string, error) {
		if !utf8.ValidString(value) || len(value) > maximum || (required && strings.TrimSpace(value) == "") {
			return "", ErrInvalidInput
		}
		redacted, _, err := publicdata.RedactText(value)
		if err != nil || len(redacted) > maximum {
			return "", ErrInvalidInput
		}
		if err := publicdata.Validate(map[string]string{name: redacted}); err != nil {
			return "", ErrInvalidInput
		}
		total += len(redacted)
		return strings.TrimSpace(redacted), nil
	}
	var err error
	if proposal.Title, err = copyField("title", input.Title, 160, true); err != nil {
		return CandidateProposal{}, err
	}
	if proposal.Summary, err = copyField("summary", input.Summary, 2048, true); err != nil {
		return CandidateProposal{}, err
	}
	if proposal.Rationale, err = copyField("rationale", input.Rationale, 4096, true); err != nil {
		return CandidateProposal{}, err
	}
	if proposal.Impact, err = copyField("impact", input.Impact, 1024, false); err != nil {
		return CandidateProposal{}, err
	}
	if proposal.Alternatives, err = sanitizeTextList("alternatives", input.Alternatives, 5, 1024, &total); err != nil {
		return CandidateProposal{}, err
	}
	if proposal.NextChecks, err = sanitizeTextList("next_checks", input.NextChecks, 8, 512, &total); err != nil {
		return CandidateProposal{}, err
	}
	if proposal.Limitations, err = sanitizeTextList("limitations", input.Limitations, 8, 512, &total); err != nil {
		return CandidateProposal{}, err
	}
	if total > maximumProposalBytes || publicdata.Validate(proposal) != nil {
		return CandidateProposal{}, ErrInvalidInput
	}
	return proposal, nil
}

func sanitizeTextList(name string, values []string, maximumCount, maximumBytes int, total *int) ([]string, error) {
	if len(values) > maximumCount {
		return nil, ErrInvalidInput
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !utf8.ValidString(value) || len(value) > maximumBytes {
			return nil, ErrInvalidInput
		}
		redacted, _, err := publicdata.RedactText(value)
		if err != nil || len(redacted) > maximumBytes || publicdata.Validate(map[string]string{name: redacted}) != nil {
			return nil, ErrInvalidInput
		}
		if strings.TrimSpace(redacted) == "" {
			continue
		}
		result = append(result, strings.TrimSpace(redacted))
		*total += len(redacted)
	}
	return result, nil
}

func validSessionInput(projectKey, sessionID string, expectedVersion int, idempotencyKey string) bool {
	return projectKeyPattern.MatchString(projectKey) && sessionIDPattern.MatchString(sessionID) && expectedVersion > 0 && validIdempotencyKey(idempotencyKey)
}

func validIdempotencyKey(value string) bool { return idempotencyKeyPattern.MatchString(value) }

func validOpaqueIDs(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !sessionIDPattern.MatchString(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func referenceMap(session Session) map[string]EvidenceReference {
	result := make(map[string]EvidenceReference, len(session.References))
	for _, ref := range session.References {
		result[ref.ID] = ref
	}
	return result
}

func comparisonMap(session Session) map[string]ComparisonRecord {
	result := make(map[string]ComparisonRecord, len(session.Comparisons))
	for _, comparison := range session.Comparisons {
		result[comparison.ID] = comparison
	}
	return result
}

func findProposal(session Session, id string) (ProposalRecord, bool) {
	for _, proposal := range session.Proposals {
		if proposal.ID == id {
			return proposal, true
		}
	}
	return ProposalRecord{}, false
}

func findReceipt(session Session, id string) (ValidationReceipt, bool) {
	for _, receipt := range session.Receipts {
		if receipt.ID == id {
			return receipt, true
		}
	}
	return ValidationReceipt{}, false
}

func newID() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("consultation identifier could not be generated")
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > 256<<10 {
		return "", ErrInvalidInput
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func encodeSafe(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > 256<<10 {
		return nil, ErrInvalidInput
	}
	if err := publicdata.ValidateJSON(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func decodeSafe(encoded []byte, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("stored JSON contains trailing data")
	}
	return publicdata.ValidateJSON(encoded)
}
