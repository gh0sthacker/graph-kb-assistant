package consultation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/publicdata"
	projectstore "bb-graph-assistant/internal/store"
)

type evidenceBuilderFake struct {
	packet appquery.EvidencePacket
	err    error
	calls  int
	got    appquery.EvidencePacketRequest
}

func (f *evidenceBuilderFake) BuildEvidencePacket(_ context.Context, request appquery.EvidencePacketRequest) (appquery.EvidencePacket, error) {
	f.calls++
	f.got = request
	if f.err != nil {
		return appquery.EvidencePacket{}, f.err
	}
	packet := f.packet
	packet.ProjectKey = request.ProjectKey
	packet.Question, _, _ = publicdata.RedactText(request.Question)
	packet.Evidence = make([]appquery.EvidencePacketEvidence, 0, len(request.Selections))
	for _, selection := range request.Selections {
		for _, item := range f.packet.Evidence {
			if item.RequestID == selection.RequestID && item.RevisionID == selection.RevisionID && item.Side == selection.Side && item.SpanStart == selection.Offset && item.SpanEnd == selection.Offset+int64(selection.Length) {
				packet.Evidence = append(packet.Evidence, item)
				break
			}
		}
	}
	packet.SelectedBytes = 0
	for _, item := range packet.Evidence {
		packet.SelectedBytes += item.SelectedBytes
	}
	return packet, nil
}

type comparisonBuilderFake struct {
	result appquery.CaptureComparison
	err    error
	calls  int
	got    appquery.CaptureComparisonRequest
}

func (f *comparisonBuilderFake) CompareCaptureMessages(_ context.Context, request appquery.CaptureComparisonRequest) (appquery.CaptureComparison, error) {
	f.calls++
	f.got = request
	if f.err != nil {
		return appquery.CaptureComparison{}, f.err
	}
	result := f.result
	result.ProjectKey = request.ProjectKey
	result.LeftRequestID = request.LeftRequestID
	result.LeftRevisionID = request.LeftRevisionID
	result.RightRequestID = request.RightRequestID
	result.RightRevisionID = request.RightRevisionID
	fillSummary := func(summary *appquery.CaptureRevisionSummary, requestID, revisionID, hash string) {
		if summary.RequestID == "" && summary.RevisionID == "" {
			*summary = appquery.CaptureRevisionSummary{RequestID: requestID, RevisionID: revisionID, Size: 16, SHA256: hash, ParseState: "complete", WireFidelity: "exact"}
		}
	}
	fillSummary(&result.Request.Left, request.LeftRequestID, request.LeftRevisionID, strings.Repeat("1", 64))
	fillSummary(&result.Request.Right, request.RightRequestID, request.RightRevisionID, strings.Repeat("1", 64))
	fillSummary(&result.Response.Left, request.LeftRequestID, request.LeftRevisionID, strings.Repeat("2", 64))
	fillSummary(&result.Response.Right, request.RightRequestID, request.RightRevisionID, strings.Repeat("3", 64))
	return result, nil
}

func TestConsultationCyclePersistsReferencesAndResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	root, ref, projectKey := createConsultationProject(t, "project-a")
	evidence := &evidenceBuilderFake{packet: samplePacket(projectKey)}
	comparison := &comparisonBuilderFake{result: sampleComparison()}
	service := newTestService(t, root, ref, evidence, comparison)
	createRequest := createRequestFor(projectKey, 2)
	created, err := service.CreateSession(ctx, createRequest, testIdempotencyKey("create-session"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if created.ID == "" || created.ProjectKey != projectKey || created.State != SessionOpen || created.Version != 1 || len(created.References) != 2 {
		t.Fatalf("created session = %#v", created)
	}
	if created.ReviewAccepted || created.Question != "" || created.Evidence.Question != "" || created.Evidence.Evidence[0].Excerpt != "" {
		t.Fatalf("unreviewed create response exposed content: %#v", created)
	}
	operatorPreview, err := service.ReviewSession(ctx, projectKey, created.ID)
	if err != nil || operatorPreview.Question == "" || operatorPreview.Evidence.Evidence[0].Excerpt == "" {
		t.Fatalf("operator preview=%#v err=%v", operatorPreview, err)
	}
	if _, err := service.Compare(ctx, projectKey, created.ID, 1, testIdempotencyKey("premature-compare"), sampleComparisonRequest(projectKey)); !errors.Is(err, ErrReviewNotAccepted) {
		t.Fatalf("pre-review assistant action error=%v", err)
	}
	accepted, err := service.AcceptReview(ctx, projectKey, created.ID, 1, testIdempotencyKey("accept-review"))
	if err != nil || !accepted.ReviewAccepted || accepted.Version != 2 || accepted.ReviewAcceptedVersion != 2 {
		t.Fatalf("accepted review=%#v err=%v", accepted, err)
	}
	assistantSession, err := service.Resume(ctx, projectKey, created.ID)
	if err != nil || !assistantSession.ReviewAccepted || assistantSession.Question == "" || assistantSession.Evidence.Evidence[0].Excerpt == "" {
		t.Fatalf("post-review assistant session=%#v err=%v", assistantSession, err)
	}
	if created.References[0].ID == created.References[0].SHA256 || created.References[0].ID == created.References[0].RevisionID {
		t.Fatal("evidence reference is not an opaque server-issued identifier")
	}
	replayed, err := service.CreateSession(ctx, createRequest, testIdempotencyKey("create-session"))
	if err != nil || replayed.ID != created.ID || evidence.calls != 1 {
		t.Fatalf("idempotent create = %#v calls=%d err=%v", replayed, evidence.calls, err)
	}
	changed := createRequest
	changed.Question = "A different question"
	if _, err := service.CreateSession(ctx, changed, testIdempotencyKey("create-session")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed retry error = %v, want idempotency conflict", err)
	}

	comparisonRecord, err := service.Compare(ctx, projectKey, created.ID, 2, testIdempotencyKey("compare-captures"), sampleComparisonRequest(projectKey))
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if comparisonRecord.Result.Request.LeftExcerpt != "" || comparisonRecord.Result.Request.RightExcerpt != "" || comparisonRecord.Result.Response.LeftExcerpt != "" {
		t.Fatalf("raw comparison excerpt was retained: %#v", comparisonRecord.Result)
	}
	proposal, err := service.RecordProposal(ctx, RecordProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 3, IdempotencyKey: testIdempotencyKey("record-proposal"),
		Proposal: sampleProposal(), EvidenceReferenceIDs: []string{created.References[0].ID, created.References[1].ID}, ComparisonIDs: []string{comparisonRecord.ID},
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if proposal.Digest == "" || len(proposal.EvidenceReferenceIDs) != 2 {
		t.Fatalf("proposal = %#v", proposal)
	}
	receipt, err := service.ValidateProposal(ctx, ValidateProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 4, IdempotencyKey: testIdempotencyKey("validate-proposal"), ProposalID: proposal.ID,
	})
	if err != nil {
		t.Fatalf("ValidateProposal: %v", err)
	}
	if receipt.Status != ValidationReferencesValid || receipt.ProposalDigest != proposal.Digest || len(receipt.EvidenceReferenceIDs) != 2 || evidence.calls != 2 || comparison.calls != 2 {
		t.Fatalf("validation receipt=%#v evidence calls=%d compare calls=%d", receipt, evidence.calls, comparison.calls)
	}
	candidateRequest := SaveCandidateRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 5, IdempotencyKey: testIdempotencyKey("save-candidate"),
		ProposalID: proposal.ID, ReceiptID: receipt.ID,
	}
	candidate, err := service.SaveCandidate(ctx, candidateRequest)
	if err != nil {
		t.Fatalf("SaveCandidate: %v", err)
	}
	if candidate.CandidateID == "" || candidate.Version != 1 || candidate.State != CandidateStateCandidate || candidate.ProposalID != proposal.ID || len(candidate.Citations) != 2 {
		t.Fatalf("candidate = %#v", candidate)
	}
	replayedCandidate, err := service.SaveCandidate(ctx, candidateRequest)
	if err != nil || replayedCandidate.Version != candidate.Version {
		t.Fatalf("idempotent candidate save=%#v err=%v", replayedCandidate, err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restarted, err := NewService(root, []project.CaidoProjectRef{ref}, evidence, comparison)
	if err != nil {
		t.Fatalf("NewService after restart: %v", err)
	}
	defer restarted.Close()
	resumed, err := restarted.Resume(ctx, projectKey, created.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Version != 6 || !resumed.ReviewAccepted || len(resumed.Comparisons) != 1 || len(resumed.Proposals) != 1 || len(resumed.Receipts) != 1 || len(resumed.Candidates) != 1 {
		t.Fatalf("resumed state is incomplete: %#v", resumed)
	}
	encoded, err := json.Marshal(resumed)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"very-secret-token", "session-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("resume returned raw comparison excerpt %q", secret)
		}
	}
}

func TestPendingSessionHidesAnalysisAndRejectsExistingMutationReplays(t *testing.T) {
	ctx := context.Background()
	root, ref, projectKey := createConsultationProject(t, "project-review-gate")
	service := newTestService(t, root, ref, &evidenceBuilderFake{packet: samplePacket(projectKey)}, &comparisonBuilderFake{result: sampleComparison()})
	created, err := service.CreateSession(ctx, createRequestFor(projectKey, 1), testIdempotencyKey("review-gate-create"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.ReviewSession(ctx, projectKey, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy := stored
	legacy.Comparisons = []ComparisonRecord{{ID: testOpaqueID('c')}}
	legacy.Proposals = []ProposalRecord{{ID: testOpaqueID('p'), Proposal: sampleProposal()}}
	legacy.Receipts = []ValidationReceipt{{ID: testOpaqueID('r')}}
	legacy.Candidates = []CandidateVersion{{CandidateID: created.ID, Version: 1, ProposalID: testOpaqueID('p'), ReceiptID: testOpaqueID('r')}}
	visible := assistantVisible(legacy)
	if visible.Question != "" || visible.Evidence.Question != "" || visible.Evidence.Evidence[0].Excerpt != "" || visible.Evidence.Evidence[0].SHA256 != "" ||
		len(visible.Comparisons) != 0 || len(visible.Proposals) != 0 || len(visible.Receipts) != 0 || len(visible.Candidates) != 0 {
		t.Fatalf("pending session exposed legacy analysis: %#v", visible)
	}

	comparisonRequest := sampleComparisonRequest(projectKey)
	comparisonHash, err := digestJSON(struct {
		ProjectKey string                            `json:"project_key"`
		SessionID  string                            `json:"session_id"`
		Version    int                               `json:"expected_version"`
		Request    appquery.CaptureComparisonRequest `json:"request"`
	}{projectKey, created.ID, 1, comparisonRequest})
	if err != nil {
		t.Fatal(err)
	}
	proposalRequest := RecordProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 1, IdempotencyKey: testIdempotencyKey("review-gate-proposal"),
		Proposal: sampleProposal(), EvidenceReferenceIDs: []string{created.References[0].ID},
	}
	proposalRequest.Proposal, err = sanitizeProposal(proposalRequest.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	proposalHash, err := digestJSON(proposalRequest)
	if err != nil {
		t.Fatal(err)
	}
	validationRequest := ValidateProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 1,
		IdempotencyKey: testIdempotencyKey("review-gate-validation"), ProposalID: testOpaqueID('p'),
	}
	validationHash, err := digestJSON(validationRequest)
	if err != nil {
		t.Fatal(err)
	}
	candidateRequest := SaveCandidateRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 1,
		IdempotencyKey: testIdempotencyKey("review-gate-candidate"), ProposalID: testOpaqueID('p'), ReceiptID: testOpaqueID('r'),
	}
	candidateHash, err := digestJSON(candidateRequest)
	if err != nil {
		t.Fatal(err)
	}
	db, err := service.database(projectKey, false)
	if err != nil {
		t.Fatal(err)
	}
	storeReplay := func(operation, key, requestHash string, result any) {
		t.Helper()
		encoded, err := encodeSafe(result)
		if err != nil {
			t.Fatalf("encode replay for %s: %v", operation, err)
		}
		tx, err := db.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin replay fixture for %s: %v", operation, err)
		}
		if err := insertMutation(ctx, tx, projectKey, created.ID, operation, key, requestHash, encoded, time.Now().UTC()); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed replay for %s: %v", operation, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit replay fixture for %s: %v", operation, err)
		}
	}
	storeReplay("compare", testIdempotencyKey("review-gate-compare"), comparisonHash, ComparisonRecord{ID: testOpaqueID('c')})
	storeReplay("record_proposal", proposalRequest.IdempotencyKey, proposalHash, ProposalRecord{ID: testOpaqueID('p')})
	storeReplay("validate_proposal", validationRequest.IdempotencyKey, validationHash, ValidationReceipt{ID: testOpaqueID('r')})
	storeReplay("save_candidate", candidateRequest.IdempotencyKey, candidateHash, CandidateVersion{ProposalID: candidateRequest.ProposalID, ReceiptID: candidateRequest.ReceiptID})

	if _, err := service.Compare(ctx, projectKey, created.ID, 1, testIdempotencyKey("review-gate-compare"), comparisonRequest); !errors.Is(err, ErrReviewNotAccepted) {
		t.Errorf("Compare replay before review error=%v", err)
	}
	if _, err := service.RecordProposal(ctx, proposalRequest); !errors.Is(err, ErrReviewNotAccepted) {
		t.Errorf("RecordProposal replay before review error=%v", err)
	}
	if _, err := service.ValidateProposal(ctx, validationRequest); !errors.Is(err, ErrReviewNotAccepted) {
		t.Errorf("ValidateProposal replay before review error=%v", err)
	}
	if _, err := service.SaveCandidate(ctx, candidateRequest); !errors.Is(err, ErrReviewNotAccepted) {
		t.Errorf("SaveCandidate replay before review error=%v", err)
	}
}

func TestCanonicalCandidateReplayRestoresLegacyCandidateID(t *testing.T) {
	canonical := CandidateVersion{
		CandidateID: testOpaqueID('c'), Version: 1, SessionVersion: 5, State: CandidateStateCandidate,
		ProposalID: testOpaqueID('p'), ReceiptID: testOpaqueID('r'),
	}
	replay := canonical
	replay.CandidateID = ""
	recovered, err := canonicalCandidateReplay(Session{Candidates: []CandidateVersion{canonical}}, replay)
	if err != nil || recovered.CandidateID != canonical.CandidateID || recovered.Version != canonical.Version {
		t.Fatalf("legacy replay canonicalized to %#v, err=%v", recovered, err)
	}
	replay.ReceiptID = testOpaqueID('x')
	if _, err := canonicalCandidateReplay(Session{Candidates: []CandidateVersion{canonical}}, replay); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("unmatched legacy replay error=%v", err)
	}
}

func TestV1MigrationHidesLegacyAnalysisAndCanonicalizesCandidateReplay(t *testing.T) {
	ctx := context.Background()
	root, ref, projectKey := createConsultationProject(t, "project-v1-migration")
	evidence := &evidenceBuilderFake{packet: samplePacket(projectKey)}
	comparison := &comparisonBuilderFake{result: sampleComparison()}
	service := newTestService(t, root, ref, evidence, comparison)
	created, err := service.CreateSession(ctx, createRequestFor(projectKey, 2), testIdempotencyKey("v1-migration-create"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.ReviewSession(ctx, projectKey, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := service.database(projectKey, false)
	if err != nil {
		t.Fatal(err)
	}

	comparisonRequest := sampleComparisonRequest(projectKey)
	comparisonResult, err := comparison.CompareCaptureMessages(ctx, comparisonRequest)
	if err != nil {
		t.Fatal(err)
	}
	comparisonResult, err = safeComparison(comparisonResult, comparisonRequest)
	if err != nil {
		t.Fatal(err)
	}
	comparisonID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	comparisonHash, err := digestJSON(struct {
		ProjectKey string                            `json:"project_key"`
		SessionID  string                            `json:"session_id"`
		Version    int                               `json:"expected_version"`
		Request    appquery.CaptureComparisonRequest `json:"request"`
	}{projectKey, created.ID, 1, comparisonRequest})
	if err != nil {
		t.Fatal(err)
	}
	comparisonKey := testIdempotencyKey("v1-migration-compare")
	comparisonRecord := ComparisonRecord{ID: comparisonID, SessionVersion: 2, Result: comparisonResult, CreatedAt: time.Now().UTC()}
	if _, err := db.appendComparison(ctx, projectKey, created.ID, 1, comparisonKey, comparisonHash, comparisonRecord); err != nil {
		t.Fatalf("append legacy comparison: %v", err)
	}

	proposalInput, err := sanitizeProposal(sampleProposal())
	if err != nil {
		t.Fatal(err)
	}
	proposalID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	proposalRequest := RecordProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 2,
		IdempotencyKey: testIdempotencyKey("v1-migration-proposal"), Proposal: proposalInput,
		EvidenceReferenceIDs: []string{stored.References[0].ID, stored.References[1].ID}, ComparisonIDs: []string{comparisonID},
	}
	proposalHash, err := digestJSON(proposalRequest)
	if err != nil {
		t.Fatal(err)
	}
	proposalDigest, err := digestJSON(struct {
		Proposal    CandidateProposal `json:"proposal"`
		EvidenceIDs []string          `json:"evidence_reference_ids"`
		CompareIDs  []string          `json:"comparison_ids"`
		Policy      string            `json:"policy_version"`
	}{proposalInput, proposalRequest.EvidenceReferenceIDs, proposalRequest.ComparisonIDs, stored.Evidence.PolicyVersion})
	if err != nil {
		t.Fatal(err)
	}
	proposal := ProposalRecord{
		ID: proposalID, SessionVersion: 3, Proposal: proposalInput,
		EvidenceReferenceIDs: proposalRequest.EvidenceReferenceIDs, ComparisonIDs: proposalRequest.ComparisonIDs,
		Digest: proposalDigest, CreatedAt: time.Now().UTC(),
	}
	if _, err := db.appendProposal(ctx, projectKey, created.ID, 2, proposalRequest.IdempotencyKey, proposalHash, proposal); err != nil {
		t.Fatalf("append legacy proposal: %v", err)
	}

	receiptID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	validationRequest := ValidateProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 3,
		IdempotencyKey: testIdempotencyKey("v1-migration-validation"), ProposalID: proposal.ID,
	}
	validationHash, err := digestJSON(validationRequest)
	if err != nil {
		t.Fatal(err)
	}
	receipt := ValidationReceipt{
		ID: receiptID, SessionVersion: 4, ProposalID: proposal.ID, ProposalDigest: proposal.Digest,
		EvidenceReferenceIDs: append([]string(nil), proposal.EvidenceReferenceIDs...), ComparisonIDs: append([]string(nil), proposal.ComparisonIDs...),
		PolicyVersion: stored.Evidence.PolicyVersion, Status: ValidationReferencesValid, ValidatedAt: time.Now().UTC(),
	}
	if _, err := db.appendReceipt(ctx, projectKey, created.ID, 3, validationRequest.IdempotencyKey, validationHash, receipt); err != nil {
		t.Fatalf("append legacy receipt: %v", err)
	}

	candidateRequest := SaveCandidateRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 4,
		IdempotencyKey: testIdempotencyKey("v1-migration-candidate"), ProposalID: proposal.ID, ReceiptID: receipt.ID,
	}
	candidateHash, err := digestJSON(candidateRequest)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	ref0 := stored.References[0]
	citations := []EvidenceCitation{{
		EvidenceReferenceID: ref0.ID, RequestID: ref0.RequestID, RevisionID: ref0.RevisionID,
		Side: ref0.Side, SpanStart: ref0.SpanStart, SpanEnd: ref0.SpanEnd, SHA256: ref0.SHA256,
	}}
	candidate, err := db.appendCandidate(ctx, projectKey, created.ID, 4, candidateRequest.IdempotencyKey, candidateHash, candidateID, proposal, receipt, citations)
	if err != nil {
		t.Fatalf("append legacy candidate: %v", err)
	}
	legacyReplay, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	var legacyReplayFields map[string]json.RawMessage
	if err := json.Unmarshal(legacyReplay, &legacyReplayFields); err != nil {
		t.Fatal(err)
	}
	delete(legacyReplayFields, "candidate_id")
	legacyReplay, err = json.Marshal(legacyReplayFields)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	projectDirectory, err := projectstore.ProjectDirectory(ref, root)
	if err != nil {
		t.Fatal(err)
	}
	downgradeConsultationV2ToV1ForTest(t, filepath.Join(projectDirectory, consultationDatabaseName), projectKey, created.ID, candidateRequest.IdempotencyKey, legacyReplay)

	migrated, err := NewService(root, []project.CaidoProjectRef{ref}, evidence, comparison)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	pending, err := migrated.Resume(ctx, projectKey, created.ID)
	if err != nil {
		t.Fatalf("resume migrated session: %v", err)
	}
	if pending.ReviewAccepted || pending.Question != "" || pending.Evidence.Question != "" || pending.Evidence.Evidence[0].Excerpt != "" ||
		pending.Evidence.Evidence[0].SHA256 != "" || len(pending.Comparisons) != 0 || len(pending.Proposals) != 0 || len(pending.Receipts) != 0 || len(pending.Candidates) != 0 {
		t.Fatalf("migrated pending session exposed legacy analysis: %#v", pending)
	}
	operatorPreview, err := migrated.ReviewSession(ctx, projectKey, created.ID)
	if err != nil || len(operatorPreview.Comparisons) != 1 || len(operatorPreview.Proposals) != 1 || len(operatorPreview.Receipts) != 1 || len(operatorPreview.Candidates) != 1 {
		t.Fatalf("operator could not review migrated content: session=%#v err=%v", operatorPreview, err)
	}
	if _, err := migrated.AcceptReview(ctx, projectKey, created.ID, 5, testIdempotencyKey("v1-migration-accept")); err != nil {
		t.Fatalf("accept migrated session: %v", err)
	}
	replayed, err := migrated.SaveCandidate(ctx, candidateRequest)
	if err != nil || replayed.CandidateID != created.ID || replayed.Version != 1 || replayed.ReceiptID != receipt.ID {
		t.Fatalf("migrated legacy candidate replay=%#v err=%v", replayed, err)
	}
}

func TestProposalCitationValidationFailsClosedAndCandidateCannotSkipReceipt(t *testing.T) {
	ctx := context.Background()
	root, ref, projectKey := createConsultationProject(t, "project-a")
	evidence := &evidenceBuilderFake{packet: samplePacket(projectKey)}
	service := newTestService(t, root, ref, evidence, &comparisonBuilderFake{result: sampleComparison()})
	created, err := service.CreateSession(ctx, createRequestFor(projectKey, 1), testIdempotencyKey("create-single"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := service.AcceptReview(ctx, projectKey, created.ID, 1, testIdempotencyKey("accept-single")); err != nil {
		t.Fatal(err)
	}
	proposal, err := service.RecordProposal(ctx, RecordProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 2, IdempotencyKey: testIdempotencyKey("record-forged"),
		Proposal: sampleProposal(), EvidenceReferenceIDs: []string{strings.Repeat("z", 43)},
	})
	if err != nil {
		t.Fatalf("RecordProposal should preserve an untrusted proposal for review: %v", err)
	}
	if _, err := service.ValidateProposal(ctx, ValidateProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 3, IdempotencyKey: testIdempotencyKey("validate-forged"), ProposalID: proposal.ID,
	}); !errors.Is(err, ErrCitationInvalid) {
		t.Fatalf("forged citation validation error = %v", err)
	}
	if _, err := service.SaveCandidate(ctx, SaveCandidateRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 3, IdempotencyKey: testIdempotencyKey("save-without-receipt"),
		ProposalID: proposal.ID, ReceiptID: strings.Repeat("y", 43),
	}); !errors.Is(err, ErrReceiptInvalid) {
		t.Fatalf("candidate without receipt error = %v", err)
	}
	resumed, err := service.Resume(ctx, projectKey, created.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(resumed.Receipts) != 0 || len(resumed.Candidates) != 0 || resumed.Version != 3 {
		t.Fatalf("invalid citation changed session state: %#v", resumed)
	}
}

func TestCandidateProposalTextIsRedactedBeforePersistence(t *testing.T) {
	ctx := context.Background()
	root, ref, projectKey := createConsultationProject(t, "project-a")
	evidence := &evidenceBuilderFake{packet: samplePacket(projectKey)}
	comparison := &comparisonBuilderFake{result: sampleComparison()}
	service := newTestService(t, root, ref, evidence, comparison)
	created, err := service.CreateSession(ctx, createRequestFor(projectKey, 1), testIdempotencyKey("create-redact"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptReview(ctx, projectKey, created.ID, 1, testIdempotencyKey("accept-redact")); err != nil {
		t.Fatal(err)
	}
	proposalInput := sampleProposal()
	proposalInput.Rationale = "session_token=private-value"
	proposal, err := service.RecordProposal(ctx, RecordProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 2, IdempotencyKey: testIdempotencyKey("record-redact"),
		Proposal: proposalInput, EvidenceReferenceIDs: []string{created.References[0].ID},
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if strings.Contains(proposal.Proposal.Rationale, "private-value") || !strings.Contains(proposal.Proposal.Rationale, "[REDACTED]") {
		t.Fatalf("proposal secret was not redacted: %#v", proposal.Proposal)
	}
	encoded, _ := json.Marshal(proposal)
	if strings.Contains(string(encoded), "private-value") {
		t.Fatalf("proposal response exposed secret: %s", encoded)
	}
}

func TestSessionVersionAndIdempotencyPreventLostUpdateAndDuplicateCandidate(t *testing.T) {
	ctx := context.Background()
	root, ref, projectKey := createConsultationProject(t, "project-a")
	service := newTestService(t, root, ref, &evidenceBuilderFake{packet: samplePacket(projectKey)}, &comparisonBuilderFake{result: sampleComparison()})
	created, err := service.CreateSession(ctx, createRequestFor(projectKey, 1), testIdempotencyKey("create-version"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptReview(ctx, projectKey, created.ID, 1, testIdempotencyKey("accept-version")); err != nil {
		t.Fatal(err)
	}
	first, err := service.RecordProposal(ctx, RecordProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 2, IdempotencyKey: testIdempotencyKey("record-version"),
		Proposal: sampleProposal(), EvidenceReferenceIDs: []string{created.References[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.RecordProposal(ctx, RecordProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 2, IdempotencyKey: testIdempotencyKey("record-version"),
		Proposal: sampleProposal(), EvidenceReferenceIDs: []string{created.References[0].ID},
	})
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("proposal retry=%#v err=%v", replayed, err)
	}
	if _, err := service.RecordProposal(ctx, RecordProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 2, IdempotencyKey: testIdempotencyKey("other-operation"),
		Proposal: sampleProposal(), EvidenceReferenceIDs: []string{created.References[0].ID},
	}); !errors.Is(err, ErrSessionVersionConflict) {
		t.Fatalf("stale session version error=%v", err)
	}
}

func TestSessionsAreProjectIsolatedAndRefuseSymlinkedDatabase(t *testing.T) {
	ctx := context.Background()
	root, refA, projectKeyA := createConsultationProject(t, "project-a")
	refB, projectKeyB := createAnotherProject(t, root, "project-b")
	service := newTestService(t, root, refA, &evidenceBuilderFake{packet: samplePacket(projectKeyA)}, &comparisonBuilderFake{result: sampleComparison()})
	created, err := service.CreateSession(ctx, createRequestFor(projectKeyA, 1), testIdempotencyKey("create-isolated"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resume(ctx, projectKeyB, created.ID); !errors.Is(err, ErrProjectNotRegistered) {
		t.Fatalf("cross-project resume error=%v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	projectDir, err := projectstore.ProjectDirectory(refB, root)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.sqlite3")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(projectDir, consultationDatabaseName)); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	otherService := newTestService(t, root, refB, &evidenceBuilderFake{packet: samplePacket(projectKeyB)}, &comparisonBuilderFake{result: sampleComparison()})
	_, err = otherService.CreateSession(ctx, createRequestFor(projectKeyB, 1), testIdempotencyKey("create-symlink"))
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("symlinked store error=%v", err)
	}
	contents, err := os.ReadFile(outside)
	if err != nil || string(contents) != "untouched" {
		t.Fatalf("symlink target changed: %q, err=%v", contents, err)
	}
}

func TestCreateSessionFailsClosedWhenEvidenceBuilderFails(t *testing.T) {
	ctx := context.Background()
	root, ref, projectKey := createConsultationProject(t, "project-a")
	service := newTestService(t, root, ref, &evidenceBuilderFake{err: errors.New("vault key is unavailable")}, &comparisonBuilderFake{})
	if _, err := service.CreateSession(ctx, createRequestFor(projectKey, 1), testIdempotencyKey("create-no-evidence")); !errors.Is(err, ErrEvidenceUnavailable) {
		t.Fatalf("CreateSession error=%v", err)
	}
}

func TestCreateIdempotencyHashesOnlySanitizedQuestion(t *testing.T) {
	ctx := context.Background()
	root, ref, projectKey := createConsultationProject(t, "project-a")
	evidence := &evidenceBuilderFake{packet: samplePacket(projectKey)}
	service := newTestService(t, root, ref, evidence, &comparisonBuilderFake{})
	first := createRequestFor(projectKey, 1)
	first.Question = "session_token=first-sensitive-value"
	created, err := service.CreateSession(ctx, first, testIdempotencyKey("sanitized-question"))
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Question = "session_token=second-sensitive-value"
	replayed, err := service.CreateSession(ctx, second, testIdempotencyKey("sanitized-question"))
	if err != nil || replayed.ID != created.ID || evidence.calls != 1 {
		t.Fatalf("sanitized question replay=%#v calls=%d err=%v", replayed, evidence.calls, err)
	}
}

func TestComparisonRejectsMismatchedNestedRevisionSummaries(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*appquery.CaptureComparison)
	}{
		{
			name: "nested request ID mismatch",
			mutate: func(comparison *appquery.CaptureComparison) {
				comparison.Request.Left.RequestID = "different-request"
			},
		},
		{
			name: "nested revision hash malformed",
			mutate: func(comparison *appquery.CaptureComparison) {
				comparison.Request.Left = appquery.CaptureRevisionSummary{
					RequestID: "request-a", RevisionID: strings.Repeat("a", 64), Size: 16,
					SHA256: "not-a-sha256", ParseState: "complete", WireFidelity: "exact",
				}
			},
		},
		{
			name: "unavailable side has malformed partial hash",
			mutate: func(comparison *appquery.CaptureComparison) {
				comparison.Request.Available = false
				comparison.Request.Reason = "selected request bytes are unavailable"
				comparison.Request.LeftExcerpt = ""
				comparison.Request.RightExcerpt = ""
				comparison.Request.Left = appquery.CaptureRevisionSummary{
					RequestID: "request-a", RevisionID: strings.Repeat("a", 64), Size: 16,
					SHA256: "malformed", ParseState: "complete", WireFidelity: "exact",
				}
			},
		},
		{
			name: "unavailable side makes equality claim without other metadata",
			mutate: func(comparison *appquery.CaptureComparison) {
				comparison.Request.Available = false
				comparison.Request.Reason = "selected request bytes are unavailable"
				comparison.Request.Equal = true
				comparison.Request.ComparedBytes = 0
				comparison.Request.LeftExcerpt = ""
				comparison.Request.RightExcerpt = ""
			},
		},
		{
			name: "unavailable side has inconsistent difference metadata",
			mutate: func(comparison *appquery.CaptureComparison) {
				comparison.Request.Available = false
				comparison.Request.Reason = "selected request bytes are unavailable"
				comparison.Request.ComparedBytes = 16
				comparison.Request.FirstDifferenceOffset = int64ptr(3)
				comparison.Request.LeftExcerpt = ""
				comparison.Request.RightExcerpt = ""
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			root, ref, projectKey := createConsultationProject(t, "project-a")
			bad := sampleComparison()
			testCase.mutate(&bad)
			service := newTestService(t, root, ref, &evidenceBuilderFake{packet: samplePacket(projectKey)}, &comparisonBuilderFake{result: bad})
			created, err := service.CreateSession(ctx, createRequestFor(projectKey, 2), testIdempotencyKey("nested-create"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.AcceptReview(ctx, projectKey, created.ID, 1, testIdempotencyKey("nested-accept")); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Compare(ctx, projectKey, created.ID, 2, testIdempotencyKey("nested-compare"), sampleComparisonRequest(projectKey)); !errors.Is(err, ErrComparisonUnavailable) {
				t.Fatalf("invalid nested comparison was accepted: %v", err)
			}
			resumed, err := service.Resume(ctx, projectKey, created.ID)
			if err != nil || resumed.Version != 2 || len(resumed.Comparisons) != 0 {
				t.Fatalf("failed comparison mutated session: session=%#v err=%v", resumed, err)
			}
		})
	}
}

func TestUnavailableComparisonAllowsValidOversizedSourceMetadata(t *testing.T) {
	request := sampleComparisonRequest(strings.Repeat("a", 64))
	side := appquery.CaptureSideComparison{
		Available: false, Reason: "One or both stored messages exceed the 1 MiB comparison limit.",
		Left: appquery.CaptureRevisionSummary{
			RequestID: request.LeftRequestID, RevisionID: request.LeftRevisionID,
			Size: int64(appquery.MaximumComparisonMessageBytes) + 1, SHA256: strings.Repeat("1", 64),
			ParseState: "complete", WireFidelity: "source_bytes",
		},
		Right: appquery.CaptureRevisionSummary{
			RequestID: request.RightRequestID, RevisionID: request.RightRevisionID,
			Size: 16, SHA256: strings.Repeat("2", 64), ParseState: "complete", WireFidelity: "source_bytes",
		},
	}
	if !comparisonSideMatches(side, request) {
		t.Fatal("valid oversized-source metadata was rejected for an unavailable comparison")
	}
}

func TestCandidateVersionsAreScopedToStableCandidateID(t *testing.T) {
	ctx := context.Background()
	root, ref, projectKey := createConsultationProject(t, "project-a")
	evidence := &evidenceBuilderFake{packet: samplePacket(projectKey)}
	comparison := &comparisonBuilderFake{result: sampleComparison()}
	service := newTestService(t, root, ref, evidence, comparison)
	created, err := service.CreateSession(ctx, createRequestFor(projectKey, 1), testIdempotencyKey("candidate-session"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptReview(ctx, projectKey, created.ID, 1, testIdempotencyKey("candidate-review")); err != nil {
		t.Fatal(err)
	}
	makeProposal := func(version int, key string, title string) (ProposalRecord, ValidationReceipt) {
		t.Helper()
		input := sampleProposal()
		input.Title = title
		proposal, callErr := service.RecordProposal(ctx, RecordProposalRequest{
			ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: version, IdempotencyKey: testIdempotencyKey("proposal-" + key),
			Proposal: input, EvidenceReferenceIDs: []string{created.References[0].ID},
		})
		if callErr != nil {
			t.Fatalf("RecordProposal(%s): %v", key, callErr)
		}
		receipt, callErr := service.ValidateProposal(ctx, ValidateProposalRequest{
			ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: version + 1, IdempotencyKey: testIdempotencyKey("receipt-" + key), ProposalID: proposal.ID,
		})
		if callErr != nil {
			t.Fatalf("ValidateProposal(%s): %v", key, callErr)
		}
		return proposal, receipt
	}
	firstProposal, firstReceipt := makeProposal(2, "one", "First candidate")
	first, err := service.SaveCandidate(ctx, SaveCandidateRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 4, IdempotencyKey: testIdempotencyKey("candidate-one"),
		ProposalID: firstProposal.ID, ReceiptID: firstReceipt.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondProposal, secondReceipt := makeProposal(5, "two", "Second candidate")
	second, err := service.SaveCandidate(ctx, SaveCandidateRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 7, IdempotencyKey: testIdempotencyKey("candidate-two"),
		ProposalID: secondProposal.ID, ReceiptID: secondReceipt.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	thirdProposal, thirdReceipt := makeProposal(8, "three", "First candidate revision")
	updated, err := service.SaveCandidate(ctx, SaveCandidateRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: 10, IdempotencyKey: testIdempotencyKey("candidate-one-revision"),
		ProposalID: thirdProposal.ID, ReceiptID: thirdReceipt.ID, CandidateID: first.CandidateID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.CandidateID == second.CandidateID || first.Version != 1 || second.Version != 1 || updated.CandidateID != first.CandidateID || updated.Version != 2 {
		t.Fatalf("candidate identity/version sequence first=%#v second=%#v updated=%#v", first, second, updated)
	}
	resumed, err := service.Resume(ctx, projectKey, created.ID)
	if err != nil || len(resumed.Candidates) != 3 {
		t.Fatalf("candidate records did not resume: count=%d err=%v", len(resumed.Candidates), err)
	}
}

func newTestService(t *testing.T, root string, ref project.CaidoProjectRef, evidence EvidenceBuilder, comparisons ComparisonBuilder) *Service {
	t.Helper()
	service, err := NewService(root, []project.CaidoProjectRef{ref}, evidence, comparisons)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func createConsultationProject(t *testing.T, projectID string) (string, project.CaidoProjectRef, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runtime")
	ref := project.CaidoProjectRef{InstanceID: "instance", ProjectID: projectID}
	selected, err := projectstore.CreateProject(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := selected.Close(); err != nil {
		t.Fatalf("close created project: %v", err)
	}
	key, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	return root, ref, key
}

func createAnotherProject(t *testing.T, root, projectID string) (project.CaidoProjectRef, string) {
	t.Helper()
	ref := project.CaidoProjectRef{InstanceID: "instance", ProjectID: projectID}
	selected, err := projectstore.CreateProject(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := selected.Close(); err != nil {
		t.Fatalf("close created project: %v", err)
	}
	key, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	return ref, key
}

func createRequestFor(projectKey string, count int) appquery.EvidencePacketRequest {
	selections := []appquery.EvidencePacketSelection{
		{RequestID: "request-a", RevisionID: strings.Repeat("a", 64), Side: "response", Offset: 0, Length: 16},
		{RequestID: "request-b", RevisionID: strings.Repeat("c", 64), Side: "response", Offset: 0, Length: 16},
	}
	return appquery.EvidencePacketRequest{ProjectKey: projectKey, Question: "Compare the selected responses", Selections: selections[:count]}
}

func samplePacket(projectKey string) appquery.EvidencePacket {
	return appquery.EvidencePacket{
		Version: "1", PolicyVersion: appquery.EvidencePacketPolicyVersion, ProjectKey: projectKey,
		Question: "Compare the selected responses", PreviewOnly: true, OperatorReviewNeeded: true,
		SelectedBytes: 32, OmittedBytesKnown: true, Evidence: []appquery.EvidencePacketEvidence{
			{Available: true, RequestID: "request-a", RevisionID: strings.Repeat("a", 64), Side: "response", SpanStart: 0, SpanEnd: 16, SelectedBytes: 16, IncludedBytes: 16, SHA256: strings.Repeat("b", 64), SourceIntegrityStatus: "verified", ParseState: "complete", WireFidelity: "exact", Excerpt: "HTTP/1.1 200 OK\r\n"},
			{Available: true, RequestID: "request-b", RevisionID: strings.Repeat("c", 64), Side: "response", SpanStart: 0, SpanEnd: 16, SelectedBytes: 16, IncludedBytes: 16, SHA256: strings.Repeat("d", 64), SourceIntegrityStatus: "verified", ParseState: "complete", WireFidelity: "exact", Excerpt: "HTTP/1.1 403 Forbidden"},
		},
	}
}

func sampleComparison() appquery.CaptureComparison {
	return appquery.CaptureComparison{
		Request:      appquery.CaptureSideComparison{Available: true, Equal: true, ComparedBytes: 16, LeftExcerpt: "Authorization: Bearer very-secret-token", RightExcerpt: "cookie=session-secret"},
		Response:     appquery.CaptureSideComparison{Available: true, Equal: false, ComparedBytes: 16, FirstDifferenceOffset: int64ptr(3), LeftExcerpt: "user_id=1", RightExcerpt: "user_id=2"},
		CoverageNote: "synthetic exact comparison",
	}
}

func sampleComparisonRequest(projectKey string) appquery.CaptureComparisonRequest {
	return appquery.CaptureComparisonRequest{
		ProjectKey: projectKey, LeftRequestID: "request-a", LeftRevisionID: strings.Repeat("a", 64),
		RightRequestID: "request-b", RightRevisionID: strings.Repeat("c", 64),
	}
}

func sampleProposal() CandidateProposal {
	return CandidateProposal{
		Title: "Possible object access issue", Summary: "The selected response differs by actor.",
		Rationale: "Compare the exact selected evidence before drawing a conclusion.", Impact: "Potential cross-account data exposure.",
		Alternatives: []string{"The response may be public."}, NextChecks: []string{"Repeat with a second permitted actor."},
		Limitations: []string{"Only one selected capture pair was compared."},
	}
}

func testIdempotencyKey(value string) string { return value + "-0123456789abc" }

func int64ptr(value int64) *int64 { return &value }
