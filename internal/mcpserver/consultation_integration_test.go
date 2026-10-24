package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/consultation"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/publicdata"
	"bb-graph-assistant/internal/store"
)

type mcpCycleEvidence struct{}

func (mcpCycleEvidence) BuildEvidencePacket(_ context.Context, request appquery.EvidencePacketRequest) (appquery.EvidencePacket, error) {
	question, questionRedactions, err := publicdata.RedactText(request.Question)
	if err != nil {
		return appquery.EvidencePacket{}, err
	}
	packet := appquery.EvidencePacket{
		Version: "1", PolicyVersion: appquery.EvidencePacketPolicyVersion,
		ProjectKey: request.ProjectKey, Question: question, QuestionRedactions: questionRedactions,
		PreviewOnly: true, OperatorReviewNeeded: true, OmittedBytesKnown: true,
		CoverageNote: "Synthetic evidence only; no model call.",
		Evidence:     make([]appquery.EvidencePacketEvidence, 0, len(request.Selections)),
	}
	for _, selected := range request.Selections {
		packet.Evidence = append(packet.Evidence, appquery.EvidencePacketEvidence{
			Available: true, RequestID: selected.RequestID, RevisionID: selected.RevisionID,
			Side: selected.Side, SpanStart: selected.Offset,
			SpanEnd: selected.Offset + int64(selected.Length), SelectedBytes: selected.Length,
			IncludedBytes: selected.Length, SHA256: strings.Repeat("a", 64),
			SourceIntegrityStatus: "verified", ParseState: "complete", WireFidelity: "exact",
			Excerpt: "fixture response excerpt", Redactions: []publicdata.TextRedactionSpan{},
		})
		packet.SelectedBytes += selected.Length
	}
	return packet, nil
}

type mcpCycleComparison struct{}

func (mcpCycleComparison) CompareCaptureMessages(_ context.Context, request appquery.CaptureComparisonRequest) (appquery.CaptureComparison, error) {
	left := appquery.CaptureRevisionSummary{
		RequestID: request.LeftRequestID, RevisionID: request.LeftRevisionID,
		Size: 16, SHA256: strings.Repeat("b", 64), ParseState: "complete", WireFidelity: "exact",
	}
	right := appquery.CaptureRevisionSummary{
		RequestID: request.RightRequestID, RevisionID: request.RightRevisionID,
		Size: 16, SHA256: strings.Repeat("b", 64), ParseState: "complete", WireFidelity: "exact",
	}
	return appquery.CaptureComparison{
		ProjectKey: request.ProjectKey, LeftRequestID: request.LeftRequestID, LeftRevisionID: request.LeftRevisionID,
		RightRequestID: request.RightRequestID, RightRevisionID: request.RightRevisionID,
		Request: appquery.CaptureSideComparison{
			Available: true, Equal: true, ComparedBytes: 16, Left: left, Right: right,
			LeftExcerpt: "Authorization: Bearer body-secret", RightExcerpt: "cookie=body-secret",
		},
		Response:     appquery.CaptureSideComparison{Available: true, Equal: true, ComparedBytes: 16, Left: left, Right: right},
		CoverageNote: "Synthetic comparison only.",
	}, nil
}

func TestMCPConsultationCycleSurvivesTransportAndRestart(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "runtime")
	ref := project.CaidoProjectRef{InstanceID: "mcp-cycle-fixture", ProjectID: "consultation-cycle"}
	projectStore, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close fixture project: %v", err)
	}
	projectKey, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	newFixtureTools := func() *Tools {
		t.Helper()
		service, serviceErr := consultation.NewService(root, []project.CaidoProjectRef{ref}, mcpCycleEvidence{}, mcpCycleComparison{})
		if serviceErr != nil {
			t.Fatalf("NewService: %v", serviceErr)
		}
		return &Tools{
			root: root, projects: []project.CaidoProjectRef{ref},
			projectsByKey: map[string]project.CaidoProjectRef{projectKey: ref},
			consultations: service,
		}
	}
	tools := newFixtureTools()
	server, err := NewServer(tools)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	initializeFixtureServer(t, server)

	leftRevision := strings.Repeat("c", 64)
	rightRevision := strings.Repeat("d", 64)
	created := callMCPFixtureTool(t, server, 2, "bb_consult_create_session", map[string]any{
		"project_key": projectKey, "question": "Compare these two synthetic responses.",
		"selections": []map[string]any{
			{"request_id": "request-a", "revision_id": leftRevision, "side": "response", "offset": 0, "length": 8},
			{"request_id": "request-b", "revision_id": rightRevision, "side": "response", "offset": 0, "length": 8},
		},
		"idempotency_key": "mcp_create_session_0001",
	})
	sessionID := fixtureString(t, created, "consultation_id")
	references := created["evidence_references"].([]any)
	if len(references) != 2 {
		t.Fatalf("created session evidence references = %d; want 2", len(references))
	}
	firstReference := references[0].(map[string]any)
	secondReference := references[1].(map[string]any)
	leftReferenceID := fixtureString(t, firstReference, "evidence_reference_id")
	rightReferenceID := fixtureString(t, secondReference, "evidence_reference_id")
	if created["question"] != "" {
		t.Fatalf("MCP create response exposed an unreviewed question: %#v", created["question"])
	}
	createdEvidence := created["evidence"].(map[string]any)["evidence"].([]any)
	if excerpt, _ := createdEvidence[0].(map[string]any)["excerpt"].(string); excerpt != "" {
		t.Fatal("MCP create response exposed an unreviewed evidence excerpt")
	}
	preReview := callMCPFixtureTool(t, server, 3, "bb_consult_resume", map[string]any{
		"project_key": projectKey, "session_id": sessionID,
	})
	preReviewEvidence := preReview["evidence"].(map[string]any)["evidence"].([]any)
	if preReview["question"] != "" {
		t.Fatal("MCP resume exposed an unreviewed question")
	}
	if excerpt, _ := preReviewEvidence[0].(map[string]any)["excerpt"].(string); excerpt != "" {
		t.Fatal("MCP resume exposed an unreviewed evidence excerpt")
	}

	if _, err := tools.AcceptReview(ctx, projectKey, sessionID, 1, "mcp_operator_accept_0001"); err != nil {
		t.Fatalf("operator review acceptance: %v", err)
	}
	resumed := callMCPFixtureTool(t, server, 4, "bb_consult_resume", map[string]any{
		"project_key": projectKey, "session_id": sessionID,
	})
	if resumed["review_accepted"] != true || resumed["question"] == "" {
		t.Fatalf("accepted session was not made available to Hermes: %#v", resumed)
	}

	comparison := callMCPFixtureTool(t, server, 5, "bb_consult_compare", map[string]any{
		"project_key": projectKey, "session_id": sessionID, "expected_version": 2,
		"idempotency_key": "mcp_compare_captures_0001",
		"left_request_id": "request-a", "left_revision_id": leftRevision,
		"right_request_id": "request-b", "right_revision_id": rightRevision,
	})
	if strings.Contains(marshalFixture(t, comparison), "body-secret") {
		t.Fatal("MCP comparison exposed a raw fixture excerpt")
	}
	comparisonID := fixtureString(t, comparison, "comparison_id")
	proposal := callMCPFixtureTool(t, server, 6, "bb_consult_record_proposal", map[string]any{
		"project_key": projectKey, "session_id": sessionID, "expected_version": 3,
		"idempotency_key": "mcp_record_proposal_0001",
		"proposal": map[string]any{
			"title": "Synthetic response difference", "summary": "The selected fixture responses compare equal.",
			"rationale":    "The result is limited to exact local fixture evidence.",
			"impact":       "No target impact is established.",
			"alternatives": []string{"The behavior may be expected."},
			"next_checks":  []string{"Review another synthetic actor pair."},
			"limitations":  []string{"Synthetic fixtures do not establish a vulnerability."},
		},
		"evidence_reference_ids": []string{leftReferenceID, rightReferenceID},
		"comparison_ids":         []string{comparisonID},
	})
	proposalID := fixtureString(t, proposal, "proposal_id")
	receipt := callMCPFixtureTool(t, server, 7, "bb_consult_validate_proposal", map[string]any{
		"project_key": projectKey, "session_id": sessionID, "expected_version": 4,
		"idempotency_key": "mcp_validate_proposal_0001", "proposal_id": proposalID,
	})
	if fixtureString(t, receipt, "status") != consultation.ValidationReferencesValid {
		t.Fatalf("proposal receipt did not validate exact references: %#v", receipt)
	}
	candidate := callMCPFixtureTool(t, server, 8, "bb_candidate_save", map[string]any{
		"project_key": projectKey, "session_id": sessionID, "expected_version": 5,
		"idempotency_key": "mcp_save_candidate_0001", "proposal_id": proposalID,
		"receipt_id": fixtureString(t, receipt, "receipt_id"),
	})
	if fixtureString(t, candidate, "state") != consultation.CandidateStateCandidate {
		t.Fatalf("saved candidate was promoted beyond candidate state: %#v", candidate)
	}
	if err := tools.Close(); err != nil {
		t.Fatalf("close first MCP tools: %v", err)
	}

	tools = newFixtureTools()
	defer tools.Close()
	restarted, err := NewServer(tools)
	if err != nil {
		t.Fatalf("NewServer after restart: %v", err)
	}
	initializeFixtureServer(t, restarted)
	final := callMCPFixtureTool(t, restarted, 2, "bb_consult_resume", map[string]any{
		"project_key": projectKey, "session_id": sessionID,
	})
	if final["version"] != float64(6) || final["review_accepted"] != true || len(final["comparisons"].([]any)) != 1 ||
		len(final["proposals"].([]any)) != 1 || len(final["validation_receipts"].([]any)) != 1 || len(final["candidates"].([]any)) != 1 {
		t.Fatalf("MCP session did not resume its completed synthetic cycle after restart: %#v", final)
	}
}

func initializeFixtureServer(t *testing.T, server *Server) {
	t.Helper()
	serveFixtureRequests(t, server,
		map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{
				"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
				"clientInfo": map[string]any{"name": "synthetic-hermes", "version": "fixture"},
			},
		},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}},
	)
}

func callMCPFixtureTool(t *testing.T, server *Server, id int, name string, args map[string]any) map[string]any {
	t.Helper()
	responses := serveFixtureRequests(t, server, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	result, ok := responseForID(t, responses, fmt.Sprint(id)).Result.(map[string]any)
	if !ok || result["isError"] == true {
		t.Fatalf("MCP tool %q failed: %#v", name, result)
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("MCP tool %q omitted structured content: %#v", name, result)
	}
	return structured
}

func fixtureString(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok || value == "" {
		t.Fatalf("fixture result field %q is missing: %#v", key, object)
	}
	return value
}

func marshalFixture(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture result: %v", err)
	}
	return string(encoded)
}
