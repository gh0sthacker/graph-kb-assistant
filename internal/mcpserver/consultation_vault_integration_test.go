package mcpserver

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/consultation"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

// This integration path uses the actual encrypted-vault evidence reader and
// byte comparator, with only the model boundary represented by a test fixture.
func TestMCPConsultationUsesEncryptedEvidenceAndResumesSavedCandidate(t *testing.T) {
	ctx := context.Background()
	vaultKey := make([]byte, 32)
	for index := range vaultKey {
		vaultKey[index] = byte(index + 1)
	}
	t.Setenv("BB_ASSISTANT_VAULT_KEY", base64.RawURLEncoding.EncodeToString(vaultKey))

	root := filepath.Join(t.TempDir(), "runtime")
	ref := project.CaidoProjectRef{InstanceID: "mcp-vault-fixture", ProjectID: "consultation-vault-cycle"}
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
	requestBytes := []byte("GET /items?id=1 HTTP/1.1\r\nHost: app.fixture.invalid\r\nAuthorization: Bearer body-secret\r\n\r\n")
	responseBytes := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"id\":1,\"value\":\"fixture\"}")
	status := 200
	exchanges := []caido.HttpExchange{
		{
			Ref:        caido.ExchangeRef{Project: ref, RequestID: "consultation-left"},
			RequestRaw: requestBytes, ResponseRaw: responseBytes, HasResponse: true,
			Method: "GET", Scheme: "https", Host: "app.fixture.invalid", Port: 443,
			Target: "https://app.fixture.invalid/items?id=1", ResponseStatus: &status,
			RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
		},
		{
			Ref:        caido.ExchangeRef{Project: ref, RequestID: "consultation-right"},
			RequestRaw: append([]byte(nil), requestBytes...), ResponseRaw: append([]byte(nil), responseBytes...), HasResponse: true,
			Method: "GET", Scheme: "https", Host: "app.fixture.invalid", Port: 443,
			Target: "https://app.fixture.invalid/items?id=1", ResponseStatus: &status,
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

	newTools := func() (*Tools, *appquery.Service) {
		t.Helper()
		queries, queryErr := appquery.NewGraphQueryService(root, []project.CaidoProjectRef{ref}, graphaccess.NewService())
		if queryErr != nil {
			t.Fatalf("NewGraphQueryService: %v", queryErr)
		}
		consultations, consultationErr := consultation.NewService(root, []project.CaidoProjectRef{ref}, queries, queries)
		if consultationErr != nil {
			_ = queries.Close(ctx)
			t.Fatalf("NewService: %v", consultationErr)
		}
		return &Tools{
			root: root, projects: []project.CaidoProjectRef{ref},
			projectsByKey: map[string]project.CaidoProjectRef{projectKey: ref},
			consultations: consultations,
		}, queries
	}

	tools, queries := newTools()
	server, err := NewServer(tools)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	initializeFixtureServer(t, server)
	created := callMCPFixtureTool(t, server, 2, "bb_consult_create_session", map[string]any{
		"project_key": projectKey, "question": "Compare the two selected fixture exchanges.",
		"selections": []map[string]any{
			{"request_id": "consultation-left", "revision_id": revisions["consultation-left"], "side": "request", "offset": 0, "length": len(requestBytes)},
			{"request_id": "consultation-right", "revision_id": revisions["consultation-right"], "side": "response", "offset": 0, "length": len(responseBytes)},
		},
		"idempotency_key": "vault_cycle_create_0001",
	})
	sessionID := fixtureString(t, created, "consultation_id")
	if created["question"] != "" {
		t.Fatal("unreviewed MCP create response exposed the question")
	}
	createdEvidence := created["evidence"].(map[string]any)["evidence"].([]any)
	if excerpt, _ := createdEvidence[0].(map[string]any)["excerpt"].(string); excerpt != "" {
		t.Fatal("unreviewed MCP create response exposed evidence text")
	}
	if _, err := tools.AcceptReview(ctx, projectKey, sessionID, 1, "vault_cycle_review_0001"); err != nil {
		t.Fatalf("operator review acceptance: %v", err)
	}
	resumed := callMCPFixtureTool(t, server, 3, "bb_consult_resume", map[string]any{
		"project_key": projectKey, "session_id": sessionID,
	})
	if resumed["review_accepted"] != true || strings.Contains(marshalFixture(t, resumed), "body-secret") {
		t.Fatal("accepted MCP resume was missing review state or exposed a synthetic credential")
	}
	comparison := callMCPFixtureTool(t, server, 4, "bb_consult_compare", map[string]any{
		"project_key": projectKey, "session_id": sessionID, "expected_version": 2,
		"idempotency_key": "vault_cycle_compare_0001",
		"left_request_id": "consultation-left", "left_revision_id": revisions["consultation-left"],
		"right_request_id": "consultation-right", "right_revision_id": revisions["consultation-right"],
	})
	if strings.Contains(marshalFixture(t, comparison), "body-secret") {
		t.Fatal("MCP comparison exposed a raw synthetic credential")
	}
	comparisonID := fixtureString(t, comparison, "comparison_id")
	modelProposal, evidenceIDs, comparisonIDs, err := (fixtureConsultationModel{}).DraftProposal(ctx, resumed, comparison)
	if err != nil {
		t.Fatalf("synthetic model adapter: %v", err)
	}
	if len(evidenceIDs) != 2 || len(comparisonIDs) != 1 || comparisonIDs[0] != comparisonID {
		t.Fatalf("fixture proposal citations mismatch: evidence=%v comparison=%v", evidenceIDs, comparisonIDs)
	}
	proposal := callMCPFixtureTool(t, server, 5, "bb_consult_record_proposal", map[string]any{
		"project_key": projectKey, "session_id": sessionID, "expected_version": 3,
		"idempotency_key": "vault_cycle_proposal_0001", "proposal": modelProposal,
		"evidence_reference_ids": evidenceIDs, "comparison_ids": comparisonIDs,
	})
	proposalID := fixtureString(t, proposal, "proposal_id")
	receipt := callMCPFixtureTool(t, server, 6, "bb_consult_validate_proposal", map[string]any{
		"project_key": projectKey, "session_id": sessionID, "expected_version": 4,
		"idempotency_key": "vault_cycle_validate_0001", "proposal_id": proposalID,
	})
	if fixtureString(t, receipt, "status") != consultation.ValidationReferencesValid {
		t.Fatalf("exact encrypted evidence references failed validation: %#v", receipt)
	}
	candidate := callMCPFixtureTool(t, server, 7, "bb_candidate_save", map[string]any{
		"project_key": projectKey, "session_id": sessionID, "expected_version": 5,
		"idempotency_key": "vault_cycle_candidate_0001", "proposal_id": proposalID,
		"receipt_id": fixtureString(t, receipt, "receipt_id"),
	})
	if fixtureString(t, candidate, "state") != consultation.CandidateStateCandidate {
		t.Fatalf("saved candidate state=%#v, want candidate pending human review", candidate)
	}
	if err := tools.Close(); err != nil {
		t.Fatalf("close MCP tools: %v", err)
	}
	if err := queries.Close(ctx); err != nil {
		t.Fatalf("close appquery service: %v", err)
	}

	tools, queries = newTools()
	defer tools.Close()
	defer queries.Close(ctx)
	restarted, err := NewServer(tools)
	if err != nil {
		t.Fatalf("NewServer after restart: %v", err)
	}
	initializeFixtureServer(t, restarted)
	final := callMCPFixtureTool(t, restarted, 2, "bb_consult_resume", map[string]any{
		"project_key": projectKey, "session_id": sessionID,
	})
	if final["version"] != float64(6) || final["review_accepted"] != true ||
		len(final["comparisons"].([]any)) != 1 || len(final["proposals"].([]any)) != 1 ||
		len(final["validation_receipts"].([]any)) != 1 || len(final["candidates"].([]any)) != 1 {
		t.Fatalf("encrypted-evidence consultation cycle did not resume after restart: %#v", final)
	}
}
