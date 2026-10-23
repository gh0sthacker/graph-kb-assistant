package appquery

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"bb-graph-assistant/internal/search"
	"bb-graph-assistant/internal/store"
)

const MaximumSurfaceCursorBytes = 16 << 10

type SurfaceRequest struct {
	ProjectKey string
	Filter     string
	Cursor     string
	Limit      int
}

type SurfaceResult struct {
	ProjectKey          string
	Filter              string
	Operations          []search.SurfaceOperation
	TotalOperations     int
	TotalObservations   int64
	RowsRead            int
	SnapshotFingerprint string
	Truncated           bool
	NextCursor          string
	Coverage            string
	CoverageGaps        []string
}

type surfaceCursorData struct {
	Version     int                      `json:"version"`
	ProjectKey  string                   `json:"project_key"`
	Filter      string                   `json:"filter"`
	Fingerprint string                   `json:"fingerprint"`
	After       search.SurfaceCatalogKey `json:"after"`
}

func (r SurfaceResult) WireMap() map[string]any {
	var next any
	if r.NextCursor != "" {
		next = r.NextCursor
	}
	return map[string]any{
		"project_key": r.ProjectKey, "filter": r.Filter, "operations": r.Operations,
		"total_operations": r.TotalOperations, "total_observations": r.TotalObservations,
		"rows_read": r.RowsRead, "snapshot_fingerprint": r.SnapshotFingerprint,
		"truncated": r.Truncated, "next_cursor": next,
		"coverage_state": r.Coverage, "coverage_gaps": r.CoverageGaps,
		"surface_map_version": "operation-catalog-v2",
		"coverage_note":       "Counts cover deduplicated records in the local structural indexes for the selected project and filter. They are not Caido totals or a complete browser/JavaScript application map.",
	}
}

// Surface reads one operation-catalog page through the same typed use case
// used by the local HTTP UI and MCP adapter. The cursor is signed and bound to
// its selected project, normalized filter, and structural-index fingerprint.
func (s *Service) Surface(ctx context.Context, request SurfaceRequest) (SurfaceResult, error) {
	if s == nil || len(s.surfaceKey) < 32 {
		return SurfaceResult{}, queryError(ErrorSurfaceUnavailable, "surface query service is unavailable", nil)
	}
	filter := strings.ToLower(strings.TrimSpace(request.Filter))
	limit := request.Limit
	if limit == 0 {
		limit = search.DefaultSurfaceCatalogLimit
	}
	if !projectKeyPattern.MatchString(request.ProjectKey) || len(filter) > search.MaximumSurfaceFilterBytes || limit < 1 || limit > search.MaximumSurfaceCatalogLimit {
		return SurfaceResult{}, queryError(ErrorInvalidInput, "surface query project, filter, or page limit is invalid", nil)
	}
	ref, ok := s.projects[request.ProjectKey]
	if !ok {
		return SurfaceResult{}, queryError(ErrorProjectNotRegistered, "selected project is not in the local project catalog", nil)
	}
	var after *search.SurfaceCatalogKey
	fingerprint := ""
	if request.Cursor != "" {
		data, err := s.decodeSurfaceCursor(request.Cursor)
		if err != nil {
			// Cursor signatures use an in-memory service key. A cursor from a prior
			// process is indistinguishable from a modified token, so both are stale
			// continuations and should trigger a fresh first-page read.
			return SurfaceResult{}, queryError(ErrorStaleCursor, "surface continuation cursor is invalid or expired; reload the first page", err)
		}
		if data.ProjectKey != request.ProjectKey || data.Filter != filter {
			return SurfaceResult{}, queryError(ErrorStaleCursor, "surface continuation belongs to a different project or filter; reload the first page", nil)
		}
		after, fingerprint = &data.After, data.Fingerprint
	}
	selected, err := store.OpenProjectReadOnly(ctx, ref, s.dataRoot)
	if err != nil {
		return SurfaceResult{}, queryError(ErrorProjectUnavailable, "selected project is unavailable for local surface reads", err)
	}
	defer selected.Close()
	page, err := search.ReadSurfaceCatalog(ctx, selected, search.SurfaceCatalogRequest{
		Filter: filter, After: after, SnapshotFingerprint: fingerprint, Limit: limit,
	})
	if errors.Is(err, search.ErrSurfaceCatalogStale) {
		return SurfaceResult{}, queryError(ErrorStaleCursor, "local structural index changed; reload the first page", err)
	}
	if err != nil {
		return SurfaceResult{}, queryError(ErrorSurfaceUnavailable, "local surface catalog could not be read", err)
	}
	result := SurfaceResult{
		ProjectKey: page.ProjectKey, Filter: page.Filter, Operations: page.Operations,
		TotalOperations: page.TotalOperations, TotalObservations: page.TotalObservations,
		RowsRead: page.RowsRead, SnapshotFingerprint: page.SnapshotFingerprint,
		Truncated: page.Truncated, Coverage: page.Coverage, CoverageGaps: page.CoverageGaps,
	}
	if page.Truncated && page.NextAfter != nil {
		result.NextCursor, err = s.encodeSurfaceCursor(surfaceCursorData{
			Version: 1, ProjectKey: request.ProjectKey, Filter: filter,
			Fingerprint: page.SnapshotFingerprint, After: *page.NextAfter,
		})
		if err != nil {
			return SurfaceResult{}, queryError(ErrorSurfaceUnavailable, "surface continuation cursor could not be created", err)
		}
	}
	return result, nil
}

func (s *Service) encodeSurfaceCursor(data surfaceCursorData) (string, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.surfaceKey)
	_, _ = mac.Write([]byte(encoded))
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(token) > MaximumSurfaceCursorBytes {
		return "", fmt.Errorf("surface cursor exceeds %d bytes", MaximumSurfaceCursorBytes)
	}
	return token, nil
}

func (s *Service) decodeSurfaceCursor(token string) (surfaceCursorData, error) {
	var data surfaceCursorData
	if len(token) > MaximumSurfaceCursorBytes {
		return data, errors.New("surface cursor is oversized")
	}
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok || encoded == "" || signature == "" || strings.Contains(signature, ".") {
		return data, errors.New("surface cursor format is invalid")
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return data, errors.New("surface cursor signature is invalid")
	}
	mac := hmac.New(sha256.New, s.surfaceKey)
	_, _ = mac.Write([]byte(encoded))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return data, errors.New("surface cursor signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return data, errors.New("surface cursor payload is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return data, errors.New("surface cursor payload is malformed")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return data, errors.New("surface cursor payload has trailing data")
	}
	if data.Version != 1 || data.ProjectKey == "" || data.Filter != strings.ToLower(strings.TrimSpace(data.Filter)) || len(data.Filter) > search.MaximumSurfaceFilterBytes ||
		len(data.Fingerprint) != 64 || !validCursorFingerprint(data.Fingerprint) || !validSurfaceCursorKey(data.After) {
		return data, errors.New("surface cursor payload is invalid")
	}
	return data, nil
}

func validCursorFingerprint(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && isLowerHex(value)
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validSurfaceCursorKey(key search.SurfaceCatalogKey) bool {
	return key.Host != "" && len(key.Host) <= 512 && key.Port >= 0 && key.Port <= 65535 && key.Method != "" && len(key.Method) <= 32 && key.Route != "" && len(key.Route) <= 8192
}
