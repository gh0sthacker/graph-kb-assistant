package caido

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	toolListProjects        = "caido_list_projects"
	toolSelectProject       = "caido_select_project"
	toolListRequests        = "caido_list_requests"
	toolGetRequest          = "caido_get_request"
	pageMaximum             = 100
	cursorMaximumBytes      = 4096
	detailBatchMaximum      = 8
	bodyChunkBytes          = 64 * 1024
	bodyMaximumBytes        = 128 * 1024 * 1024
	headerCountMaximum      = 2000
	HeaderBytesMaximum      = 128 * 1024
	MessageLineBytesMaximum = 64 * 1024
	TargetURLBytesMaximum   = MessageLineBytesMaximum + 512
)

// MaxSerializedMessageBytes returns a conservative upper bound for one
// reconstructed HTTP message, including its body, headers, and first line.
func MaxSerializedMessageBytes(bodyLimit int) int64 {
	if bodyLimit < 0 {
		return 0
	}
	return int64(bodyLimit) + int64(HeaderBytesMaximum) + int64(MessageLineBytesMaximum) + 4
}

var (
	processToolMu       = &sync.Mutex{}
	httpToken           = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
	http2PseudoHeader   = regexp.MustCompile(`^:[a-z0-9!#$%&'*+.^_` + "`" + `|~-]{1,128}$`)
	requestLinePattern  = regexp.MustCompile(`^(\S+)\s+(\S+)\s+(HTTP/\d(\.\d)?)$`)
	responseLinePattern = regexp.MustCompile(`^HTTP/\d(\.\d)?\s+(\d{3})(\s|$)`)
)

type ToolCaller interface {
	CallTool(name string, arguments map[string]any) (map[string]any, error)
}

type contextualToolCaller interface {
	CallToolContext(ctx context.Context, name string, arguments map[string]any) (map[string]any, error)
}

type callToolFunc func(name string, arguments map[string]any) (map[string]any, error)

func (f callToolFunc) CallTool(name string, arguments map[string]any) (map[string]any, error) {
	return f(name, arguments)
}

// MCPReadSource normalizes the four fixed, read-oriented Caido MCP tools.
// It serializes project selection with reads because the source API has no
// project-ID parameter on list/detail operations.
type MCPReadSource struct {
	caller     ToolCaller
	instanceID string
}

func NewMCPReadSource(caller ToolCaller, instanceID string) (*MCPReadSource, error) {
	if caller == nil {
		return nil, errors.New("call_tool is required")
	}
	if err := (ProjectRef{InstanceID: instanceID, ProjectID: "validation-only"}).Validate(); err != nil {
		return nil, errors.New("instance key must be a non-empty configured identifier")
	}
	return &MCPReadSource{caller: caller, instanceID: instanceID}, nil
}

func (s *MCPReadSource) ListProjects() ([]ProjectRef, error) {
	processToolMu.Lock()
	defer processToolMu.Unlock()
	payload, err := s.payload(toolListProjects, map[string]any{}, "")
	if err != nil {
		return nil, err
	}
	raw, exists := payload["projects"]
	if !exists {
		return nil, sanitized("Caido project response has an unsupported shape")
	}
	if raw == nil {
		return []ProjectRef{}, nil
	}
	rows, ok := raw.([]any)
	if !ok {
		return nil, sanitized("Caido project response has an unsupported shape")
	}
	projects := make([]ProjectRef, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	currentCount := 0
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, sanitized("Caido project response has an unsupported shape")
		}
		id, idOK := row["id"].(string)
		name, nameOK := row["name"].(string)
		_, statusOK := row["status"].(string)
		_, versionOK := row["version"].(string)
		current, currentOK := row["isCurrent"].(bool)
		if !idOK || !nameOK || !statusOK || !versionOK || !currentOK {
			return nil, sanitized("Caido project response has an unsupported shape")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, sanitized("Caido project response contains duplicate IDs")
		}
		seen[id] = struct{}{}
		project := ProjectRef{InstanceID: s.instanceID, ProjectID: id, DisplayName: stringPointer(name)}
		if project.Validate() != nil {
			return nil, sanitized("Caido project response contains an invalid ID")
		}
		projects = append(projects, project)
		if current {
			currentCount++
		}
	}
	if currentCount > 1 {
		return nil, sanitized("Caido reported an ambiguous selected project")
	}
	return projects, nil
}

// CurrentProject returns the project Caido already has selected. It never
// changes the shared selection.
func (s *MCPReadSource) CurrentProject() (ProjectRef, error) {
	return s.CurrentProjectContext(context.Background())
}

// CurrentProjectContext returns the project Caido already has selected. It
// never changes the shared selection and honors cancellation while reading
// project metadata.
func (s *MCPReadSource) CurrentProjectContext(ctx context.Context) (ProjectRef, error) {
	if ctx == nil {
		return ProjectRef{}, sanitized("Caido project context is required")
	}
	if err := ctx.Err(); err != nil {
		return ProjectRef{}, err
	}
	processToolMu.Lock()
	defer processToolMu.Unlock()
	projects, currentID, err := s.listProjectsUnlockedContext(ctx)
	if err != nil {
		return ProjectRef{}, err
	}
	if currentID == "" {
		return ProjectRef{}, sanitized("Caido has no selected project")
	}
	for _, project := range projects {
		if project.ProjectID == currentID {
			return project, nil
		}
	}
	return ProjectRef{}, sanitized("Caido selected project is unavailable")
}

func (s *MCPReadSource) ListRequestPage(project ProjectRef, cursor *string, pageSize int, httpql *string) (RequestMetadataPage, error) {
	if err := s.checkProject(project); err != nil {
		return RequestMetadataPage{}, err
	}
	if pageSize < 1 || pageSize > pageMaximum {
		return RequestMetadataPage{}, sanitized("Caido MCP pages must contain between 1 and 100 requests")
	}
	if cursor != nil && (*cursor == "" || len(*cursor) > cursorMaximumBytes) {
		return RequestMetadataPage{}, sanitized("Caido cursor is invalid")
	}
	if httpql != nil && (!strings.HasPrefix(*httpql, `req.created_at.gt:"`) || !strings.HasSuffix(*httpql, `"`) || len(*httpql) > 128) {
		return RequestMetadataPage{}, sanitized("Caido HTTPQL time filter is invalid")
	}
	processToolMu.Lock()
	defer processToolMu.Unlock()
	var page RequestMetadataPage
	err := s.withSelectedProject(project, func() error {
		args := map[string]any{"limit": pageSize}
		if cursor != nil {
			args["after"] = *cursor
		}
		if httpql != nil {
			args["httpql"] = *httpql
		}
		payload, err := s.payload(toolListRequests, args, project.ProjectID)
		if err != nil {
			return err
		}
		page, err = parseRequestPage(payload, cursor)
		return err
	})
	if err != nil {
		return RequestMetadataPage{}, err
	}
	return page, nil
}

// ListRequestPageNoSwitch reads one metadata page only when the requested
// project is already selected. It verifies the selection before and after the
// request and never invokes project selection or request-detail tools.
func (s *MCPReadSource) ListRequestPageNoSwitch(project ProjectRef, cursor *string, pageSize int) (RequestMetadataPage, error) {
	return s.ListRequestPageNoSwitchContext(context.Background(), project, cursor, pageSize)
}

// ListRequestPageNoSwitchContext reads one metadata page only when the
// requested project is already selected. It checks selection around each
// Caido call, honors cancellation, and never selects a project or reads
// request details.
func (s *MCPReadSource) ListRequestPageNoSwitchContext(ctx context.Context, project ProjectRef, cursor *string, pageSize int) (RequestMetadataPage, error) {
	return s.listRequestPageNoSwitchContext(ctx, project, cursor, pageSize, nil)
}

// ListRequestPageNoSwitchHostContext lists one exact-host metadata page while
// leaving Caido's selected project untouched.
func (s *MCPReadSource) ListRequestPageNoSwitchHostContext(ctx context.Context, project ProjectRef, cursor *string, pageSize int, host string) (RequestMetadataPage, error) {
	filter, err := RequestHostHTTPQLFilter(host)
	if err != nil {
		return RequestMetadataPage{}, err
	}
	return s.listRequestPageNoSwitchContext(ctx, project, cursor, pageSize, &filter)
}

func (s *MCPReadSource) listRequestPageNoSwitchContext(ctx context.Context, project ProjectRef, cursor *string, pageSize int, httpql *string) (RequestMetadataPage, error) {
	if ctx == nil {
		return RequestMetadataPage{}, sanitized("Caido request context is required")
	}
	if err := ctx.Err(); err != nil {
		return RequestMetadataPage{}, err
	}
	if err := s.checkProject(project); err != nil {
		return RequestMetadataPage{}, err
	}
	if pageSize < 1 || pageSize > pageMaximum {
		return RequestMetadataPage{}, sanitized("Caido MCP pages must contain between 1 and 100 requests")
	}
	if cursor != nil && (*cursor == "" || len(*cursor) > cursorMaximumBytes) {
		return RequestMetadataPage{}, sanitized("Caido cursor is invalid")
	}

	processToolMu.Lock()
	defer processToolMu.Unlock()
	if err := ctx.Err(); err != nil {
		return RequestMetadataPage{}, err
	}
	_, currentID, err := s.listProjectsUnlockedContext(ctx)
	if err != nil {
		return RequestMetadataPage{}, err
	}
	if currentID != project.ProjectID {
		return RequestMetadataPage{}, sanitized("Requested Caido project is not already selected")
	}

	args := map[string]any{"limit": pageSize}
	if cursor != nil {
		args["after"] = *cursor
	}
	if httpql != nil {
		args["httpql"] = *httpql
	}
	payload, err := s.payloadContext(ctx, toolListRequests, args, project.ProjectID)
	if err != nil {
		return RequestMetadataPage{}, err
	}
	page, err := parseRequestPage(payload, cursor)
	if err != nil {
		return RequestMetadataPage{}, err
	}
	return page, nil
}

func (s *MCPReadSource) FetchHistoryPage(project ProjectRef, page RequestMetadataPage, skipIDs map[string]struct{}, maxBodyBytes int) (HistoryPage, error) {
	if err := s.checkProject(project); err != nil {
		return HistoryPage{}, err
	}
	if page.HasMore && (page.NextCursor == nil || *page.NextCursor == "") {
		return HistoryPage{}, sanitized("Caido request metadata cursor is invalid")
	}
	if maxBodyBytes < bodyChunkBytes || maxBodyBytes > bodyMaximumBytes {
		return HistoryPage{}, sanitized("Caido live body limit is outside supported bounds")
	}
	rows := make([]RequestMetadata, 0, len(page.Requests))
	seen := make(map[string]struct{}, len(page.Requests))
	for _, row := range page.Requests {
		if _, skip := skipIDs[row.RequestID]; skip {
			continue
		}
		if _, duplicate := seen[row.RequestID]; duplicate {
			return HistoryPage{}, sanitized("Caido request metadata page contains duplicate IDs")
		}
		seen[row.RequestID] = struct{}{}
		rows = append(rows, row)
	}
	if len(rows) > pageMaximum {
		return HistoryPage{}, sanitized("Caido request metadata page contains too many rows")
	}
	processToolMu.Lock()
	defer processToolMu.Unlock()
	var result HistoryPage
	err := s.withSelectedProject(project, func() error {
		result.Exchanges = make([]HttpExchange, 0, len(rows))
		for start := 0; start < len(rows); start += detailBatchMaximum {
			end := start + detailBatchMaximum
			if end > len(rows) {
				end = len(rows)
			}
			batch := rows[start:end]
			ids := make([]string, 0, len(batch))
			for _, row := range batch {
				ids = append(ids, row.RequestID)
			}
			payloads, err := s.getDetails(ids, 0, project.ProjectID)
			if err != nil {
				return err
			}
			var batchErr error
			for rowIndex, row := range batch {
				exchange, err := s.fetchExchange(project, row, payloads[row.RequestID], maxBodyBytes)
				if err != nil {
					batchErr = sanitized("Caido history page row " + strconv.Itoa(start+rowIndex+1) + ": " + err.Error())
					break
				}
				result.Exchanges = append(result.Exchanges, exchange)
			}
			releaseDetailPayloads(payloads)
			if batchErr != nil {
				return batchErr
			}
		}
		result.NextCursor = cloneString(page.NextCursor)
		result.HasMore = page.HasMore
		return nil
	})
	if err != nil {
		for index := range result.Exchanges {
			clear(result.Exchanges[index].RequestRaw)
			clear(result.Exchanges[index].ResponseRaw)
		}
		return HistoryPage{}, err
	}
	return result, nil
}

func (s *MCPReadSource) GetExchange(ref ExchangeRef) (*HttpExchange, error) {
	if err := ref.Validate(); err != nil {
		return nil, sanitized("Caido exchange reference is invalid")
	}
	if err := s.checkProject(ref.Project); err != nil {
		return nil, err
	}
	processToolMu.Lock()
	defer processToolMu.Unlock()
	var exchange *HttpExchange
	err := s.withSelectedProject(ref.Project, func() error {
		metadata, err := s.findRequestMetadata(ref.RequestID, ref.Project.ProjectID)
		if err != nil || metadata == nil {
			return err
		}
		payloads, err := s.getDetails([]string{ref.RequestID}, 0, ref.Project.ProjectID)
		if err != nil {
			return err
		}
		value, err := s.fetchExchange(ref.Project, *metadata, payloads[ref.RequestID], bodyMaximumBytes)
		if err != nil {
			return err
		}
		exchange = &value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return exchange, nil
}

func (s *MCPReadSource) checkProject(project ProjectRef) error {
	if err := project.Validate(); err != nil || project.InstanceID != s.instanceID {
		return sanitized("Project belongs to a different configured Caido instance")
	}
	return nil
}

func (s *MCPReadSource) withSelectedProject(project ProjectRef, action func() error) error {
	projects, originalID, err := s.listProjectsUnlocked()
	if err != nil {
		return err
	}
	known := false
	for _, candidate := range projects {
		if candidate.SameProject(project) {
			known = true
			break
		}
	}
	if !known {
		return sanitized("Requested Caido project is not available")
	}
	if originalID == project.ProjectID {
		if err := action(); err != nil {
			return err
		}
		return s.assertCurrentProject(project.ProjectID)
	}
	if originalID == "" {
		return sanitized("Cannot switch projects while preserving an empty selection")
	}
	if err := s.selectProject(project.ProjectID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if restoreErr := s.restoreProjectSelection(cleanupCtx, project.ProjectID, originalID); restoreErr != nil {
			return sanitized("Failed to restore the original Caido project")
		}
		return err
	}
	actionErr := action()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.restoreProjectSelection(cleanupCtx, project.ProjectID, originalID); err != nil {
		return sanitized("Failed to restore the original Caido project")
	}
	if actionErr != nil {
		return actionErr
	}
	return nil
}

func (s *MCPReadSource) restoreProjectSelection(ctx context.Context, selectedID, originalID string) error {
	_, currentID, err := s.listProjectsUnlockedContext(ctx)
	if err != nil {
		return err
	}
	if currentID == selectedID {
		return s.selectProjectContext(ctx, originalID)
	}
	if currentID != originalID {
		return sanitized("Caido project selection changed externally")
	}
	return nil
}

func (s *MCPReadSource) listProjectsUnlocked() ([]ProjectRef, string, error) {
	return s.listProjectsUnlockedContext(nil)
}

func (s *MCPReadSource) listProjectsUnlockedContext(ctx context.Context) ([]ProjectRef, string, error) {
	var payload map[string]any
	var err error
	if ctx == nil {
		payload, err = s.payload(toolListProjects, map[string]any{}, "")
	} else {
		payload, err = s.payloadContext(ctx, toolListProjects, map[string]any{}, "")
	}
	if err != nil {
		return nil, "", err
	}
	raw, exists := payload["projects"]
	if !exists {
		return nil, "", sanitized("Caido project response has an unsupported shape")
	}
	if raw == nil {
		return []ProjectRef{}, "", nil
	}
	rows, ok := raw.([]any)
	if !ok {
		return nil, "", sanitized("Caido project response has an unsupported shape")
	}
	projects := make([]ProjectRef, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	current := ""
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, "", sanitized("Caido project response has an unsupported shape")
		}
		id, a := row["id"].(string)
		name, b := row["name"].(string)
		_, c := row["status"].(string)
		_, d := row["version"].(string)
		isCurrent, e := row["isCurrent"].(bool)
		if !a || !b || !c || !d || !e {
			return nil, "", sanitized("Caido project response has an unsupported shape")
		}
		if _, exists := seen[id]; exists {
			return nil, "", sanitized("Caido project response contains duplicate IDs")
		}
		seen[id] = struct{}{}
		p := ProjectRef{InstanceID: s.instanceID, ProjectID: id, DisplayName: stringPointer(name)}
		if p.Validate() != nil {
			return nil, "", sanitized("Caido project response contains an invalid ID")
		}
		projects = append(projects, p)
		if isCurrent {
			if current != "" {
				return nil, "", sanitized("Caido reported an ambiguous selected project")
			}
			current = id
		}
	}
	return projects, current, nil
}

func (s *MCPReadSource) selectProject(projectID string) error {
	return s.selectProjectContext(nil, projectID)
}

func (s *MCPReadSource) selectProjectContext(ctx context.Context, projectID string) error {
	var payload map[string]any
	var err error
	if ctx == nil {
		payload, err = s.payload(toolSelectProject, map[string]any{"id": projectID}, "")
	} else {
		payload, err = s.payloadContext(ctx, toolSelectProject, map[string]any{"id": projectID}, "")
	}
	if err != nil {
		return err
	}
	id, idOK := payload["id"].(string)
	_, nameOK := payload["name"].(string)
	if !idOK || id != projectID || !nameOK {
		return sanitized("Caido project selection was not confirmed")
	}
	var current string
	var projectsErr error
	if ctx == nil {
		_, current, projectsErr = s.listProjectsUnlocked()
	} else {
		_, current, projectsErr = s.listProjectsUnlockedContext(ctx)
	}
	if projectsErr != nil {
		return projectsErr
	}
	if current != projectID {
		return sanitized("Caido active project does not match the requested project")
	}
	return nil
}

func (s *MCPReadSource) assertCurrentProject(projectID string) error {
	_, current, err := s.listProjectsUnlocked()
	if err != nil {
		return err
	}
	if current != projectID {
		return sanitized("Caido active project changed during history read")
	}
	return nil
}

func (s *MCPReadSource) payload(tool string, args map[string]any, expectedProject string) (map[string]any, error) {
	if expectedProject != "" {
		if err := s.assertCurrentProject(expectedProject); err != nil {
			return nil, err
		}
	}
	result, err := s.caller.CallTool(tool, args)
	if err != nil {
		releaseToolResult(result)
		return nil, sanitized("Caido MCP call failed")
	}
	if expectedProject != "" {
		if err := s.assertCurrentProject(expectedProject); err != nil {
			releaseToolResult(result)
			return nil, err
		}
	}
	payload, decodeErr := decodeToolPayload(result)
	releaseToolResult(result)
	return payload, decodeErr
}

func (s *MCPReadSource) payloadContext(ctx context.Context, tool string, args map[string]any, expectedProject string) (map[string]any, error) {
	if expectedProject != "" {
		if err := s.assertCurrentProjectContext(ctx, expectedProject); err != nil {
			return nil, err
		}
	}
	var result map[string]any
	var err error
	if caller, ok := s.caller.(contextualToolCaller); ok {
		result, err = caller.CallToolContext(ctx, tool, args)
	} else {
		result, err = s.caller.CallTool(tool, args)
	}
	if err != nil {
		releaseToolResult(result)
		return nil, sanitized("Caido MCP call failed")
	}
	if expectedProject != "" {
		if err := s.assertCurrentProjectContext(ctx, expectedProject); err != nil {
			releaseToolResult(result)
			return nil, err
		}
	}
	payload, decodeErr := decodeToolPayload(result)
	releaseToolResult(result)
	return payload, decodeErr
}

func releaseToolResult(result map[string]any) {
	if content, ok := result["content"].([]any); ok {
		for _, item := range content {
			if block, ok := item.(map[string]any); ok {
				clear(block)
			}
		}
		clear(content)
	}
	clear(result)
}

func decodeToolPayload(result map[string]any) (map[string]any, error) {
	if result == nil {
		return nil, sanitized("Caido MCP returned an empty result")
	}
	if isError, exists := result["isError"]; exists {
		flag, ok := isError.(bool)
		if !ok || flag {
			return nil, sanitized("Caido MCP call returned an error")
		}
	}
	if raw, exists := result["structuredContent"]; exists && raw != nil {
		payload, ok := raw.(map[string]any)
		if !ok {
			return nil, sanitized("Caido MCP structured result has an unsupported shape")
		}
		return payload, nil
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		return nil, sanitized("Caido MCP result has an unsupported shape")
	}
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			return nil, sanitized("Caido MCP result has an unsupported shape")
		}
		if block["type"] == "text" {
			text, ok := block["text"].(string)
			if !ok {
				return nil, sanitized("Caido MCP result has an unsupported shape")
			}
			textBytes := []byte(text)
			var payload map[string]any
			err := json.Unmarshal(textBytes, &payload)
			clear(textBytes)
			if err != nil || payload == nil {
				return nil, sanitized("Caido MCP result text has an unsupported shape")
			}
			return payload, nil
		}
	}
	return nil, sanitized("Caido MCP result has no supported content")
}

func (s *MCPReadSource) assertCurrentProjectContext(ctx context.Context, projectID string) error {
	_, current, err := s.listProjectsUnlockedContext(ctx)
	if err != nil {
		return err
	}
	if current != projectID {
		return sanitized("Caido project selection changed during history read")
	}
	return nil
}

func parseRequestPage(payload map[string]any, previous *string) (RequestMetadataPage, error) {
	raw, exists := payload["requests"]
	hasMore, hasMoreOK := payload["hasMore"].(bool)
	if !exists || !hasMoreOK {
		return RequestMetadataPage{}, sanitized("Caido request page has an unsupported shape")
	}
	var rows []any
	if raw == nil {
		rows = []any{}
	} else {
		var ok bool
		rows, ok = raw.([]any)
		if !ok {
			return RequestMetadataPage{}, sanitized("Caido request page has an unsupported shape")
		}
	}
	var next *string
	if value, exists := payload["nextCursor"]; exists && value != nil {
		cursor, ok := value.(string)
		if !ok || cursor == "" || len(cursor) > cursorMaximumBytes {
			return RequestMetadataPage{}, sanitized("Caido request page has an invalid cursor")
		}
		next = &cursor
	}
	if hasMore && (next == nil || (previous != nil && *next == *previous)) {
		return RequestMetadataPage{}, sanitized("Caido pagination did not advance")
	}
	requests := make([]RequestMetadata, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			return RequestMetadataPage{}, sanitized("Caido request page has an unsupported shape")
		}
		id, idOK := row["id"].(string)
		method, methodOK := row["method"].(string)
		target, urlOK := row["url"].(string)
		if !idOK || id == "" || len(id) > 512 || !methodOK || !urlOK {
			return RequestMetadataPage{}, sanitized("Caido request page has an unsupported shape")
		}
		if _, duplicate := seen[id]; duplicate {
			return RequestMetadataPage{}, sanitized("Caido request page contains duplicate IDs")
		}
		seen[id] = struct{}{}
		status, err := optionalStatus(row["statusCode"])
		if err != nil {
			return RequestMetadataPage{}, sanitized("Caido request page has an invalid status")
		}
		requests = append(requests, RequestMetadata{RequestID: id, Method: method, URL: target, StatusCode: status})
	}
	// Preserve valid empty advancing pages for the consumer to interpret:
	// live-tail treats them as end-of-history; historical backfill stays strict.
	if !hasMore {
		next = nil
	}
	return RequestMetadataPage{Requests: requests, NextCursor: next, HasMore: hasMore}, nil
}

func (s *MCPReadSource) findRequestMetadata(requestID, projectID string) (*RequestMetadata, error) {
	var cursor *string
	seen := make(map[string]struct{})
	for {
		if cursor != nil {
			if _, duplicate := seen[*cursor]; duplicate {
				return nil, sanitized("Caido request metadata pagination repeated a cursor")
			}
			seen[*cursor] = struct{}{}
		}
		args := map[string]any{"limit": pageMaximum}
		if cursor != nil {
			args["after"] = *cursor
		}
		payload, err := s.payload(toolListRequests, args, projectID)
		if err != nil {
			return nil, err
		}
		page, err := parseRequestPage(payload, cursor)
		if err != nil {
			return nil, err
		}
		for _, row := range page.Requests {
			if row.RequestID == requestID {
				copy := row
				return &copy, nil
			}
		}
		if !page.HasMore {
			return nil, nil
		}
		cursor = page.NextCursor
	}
}

func (s *MCPReadSource) getDetails(ids []string, bodyOffset int, projectID string) (map[string]map[string]any, error) {
	if len(ids) == 0 || len(ids) > detailBatchMaximum || bodyOffset < 0 {
		return nil, sanitized("Caido detail batch is invalid")
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			return nil, sanitized("Caido detail batch is invalid")
		}
		seen[id] = struct{}{}
	}
	payload, err := s.payload(toolGetRequest, map[string]any{
		"ids":        ids,
		"include":    []string{"requestHeaders", "requestBody", "responseHeaders", "responseBody"},
		"bodyLimit":  bodyChunkBytes,
		"bodyOffset": bodyOffset,
	}, projectID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 1 {
		if _, hasRequests := payload["requests"]; hasRequests || payload["id"] != ids[0] {
			return nil, sanitized("Caido single exchange response did not match the requested ID")
		}
		return map[string]map[string]any{ids[0]: payload}, nil
	}
	raw, ok := payload["requests"].([]any)
	if _, hasID := payload["id"]; hasID || !ok {
		return nil, sanitized("Caido batch exchange response has an unsupported shape")
	}
	items := make(map[string]map[string]any, len(raw))
	for _, value := range raw {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, sanitized("Caido batch exchange response has an unsupported shape")
		}
		id, ok := item["id"].(string)
		if !ok {
			return nil, sanitized("Caido batch exchange response IDs do not match the request")
		}
		if _, expected := seen[id]; !expected {
			return nil, sanitized("Caido batch exchange response IDs do not match the request")
		}
		if _, duplicate := items[id]; duplicate {
			return nil, sanitized("Caido batch exchange response IDs do not match the request")
		}
		items[id] = item
	}
	if len(items) != len(ids) {
		return nil, sanitized("Caido batch exchange response IDs do not match the request")
	}
	return items, nil
}

// releaseDetailPayloads drops references to the decoded Caido payload maps as
// soon as a detail batch has been normalized. Go strings are immutable, so
// their backing bytes cannot be reliably zeroed by this package.
func releaseDetailPayloads(payloads map[string]map[string]any) {
	for id, payload := range payloads {
		clearExchangeDetailPayload(payload)
		delete(payloads, id)
	}
	clear(payloads)
}

func clearExchangeDetailPayload(payload map[string]any) {
	for _, sideName := range []string{"request", "response"} {
		if side, ok := payload[sideName].(map[string]any); ok {
			clear(side)
		}
	}
	clear(payload)
}

func (s *MCPReadSource) fetchExchange(project ProjectRef, metadata RequestMetadata, payload map[string]any, maxBodyBytes int) (exchange HttpExchange, retErr error) {
	var requestRaw, responseRaw []byte
	defer func() {
		if retErr != nil {
			clear(requestRaw)
			clear(responseRaw)
		}
	}()
	request, ok := payload["request"].(map[string]any)
	if !ok {
		return HttpExchange{}, sanitized("Caido exchange is missing its request")
	}
	var response map[string]any
	if raw, exists := payload["response"]; exists && raw != nil {
		var valid bool
		response, valid = raw.(map[string]any)
		if !valid {
			return HttpExchange{}, sanitized("Caido response has an unsupported shape")
		}
	}
	method := metadata.Method
	requestLine, err := parseRequestLine(request["firstLine"], method)
	if err != nil {
		return HttpExchange{}, err
	}
	if method == "" {
		method = strings.ToUpper(strings.SplitN(requestLine, " ", 2)[0])
	}
	requestHeaders, err := parseHeaders(request["headers"])
	if err != nil {
		return HttpExchange{}, sanitized("Caido request headers are invalid: " + err.Error())
	}
	malformedHeaderNamePresent := hasMalformedHeaderName(requestHeaders, requestLine)
	requestSize, err := bodySize(request, true)
	if err != nil {
		return HttpExchange{}, err
	}
	var responseLine string
	var responseHeaders [][2]string
	var responseSize int
	var responseStatus *int
	responseStatusMetadataMismatch := response == nil && metadata.StatusCode != nil
	if response != nil {
		responseLine, responseStatus, err = parseResponseLine(response["firstLine"])
		if err != nil {
			return HttpExchange{}, err
		}
		responseStatusMetadataMismatch = responseStatusMetadataMismatch ||
			metadata.StatusCode != nil && *responseStatus != *metadata.StatusCode
		responseHeaders, err = parseHeaders(response["headers"])
		if err != nil {
			return HttpExchange{}, sanitized("Caido response headers are invalid: " + err.Error())
		}
		malformedHeaderNamePresent = malformedHeaderNamePresent || hasMalformedHeaderName(responseHeaders, responseLine)
		responseSize, err = bodySize(response, false)
		if err != nil {
			return HttpExchange{}, err
		}
	}
	if requestSize > bodyMaximumBytes || responseSize > bodyMaximumBytes {
		return HttpExchange{}, sanitized("Caido body exceeds the local ingestion limit")
	}
	requestBody, requestTruncated, err := s.readBody(metadata.RequestID, "request", payload, requestSize, requestHeaders, project.ProjectID, maxBodyBytes)
	if err != nil {
		return HttpExchange{}, err
	}
	requestRaw, err = serializeMessage(requestLine, requestHeaders, requestBody)
	clear(requestBody)
	if err != nil {
		return HttpExchange{}, err
	}
	var responseTruncated *bool
	if response != nil {
		responseBody, truncated, bodyErr := s.readBody(metadata.RequestID, "response", payload, responseSize, responseHeaders, project.ProjectID, maxBodyBytes)
		if bodyErr != nil {
			clear(responseBody)
			return HttpExchange{}, bodyErr
		}
		responseTruncated = truncated
		responseRaw, err = serializeMessage(responseLine, responseHeaders, responseBody)
		clear(responseBody)
		if err != nil {
			return HttpExchange{}, err
		}
	}
	parsedURL, err := url.Parse(metadata.URL)
	if err != nil || (strings.ToLower(parsedURL.Scheme) != "http" && strings.ToLower(parsedURL.Scheme) != "https") || parsedURL.Hostname() == "" || parsedURL.User != nil {
		return HttpExchange{}, sanitized("Caido exchange URL is invalid")
	}
	port := 80
	if strings.EqualFold(parsedURL.Scheme, "https") {
		port = 443
	}
	if portText := parsedURL.Port(); portText != "" {
		p, err := strconv.Atoi(portText)
		if err != nil || p < 1 || p > 65535 {
			return HttpExchange{}, sanitized("Caido exchange URL is invalid")
		}
		port = p
	}
	return HttpExchange{
		Ref:        ExchangeRef{Project: project, RequestID: metadata.RequestID},
		RequestRaw: requestRaw, ResponseRaw: responseRaw, HasResponse: response != nil,
		Method: method, Scheme: strings.ToLower(parsedURL.Scheme), Host: parsedURL.Hostname(), Port: port,
		Target: metadata.URL, MalformedHeaderNamePresent: malformedHeaderNamePresent,
		ResponseStatus: responseStatus, ResponseStatusMetadataMismatch: responseStatusMetadataMismatch,
		RequestTruncated: requestTruncated, ResponseTruncated: responseTruncated,
		RequestWireFidelity: "reconstructed_text", ResponseWireFidelity: choose(response != nil, "reconstructed_text", "unknown"),
	}, nil
}

func (s *MCPReadSource) readBody(requestID, side string, initial map[string]any, totalSize int, headers [][2]string, projectID string, maxBodyBytes int) ([]byte, *bool, error) {
	if totalSize == 0 {
		if detail, ok := initial[side].(map[string]any); ok {
			body, bodyExists := detail["body"]
			truncated, truncExists := detail["truncated"]
			if bodyExists && body != nil && body != "" {
				return nil, nil, sanitized("Caido empty body metadata is inconsistent")
			}
			if truncExists && truncated != nil && truncated != false {
				return nil, nil, sanitized("Caido empty body metadata is inconsistent")
			}
		}
		value := false
		return []byte{}, &value, nil
	}
	baseline, ok := initial[side].(map[string]any)
	if !ok {
		return []byte{}, nil, nil
	}
	baselineHeaders := baseline["headers"]
	baselineLine := baseline["firstLine"]
	canPage := canPageUTF8Body(headers)
	var assembled []byte
	var offset int
	var current = initial
	currentOwned := false
	defer func() {
		if currentOwned {
			clearExchangeDetailPayload(current)
		}
	}()
	lossyMarker := false
	for offset < min(totalSize, maxBodyBytes) {
		detail, ok := current[side].(map[string]any)
		if !ok {
			clear(assembled)
			return nil, nil, sanitized("Caido body detail is incomplete or changed during paging")
		}
		size, err := bodySize(detail, side == "request")
		if err != nil || size != totalSize || !reflect.DeepEqual(detail["headers"], baselineHeaders) || detail["firstLine"] != baselineLine {
			clear(assembled)
			return nil, nil, sanitized("Caido body detail is incomplete or changed during paging")
		}
		body, ok := detail["body"].(string)
		if !ok {
			return assembled, nil, nil
		}
		if strings.ContainsRune(body, '\uFFFD') {
			lossyMarker = true
		}
		if len(body) == 0 {
			if detail["truncated"] == true {
				value := true
				return assembled, &value, nil
			}
			return assembled, nil, nil
		}
		remainingBody := maxBodyBytes - offset
		remainingDeclared := totalSize - offset
		if len(body) > remainingBody || len(body) > remainingDeclared {
			limit := min(remainingBody, remainingDeclared)
			assembled = append(assembled, body[:limit]...)
			value := true
			return assembled, &value, nil
		}
		expectedMore := offset+len(body) < totalSize
		if truncated, exists := detail["truncated"]; exists && truncated != nil {
			flag, ok := truncated.(bool)
			if !ok || (flag == false && expectedMore) || (flag == true && !expectedMore) {
				return append(assembled, body...), nil, nil
			}
		}
		assembled = append(assembled, body...)
		offset += len(body)
		if offset >= maxBodyBytes && offset < totalSize {
			value := true
			return assembled, &value, nil
		}
		if !expectedMore {
			if lossyMarker {
				return assembled, nil, nil
			}
			value := false
			return assembled, &value, nil
		}
		if !canPage {
			if detail["truncated"] == true {
				value := true
				return assembled, &value, nil
			}
			return assembled, nil, nil
		}
		body = ""
		if currentOwned {
			clearExchangeDetailPayload(current)
			currentOwned = false
		}
		payloads, err := s.getDetails([]string{requestID}, offset, projectID)
		if err != nil {
			value := true
			return assembled, &value, nil
		}
		current = payloads[requestID]
		delete(payloads, requestID)
		clear(payloads)
		currentOwned = true
	}
	return assembled, nil, nil
}

func optionalStatus(value any) (*int, error) {
	if value == nil {
		return nil, nil
	}
	status, ok := integer(value)
	if !ok || status < 100 || status > 599 {
		return nil, errors.New("invalid status")
	}
	return &status, nil
}

func parseRequestLine(value any, metadataMethod string) (string, error) {
	line, ok := value.(string)
	if !ok || len(line) > MessageLineBytesMaximum || strings.ContainsAny(line, "\r\n") {
		return "", sanitized("Caido request line is missing or unsupported")
	}
	parts := requestLinePattern.FindStringSubmatch(line)
	if parts == nil || !httpToken.MatchString(parts[1]) {
		return "", sanitized("Caido request line is unsupported")
	}
	if metadataMethod != "" && !strings.EqualFold(parts[1], metadataMethod) {
		return "", sanitized("Caido request method does not match request metadata")
	}
	if !latin1Representable(line) {
		return "", sanitized("Caido request line cannot be represented as HTTP bytes")
	}
	return line, nil
}

func parseResponseLine(value any) (string, *int, error) {
	line, ok := value.(string)
	if !ok || len(line) > MessageLineBytesMaximum || strings.ContainsAny(line, "\r\n") {
		return "", nil, sanitized("Caido response line is missing or unsupported")
	}
	parts := responseLinePattern.FindStringSubmatch(line)
	if parts == nil || !latin1Representable(line) {
		return "", nil, sanitized("Caido response line is unsupported")
	}
	status, _ := strconv.Atoi(parts[2])
	if status < 100 || status > 599 {
		return "", nil, sanitized("Caido response status is invalid")
	}
	return line, &status, nil
}

func bodySize(detail map[string]any, requestSide bool) (int, error) {
	fingerprint, ok := detail["fingerprint"].(map[string]any)
	if !ok {
		return 0, sanitized("Caido body fingerprint is missing or unsupported")
	}
	kind, kindOK := fingerprint["kind"].(string)
	if !kindOK {
		return 0, sanitized("Caido body fingerprint is missing or unsupported")
	}
	var sizeValue any
	if requestSide {
		sizeValue = fingerprint["bodySize"]
	} else {
		sizeValue = detail["bodySize"]
		if sizeValue == nil && kind == "empty" {
			if fingerprintSize, ok := integer(fingerprint["bodySize"]); ok && fingerprintSize == 0 {
				sizeValue = 0
			}
		}
	}
	size, ok := integer(sizeValue)
	if !ok || size < 0 {
		return 0, sanitized("Caido body size is unknown")
	}
	for _, declared := range []any{detail["bodySize"], fingerprint["bodySize"]} {
		if declared == nil {
			continue
		}
		value, ok := integer(declared)
		if !ok || value != size {
			return 0, sanitized("Caido body size declarations disagree")
		}
	}
	return size, nil
}

func parseHeaders(value any) ([][2]string, error) {
	rows, ok := value.([]any)
	if !ok {
		return nil, sanitized("Caido exchange headers are missing or unsupported")
	}
	if len(rows) > headerCountMaximum {
		return nil, sanitized("Caido exchange contains too many headers")
	}
	headers := make([][2]string, 0, len(rows))
	totalBytes := 0
	for index, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, sanitized("Caido exchange header " + strconv.Itoa(index+1) + " has an unsupported shape")
		}
		name, nameOK := row["name"].(string)
		value, valueOK := row["value"].(string)
		if !nameOK {
			return nil, sanitized("Caido exchange header " + strconv.Itoa(index+1) + " has a non-text name")
		}
		for _, r := range name {
			if unicode.IsControl(r) {
				return nil, sanitized("Caido exchange header " + strconv.Itoa(index+1) + " has a control character in its name")
			}
		}
		if !valueOK {
			return nil, sanitized("Caido exchange header " + strconv.Itoa(index+1) + " has a non-text value")
		}
		for _, r := range value {
			if (r < 32 && r != '\t') || r == 127 {
				return nil, sanitized("Caido exchange header " + strconv.Itoa(index+1) + " contains a disallowed control character")
			}
		}
		totalBytes += len(name) + len(value) + 4
		if totalBytes > HeaderBytesMaximum {
			return nil, sanitized("Caido exchange headers exceed the local size limit")
		}
		headers = append(headers, [2]string{name, value})
	}
	return headers, nil
}

func hasMalformedHeaderName(headers [][2]string, messageLine string) bool {
	allowPseudoHeaders := false
	for _, field := range strings.Fields(messageLine) {
		if field == "HTTP/2" || field == "HTTP/3" {
			allowPseudoHeaders = true
			break
		}
	}
	for _, header := range headers {
		if httpToken.MatchString(header[0]) {
			continue
		}
		if allowPseudoHeaders && http2PseudoHeader.MatchString(header[0]) {
			continue
		}
		if !httpToken.MatchString(header[0]) {
			return true
		}
	}
	return false
}

func declaredUTF8Text(headers [][2]string) bool {
	var values []string
	for _, header := range headers {
		if strings.EqualFold(header[0], "content-type") {
			values = append(values, header[1])
		}
	}
	if len(values) != 1 {
		return false
	}
	components := strings.Split(values[0], ";")
	mediaType := strings.ToLower(strings.TrimSpace(components[0]))
	var charsets []string
	for _, param := range components[1:] {
		key, value, ok := strings.Cut(param, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "charset") {
			charsets = append(charsets, strings.ToLower(strings.Trim(strings.TrimSpace(value), `"`)))
		}
	}
	if len(charsets) > 1 || (len(charsets) == 1 && charsets[0] != "utf-8" && charsets[0] != "utf8") {
		return false
	}
	isJSON := mediaType == "application/json" || (strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json"))
	if isJSON {
		return true
	}
	hasCharset := len(charsets) == 1
	return hasCharset && (strings.HasPrefix(mediaType, "text/") || mediaType == "application/xml" || mediaType == "application/x-www-form-urlencoded" || strings.HasSuffix(mediaType, "+xml"))
}

func canPageUTF8Body(headers [][2]string) bool {
	if !declaredUTF8Text(headers) {
		return false
	}
	for _, header := range headers {
		if strings.EqualFold(header[0], "content-encoding") {
			encoding := strings.ToLower(strings.TrimSpace(header[1]))
			if encoding != "" && encoding != "identity" {
				return false
			}
		}
	}
	return true
}

func serializeMessage(firstLine string, headers [][2]string, body []byte) ([]byte, error) {
	if !latin1Representable(firstLine) {
		return nil, sanitized("Caido message metadata cannot be serialized as HTTP bytes")
	}
	var buffer bytes.Buffer
	firstLineBytes := latin1Bytes(firstLine)
	buffer.Write(firstLineBytes)
	clear(firstLineBytes)
	buffer.WriteString("\r\n")
	for _, header := range headers {
		buffer.WriteString(header[0])
		buffer.WriteString(": ")
		headerValue := headerValueBytes(header[1])
		buffer.Write(headerValue)
		clear(headerValue)
		buffer.WriteString("\r\n")
	}
	buffer.WriteString("\r\n")
	buffer.Write(body)
	return buffer.Bytes(), nil
}

func latin1Representable(value string) bool {
	for _, r := range value {
		if r > 255 {
			return false
		}
	}
	return true
}

func latin1Bytes(value string) []byte {
	result := make([]byte, 0, len(value))
	for _, r := range value {
		result = append(result, byte(r))
	}
	return result
}

func headerValueBytes(value string) []byte {
	if latin1Representable(value) {
		return latin1Bytes(value)
	}
	return []byte(value)
}

func integer(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		return int(number), int64(int(number)) == number
	case uint64:
		return int(number), uint64(int(number)) == number
	case float64:
		if number < 0 || number > float64(int(^uint(0)>>1)) || number != float64(int(number)) {
			return 0, false
		}
		return int(number), true
	case json.Number:
		parsed, err := number.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), int64(int(parsed)) == parsed
	default:
		return 0, false
	}
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func stringPointer(value string) *string { return &value }

func choose[T any](condition bool, yes, no T) T {
	if condition {
		return yes
	}
	return no
}
