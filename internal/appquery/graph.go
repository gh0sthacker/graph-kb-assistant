// Package appquery owns typed, read-only application graph queries shared by
// the CLI, HTTP, and MCP adapters.
package appquery

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/search"
	"bb-graph-assistant/internal/store"
)

type GraphView string

type GraphCursor string

const (
	GraphViewDirect      GraphView = "direct"
	GraphViewFull        GraphView = "full"
	maxGraphContextBytes           = 64 << 10
)

type GraphBudget struct {
	MaxNodes int
	MaxEdges int
	MaxDepth int
}

type GraphRequest struct {
	ProjectKey string
	View       GraphView
	SeedIDs    []string
	Budget     GraphBudget
	Context    map[string]any // Full-view context is included in continuation binding.
	Cursor     GraphCursor
}

// GraphResult keeps query metadata typed while preserving the current graph
// payload contract consumed by the UI and MCP clients.
type GraphResult struct {
	ProjectKey   string
	View         GraphView
	GraphVersion string
	CoverageNote string
	Payload      map[string]any
}

type graphCursorData struct {
	Version          int    `json:"version"`
	ProjectKey       string `json:"project_key"`
	SeedHash         string `json:"seed_hash"`
	Generation       int64  `json:"generation"`
	Priority         int    `json:"priority"`
	AfterAssertionID string `json:"after_assertion_id"`
}

func graphContextHash(value map[string]any) (string, error) {
	if len(value) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(encoded) > maxGraphContextBytes {
		return "", errors.New("graph context exceeds the supported size")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (r GraphResult) WireMap() map[string]any {
	return r.Payload
}

type ErrorCode string

const (
	ErrorInvalidInput           ErrorCode = "invalid_input"
	ErrorProjectNotRegistered   ErrorCode = "project_not_registered"
	ErrorProjectUnavailable     ErrorCode = "project_unavailable"
	ErrorGraphUnavailable       ErrorCode = "graph_unavailable"
	ErrorSurfaceUnavailable     ErrorCode = "surface_unavailable"
	ErrorOutputTooLarge         ErrorCode = "output_too_large"
	ErrorEvidenceUnavailable    ErrorCode = "evidence_unavailable"
	ErrorEvidenceKeyUnavailable ErrorCode = "evidence_key_unavailable"
	ErrorStaleCursor            ErrorCode = "stale_cursor"
	ErrorCursorUnsupported      ErrorCode = "cursor_unsupported"
)

type QueryError struct {
	Code    ErrorCode
	Message string
	Err     error
}

func (e *QueryError) Error() string {
	if e == nil {
		return "graph query failed"
	}
	if e.Message != "" {
		return e.Message
	}
	switch e.Code {
	case ErrorInvalidInput:
		return "graph query is invalid"
	case ErrorProjectNotRegistered:
		return "selected project is not registered"
	case ErrorProjectUnavailable:
		return "selected project is unavailable"
	case ErrorSurfaceUnavailable:
		return "local surface catalog is unavailable"
	case ErrorOutputTooLarge:
		return "graph result exceeds the output limit"
	case ErrorStaleCursor:
		return "graph continuation cursor is stale; reload the first page"
	case ErrorCursorUnsupported:
		return "configured graph backend does not support continuation; reload the first page"
	default:
		return "graph data is unavailable"
	}
}

func (e *QueryError) Unwrap() error { return e.Err }

type Service struct {
	dataRoot    string
	projects    map[string]project.CaidoProjectRef
	graph       *graphaccess.Service
	fullCursors *fullCursorStore
	surfaceKey  []byte
}

var (
	projectKeyPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	entitySeedPattern    = regexp.MustCompile(`^[a-z][a-z0-9_]*-[a-f0-9]{64}$`)
	publicLocatorPattern = regexp.MustCompile(`^\$(\.[A-Za-z_][A-Za-z0-9_]*|\[[0-9]+\])*$`)
)

func NewGraphQueryService(dataRoot string, refs []project.CaidoProjectRef, graph *graphaccess.Service) (*Service, error) {
	if strings.TrimSpace(dataRoot) == "" {
		return nil, errors.New("graph query data root is required")
	}
	if graph == nil {
		graph = graphaccess.NewService()
	}
	projects := make(map[string]project.CaidoProjectRef, len(refs))
	for _, ref := range refs {
		key, err := ref.StorageKey()
		if err != nil {
			return nil, errors.New("project catalog contains an invalid identity")
		}
		if _, exists := projects[key]; exists {
			return nil, errors.New("project catalog contains duplicate identities")
		}
		projects[key] = ref
	}
	surfaceKey := make([]byte, 32)
	if _, err := rand.Read(surfaceKey); err != nil {
		return nil, errors.New("surface cursor signing key could not be initialized")
	}
	return &Service{dataRoot: dataRoot, projects: projects, graph: graph, fullCursors: newFullCursorStore(), surfaceKey: surfaceKey}, nil
}

// NewGraphQueryServiceWithPersistentCursors keeps bounded full-view cursor
// state in the private query-state directory so short-lived CLI invocations
// can resume a page. Project capture and graph databases remain read-only.
func NewGraphQueryServiceWithPersistentCursors(dataRoot string, refs []project.CaidoProjectRef, graph *graphaccess.Service) (*Service, error) {
	service, err := NewGraphQueryService(dataRoot, refs, graph)
	if err != nil {
		return nil, err
	}
	cursors, err := openPersistentFullCursorStore(filepath.Join(dataRoot, "query-state"))
	if err != nil {
		_ = service.Close(context.Background())
		return nil, errors.New("persistent graph continuation state is unavailable")
	}
	service.fullCursors = cursors
	return service, nil
}

func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.fullCursors.clear()
	var cursorErr error
	if s.fullCursors != nil {
		cursorErr = s.fullCursors.close()
	}
	if s.graph == nil {
		return cursorErr
	}
	return errors.Join(cursorErr, s.graph.Close(ctx))
}

func (s *Service) Graph(ctx context.Context, request GraphRequest) (GraphResult, error) {
	if s == nil || s.graph == nil {
		return GraphResult{}, queryError(ErrorGraphUnavailable, "graph query service is unavailable", nil)
	}
	if err := validateGraphRequest(request); err != nil {
		return GraphResult{}, queryError(ErrorInvalidInput, err.Error(), err)
	}
	contextHash, err := graphContextHash(request.Context)
	if err != nil {
		return GraphResult{}, queryError(ErrorInvalidInput, "graph context is invalid or exceeds its limit", err)
	}
	ref, ok := s.projects[request.ProjectKey]
	if !ok {
		return GraphResult{}, queryError(ErrorProjectNotRegistered, "selected project is not in the local project catalog", nil)
	}
	if request.View == GraphViewFull && request.Cursor != "" && graphaccess.ConfiguredBackend() != "neo4j" {
		return GraphResult{}, queryError(ErrorCursorUnsupported, "full analytical continuation requires the Neo4j backend", nil)
	}
	var backendCursor *neo4jstore.NeighborhoodCursor
	if request.View == GraphViewDirect && request.Cursor != "" {
		backendCursor, err = decodeGraphCursor(string(request.Cursor), request.ProjectKey, request.SeedIDs)
		if err != nil {
			return GraphResult{}, err
		}
	}
	selected, err := store.OpenProjectReadOnly(ctx, ref, s.dataRoot)
	if err != nil {
		return GraphResult{}, queryError(ErrorProjectUnavailable, "selected project is unavailable for graph reads", err)
	}
	defer selected.Close()

	var payload map[string]any
	var version, coverage string
	responsePrepared := false
	switch request.View {
	case GraphViewDirect:
		page, pageErr := search.GraphNeighborhoodPageWithService(ctx, s.graph, selected, request.SeedIDs, backendCursor)
		err, payload = pageErr, page.Payload
		if errors.Is(err, neo4jstore.ErrNeighborhoodCursorStale) {
			return GraphResult{}, queryError(ErrorStaleCursor, "graph continuation cursor is stale; reload the first page", err)
		}
		if errors.Is(err, graphaccess.ErrNeighborhoodCursorUnsupported) {
			return GraphResult{}, queryError(ErrorCursorUnsupported, "configured graph backend does not support continuation; reload the first page", err)
		}
		if err == nil && page.NextCursor != nil {
			token, tokenErr := encodeGraphCursor(graphCursorData{
				Version: 1, ProjectKey: request.ProjectKey, SeedHash: graphSeedHash(request.SeedIDs),
				Generation: page.NextCursor.Generation, Priority: page.NextCursor.Priority,
				AfterAssertionID: page.NextCursor.AfterAssertionID,
			})
			if tokenErr != nil {
				return GraphResult{}, queryError(ErrorGraphUnavailable, "graph continuation cursor could not be encoded", tokenErr)
			}
			payload["next_cursor"] = token
		} else if err == nil {
			delete(payload, "next_cursor")
		}
		version = "behavior-graph-direct-v1"
		coverage = "Direct structural neighborhood; coverage and full-project evidence integrity remain partial."
	case GraphViewFull:
		version = "behavior-graph-source-pinned-v1"
		coverage = "Graph entities and edges describe captured evidence; they do not establish vulnerability or impact."
		budget := effectiveGraphBudget(request.Budget)
		binding := fullCursorBinding{
			ProjectKey: request.ProjectKey, SeedHash: graphSeedHash(request.SeedIDs), Budget: budget,
			ContextHash: contextHash,
		}
		if request.Cursor == "" {
			payload, err = search.GraphSliceWithService(ctx, s.graph, selected, request.SeedIDs, budget.MaxNodes, budget.MaxEdges, budget.MaxDepth, request.Context)
			if err == nil {
				payload["continuation_scope"] = "rendered_and_native_candidate_branches"
				branchSeeds := fullContinuationBranchSeeds(payload)
				candidateLimits := graphCandidateLimitReached(payload)
				branchListTruncated := graphPayloadBool(payload["missing_branches_truncated"])
				if len(branchSeeds) > 0 && graphaccess.ConfiguredBackend() == "neo4j" && graphPayloadGeneration(payload) > 0 {
					token, cursorErr := s.fullCursors.create(binding, graphPayloadGeneration(payload), branchSeeds, payload)
					if cursorErr == nil {
						payload["next_cursor"] = token
						payload["continuation_state"] = "available"
						if branchListTruncated {
							payload["continuation_state"] = "available_with_truncated_branch_list"
							payload["continuation_branches_truncated"] = true
						}
					} else {
						payload["continuation_state"] = "unavailable"
						appendGraphLimitation(payload, "Rendered or native frontier branches could not be queued for continuation; reload and retry this focused slice.")
					}
				} else if len(branchSeeds) > 0 && graphaccess.ConfiguredBackend() != "neo4j" {
					payload["continuation_state"] = "unsupported_backend"
					appendGraphLimitation(payload, "This graph backend cannot pin candidate-branch continuation to a Neo4j generation.")
				} else if len(branchSeeds) > 0 {
					payload["continuation_state"] = "unavailable"
					appendGraphLimitation(payload, "Neo4j did not provide a generation for the candidate-branch cursor; reload and retry this focused slice.")
				} else if branchListTruncated {
					payload["continuation_state"] = "branch_list_truncated"
					payload["continuation_branches_truncated"] = true
				} else if candidateLimits {
					payload["continuation_state"] = "bounded_candidates_remain"
				} else {
					payload["continuation_state"] = "no_resumable_rendered_branches"
				}
				if candidateLimits {
					payload["continuation_candidate_limits_reached"] = true
					appendGraphLimitation(payload, "This cursor continues rendered omissions and queued native assertion/frontier candidates. Candidate queues are bounded; card and provenance limits still need a narrower query or a later continuation feature.")
				}
			}
		} else {
			payload, _, _, err = s.fullCursors.next(string(request.Cursor), binding, func(branches []string, accumulated map[string]any, page int) (map[string]any, []string, int64, error) {
				pagePayload, pageErr := search.ContinueGraphSliceWithService(
					ctx, s.graph, selected, request.SeedIDs, branches,
					budget.MaxNodes, budget.MaxEdges, budget.MaxDepth, page, request.Context, accumulated,
				)
				if pageErr != nil {
					if errors.Is(pageErr, search.ErrAnalyticalGenerationChanged) {
						return nil, nil, 0, queryError(ErrorStaleCursor, "Neo4j project changed during graph continuation; reload the first page", pageErr)
					}
					return nil, nil, 0, queryError(ErrorGraphUnavailable, "full graph continuation page could not be read", pageErr)
				}
				pagePayload["continuation_scope"] = "rendered_and_native_candidate_branches"
				if graphCandidateLimitReached(pagePayload) {
					pagePayload["continuation_candidate_limits_reached"] = true
					appendGraphLimitation(pagePayload, "This cursor continues rendered omissions and queued native assertion/frontier candidates. Candidate queues are bounded; card and provenance limits still need a narrower query or a later continuation feature.")
				}
				return pagePayload, fullContinuationBranchSeeds(pagePayload), graphPayloadGeneration(pagePayload), nil
			}, func(pagePayload map[string]any) (map[string]any, error) {
				prepared, prepareErr := prepareGraphPayload(pagePayload, version, coverage)
				if prepareErr != nil {
					return nil, prepareErr
				}
				if sizeErr := validateFullGraphWireSize(prepared); sizeErr != nil {
					return nil, sizeErr
				}
				return prepared, nil
			})
			if err == nil {
				responsePrepared = true
			}
		}
	default:
		return GraphResult{}, queryError(ErrorInvalidInput, "graph view must be direct or full", nil)
	}
	if err != nil {
		var queryErr *QueryError
		if errors.As(err, &queryErr) {
			return GraphResult{}, queryErr
		}
		return GraphResult{}, queryError(ErrorGraphUnavailable, "selected graph view is unavailable or exceeds its limits", err)
	}
	redacted := payload
	if !responsePrepared {
		redacted, err = prepareGraphPayload(payload, version, coverage)
		if err != nil {
			if request.View == GraphViewFull && request.Cursor == "" {
				if token, ok := payload["next_cursor"].(string); ok {
					s.fullCursors.discard(token)
				}
			}
			return GraphResult{}, err
		}
		if request.View == GraphViewFull {
			if err := validateFullGraphWireSize(redacted); err != nil {
				if request.Cursor == "" {
					if token, ok := payload["next_cursor"].(string); ok {
						s.fullCursors.discard(token)
					}
				}
				return GraphResult{}, err
			}
		}
	}
	return GraphResult{
		ProjectKey: request.ProjectKey, View: request.View, GraphVersion: version,
		CoverageNote: coverage, Payload: redacted,
	}, nil
}

func prepareGraphPayload(payload map[string]any, version, coverage string) (map[string]any, error) {
	if payload == nil {
		return nil, queryError(ErrorGraphUnavailable, "graph result could not be prepared", nil)
	}
	prepared, err := cloneGraphPayload(payload)
	if err != nil {
		return nil, queryError(ErrorGraphUnavailable, "graph result could not be prepared", err)
	}
	prepared["graph_version"] = version
	prepared["coverage_note"] = coverage
	redacted, paths, locators, err := redactSourcePaths(prepared)
	if err != nil {
		return nil, queryError(ErrorGraphUnavailable, "graph result could not be prepared", err)
	}
	redacted["local_source_paths_redacted"] = paths
	redacted["unsafe_source_locators_redacted"] = locators
	redacted, err = fingerprintGraphPayload(redacted)
	if err != nil {
		return nil, queryError(ErrorGraphUnavailable, "graph result could not be fingerprinted", err)
	}
	return redacted, nil
}

func validateFullGraphWireSize(payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return queryError(ErrorGraphUnavailable, "full graph response could not be encoded", err)
	}
	if len(encoded) > maxFullCursorReplayPayloadBytes {
		return queryError(ErrorOutputTooLarge, "full graph response exceeds the shared wire limit; reload with a narrower seed set", nil)
	}
	return nil
}

func fingerprintGraphPayload(payload map[string]any) (map[string]any, error) {
	delete(payload, "result_id")
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	payload["result_id"] = "result-" + hex.EncodeToString(digest[:])
	return payload, nil
}

func graphSeedHash(seedIDs []string) string {
	seeds := append([]string(nil), seedIDs...)
	sort.Strings(seeds)
	encoded, _ := json.Marshal(seeds)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func effectiveGraphBudget(budget GraphBudget) GraphBudget {
	if budget == (GraphBudget{}) {
		return GraphBudget{MaxNodes: 80, MaxEdges: 160, MaxDepth: 3}
	}
	return budget
}

func graphPayloadGeneration(payload map[string]any) int64 {
	if payload == nil {
		return 0
	}
	switch value := payload["graph_generation"].(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case json.Number:
		generation, err := value.Int64()
		if err == nil {
			return generation
		}
	case float64:
		if value >= 1 && value == float64(int64(value)) {
			return int64(value)
		}
	}
	return 0
}

func fullContinuationBranchSeeds(payload map[string]any) []string {
	if payload == nil {
		return nil
	}
	values, _ := payload["missing_branches"]
	branches := make([]map[string]any, 0)
	switch rows := values.(type) {
	case []behaviorgraph.Record:
		for _, row := range rows {
			branches = append(branches, row)
		}
	case []map[string]any:
		branches = append(branches, rows...)
	case []any:
		for _, item := range rows {
			if row, ok := item.(map[string]any); ok {
				branches = append(branches, row)
			}
		}
	}
	unique := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		reason, _ := branch["reason"].(string)
		var id string
		switch reason {
		case "max_nodes", "max_edges", "mandatory_evidence_branch_omitted", "analytical_assertion_candidate_limit":
			id, _ = branch["assertion_id"].(string)
			if !strings.HasPrefix(id, "assertion-") {
				continue
			}
		case "analytical_frontier_node_limit":
			id, _ = branch["frontier_id"].(string)
			if strings.HasPrefix(id, "source-") || strings.HasPrefix(id, "card-") {
				continue
			}
		default:
			continue
		}
		if !entitySeedPattern.MatchString(id) {
			continue
		}
		unique[id] = struct{}{}
	}
	seeds := make([]string, 0, len(unique))
	for id := range unique {
		seeds = append(seeds, id)
	}
	sort.Strings(seeds)
	if len(seeds) > maxFullCursorPendingBranches {
		payload["missing_branches_truncated"] = true
		seeds = seeds[:maxFullCursorPendingBranches]
	}
	return seeds
}

func graphCandidateLimitReached(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	for _, key := range []string{
		"candidate_assertion_limit_reached", "candidate_assertion_exclusion_truncated", "analytical_frontier_node_limit_reached",
		"candidate_card_limit_reached", "provenance_truncated", "source_integrity_budget_exceeded",
	} {
		if reached, _ := payload[key].(bool); reached {
			return true
		}
	}
	return false
}

func appendGraphLimitation(payload map[string]any, limitation string) {
	if payload == nil || strings.TrimSpace(limitation) == "" {
		return
	}
	limitations := make([]string, 0)
	switch values := payload["limitations"].(type) {
	case []string:
		limitations = append(limitations, values...)
	case []any:
		for _, value := range values {
			if text, ok := value.(string); ok {
				limitations = append(limitations, text)
			}
		}
	}
	for _, existing := range limitations {
		if existing == limitation {
			return
		}
	}
	payload["limitations"] = append(limitations, limitation)
}

func encodeGraphCursor(cursor graphCursorData) (string, error) {
	if cursor.Version != 1 || !projectKeyPattern.MatchString(cursor.ProjectKey) ||
		!projectKeyPattern.MatchString(cursor.SeedHash) || cursor.Generation < 1 ||
		(cursor.Priority != 0 && cursor.Priority != 1) || !entitySeedPattern.MatchString(cursor.AfterAssertionID) ||
		!strings.HasPrefix(cursor.AfterAssertionID, "assertion-") {
		return "", errors.New("graph continuation cursor is malformed")
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeGraphCursor(token, projectKey string, seeds []string) (*neo4jstore.NeighborhoodCursor, error) {
	if len(token) < 1 || len(token) > 4096 || !projectKeyPattern.MatchString(projectKey) {
		return nil, queryError(ErrorInvalidInput, "graph continuation cursor is malformed", nil)
	}
	encoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(encoded) > 3072 {
		return nil, queryError(ErrorInvalidInput, "graph continuation cursor is malformed", nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var cursor graphCursorData
	if err := decoder.Decode(&cursor); err != nil {
		return nil, queryError(ErrorInvalidInput, "graph continuation cursor is malformed", nil)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, queryError(ErrorInvalidInput, "graph continuation cursor is malformed", nil)
	}
	if cursor.Version != 1 || cursor.ProjectKey != projectKey || cursor.SeedHash != graphSeedHash(seeds) ||
		cursor.Generation < 1 || (cursor.Priority != 0 && cursor.Priority != 1) ||
		!entitySeedPattern.MatchString(cursor.AfterAssertionID) || !strings.HasPrefix(cursor.AfterAssertionID, "assertion-") {
		return nil, queryError(ErrorInvalidInput, "graph continuation cursor does not match this project and seed set", nil)
	}
	return &neo4jstore.NeighborhoodCursor{
		Generation: cursor.Generation, Priority: cursor.Priority, AfterAssertionID: cursor.AfterAssertionID,
	}, nil
}

func validateGraphRequest(request GraphRequest) error {
	if request.View != GraphViewDirect && request.View != GraphViewFull {
		return errors.New("graph view must be direct or full")
	}
	if request.View == GraphViewFull && request.Cursor != "" && !fullCursorTokenPattern.MatchString(string(request.Cursor)) {
		return errors.New("full graph continuation cursor is malformed")
	}
	if !projectKeyPattern.MatchString(request.ProjectKey) {
		if request.View == GraphViewDirect {
			return errors.New("select one project and 1-20 graph seeds")
		}
		return errors.New("project key has an invalid format")
	}
	maxSeeds := 80
	if request.View == GraphViewDirect {
		maxSeeds = 20
	}
	if len(request.SeedIDs) < 1 || len(request.SeedIDs) > maxSeeds {
		if request.View == GraphViewDirect {
			return errors.New("select one project and 1-20 graph seeds")
		}
		return fmt.Errorf("provide between 1 and %d graph seed IDs", maxSeeds)
	}
	seen := make(map[string]struct{}, len(request.SeedIDs))
	for _, seed := range request.SeedIDs {
		if strings.TrimSpace(seed) != seed || seed == "" || len(seed) > 512 {
			if request.View == GraphViewDirect {
				return errors.New("graph neighborhood seed ID is malformed")
			}
			return errors.New("graph seed ID is empty, too long, or malformed")
		}
		if request.View == GraphViewDirect && (!entitySeedPattern.MatchString(seed) || strings.HasPrefix(seed, "source-") || strings.HasPrefix(seed, "card-") || strings.HasPrefix(seed, "assertion-")) {
			return errors.New("graph neighborhood seed ID is malformed")
		}
		if _, exists := seen[seed]; exists {
			if request.View == GraphViewDirect {
				return errors.New("graph neighborhood seed IDs must be unique")
			}
			return errors.New("graph seed IDs must be unique")
		}
		seen[seed] = struct{}{}
	}
	if request.View == GraphViewFull && request.Budget != (GraphBudget{}) {
		b := effectiveGraphBudget(request.Budget)
		if b.MaxNodes < 1 || b.MaxNodes > 100 || b.MaxEdges < 0 || b.MaxEdges > 300 || b.MaxDepth < 0 || b.MaxDepth > 6 {
			return errors.New("graph slice limits are outside the supported range")
		}
	}
	if request.View == GraphViewDirect && request.Budget != (GraphBudget{}) {
		return errors.New("direct graph queries do not accept a slice budget")
	}
	if request.View == GraphViewDirect && len(request.Context) != 0 {
		return errors.New("direct graph queries do not accept an analysis context")
	}
	if _, err := graphContextHash(request.Context); err != nil {
		return errors.New("graph context is invalid or exceeds its limit")
	}
	return nil
}

func queryError(code ErrorCode, message string, cause error) *QueryError {
	if cause == nil {
		return &QueryError{Code: code, Message: message, Err: errors.New(message)}
	}
	return &QueryError{Code: code, Message: message, Err: fmt.Errorf("%s: %w", message, cause)}
}

func redactSourcePaths(input map[string]any) (map[string]any, int, int, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, 0, 0, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var output map[string]any
	if err := decoder.Decode(&output); err != nil {
		return nil, 0, 0, err
	}
	paths, locators := stripSourcePaths(output)
	return output, paths, locators, nil
}

func stripSourcePaths(value any) (int, int) {
	switch node := value.(type) {
	case map[string]any:
		paths, locators := 0, 0
		_, hasPath := node["path"]
		_, hasDigest := node["sha256"]
		locator, hasLocator := node["record_locator"].(string)
		isSourceRef := hasDigest && hasLocator
		if isSourceRef && hasPath {
			delete(node, "path")
			node["path_redacted"] = true
			paths++
		}
		if isSourceRef && !publicLocatorPattern.MatchString(locator) {
			node["record_locator"] = "[REDACTED_UNSAFE_LOCATOR]"
			node["record_locator_redacted"] = true
			locators++
		}
		for _, child := range node {
			p, l := stripSourcePaths(child)
			paths += p
			locators += l
		}
		return paths, locators
	case []any:
		paths, locators := 0, 0
		for _, child := range node {
			p, l := stripSourcePaths(child)
			paths += p
			locators += l
		}
		return paths, locators
	default:
		return 0, 0
	}
}
