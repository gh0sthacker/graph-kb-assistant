// Package ingest stores bounded, read-only-source HTTP history pages in the
// selected project's encrypted capture vault and SQLite metadata database.
package ingest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphsync"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

const (
	sourceKey        = "caido-http-history-v1"
	maxPageExchanges = 100
	maxPageBytes     = 128 << 20
	maxCursorBytes   = 4096
)

// PageWriter is bound to one registered project. It stores historical bytes,
// structural metadata, and source-linked incomplete BehaviorGraph records.
type PageWriter struct {
	selected  *store.ProjectStore
	key       []byte
	projector projectionExecutor
}

type projectionExecutor func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error)

// RunCheckpoint identifies the currently resumable historical sync run.
type RunCheckpoint struct {
	RunID                string  `json:"run_id"`
	Cursor               *string `json:"cursor"`
	State                string  `json:"state"`
	PageCount            int64   `json:"page_count"`
	DistinctRequestCount int64   `json:"distinct_request_count"`
}

// PageResult contains structural accounting only. It never returns HTTP data.
type PageResult struct {
	ProjectKey      string      `json:"project_key"`
	RunID           string      `json:"run_id"`
	PageCount       int64       `json:"page_count"`
	Stored          int         `json:"stored"`
	Replayed        int         `json:"replayed"`
	Revised         int         `json:"revised"`
	CursorState     string      `json:"cursor_state"`
	State           string      `json:"state"`
	GraphMapping    string      `json:"graph_mapping"`
	ProjectionState string      `json:"projection_state"`
	ReceiptSHA256   string      `json:"receipt_sha256"`
	SourceRefs      []SourceRef `json:"source_refs"`
}

// SourceRef addresses one immutable per-record receipt. Page receipts are kept
// separately as batch provenance and are not used as graph source locators.
type SourceRef struct {
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	RecordLocator string `json:"record_locator"`
}

// HistoryPageInput combines a source page with the cursor used to request it.
type HistoryPageInput struct {
	RunID  string
	Cursor *string
	Page   caido.HistoryPage
}

type preparedExchange struct {
	exchange       caido.HttpExchange
	observationID  string
	requestSHA256  string
	responseSHA256 *string
	requestRef     string
	responseRef    *string
	revisionID     string
	projection     behaviorgraph.ExchangeProjection
}

// OpenPageWriter validates the selected registered project and reads the
// vault key from BB_ASSISTANT_VAULT_KEY at this API boundary.
func OpenPageWriter(ctx context.Context, selected *store.ProjectStore) (*PageWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSelectedProject(selected); err != nil {
		return nil, err
	}
	key, err := readVaultKey()
	if err != nil {
		return nil, err
	}
	return &PageWriter{selected: selected, key: key}, nil
}

// Close clears the in-memory vault key held by this writer.
func (w *PageWriter) Close() {
	if w == nil {
		return
	}
	for index := range w.key {
		w.key[index] = 0
	}
	w.key = nil
}

// BeginRun resumes an interrupted sync or starts a fresh run.
func (w *PageWriter) BeginRun(ctx context.Context) (RunCheckpoint, error) {
	if err := w.validateOpen(); err != nil {
		return RunCheckpoint{}, err
	}
	conn, err := w.beginImmediate(ctx)
	if err != nil {
		return RunCheckpoint{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	now := utcNow()
	var cursor sql.NullString
	var state, runID sql.NullString
	err = conn.QueryRowContext(ctx,
		"SELECT cursor,state,run_id FROM sync_state WHERE source_key=?", sourceKey,
	).Scan(&cursor, &state, &runID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return RunCheckpoint{}, errors.New("sync checkpoint could not be read")
	}
	if err == nil && runID.Valid && (state.String == "partial" || state.String == "running") {
		var startedAt, runState string
		var pages, requests int64
		runErr := conn.QueryRowContext(ctx,
			"SELECT started_at,state,page_count,distinct_request_count FROM sync_runs WHERE run_id=? AND source_key=?",
			runID.String, sourceKey,
		).Scan(&startedAt, &runState, &pages, &requests)
		if runErr == nil && (runState == "partial" || runState == "running") {
			if _, err := conn.ExecContext(ctx, "UPDATE sync_state SET state='running',updated_at=? WHERE source_key=? AND run_id=?", now, sourceKey, runID.String); err != nil {
				return RunCheckpoint{}, errors.New("sync checkpoint could not be resumed")
			}
			if _, err := conn.ExecContext(ctx, "UPDATE sync_runs SET state='running',updated_at=? WHERE run_id=?", now, runID.String); err != nil {
				return RunCheckpoint{}, errors.New("sync run could not be resumed")
			}
			if err := commitImmediate(ctx, conn); err != nil {
				return RunCheckpoint{}, err
			}
			committed = true
			return RunCheckpoint{RunID: runID.String, Cursor: nullableStringPtr(cursor), State: "running", PageCount: pages, DistinctRequestCount: requests}, nil
		}
	}
	newRunID, err := newRunID()
	if err != nil {
		return RunCheckpoint{}, errors.New("sync run ID could not be created")
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT INTO sync_runs(run_id,source_key,started_at,updated_at,page_count,distinct_request_count,state) VALUES(?,?,?,?,0,0,'running')",
		newRunID, sourceKey, now, now,
	); err != nil {
		return RunCheckpoint{}, errors.New("sync run could not be created")
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT INTO sync_state(source_key,cursor,last_request_id,run_id,state,updated_at) VALUES(?,NULL,NULL,?,'running',?) "+
			"ON CONFLICT(source_key) DO UPDATE SET cursor=NULL,last_request_id=NULL,run_id=excluded.run_id,state='running',updated_at=excluded.updated_at",
		sourceKey, newRunID, now,
	); err != nil {
		return RunCheckpoint{}, errors.New("sync checkpoint could not be initialized")
	}
	if err := commitImmediate(ctx, conn); err != nil {
		return RunCheckpoint{}, err
	}
	committed = true
	return RunCheckpoint{RunID: newRunID, State: "running"}, nil
}

// WritePage persists vault objects and a redacted receipt before committing
// captures, revisions, search projections, sync digests, and cursor atomically.
func (w *PageWriter) WritePage(ctx context.Context, input HistoryPageInput) (PageResult, error) {
	if err := w.validateOpen(); err != nil {
		return PageResult{}, err
	}
	if err := validateSelectedProject(w.selected); err != nil {
		return PageResult{}, err
	}
	if !validRunID(input.RunID) || len(input.Page.Exchanges) < 1 || len(input.Page.Exchanges) > maxPageExchanges {
		return PageResult{}, errors.New("history page must contain 1 to 100 exchanges and an active run")
	}
	for _, cursor := range []*string{input.Cursor, input.Page.NextCursor} {
		if cursor != nil && (len(*cursor) == 0 || len(*cursor) > maxCursorBytes || !utf8.ValidString(*cursor)) {
			return PageResult{}, errors.New("history cursor is invalid or exceeds its size limit")
		}
	}
	if input.Page.HasMore && (input.Page.NextCursor == nil || input.Page.NextCursor != nil && *input.Page.NextCursor == deref(input.Cursor)) {
		return PageResult{}, errors.New("history page continuation cursor is invalid")
	}
	if !input.Page.HasMore && input.Page.NextCursor != nil {
		return PageResult{}, errors.New("terminal history page must not have a next cursor")
	}

	projectKey := w.selected.ProjectKey
	prepared := make([]preparedExchange, 0, len(input.Page.Exchanges))
	observationIDs := make([]string, 0, len(input.Page.Exchanges))
	requestHashes := make([]string, 0, len(input.Page.Exchanges))
	responseHashes := make([]*string, 0, len(input.Page.Exchanges))
	seenRequests := make(map[string]struct{}, len(input.Page.Exchanges))
	seenObservations := make(map[string]struct{}, len(input.Page.Exchanges))
	var totalBytes int64
	for _, exchange := range input.Page.Exchanges {
		observationID, err := validateExchange(exchange, projectKey)
		if err != nil {
			return PageResult{}, err
		}
		if _, exists := seenRequests[exchange.Ref.RequestID]; exists {
			return PageResult{}, errors.New("history page contains duplicate request IDs")
		}
		if _, exists := seenObservations[observationID]; exists {
			return PageResult{}, errors.New("history page contains duplicate observations")
		}
		seenRequests[exchange.Ref.RequestID] = struct{}{}
		seenObservations[observationID] = struct{}{}
		sideBytes := int64(len(exchange.RequestRaw))
		if exchange.HasResponse {
			sideBytes += int64(len(exchange.ResponseRaw))
		}
		if sideBytes > maxPageBytes-totalBytes {
			return PageResult{}, errors.New("aggregate history page exceeds the 128 MiB limit")
		}
		totalBytes += sideBytes
		if exchange.ResponseStatus != nil && (*exchange.ResponseStatus < 100 || *exchange.ResponseStatus > 599) {
			return PageResult{}, errors.New("history response status is invalid")
		}
		observationIDs = append(observationIDs, observationID)
	}
	// The complete raw-size budget and every project/request identity are
	// checked before parsing, hashing, encryption buffers, or file writes.
	for index, exchange := range input.Page.Exchanges {
		observationID := observationIDs[index]
		projection, err := behaviorgraph.ProjectExchange(exchange)
		if err != nil || projection.ObservationID != observationID {
			return PageResult{}, errors.New("history exchange projection is invalid")
		}
		requestHash := sha256Hex(exchange.RequestRaw)
		var responseHash *string
		if exchange.HasResponse {
			value := sha256Hex(exchange.ResponseRaw)
			responseHash = &value
		}
		revisionID, err := captureRevisionID(observationID, requestHash, responseHash, len(exchange.RequestRaw), responseLen(exchange),
			exchange.CapturedAt, exchange.RequestTruncated, exchange.ResponseTruncated,
			exchange.RequestWireFidelity, exchange.ResponseWireFidelity)
		if err != nil {
			return PageResult{}, errors.New("history revision identity could not be computed")
		}
		prepared = append(prepared, preparedExchange{
			exchange: exchange, observationID: observationID, requestSHA256: requestHash,
			responseSHA256: responseHash,
			revisionID:     revisionID, projection: projection,
		})
		requestHashes = append(requestHashes, requestHash)
		responseHashes = append(responseHashes, responseHash)
	}
	// All page entries and aggregate bytes are validated before any vault or
	// receipt file is allocated. A later bad row cannot produce partial objects.
	for index := range prepared {
		item := &prepared[index]
		requestRef, _, err := writeVaultObject(w.selected.VaultDirectory, item.observationID, "request", item.exchange.RequestRaw, w.key)
		if err != nil {
			return PageResult{}, err
		}
		item.requestRef = requestRef
		if item.exchange.HasResponse {
			responseRef, _, err := writeVaultObject(w.selected.VaultDirectory, item.observationID, "response", item.exchange.ResponseRaw, w.key)
			if err != nil {
				return PageResult{}, err
			}
			item.responseRef = &responseRef
		}
	}

	cursorDigest := cursorDigest(input.Cursor)
	var nextCursorDigest *string
	if input.Page.HasMore {
		value := cursorDigestOf(input.Page.NextCursor)
		nextCursorDigest = &value
	}
	_, receiptHash, sourceRefs, receiptReused, err := writePageReceipt(
		w.selected.BehaviorReceiptPath, projectKey, cursorDigest, nextCursorDigest,
		input.Page.Exchanges, observationIDs, requestHashes, responseHashes,
	)
	if err != nil {
		return PageResult{}, err
	}
	bundles, graphObservationIDs, err := buildHistoricalBundles(projectKey, w.key, prepared, sourceRefs)
	if err != nil {
		return PageResult{}, err
	}

	conn, err := w.beginImmediate(ctx)
	if err != nil {
		return PageResult{}, err
	}
	committed := false
	defer func() {
		if !committed && conn != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		if conn != nil {
			_ = conn.Close()
		}
	}()
	var currentCursor sql.NullString
	var syncState, currentRunID sql.NullString
	if err := conn.QueryRowContext(ctx,
		"SELECT cursor,state,run_id FROM sync_state WHERE source_key=?", sourceKey,
	).Scan(&currentCursor, &syncState, &currentRunID); err != nil {
		return PageResult{}, errors.New("sync checkpoint is unavailable")
	}
	var runState string
	var pageCount, distinctCount int64
	if err := conn.QueryRowContext(ctx,
		"SELECT state,page_count,distinct_request_count FROM sync_runs WHERE run_id=? AND source_key=?",
		input.RunID, sourceKey,
	).Scan(&runState, &pageCount, &distinctCount); err != nil || !currentRunID.Valid || currentRunID.String != input.RunID {
		return PageResult{}, errors.New("sync run is not active for the selected project")
	}
	cursorAlreadyCommitted := cursorWasCommitted(ctx, conn, input.RunID, cursorDigest)
	if cursorAlreadyCommitted &&
		receiptReused &&
		sameOptionalString(nullableStringPtr(currentCursor), input.Page.NextCursor) &&
		pageRequestsCommitted(ctx, conn, input.RunID, prepared) &&
		pageContentsMatch(ctx, conn, prepared) {
		if err := enqueueProjectionJobs(ctx, conn, w.selected, prepared, bundles, graphObservationIDs); err != nil {
			return PageResult{}, err
		}
		if err := commitImmediate(ctx, conn); err != nil {
			return PageResult{}, err
		}
		committed = true
		_ = conn.Close()
		conn = nil
		graphMapping, projectionState := w.projectAcceptedPage(ctx, prepared, bundles, graphObservationIDs)
		return PageResult{
			ProjectKey: projectKey, RunID: input.RunID, PageCount: pageCount,
			Replayed: len(prepared), CursorState: cursorState(nullableStringPtr(currentCursor), syncState.String), State: syncState.String,
			GraphMapping: graphMapping, ProjectionState: projectionState,
			ReceiptSHA256: receiptHash, SourceRefs: sourceRefs,
		}, nil
	}
	if cursorAlreadyCommitted {
		return PageResult{}, errors.New("history pagination repeated a cursor")
	}
	if !sameOptionalString(nullableStringPtr(currentCursor), input.Cursor) {
		return PageResult{}, errors.New("history page cursor does not match the current checkpoint")
	}
	if runState != "running" {
		return PageResult{}, errors.New("sync run is not active for the selected project")
	}
	if input.Page.HasMore && (cursorDigest == *nextCursorDigest || cursorDigestWasCommitted(ctx, conn, input.RunID, *nextCursorDigest)) {
		return PageResult{}, errors.New("history pagination would repeat a cursor")
	}
	requestDigests := make([]string, 0, len(prepared))
	for _, item := range prepared {
		digest := requestIDDigest(item.exchange.Ref.RequestID)
		if requestDigestWasCommitted(ctx, conn, input.RunID, digest) {
			return PageResult{}, errors.New("history pagination repeated a request ID")
		}
		requestDigests = append(requestDigests, digest)
	}

	counts, err := persistExchanges(ctx, conn, w.selected, prepared)
	if err != nil {
		return PageResult{}, err
	}
	if err := enqueueProjectionJobs(ctx, conn, w.selected, prepared, bundles, graphObservationIDs); err != nil {
		return PageResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO sync_run_cursor_digests(run_id,cursor_digest) VALUES(?,?)", input.RunID, cursorDigest); err != nil {
		return PageResult{}, errors.New("sync cursor digest could not be committed")
	}
	for _, digest := range requestDigests {
		if _, err := conn.ExecContext(ctx, "INSERT INTO sync_run_request_digests(run_id,request_id_digest) VALUES(?,?)", input.RunID, digest); err != nil {
			return PageResult{}, errors.New("sync request digest could not be committed")
		}
	}
	now := utcNow()
	nextState := "running"
	var storedCursor *string
	var lastRequestID *string
	if input.Page.HasMore {
		nextCursor := *input.Page.NextCursor
		storedCursor = &nextCursor
	} else {
		nextState = "done"
	}
	lastID := prepared[len(prepared)-1].exchange.Ref.RequestID
	lastRequestID = &lastID
	updated, err := conn.ExecContext(ctx,
		"UPDATE sync_runs SET page_count=page_count+1,distinct_request_count=distinct_request_count+?,state=?,updated_at=? WHERE run_id=? AND source_key=? AND state='running'",
		len(prepared), nextState, now, input.RunID, sourceKey,
	)
	if err != nil {
		return PageResult{}, errors.New("sync run checkpoint could not be advanced")
	}
	if rows, _ := updated.RowsAffected(); rows != 1 {
		return PageResult{}, errors.New("sync run is no longer active")
	}
	updated, err = conn.ExecContext(ctx,
		"UPDATE sync_state SET cursor=?,last_request_id=?,state=?,updated_at=? WHERE source_key=? AND run_id=? AND state='running'",
		storedCursor, lastRequestID, nextState, now, sourceKey, input.RunID,
	)
	if err != nil {
		return PageResult{}, errors.New("sync cursor could not be advanced")
	}
	if rows, _ := updated.RowsAffected(); rows != 1 {
		return PageResult{}, errors.New("sync run is no longer active")
	}
	if err := commitImmediate(ctx, conn); err != nil {
		return PageResult{}, err
	}
	committed = true
	_ = conn.Close()
	conn = nil
	graphMapping, projectionState := w.projectAcceptedPage(ctx, prepared, bundles, graphObservationIDs)
	return PageResult{
		ProjectKey: projectKey, RunID: input.RunID, PageCount: pageCount + 1,
		Stored: counts.stored, Replayed: counts.replayed, Revised: counts.revised,
		CursorState: cursorState(storedCursor, nextState), State: nextState,
		GraphMapping: graphMapping, ProjectionState: projectionState,
		ReceiptSHA256: receiptHash, SourceRefs: sourceRefs,
	}, nil
}

// CompleteEmpty closes a run after Caido returns an empty terminal page. It
// records the requested cursor and marks the checkpoint done without inventing
// an exchange, receipt, or graph observation.
func (w *PageWriter) CompleteEmpty(ctx context.Context, runID string, cursor *string) (RunCheckpoint, error) {
	if err := w.validateOpen(); err != nil {
		return RunCheckpoint{}, err
	}
	if err := validateSelectedProject(w.selected); err != nil {
		return RunCheckpoint{}, err
	}
	if !validRunID(runID) || (cursor != nil && (len(*cursor) == 0 || len(*cursor) > maxCursorBytes || !utf8.ValidString(*cursor))) {
		return RunCheckpoint{}, errors.New("empty history checkpoint is invalid")
	}
	conn, err := w.beginImmediate(ctx)
	if err != nil {
		return RunCheckpoint{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	var currentCursor sql.NullString
	var state, currentRunID sql.NullString
	if err := conn.QueryRowContext(ctx,
		"SELECT cursor,state,run_id FROM sync_state WHERE source_key=?", sourceKey,
	).Scan(&currentCursor, &state, &currentRunID); err != nil || !currentRunID.Valid || currentRunID.String != runID || state.String != "running" ||
		!sameOptionalString(nullableStringPtr(currentCursor), cursor) {
		return RunCheckpoint{}, errors.New("empty history page does not match the active checkpoint")
	}
	var pageCount, requestCount int64
	var runState string
	if err := conn.QueryRowContext(ctx,
		"SELECT state,page_count,distinct_request_count FROM sync_runs WHERE run_id=? AND source_key=?", runID, sourceKey,
	).Scan(&runState, &pageCount, &requestCount); err != nil || runState != "running" {
		return RunCheckpoint{}, errors.New("empty history page has no active sync run")
	}
	digest := cursorDigest(cursor)
	if cursorWasCommitted(ctx, conn, runID, digest) {
		return RunCheckpoint{}, errors.New("empty history page repeated a committed cursor")
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO sync_run_cursor_digests(run_id,cursor_digest) VALUES(?,?)", runID, digest); err != nil {
		return RunCheckpoint{}, errors.New("empty history cursor could not be recorded")
	}
	now := utcNow()
	updated, err := conn.ExecContext(ctx,
		"UPDATE sync_runs SET state='done',updated_at=? WHERE run_id=? AND source_key=? AND state='running'", now, runID, sourceKey,
	)
	if err != nil {
		return RunCheckpoint{}, errors.New("empty history run could not be completed")
	}
	if rows, _ := updated.RowsAffected(); rows != 1 {
		return RunCheckpoint{}, errors.New("empty history run is no longer active")
	}
	updated, err = conn.ExecContext(ctx,
		"UPDATE sync_state SET cursor=NULL,state='done',updated_at=? WHERE source_key=? AND run_id=? AND state='running'", now, sourceKey, runID,
	)
	if err != nil {
		return RunCheckpoint{}, errors.New("empty history checkpoint could not be completed")
	}
	if rows, _ := updated.RowsAffected(); rows != 1 {
		return RunCheckpoint{}, errors.New("empty history checkpoint is no longer active")
	}
	if err := commitImmediate(ctx, conn); err != nil {
		return RunCheckpoint{}, err
	}
	committed = true
	return RunCheckpoint{RunID: runID, State: "done", PageCount: pageCount, DistinctRequestCount: requestCount}, nil
}

func buildHistoricalBundles(projectKey string, rootKey []byte, prepared []preparedExchange, sourceRefs []SourceRef) ([]behaviorgraph.Record, []string, error) {
	if len(prepared) == 0 || len(prepared) != len(sourceRefs) || len(prepared) > maxPageExchanges {
		return nil, nil, errors.New("history receipt references do not match the page")
	}
	indexKey, responseKey, err := behaviorgraph.DeriveLiveKeys(projectKey, rootKey)
	if err != nil {
		return nil, nil, errors.New("historical graph signature keys could not be derived")
	}
	defer clear(indexKey)
	defer clear(responseKey)
	bundles := make([]behaviorgraph.Record, 0, len(prepared))
	graphObservationIDs := make([]string, 0, len(prepared))
	for index, item := range prepared {
		ref := sourceRefs[index]
		requestSignature, signatureErr := behaviorgraph.VariantSignature(item.exchange, true, indexKey, responseKey)
		if signatureErr != nil {
			return nil, nil, errors.New("historical request signature could not be computed")
		}
		responseSignatureValue, signatureErr := behaviorgraph.VariantSignature(item.exchange, false, indexKey, responseKey)
		if signatureErr != nil {
			return nil, nil, errors.New("historical response signature could not be computed")
		}
		var responseSignature *string
		if responseSignatureValue != "" {
			responseSignature = &responseSignatureValue
		}
		bundle, observationID, err := behaviorgraph.BuildHistoricalBundleWithSignatures(item.exchange, projectKey, behaviorgraph.Record{
			"path": ref.Path, "sha256": ref.SHA256, "record_locator": ref.RecordLocator,
		}, requestSignature, responseSignature)
		if err != nil || observationID == "" {
			return nil, nil, errors.New("historical graph bundle could not be built")
		}
		bundles = append(bundles, bundle)
		graphObservationIDs = append(graphObservationIDs, observationID)
	}
	return bundles, graphObservationIDs, nil
}

func enqueueProjectionJobs(
	ctx context.Context,
	conn *sql.Conn,
	selected *store.ProjectStore,
	prepared []preparedExchange,
	bundles []behaviorgraph.Record,
	graphObservationIDs []string,
) error {
	if conn == nil || selected == nil || len(prepared) == 0 || len(prepared) != len(bundles) ||
		len(prepared) != len(graphObservationIDs) || len(prepared) > maxPageExchanges {
		return errors.New("projection jobs do not match the accepted capture revisions")
	}
	for index, item := range prepared {
		payload, err := json.Marshal(bundles[index])
		if err != nil {
			return errors.New("graph bundle could not be encoded for the projection ledger")
		}
		bundleObservationID, err := behaviorgraph.ProjectionObservationID(payload, selected.ProjectKey, item.observationID)
		if err != nil || bundleObservationID != graphObservationIDs[index] {
			return errors.New("graph bundle observation does not match the capture revision mapping")
		}
		if _, _, err := behaviorgraph.EnqueueProjectionJob(
			ctx, conn, selected.ProjectKey, item.revisionID, item.observationID, payload, time.Now().UTC(),
		); errors.Is(err, behaviorgraph.ErrProjectionJobConflict) {
			canonical, graphObservationID, reuseErr := existingLiveProjectionForRevision(ctx, conn, selected, item)
			if reuseErr != nil {
				return errors.New("conflicting projection job is not a matching live revision")
			}
			bundles[index] = canonical
			graphObservationIDs[index] = graphObservationID
		} else if err != nil {
			return errors.New("projection job could not be committed with its capture revision")
		}
		canonicalPayload, err := json.Marshal(bundles[index])
		if err != nil {
			return errors.New("canonical graph bundle could not be encoded")
		}
		canonicalObservationID, err := behaviorgraph.ProjectionObservationID(canonicalPayload, selected.ProjectKey, item.observationID)
		if err != nil || canonicalObservationID != graphObservationIDs[index] {
			return errors.New("canonical graph observation does not match the capture revision mapping")
		}
	}
	return nil
}

func existingLiveProjectionForRevision(
	ctx context.Context,
	conn *sql.Conn,
	selected *store.ProjectStore,
	item preparedExchange,
) (behaviorgraph.Record, string, error) {
	var observationID, sourceKind, graphObservationID string
	if err := conn.QueryRowContext(ctx,
		"SELECT observation_id,source_kind FROM capture_revision_registry WHERE revision_id=?", item.revisionID,
	).Scan(&observationID, &sourceKind); err != nil || observationID != item.observationID || sourceKind != "live" {
		return nil, "", errors.New("revision is not first registered by the live source")
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT graph_observation_id FROM live_capture_index
		 WHERE caido_instance_id=? AND caido_project_id=? AND request_id=? AND observation_id=?`,
		selected.Project.InstanceID, selected.Project.ProjectID, item.exchange.Ref.RequestID, item.observationID,
	).Scan(&graphObservationID); err != nil || graphObservationID == "" {
		return nil, "", errors.New("matching live capture mapping is unavailable")
	}
	job, err := behaviorgraph.ProjectionJobByRevision(ctx, conn, selected.ProjectKey, item.revisionID)
	if err != nil {
		return nil, "", err
	}
	var bundle behaviorgraph.Record
	if err := json.Unmarshal([]byte(job.PayloadJSON), &bundle); err != nil || !hasProjectionObservation(bundle, graphObservationID, item.observationID, "live_capture") {
		return nil, "", errors.New("stored job does not contain the matching live graph observation")
	}
	return bundle, graphObservationID, nil
}

func hasProjectionObservation(bundle behaviorgraph.Record, observationID, captureID, observationKind string) bool {
	entities, ok := projectionRecords(bundle["entities"])
	if !ok {
		return false
	}
	for _, entity := range entities {
		if entity["id"] != observationID || entity["kind"] != "observation" {
			continue
		}
		data, ok := projectionRecord(entity["data"])
		return ok && data["observation_kind"] == observationKind && data["capture_id"] == captureID
	}
	return false
}

func projectionRecords(value any) ([]behaviorgraph.Record, bool) {
	switch records := value.(type) {
	case []behaviorgraph.Record:
		return records, true
	case []any:
		result := make([]behaviorgraph.Record, 0, len(records))
		for _, value := range records {
			record, ok := projectionRecord(value)
			if !ok {
				return nil, false
			}
			result = append(result, record)
		}
		return result, true
	default:
		return nil, false
	}
}

func projectionRecord(value any) (behaviorgraph.Record, bool) {
	switch record := value.(type) {
	case behaviorgraph.Record:
		return record, true
	case map[string]any:
		return behaviorgraph.Record(record), true
	default:
		return nil, false
	}
}

func (w *PageWriter) projectAcceptedPage(
	ctx context.Context,
	prepared []preparedExchange,
	bundles []behaviorgraph.Record,
	graphObservationIDs []string,
) (string, string) {
	projector := w.projector
	if projector == nil {
		projector = projectGraphBundles
	}
	materialized, neo4jReady, projectErr := projector(ctx, w.selected, bundles)
	if !materialized {
		return "pending", "pending"
	}
	if err := persistProjectionCompletion(ctx, w, prepared, graphObservationIDs, neo4jReady && projectErr == nil); err != nil {
		return "pending", "pending"
	}
	if neo4jReady && projectErr == nil {
		return "mapped", "applied"
	}
	return "mapped", "pending"
}

func persistProjectionCompletion(
	ctx context.Context,
	w *PageWriter,
	prepared []preparedExchange,
	graphObservationIDs []string,
	acknowledge bool,
) error {
	if w == nil || len(prepared) == 0 || len(prepared) != len(graphObservationIDs) {
		return errors.New("projected graph mappings do not match the accepted capture revisions")
	}
	conn, err := w.beginImmediate(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	for index, item := range prepared {
		job, err := behaviorgraph.ProjectionJobByRevision(ctx, conn, w.selected.ProjectKey, item.revisionID)
		if err != nil {
			return errors.New("projection job could not be verified before mapping")
		}
		jobObservationID, err := behaviorgraph.ProjectionObservationID([]byte(job.PayloadJSON), w.selected.ProjectKey, item.observationID)
		if err != nil || jobObservationID != graphObservationIDs[index] {
			return errors.New("projection job observation does not match the graph mapping")
		}
	}
	if err := persistGraphMappings(ctx, conn, prepared, graphObservationIDs); err != nil {
		return err
	}
	if acknowledge {
		for _, item := range prepared {
			if err := behaviorgraph.MarkProjectionJobApplied(ctx, conn, w.selected.ProjectKey, item.revisionID, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	if err := commitImmediate(ctx, conn); err != nil {
		return err
	}
	committed = true
	return nil
}

func importGraphBundles(ctx context.Context, selected *store.ProjectStore, bundles []behaviorgraph.Record) error {
	_, _, err := projectGraphBundles(ctx, selected, bundles)
	return err
}

func projectGraphBundles(ctx context.Context, selected *store.ProjectStore, bundles []behaviorgraph.Record) (bool, bool, error) {
	if selected == nil || len(bundles) == 0 || len(bundles) > maxPageExchanges {
		return false, false, errors.New("historical graph batch is unavailable")
	}
	if err := validateSelectedProject(selected); err != nil {
		return false, false, err
	}
	graphWriter, err := behaviorgraph.OpenWriter(ctx, selected)
	if err != nil {
		return false, false, errors.New("selected project graph writer could not be opened")
	}
	importResult, importErr := graphWriter.ImportBundles(ctx, bundles)
	if importErr != nil {
		return false, false, importErr
	}
	syncResult, syncErr := graphsync.DrainUntilBundleApplied(ctx, selected, importResult.BundleID)
	if syncErr != nil {
		return true, false, syncErr
	}
	return true, syncResult.State == "ready", nil
}

func persistGraphMappings(ctx context.Context, conn *sql.Conn, prepared []preparedExchange, graphObservationIDs []string) error {
	if len(prepared) == 0 || len(prepared) != len(graphObservationIDs) {
		return errors.New("historical graph mappings do not match the page")
	}
	now := utcNow()
	for index, item := range prepared {
		graphObservationID := graphObservationIDs[index]
		if graphObservationID == "" {
			return errors.New("historical graph observation identity is missing")
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO graph_capture_revisions(revision_id,graph_observation_id,updated_at) VALUES(?,?,?)
			 ON CONFLICT(revision_id) DO UPDATE SET graph_observation_id=excluded.graph_observation_id,updated_at=excluded.updated_at`,
			item.revisionID, graphObservationID, now,
		); err != nil {
			return errors.New("historical graph revision mapping could not be stored")
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO graph_capture_map(observation_id,revision_id,graph_observation_id,updated_at) VALUES(?,?,?,?)
			 ON CONFLICT(observation_id) DO UPDATE SET revision_id=excluded.revision_id,
			 graph_observation_id=excluded.graph_observation_id,updated_at=excluded.updated_at`,
			item.observationID, item.revisionID, graphObservationID, now,
		); err != nil {
			return errors.New("current historical graph mapping could not be stored")
		}
	}
	return nil
}

// MarkPartial keeps a committed cursor resumable after a source interruption.
func (w *PageWriter) MarkPartial(ctx context.Context, runID string) error {
	if err := w.validateOpen(); err != nil {
		return err
	}
	if runID == "" {
		return errors.New("sync run ID is required")
	}
	conn, err := w.beginImmediate(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	now := utcNow()
	updated, err := conn.ExecContext(ctx, "UPDATE sync_state SET state='partial',updated_at=? WHERE source_key=? AND run_id=? AND state IN ('running','partial')", now, sourceKey, runID)
	if err != nil {
		return errors.New("sync checkpoint could not be marked partial")
	}
	if rows, _ := updated.RowsAffected(); rows != 1 {
		return errors.New("sync run is not active")
	}
	updated, err = conn.ExecContext(ctx, "UPDATE sync_runs SET state='partial',updated_at=? WHERE source_key=? AND run_id=? AND state IN ('running','partial')", now, sourceKey, runID)
	if err != nil {
		return errors.New("sync run could not be marked partial")
	}
	if rows, _ := updated.RowsAffected(); rows != 1 {
		return errors.New("sync run is not active")
	}
	if err := commitImmediate(ctx, conn); err != nil {
		return err
	}
	committed = true
	return nil
}

func validateSelectedProject(selected *store.ProjectStore) error {
	if selected == nil || selected.Database == nil || selected.Database.DB() == nil || selected.Database.ReadOnly() {
		return errors.New("historical writer requires a writable selected project store")
	}
	key, err := selected.Project.StorageKey()
	if err != nil || key != selected.ProjectKey {
		return errors.New("selected project identity is inconsistent")
	}
	directory, err := filepath.Abs(selected.Directory)
	if err != nil || filepath.Base(directory) != key || filepath.Base(filepath.Dir(directory)) != "projects" {
		return errors.New("selected project directory is invalid")
	}
	if selected.Database.Path() != filepath.Join(directory, "graph.sqlite3") ||
		filepath.Clean(selected.VaultDirectory) != filepath.Join(directory, "vault") ||
		filepath.Clean(selected.BehaviorReceiptPath) != filepath.Join(directory, "behavior-receipts") ||
		filepath.Clean(selected.BehaviorGraphPath) != filepath.Join(directory, "behavior.sqlite3") {
		return errors.New("selected project paths are inconsistent")
	}
	root := filepath.Dir(filepath.Dir(directory))
	registered, err := project.LoadProjectRefsReadOnly(root)
	if err != nil {
		return errors.New("selected project registry is unavailable")
	}
	registeredSelected := false
	for _, ref := range registered {
		registeredKey, keyErr := ref.StorageKey()
		if keyErr == nil && registeredKey == key && ref.SameProject(selected.Project) {
			registeredSelected = true
			break
		}
	}
	if !registeredSelected {
		return errors.New("selected project is not registered")
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("selected project directory is unavailable")
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return errors.New("selected project directory is unavailable")
	}
	// The Store open validated the schema. Reopening the same database
	// read-only here can race its active WAL writer and adds no stronger
	// identity check than the path and registry validations above.
	for _, path := range []string{selected.VaultDirectory, selected.BehaviorReceiptPath} {
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("selected project storage directory is unavailable")
		}
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil || resolved != path {
			return errors.New("selected project storage directory is unavailable")
		}
	}
	return nil
}

func (w *PageWriter) validateOpen() error {
	if w == nil || w.selected == nil || w.selected.Database == nil || len(w.key) != 32 {
		return errors.New("historical page writer is closed")
	}
	return nil
}

func (w *PageWriter) beginImmediate(ctx context.Context) (*sql.Conn, error) {
	conn, err := w.selected.Database.DB().Conn(ctx)
	if err != nil {
		return nil, errors.New("capture database connection could not be opened")
	}
	for _, statement := range []string{"PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=30000", "PRAGMA synchronous=FULL", "BEGIN IMMEDIATE"} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			_ = conn.Close()
			return nil, errors.New("capture page transaction could not begin")
		}
	}
	return conn, nil
}

func commitImmediate(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return errors.New("capture page transaction could not commit")
	}
	return nil
}

func persistExchanges(ctx context.Context, conn *sql.Conn, selected *store.ProjectStore, prepared []preparedExchange) (writeCounts, error) {
	var counts writeCounts
	for _, item := range prepared {
		observationID := item.observationID
		exchange := item.exchange
		var existingRequestID, existingInstance, existingProject string
		var currentRevision sql.NullString
		err := conn.QueryRowContext(ctx,
			"SELECT caido_instance_id,caido_project_id,request_id,current_revision_id FROM captures WHERE observation_id=?", observationID,
		).Scan(&existingInstance, &existingProject, &existingRequestID, &currentRevision)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return counts, errors.New("existing capture identity could not be checked")
		}
		if exists && (existingInstance != selected.Project.InstanceID || existingProject != selected.Project.ProjectID || existingRequestID != exchange.Ref.RequestID) {
			return counts, errors.New("capture observation identity conflicts with an existing request")
		}
		if !exists {
			var otherObservation string
			err = conn.QueryRowContext(ctx,
				"SELECT observation_id FROM captures WHERE caido_instance_id=? AND caido_project_id=? AND request_id=?",
				selected.Project.InstanceID, selected.Project.ProjectID, exchange.Ref.RequestID,
			).Scan(&otherObservation)
			if err == nil && otherObservation != observationID {
				return counts, errors.New("request identity conflicts with an existing observation")
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return counts, errors.New("existing request identity could not be checked")
			}
		}
		responseSize := responseSizeArg(exchange)
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO captures(observation_id,caido_instance_id,caido_project_id,request_id,captured_at,
			 request_sha256,response_sha256,request_size,response_size,current_revision_id,request_vault_ref,response_vault_ref,parse_state)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(observation_id) DO UPDATE SET captured_at=excluded.captured_at,
			 request_sha256=excluded.request_sha256,response_sha256=excluded.response_sha256,
			 request_size=excluded.request_size,response_size=excluded.response_size,
			 current_revision_id=excluded.current_revision_id,request_vault_ref=excluded.request_vault_ref,
			 response_vault_ref=excluded.response_vault_ref,parse_state=excluded.parse_state`,
			observationID, selected.Project.InstanceID, selected.Project.ProjectID, exchange.Ref.RequestID, exchange.CapturedAt,
			item.requestSHA256, item.responseSHA256, len(exchange.RequestRaw), responseSize, item.revisionID, item.requestRef, item.responseRef,
			item.projection.ParseState,
		); err != nil {
			return counts, errors.New("capture metadata could not be stored")
		}
		requestTruncated := nullableBoolInt(exchange.RequestTruncated)
		responseTruncated := nullableBoolInt(exchange.ResponseTruncated)
		firstSeenAt := utcNow()
		if _, err := conn.ExecContext(ctx,
			`INSERT OR IGNORE INTO capture_revisions(revision_id,observation_id,request_sha256,response_sha256,
			 request_size,response_size,request_vault_ref,response_vault_ref,captured_at,request_truncated,response_truncated,
			 request_wire_fidelity,response_wire_fidelity,first_seen_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			item.revisionID, observationID, item.requestSHA256, item.responseSHA256,
			len(exchange.RequestRaw), responseSize, item.requestRef, item.responseRef, exchange.CapturedAt,
			requestTruncated, responseTruncated, exchange.RequestWireFidelity, exchange.ResponseWireFidelity, firstSeenAt,
		); err != nil {
			return counts, errors.New("capture revision could not be stored")
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT OR IGNORE INTO capture_revision_registry(revision_id,observation_id,source_kind,first_seen_at)
			 VALUES(?,?,'historical',?)`, item.revisionID, observationID, firstSeenAt,
		); err != nil {
			return counts, errors.New("capture revision registry entry could not be stored")
		}
		var registeredObservation, registeredSource string
		if err := conn.QueryRowContext(ctx,
			"SELECT observation_id,source_kind FROM capture_revision_registry WHERE revision_id=?", item.revisionID,
		).Scan(&registeredObservation, &registeredSource); err != nil || registeredObservation != observationID || !validRegisteredRevisionSource(registeredSource) {
			return counts, errors.New("capture revision registry identity conflicts with the stored revision")
		}
		if err := verifyRevision(ctx, conn, item); err != nil {
			return counts, err
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO capture_search(observation_id,method,host,port,route,response_status,request_content_type,
			 response_content_type,field_names_json,header_names_json,captured_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(observation_id) DO UPDATE SET method=excluded.method,host=excluded.host,port=excluded.port,
			 route=excluded.route,response_status=excluded.response_status,request_content_type=excluded.request_content_type,
			 response_content_type=excluded.response_content_type,field_names_json=excluded.field_names_json,
			 header_names_json=excluded.header_names_json,captured_at=excluded.captured_at`,
			observationID, item.projection.Search.Method, item.projection.Search.Host, item.projection.Search.Port,
			item.projection.Search.Route, item.projection.Search.ResponseStatus,
			item.projection.Search.RequestContentType, item.projection.Search.ResponseContentType,
			jsonString(item.projection.Search.FieldNames), jsonString(item.projection.Search.HeaderNames), item.projection.Search.CapturedAt,
		); err != nil {
			return counts, errors.New("capture search projection could not be stored")
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM capture_search_fts WHERE observation_id=?", observationID); err != nil {
			return counts, errors.New("capture search index could not be updated")
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO capture_search_fts(observation_id,method,host,route,fields,headers) VALUES(?,?,?,?,?,?)",
			observationID, item.projection.Search.Method, item.projection.Search.Host, item.projection.Search.Route,
			strings.Join(item.projection.Search.FieldNames, " "), strings.Join(item.projection.Search.HeaderNames, " "),
		); err != nil {
			return counts, errors.New("capture full-text projection could not be stored")
		}
		if !exists {
			counts.stored++
		} else if currentRevision.Valid && currentRevision.String == item.revisionID {
			counts.replayed++
		} else {
			counts.revised++
		}
	}
	return counts, nil
}

func validRegisteredRevisionSource(source string) bool {
	return source == "historical" || source == "live"
}

type writeCounts struct{ stored, replayed, revised int }

func verifyRevision(ctx context.Context, conn *sql.Conn, item preparedExchange) error {
	var observationID, requestHash, requestRef, requestFidelity, responseFidelity string
	var responseHash, responseRef, capturedAt sql.NullString
	var requestSize int64
	var responseSize, requestTruncated, responseTruncated sql.NullInt64
	err := conn.QueryRowContext(ctx,
		`SELECT observation_id,request_sha256,response_sha256,request_size,response_size,request_vault_ref,
		 response_vault_ref,captured_at,request_truncated,response_truncated,request_wire_fidelity,response_wire_fidelity
		 FROM capture_revisions WHERE revision_id=?`, item.revisionID,
	).Scan(&observationID, &requestHash, &responseHash, &requestSize, &responseSize, &requestRef, &responseRef,
		&capturedAt, &requestTruncated, &responseTruncated, &requestFidelity, &responseFidelity)
	if err != nil {
		return errors.New("capture revision identity could not be verified")
	}
	exchange := item.exchange
	if observationID != item.observationID || requestHash != item.requestSHA256 ||
		!nullableStringEquals(responseHash, item.responseSHA256) || requestSize != int64(len(exchange.RequestRaw)) ||
		!nullableIntEquals(responseSize, responseSizeArg(exchange)) || requestRef != item.requestRef ||
		!nullableStringEquals(responseRef, item.responseRef) || !nullableStringEquals(capturedAt, exchange.CapturedAt) ||
		!nullableIntEquals(requestTruncated, nullableBoolInt(exchange.RequestTruncated)) ||
		!nullableIntEquals(responseTruncated, nullableBoolInt(exchange.ResponseTruncated)) ||
		requestFidelity != exchange.RequestWireFidelity || responseFidelity != exchange.ResponseWireFidelity {
		return errors.New("capture revision hash collision or inconsistent immutable content")
	}
	return nil
}

func captureRevisionID(observationID, requestHash string, responseHash *string, requestSize int, responseSize *int,
	capturedAt *string, requestTruncated, responseTruncated *bool, requestFidelity, responseFidelity string) (string, error) {
	payload := map[string]any{
		"observation_id": observationID, "request_sha256": requestHash, "response_sha256": responseHash,
		"request_size": requestSize, "response_size": responseSize, "captured_at": capturedAt,
		"request_truncated": requestTruncated, "response_truncated": responseTruncated,
		"request_wire_fidelity": requestFidelity, "response_wire_fidelity": responseFidelity,
	}
	encoded, err := marshalPythonCanonical(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func cursorDigest(cursor *string) string { return cursorDigestOf(cursor) }

func cursorDigestOf(cursor *string) string {
	material := []byte("caido-history-cursor:start\x00")
	if cursor != nil {
		material = append([]byte("caido-history-cursor:value\x00"), []byte(*cursor)...)
	}
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:])
}

func requestIDDigest(requestID string) string {
	digest := sha256.Sum256(append([]byte("caido-history-request-id\x00"), []byte(requestID)...))
	return hex.EncodeToString(digest[:])
}

func cursorWasCommitted(ctx context.Context, conn *sql.Conn, runID, digest string) bool {
	var count int
	return conn.QueryRowContext(ctx, "SELECT count(*) FROM sync_run_cursor_digests WHERE run_id=? AND cursor_digest=?", runID, digest).Scan(&count) == nil && count > 0
}

func cursorDigestWasCommitted(ctx context.Context, conn *sql.Conn, runID, digest string) bool {
	return cursorWasCommitted(ctx, conn, runID, digest)
}

func requestDigestWasCommitted(ctx context.Context, conn *sql.Conn, runID, digest string) bool {
	var count int
	return conn.QueryRowContext(ctx, "SELECT count(*) FROM sync_run_request_digests WHERE run_id=? AND request_id_digest=?", runID, digest).Scan(&count) == nil && count > 0
}

func pageRequestsCommitted(ctx context.Context, conn *sql.Conn, runID string, prepared []preparedExchange) bool {
	for _, item := range prepared {
		if !requestDigestWasCommitted(ctx, conn, runID, requestIDDigest(item.exchange.Ref.RequestID)) {
			return false
		}
	}
	return true
}

func pageContentsMatch(ctx context.Context, conn *sql.Conn, prepared []preparedExchange) bool {
	for _, item := range prepared {
		var revision sql.NullString
		if err := conn.QueryRowContext(ctx, "SELECT current_revision_id FROM captures WHERE observation_id=?", item.observationID).Scan(&revision); err != nil || !revision.Valid || revision.String != item.revisionID {
			return false
		}
	}
	return true
}

func nullableBoolInt(value *bool) any {
	if value == nil {
		return nil
	}
	if *value {
		return 1
	}
	return 0
}

func nullableStringEquals(value sql.NullString, expected *string) bool {
	return value.Valid == (expected != nil) && (!value.Valid || value.String == *expected)
}

func nullableIntEquals(value sql.NullInt64, expected any) bool {
	if expected == nil {
		return !value.Valid
	}
	integer, ok := expected.(int)
	return ok && value.Valid && value.Int64 == int64(integer)
}

func nullableStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

func responseSizeArg(exchange caido.HttpExchange) any {
	if !exchange.HasResponse {
		return nil
	}
	return len(exchange.ResponseRaw)
}

func responseLen(exchange caido.HttpExchange) *int {
	if !exchange.HasResponse {
		return nil
	}
	value := len(exchange.ResponseRaw)
	return &value
}

func jsonString(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func sameOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func newRunID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func validRunID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func utcNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func nullableString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func cursorState(cursor *string, state string) string {
	if state == "done" {
		return "complete"
	}
	if cursor == nil {
		return "start"
	}
	return "positioned"
}
