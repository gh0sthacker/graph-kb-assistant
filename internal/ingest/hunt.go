package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/hunt"
	"bb-graph-assistant/internal/store"
)

func huntObservation(selected *store.ProjectStore, e caido.HttpExchange, revision string) hunt.Observation {
	id, _ := e.Ref.ObservationID()
	return hunt.Extract(hunt.Capture{ID: e.Ref.RequestID, ObservationID: id, RevisionID: revision, Namespace: selected.ProjectKey, Host: e.Host, Port: e.Port, Scheme: e.Scheme, Request: e.RequestRaw, Response: e.ResponseRaw})
}
func persistHunt(ctx context.Context, selected *store.ProjectStore, obs []hunt.Observation) error {
	if len(obs) == 0 {
		return nil
	}
	s, err := hunt.Open(filepath.Join(selected.Directory, hunt.DatabaseName))
	if err != nil {
		return errors.New("hunt projection store is unavailable")
	}
	defer s.Close()
	if err = s.Project(ctx, obs); err != nil {
		return errors.New("hunt projection could not be committed; replay the intake page")
	}
	return nil
}

func persistHistoricalHunt(ctx context.Context, selected *store.ProjectStore, prepared []preparedExchange) error {
	obs := make([]hunt.Observation, 0, len(prepared))
	for _, p := range prepared {
		obs = append(obs, huntObservation(selected, p.exchange, p.revisionID))
	}
	return persistHunt(ctx, selected, obs)
}
func persistLiveHunt(ctx context.Context, selected *store.ProjectStore, prepared []preparedLiveCapture) error {
	obs := make([]hunt.Observation, 0, len(prepared))
	for _, p := range prepared {
		obs = append(obs, huntObservation(selected, p.exchange, p.revisionID))
	}
	return persistHunt(ctx, selected, obs)
}

type huntCaptureRow struct {
	id, observation, revision, source string
	hasResponse                       bool
}

func huntCaptureRows(ctx context.Context, selected *store.ProjectStore) ([]huntCaptureRow, error) {
	rows, err := selected.Database.DB().QueryContext(ctx, `SELECT request_id,observation_id,coalesce(current_revision_id,''),'historical',response_sha256 IS NOT NULL FROM captures
 UNION ALL SELECT request_id,observation_id,coalesce(current_revision_id,''),'live',response_size IS NOT NULL FROM live_capture_index WHERE request_id NOT IN (SELECT request_id FROM captures)
 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []huntCaptureRow{}
	for rows.Next() {
		var r huntCaptureRow
		if err = rows.Scan(&r.id, &r.observation, &r.revision, &r.source, &r.hasResponse); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReadHuntGraph never writes a project or contacts Caido. Legacy historical
// captures can be projected in memory; unprojected live bytes are reported as
// a coverage gap, not guessed from the value-free structural index.
func ReadHuntGraph(ctx context.Context, selected *store.ProjectStore) (hunt.Graph, error) {
	if err := validateSelectedProjectReadOnly(selected); err != nil {
		return hunt.Graph{}, err
	}
	rows, err := huntCaptureRows(ctx, selected)
	if err != nil {
		return hunt.Graph{}, err
	}
	cached := map[string]hunt.Observation{}
	path := filepath.Join(selected.Directory, hunt.DatabaseName)
	if _, e := os.Lstat(path); e == nil {
		cache, e := hunt.OpenReadOnly(path)
		if e != nil {
			return hunt.Graph{}, e
		}
		obs, e := cache.Observations(ctx)
		cache.Close()
		if e != nil {
			return hunt.Graph{}, e
		}
		for _, o := range obs {
			cached[o.CaptureID] = o
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return hunt.Graph{}, e
	}
	obs := []hunt.Observation{}
	missingLive, oversized := 0, 0
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return hunt.Graph{}, err
		}
		if o, ok := cached[row.id]; ok && o.ObservationID == row.observation && o.RevisionID == row.revision {
			obs = append(obs, o)
			continue
		}
		if row.source == "live" {
			missingLive++
			continue
		}
		req, e := ReadCaptureRevisionForRedaction(ctx, selected, row.id, row.revision, "request")
		if errors.Is(e, ErrCaptureContextLimit) {
			oversized++
			continue
		}
		if e != nil {
			return hunt.Graph{}, e
		}
		var res CaptureRange
		if row.hasResponse {
			res, e = ReadCaptureRevisionForRedaction(ctx, selected, row.id, row.revision, "response")
			if e != nil {
				clear(req.Data)
				if errors.Is(e, ErrCaptureContextLimit) {
					oversized++
					continue
				}
				return hunt.Graph{}, e
			}
		}
		o := hunt.Extract(hunt.Capture{ID: row.id, ObservationID: row.observation, RevisionID: row.revision, Namespace: selected.ProjectKey, Request: req.Data, Response: res.Data})
		clear(req.Data)
		clear(res.Data)
		obs = append(obs, o)
	}
	g := hunt.Build(obs)
	if missingLive > 0 {
		g.Result.CoverageGaps = append(g.Result.CoverageGaps, fmt.Sprintf("%d imported live captures have no current hunt projection; bytes are only in Caido (use hunt-backfill on an authorized copy)", missingLive))
	}
	if oversized > 0 {
		g.Result.CoverageGaps = append(g.Result.CoverageGaps, fmt.Sprintf("%d historical captures exceeded the 1 MiB per-message extraction limit", oversized))
	}
	return g, nil
}

// Fetching known IDs must not enumerate source history for every capture.
// Only the indexed origin is a metadata seed; all extracted paths/parameters
// come from the returned raw request, validated against its revision digest.
func indexedHuntExchange(ctx context.Context, selected *store.ProjectStore, id string, source interface {
	FetchHistoryPage(caido.ProjectRef, caido.RequestMetadataPage, map[string]struct{}, int) (caido.HistoryPage, error)
}) (*caido.HttpExchange, error) {
	var method, host string
	var port int
	var status sql.NullInt64
	err := selected.Database.DB().QueryRowContext(ctx, `SELECT method,host,port,response_status FROM live_capture_index WHERE request_id=? UNION ALL SELECT s.method,s.host,s.port,s.response_status FROM capture_search s JOIN captures c USING(observation_id) WHERE c.request_id=? LIMIT 1`, id, id).Scan(&method, &host, &port, &status)
	if err != nil {
		return nil, err
	}
	scheme := "http"
	if port == 443 {
		scheme = "https"
	}
	origin := url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/"}
	var code *int
	if status.Valid {
		v := int(status.Int64)
		code = &v
	}
	page, err := source.FetchHistoryPage(selected.Project, caido.RequestMetadataPage{Requests: []caido.RequestMetadata{{RequestID: id, Method: method, URL: origin.String(), StatusCode: code}}}, nil, 1<<20)
	if err != nil {
		return nil, err
	}
	if len(page.Exchanges) != 1 {
		return nil, errors.New("hunt source did not return the exact imported capture")
	}
	return &page.Exchanges[0], nil
}

type HuntExchangeSource interface {
	GetExchange(caido.ExchangeRef) (*caido.HttpExchange, error)
}
type HuntBackfillResult struct {
	Projected          int `json:"projected"`
	RevisionMismatches int `json:"revision_mismatches"`
	Missing            int `json:"missing"`
}

// BackfillHunt reads only already-imported IDs. It writes only hunt.sqlite3,
// never capture/graph databases. Source bytes must match the imported revision.
func BackfillHunt(ctx context.Context, selected *store.ProjectStore, source HuntExchangeSource) (HuntBackfillResult, error) {
	var result HuntBackfillResult
	if err := validateSelectedProjectReadOnly(selected); err != nil {
		return result, err
	}
	if source == nil {
		return result, errors.New("explicit read-only capture source is required")
	}
	rows, err := huntCaptureRows(ctx, selected)
	if err != nil {
		return result, err
	}
	batch := []hunt.Observation{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var e *caido.HttpExchange
		if indexed, ok := source.(interface {
			FetchHistoryPage(caido.ProjectRef, caido.RequestMetadataPage, map[string]struct{}, int) (caido.HistoryPage, error)
		}); ok {
			e, err = indexedHuntExchange(ctx, selected, row.id, indexed)
		} else {
			e, err = source.GetExchange(caido.ExchangeRef{Project: selected.Project, RequestID: row.id})
		}
		if err != nil {
			return result, err
		}
		if e == nil {
			result.Missing++
			continue
		}
		if !e.Ref.Project.SameProject(selected.Project) || e.Ref.RequestID != row.id {
			clear(e.RequestRaw)
			clear(e.ResponseRaw)
			return result, errors.New("hunt source returned a different capture identity")
		}
		observation, _ := e.Ref.ObservationID()
		var responseHash *string
		var responseSize *int
		if e.HasResponse {
			v := sha256Hex(e.ResponseRaw)
			responseHash = &v
			n := len(e.ResponseRaw)
			responseSize = &n
		}
		rev, revErr := captureRevisionID(observation, sha256Hex(e.RequestRaw), responseHash, len(e.RequestRaw), responseSize, e.CapturedAt, e.RequestTruncated, e.ResponseTruncated, e.RequestWireFidelity, e.ResponseWireFidelity)
		if revErr != nil || rev != row.revision {
			result.RevisionMismatches++
			clear(e.RequestRaw)
			clear(e.ResponseRaw)
			continue
		}
		batch = append(batch, huntObservation(selected, *e, rev))
		clear(e.RequestRaw)
		clear(e.ResponseRaw)
		result.Projected++
		if len(batch) >= 100 {
			if err := persistHunt(ctx, selected, batch); err != nil {
				return result, err
			}
			batch = nil
		}
	}
	if err := persistHunt(ctx, selected, batch); err != nil {
		return result, err
	}
	return result, nil
}
