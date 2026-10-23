package ingest

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
)

const (
	defaultBackfillPageSize  = 50
	defaultBackfillBodyLimit = 512 << 10
	maxBackfillBodyBudget    = 64 << 20
)

// BackfillOptions bounds one invocation of the historical Caido import.
// MaxPages is an invocation limit; zero means continue until the source ends.
type BackfillOptions struct {
	PageSize     int
	MaxPages     int
	MaxBodyBytes int
	Delay        time.Duration
	OnProgress   func(BackfillProgress)
}

// BackfillProgress contains page-level operational counts and timings only.
// It deliberately excludes cursors, request IDs, and message contents.
// Durations are cumulative; graph projection is included in page processing,
// so those values are a total and its breakdown rather than additive phases.
type BackfillProgress struct {
	PagesThisRun              int     `json:"pages_this_run"`
	PageCount                 int64   `json:"page_count"`
	Stored                    int     `json:"stored"`
	Replayed                  int     `json:"replayed"`
	Revised                   int     `json:"revised"`
	ProjectionPendingPages    int     `json:"projection_pending_pages"`
	State                     string  `json:"state"`
	CursorState               string  `json:"cursor_state"`
	MetadataDurationMS        float64 `json:"metadata_duration_ms"`
	DetailDurationMS          float64 `json:"detail_duration_ms"`
	PageProcessingDurationMS  float64 `json:"page_processing_duration_ms"`
	GraphProjectionDurationMS float64 `json:"graph_projection_duration_ms"`
}

// BackfillResult is bounded accounting and checkpoint state. It contains no
// request IDs, message data, credentials, or Caido cursors.
type BackfillResult struct {
	ProjectKey                string  `json:"project_key"`
	RunID                     string  `json:"run_id"`
	PagesThisRun              int     `json:"pages_this_run"`
	PageCount                 int64   `json:"page_count"`
	Stored                    int     `json:"stored"`
	Replayed                  int     `json:"replayed"`
	Revised                   int     `json:"revised"`
	ProjectionPendingPages    int     `json:"projection_pending_pages"`
	MetadataDurationMS        float64 `json:"metadata_duration_ms"`
	DetailDurationMS          float64 `json:"detail_duration_ms"`
	PageProcessingDurationMS  float64 `json:"page_processing_duration_ms"`
	GraphProjectionDurationMS float64 `json:"graph_projection_duration_ms"`
	State                     string  `json:"state"`
	CursorState               string  `json:"cursor_state"`
	// CoverageReconciled stays false because the read API provides no stable
	// point-in-time inventory or expected history total to compare against.
	CoverageReconciled bool   `json:"coverage_reconciled"`
	CoverageNote       string `json:"coverage_note"`
	LimitReached       bool   `json:"limit_reached"`
}

// RunBackfill reads one explicitly selected Caido project page by page. The
// source is read-only; the writer advances its durable cursor only after the
// capture and projection-job transaction have succeeded. Derived graph
// projection lag is reported separately from source checkpoint state.
func RunBackfill(ctx context.Context, source caido.ReadSource, writer *PageWriter, selected project.CaidoProjectRef, options BackfillOptions) (BackfillResult, error) {
	if source == nil || writer == nil {
		return BackfillResult{}, errors.New("Caido backfill requires a read source and selected-project writer")
	}
	if err := writer.validateOpen(); err != nil {
		return BackfillResult{}, err
	}
	if err := selected.Validate(); err != nil {
		return BackfillResult{}, errors.New("Caido backfill project identity is invalid")
	}
	if !writer.selected.Project.SameProject(selected) {
		return BackfillResult{}, errors.New("Caido source selection does not match the writable project")
	}
	if options.PageSize == 0 {
		options.PageSize = defaultBackfillPageSize
	}
	if options.MaxBodyBytes == 0 {
		options.MaxBodyBytes = defaultBackfillBodyLimit
	}
	if options.PageSize < 1 || options.PageSize > 100 || options.MaxPages < 0 ||
		options.MaxBodyBytes < 64<<10 || options.MaxBodyBytes > 128<<20 ||
		options.Delay < 0 || options.Delay > time.Minute ||
		int64(options.PageSize)*int64(options.MaxBodyBytes)*2 > maxBackfillBodyBudget {
		return BackfillResult{}, errors.New("Caido backfill limits are outside the bounded resource policy")
	}
	projectKey, err := selected.StorageKey()
	if err != nil {
		return BackfillResult{}, errors.New("Caido backfill project identity is invalid")
	}
	checkpoint, err := writer.BeginRun(ctx)
	if err != nil {
		return BackfillResult{}, err
	}
	result := BackfillResult{
		ProjectKey: projectKey, RunID: checkpoint.RunID,
		PageCount: checkpoint.PageCount, State: checkpoint.State,
		CoverageNote: "Caido provided no stable snapshot token or expected history total to reconcile completeness; the cursor reports enumeration progress only.",
	}
	cursor := checkpoint.Cursor
	var metadataDuration, detailDuration, pageProcessingDuration, graphProjectionDuration time.Duration

	markPartial := func(cause error) (BackfillResult, error) {
		partialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if markErr := writer.MarkPartial(partialCtx, checkpoint.RunID); markErr != nil {
			result.State = "partial"
			return result, errors.New("Caido backfill stopped and its checkpoint could not be marked partial")
		}
		result.State = "partial"
		return result, cause
	}

	for options.MaxPages == 0 || result.PagesThisRun < options.MaxPages {
		if err := ctx.Err(); err != nil {
			return markPartial(errors.New("Caido backfill was interrupted"))
		}
		metadataStarted := time.Now()
		metadata, err := source.ListRequestPage(selected, cursor, options.PageSize, nil)
		metadataDuration += time.Since(metadataStarted)
		result.MetadataDurationMS = durationMilliseconds(metadataDuration)
		if err != nil {
			return markPartial(err)
		}
		if err := validateMetadataPage(metadata, selected, options.PageSize, cursor); err != nil {
			return markPartial(err)
		}
		if len(metadata.Requests) == 0 {
			if metadata.HasMore {
				return markPartial(errors.New("Caido returned an empty advancing history page"))
			}
			checkpoint, err = writer.CompleteEmpty(ctx, checkpoint.RunID, cursor)
			if err != nil {
				return markPartial(err)
			}
			result.PageCount = checkpoint.PageCount
			result.State = checkpoint.State
			result.CursorState = "complete"
			return result, nil
		}

		detailsStarted := time.Now()
		page, err := source.FetchHistoryPage(selected, metadata, nil, options.MaxBodyBytes)
		detailDuration += time.Since(detailsStarted)
		result.DetailDurationMS = durationMilliseconds(detailDuration)
		if err != nil {
			return markPartial(err)
		}
		if err := validateFetchedPage(page, metadata, selected); err != nil {
			return markPartial(err)
		}
		pageWriteStarted := time.Now()
		pageResult, err := writer.WritePage(ctx, HistoryPageInput{RunID: checkpoint.RunID, Cursor: cursor, Page: page})
		pageProcessingDuration += time.Since(pageWriteStarted)
		result.PageProcessingDurationMS = durationMilliseconds(pageProcessingDuration)
		if err != nil {
			return markPartial(err)
		}
		graphProjectionDuration += pageResult.graphProjectionDuration
		result.GraphProjectionDurationMS = durationMilliseconds(graphProjectionDuration)
		result.PagesThisRun++
		result.PageCount = pageResult.PageCount
		result.Stored += pageResult.Stored
		result.Replayed += pageResult.Replayed
		result.Revised += pageResult.Revised
		if pageResult.ProjectionState == "pending" {
			result.ProjectionPendingPages++
		}
		result.State = pageResult.State
		result.CursorState = pageResult.CursorState
		if options.OnProgress != nil {
			options.OnProgress(BackfillProgress{
				PagesThisRun: result.PagesThisRun, PageCount: result.PageCount,
				Stored: result.Stored, Replayed: result.Replayed, Revised: result.Revised,
				ProjectionPendingPages: result.ProjectionPendingPages, State: result.State,
				CursorState: result.CursorState, MetadataDurationMS: result.MetadataDurationMS,
				DetailDurationMS: result.DetailDurationMS, PageProcessingDurationMS: result.PageProcessingDurationMS,
				GraphProjectionDurationMS: result.GraphProjectionDurationMS,
			})
		}
		if !page.HasMore {
			return result, nil
		}
		cursor = cloneCursor(page.NextCursor)
		if options.MaxPages > 0 && result.PagesThisRun == options.MaxPages {
			if err := writer.MarkPartial(ctx, checkpoint.RunID); err != nil {
				return markPartial(err)
			}
			result.State = "partial"
			result.LimitReached = true
			return result, nil
		}
		if options.Delay > 0 {
			timer := time.NewTimer(options.Delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return markPartial(errors.New("Caido backfill was interrupted"))
			}
		}
	}
	return result, nil
}

func durationMilliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func validateMetadataPage(page caido.RequestMetadataPage, selected project.CaidoProjectRef, pageSize int, cursor *string) error {
	if len(page.Requests) > pageSize || (page.HasMore && (page.NextCursor == nil || *page.NextCursor == "" || sameCursor(page.NextCursor, cursor))) ||
		(!page.HasMore && page.NextCursor != nil) {
		return errors.New("Caido returned an invalid history page boundary")
	}
	if page.NextCursor != nil && (len(*page.NextCursor) > maxCursorBytes || !utf8.ValidString(*page.NextCursor)) {
		return errors.New("Caido returned an invalid history page cursor")
	}
	seen := make(map[string]struct{}, len(page.Requests))
	for _, request := range page.Requests {
		if err := (project.CaidoExchangeRef{Project: selected, RequestID: request.RequestID}).Validate(); err != nil {
			return errors.New("Caido returned a history row without an identity")
		}
		if _, duplicate := seen[request.RequestID]; duplicate {
			return errors.New("Caido returned duplicate history identities")
		}
		seen[request.RequestID] = struct{}{}
	}
	if page.HasMore && len(page.Requests) == 0 {
		return errors.New("Caido returned an empty advancing history page")
	}
	return nil
}

func validateFetchedPage(page caido.HistoryPage, metadata caido.RequestMetadataPage, selected project.CaidoProjectRef) error {
	if len(page.Exchanges) != len(metadata.Requests) || page.HasMore != metadata.HasMore || !sameCursor(page.NextCursor, metadata.NextCursor) {
		return errors.New("Caido history details do not match their metadata page")
	}
	for index, exchange := range page.Exchanges {
		if err := exchange.Validate(); err != nil || !exchange.Ref.Project.SameProject(selected) || exchange.Ref.RequestID != metadata.Requests[index].RequestID {
			return errors.New("Caido history detail identity does not match its selected project row")
		}
	}
	return nil
}

func sameCursor(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneCursor(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
