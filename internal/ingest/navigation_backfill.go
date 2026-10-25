package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

const (
	DefaultNavigationBackfillMaxCaptures = 5000
	MaximumNavigationBackfillMaxCaptures = 20000
	DefaultNavigationBackfillMaxBytes    = int64(512 << 20)
	MaximumNavigationBackfillMaxBytes    = int64(1 << 30)
	navigationBackfillVersion            = "url-navigation-backfill-v1"
)

type NavigationBackfillOptions struct {
	MaxCaptures int
	MaxBytes    int64
}

// Bundles and revision anchors are private in-memory candidates, never CLI output.
type NavigationBackfillPreview struct {
	ProjectKey        string                 `json:"project_key"`
	State             string                 `json:"state"`
	WriteMode         string                 `json:"write_mode"`
	Version           string                 `json:"version"`
	Captures          int                    `json:"captures"`
	Scanned           int                    `json:"scanned"`
	EligibleCaptures  int                    `json:"eligible_captures"`
	MessageBytes      int64                  `json:"message_bytes"`
	URLReferences     int                    `json:"url_references"`
	NewURLReferences  int                    `json:"new_url_references"`
	AssertionLinks    int                    `json:"assertion_links"`
	NewAssertionLinks int                    `json:"new_assertion_links"`
	SkippedUnmapped   int                    `json:"skipped_unmapped"`
	SkippedOversized  int                    `json:"skipped_oversized"`
	SkippedInvalid    int                    `json:"skipped_invalid"`
	StatusConflicts   int                    `json:"status_conflicts"`
	AbsentResponses   int                    `json:"absent_responses"`
	Truncated         bool                   `json:"truncated"`
	CandidateSHA256   string                 `json:"candidate_sha256"`
	Bundles           []behaviorgraph.Record `json:"-"`
	options           NavigationBackfillOptions
	anchors           []navigationBackfillAnchor
}

type NavigationBackfillApplyResult = OperationBackfillApplyResult
type navigationBackfillAnchor struct{ CaptureID, RevisionID, ObservationID string }
type navigationBackfillRow struct {
	requestID, captureID      string
	revisionID, observationID sql.NullString
	requestSize, responseSize sql.NullInt64
}

func navigationBackfillOptions(options NavigationBackfillOptions) (NavigationBackfillOptions, error) {
	if options.MaxCaptures == 0 {
		options.MaxCaptures = DefaultNavigationBackfillMaxCaptures
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultNavigationBackfillMaxBytes
	}
	if options.MaxCaptures < 1 || options.MaxCaptures > MaximumNavigationBackfillMaxCaptures || options.MaxBytes < 1<<20 || options.MaxBytes > MaximumNavigationBackfillMaxBytes {
		return options, errors.New("navigation backfill limits are invalid")
	}
	return options, nil
}

// PreviewHistoricalNavigationBackfill authenticates bounded retained messages
// under an exclusive lease. It does not modify captures, graph, jobs or receipts.
func PreviewHistoricalNavigationBackfill(ctx context.Context, selected *store.ProjectStore, maintenance *projectlock.Lock, options NavigationBackfillOptions) (NavigationBackfillPreview, error) {
	preview := NavigationBackfillPreview{State: "unavailable", WriteMode: "preview_only", Version: navigationBackfillVersion, Bundles: []behaviorgraph.Record{}, anchors: []navigationBackfillAnchor{}}
	if ctx == nil || selected == nil || selected.Database == nil || selected.Database.DB() == nil || !selected.Database.ReadOnly() || maintenance == nil || !maintenance.CoversExclusiveDirectory(selected.Directory) {
		return preview, errors.New("navigation backfill preview requires a read-only project and matching maintenance lease")
	}
	preview.ProjectKey = selected.ProjectKey
	var err error
	options, err = navigationBackfillOptions(options)
	if err != nil {
		return preview, err
	}
	preview.options = options
	reader, err := behaviorgraph.OpenReader(ctx, selected.BehaviorGraphPath, selected.ProjectKey, selected.BehaviorReceiptPath)
	if err != nil {
		return preview, errors.New("active local graph could not be read for navigation preview")
	}
	snapshot, snapshotErr := reader.Snapshot()
	closeErr := reader.Close()
	if snapshotErr != nil || closeErr != nil {
		return preview, errors.New("navigation graph snapshot could not be read and closed")
	}
	entities, ok := operationBackfillRecords(snapshot["entities"])
	if !ok {
		return preview, errors.New("navigation graph entities are invalid")
	}
	byID := map[string]behaviorgraph.Record{}
	for _, entity := range entities {
		id, _ := entity["id"].(string)
		byID[id] = entity
	}
	cards, ok := operationBackfillRecords(snapshot["cards"])
	if !ok {
		return preview, errors.New("navigation graph cards are invalid")
	}
	statusConflicts := map[string]bool{}
	for _, card := range cards {
		id, _ := card["id"].(string)
		// Older receipts preserve this source disagreement as a card gap.
		// Retain it even though the parsed response status itself was kept.
		missing, _ := json.Marshal(card["missing_evidence"])
		statusConflicts[id] = bytes.Contains(missing, []byte("Caido history status metadata differed from the fetched response line"))
	}
	existingAssertions := map[string]bool{}
	assertions, ok := operationBackfillRecords(snapshot["assertions"])
	if !ok {
		return preview, errors.New("navigation graph assertions are invalid")
	}
	for _, assertion := range assertions {
		id, _ := assertion["id"].(string)
		existingAssertions[id] = true
	}
	if err := selected.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM captures WHERE caido_instance_id=? AND caido_project_id=?", selected.Project.InstanceID, selected.Project.ProjectID).Scan(&preview.Captures); err != nil {
		return preview, errors.New("navigation capture inventory could not be counted")
	}
	rows, err := selected.Database.DB().QueryContext(ctx, `SELECT c.request_id,c.observation_id,c.current_revision_id,c.request_size,c.response_size,r.graph_observation_id
		FROM captures c LEFT JOIN graph_capture_revisions r ON r.revision_id=c.current_revision_id
		WHERE c.caido_instance_id=? AND c.caido_project_id=? ORDER BY c.observation_id LIMIT ?`, selected.Project.InstanceID, selected.Project.ProjectID, options.MaxCaptures)
	if err != nil {
		return preview, errors.New("navigation capture inventory is unavailable")
	}
	inventory := []navigationBackfillRow{}
	for rows.Next() {
		var row navigationBackfillRow
		if err := rows.Scan(&row.requestID, &row.captureID, &row.revisionID, &row.requestSize, &row.responseSize, &row.observationID); err != nil {
			rows.Close()
			return preview, errors.New("navigation capture inventory is invalid")
		}
		inventory = append(inventory, row)
	}
	err = rows.Err()
	closeErr = rows.Close()
	if err != nil || closeErr != nil {
		return preview, errors.New("navigation capture inventory could not be read")
	}
	preview.Truncated = preview.Captures > len(inventory)
	var indexKey []byte
	if len(inventory) > 0 {
		rootKey, keyErr := readVaultKey()
		if keyErr != nil {
			return preview, keyErr
		}
		var responseKey []byte
		indexKey, responseKey, err = behaviorgraph.DeriveLiveKeys(selected.ProjectKey, rootKey)
		clear(rootKey)
		clear(responseKey)
		if err != nil {
			return preview, errors.New("navigation signature key could not be derived")
		}
		defer clear(indexKey)
	}
	additions := []behaviorgraph.Record{}
	for _, row := range inventory {
		if err := ctx.Err(); err != nil {
			return preview, err
		}
		preview.Scanned++
		observation, exists := byID[row.observationID.String]
		data, dataOK := operationBackfillRecord(observation["data"])
		endpointID, _ := data["endpoint_id"].(string)
		endpoint := byID[endpointID]
		endpointData, _ := operationBackfillRecord(endpoint["data"])
		origin, _ := endpointData["origin"].(string)
		refs, refsOK := operationBackfillRecords(observation["source_refs"])
		if !row.revisionID.Valid || !row.observationID.Valid || !exists || !dataOK || observation["kind"] != "observation" || data["capture_id"] != row.captureID || origin == "" || !refsOK || len(refs) == 0 {
			preview.SkippedUnmapped++
			continue
		}
		if !row.requestSize.Valid || row.requestSize.Int64 < 0 || row.responseSize.Int64 < 0 {
			preview.SkippedInvalid++
			continue
		}
		if row.requestSize.Int64 > maxGraphReprojectionMessageBytes || row.responseSize.Int64 > maxGraphReprojectionMessageBytes {
			preview.SkippedOversized++
			continue
		}
		if row.requestSize.Int64+row.responseSize.Int64 > options.MaxBytes-preview.MessageBytes {
			preview.Truncated = true
			break
		}
		cardID, _ := data["card_id"].(string)
		bundle, conflict, invalid, readBytes, err := navigationBackfillCapture(ctx, selected, row, observation, refs, origin, indexKey, statusConflicts[cardID])
		preview.MessageBytes += readBytes
		if err != nil {
			return preview, err
		}
		if invalid {
			preview.SkippedInvalid++
			continue
		}
		if conflict {
			preview.StatusConflicts++
		}
		if !row.responseSize.Valid {
			preview.AbsentResponses++
		}
		preview.EligibleCaptures++
		preview.anchors = append(preview.anchors, navigationBackfillAnchor{row.captureID, row.revisionID.String, row.observationID.String})
		additions = append(additions, bundle)
	}
	preview.Bundles, err = buildNavigationBackfillBundles(selected.ProjectKey, additions)
	if err != nil {
		return preview, err
	}
	urlIDs, assertionIDs := map[string]bool{}, map[string]bool{}
	for _, bundle := range preview.Bundles {
		for _, entity := range bundle["entities"].([]behaviorgraph.Record) {
			if entity["kind"] == "url_reference" {
				urlIDs[entity["id"].(string)] = true
			}
		}
		for _, assertion := range bundle["assertions"].([]behaviorgraph.Record) {
			assertionIDs[assertion["id"].(string)] = true
		}
	}
	preview.URLReferences, preview.AssertionLinks = len(urlIDs), len(assertionIDs)
	for id := range urlIDs {
		if _, exists := byID[id]; !exists {
			preview.NewURLReferences++
		}
	}
	for id := range assertionIDs {
		if !existingAssertions[id] {
			preview.NewAssertionLinks++
		}
	}
	preview.State = "ready"
	if preview.Truncated || preview.SkippedUnmapped+preview.SkippedOversized+preview.SkippedInvalid+preview.StatusConflicts > 0 {
		preview.State = "partial"
	}
	preview.CandidateSHA256, err = navigationBackfillDigest(preview)
	if err != nil {
		preview.State = "unavailable"
	}
	return preview, err
}

func navigationBackfillCapture(ctx context.Context, selected *store.ProjectStore, row navigationBackfillRow, observation behaviorgraph.Record, refs []behaviorgraph.Record, origin string, indexKey []byte, knownStatusConflict bool) (behaviorgraph.Record, bool, bool, int64, error) {
	request, err := ReadCaptureMessageForGraphRebuild(ctx, selected, row.requestID, "request")
	if err != nil {
		return nil, false, false, 0, errors.New("navigation request could not be authenticated")
	}
	defer clear(request.Data)
	readBytes := int64(len(request.Data))
	if request.RevisionID == nil || *request.RevisionID != row.revisionID.String {
		return nil, false, false, readBytes, errors.New("navigation capture revision changed")
	}
	method, target, scheme, host, port, ok := operationBackfillTarget(request.Data, origin)
	if !ok {
		return nil, false, true, readBytes, nil
	}
	exchange := caido.HttpExchange{Ref: caido.ExchangeRef{Project: selected.Project, RequestID: row.requestID}, CapturedAt: request.CapturedAt, RequestRaw: request.Data, Method: method, Target: target, Scheme: scheme, Host: host, Port: port, RequestTruncated: request.RequestTruncated, RequestWireFidelity: request.WireFidelity, ResponseWireFidelity: "unknown"}
	conflict := knownStatusConflict
	if row.responseSize.Valid {
		response, err := ReadCaptureMessageForGraphRebuild(ctx, selected, row.requestID, "response")
		if err != nil {
			return nil, false, false, readBytes, errors.New("navigation response could not be authenticated")
		}
		defer clear(response.Data)
		readBytes += int64(len(response.Data))
		if response.RevisionID == nil || *response.RevisionID != row.revisionID.String {
			return nil, false, false, readBytes, errors.New("navigation response revision changed")
		}
		exchange.HasResponse = true
		exchange.ResponseRaw = response.Data
		exchange.ResponseWireFidelity = response.WireFidelity
		exchange.ResponseTruncated = response.ResponseTruncated
		line, _, _ := bytes.Cut(response.Data, []byte("\n"))
		if len(line) <= 4096 {
			parts := strings.Fields(string(line))
			if len(parts) >= 2 && strings.HasPrefix(parts[0], "HTTP/") {
				if status, e := strconv.Atoi(parts[1]); e == nil && status >= 100 && status <= 599 {
					exchange.ResponseStatus = &status
				}
			}
		}
		data, _ := operationBackfillRecord(observation["data"])
		if stored, exists := data["http_status"]; exists {
			encoded, _ := json.Marshal(stored)
			storedStatus, e := strconv.Atoi(string(encoded))
			conflict = conflict || e != nil || exchange.ResponseStatus == nil || storedStatus != *exchange.ResponseStatus
		}
		exchange.ResponseStatusMetadataMismatch = conflict
	}
	refs, err = mergeOperationBackfillRefs(refs)
	if err != nil {
		return nil, false, false, readBytes, err
	}
	anchor := cloneOperationBackfillRecord(observation)
	anchor["source_refs"] = refs
	bundle := behaviorgraph.Record{"schema_version": json.Number("1"), "program": selected.ProjectKey, "sources": refs, "entities": []behaviorgraph.Record{anchor}, "assertions": []behaviorgraph.Record{}, "cards": []behaviorgraph.Record{}}
	if err := behaviorgraph.AddNavigationURLReferences(bundle, exchange, row.observationID.String, indexKey); err != nil {
		return nil, false, false, readBytes, errors.New("navigation references could not be prepared")
	}
	if len(bundle["assertions"].([]behaviorgraph.Record)) == 0 {
		return nil, conflict, true, readBytes, nil
	}
	return bundle, conflict, false, readBytes, nil
}

// Each page is bounded by provenance count as well as capture count.
func buildNavigationBackfillBundles(program string, additions []behaviorgraph.Record) ([]behaviorgraph.Record, error) {
	bundles := []behaviorgraph.Record{}
	for start := 0; start < len(additions); {
		sources, entities, assertions := map[string]behaviorgraph.Record{}, map[string]behaviorgraph.Record{}, map[string]behaviorgraph.Record{}
		end := start
		for end < len(additions) && end-start < 500 {
			refs := additions[end]["sources"].([]behaviorgraph.Record)
			if end > start && len(sources)+len(refs) > 900 {
				break
			}
			if len(refs) > 900 {
				return nil, errors.New("navigation capture provenance exceeds bundle limit")
			}
			for _, ref := range refs {
				encoded, err := json.Marshal(ref)
				if err != nil {
					return nil, errors.New("navigation provenance is invalid")
				}
				sources[string(encoded)] = ref
			}
			for _, entity := range additions[end]["entities"].([]behaviorgraph.Record) {
				id, _ := entity["id"].(string)
				if old, exists := entities[id]; exists {
					oldRefs, _ := operationBackfillRecords(old["source_refs"])
					newRefs, _ := operationBackfillRecords(entity["source_refs"])
					left, right := cloneOperationBackfillRecord(old), cloneOperationBackfillRecord(entity)
					delete(left, "source_refs")
					delete(right, "source_refs")
					if !sameOperationBackfillRecord(left, right) {
						return nil, errors.New("navigation entity identity conflicts")
					}
					merged, err := mergeOperationBackfillRefs(append(oldRefs, newRefs...))
					if err != nil {
						return nil, err
					}
					old["source_refs"] = merged
				} else {
					entities[id] = cloneOperationBackfillRecord(entity)
				}
			}
			for _, assertion := range additions[end]["assertions"].([]behaviorgraph.Record) {
				id, _ := assertion["id"].(string)
				if old, exists := assertions[id]; exists && !sameOperationBackfillRecord(old, assertion) {
					return nil, errors.New("navigation assertion identity conflicts")
				}
				assertions[id] = assertion
			}
			end++
		}
		bundles = append(bundles, behaviorgraph.Record{"schema_version": json.Number("1"), "program": program, "sources": sortedOperationBackfillMap(sources), "entities": sortedOperationBackfillMap(entities), "assertions": sortedOperationBackfillMap(assertions), "cards": []behaviorgraph.Record{}})
		start = end
	}
	if len(bundles) > 100 {
		return nil, errors.New("navigation candidate exceeds bundle limit")
	}
	return bundles, nil
}

func navigationBackfillDigest(preview NavigationBackfillPreview) (string, error) {
	encoded, err := json.Marshal(struct {
		Version, Project string
		Anchors          []navigationBackfillAnchor
		Bundles          []behaviorgraph.Record
	}{preview.Version, preview.ProjectKey, preview.anchors, preview.Bundles})
	if err != nil {
		return "", errors.New("navigation candidate digest could not be computed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// Apply reauthenticates the current revisions and candidate before committing
// local graph bundles and the durable Neo4j outbox. It never contacts Neo4j.
func ApplyHistoricalNavigationBackfill(ctx context.Context, selected *store.ProjectStore, maintenance *projectlock.Lock, preview NavigationBackfillPreview, expectedDigest string) (NavigationBackfillApplyResult, error) {
	result := NavigationBackfillApplyResult{ProjectKey: preview.ProjectKey, State: "rejected", CandidateSHA256: preview.CandidateSHA256, BundleIDs: []string{}, Neo4jDrain: "not_attempted"}
	if ctx == nil || selected == nil || selected.Database == nil || selected.Database.DB() == nil || selected.Database.ReadOnly() || maintenance == nil || !maintenance.CoversExclusiveDirectory(selected.Directory) {
		return result, errors.New("navigation backfill apply requires a writable project and matching maintenance lease")
	}
	if preview.ProjectKey != selected.ProjectKey || preview.State != "ready" || preview.WriteMode != "preview_only" || preview.Version != navigationBackfillVersion || len(expectedDigest) != 64 || expectedDigest != preview.CandidateSHA256 || len(preview.Bundles) > 100 {
		return result, errors.New("navigation backfill apply requires the exact digest of a complete ready preview")
	}
	digest, err := navigationBackfillDigest(preview)
	if err != nil || digest != expectedDigest {
		return result, errors.New("navigation backfill candidate digest is invalid")
	}
	readOnly, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, selected.Project, filepath.Dir(filepath.Dir(selected.Directory)), maintenance)
	if err != nil {
		return result, errors.New("navigation current revision snapshot could not be opened")
	}
	fresh, previewErr := PreviewHistoricalNavigationBackfill(ctx, readOnly, maintenance, preview.options)
	closeErr := readOnly.Close()
	if previewErr != nil || closeErr != nil || fresh.State != "ready" || fresh.CandidateSHA256 != expectedDigest {
		return result, errors.New("navigation backfill preview is stale or cannot be authenticated")
	}
	if len(fresh.Bundles) == 0 {
		result.State = "ready"
		return result, nil
	}
	writer, err := behaviorgraph.OpenWriter(ctx, selected)
	if err != nil {
		return result, errors.New("navigation graph writer could not be opened")
	}
	result.State = "partial"
	for _, bundle := range fresh.Bundles {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		imported, err := writer.ImportBundle(ctx, bundle)
		if err != nil {
			return result, errors.New("navigation backfill stopped with retryable bundles remaining")
		}
		result.BundleIDs = append(result.BundleIDs, imported.BundleID)
		if imported.Replayed {
			result.BundlesReplayed++
		} else {
			result.BundlesImported++
		}
	}
	result.State = "committed"
	return result, nil
}
