package consultation

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
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

func TestConsultationValidationReopensExactVaultEvidence(t *testing.T) {
	ctx := context.Background()
	vaultKey := make([]byte, 32)
	for index := range vaultKey {
		vaultKey[index] = byte(index + 1)
	}
	t.Setenv("BB_ASSISTANT_VAULT_KEY", base64.RawURLEncoding.EncodeToString(vaultKey))

	root := filepath.Join(t.TempDir(), "runtime")
	ref := project.CaidoProjectRef{InstanceID: "consultation-fixture", ProjectID: "exact-evidence"}
	projectStore, err := store.CreateProject(ctx, ref, root)
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
	requestBytes := []byte("GET /items?id=17 HTTP/1.1\r\nHost: app.fixture.invalid\r\n\r\n")
	responseBytes := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"id\":17}")
	status := 200
	exchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: ref, RequestID: "consultation-request-1"},
		RequestRaw: requestBytes, ResponseRaw: responseBytes, HasResponse: true,
		Method: "GET", Scheme: "https", Host: "app.fixture.invalid", Port: 443,
		Target: "https://app.fixture.invalid/items?id=17", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	if _, err := pageWriter.WritePage(ctx, ingest.HistoryPageInput{RunID: run.RunID, Page: caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}}}); err != nil {
		pageWriter.Close()
		_ = projectStore.Close()
		t.Fatalf("WritePage: %v", err)
	}
	var revisionID, vaultRef string
	if err := projectStore.Database.DB().QueryRowContext(ctx, `
		SELECT cr.revision_id, cr.request_vault_ref
		FROM captures AS c JOIN capture_revisions AS cr ON cr.observation_id=c.observation_id
		WHERE c.request_id=? AND cr.revision_id=c.current_revision_id`, exchange.Ref.RequestID).Scan(&revisionID, &vaultRef); err != nil {
		pageWriter.Close()
		_ = projectStore.Close()
		t.Fatalf("read stored revision: %v", err)
	}
	pageWriter.Close()
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close project: %v", err)
	}

	projectKey, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	queries, err := appquery.NewGraphQueryService(root, []project.CaidoProjectRef{ref}, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	defer queries.Close(ctx)
	service, err := NewService(root, []project.CaidoProjectRef{ref}, queries, queries)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer service.Close()
	request := appquery.EvidencePacketRequest{
		ProjectKey: projectKey, Question: "Compare this exact local request.",
		Selections: []appquery.EvidencePacketSelection{{RequestID: exchange.Ref.RequestID, RevisionID: revisionID, Side: "request", Length: len(requestBytes)}},
	}
	session, err := service.CreateSession(ctx, request, testIdempotencyKey("real-evidence-create"))
	if err != nil {
		t.Fatalf("CreateSession with real resolver: %v", err)
	}
	preview, err := service.ReviewSession(ctx, projectKey, session.ID)
	if err != nil || !preview.Evidence.Evidence[0].Available || preview.Evidence.Evidence[0].SHA256 == "" {
		t.Fatalf("real evidence preview=%#v err=%v", preview.Evidence, err)
	}
	if _, err := service.AcceptReview(ctx, projectKey, session.ID, 1, testIdempotencyKey("real-evidence-review")); err != nil {
		t.Fatalf("AcceptReview: %v", err)
	}
	proposalInput := sampleProposal()
	proposal, err := service.RecordProposal(ctx, RecordProposalRequest{
		ProjectKey: projectKey, SessionID: session.ID, ExpectedVersion: 2, IdempotencyKey: testIdempotencyKey("real-evidence-proposal"),
		Proposal: proposalInput, EvidenceReferenceIDs: []string{session.References[0].ID},
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if _, err := service.ValidateProposal(ctx, ValidateProposalRequest{
		ProjectKey: projectKey, SessionID: session.ID, ExpectedVersion: 3, IdempotencyKey: testIdempotencyKey("real-evidence-validate"), ProposalID: proposal.ID,
	}); err != nil {
		t.Fatalf("validation against exact encrypted source failed: %v", err)
	}

	secondInput := sampleProposal()
	secondInput.Title = "Validation after source removal"
	second, err := service.RecordProposal(ctx, RecordProposalRequest{
		ProjectKey: projectKey, SessionID: session.ID, ExpectedVersion: 4, IdempotencyKey: testIdempotencyKey("removed-source-proposal"),
		Proposal: secondInput, EvidenceReferenceIDs: []string{session.References[0].ID},
	})
	if err != nil {
		t.Fatalf("RecordProposal before source removal: %v", err)
	}
	if err := os.Remove(filepath.Join(projectStore.VaultDirectory, vaultRef)); err != nil {
		t.Fatalf("remove synthetic evidence blob: %v", err)
	}
	if _, err := service.ValidateProposal(ctx, ValidateProposalRequest{
		ProjectKey: projectKey, SessionID: session.ID, ExpectedVersion: 5, IdempotencyKey: testIdempotencyKey("removed-source-validate"), ProposalID: second.ID,
	}); !errors.Is(err, ErrCitationInvalid) {
		t.Fatalf("validation fell back after exact encrypted evidence disappeared: %v", err)
	}
	if strings.Contains(vaultRef, "..") || filepath.IsAbs(vaultRef) {
		t.Fatalf("fixture vault reference escaped project storage: %q", vaultRef)
	}
}
