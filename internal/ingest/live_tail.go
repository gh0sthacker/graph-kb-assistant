package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

const (
	liveTailSourceKey       = "caido-http-live-tail-v1"
	defaultLivePageSize     = 50
	defaultLiveBodyLimit    = 256 << 10
	defaultLiveOverlap      = 90
	maxLivePageMessageBytes = 64 << 20
	liveReceiptMaxBytes     = 4 << 20
	defaultLiveMaxPages     = 1
)

// LivePageBudgetFits checks the worst-case serialized request and response
// messages, including bounded headers and first lines, before fetching details.
func LivePageBudgetFits(pageSize, bodyLimit int) bool {
	if pageSize < 1 || pageSize > 100 || bodyLimit < 64<<10 || bodyLimit > 16<<20 {
		return false
	}
	perMessage := caido.MaxSerializedMessageBytes(bodyLimit)
	return int64(pageSize)*2*perMessage <= int64(maxLivePageMessageBytes)
}

type LiveTailOptions struct {
	PageSize       int
	MaxPages       int
	OverlapSeconds int
	MaxBodyBytes   int
}

type LiveTailCheckpoint struct {
	Watermark   *string
	WindowSince string
	WindowUntil string
	Cursor      *string
	State       string
}

type LiveTailResult struct {
	ProjectKey     string  `json:"project_key"`
	State          string  `json:"state"`
	RequestsSeen   int     `json:"requests_seen"`
	RequestsAdded  int     `json:"requests_added"`
	PagesCompleted int     `json:"pages_completed"`
	Watermark      *string `json:"watermark"`
	CursorPending  bool    `json:"cursor_pending"`
	LimitReached   bool    `json:"limit_reached"`
}

type LivePageResult struct {
	RequestsAdded int
	Checkpoint    LiveTailCheckpoint
}

type liveProgressRow struct {
	watermark, windowSince, windowUntil, cursor, lastPolledAt sql.NullString
	state, updatedAt                                          string
}

type preparedLiveCapture struct {
	exchange           caido.HttpExchange
	projection         behaviorgraph.ExchangeProjection
	observationID      string
	graphObservationID string
	bundle             behaviorgraph.Record
	requestSignature   string
	responseSignature  *string
	requestURLKey      string
}

type liveSourceReceipt struct {
	SchemaVersion        int                        `json:"schema_version"`
	Source               string                     `json:"source"`
	ProjectKey           string                     `json:"project_key"`
	ObservationID        string                     `json:"observation_id"`
	RequestSignature     string                     `json:"request_signature"`
	ResponseSignature    *string                    `json:"response_signature"`
	RequestURLKey        string                     `json:"request_url_key"`
	Search               behaviorgraph.SearchRecord `json:"search_record"`
	RequestWireFidelity  string                     `json:"request_wire_fidelity"`
	ResponseWireFidelity string                     `json:"response_wire_fidelity"`
	RequestTruncated     *bool                      `json:"request_truncated"`
	ResponseTruncated    *bool                      `json:"response_truncated"`
}

type LiveTailWriter struct {
	selected    *store.ProjectStore
	indexKey    []byte
	responseKey []byte
}

func OpenLiveTailWriter(ctx context.Context, selected *store.ProjectStore) (*LiveTailWriter, error) {
	if err := validateSelectedProject(ctx, selected); err != nil {
		return nil, err
	}
	rootKey, err := readVaultKey()
	if err != nil {
		return nil, errors.New("live-tail project key is unavailable")
	}
	defer clear(rootKey)
	indexKey, responseKey, err := behaviorgraph.DeriveLiveKeys(selected.ProjectKey, rootKey)
	if err != nil {
		return nil, errors.New("live-tail project keys could not be derived")
	}
	return &LiveTailWriter{selected: selected, indexKey: indexKey, responseKey: responseKey}, nil
}

func (w *LiveTailWriter) Close() {
	if w == nil {
		return
	}
	clear(w.indexKey)
	clear(w.responseKey)
	w.indexKey = nil
	w.responseKey = nil
}

func (w *LiveTailWriter) validateOpen() error {
	if w == nil || w.selected == nil || w.selected.Database == nil || w.selected.Database.ReadOnly() ||
		len(w.indexKey) != 32 || len(w.responseKey) != 32 {
		return errors.New("live-tail writer is unavailable")
	}
	return nil
}

func (w *LiveTailWriter) BeginCycle(ctx context.Context, now time.Time, overlapSeconds int) (LiveTailCheckpoint, error) {
	if err := w.validateOpen(); err != nil {
		return LiveTailCheckpoint{}, err
	}
	if overlapSeconds < 1 || overlapSeconds > 3600 {
		return LiveTailCheckpoint{}, errors.New("live-tail overlap is outside the allowed range")
	}
	if err := validateSelectedProject(ctx, w.selected); err != nil {
		return LiveTailCheckpoint{}, err
	}
	conn, err := beginSelectedImmediate(ctx, w.selected)
	if err != nil {
		return LiveTailCheckpoint{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()

	progress, err := readLiveProgress(ctx, conn)
	if err != nil {
		return LiveTailCheckpoint{}, err
	}
	now = now.UTC()
	nowText := now.Format(time.RFC3339Nano)
	checkpoint := LiveTailCheckpoint{}
	resume := progress != nil && (progress.state == "running" || progress.state == "partial")
	if resume {
		if !progress.windowSince.Valid || !progress.windowUntil.Valid || progress.windowSince.String == "" || progress.windowUntil.String == "" {
			return LiveTailCheckpoint{}, errors.New("live-tail checkpoint has no resumable time window")
		}
		since, parseSinceErr := time.Parse(time.RFC3339Nano, progress.windowSince.String)
		until, parseUntilErr := time.Parse(time.RFC3339Nano, progress.windowUntil.String)
		if parseSinceErr != nil || parseUntilErr != nil || !since.Before(until) ||
			(progress.cursor.Valid && (len(progress.cursor.String) == 0 || len(progress.cursor.String) > maxCursorBytes || !utf8.ValidString(progress.cursor.String))) {
			return LiveTailCheckpoint{}, errors.New("live-tail checkpoint is malformed")
		}
		checkpoint = LiveTailCheckpoint{
			Watermark: nullableStringPtr(progress.watermark), WindowSince: progress.windowSince.String,
			WindowUntil: progress.windowUntil.String, Cursor: nullableStringPtr(progress.cursor), State: "running",
		}
	} else {
		lower := now.Add(-time.Duration(overlapSeconds) * time.Second)
		if progress != nil && progress.watermark.Valid {
			watermark, parseErr := time.Parse(time.RFC3339Nano, progress.watermark.String)
			if parseErr != nil {
				return LiveTailCheckpoint{}, errors.New("live-tail watermark is malformed")
			}
			lower = watermark.Add(-time.Duration(overlapSeconds) * time.Second)
		}
		checkpoint = LiveTailCheckpoint{
			Watermark:   nullableStringPtrFromRow(progress),
			WindowSince: lower.UTC().Format(time.RFC3339Nano), WindowUntil: nowText,
			State: "running",
		}
	}
	if err := validateLiveCheckpoint(checkpoint); err != nil {
		return LiveTailCheckpoint{}, err
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO live_sync_state(source_key,watermark,window_since,window_until,cursor,state,last_polled_at,updated_at)
		 VALUES(?,?,?,?,?,'running',?,?)
		 ON CONFLICT(source_key) DO UPDATE SET watermark=excluded.watermark,window_since=excluded.window_since,
		 window_until=excluded.window_until,cursor=excluded.cursor,state='running',updated_at=excluded.updated_at`,
		liveTailSourceKey, checkpoint.Watermark, checkpoint.WindowSince, checkpoint.WindowUntil,
		checkpoint.Cursor, nullableProgressLastPolled(progress), nowText,
	); err != nil {
		return LiveTailCheckpoint{}, errors.New("live-tail checkpoint could not be started")
	}
	if err := commitImmediate(ctx, conn); err != nil {
		return LiveTailCheckpoint{}, err
	}
	committed = true
	return checkpoint, nil
}

func (w *LiveTailWriter) KnownRequestIDs(ctx context.Context, requestIDs []string) (map[string]struct{}, error) {
	if err := w.validateOpen(); err != nil {
		return nil, err
	}
	if len(requestIDs) > 100 {
		return nil, errors.New("live-tail request ID lookup exceeds the page limit")
	}
	known := make(map[string]struct{}, len(requestIDs))
	if len(requestIDs) == 0 {
		return known, nil
	}
	seen := make(map[string]struct{}, len(requestIDs))
	for _, requestID := range requestIDs {
		if err := (project.CaidoExchangeRef{Project: w.selected.Project, RequestID: requestID}).Validate(); err != nil {
			return nil, errors.New("live-tail request ID is invalid")
		}
		if _, exists := seen[requestID]; exists {
			return nil, errors.New("live-tail metadata page contains duplicate request IDs")
		}
		seen[requestID] = struct{}{}
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(requestIDs)), ",")
	query := `SELECT request_id FROM live_capture_index WHERE caido_instance_id=? AND caido_project_id=? AND request_id IN (` + placeholders + `)
	 UNION SELECT request_id FROM captures WHERE caido_instance_id=? AND caido_project_id=? AND request_id IN (` + placeholders + `)`
	args := make([]any, 0, 2*len(requestIDs)+4)
	args = append(args, w.selected.Project.InstanceID, w.selected.Project.ProjectID)
	for _, requestID := range requestIDs {
		args = append(args, requestID)
	}
	args = append(args, w.selected.Project.InstanceID, w.selected.Project.ProjectID)
	for _, requestID := range requestIDs {
		args = append(args, requestID)
	}
	rows, err := w.selected.Database.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.New("live-tail local deduplication lookup failed")
	}
	defer rows.Close()
	for rows.Next() {
		var requestID string
		if err := rows.Scan(&requestID); err != nil {
			return nil, errors.New("live-tail local deduplication result is malformed")
		}
		known[requestID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("live-tail local deduplication lookup failed")
	}
	return known, nil
}

func (w *LiveTailWriter) WritePage(ctx context.Context, checkpoint LiveTailCheckpoint, page caido.HistoryPage) (LivePageResult, error) {
	if err := w.validateOpen(); err != nil {
		return LivePageResult{}, err
	}
	if err := validateSelectedProject(ctx, w.selected); err != nil {
		return LivePageResult{}, err
	}
	if err := validateLiveCheckpoint(checkpoint); err != nil {
		return LivePageResult{}, err
	}
	if len(page.Exchanges) > 100 ||
		(page.HasMore && (page.NextCursor == nil || *page.NextCursor == "" || sameCursor(page.NextCursor, checkpoint.Cursor))) ||
		(!page.HasMore && page.NextCursor != nil) {
		return LivePageResult{}, errors.New("live-tail history page boundary is invalid")
	}
	prepared, err := w.prepareLiveCaptures(page.Exchanges)
	if err != nil {
		return LivePageResult{}, err
	}
	bundles := make([]behaviorgraph.Record, 0, len(prepared))
	for _, item := range prepared {
		bundles = append(bundles, item.bundle)
	}
	if len(bundles) > 0 {
		if err := importGraphBundles(ctx, w.selected, bundles); err != nil {
			return LivePageResult{}, err
		}
	}
	conn, err := beginSelectedImmediate(ctx, w.selected)
	if err != nil {
		return LivePageResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	if err := liveCheckpointMatches(ctx, conn, checkpoint); err != nil {
		return LivePageResult{}, err
	}
	added, err := persistLiveCaptures(ctx, conn, w.selected, prepared)
	if err != nil {
		return LivePageResult{}, err
	}
	updated := checkpoint
	updated.State = "partial"
	if page.HasMore {
		updated.Cursor = cloneCursor(page.NextCursor)
	} else {
		updated.State = "idle"
		updated.Watermark = stringPointerValue(checkpoint.WindowUntil)
		updated.WindowSince = ""
		updated.WindowUntil = ""
		updated.Cursor = nil
	}
	if err := updateLiveCheckpoint(ctx, conn, checkpoint, updated, !page.HasMore); err != nil {
		return LivePageResult{}, err
	}
	if err := commitImmediate(ctx, conn); err != nil {
		return LivePageResult{}, err
	}
	committed = true
	return LivePageResult{RequestsAdded: added, Checkpoint: updated}, nil
}

func (w *LiveTailWriter) MarkPartial(ctx context.Context, checkpoint LiveTailCheckpoint) error {
	if err := w.validateOpen(); err != nil {
		return err
	}
	if err := validateLiveCheckpoint(checkpoint); err != nil {
		return err
	}
	conn, err := beginSelectedImmediate(ctx, w.selected)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	}()
	if err := liveCheckpointMatches(ctx, conn, checkpoint); err != nil {
		return err
	}
	updated := checkpoint
	updated.State = "partial"
	if err := updateLiveCheckpoint(ctx, conn, checkpoint, updated, false); err != nil {
		return err
	}
	if err := commitImmediate(ctx, conn); err != nil {
		return err
	}
	return nil
}

func RunLiveTail(ctx context.Context, source caido.ReadSource, writer *LiveTailWriter, selected project.CaidoProjectRef, options LiveTailOptions) (LiveTailResult, error) {
	if source == nil || writer == nil {
		return LiveTailResult{}, errors.New("live tail requires a read-only Caido source and selected-project writer")
	}
	if err := writer.validateOpen(); err != nil {
		return LiveTailResult{}, err
	}
	if err := selected.Validate(); err != nil || !writer.selected.Project.SameProject(selected) {
		return LiveTailResult{}, errors.New("live-tail source and local writer project do not match")
	}
	if options.PageSize == 0 {
		options.PageSize = defaultLivePageSize
	}
	if options.MaxPages == 0 {
		options.MaxPages = defaultLiveMaxPages
	}
	if options.OverlapSeconds == 0 {
		options.OverlapSeconds = defaultLiveOverlap
	}
	if options.MaxBodyBytes == 0 {
		options.MaxBodyBytes = defaultLiveBodyLimit
	}
	if options.PageSize < 1 || options.PageSize > 100 || options.MaxPages < 1 || options.MaxPages > 100 ||
		options.OverlapSeconds < 1 || options.OverlapSeconds > 3600 ||
		options.MaxBodyBytes < 64<<10 || options.MaxBodyBytes > 16<<20 ||
		!LivePageBudgetFits(options.PageSize, options.MaxBodyBytes) {
		return LiveTailResult{}, errors.New("live-tail resource limits are invalid")
	}
	available, err := source.ListProjects()
	if err != nil {
		return LiveTailResult{}, err
	}
	found := false
	for _, candidate := range available {
		if candidate.SameProject(selected) {
			found = true
			break
		}
	}
	if !found {
		return LiveTailResult{}, errors.New("selected project is not available from the configured Caido MCP instance")
	}
	projectKey, err := selected.StorageKey()
	if err != nil {
		return LiveTailResult{}, errors.New("live-tail project identity is invalid")
	}
	checkpoint, err := writer.BeginCycle(ctx, time.Now().UTC(), options.OverlapSeconds)
	if err != nil {
		return LiveTailResult{}, err
	}
	result := LiveTailResult{ProjectKey: projectKey, State: checkpoint.State, Watermark: checkpoint.Watermark, CursorPending: checkpoint.Cursor != nil}
	markPartial := func(cause error) (LiveTailResult, error) {
		partialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if markErr := writer.MarkPartial(partialCtx, checkpoint); markErr != nil {
			result.State = "partial"
			return result, errors.New("live-tail stopped and its checkpoint could not be marked partial")
		}
		checkpoint.State = "partial"
		result.State = "partial"
		result.Watermark = checkpoint.Watermark
		result.CursorPending = checkpoint.Cursor != nil
		return result, cause
	}

	for result.PagesCompleted < options.MaxPages {
		if err := ctx.Err(); err != nil {
			return markPartial(errors.New("live tail was interrupted"))
		}
		httpql := liveHTTPQL(checkpoint.WindowSince, checkpoint.WindowUntil)
		metadata, err := source.ListRequestPage(selected, checkpoint.Cursor, options.PageSize, &httpql)
		if err != nil {
			return markPartial(err)
		}
		if err := validateMetadataPage(metadata, selected, options.PageSize, checkpoint.Cursor); err != nil {
			return markPartial(err)
		}
		requestIDs := make([]string, len(metadata.Requests))
		for index, request := range metadata.Requests {
			requestIDs[index] = request.RequestID
		}
		known, err := writer.KnownRequestIDs(ctx, requestIDs)
		if err != nil {
			return markPartial(err)
		}
		page, err := source.FetchHistoryPage(selected, metadata, known, options.MaxBodyBytes)
		if err != nil {
			return markPartial(err)
		}
		if err := validateLiveFetchedPage(page, metadata, known, selected); err != nil {
			clearHistoryPage(&page)
			return markPartial(err)
		}
		pageResult, pageErr := writer.WritePage(ctx, checkpoint, page)
		clearHistoryPage(&page)
		if pageErr != nil {
			return markPartial(pageErr)
		}
		result.RequestsSeen += len(metadata.Requests)
		result.RequestsAdded += pageResult.RequestsAdded
		result.PagesCompleted++
		checkpoint = pageResult.Checkpoint
		result.State = checkpoint.State
		result.Watermark = checkpoint.Watermark
		result.CursorPending = checkpoint.Cursor != nil
		if checkpoint.State == "idle" {
			return result, nil
		}
	}
	result.LimitReached = checkpoint.State == "partial"
	return result, nil
}

func (w *LiveTailWriter) prepareLiveCaptures(exchanges []caido.HttpExchange) ([]preparedLiveCapture, error) {
	if len(exchanges) > 100 {
		return nil, errors.New("live-tail page exceeds the exchange limit")
	}
	prepared := make([]preparedLiveCapture, 0, len(exchanges))
	seenRequests := make(map[string]struct{}, len(exchanges))
	seenObservations := make(map[string]struct{}, len(exchanges))
	var totalBytes int64
	for _, exchange := range exchanges {
		observationID, err := validateExchange(exchange, w.selected.ProjectKey)
		if err != nil {
			return nil, errors.New("live-tail exchange failed project and identity validation")
		}
		if _, exists := seenRequests[exchange.Ref.RequestID]; exists {
			return nil, errors.New("live-tail page contains duplicate request IDs")
		}
		if _, exists := seenObservations[observationID]; exists {
			return nil, errors.New("live-tail page contains duplicate observations")
		}
		seenRequests[exchange.Ref.RequestID] = struct{}{}
		seenObservations[observationID] = struct{}{}
		size := int64(len(exchange.RequestRaw))
		if exchange.HasResponse {
			size += int64(len(exchange.ResponseRaw))
		}
		if size > maxLivePageMessageBytes-totalBytes {
			return nil, errors.New("live-tail page exceeds the aggregate in-memory message budget")
		}
		totalBytes += size
		projection, err := behaviorgraph.ProjectExchange(exchange)
		if err != nil || projection.ObservationID != observationID {
			return nil, errors.New("live-tail structural projection could not be built")
		}
		requestSignature, err := behaviorgraph.VariantSignature(exchange, true, w.indexKey, w.responseKey)
		if err != nil || !validDigest(requestSignature) {
			return nil, errors.New("live-tail request signature could not be built")
		}
		responseSignatureValue, err := behaviorgraph.VariantSignature(exchange, false, w.indexKey, w.responseKey)
		if err != nil {
			return nil, errors.New("live-tail response signature could not be built")
		}
		var responseSignature *string
		if responseSignatureValue != "" {
			if !validDigest(responseSignatureValue) {
				return nil, errors.New("live-tail response signature is invalid")
			}
			responseSignature = &responseSignatureValue
		}
		baseURL, err := liveBaseURL(exchange)
		if err != nil {
			return nil, errors.New("live-tail URL key could not be normalized")
		}
		requestURLKey, err := behaviorgraph.URLIndexKey(exchange.Target, baseURL, w.indexKey)
		if err != nil || !validDigest(requestURLKey) {
			return nil, errors.New("live-tail URL key could not be built")
		}
		item := preparedLiveCapture{
			exchange: exchange, projection: projection, observationID: observationID,
			requestSignature: requestSignature, responseSignature: responseSignature, requestURLKey: requestURLKey,
		}
		source, err := writeLiveSourceReceipt(w.selected, item)
		if err != nil {
			return nil, err
		}
		bundle, graphObservationID, err := behaviorgraph.BuildLiveBundleWithSignatures(
			exchange, w.selected.ProjectKey, source, requestSignature, responseSignature,
		)
		if err != nil || !strings.HasPrefix(graphObservationID, "observation-") {
			return nil, errors.New("live-tail graph bundle could not be built")
		}
		item.bundle = bundle
		item.graphObservationID = graphObservationID
		prepared = append(prepared, item)
	}
	return prepared, nil
}

func writeLiveSourceReceipt(selected *store.ProjectStore, item preparedLiveCapture) (behaviorgraph.Record, error) {
	receipt := liveSourceReceipt{
		SchemaVersion: 1, Source: "caido_http_live_tail", ProjectKey: selected.ProjectKey,
		ObservationID: item.observationID, RequestSignature: item.requestSignature,
		ResponseSignature: item.responseSignature, RequestURLKey: item.requestURLKey,
		Search: item.projection.Search, RequestWireFidelity: item.exchange.RequestWireFidelity,
		ResponseWireFidelity: item.exchange.ResponseWireFidelity,
		RequestTruncated:     item.exchange.RequestTruncated, ResponseTruncated: item.exchange.ResponseTruncated,
	}
	encoded, err := marshalCanonical(receipt)
	if err != nil || len(encoded) == 0 || len(encoded)+1 > liveReceiptMaxBytes {
		return nil, errors.New("live-tail source receipt exceeds its size limit")
	}
	encoded = append(encoded, '\n')
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	root := selected.BehaviorReceiptPath
	directory := filepath.Join(root, selected.ProjectKey)
	if err := ensureReceiptProjectDirectory(root, directory); err != nil {
		return nil, errors.New("live-tail source receipt directory is unavailable")
	}
	path := filepath.Join(directory, item.observationID+"."+digest+".receipt.json")
	if _, err := writeImmutableReceipt(directory, path, encoded, liveReceiptMaxBytes); err != nil {
		return nil, errors.New("live-tail source receipt could not be published or verified")
	}
	return behaviorgraph.Record{"path": path, "sha256": digest, "record_locator": "$"}, nil
}

func persistLiveCaptures(ctx context.Context, conn *sql.Conn, selected *store.ProjectStore, prepared []preparedLiveCapture) (int, error) {
	added := 0
	for _, item := range prepared {
		var responseSize any
		if item.exchange.HasResponse {
			responseSize = len(item.exchange.ResponseRaw)
		}
		result, err := conn.ExecContext(ctx,
			`INSERT OR IGNORE INTO live_capture_index(observation_id,caido_instance_id,caido_project_id,request_id,
			 graph_observation_id,synced_at,method,host,port,route,response_status,request_content_type,
			 response_content_type,request_size,response_size,request_signature,response_signature,request_url_key,
			 referer_url_key,redirect_url_key,field_names_json,header_names_json,parse_state)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,NULL,?,?,?)`,
			item.observationID, selected.Project.InstanceID, selected.Project.ProjectID, item.exchange.Ref.RequestID,
			item.graphObservationID, utcNow(), item.projection.Search.Method, item.projection.Search.Host,
			item.projection.Search.Port, item.projection.Search.Route, item.projection.Search.ResponseStatus,
			item.projection.Search.RequestContentType, item.projection.Search.ResponseContentType,
			len(item.exchange.RequestRaw), responseSize, item.requestSignature, item.responseSignature,
			item.requestURLKey, jsonString(item.projection.Search.FieldNames), jsonString(item.projection.Search.HeaderNames),
			item.projection.ParseState,
		)
		if err != nil {
			return 0, errors.New("live-tail capture index could not be written")
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return 0, errors.New("live-tail capture index result could not be checked")
		}
		if rowsAffected == 1 {
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO live_capture_fts(observation_id,method,host,route,fields,headers) VALUES(?,?,?,?,?,?)`,
				item.observationID, item.projection.Search.Method, item.projection.Search.Host,
				item.projection.Search.Route, strings.Join(item.projection.Search.FieldNames, " "),
				strings.Join(item.projection.Search.HeaderNames, " "),
			); err != nil {
				return 0, errors.New("live-tail full-text index could not be written")
			}
			added++
			continue
		}
		var existingObservation, existingGraph, existingSignature, existingURL string
		err = conn.QueryRowContext(ctx,
			`SELECT observation_id,graph_observation_id,request_signature,request_url_key
			 FROM live_capture_index WHERE caido_instance_id=? AND caido_project_id=? AND request_id=?`,
			selected.Project.InstanceID, selected.Project.ProjectID, item.exchange.Ref.RequestID,
		).Scan(&existingObservation, &existingGraph, &existingSignature, &existingURL)
		if err != nil || existingObservation != item.observationID || existingGraph != item.graphObservationID ||
			existingSignature != item.requestSignature || existingURL != item.requestURLKey {
			return 0, errors.New("live-tail request identity conflicts with an existing project record")
		}
	}
	return added, nil
}

func validateLiveFetchedPage(page caido.HistoryPage, metadata caido.RequestMetadataPage, skipped map[string]struct{}, selected project.CaidoProjectRef) error {
	if page.HasMore != metadata.HasMore || !sameCursor(page.NextCursor, metadata.NextCursor) || len(page.Exchanges) > len(metadata.Requests) {
		return errors.New("Caido live-tail details disagree with their metadata page")
	}
	expected := make([]string, 0, len(metadata.Requests))
	for _, request := range metadata.Requests {
		if _, skip := skipped[request.RequestID]; !skip {
			expected = append(expected, request.RequestID)
		}
	}
	if len(page.Exchanges) != len(expected) {
		return errors.New("Caido live-tail detail count does not match the uncached metadata rows")
	}
	seen := make(map[string]struct{}, len(page.Exchanges))
	for index, exchange := range page.Exchanges {
		if err := exchange.Validate(); err != nil || !exchange.Ref.Project.SameProject(selected) || exchange.Ref.RequestID != expected[index] {
			return errors.New("Caido live-tail detail identity does not match its selected project row")
		}
		if _, exists := seen[exchange.Ref.RequestID]; exists {
			return errors.New("Caido live-tail detail page contains duplicate request IDs")
		}
		seen[exchange.Ref.RequestID] = struct{}{}
	}
	return nil
}

func clearHistoryPage(page *caido.HistoryPage) {
	if page == nil {
		return
	}
	for index := range page.Exchanges {
		clear(page.Exchanges[index].RequestRaw)
		clear(page.Exchanges[index].ResponseRaw)
		page.Exchanges[index].RequestRaw = nil
		page.Exchanges[index].ResponseRaw = nil
	}
}

func liveHTTPQL(since, until string) string {
	return fmt.Sprintf(`req.created_at.gt:"%s" AND req.created_at.lt:"%s"`, since, until)
}

func liveBaseURL(exchange caido.HttpExchange) (string, error) {
	parsed, _ := url.Parse(exchange.Target)
	scheme := strings.ToLower(exchange.Scheme)
	if scheme != "http" && scheme != "https" && parsed != nil {
		scheme = strings.ToLower(parsed.Scheme)
	}
	host := strings.ToLower(exchange.Host)
	if host == "" || host == "unknown" {
		if parsed != nil {
			host = strings.ToLower(parsed.Hostname())
		}
	}
	if (scheme != "http" && scheme != "https") || host == "" || len(host) > 253 ||
		strings.ContainsAny(host, "/\\@:#") {
		return "", errors.New("live exchange has no valid HTTP origin")
	}
	port := exchange.Port
	if port == 0 && parsed != nil && parsed.Port() != "" {
		port, _ = strconv.Atoi(parsed.Port())
	}
	if port == 0 {
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}
	if port < 1 || port > 65535 {
		return "", errors.New("live exchange HTTP port is invalid")
	}
	defaultPort := scheme == "http" && port == 80 || scheme == "https" && port == 443
	origin := scheme + "://" + host
	if !defaultPort {
		origin += ":" + strconv.Itoa(port)
	}
	return origin, nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func beginSelectedImmediate(ctx context.Context, selected *store.ProjectStore) (*sql.Conn, error) {
	if selected == nil || selected.Database == nil || selected.Database.ReadOnly() {
		return nil, errors.New("live-tail writer requires a writable selected project store")
	}
	conn, err := selected.Database.DB().Conn(ctx)
	if err != nil {
		return nil, errors.New("live-tail capture database connection could not be opened")
	}
	for _, statement := range []string{"PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=30000", "PRAGMA synchronous=FULL", "BEGIN IMMEDIATE"} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			_ = conn.Close()
			return nil, errors.New("live-tail capture transaction could not begin")
		}
	}
	return conn, nil
}

func readLiveProgress(ctx context.Context, conn *sql.Conn) (*liveProgressRow, error) {
	var row liveProgressRow
	err := conn.QueryRowContext(ctx,
		`SELECT watermark,window_since,window_until,cursor,state,last_polled_at,updated_at
		 FROM live_sync_state WHERE source_key=?`, liveTailSourceKey,
	).Scan(&row.watermark, &row.windowSince, &row.windowUntil, &row.cursor, &row.state, &row.lastPolledAt, &row.updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("live-tail checkpoint could not be read")
	}
	return &row, nil
}

func nullableStringPtrFromRow(row *liveProgressRow) *string {
	if row == nil {
		return nil
	}
	return nullableStringPtr(row.watermark)
}

func nullableProgressLastPolled(row *liveProgressRow) any {
	if row == nil || !row.lastPolledAt.Valid {
		return nil
	}
	return row.lastPolledAt.String
}

func validateLiveCheckpoint(checkpoint LiveTailCheckpoint) error {
	if checkpoint.State != "running" && checkpoint.State != "partial" {
		return errors.New("live-tail checkpoint state is invalid")
	}
	if checkpoint.WindowSince == "" || checkpoint.WindowUntil == "" ||
		len(checkpoint.WindowSince) > 128 || len(checkpoint.WindowUntil) > 128 {
		return errors.New("live-tail checkpoint window is invalid")
	}
	since, sinceErr := time.Parse(time.RFC3339Nano, checkpoint.WindowSince)
	until, untilErr := time.Parse(time.RFC3339Nano, checkpoint.WindowUntil)
	if sinceErr != nil || untilErr != nil || !since.Before(until) {
		return errors.New("live-tail checkpoint window is invalid")
	}
	if checkpoint.Watermark != nil {
		if len(*checkpoint.Watermark) > 128 {
			return errors.New("live-tail checkpoint watermark is invalid")
		}
		if _, err := time.Parse(time.RFC3339Nano, *checkpoint.Watermark); err != nil {
			return errors.New("live-tail checkpoint watermark is invalid")
		}
	}
	if checkpoint.Cursor != nil && (len(*checkpoint.Cursor) == 0 || len(*checkpoint.Cursor) > maxCursorBytes || !utf8.ValidString(*checkpoint.Cursor)) {
		return errors.New("live-tail checkpoint cursor is invalid")
	}
	return nil
}

func liveCheckpointMatches(ctx context.Context, conn *sql.Conn, expected LiveTailCheckpoint) error {
	current, err := readLiveProgress(ctx, conn)
	if err != nil || current == nil || !nullableMatches(current.watermark, expected.Watermark) ||
		!nullableMatches(current.windowSince, stringPointerValue(expected.WindowSince)) ||
		!nullableMatches(current.windowUntil, stringPointerValue(expected.WindowUntil)) ||
		!nullableMatches(current.cursor, expected.Cursor) || current.state != expected.State {
		return errors.New("live-tail checkpoint changed before this page could commit")
	}
	return nil
}

func updateLiveCheckpoint(ctx context.Context, conn *sql.Conn, before, after LiveTailCheckpoint, terminal bool) error {
	var since, until, cursor any = after.WindowSince, after.WindowUntil, after.Cursor
	lastPolledAt := any(nil)
	if terminal {
		since, until, cursor = nil, nil, nil
		lastPolledAt = utcNow()
	}
	result, err := conn.ExecContext(ctx,
		`UPDATE live_sync_state SET watermark=?,window_since=?,window_until=?,cursor=?,state=?,last_polled_at=COALESCE(?,last_polled_at),updated_at=?
		 WHERE source_key=? AND watermark IS ? AND window_since=? AND window_until=? AND cursor IS ? AND state=?`,
		after.Watermark, since, until, cursor, after.State, lastPolledAt, utcNow(), liveTailSourceKey,
		before.Watermark, before.WindowSince, before.WindowUntil, before.Cursor, before.State,
	)
	if err != nil {
		return errors.New("live-tail checkpoint could not be advanced")
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.New("live-tail checkpoint changed before it could advance")
	}
	return nil
}

func nullableMatches(value sql.NullString, expected *string) bool {
	return value.Valid == (expected != nil) && (!value.Valid || value.String == *expected)
}

func stringPointerValue(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}
