package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

const (
	DefaultOperationBackfillMaxCaptures = 5000
	MaximumOperationBackfillMaxCaptures = 20000
	DefaultOperationBackfillMaxBytes    = int64(512 << 20)
	MaximumOperationBackfillMaxBytes    = int64(1 << 30)
	operationBackfillBundleSources      = 900
)

var ErrOperationBackfillCandidateChanged = errors.New("operation backfill preview changed")

type OperationBackfillOptions struct {
	MaxCaptures int
	MaxBytes    int64
}

type OperationBackfillSample struct {
	ID          string `json:"id"`
	Method      string `json:"method"`
	RouteFamily string `json:"route_family"`
}

// OperationBackfillPreview describes a candidate built from locally retained
// historical requests. It never writes graph records, receipt files or queue
// state; Bundles remain in memory and callers must not serialize them.
type OperationBackfillPreview struct {
	ProjectKey                      string                    `json:"project_key"`
	State                           string                    `json:"state"`
	CoverageState                   string                    `json:"coverage_state"`
	WriteMode                       string                    `json:"write_mode"`
	HistoricalCaptures              int                       `json:"historical_captures"`
	Scanned                         int                       `json:"scanned"`
	RequestBytes                    int64                     `json:"request_bytes"`
	EligibleCaptures                int                       `json:"eligible_captures"`
	OperationGroups                 int                       `json:"operation_groups"`
	NewOperationGroups              int                       `json:"new_operation_groups"`
	AssertionLinks                  int                       `json:"assertion_links"`
	NewAssertionLinks               int                       `json:"new_assertion_links"`
	NewEndpointEntities             int                       `json:"new_endpoint_entities"`
	SkippedUnmapped                 int                       `json:"skipped_unmapped"`
	SkippedOversized                int                       `json:"skipped_oversized"`
	SkippedNoOperation              int                       `json:"skipped_no_operation"`
	SkippedEndpointIdentityMismatch int                       `json:"skipped_endpoint_identity_mismatch"`
	Truncated                       bool                      `json:"truncated"`
	CandidateSHA256                 string                    `json:"candidate_sha256,omitempty"`
	OperationSamples                []OperationBackfillSample `json:"operation_samples"`
	Bundles                         []behaviorgraph.Record    `json:"-"`
}

type OperationBackfillApplyResult struct {
	ProjectKey      string   `json:"project_key"`
	State           string   `json:"state"`
	CandidateSHA256 string   `json:"candidate_sha256"`
	BundlesImported int      `json:"bundles_imported"`
	BundlesReplayed int      `json:"bundles_replayed"`
	BundleIDs       []string `json:"bundle_ids"`
	Neo4jDrain      string   `json:"neo4j_drain"`
}

type operationBackfillAddition struct {
	source    behaviorgraph.Record
	endpoint  behaviorgraph.Record
	operation behaviorgraph.Record
	assertion behaviorgraph.Record
}

// PreviewHistoricalOperationBackfill builds a bounded, in-memory projection
// for legacy graph projects. It decrypts only the request side of locally
// stored historical captures and clears each buffer after route grouping.
// The selected project must be protected by its exclusive maintenance lease.
func PreviewHistoricalOperationBackfill(
	ctx context.Context,
	selected *store.ProjectStore,
	maintenance *projectlock.Lock,
	options OperationBackfillOptions,
) (OperationBackfillPreview, error) {
	if ctx == nil || selected == nil || selected.Database == nil || selected.Database.DB() == nil ||
		!selected.Database.ReadOnly() || maintenance == nil || !maintenance.CoversExclusiveDirectory(selected.Directory) {
		return OperationBackfillPreview{}, errors.New("operation backfill preview requires a read-only project and matching maintenance lease")
	}
	if options.MaxCaptures == 0 {
		options.MaxCaptures = DefaultOperationBackfillMaxCaptures
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultOperationBackfillMaxBytes
	}
	if options.MaxCaptures < 1 || options.MaxCaptures > MaximumOperationBackfillMaxCaptures ||
		options.MaxBytes < 1<<20 || options.MaxBytes > MaximumOperationBackfillMaxBytes {
		return OperationBackfillPreview{}, errors.New("operation backfill preview limits are invalid")
	}

	preview := OperationBackfillPreview{
		ProjectKey: selected.ProjectKey, WriteMode: "preview_only",
		OperationSamples: []OperationBackfillSample{},
	}
	reader, err := behaviorgraph.OpenReader(ctx, selected.BehaviorGraphPath, selected.ProjectKey, selected.BehaviorReceiptPath)
	if err != nil {
		return preview, errors.New("active local graph could not be read for operation preview")
	}
	snapshot, snapshotErr := reader.Snapshot()
	readerCloseErr := reader.Close()
	if snapshotErr != nil || readerCloseErr != nil {
		return preview, errors.New("active local graph snapshot could not be read and closed cleanly")
	}
	cardsByCapture, entityIDs, assertionIDs, err := indexOperationBackfillSnapshot(snapshot)
	if err != nil {
		return preview, err
	}
	rows, total, err := listHistoricalOperationBackfillRows(ctx, selected.Database.DB(), options.MaxCaptures)
	if err != nil {
		return preview, err
	}
	preview.HistoricalCaptures = total
	if total > options.MaxCaptures {
		preview.Truncated = true
	}

	additions := make([]operationBackfillAddition, 0, len(rows))
	var bytesRead int64
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return preview, err
		}
		preview.Scanned++
		card, exists := cardsByCapture[row.observationID]
		if !exists {
			preview.SkippedUnmapped++
			continue
		}
		refs, ok := operationBackfillRecords(card["source_refs"])
		if !ok || len(refs) != 1 {
			preview.SkippedUnmapped++
			continue
		}
		endpointID, endpointData, ok := operationBackfillCardEndpoint(card)
		if !ok {
			preview.SkippedUnmapped++
			continue
		}
		origin, _ := endpointData["origin"].(string)
		captured, readErr := ReadCaptureMessageForGraphRebuild(ctx, selected, row.requestID, "request")
		if errors.Is(readErr, ErrCaptureContextLimit) {
			preview.SkippedOversized++
			continue
		}
		if readErr != nil {
			return preview, errors.New("a locally retained historical request failed integrity or vault validation")
		}
		if captured.TotalSize > options.MaxBytes-bytesRead {
			clear(captured.Data)
			preview.Truncated = true
			break
		}
		bytesRead += captured.TotalSize
		preview.RequestBytes = bytesRead
		method, target, scheme, host, port, validTarget := operationBackfillTarget(captured.Data, origin)
		if !validTarget {
			clear(captured.Data)
			preview.SkippedNoOperation++
			continue
		}
		exchange := caido.HttpExchange{
			Ref:        caido.ExchangeRef{Project: selected.Project, RequestID: row.requestID},
			CapturedAt: captured.CapturedAt, RequestRaw: captured.Data,
			Method: method, Scheme: scheme, Host: host, Port: port, Target: target,
			RequestTruncated: captured.RequestTruncated, RequestWireFidelity: captured.WireFidelity,
			ResponseWireFidelity: "unknown",
		}
		bundle, _, buildErr := behaviorgraph.BuildHistoricalBundle(exchange, selected.ProjectKey, refs[0])
		clear(captured.Data)
		if buildErr != nil {
			return preview, errors.New("a verified historical request could not be projected into a canonical operation")
		}
		operation, foundOperation := operationBackfillEntity(bundle, "operation")
		projectedEndpoint, foundEndpoint := operationBackfillEntity(bundle, "endpoint")
		assertion, foundAssertion := operationBackfillAssertion(bundle, "belongs_to_operation")
		if !foundOperation || !foundEndpoint || !foundAssertion {
			preview.SkippedNoOperation++
			continue
		}
		assertionSubject, _ := assertion["subject"].(string)
		projectedEndpointID, _ := projectedEndpoint["id"].(string)
		if assertionSubject != endpointID || projectedEndpointID != endpointID {
			preview.SkippedEndpointIdentityMismatch++
			continue
		}
		operationID, _ := operation["id"].(string)
		assertionID, _ := assertion["id"].(string)
		if operationID == "" || assertionID == "" {
			return preview, errors.New("canonical operation projection returned an invalid identity")
		}
		addition := operationBackfillAddition{source: refs[0], operation: operation, assertion: assertion}
		if !entityIDs[endpointID] {
			addition.endpoint = projectedEndpoint
		}
		additions = append(additions, addition)
		preview.EligibleCaptures++
	}

	bundles, err := buildOperationBackfillBundles(selected.ProjectKey, additions)
	if err != nil {
		return preview, err
	}
	preview.Bundles = bundles
	operationIDs := make(map[string]behaviorgraph.Record)
	endpointIDs := make(map[string]behaviorgraph.Record)
	assertionIDsCandidate := make(map[string]struct{})
	newAssertions := make(map[string]struct{})
	for _, bundle := range bundles {
		entities, _ := operationBackfillRecords(bundle["entities"])
		for _, entity := range entities {
			id, _ := entity["id"].(string)
			switch entity["kind"] {
			case "operation":
				operationIDs[id] = entity
			case "endpoint":
				endpointIDs[id] = entity
			}
		}
		assertions, _ := operationBackfillRecords(bundle["assertions"])
		for _, assertion := range assertions {
			id, _ := assertion["id"].(string)
			assertionIDsCandidate[id] = struct{}{}
			if !assertionIDs[id] {
				newAssertions[id] = struct{}{}
			}
		}
	}
	preview.OperationGroups = len(operationIDs)
	preview.AssertionLinks = len(assertionIDsCandidate)
	preview.NewAssertionLinks = len(newAssertions)
	for id := range endpointIDs {
		if !entityIDs[id] {
			preview.NewEndpointEntities++
		}
	}
	for id, operation := range operationIDs {
		if !entityIDs[id] {
			preview.NewOperationGroups++
		}
		data, _ := operationBackfillRecord(operation["data"])
		sample := OperationBackfillSample{ID: id}
		sample.Method, _ = data["method"].(string)
		sample.RouteFamily, _ = data["route_family"].(string)
		preview.OperationSamples = append(preview.OperationSamples, sample)
	}
	sort.Slice(preview.OperationSamples, func(i, j int) bool { return preview.OperationSamples[i].ID < preview.OperationSamples[j].ID })
	if len(preview.OperationSamples) > 10 {
		preview.OperationSamples = preview.OperationSamples[:10]
	}
	finalizeOperationBackfillCoverage(&preview)
	preview.CandidateSHA256, err = operationBackfillDigest(bundles)
	if err != nil {
		return preview, err
	}
	return preview, nil
}

func finalizeOperationBackfillCoverage(preview *OperationBackfillPreview) {
	if preview == nil {
		return
	}
	coveragePartial := preview.Truncated || preview.SkippedUnmapped+preview.SkippedOversized+
		preview.SkippedNoOperation+preview.SkippedEndpointIdentityMismatch > 0
	if coveragePartial {
		preview.CoverageState = "partial"
	} else {
		preview.CoverageState = "complete"
	}
	switch {
	case preview.Truncated:
		preview.State = "partial"
	case preview.EligibleCaptures == 0:
		preview.State = "empty"
	default:
		preview.State = "ready"
	}
}

// ApplyHistoricalOperationBackfill commits the exact ready preview to the
// project's local BehaviorGraph and durable Neo4j outbox. The expected digest
// is an explicit confirmation of the previewed candidate set. This function
// never contacts Neo4j; the regular outbox worker owns remote projection.
func ApplyHistoricalOperationBackfill(
	ctx context.Context,
	selected *store.ProjectStore,
	maintenance *projectlock.Lock,
	preview OperationBackfillPreview,
	expectedCandidateSHA256 string,
) (OperationBackfillApplyResult, error) {
	result := OperationBackfillApplyResult{
		ProjectKey: preview.ProjectKey, State: "rejected", CandidateSHA256: preview.CandidateSHA256,
		BundleIDs: []string{}, Neo4jDrain: "not_attempted",
	}
	if ctx == nil || selected == nil || selected.Database == nil || selected.Database.DB() == nil ||
		selected.Database.ReadOnly() || maintenance == nil || !maintenance.CoversExclusiveDirectory(selected.Directory) {
		return result, errors.New("operation backfill apply requires a writable selected project and matching maintenance lease")
	}
	if preview.ProjectKey != selected.ProjectKey || preview.State != "ready" || preview.WriteMode != "preview_only" ||
		len(preview.CandidateSHA256) != sha256.Size*2 || expectedCandidateSHA256 != preview.CandidateSHA256 ||
		len(preview.Bundles) > 100 {
		return result, errors.New("operation backfill apply requires the exact digest of a complete ready preview")
	}
	actualDigest, err := operationBackfillDigest(preview.Bundles)
	if err != nil || actualDigest != preview.CandidateSHA256 {
		return result, errors.New("operation backfill preview candidate digest is invalid")
	}
	for _, bundle := range preview.Bundles {
		if bundle["program"] != selected.ProjectKey {
			return result, errors.New("operation backfill candidate contains a different project namespace")
		}
	}
	if len(preview.Bundles) == 0 {
		result.State = "ready"
		return result, nil
	}
	writer, err := behaviorgraph.OpenWriter(ctx, selected)
	if err != nil {
		return result, errors.New("selected project graph writer could not be opened for operation backfill")
	}
	result.State = "partial"
	for _, bundle := range preview.Bundles {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		imported, importErr := writer.ImportBundle(ctx, bundle)
		if importErr != nil {
			return result, errors.New("operation backfill stopped with retryable bundles remaining")
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

func operationBackfillCardEndpoint(card behaviorgraph.Record) (string, behaviorgraph.Record, bool) {
	endpoint, ok := operationBackfillRecord(card["endpoint"])
	if !ok {
		return "", nil, false
	}
	endpointID, _ := endpoint["id"].(string)
	endpointData, ok := operationBackfillRecord(endpoint["data"])
	if !ok {
		// Current historical cards store endpoint identity and fields in one
		// flat object. Older receipts may contain the graph entity shape with
		// a nested data object; support both without changing either record.
		endpointData = endpoint
	}
	return endpointID, endpointData, endpointID != ""
}

type historicalOperationBackfillRow struct{ requestID, observationID string }

func listHistoricalOperationBackfillRows(ctx context.Context, db *sql.DB, limit int) ([]historicalOperationBackfillRow, int, error) {
	var total int
	countQuery := `SELECT count(*) FROM captures c
		JOIN capture_revision_registry r ON r.observation_id=c.observation_id AND r.revision_id=c.current_revision_id
		WHERE r.source_kind='historical'`
	if err := db.QueryRowContext(ctx, countQuery).Scan(&total); err != nil {
		return nil, 0, errors.New("historical operation backfill inventory could not be counted")
	}
	rows, err := db.QueryContext(ctx, `SELECT c.request_id,c.observation_id FROM captures c
		JOIN capture_revision_registry r ON r.observation_id=c.observation_id AND r.revision_id=c.current_revision_id
		WHERE r.source_kind='historical' ORDER BY c.request_id LIMIT ?`, limit)
	if err != nil {
		return nil, 0, errors.New("historical operation backfill inventory could not be read")
	}
	defer rows.Close()
	result := make([]historicalOperationBackfillRow, 0, min(limit, total))
	for rows.Next() {
		var row historicalOperationBackfillRow
		if err := rows.Scan(&row.requestID, &row.observationID); err != nil {
			return nil, 0, errors.New("historical operation backfill inventory is malformed")
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, errors.New("historical operation backfill inventory could not be completed")
	}
	return result, total, nil
}

func indexOperationBackfillSnapshot(snapshot behaviorgraph.Record) (map[string]behaviorgraph.Record, map[string]bool, map[string]bool, error) {
	cards, ok := operationBackfillRecords(snapshot["cards"])
	if !ok {
		return nil, nil, nil, errors.New("active graph cards are malformed")
	}
	entities, ok := operationBackfillRecords(snapshot["entities"])
	if !ok {
		return nil, nil, nil, errors.New("active graph entities are malformed")
	}
	assertions, ok := operationBackfillRecords(snapshot["assertions"])
	if !ok {
		return nil, nil, nil, errors.New("active graph assertions are malformed")
	}
	byCapture := make(map[string]behaviorgraph.Record, len(cards))
	for _, card := range cards {
		captureID, _ := card["capture_id"].(string)
		if captureID == "" || byCapture[captureID] != nil {
			return nil, nil, nil, errors.New("active graph capture cards have conflicting identities")
		}
		byCapture[captureID] = card
	}
	entityIDs := make(map[string]bool, len(entities))
	for _, entity := range entities {
		id, _ := entity["id"].(string)
		if id == "" || entityIDs[id] {
			return nil, nil, nil, errors.New("active graph entity identities are conflicting")
		}
		entityIDs[id] = true
	}
	assertionIDs := make(map[string]bool, len(assertions))
	for _, assertion := range assertions {
		id, _ := assertion["id"].(string)
		if id == "" || assertionIDs[id] {
			return nil, nil, nil, errors.New("active graph assertion identities are conflicting")
		}
		assertionIDs[id] = true
	}
	return byCapture, entityIDs, assertionIDs, nil
}

func operationBackfillTarget(raw []byte, origin string) (method, target, scheme, host string, port int, ok bool) {
	lineEnd := bytes.IndexByte(raw, '\n')
	line := raw
	if lineEnd >= 0 {
		line = line[:lineEnd]
	}
	line = bytes.TrimSpace(bytes.TrimSuffix(line, []byte("\r")))
	if len(line) > 4096 {
		return "", "", "", "", 0, false
	}
	parts := strings.Fields(string(line))
	if len(parts) != 3 || !validOperationBackfillMethod(parts[0]) {
		return "", "", "", "", 0, false
	}
	base, err := url.Parse(origin)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" ||
		base.User != nil || base.Path != "" || base.RawQuery != "" || base.Fragment != "" {
		return "", "", "", "", 0, false
	}
	requestTarget, err := url.Parse(parts[1])
	if err != nil || requestTarget.Fragment != "" || requestTarget.User != nil {
		return "", "", "", "", 0, false
	}
	if requestTarget.IsAbs() {
		if (requestTarget.Scheme != "http" && requestTarget.Scheme != "https") || requestTarget.Hostname() == "" {
			return "", "", "", "", 0, false
		}
		if !sameOperationBackfillOrigin(base, requestTarget) {
			return "", "", "", "", 0, false
		}
		base = requestTarget
	} else {
		if requestTarget.Host != "" || !strings.HasPrefix(parts[1], "/") {
			return "", "", "", "", 0, false
		}
		base = base.ResolveReference(requestTarget)
	}
	if base.Opaque != "" || base.Hostname() == "" || base.Path == "" && base.RawPath == "" && parts[1] != "/" {
		return "", "", "", "", 0, false
	}
	if base.Path == "" && base.RawPath == "" {
		base.Path = "/"
	}
	portText := base.Port()
	if portText != "" {
		port, err = strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return "", "", "", "", 0, false
		}
	} else if base.Scheme == "https" {
		port = 443
	} else {
		port = 80
	}
	return strings.ToUpper(parts[0]), base.String(), strings.ToLower(base.Scheme), strings.ToLower(base.Hostname()), port, true
}

func sameOperationBackfillOrigin(left, right *url.URL) bool {
	leftPort, rightPort := left.Port(), right.Port()
	if leftPort == "" {
		if left.Scheme == "https" {
			leftPort = "443"
		} else {
			leftPort = "80"
		}
	}
	if rightPort == "" {
		if right.Scheme == "https" {
			rightPort = "443"
		} else {
			rightPort = "80"
		}
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Hostname(), right.Hostname()) && leftPort == rightPort
}

func validOperationBackfillMethod(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, char := range value {
		if !(char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || strings.ContainsRune("!#$%&'*+-.^_`|~0123456789", char)) {
			return false
		}
	}
	return true
}

func buildOperationBackfillBundles(projectKey string, additions []operationBackfillAddition) ([]behaviorgraph.Record, error) {
	if projectKey == "" || len(additions) == 0 {
		return []behaviorgraph.Record{}, nil
	}
	bundles := make([]behaviorgraph.Record, 0, (len(additions)+operationBackfillBundleSources-1)/operationBackfillBundleSources)
	for start := 0; start < len(additions); start += operationBackfillBundleSources {
		end := min(start+operationBackfillBundleSources, len(additions))
		sourcesByKey := make(map[string]behaviorgraph.Record, end-start)
		entitiesByID := make(map[string]behaviorgraph.Record)
		assertionsByID := make(map[string]behaviorgraph.Record, end-start)
		for _, addition := range additions[start:end] {
			sourceJSON, err := json.Marshal(addition.source)
			if err != nil {
				return nil, errors.New("operation source reference could not be encoded")
			}
			sourcesByKey[string(sourceJSON)] = addition.source
			operationID, _ := addition.operation["id"].(string)
			assertionID, _ := addition.assertion["id"].(string)
			if operationID == "" || assertionID == "" {
				return nil, errors.New("operation backfill record identity is invalid")
			}
			if existing, found := entitiesByID[operationID]; found {
				oldRefs, okOld := operationBackfillRecords(existing["source_refs"])
				newRefs, okNew := operationBackfillRecords(addition.operation["source_refs"])
				if !okOld || !okNew {
					return nil, errors.New("operation backfill provenance is invalid")
				}
				combined, err := mergeOperationBackfillRefs(append(oldRefs, newRefs...))
				if err != nil {
					return nil, err
				}
				copyRecord := cloneOperationBackfillRecord(existing)
				copyRecord["source_refs"] = combined
				entitiesByID[operationID] = copyRecord
			} else {
				entitiesByID[operationID] = cloneOperationBackfillRecord(addition.operation)
			}
			if addition.endpoint != nil {
				endpointID, _ := addition.endpoint["id"].(string)
				if endpointID == "" || addition.endpoint["kind"] != "endpoint" {
					return nil, errors.New("operation backfill endpoint identity is invalid")
				}
				if existing, found := entitiesByID[endpointID]; found {
					oldRefs, okOld := operationBackfillRecords(existing["source_refs"])
					newRefs, okNew := operationBackfillRecords(addition.endpoint["source_refs"])
					if !okOld || !okNew {
						return nil, errors.New("endpoint backfill provenance is invalid")
					}
					combined, err := mergeOperationBackfillRefs(append(oldRefs, newRefs...))
					if err != nil {
						return nil, err
					}
					copyRecord := cloneOperationBackfillRecord(existing)
					copyRecord["source_refs"] = combined
					entitiesByID[endpointID] = copyRecord
				} else {
					entitiesByID[endpointID] = cloneOperationBackfillRecord(addition.endpoint)
				}
			}
			if existing, found := assertionsByID[assertionID]; found {
				if !sameOperationBackfillRecord(existing, addition.assertion) {
					return nil, errors.New("operation assertion identity conflicts with existing content")
				}
			} else {
				assertionsByID[assertionID] = cloneOperationBackfillRecord(addition.assertion)
			}
		}
		sources := sortedOperationBackfillMap(sourcesByKey)
		entities := sortedOperationBackfillMap(entitiesByID)
		assertions := sortedOperationBackfillMap(assertionsByID)
		bundles = append(bundles, behaviorgraph.Record{
			"schema_version": json.Number("1"), "program": projectKey,
			"sources": sources, "entities": entities, "assertions": assertions,
			"cards": []behaviorgraph.Record{},
		})
	}
	return bundles, nil
}

func operationBackfillEntity(bundle behaviorgraph.Record, kind string) (behaviorgraph.Record, bool) {
	entities, ok := operationBackfillRecords(bundle["entities"])
	if !ok {
		return nil, false
	}
	for _, entity := range entities {
		if entity["kind"] == kind {
			return entity, true
		}
	}
	return nil, false
}

func operationBackfillAssertion(bundle behaviorgraph.Record, predicate string) (behaviorgraph.Record, bool) {
	assertions, ok := operationBackfillRecords(bundle["assertions"])
	if !ok {
		return nil, false
	}
	for _, assertion := range assertions {
		if assertion["predicate"] == predicate {
			return assertion, true
		}
	}
	return nil, false
}

func operationBackfillRecords(value any) ([]behaviorgraph.Record, bool) {
	switch records := value.(type) {
	case []behaviorgraph.Record:
		return records, true
	case []any:
		result := make([]behaviorgraph.Record, 0, len(records))
		for _, item := range records {
			row, ok := item.(map[string]any)
			if !ok {
				return nil, false
			}
			result = append(result, behaviorgraph.Record(row))
		}
		return result, true
	default:
		return nil, false
	}
}

func operationBackfillRecord(value any) (behaviorgraph.Record, bool) {
	if record, ok := value.(behaviorgraph.Record); ok {
		return record, true
	}
	if record, ok := value.(map[string]any); ok {
		return behaviorgraph.Record(record), true
	}
	return nil, false
}

func mergeOperationBackfillRefs(refs []behaviorgraph.Record) ([]behaviorgraph.Record, error) {
	byKey := make(map[string]behaviorgraph.Record, len(refs))
	for _, ref := range refs {
		encoded, err := json.Marshal(ref)
		if err != nil {
			return nil, errors.New("operation backfill provenance could not be encoded")
		}
		byKey[string(encoded)] = ref
	}
	return sortedOperationBackfillMap(byKey), nil
}

func sortedOperationBackfillMap(records map[string]behaviorgraph.Record) []behaviorgraph.Record {
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]behaviorgraph.Record, 0, len(keys))
	for _, key := range keys {
		result = append(result, records[key])
	}
	return result
}

func cloneOperationBackfillRecord(record behaviorgraph.Record) behaviorgraph.Record {
	copyRecord := make(behaviorgraph.Record, len(record))
	for key, value := range record {
		if key == "source_refs" {
			refs, ok := operationBackfillRecords(value)
			if ok {
				cloned := make([]behaviorgraph.Record, len(refs))
				for index, ref := range refs {
					cloned[index] = cloneOperationBackfillRecord(ref)
				}
				copyRecord[key] = cloned
				continue
			}
		}
		copyRecord[key] = value
	}
	return copyRecord
}

func sameOperationBackfillRecord(left, right behaviorgraph.Record) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func operationBackfillDigest(bundles []behaviorgraph.Record) (string, error) {
	encoded, err := json.Marshal(bundles)
	if err != nil {
		return "", fmt.Errorf("operation backfill candidate could not be encoded: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
