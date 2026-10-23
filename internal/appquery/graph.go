// Package appquery owns typed, read-only application graph queries shared by
// the HTTP and MCP adapters.
package appquery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/search"
	"bb-graph-assistant/internal/store"
)

type GraphView string

type GraphCursor string

const (
	GraphViewDirect GraphView = "direct"
	GraphViewFull   GraphView = "full"
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

func (r GraphResult) WireMap() map[string]any {
	return r.Payload
}

type ErrorCode string

const (
	ErrorInvalidInput         ErrorCode = "invalid_input"
	ErrorProjectNotRegistered ErrorCode = "project_not_registered"
	ErrorProjectUnavailable   ErrorCode = "project_unavailable"
	ErrorGraphUnavailable     ErrorCode = "graph_unavailable"
	ErrorOutputTooLarge       ErrorCode = "output_too_large"
	ErrorStaleCursor          ErrorCode = "stale_cursor"
	ErrorCursorUnsupported    ErrorCode = "cursor_unsupported"
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
	dataRoot string
	projects map[string]project.CaidoProjectRef
	graph    *graphaccess.Service
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
	return &Service{dataRoot: dataRoot, projects: projects, graph: graph}, nil
}

func (s *Service) Close(ctx context.Context) error {
	if s == nil || s.graph == nil {
		return nil
	}
	return s.graph.Close(ctx)
}

func (s *Service) Graph(ctx context.Context, request GraphRequest) (GraphResult, error) {
	if s == nil || s.graph == nil {
		return GraphResult{}, queryError(ErrorGraphUnavailable, "graph query service is unavailable", nil)
	}
	if err := validateGraphRequest(request); err != nil {
		return GraphResult{}, queryError(ErrorInvalidInput, err.Error(), err)
	}
	ref, ok := s.projects[request.ProjectKey]
	if !ok {
		return GraphResult{}, queryError(ErrorProjectNotRegistered, "selected project is not in the local project catalog", nil)
	}
	var backendCursor *neo4jstore.NeighborhoodCursor
	var err error
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
		budget := request.Budget
		if budget == (GraphBudget{}) {
			budget = GraphBudget{MaxNodes: 80, MaxEdges: 160, MaxDepth: 3}
		}
		payload, err = search.GraphSliceWithService(ctx, s.graph, selected, request.SeedIDs, budget.MaxNodes, budget.MaxEdges, budget.MaxDepth, nil)
		version = "behavior-graph-source-pinned-v1"
		coverage = "Graph entities and edges describe captured evidence; they do not establish vulnerability or impact."
	default:
		return GraphResult{}, queryError(ErrorInvalidInput, "graph view must be direct or full", nil)
	}
	if err != nil {
		return GraphResult{}, queryError(ErrorGraphUnavailable, "selected graph view is unavailable or exceeds its limits", err)
	}
	payload["graph_version"] = version
	payload["coverage_note"] = coverage
	redacted, paths, locators, err := redactSourcePaths(payload)
	if err != nil {
		return GraphResult{}, queryError(ErrorGraphUnavailable, "graph result could not be prepared", err)
	}
	redacted["local_source_paths_redacted"] = paths
	redacted["unsafe_source_locators_redacted"] = locators
	redacted, err = fingerprintGraphPayload(redacted)
	if err != nil {
		return GraphResult{}, queryError(ErrorGraphUnavailable, "graph result could not be fingerprinted", err)
	}
	return GraphResult{
		ProjectKey: request.ProjectKey, View: request.View, GraphVersion: version,
		CoverageNote: coverage, Payload: redacted,
	}, nil
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
	if request.View == GraphViewFull && request.Cursor != "" {
		return errors.New("full graph slices do not accept direct-view cursors")
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
		b := request.Budget
		if b.MaxNodes < 1 || b.MaxNodes > 100 || b.MaxEdges < 0 || b.MaxEdges > 300 || b.MaxDepth < 0 || b.MaxDepth > 6 {
			return errors.New("graph slice limits are outside the supported range")
		}
	}
	if request.View == GraphViewDirect && request.Budget != (GraphBudget{}) {
		return errors.New("direct graph queries do not accept a slice budget")
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
