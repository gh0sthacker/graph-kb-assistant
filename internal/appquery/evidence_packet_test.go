package appquery

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestEvidencePacketRequestValidation(t *testing.T) {
	valid := EvidencePacketRequest{
		ProjectKey: strings.Repeat("a", 64), Question: "Compare these two selected request spans.",
		Selections: []EvidencePacketSelection{{RequestID: "239", RevisionID: strings.Repeat("b", 64), Side: "request", Offset: 8, Length: 64}},
	}
	if err := validateEvidencePacketRequest(valid); err != nil {
		t.Fatalf("valid packet request rejected: %v", err)
	}
	cases := []EvidencePacketRequest{}
	invalidProject := valid
	invalidProject.ProjectKey = "../" + strings.Repeat("a", 64)
	cases = append(cases, invalidProject)
	missingQuestion := valid
	missingQuestion.Question = "  "
	cases = append(cases, missingQuestion)
	missingSelection := valid
	missingSelection.Selections = nil
	cases = append(cases, missingSelection)
	badRevision := valid
	badRevision.Selections = append([]EvidencePacketSelection(nil), valid.Selections...)
	badRevision.Selections[0].RevisionID = strings.Repeat("z", 64)
	cases = append(cases, badRevision)
	badRange := valid
	badRange.Selections = append([]EvidencePacketSelection(nil), valid.Selections...)
	badRange.Selections[0].Length = MaximumEvidencePacketSpanBytes + 1
	cases = append(cases, badRange)
	overBudget := valid
	overBudget.Selections = []EvidencePacketSelection{
		{RequestID: "one", RevisionID: strings.Repeat("b", 64), Side: "request", Length: MaximumEvidencePacketSpanBytes},
		{RequestID: "two", RevisionID: strings.Repeat("c", 64), Side: "response", Length: MaximumEvidencePacketSpanBytes},
		{RequestID: "three", RevisionID: strings.Repeat("d", 64), Side: "request", Length: 1},
	}
	cases = append(cases, overBudget)
	duplicate := valid
	duplicate.Selections = append(append([]EvidencePacketSelection(nil), valid.Selections...), valid.Selections...)
	cases = append(cases, duplicate)
	for index, request := range cases {
		if err := validateEvidencePacketRequest(request); err == nil {
			t.Errorf("invalid packet request %d was accepted", index)
		}
	}
}

func TestBuildEvidencePacketEvidenceRedactsAndPreservesSourceOffsets(t *testing.T) {
	prefix := []byte("GET /items/42 HTTP/1.1\r\nX-Route: safe\r\n")
	selected := []byte("Authorization: Bearer abcdefghijklmnop\r\nX-User: max@example.test\r\nX-Id: 42")
	suffix := []byte("\r\n\r\n")
	fullMessage := append(append(append([]byte(nil), prefix...), selected...), suffix...)
	selection := EvidencePacketSelection{
		RequestID: "239", RevisionID: strings.Repeat("b", 64), Side: "request", Offset: int64(len(prefix)), Length: len(selected),
	}
	truncated := true
	revisionID := selection.RevisionID
	capturedAt := "2026-10-22T06:38:33Z"
	raw := ingest.CaptureRange{
		Data: fullMessage, TotalSize: int64(len(fullMessage)),
		SHA256: strings.Repeat("c", 64), RevisionID: &revisionID, CapturedAt: &capturedAt,
		ParseState: "incomplete", WireFidelity: "source_bytes", RequestTruncated: &truncated,
		IntegrityStatus: ingest.CaptureIntegrityVerified,
	}
	item := buildEvidencePacketEvidence(selection, raw)
	if !item.Available || item.OmittedReason != "" || item.Excerpt == "" {
		t.Fatalf("packet evidence = %#v, want available redacted excerpt", item)
	}
	for _, secret := range []string{"abcdefghijklmnop", "max@example.test"} {
		if strings.Contains(item.Excerpt, secret) {
			t.Errorf("packet excerpt contains a value that should be redacted: %q", secret)
		}
	}
	if !strings.Contains(item.Excerpt, "X-Id: 42") || item.SHA256 != raw.SHA256 || item.SpanStart != selection.Offset || item.SpanEnd != selection.Offset+int64(selection.Length) {
		t.Fatalf("safe excerpt or source metadata was lost: %#v", item)
	}
	if item.MessageSize == nil || *item.MessageSize != raw.TotalSize || item.Truncated == nil || !*item.Truncated || item.ParseState != "incomplete" || item.WireFidelity != "source_bytes" {
		t.Fatalf("capture integrity metadata was lost: %#v", item)
	}
	if item.SelectedBytes != len(selected) || item.RedactedBytes == 0 || item.IncludedBytes >= item.SelectedBytes || item.OmittedBytes == nil || *item.OmittedBytes != raw.TotalSize-int64(item.IncludedBytes) {
		t.Fatalf("packet byte accounting = %#v", item)
	}
	for _, span := range item.Redactions {
		if span.Start < int(selection.Offset) || span.End > int(selection.Offset)+len(selected) || span.End <= span.Start {
			t.Errorf("redaction span is not an absolute source range: %#v", span)
		}
	}
}

func TestBuildEvidencePacketEvidenceRedactsWhenSelectionStartsInsideCredential(t *testing.T) {
	message := []byte("GET /items?access_token=packet-fixture-secret HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n")
	partial := []byte("fixture-secret")
	start := strings.Index(string(message), string(partial))
	if start < 0 {
		t.Fatal("synthetic credential substring was not found")
	}
	revisionID := strings.Repeat("b", 64)
	selection := EvidencePacketSelection{
		RequestID: "239", RevisionID: revisionID, Side: "request", Offset: int64(start), Length: len(partial),
	}
	item := buildEvidencePacketEvidence(selection, ingest.CaptureRange{
		Data: message, TotalSize: int64(len(message)), RevisionID: &revisionID, SHA256: strings.Repeat("c", 64), IntegrityStatus: ingest.CaptureIntegrityVerified,
	})
	if !item.Available || item.Excerpt != "[REDACTED]" || item.RedactedBytes != len(partial) || len(item.Redactions) != 1 || item.SourceIntegrityStatus != string(ingest.CaptureIntegrityVerified) {
		t.Fatalf("partial credential selection was not replaced using full-message context: %#v", item)
	}
}

func TestBuildEvidencePacketEvidenceOmitsMessagesAboveRedactionContextLimit(t *testing.T) {
	revisionID := strings.Repeat("b", 64)
	selection := EvidencePacketSelection{
		RequestID: "239", RevisionID: revisionID, Side: "request", Offset: 0, Length: 32,
	}
	raw := ingest.CaptureRange{
		Data: make([]byte, MaximumEvidenceRangeBytes), TotalSize: MaximumEvidenceRangeBytes + 1,
		RevisionID: &revisionID, IntegrityStatus: ingest.CaptureIntegrityIndexedOnly,
	}
	item := buildEvidencePacketEvidence(selection, raw)
	if item.Available || item.Excerpt != "" || !strings.Contains(item.OmittedReason, "context limit") || item.OmittedBytes == nil || *item.OmittedBytes != raw.TotalSize || item.SourceIntegrityStatus != string(ingest.CaptureIntegrityIndexedOnly) {
		t.Fatalf("oversized message was not omitted from the packet: %#v", item)
	}
}

func TestBuildEvidencePacketEvidenceOmitsInvalidTextAndWrongRevision(t *testing.T) {
	selection := EvidencePacketSelection{RequestID: "239", RevisionID: strings.Repeat("b", 64), Side: "response", Offset: 0, Length: 2}
	revisionID := selection.RevisionID
	raw := ingest.CaptureRange{Data: []byte{0xff, 0xfe}, TotalSize: 2, RevisionID: &revisionID}
	item := buildEvidencePacketEvidence(selection, raw)
	if item.Available || item.Excerpt != "" || item.OmittedReason == "" || item.OmittedBytes == nil || *item.OmittedBytes != 2 || item.SourceIntegrityStatus != string(ingest.CaptureIntegrityUnavailable) {
		t.Fatalf("invalid UTF-8 evidence was not omitted: %#v", item)
	}
	wrongRevision := strings.Repeat("c", 64)
	raw = ingest.CaptureRange{Data: []byte("ok"), TotalSize: 2, RevisionID: &wrongRevision}
	item = buildEvidencePacketEvidence(selection, raw)
	if item.Available || item.Excerpt != "" || item.OmittedReason == "" || item.MessageSize != nil || item.OmittedBytes != nil || item.SourceIntegrityStatus != string(ingest.CaptureIntegrityUnavailable) {
		t.Fatalf("wrong-revision evidence claimed metadata for a different revision: %#v", item)
	}
}

func TestEvidencePacketMarksUnknownOmissionsWhenExactBytesAreUnavailable(t *testing.T) {
	selection := EvidencePacketSelection{
		RequestID: "239", RevisionID: strings.Repeat("b", 64), Side: "request", Offset: 0, Length: 128,
	}
	packet := EvidencePacket{OmittedBytesKnown: true}
	packet.appendEvidence(unavailablePacketEvidence(selection, "Exact local historical bytes are unavailable for the selected span."))
	if packet.OmittedBytesKnown || packet.SelectedBytes != selection.Length || len(packet.Evidence) != 1 {
		t.Fatalf("unavailable span accounting = %#v, want unknown omitted-byte count", packet)
	}
	if packet.Evidence[0].OmittedBytes != nil {
		t.Fatalf("unavailable span claimed an omitted-byte total: %#v", packet.Evidence[0])
	}
}

func TestBuildEvidencePacketReadsExactEncryptedSpansAndFailsClosedOnMissingCapture(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	t.Setenv("BB_ASSISTANT_VAULT_KEY", base64.RawURLEncoding.EncodeToString(key))

	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "packet-fixture", ProjectID: "exact-span-test"}
	projectStore, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	pageWriter, err := ingest.OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("OpenPageWriter: %v", err)
	}
	run, err := pageWriter.BeginRun(ctx)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	requestID := "packet-exact-request"
	requestBytes := []byte("GET /items?access_token=packet-fixture-secret HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n")
	largeRequestID := "packet-oversized-context-request"
	largeRequest := append([]byte("GET /large HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"), bytes.Repeat([]byte("x"), MaximumEvidenceRangeBytes+1)...)
	responseBytes := []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nsynthetic response")
	status := 200
	exchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: ref, RequestID: requestID},
		RequestRaw: requestBytes, ResponseRaw: responseBytes, HasResponse: true,
		Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/items", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	largeExchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: ref, RequestID: largeRequestID},
		RequestRaw: largeRequest, Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/large", RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	if _, err := pageWriter.WritePage(ctx, ingest.HistoryPageInput{RunID: run.RunID, Page: caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange, largeExchange}}}); err != nil {
		pageWriter.Close()
		_ = projectStore.Close()
		t.Fatalf("WritePage: %v", err)
	}
	var revisionID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM captures WHERE request_id=?", requestID,
	).Scan(&revisionID); err != nil {
		pageWriter.Close()
		_ = projectStore.Close()
		t.Fatalf("read revision ID: %v", err)
	}
	var largeRevisionID, largeVaultRef string
	if err := projectStore.Database.DB().QueryRowContext(ctx, `
		SELECT cr.revision_id,cr.request_vault_ref
		FROM captures AS c JOIN capture_revisions AS cr ON cr.observation_id=c.observation_id
		WHERE c.request_id=? AND cr.revision_id=c.current_revision_id`, largeRequestID,
	).Scan(&largeRevisionID, &largeVaultRef); err != nil {
		pageWriter.Close()
		_ = projectStore.Close()
		t.Fatalf("read oversized revision metadata: %v", err)
	}
	pageWriter.Close()
	if _, err := projectStore.Database.DB().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = projectStore.Close()
		t.Fatalf("checkpoint fixture database: %v", err)
	}
	if err := os.Remove(filepath.Join(projectStore.VaultDirectory, largeVaultRef)); err != nil {
		_ = projectStore.Close()
		t.Fatalf("remove oversized vault fixture to prove bounded preflight: %v", err)
	}
	databasePath := filepath.Join(projectStore.Directory, "graph.sqlite3")
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close fixture project: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		path := databasePath + suffix
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || suffix == "-wal" && info.Size() != 0 {
			t.Fatalf("fixture database sidecar %q is not safely removable", suffix)
		}
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove closed fixture sidecar %q: %v", suffix, err)
		}
	}

	projectKey, err := ref.StorageKey()
	if err != nil {
		t.Fatalf("StorageKey: %v", err)
	}
	service, err := NewGraphQueryService(root, []project.CaidoProjectRef{ref}, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	t.Cleanup(func() { _ = service.Close(ctx) })
	packet, err := service.BuildEvidencePacket(ctx, EvidencePacketRequest{
		ProjectKey: projectKey, Question: "Why does access_token=question-fixture-secret appear here?",
		Selections: []EvidencePacketSelection{
			{RequestID: requestID, RevisionID: revisionID, Side: "request", Offset: 0, Length: len(requestBytes)},
			{RequestID: "missing-fixture-request", RevisionID: strings.Repeat("f", 64), Side: "response", Offset: 0, Length: 8},
			{RequestID: largeRequestID, RevisionID: largeRevisionID, Side: "request", Offset: 0, Length: 32},
		},
	})
	if err != nil {
		t.Fatalf("BuildEvidencePacket: %v", err)
	}
	if !packet.PreviewOnly || !packet.OperatorReviewNeeded || packet.OmittedBytesKnown || len(packet.Evidence) != 3 {
		t.Fatalf("packet status = %#v, want local reviewed preview with incomplete omission accounting", packet)
	}
	if strings.Contains(packet.Question, "question-fixture-secret") || strings.Contains(packet.Evidence[0].Excerpt, "packet-fixture-secret") {
		t.Fatal("redacted packet retained a synthetic credential")
	}
	first := packet.Evidence[0]
	if !first.Available || first.RevisionID != revisionID || first.SHA256 == "" || first.Excerpt == "" || first.MessageSize == nil || *first.MessageSize != int64(len(requestBytes)) || first.SourceIntegrityStatus != string(ingest.CaptureIntegrityVerified) {
		t.Fatalf("exact evidence metadata = %#v", first)
	}
	if packet.Evidence[1].Available || packet.Evidence[1].OmittedBytes != nil || packet.Evidence[1].OmittedReason == "" || packet.Evidence[1].SourceIntegrityStatus != string(ingest.CaptureIntegrityUnavailable) {
		t.Fatalf("missing capture was not reported as unknown/unavailable: %#v", packet.Evidence[1])
	}
	oversized := packet.Evidence[2]
	if oversized.Available || oversized.Excerpt != "" || oversized.MessageSize == nil || *oversized.MessageSize != int64(len(largeRequest)) ||
		oversized.OmittedBytes == nil || *oversized.OmittedBytes != int64(len(largeRequest)) || !strings.Contains(oversized.OmittedReason, "context limit") || oversized.SourceIntegrityStatus != string(ingest.CaptureIntegrityIndexedOnly) {
		t.Fatalf("oversized message should be reported with known bounds without reading its removed vault file: %#v", oversized)
	}
}

func TestBuildEvidencePacketFailsClosedForUnregisteredProjects(t *testing.T) {
	service, err := NewGraphQueryService(t.TempDir(), nil, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	defer service.Close(context.Background())
	request := EvidencePacketRequest{
		ProjectKey: strings.Repeat("a", 64), Question: "Compare password=private-value safely.",
		Selections: []EvidencePacketSelection{{RequestID: "239", RevisionID: strings.Repeat("b", 64), Side: "request", Length: 8}},
	}
	_, err = service.BuildEvidencePacket(context.Background(), request)
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorProjectNotRegistered {
		t.Fatalf("unregistered packet project error = %v, want project-not-registered", err)
	}
	_, err = service.BuildEvidencePacket(nil, request)
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorInvalidInput {
		t.Fatalf("nil context error = %v, want invalid-input", err)
	}
}
