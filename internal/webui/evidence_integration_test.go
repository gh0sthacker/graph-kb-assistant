package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

type evidenceServiceAPI struct {
	*explorerAPIFixture
	service *appquery.Service
}

func (api *evidenceServiceAPI) CaptureMessageRange(ctx context.Context, request appquery.CaptureMessageRangeRequest) (appquery.CaptureMessageRange, error) {
	return api.service.CaptureMessageRange(ctx, request)
}

func (api *evidenceServiceAPI) CompareCaptureMessages(ctx context.Context, request appquery.CaptureComparisonRequest) (appquery.CaptureComparison, error) {
	return api.service.CompareCaptureMessages(ctx, request)
}

func (api *evidenceServiceAPI) BuildEvidencePacket(ctx context.Context, request appquery.EvidencePacketRequest) (appquery.EvidencePacket, error) {
	return api.service.BuildEvidencePacket(ctx, request)
}

func TestEvidenceAndComparisonHTTPUseExactEncryptedCaptureRevisions(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	t.Setenv("BB_ASSISTANT_VAULT_KEY", base64.RawURLEncoding.EncodeToString(key))

	dataRoot := filepath.Join(t.TempDir(), "runtime")
	ref := project.CaidoProjectRef{InstanceID: "webui-fixture", ProjectID: "evidence-compare"}
	projectStore, err := store.CreateProject(ctx, ref, dataRoot)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	pageWriter, err := ingest.OpenPageWriter(ctx, projectStore)
	if err != nil {
		_ = projectStore.Close()
		t.Fatalf("OpenPageWriter: %v", err)
	}
	run, err := pageWriter.BeginRun(ctx)
	if err != nil {
		pageWriter.Close()
		_ = projectStore.Close()
		t.Fatalf("BeginRun: %v", err)
	}
	leftRequest := []byte("GET /items?id=one&id=two HTTP/1.1\r\nHost: app.fixture.invalid\r\nAuthorization: Bearer integration-fixture-secret\r\n\r\n")
	leftResponse := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"id\":\"one\",\"value\":\"alpha\"}")
	rightRequest := []byte("GET /items?id=one&id=three HTTP/1.1\r\nHost: app.fixture.invalid\r\n\r\n")
	rightResponse := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"id\":\"one\",\"value\":\"beta\"}")
	leftRequestTruncated := true
	rightResponseTruncated := true
	status := 200
	exchanges := []caido.HttpExchange{
		{
			Ref: caido.ExchangeRef{Project: ref, RequestID: "left-capture"}, RequestRaw: leftRequest, ResponseRaw: leftResponse, HasResponse: true,
			Method: "GET", Scheme: "https", Host: "app.fixture.invalid", Port: 443, Target: "https://app.fixture.invalid/items?id=one&id=two",
			ResponseStatus: &status, RequestTruncated: &leftRequestTruncated,
			RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
		},
		{
			Ref: caido.ExchangeRef{Project: ref, RequestID: "right-capture"}, RequestRaw: rightRequest, ResponseRaw: rightResponse, HasResponse: true,
			Method: "GET", Scheme: "https", Host: "app.fixture.invalid", Port: 443, Target: "https://app.fixture.invalid/items?id=one&id=three",
			ResponseStatus: &status, ResponseTruncated: &rightResponseTruncated,
			RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
		},
	}
	if _, err := pageWriter.WritePage(ctx, ingest.HistoryPageInput{RunID: run.RunID, Page: caido.HistoryPage{Exchanges: exchanges}}); err != nil {
		pageWriter.Close()
		_ = projectStore.Close()
		t.Fatalf("WritePage: %v", err)
	}
	revisions := make(map[string]string, len(exchanges))
	for _, exchange := range exchanges {
		var revision string
		if err := projectStore.Database.DB().QueryRowContext(ctx,
			"SELECT current_revision_id FROM captures WHERE request_id=?", exchange.Ref.RequestID,
		).Scan(&revision); err != nil {
			pageWriter.Close()
			_ = projectStore.Close()
			t.Fatalf("read revision for %s: %v", exchange.Ref.RequestID, err)
		}
		revisions[exchange.Ref.RequestID] = revision
	}
	pageWriter.Close()
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close fixture project: %v", err)
	}

	projectKey, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	queries, err := appquery.NewGraphQueryService(dataRoot, []project.CaidoProjectRef{ref}, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	defer queries.Close(ctx)
	handler, err := NewHandler(&evidenceServiceAPI{explorerAPIFixture: &explorerAPIFixture{}, service: queries})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	rangeQuery := url.Values{
		"project_key": {projectKey}, "request_id": {"left-capture"}, "revision_id": {revisions["left-capture"]},
		"side": {"request"}, "offset": {"0"}, "max_bytes": {"16"},
	}
	evidenceResponse := serveLocalGET(handler, "/api/evidence?"+rangeQuery.Encode())
	if evidenceResponse.Code != http.StatusOK {
		t.Fatalf("evidence status=%d body=%s", evidenceResponse.Code, evidenceResponse.Body.String())
	}
	if evidenceResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive evidence response must not be cached")
	}
	var evidence struct {
		ProjectKey                string `json:"project_key"`
		RequestID                 string `json:"request_id"`
		RevisionID                string `json:"revision_id"`
		Side                      string `json:"side"`
		Offset                    int64  `json:"offset"`
		TotalSize                 int64  `json:"total_size"`
		Data                      string `json:"data"`
		ReturnedSize              int    `json:"returned_size"`
		NextOffset                *int64 `json:"next_offset"`
		RequestTruncated          *bool  `json:"request_truncated"`
		SensitiveValuesMayPresent bool   `json:"sensitive_values_may_be_present"`
	}
	if err := json.Unmarshal(evidenceResponse.Body.Bytes(), &evidence); err != nil {
		t.Fatalf("decode evidence response: %v", err)
	}
	if evidence.ProjectKey != projectKey || evidence.RequestID != "left-capture" || evidence.RevisionID != revisions["left-capture"] ||
		evidence.Side != "request" || evidence.Offset != 0 || evidence.TotalSize != int64(len(leftRequest)) ||
		evidence.Data != string(leftRequest[:16]) || evidence.ReturnedSize != 16 || evidence.NextOffset == nil || *evidence.NextOffset != 16 {
		t.Fatalf("evidence response lost exact revision/range identity: %#v", evidence)
	}
	if evidence.RequestTruncated == nil || !*evidence.RequestTruncated || !evidence.SensitiveValuesMayPresent {
		t.Fatalf("evidence response hid source integrity/sensitivity flags: %#v", evidence)
	}
	rangeQuery.Set("offset", fmt.Sprint(*evidence.NextOffset))
	continuationResponse := serveLocalGET(handler, "/api/evidence?"+rangeQuery.Encode())
	if continuationResponse.Code != http.StatusOK || continuationResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("evidence continuation status=%d cache-control=%q body=%s", continuationResponse.Code, continuationResponse.Header().Get("Cache-Control"), continuationResponse.Body.String())
	}
	var continuation struct {
		ProjectKey       string `json:"project_key"`
		RequestID        string `json:"request_id"`
		RevisionID       string `json:"revision_id"`
		Side             string `json:"side"`
		Offset           int64  `json:"offset"`
		TotalSize        int64  `json:"total_size"`
		Data             string `json:"data"`
		ReturnedSize     int    `json:"returned_size"`
		NextOffset       *int64 `json:"next_offset"`
		RequestTruncated *bool  `json:"request_truncated"`
	}
	if err := json.Unmarshal(continuationResponse.Body.Bytes(), &continuation); err != nil {
		t.Fatalf("decode evidence continuation: %v", err)
	}
	if continuation.ProjectKey != projectKey || continuation.RequestID != "left-capture" || continuation.RevisionID != revisions["left-capture"] ||
		continuation.Side != "request" || continuation.Offset != 16 || continuation.TotalSize != int64(len(leftRequest)) ||
		continuation.Data != string(leftRequest[16:32]) || continuation.ReturnedSize != 16 || continuation.NextOffset == nil || *continuation.NextOffset != 32 ||
		continuation.RequestTruncated == nil || !*continuation.RequestTruncated {
		t.Fatalf("evidence continuation lost its pinned revision or exact byte range: %#v", continuation)
	}

	responseRangeQuery := url.Values{
		"project_key": {projectKey}, "request_id": {"right-capture"}, "revision_id": {revisions["right-capture"]},
		"side": {"response"}, "offset": {"7"}, "max_bytes": {"13"},
	}
	responseRange := serveLocalGET(handler, "/api/evidence?"+responseRangeQuery.Encode())
	if responseRange.Code != http.StatusOK || responseRange.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response evidence status=%d cache-control=%q body=%s", responseRange.Code, responseRange.Header().Get("Cache-Control"), responseRange.Body.String())
	}
	var responseEvidence struct {
		RevisionID                string `json:"revision_id"`
		Side                      string `json:"side"`
		Offset                    int64  `json:"offset"`
		TotalSize                 int64  `json:"total_size"`
		Data                      string `json:"data"`
		ReturnedSize              int    `json:"returned_size"`
		NextOffset                *int64 `json:"next_offset"`
		ResponseTruncated         *bool  `json:"response_truncated"`
		SensitiveValuesMayPresent bool   `json:"sensitive_values_may_be_present"`
	}
	if err := json.Unmarshal(responseRange.Body.Bytes(), &responseEvidence); err != nil {
		t.Fatalf("decode response evidence range: %v", err)
	}
	wantNextOffset := int64(20)
	if responseEvidence.RevisionID != revisions["right-capture"] || responseEvidence.Side != "response" ||
		responseEvidence.Offset != 7 || responseEvidence.TotalSize != int64(len(rightResponse)) ||
		responseEvidence.Data != string(rightResponse[7:20]) || responseEvidence.ReturnedSize != 13 || responseEvidence.NextOffset == nil || *responseEvidence.NextOffset != wantNextOffset {
		t.Fatalf("response evidence range lost exact byte bounds/revision: %#v", responseEvidence)
	}
	if responseEvidence.ResponseTruncated == nil || !*responseEvidence.ResponseTruncated {
		t.Fatalf("response evidence range hid truncation state: %#v", responseEvidence)
	}
	if !responseEvidence.SensitiveValuesMayPresent {
		t.Fatalf("response evidence did not surface its sensitive-content warning: %#v", responseEvidence)
	}

	packetRequest := appquery.EvidencePacketRequest{
		ProjectKey: projectKey, Question: "Compare the selected item responses.",
		Selections: []appquery.EvidencePacketSelection{{
			RequestID: "left-capture", RevisionID: revisions["left-capture"], Side: "request", Length: len(leftRequest),
		}},
	}
	packetBody, err := json.Marshal(packetRequest)
	if err != nil {
		t.Fatalf("encode packet preview request: %v", err)
	}
	packetHTTP := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/evidence/packet-preview", bytes.NewReader(packetBody))
	packetHTTP.Header.Set("Content-Type", "application/json")
	packetHTTP.Header.Set("Origin", "http://127.0.0.1")
	packetResponse := httptest.NewRecorder()
	handler.ServeHTTP(packetResponse, packetHTTP)
	if packetResponse.Code != http.StatusOK || packetResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("packet preview status=%d cache=%q body=%s", packetResponse.Code, packetResponse.Header().Get("Cache-Control"), packetResponse.Body.String())
	}
	var packet appquery.EvidencePacket
	if err := json.Unmarshal(packetResponse.Body.Bytes(), &packet); err != nil {
		t.Fatalf("decode packet preview: %v", err)
	}
	if !packet.PreviewOnly || !packet.OperatorReviewNeeded || len(packet.Evidence) != 1 || !packet.Evidence[0].Available ||
		packet.Evidence[0].RedactedBytes == 0 || !strings.Contains(packet.Evidence[0].Excerpt, "[REDACTED]") ||
		strings.Contains(packet.Evidence[0].Excerpt, "integration-fixture-secret") {
		t.Fatalf("packet preview did not preserve the review gate and redact the synthetic credential: %#v", packet)
	}

	compareQuery := url.Values{
		"project_key": {projectKey}, "left_request_id": {"left-capture"}, "left_revision_id": {revisions["left-capture"]},
		"right_request_id": {"right-capture"}, "right_revision_id": {revisions["right-capture"]},
	}
	comparisonResponse := serveLocalGET(handler, "/api/compare?"+compareQuery.Encode())
	if comparisonResponse.Code != http.StatusOK {
		t.Fatalf("comparison status=%d body=%s", comparisonResponse.Code, comparisonResponse.Body.String())
	}
	if comparisonResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive comparison response must not be cached")
	}
	var comparison appquery.CaptureComparison
	if err := json.Unmarshal(comparisonResponse.Body.Bytes(), &comparison); err != nil {
		t.Fatalf("decode comparison response: %v", err)
	}
	if comparison.ProjectKey != projectKey || comparison.LeftRequestID != "left-capture" || comparison.LeftRevisionID != revisions["left-capture"] ||
		comparison.RightRequestID != "right-capture" || comparison.RightRevisionID != revisions["right-capture"] {
		t.Fatalf("comparison response lost selected revision identities: %#v", comparison)
	}
	if !comparison.Request.Available || comparison.Request.Equal || comparison.Request.FirstDifferenceOffset == nil ||
		!strings.Contains(comparison.Request.LeftExcerpt, "id=two") || !strings.Contains(comparison.Request.RightExcerpt, "id=three") {
		t.Fatalf("request comparison did not report deterministic difference: %#v", comparison.Request)
	}
	if comparison.Request.Left.RevisionID != revisions["left-capture"] || comparison.Request.Right.RevisionID != revisions["right-capture"] ||
		comparison.Request.Left.Truncated == nil || !*comparison.Request.Left.Truncated {
		t.Fatalf("request comparison lost exact source revision/truncation state: %#v", comparison.Request)
	}
	if !comparison.Response.Available || comparison.Response.Equal || comparison.Response.FirstDifferenceOffset == nil ||
		!strings.Contains(comparison.Response.LeftExcerpt, "alpha") || !strings.Contains(comparison.Response.RightExcerpt, "beta") {
		t.Fatalf("response comparison did not report deterministic difference: %#v", comparison.Response)
	}
	if comparison.Response.Left.RevisionID != revisions["left-capture"] || comparison.Response.Right.RevisionID != revisions["right-capture"] ||
		comparison.Response.Right.Truncated == nil || !*comparison.Response.Right.Truncated {
		t.Fatalf("response comparison lost exact source revision/truncation state: %#v", comparison.Response)
	}
}

func serveLocalGET(handler http.Handler, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
