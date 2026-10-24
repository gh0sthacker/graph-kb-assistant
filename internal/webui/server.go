// Package webui serves the local graph explorer and consultation workspace.
package webui

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/consultation"
)

const (
	maxGraphSeeds    = 20
	maxGraphResponse = 8 << 20
	requestTimeout   = 30 * time.Second
	graphTimeout     = 3 * time.Minute
)

var (
	projectKeyPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	captureRevisionRegex = regexp.MustCompile(`^[a-f0-9]{64}$`)
	sameOriginPostRoutes = map[string]struct{}{
		"/api/evidence/packet-preview": {}, "/api/consultation/session": {},
		"/api/consultation/accept-review": {}, "/api/consultation/compare": {},
		"/api/consultation/context-annotation": {},
		"/api/consultation/proposal":           {}, "/api/consultation/validate": {},
		"/api/consultation/candidate": {},
	}
)

//go:embed assets/*
var assets embed.FS

type readAPI interface {
	GraphExplorerProjects(context.Context, string, int) (map[string]any, error)
	GraphExplorerSurface(context.Context, appquery.SurfaceRequest) (appquery.SurfaceResult, error)
	Graph(context.Context, appquery.GraphRequest) (appquery.GraphResult, error)
	CaptureMessageRange(context.Context, appquery.CaptureMessageRangeRequest) (appquery.CaptureMessageRange, error)
	CompareCaptureMessages(context.Context, appquery.CaptureComparisonRequest) (appquery.CaptureComparison, error)
	BuildEvidencePacket(context.Context, appquery.EvidencePacketRequest) (appquery.EvidencePacket, error)
}

type consultationAPI interface {
	CreateSession(context.Context, appquery.EvidencePacketRequest, string) (consultation.Session, error)
	Resume(context.Context, string, string) (consultation.Session, error)
	ReviewSession(context.Context, string, string) (consultation.Session, error)
	AcceptReview(context.Context, string, string, int, string) (consultation.Session, error)
	RecordContextAnnotation(context.Context, consultation.RecordContextAnnotationRequest) (consultation.ContextAnnotation, error)
	Compare(context.Context, string, string, int, string, appquery.CaptureComparisonRequest) (consultation.ComparisonRecord, error)
	RecordProposal(context.Context, consultation.RecordProposalRequest) (consultation.ProposalRecord, error)
	ValidateProposal(context.Context, consultation.ValidateProposalRequest) (consultation.ValidationReceipt, error)
	SaveCandidate(context.Context, consultation.SaveCandidateRequest) (consultation.CandidateVersion, error)
}

// NewHandler constructs the local UI and its narrowly scoped API.
func NewHandler(api readAPI) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("graph explorer API is unavailable")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeError(w, http.StatusNotFound, "route is unavailable")
			return
		}
		serveAsset("assets/index.html", "text/html; charset=utf-8")(w, r)
	})
	mux.HandleFunc("GET /assets/app.js", serveAsset("assets/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /assets/graph-focus.js", serveAsset("assets/graph-focus.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /assets/style.css", serveAsset("assets/style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /api/projects", projectsHandler(api))
	mux.HandleFunc("GET /api/surface", surfaceHandler(api))
	mux.HandleFunc("GET /api/graph", graphHandler(api))
	mux.HandleFunc("GET /api/evidence", captureMessageRangeHandler(api))
	mux.HandleFunc("GET /api/compare", captureComparisonHandler(api))
	mux.HandleFunc("POST /api/evidence/packet-preview", evidencePacketPreviewHandler(api))
	if consultations, ok := api.(consultationAPI); ok {
		mux.HandleFunc("GET /api/consultation/session", consultationResumeHandler(consultations))
		mux.HandleFunc("GET /api/consultation/review", consultationReviewHandler(consultations))
		mux.HandleFunc("POST /api/consultation/session", consultationCreateHandler(consultations))
		mux.HandleFunc("POST /api/consultation/accept-review", consultationAcceptReviewHandler(consultations))
		mux.HandleFunc("POST /api/consultation/context-annotation", consultationContextAnnotationHandler(consultations))
		mux.HandleFunc("POST /api/consultation/compare", consultationCompareHandler(consultations))
		mux.HandleFunc("POST /api/consultation/proposal", consultationProposalHandler(consultations))
		mux.HandleFunc("POST /api/consultation/validate", consultationValidateHandler(consultations))
		mux.HandleFunc("POST /api/consultation/candidate", consultationCandidateHandler(consultations))
	}
	return securityHeaders(localRequestGate(mux)), nil
}

// ValidateLoopbackAddress rejects wildcard and non-loopback listeners.
func ValidateLoopbackAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return errors.New("web UI address must include a loopback host and port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("web UI port must be between 1 and 65535")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("web UI can only bind to localhost or a loopback IP address")
}

func projectsHandler(api readAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		after := query.Get("after_project_key")
		if after != "" && !projectKeyPattern.MatchString(after) {
			writeError(w, http.StatusBadRequest, "project cursor is invalid")
			return
		}
		limit := 200
		if value := query.Get("limit"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 200 {
				writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
				return
			}
			limit = parsed
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.GraphExplorerProjects(ctx, after, limit)
		writeResult(w, result, err)
	}
}

func surfaceHandler(api readAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		projectValues, projectPresent := query["project_key"]
		if !projectPresent || len(projectValues) != 1 {
			writeError(w, http.StatusBadRequest, "project_key must identify one registered project")
			return
		}
		projectKey := projectValues[0]
		if !projectKeyPattern.MatchString(projectKey) {
			writeError(w, http.StatusBadRequest, "project_key must identify one registered project")
			return
		}
		filterValues, hasFilter := query["filter"]
		if hasFilter && len(filterValues) != 1 {
			writeError(w, http.StatusBadRequest, "surface filter is malformed")
			return
		}
		filter := ""
		if hasFilter {
			filter = filterValues[0]
		}
		if len(filter) > 256 {
			writeError(w, http.StatusBadRequest, "surface filter exceeds 256 bytes")
			return
		}
		cursorValues, hasCursor := query["cursor"]
		if hasCursor && (len(cursorValues) != 1 || len(cursorValues[0]) > appquery.MaximumSurfaceCursorBytes) {
			writeError(w, http.StatusBadRequest, "surface cursor is malformed")
			return
		}
		cursor := ""
		if hasCursor {
			cursor = cursorValues[0]
		}
		limit := 40
		limitValues, hasLimit := query["limit"]
		if hasLimit && len(limitValues) != 1 {
			writeError(w, http.StatusBadRequest, "surface limit is malformed")
			return
		}
		if hasLimit {
			value := limitValues[0]
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 100 {
				writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
				return
			}
			limit = parsed
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, callErr := api.GraphExplorerSurface(ctx, appquery.SurfaceRequest{
			ProjectKey: projectKey, Filter: filter, Cursor: cursor, Limit: limit,
		})
		writeGraphResult(w, result.WireMap(), callErr)
	}
}

func graphHandler(api readAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		projectKey := query.Get("project_key")
		seeds := query["seed"]
		cursorValues, hasCursor := query["cursor"]
		if hasCursor && len(cursorValues) != 1 {
			writeError(w, http.StatusBadRequest, "graph cursor is malformed")
			return
		}
		cursor := query.Get("cursor")
		view := query.Get("view")
		if view != "" && view != "direct" && view != "full" {
			writeError(w, http.StatusBadRequest, "graph view must be direct or full")
			return
		}
		if !projectKeyPattern.MatchString(projectKey) || len(seeds) == 0 || len(seeds) > maxGraphSeeds {
			writeError(w, http.StatusBadRequest, "select one project and between 1 and 20 graph seeds")
			return
		}
		seen := make(map[string]struct{}, len(seeds))
		for _, seed := range seeds {
			if strings.TrimSpace(seed) != seed || seed == "" || len(seed) > 512 {
				writeError(w, http.StatusBadRequest, "graph seed is malformed")
				return
			}
			if _, exists := seen[seed]; exists {
				writeError(w, http.StatusBadRequest, "graph seeds must be unique")
				return
			}
			seen[seed] = struct{}{}
		}
		timeout := requestTimeout
		if view == "full" {
			timeout = graphTimeout
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		graphView := appquery.GraphViewDirect
		if view == "full" {
			graphView = appquery.GraphViewFull
		}
		result, err := api.Graph(ctx, appquery.GraphRequest{ProjectKey: projectKey, View: graphView, SeedIDs: seeds, Cursor: appquery.GraphCursor(cursor)})
		if err != nil {
			writeGraphResult(w, nil, err)
			return
		}
		writeGraphResult(w, result.WireMap(), nil)
	}
}

func captureMessageRangeHandler(api readAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		allowed := map[string]struct{}{
			"project_key": {}, "request_id": {}, "revision_id": {}, "side": {}, "offset": {}, "max_bytes": {},
		}
		for key := range query {
			if _, ok := allowed[key]; !ok {
				writeError(w, http.StatusBadRequest, "capture evidence query contains an unsupported field")
				return
			}
		}
		value := func(key string, required bool) (string, bool) {
			values, exists := query[key]
			if !exists {
				return "", !required
			}
			if len(values) != 1 || required && values[0] == "" {
				return "", false
			}
			return values[0], true
		}
		projectKey, projectOK := value("project_key", true)
		requestID, requestOK := value("request_id", true)
		revisionID, revisionOK := value("revision_id", false)
		side, sideOK := value("side", true)
		offsetValue, offsetOK := value("offset", false)
		maximumValue, maximumOK := value("max_bytes", false)
		if !projectOK || !projectKeyPattern.MatchString(projectKey) || !requestOK || len(requestID) > 512 ||
			!revisionOK || len(revisionID) > 64 || !sideOK || side != "request" && side != "response" || !offsetOK || !maximumOK {
			writeError(w, http.StatusBadRequest, "capture evidence query is malformed")
			return
		}
		offset := int64(0)
		if offsetValue != "" {
			parsed, err := strconv.ParseInt(offsetValue, 10, 64)
			if err != nil || parsed < 0 {
				writeError(w, http.StatusBadRequest, "capture evidence offset is malformed")
				return
			}
			offset = parsed
		}
		maximum := appquery.MaximumEvidenceRangeBytes
		if maximumValue != "" {
			parsed, err := strconv.Atoi(maximumValue)
			if err != nil || parsed < 1 || parsed > appquery.MaximumEvidenceRangeBytes {
				writeError(w, http.StatusBadRequest, "capture evidence range limit is malformed")
				return
			}
			maximum = parsed
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.CaptureMessageRange(ctx, appquery.CaptureMessageRangeRequest{
			ProjectKey: projectKey, RequestID: requestID, RevisionID: revisionID,
			Side: side, Offset: offset, Maximum: maximum,
		})
		writeEvidenceResult(w, result, err)
	}
}

func captureComparisonHandler(api readAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		allowed := map[string]struct{}{
			"project_key": {}, "left_request_id": {}, "left_revision_id": {},
			"right_request_id": {}, "right_revision_id": {},
		}
		for key := range query {
			if _, ok := allowed[key]; !ok {
				writeError(w, http.StatusBadRequest, "capture comparison query contains an unsupported field")
				return
			}
		}
		value := func(key string) (string, bool) {
			values, exists := query[key]
			if !exists || len(values) != 1 || values[0] == "" {
				return "", false
			}
			return values[0], true
		}
		projectKey, projectOK := value("project_key")
		leftRequestID, leftRequestOK := value("left_request_id")
		leftRevisionID, leftRevisionOK := value("left_revision_id")
		rightRequestID, rightRequestOK := value("right_request_id")
		rightRevisionID, rightRevisionOK := value("right_revision_id")
		if !projectOK || !projectKeyPattern.MatchString(projectKey) || !leftRequestOK || strings.TrimSpace(leftRequestID) == "" || len(leftRequestID) > 512 ||
			!leftRevisionOK || !captureRevisionRegex.MatchString(leftRevisionID) || !rightRequestOK || strings.TrimSpace(rightRequestID) == "" || len(rightRequestID) > 512 ||
			!rightRevisionOK || !captureRevisionRegex.MatchString(rightRevisionID) {
			writeError(w, http.StatusBadRequest, "capture comparison query is malformed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.CompareCaptureMessages(ctx, appquery.CaptureComparisonRequest{
			ProjectKey: projectKey, LeftRequestID: leftRequestID, LeftRevisionID: leftRevisionID,
			RightRequestID: rightRequestID, RightRevisionID: rightRevisionID,
		})
		writeComparisonResult(w, result, err)
	}
}

func evidencePacketPreviewHandler(api readAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.EqualFold(mediaType, "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, "evidence packet preview requires application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var request appquery.EvidencePacketRequest
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "evidence packet request is malformed or too large")
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "evidence packet request must contain one JSON object")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.BuildEvidencePacket(ctx, request)
		writeEvidencePacketResult(w, result, err)
	}
}

func optionalNonnegativeInt64(value string) (int64, bool, error) {
	if value == "" {
		return 0, false, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, true, errors.New("invalid nonnegative integer")
	}
	return parsed, true, nil
}

func serveAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		data, err := assets.ReadFile(name)
		if err != nil {
			writeError(w, http.StatusNotFound, "asset is unavailable")
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

func writeResult(w http.ResponseWriter, result map[string]any, err error) {
	if err != nil {
		writeError(w, http.StatusBadRequest, "local graph data could not be read; check project compatibility and coverage")
		return
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil || len(encoded) > maxGraphResponse {
		writeError(w, http.StatusRequestEntityTooLarge, "graph response exceeds the local UI limit")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(encoded))
}

func writeGraphResult(w http.ResponseWriter, result map[string]any, err error) {
	if err != nil {
		var queryErr *appquery.QueryError
		if errors.As(err, &queryErr) {
			switch queryErr.Code {
			case appquery.ErrorInvalidInput:
				writeError(w, http.StatusBadRequest, "graph query is invalid")
			case appquery.ErrorProjectNotRegistered:
				writeError(w, http.StatusNotFound, "selected project is not registered")
			case appquery.ErrorProjectUnavailable:
				writeError(w, http.StatusServiceUnavailable, "selected project is unavailable for graph reads")
			case appquery.ErrorSurfaceUnavailable:
				writeError(w, http.StatusServiceUnavailable, "selected project's local surface catalog is unavailable")
			case appquery.ErrorStaleCursor:
				writeError(w, http.StatusConflict, "local data or continuation cursor changed; reload the first page before continuing")
			case appquery.ErrorCursorUnsupported:
				writeError(w, http.StatusNotImplemented, "configured graph backend does not support continuation")
			case appquery.ErrorOutputTooLarge:
				writeError(w, http.StatusRequestEntityTooLarge, "graph response exceeds the local UI limit")
			default:
				writeError(w, http.StatusServiceUnavailable, "local graph data is unavailable")
			}
			return
		}
		writeError(w, http.StatusInternalServerError, "graph query failed")
		return
	}
	writeResult(w, result, nil)
}

func writeEvidenceResult(w http.ResponseWriter, result appquery.CaptureMessageRange, err error) {
	defer clear(result.Data)
	if err != nil {
		var queryErr *appquery.QueryError
		if errors.As(err, &queryErr) {
			switch queryErr.Code {
			case appquery.ErrorInvalidInput:
				writeError(w, http.StatusBadRequest, "capture evidence request is invalid")
			case appquery.ErrorProjectNotRegistered:
				writeError(w, http.StatusNotFound, "selected project is not registered")
			case appquery.ErrorProjectUnavailable:
				writeError(w, http.StatusServiceUnavailable, "selected project is unavailable for local evidence reads")
			case appquery.ErrorEvidenceKeyUnavailable:
				writeError(w, http.StatusServiceUnavailable, "local vault key is unavailable or invalid; historical bytes cannot be opened")
			case appquery.ErrorEvidenceUnavailable:
				writeError(w, http.StatusNotFound, "exact local historical bytes are unavailable for this capture revision")
			default:
				writeError(w, http.StatusServiceUnavailable, "local capture evidence is unavailable")
			}
			return
		}
		writeError(w, http.StatusInternalServerError, "capture evidence could not be read")
		return
	}
	encoding, data := "utf-8", string(result.Data)
	if !utf8.Valid(result.Data) {
		encoding, data = "base64", base64.StdEncoding.EncodeToString(result.Data)
	}
	var nextOffset any
	if next := result.Offset + int64(len(result.Data)); next < result.TotalSize {
		nextOffset = next
	}
	payload := map[string]any{
		"project_key": result.ProjectKey, "request_id": result.RequestID, "revision_id": result.RevisionID,
		"side": result.Side, "offset": result.Offset, "total_size": result.TotalSize,
		"returned_size": len(result.Data), "next_offset": nextOffset,
		"data": data, "encoding": encoding, "sha256": result.SHA256,
		"captured_at": result.CapturedAt, "parse_state": result.ParseState, "wire_fidelity": result.WireFidelity,
		"request_truncated": result.RequestTruncated, "response_truncated": result.ResponseTruncated,
		"sensitive_values_may_be_present": true,
		"coverage_note":                   "Exact bytes from this locally stored historical revision only; live-tail raw bytes remain in Caido.",
	}
	encoded, marshalErr := json.Marshal(payload)
	if marshalErr == nil && len(encoded) > 2<<20 && encoding != "base64" {
		clear(encoded)
		payload["encoding"] = "base64"
		payload["data"] = base64.StdEncoding.EncodeToString(result.Data)
		encoded, marshalErr = json.Marshal(payload)
	}
	if marshalErr != nil || len(encoded) > 2<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "capture evidence range exceeds the local UI limit")
		return
	}
	defer clear(encoded)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(encoded))
}

func writeComparisonResult(w http.ResponseWriter, result appquery.CaptureComparison, err error) {
	if err != nil {
		var queryErr *appquery.QueryError
		if errors.As(err, &queryErr) {
			switch queryErr.Code {
			case appquery.ErrorInvalidInput:
				writeError(w, http.StatusBadRequest, "capture comparison request is invalid")
			case appquery.ErrorProjectNotRegistered:
				writeError(w, http.StatusNotFound, "selected project is not registered")
			case appquery.ErrorProjectUnavailable:
				writeError(w, http.StatusServiceUnavailable, "selected project is unavailable for local evidence reads")
			case appquery.ErrorEvidenceKeyUnavailable:
				writeError(w, http.StatusServiceUnavailable, "local vault key is unavailable or invalid; historical bytes cannot be compared")
			default:
				writeError(w, http.StatusNotFound, "exact local historical bytes are unavailable for this comparison")
			}
			return
		}
		writeError(w, http.StatusInternalServerError, "capture comparison could not be completed")
		return
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil || len(encoded) > 64<<10 {
		writeError(w, http.StatusRequestEntityTooLarge, "capture comparison exceeds the local UI limit")
		return
	}
	defer clear(encoded)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(encoded))
}

func writeEvidencePacketResult(w http.ResponseWriter, result appquery.EvidencePacket, err error) {
	if err != nil {
		var queryErr *appquery.QueryError
		if errors.As(err, &queryErr) {
			switch queryErr.Code {
			case appquery.ErrorInvalidInput:
				writeError(w, http.StatusBadRequest, "evidence packet request is invalid")
			case appquery.ErrorProjectNotRegistered:
				writeError(w, http.StatusNotFound, "selected project is not registered")
			case appquery.ErrorProjectUnavailable:
				writeError(w, http.StatusServiceUnavailable, "selected project is unavailable for local evidence reads")
			case appquery.ErrorEvidenceKeyUnavailable:
				writeError(w, http.StatusServiceUnavailable, "local vault key is unavailable or invalid; evidence packet cannot be built")
			default:
				writeError(w, http.StatusNotFound, "selected local evidence is unavailable")
			}
			return
		}
		writeError(w, http.StatusInternalServerError, "evidence packet preview could not be built")
		return
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil || len(encoded) > 128<<10 {
		writeError(w, http.StatusRequestEntityTooLarge, "evidence packet exceeds the local preview limit")
		return
	}
	defer clear(encoded)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(encoded))
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func localRequestGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localAuthority(r.Host) {
			writeError(w, http.StatusMisdirectedRequest, "graph explorer accepts localhost requests only")
			return
		}
		sameOrigin := false
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "http" || !strings.EqualFold(parsed.Host, r.Host) {
				writeError(w, http.StatusForbidden, "cross-origin requests are not allowed")
				return
			}
			sameOrigin = true
		}
		_, allowedPost := sameOriginPostRoutes[r.URL.Path]
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodPost && allowedPost && sameOrigin) {
			w.Header().Set("Allow", "GET, HEAD, POST")
			writeError(w, http.StatusMethodNotAllowed, "unsupported local graph explorer method")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func localAuthority(authority string) bool {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		host = strings.Trim(authority, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
