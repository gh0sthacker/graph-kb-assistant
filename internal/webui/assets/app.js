"use strict";

const $ = (selector, root = document) => root.querySelector(selector);
const MAX_GRAPH_ZOOM = 10;
const MIN_GRAPH_ZOOM = 0.35;
const MAX_EVIDENCE_PREVIEW_BYTES = 2 << 20;
const FIELD_VALUE_TRAVERSAL_STATES = new Set(["expanded", "bounded", "index_unavailable", "no_matches", "not_applicable"]);
const OVERVIEW_HIDDEN_KINDS = ["observation", "request_variant", "response_variant", "field_occurrence", "value_reference", "url_reference"];
const state = {
  projects: [], projectCursor: null, projectKey: "", endpoints: [], endpointCursor: null, surfaceGaps: [],
  operationAliases: new Map(), operationAliasRequestId: 0,
  surfaceFilter: "", surfaceDebounce: null, totalOperations: 0,
  graph: null, selectedEndpoint: null, selectedNode: null, focusedNodeId: null, graphFocus: false, loadingSurface: false,
  surfaceRequestId: 0, surfaceAbort: null,
  graphSeeds: [], graphView: "direct", graphHistory: [], graphRequestId: 0, graphAbort: null,
  loadingGraph: false, hiddenKinds: new Set(OVERVIEW_HIDDEN_KINDS), routeScope: true, edgeLabelsVisible: true,
  renderedEntities: [], renderedAssertions: [], layoutPositions: new Map(), zoom: 1, panX: 0, panY: 0, drag: null, toastTimer: null,
  evidence: null, evidenceRequestId: 0, evidenceTrigger: null, packetPreviewRequestId: 0,
  evidencePacketDraft: { projectKey: "", question: "", selections: [] }, comparisonBaseline: null, comparison: null,
  evidencePacketPreview: null, consultation: null,
  operationBackfillPreview: null, operationBackfillRequestId: 0,
  consultationCreateKey: "", contextAnnotationKey: "", consultationResumeRequestId: 0,
  contextEvidenceReferences: [], contextEvidenceAccepted: false, contextFlowOrder: [],
};

const elements = {
  project: $("#project-select"), endpointList: $("#endpoint-list"), endpointCount: $("#endpoint-count"),
  filter: $("#endpoint-filter"), loadMore: $("#load-more"), surfaceFoot: $("#surface-foot-text"),
  operationBackfillOpen: $("#operation-backfill-open"), operationBackfillDialog: $("#operation-backfill-dialog"),
  operationBackfillClose: $("#operation-backfill-close"), operationBackfillMaxCaptures: $("#operation-backfill-max-captures"),
  operationBackfillMaxBytes: $("#operation-backfill-max-bytes"), operationBackfillPreview: $("#operation-backfill-preview"),
  operationBackfillApply: $("#operation-backfill-apply"), operationBackfillStatus: $("#operation-backfill-status"),
  operationBackfillResult: $("#operation-backfill-result"),
  graphCanvas: $("#graph-canvas"), graphScene: $("#graph-scene"), canvasEmpty: $("#canvas-empty"),
  emptyTitle: $("#canvas-empty-title"), emptyCopy: $("#canvas-empty-copy"), graphState: $("#graph-state"),
  fieldValueStatus: $("#field-value-status"),
  graphTitle: $("#graph-title"), graphSubtitle: $("#graph-subtitle"), graphProject: $("#graph-project-label"),
  graphCounts: $("#graph-counts"), inspector: $("#inspector-content"), inspectIndex: $("#inspect-index"),
  graphFilters: $("#graph-filters"), graphFilterSummary: $("#graph-filter-summary"),
  graphOverview: $("#graph-overview"), graphShowAll: $("#graph-show-all"), edgeLabels: $("#toggle-edge-labels"),
  status: $("#status-text"), coverage: $("#coverage-status"), updated: $("#last-updated"), evidenceVaultStatus: $("#evidence-vault-status"),
  zoom: $("#zoom-readout"), zoomIn: $("#zoom-in"), zoomOut: $("#zoom-out"),
  focusGraph: $("#focus-graph"), workspace: $(".workspace"),
  captureSelect: $("#capture-select"), graphBack: $("#graph-back"), graphMore: $("#graph-more"), graphDeeper: $("#graph-deeper"),
  toast: $("#toast"), viewport: $("#graph-viewport"),
  evidenceDialog: $("#evidence-dialog"), evidenceClose: $("#evidence-close"),
  evidenceLabel: $("#evidence-capture-label"), evidenceMeta: $("#evidence-meta"), evidenceContent: $("#evidence-content"),
  evidenceFoot: $("#evidence-foot"), evidenceMore: $("#evidence-more"), evidenceCompareContent: $("#evidence-compare-content"),
  comparisonAttach: $("#consultation-comparison-link"), comparisonAttachButton: $("#consultation-comparison-attach"),
  comparisonAttachStatus: $("#consultation-comparison-status"),
  evidenceTitle: $("#evidence-title"), evidenceSides: $(".evidence-sides"),
  evidencePacketControls: $("#evidence-packet-controls"), evidencePacketQuestion: $("#evidence-packet-question"),
  evidencePacketOffset: $("#evidence-packet-offset"), evidencePacketLength: $("#evidence-packet-length"),
  evidencePacketAdd: $("#evidence-packet-add"), evidencePacketSelectionCount: $("#evidence-packet-selection-count"),
  evidencePacketSelectionList: $("#evidence-packet-selection-list"), evidencePacketClear: $("#evidence-packet-clear"),
  evidencePacketPreview: $("#evidence-packet-preview"), evidencePacketContent: $("#evidence-packet-content"),
  consultationCreate: $("#consultation-create"), consultationCopyID: $("#consultation-copy-id"), consultationStatus: $("#consultation-status"),
  consultationResumeID: $("#consultation-resume-id"), consultationResume: $("#consultation-resume"),
  consultationOpen: $("#consultation-open"),
  consultationReview: $("#consultation-review"), consultationReviewContent: $("#consultation-review-content"),
  consultationContext: $("#consultation-context"), consultationContextKind: $("#consultation-context-kind"),
  consultationContextLabel: $("#consultation-context-label"), consultationContextEvidence: $("#consultation-context-evidence"),
  consultationContextSave: $("#consultation-context-save"), consultationContextList: $("#consultation-context-list"),
  consultationAccept: $("#consultation-accept"),
  consultationCandidate: $("#consultation-candidate"), candidateTitle: $("#candidate-title"),
  candidateImpact: $("#candidate-impact"), candidateSummary: $("#candidate-summary"),
  candidateRationale: $("#candidate-rationale"), candidateAlternatives: $("#candidate-alternatives"),
  candidateNextChecks: $("#candidate-next-checks"), candidateLimitations: $("#candidate-limitations"),
  candidateEvidence: $("#candidate-evidence"), candidateComparisons: $("#candidate-comparisons"),
  candidateRecoveryList: $("#candidate-recovery-list"), candidateSave: $("#candidate-save"),
  candidateStatus: $("#candidate-status"), candidateList: $("#candidate-list"),
  evidenceRequest: $("#evidence-request"), evidenceResponse: $("#evidence-response"),
};

const nodeLabels = {
  observation: "HTTP REQUEST", endpoint: "ENDPOINT", operation: "OPERATION", parameter: "PARAMETER",
  asset: "HOST", actor: "ACTOR", object: "OBJECT", session_context: "SESSION CONTEXT",
  condition: "CONDITION", flow: "FLOW", classifier_signal: "SIGNAL",
  hypothesis: "HYPOTHESIS", request_variant: "REQUEST SHAPE", response_variant: "RESPONSE SHAPE",
  field_occurrence: "FIELD OCCURRENCE", value_reference: "VALUE MATCH", url_reference: "URL MATCH",
};
const filterLabels = {
  observation: "Requests", endpoint: "Endpoints", operation: "Operations", parameter: "Parameters", asset: "Hosts",
  actor: "Actors", object: "Objects", session_context: "Contexts",
  condition: "Conditions", flow: "Flows", classifier_signal: "Signals",
  hypothesis: "Hypotheses", request_variant: "Request shapes", response_variant: "Response shapes",
  field_occurrence: "Field occurrences", value_reference: "Value matches", url_reference: "URL matches",
};
const relationLabels = {
  belongs_to_operation: "belongs to operation",
  exposes: "serves endpoint",
  invokes: "calls endpoint",
  has_parameter: "has parameter",
  has_occurrence: "observed field",
  occurrence_of: "field shape",
  value_used_in: "same project-keyed value; causality unknown",
  returns: "returns parameter",
  uses_request_variant: "uses request shape",
  received_response: "received response shape",
  supports: "supports hypothesis",
  contradicts: "contradicts hypothesis",
  contains: "contains",
  precedes: "precedes",
  referrer_context_for: "referer matches",
  redirects_to: "redirects to",
  uses_request_url: "request URL matches",
  referrer_url_matches: "Referer URL matches",
  redirect_url_matches: "redirect URL matches",
};
const MASKED_PATH_HELP = "Graph route labels hide dynamic values so IDs and secrets do not become graph data. To inspect one saved request's exact path, open its linked capture with Open stored HTTP; live-only records remain in Caido.";
const kindColors = { observation: "#83c8cf", endpoint: "#e0b56b", operation: "#83b5e8", parameter: "#b8a3da", asset: "#b2d88a" };

function el(tag, className, text) {
  const result = document.createElement(tag);
  if (className) result.className = className;
  if (text !== undefined) result.textContent = String(text);
  return result;
}

function svgEl(tag, attrs = {}) {
  const result = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [name, value] of Object.entries(attrs)) result.setAttribute(name, String(value));
  return result;
}

function setStatus(message, kind = "") {
  elements.status.textContent = message;
  elements.graphState.className = `graph-state ${kind}`.trim();
  const statusLabel = elements.graphState.querySelector("span:last-child");
  if (statusLabel) statusLabel.textContent = message;
}

function fieldValueTraversalPresentation(traversal) {
  let state = FIELD_VALUE_TRAVERSAL_STATES.has(traversal?.state) ? traversal.state : "not_applicable";
  if (state === "expanded" && traversal?.truncated === true) state = "bounded";
  const pathNodeCount = Number.isSafeInteger(traversal?.path_node_count) && traversal.path_node_count >= 0
    ? traversal.path_node_count : null;
  const countLabel = pathNodeCount === null || pathNodeCount === 0 ? "" : ` · ${pathNodeCount} path nodes`;
  const correlationNote = "Same-project keyed value equality is correlation only; it does not establish causality, identity, or ownership.";

  switch (state) {
    case "expanded":
      return { state, label: `Value correlation expanded${countLabel}`, title: correlationNote };
    case "bounded":
      return { state, label: `Value correlation bounded${countLabel} · more may exist`, title: `${correlationNote} This traversal is bounded, so additional matching paths may exist.` };
    case "index_unavailable":
      return {
        state, label: "Value index unavailable · direct graph only",
        title: `The project field-value index is unavailable or stale; only direct graph neighbors are shown. ${correlationNote}`,
      };
    case "no_matches":
      return { state, label: "No matching same-project value correlations", title: correlationNote };
    default:
      return { state: "not_applicable", label: "Field-value traversal not applicable", title: "This graph slice does not include field-value traversal. " + correlationNote };
  }
}

function renderFieldValueTraversalStatus(traversal) {
  const status = elements.fieldValueStatus;
  if (!status) return;
  const presentation = fieldValueTraversalPresentation(traversal);
  status.hidden = false;
  status.dataset.state = presentation.state;
  status.textContent = presentation.label;
  status.title = presentation.title;
  status.setAttribute("aria-label", `${presentation.label}. ${presentation.title}`);
}

function clearFieldValueTraversalStatus() {
  const status = elements.fieldValueStatus;
  if (!status) return;
  status.hidden = true;
  status.textContent = "";
  status.title = "";
  status.dataset.state = "";
  status.setAttribute("aria-label", "");
}

function showToast(message) {
  elements.toast.textContent = message;
  elements.toast.classList.add("show");
  clearTimeout(state.toastTimer);
  state.toastTimer = setTimeout(() => elements.toast.classList.remove("show"), 2600);
}

async function getJSON(path, signal) {
  const response = await fetch(path, { headers: { Accept: "application/json" }, credentials: "same-origin", cache: "no-store", signal });
  let payload;
  try { payload = await response.json(); } catch { throw new Error("The server returned an invalid response."); }
  if (!response.ok) {
    const error = new Error(payload.error || `Read failed (${response.status}).`);
    error.status = response.status;
    throw error;
  }
  return payload;
}

async function postJSON(path, body) {
  const response = await fetch(path, {
    method: "POST", headers: { Accept: "application/json", "Content-Type": "application/json" },
    credentials: "same-origin", cache: "no-store", body: JSON.stringify(body),
  });
  let payload;
  try { payload = await response.json(); } catch { throw new Error("The server returned an invalid response."); }
  if (!response.ok) {
    const error = new Error(payload.error || `Read failed (${response.status}).`);
    error.status = response.status;
    throw error;
  }
  return payload;
}

function clearOperationBackfillPreview(message = "Select a project, then request an explicit preview.") {
  state.operationBackfillRequestId++;
  state.operationBackfillPreview = null;
  elements.operationBackfillPreview.disabled = !state.projectKey;
  elements.operationBackfillApply.disabled = true;
  elements.operationBackfillStatus.textContent = message;
  elements.operationBackfillResult.replaceChildren();
  elements.operationBackfillResult.hidden = true;
}

function operationBackfillOptions() {
  const maxCaptures = Number(elements.operationBackfillMaxCaptures.value);
  const maxRequestBytes = Number(elements.operationBackfillMaxBytes.value);
  if (!Number.isInteger(maxCaptures) || maxCaptures < 1 || maxCaptures > 20000 ||
      !Number.isSafeInteger(maxRequestBytes) || maxRequestBytes < 1048576 || maxRequestBytes > 1073741824) {
    throw new Error("Choose limits within 1–20,000 captures and 1 MiB–1 GiB.");
  }
  return { max_captures: maxCaptures, max_request_bytes: maxRequestBytes };
}

function operationBackfillSkipReasonDetails(reasons) {
  const labels = {
    invalid_request_line: "Malformed or unsupported HTTP request line",
    invalid_origin: "Capture origin is not a valid HTTP origin",
    invalid_request_target: "Request target is malformed or unsupported",
    cross_origin_request_target: "Absolute request target does not match the captured origin",
    unsupported_request_target: "Request target form is not supported for safe grouping",
    invalid_resolved_endpoint: "Request target could not form a valid endpoint",
    canonical_projection_incomplete: "Canonical operation projection was incomplete",
    canonical_operation_missing: "Canonical operation record was not produced",
    canonical_endpoint_missing: "Canonical endpoint record was not produced",
    canonical_assertion_missing: "Canonical operation link was not produced",
    canonical_origin_unknown: "Operation origin could not be normalized",
    canonical_method_unknown: "Operation method could not be parsed",
    canonical_route_family_unavailable: "Route could not be safely grouped",
    canonical_route_path_too_long: "Route exceeds the safe grouping length",
    canonical_route_opaque: "Opaque URI targets cannot be grouped as HTTP routes",
    canonical_route_invalid: "Route target is invalid for grouping",
    canonical_route_segments_exceeded: "Route has too many path segments to group",
    canonical_route_unparseable: "Route target could not be parsed for grouping",
  };
  if (!reasons || typeof reasons !== "object" || Array.isArray(reasons)) return [];
  return Object.entries(labels)
    .map(([code, label]) => [label, Number(reasons[code] || 0)])
    .filter(([, count]) => Number.isSafeInteger(count) && count > 0);
}

function renderOperationBackfillPreview(result) {
  const details = [
    ["State", result.state], ["Coverage", result.coverage_state || "unknown"],
    ["Scanned", result.scanned], ["Eligible captures", result.eligible_captures],
    ["New operations", result.new_operation_groups], ["New endpoint entities", result.new_endpoint_entities],
    ["New links", result.new_assertion_links],
    ["Skipped", Number(result.skipped_unmapped || 0) + Number(result.skipped_oversized || 0) + Number(result.skipped_no_operation || 0)],
    ["Legacy endpoint ID differences", Number(result.endpoint_identity_mismatches || 0)],
    ["Request bytes inspected", formatBytes(result.request_bytes)],
  ];
  const list = el("ul");
  for (const [label, value] of details) list.append(el("li", "", `${label}: ${value ?? "—"}`));
  for (const [label, count] of operationBackfillSkipReasonDetails(result.skipped_no_operation_reasons)) {
    list.append(el("li", "", `No safe operation · ${label}: ${count}`));
  }
  if (Array.isArray(result.operation_samples) && result.operation_samples.length) {
    list.append(el("li", "", `Samples: ${result.operation_samples.map(sample => `${sample.method || "?"} ${sample.route_family || "/"}`).join(" · ")}`));
  }
  if (result.candidate_sha256) list.append(el("li", "", `Preview digest: ${String(result.candidate_sha256).slice(0, 12)}…`));
  elements.operationBackfillResult.replaceChildren(list);
  elements.operationBackfillResult.hidden = false;
}

async function previewOperationBackfill() {
  if (!state.projectKey) return;
  const projectKey = state.projectKey;
  const requestId = ++state.operationBackfillRequestId;
  let options;
  try { options = operationBackfillOptions(); } catch (error) {
    elements.operationBackfillStatus.textContent = error.message;
    return;
  }
  state.operationBackfillPreview = null;
  elements.operationBackfillPreview.disabled = true;
  elements.operationBackfillApply.disabled = true;
  elements.operationBackfillStatus.textContent = "Inspecting bounded historical request metadata locally…";
  elements.operationBackfillResult.replaceChildren();
  elements.operationBackfillResult.hidden = true;
  try {
    const result = await postJSON("/api/maintenance/operation-backfill/preview", { project_key: projectKey, ...options });
    if (requestId !== state.operationBackfillRequestId || state.projectKey !== projectKey) return;
    state.operationBackfillPreview = { projectKey, options, result };
    renderOperationBackfillPreview(result);
    const ready = result.state === "ready" && !!result.candidate_sha256 && Number(result.eligible_captures) > 0;
    elements.operationBackfillApply.disabled = !ready;
    elements.operationBackfillStatus.textContent = ready
      ? result.coverage_state === "partial"
        ? "Candidate is fully scanned but coverage is partial. Review excluded captures, then explicitly apply only the eligible records."
        : "Ready. Review the counts, then explicitly apply this exact preview."
      : result.state === "partial"
        ? "Preview is partial; it cannot be applied. Reduce the limits or resolve the reported coverage gaps."
        : "No operation changes are ready to apply.";
  } catch (error) {
    if (requestId !== state.operationBackfillRequestId || state.projectKey !== projectKey) return;
    elements.operationBackfillStatus.textContent = `Preview failed: ${error.message}`;
  } finally {
    if (requestId === state.operationBackfillRequestId && state.projectKey === projectKey) elements.operationBackfillPreview.disabled = false;
  }
}

async function applyOperationBackfill() {
  const preview = state.operationBackfillPreview;
  if (!preview || preview.projectKey !== state.projectKey || !preview.result.candidate_sha256 || preview.result.state !== "ready") return;
  const result = preview.result;
  const skipped = Number(result.skipped_unmapped || 0) + Number(result.skipped_oversized || 0) + Number(result.skipped_no_operation || 0);
  const identityMismatches = Number(result.endpoint_identity_mismatches || 0);
  const coverageNote = result.coverage_state === "partial"
    ? `\n${identityMismatches} legacy endpoint IDs differ from the canonical request-derived IDs; ${skipped} captures remain excluded.\n`
    : "";
  const message = `Apply this exact preview to the selected local graph?\n\n${result.eligible_captures} captures · ${result.new_operation_groups} operations · ${Number(result.new_endpoint_entities || 0)} endpoints · ${result.new_assertion_links} links${coverageNote}\nThis commits the local graph and then syncs its selected-project Neo4j outbox in order. Earlier graph events for this project may be applied first. It does not contact Caido or send data to Hermes.`;
  if (!window.confirm(message)) return;
  const requestId = ++state.operationBackfillRequestId;
  elements.operationBackfillPreview.disabled = true;
  elements.operationBackfillApply.disabled = true;
  elements.operationBackfillStatus.textContent = "Rechecking the exact preview and applying its bounded graph update…";
  try {
    const applied = await postJSON("/api/maintenance/operation-backfill/apply", {
      project_key: preview.projectKey, ...preview.options, candidate_sha256: result.candidate_sha256,
    });
    if (requestId !== state.operationBackfillRequestId || state.projectKey !== preview.projectKey) return;
    state.operationBackfillPreview = null;
    const neo4jStatus = applied.neo4j_drain === "ready"
      ? "Neo4j sync complete."
      : applied.neo4j_drain === "disabled"
        ? "Neo4j sync is disabled; local outbox remains pending."
        : applied.neo4j_drain === "not_applicable"
          ? "No graph updates were needed."
          : "Local graph committed; Neo4j outbox remains retryable and sync is pending.";
    elements.operationBackfillStatus.textContent = `Applied ${applied.bundles_imported} new bundles; ${applied.bundles_replayed} replayed. ${neo4jStatus}`;
    renderOperationBackfillPreview(result);
    elements.operationBackfillApply.disabled = true;
    await loadSurface(false);
  } catch (error) {
    if (requestId !== state.operationBackfillRequestId || state.projectKey !== preview.projectKey) return;
    elements.operationBackfillStatus.textContent = error.status === 409
      ? "The preview is stale or incomplete. Request a new preview before applying."
      : `Apply failed: ${error.message}`;
    elements.operationBackfillPreview.disabled = false;
  }
}

function clearEvidence(closeDialog = true, preserveAcceptedConsultation = false) {
  const consultationToPreserve = preserveAcceptedConsultation && state.consultation?.accepted === true &&
    state.consultation.reviewLoaded === true ? state.consultation : null;
  state.evidenceRequestId++;
  state.packetPreviewRequestId++;
  state.evidence = null;
  state.comparison = null;
  state.evidencePacketPreview = null;
  state.contextEvidenceReferences = [];
  state.contextEvidenceAccepted = false;
  state.contextFlowOrder = [];
  resetConsultationUI("Preview the selected evidence before creating a consultation.");
  if (consultationToPreserve?.projectKey === state.projectKey) state.consultation = consultationToPreserve;
  const trigger = state.evidenceTrigger;
  state.evidenceTrigger = null;
  if (closeDialog && elements.evidenceDialog.open) {
    elements.evidenceDialog.close();
    if (trigger?.isConnected) trigger.focus();
  }
  elements.evidenceLabel.textContent = "No capture selected";
  elements.evidenceMeta.textContent = "Select an exact historical capture to inspect its stored message.";
  elements.evidenceContent.textContent = "";
  elements.evidenceContent.hidden = false;
  elements.evidenceCompareContent.textContent = "";
  elements.evidenceCompareContent.hidden = true;
  elements.evidenceSides.hidden = false;
  elements.evidenceTitle.textContent = "Captured HTTP message";
  elements.evidencePacketControls.hidden = true;
  elements.evidencePacketQuestion.value = state.evidencePacketDraft.question;
  elements.evidencePacketOffset.value = "0";
  elements.evidencePacketLength.value = "";
  elements.evidencePacketAdd.disabled = true;
  elements.evidencePacketPreview.disabled = true;
  elements.evidencePacketContent.textContent = "";
  elements.evidencePacketContent.hidden = true;
  renderEvidencePacketSelections();
  elements.evidenceFoot.textContent = "";
  elements.evidenceMore.hidden = true;
  elements.evidenceMore.disabled = true;
  setEvidenceSideButtons("request");
}

function setEvidenceSideButtons(side) {
  for (const [button, value] of [[elements.evidenceRequest, "request"], [elements.evidenceResponse, "response"]]) {
    const active = side === value;
    button.classList.toggle("active", active);
    button.setAttribute("aria-pressed", String(active));
  }
}

function openEvidence(item, trigger = null) {
  const requestId = String(item?.request_id ?? "");
  const revisionId = String(item?.revision_id ?? "");
  if (!state.projectKey || !requestId || requestId.length > 512 || !/^[a-f0-9]{64}$/.test(revisionId) ||
      item?.record_source !== "encrypted_local_capture" || item?.raw_stored_locally !== true) {
    showToast("Exact local HTTP bytes are unavailable for this record.");
    return Promise.resolve();
  }
  clearEvidence(false);
  const project = state.projectKey;
  const requestID = ++state.evidenceRequestId;
  state.evidence = {
    projectKey: project, requestId, requestID, revisionId,
    side: "request", nextOffset: 0, totalSize: null, loading: false, packetPreviewLoading: false, packetDefaultsInitialized: false,
  };
  state.comparison = null;
  state.evidenceTrigger = trigger;
  elements.evidenceTitle.textContent = "Captured HTTP message";
  elements.evidencePacketControls.hidden = false;
  elements.evidencePacketQuestion.value = state.evidencePacketDraft.question;
  elements.evidencePacketOffset.value = "0";
  elements.evidencePacketLength.value = "";
  elements.evidencePacketContent.textContent = "";
  elements.evidencePacketContent.hidden = true;
  renderEvidencePacketSelections();
  elements.evidenceSides.hidden = false;
  elements.evidenceContent.hidden = false;
  elements.evidenceCompareContent.hidden = true;
  elements.evidenceCompareContent.textContent = "";
  elements.evidenceLabel.textContent = `Request ID ${requestId} · ${state.projects.find(row => row.project_key === project)?.display_name || project}`;
  elements.evidenceMeta.textContent = "Authenticating the selected local message revision…";
  elements.evidenceContent.textContent = "";
  elements.evidenceFoot.textContent = "";
  elements.evidenceMore.hidden = true;
  elements.evidenceMore.disabled = true;
  setEvidenceSideButtons("request");
  if (!elements.evidenceDialog.open) elements.evidenceDialog.showModal();
  return loadEvidenceRange(false);
}

function evidenceText(payload) {
  if (payload.encoding === "base64") return `[Binary data · ${payload.returned_size} bytes · base64]\n${payload.data || ""}`;
  return String(payload.data || "");
}

async function loadEvidenceRange(append) {
  const evidence = state.evidence;
  if (!evidence || evidence.loading || evidence.projectKey !== state.projectKey) return;
  const requestedOffset = append ? evidence.nextOffset : 0;
  if (!Number.isSafeInteger(requestedOffset) || requestedOffset < 0) return;
  const requestID = evidence.requestID;
  evidence.loading = true;
  elements.evidenceMore.disabled = true;
  elements.evidenceFoot.textContent = append ? "Loading the next authenticated range…" : "Loading the authenticated message range…";
  const query = new URLSearchParams({
    project_key: evidence.projectKey, request_id: evidence.requestId, side: evidence.side,
    offset: String(requestedOffset), max_bytes: String(1 << 20),
  });
  query.set("revision_id", evidence.revisionId);
  try {
    const payload = await getJSON(`/api/evidence?${query}`);
    if (state.evidence !== evidence || requestID !== state.evidenceRequestId || evidence.projectKey !== state.projectKey) return;
    validateEvidenceRangePayload(payload, evidence, requestedOffset);
    evidence.totalSize = payload.total_size;
    if (!evidence.packetDefaultsInitialized) {
      elements.evidencePacketOffset.value = "0";
      elements.evidencePacketLength.value = evidence.totalSize > 0 ? String(Math.min(4096, evidence.totalSize)) : "";
      evidence.packetDefaultsInitialized = true;
    }
    evidence.nextOffset = payload.next_offset;
    evidence.loadedBytes = (evidence.loadedBytes || 0) + payload.returned_size;
    const chunk = evidenceText(payload);
    elements.evidenceContent.textContent = append ? `${elements.evidenceContent.textContent}${chunk}` : chunk;
    const fidelity = payload.wire_fidelity || "unknown fidelity";
    const parse = payload.parse_state || "parse state unknown";
    const size = formatBytes(payload.total_size);
    const truncated = payload[`${evidence.side}_truncated`] === true ? " · stored message is marked truncated" : "";
    const capturedAt = payload.captured_at ? ` · captured ${payload.captured_at}` : " · capture time unknown";
    const digest = payload.sha256 ? ` · SHA-256 ${payload.sha256}` : " · SHA-256 unavailable";
    elements.evidenceMeta.textContent = `Revision ${evidence.revisionId || "unknown"}${capturedAt} · ${size} · ${fidelity} · parse ${parse}${truncated}${digest}`;
    updateEvidencePacketControl();
    const previewCapped = evidence.nextOffset !== null && evidence.loadedBytes >= MAX_EVIDENCE_PREVIEW_BYTES;
    elements.evidenceMore.hidden = evidence.nextOffset === null || previewCapped;
    elements.evidenceMore.disabled = evidence.nextOffset === null || previewCapped;
    elements.evidenceFoot.textContent = previewCapped
      ? `Local preview capped at ${formatBytes(MAX_EVIDENCE_PREVIEW_BYTES)}. The full stored message size and SHA-256 remain shown; no bytes were sent to a model.`
      : evidence.nextOffset === null
        ? "End of this stored message. The displayed bytes are not sent to a model."
        : `Showing bytes ${payload.offset}–${Number(payload.offset) + Number(payload.returned_size)} of ${payload.total_size}. Load one more bounded range to continue.`;
  } catch (error) {
    if (state.evidence !== evidence || requestID !== state.evidenceRequestId) return;
    elements.evidenceMeta.textContent = "The selected message range could not be read.";
    elements.evidenceContent.textContent = "";
    elements.evidencePacketPreview.disabled = true;
    elements.evidenceFoot.textContent = error.message;
    elements.evidenceMore.hidden = true;
    showToast(`HTTP evidence unavailable: ${error.message}`);
  } finally {
    if (state.evidence === evidence && requestID === state.evidenceRequestId) {
      evidence.loading = false;
      if (evidence.nextOffset !== null) elements.evidenceMore.disabled = false;
      updateEvidencePacketControl();
    }
  }
}

function validateEvidenceRangePayload(payload, evidence, requestedOffset) {
  if (!payload || String(payload.project_key ?? "") !== evidence.projectKey ||
      String(payload.request_id ?? "") !== evidence.requestId ||
      payload.revision_id !== evidence.revisionId || payload.side !== evidence.side ||
      payload.offset !== requestedOffset || !Number.isSafeInteger(payload.total_size) || payload.total_size < 0 ||
      !Number.isSafeInteger(payload.returned_size) || payload.returned_size < 0 ||
      payload.offset + payload.returned_size > payload.total_size) {
    throw new Error("Evidence response provenance or range metadata did not match the selected capture.");
  }
  const expectedNextOffset = payload.offset + payload.returned_size < payload.total_size
    ? payload.offset + payload.returned_size
    : null;
  if (payload.returned_size === 0 && payload.offset < payload.total_size || payload.next_offset !== expectedNextOffset) {
    throw new Error("Evidence response provenance or range metadata did not match the selected capture.");
  }
}

function selectEvidenceSide(side) {
  if (!state.evidence || (side !== "request" && side !== "response")) return;
  invalidateEvidencePacketPreview();
  state.evidence.requestID = ++state.evidenceRequestId;
  state.evidence.loading = false;
  state.evidence.side = side;
  state.evidence.nextOffset = 0;
  state.evidence.loadedBytes = 0;
  state.evidence.packetDefaultsInitialized = false;
  elements.evidencePacketOffset.value = "0";
  elements.evidencePacketLength.value = "";
  elements.evidencePacketContent.textContent = "";
  elements.evidencePacketContent.hidden = true;
  elements.evidencePacketPreview.disabled = true;
  setEvidenceSideButtons(side);
  elements.evidenceContent.textContent = "";
  elements.evidenceMore.hidden = true;
  elements.evidenceMeta.textContent = `Loading ${side} from the same pinned capture revision…`;
  return loadEvidenceRange(false);
}

function invalidateEvidencePacketPreview() {
  state.packetPreviewRequestId++;
  if (state.evidence) state.evidence.packetPreviewLoading = false;
  state.evidencePacketPreview = null;
  resetConsultationUI("Preview the selected evidence before creating a consultation.");
  elements.evidencePacketContent.textContent = "";
  elements.evidencePacketContent.hidden = true;
  updateEvidencePacketControl();
}

function renderEvidencePacketSelections() {
  const draft = state.evidencePacketDraft;
  const selections = draft.projectKey === state.projectKey ? draft.selections : [];
  const selectedBytes = selections.reduce((total, selection) => total + selection.length, 0);
  elements.evidencePacketSelectionCount.textContent = `${selections.length} selected spans · ${selectedBytes}/8192 bytes`;
  elements.evidencePacketSelectionList.replaceChildren();
  for (const [index, selection] of selections.entries()) {
    const row = el("div", "evidence-packet-selection");
    const label = el("span", "", `Request ${selection.request_id} · ${selection.side} · bytes ${selection.offset}–${selection.offset + selection.length}`);
    const remove = el("button", "tool-button", "Remove");
    remove.type = "button";
    remove.setAttribute("aria-label", `Remove request ${selection.request_id} ${selection.side} span`);
    remove.addEventListener("click", () => removeEvidencePacketSelection(index));
    row.append(label, remove);
    elements.evidencePacketSelectionList.append(row);
  }
  elements.evidencePacketClear.disabled = selections.length === 0 && !draft.question;
  updateEvidencePacketControl();
}

function updateEvidencePacketControl() {
  const evidence = state.evidence;
  const offset = Number(elements.evidencePacketOffset.value);
  const length = Number(elements.evidencePacketLength.value);
  const draft = state.evidencePacketDraft;
  const selections = draft.projectKey === state.projectKey ? draft.selections : [];
  const selectedBytes = selections.reduce((total, selection) => total + selection.length, 0);
  const validRevision = !!evidence && /^[a-f0-9]{64}$/.test(evidence.revisionId);
  const validSpan = validRevision && evidence.totalSize !== null && Number.isSafeInteger(offset) && offset >= 0 &&
    Number.isSafeInteger(length) && length > 0 && length <= 4096 && offset + length <= evidence.totalSize;
  const duplicate = validSpan && selections.some(selection => selection.request_id === evidence.requestId &&
    selection.revision_id === evidence.revisionId && selection.side === evidence.side &&
    selection.offset === offset && selection.length === length);
  elements.evidencePacketAdd.disabled = !validSpan || evidence.loading || selections.length >= 4 || selectedBytes + length > 8192 || duplicate;
  elements.evidencePacketPreview.disabled = !evidence || evidence.loading || evidence.packetPreviewLoading ||
    !draft.question.trim() || selections.length === 0;
  elements.consultationCreate.disabled = !state.evidencePacketPreview || evidence?.packetPreviewLoading;
  elements.consultationCreate.textContent = state.consultation ? "Reload saved review" : "Save session for review";
}

function newIdempotencyKey() {
  const bytes = crypto.getRandomValues(new Uint8Array(24));
  return [...bytes].map(value => value.toString(16).padStart(2, "0")).join("");
}

async function loadOperationAliases(projectKey) {
  const requestId = ++state.operationAliasRequestId;
  try {
    const aliases = await getJSON(`/api/consultation/operation-aliases?${new URLSearchParams({ project_key: projectKey })}`);
    if (requestId !== state.operationAliasRequestId || state.projectKey !== projectKey) return;
    state.operationAliases = new Map((Array.isArray(aliases) ? aliases : []).map(item => [item.operation_id, item]));
    if (state.graph) renderGraph(false);
    if (state.selectedNode?.kind === "operation") selectNode(state.selectedNode.id);
  } catch {
    if (requestId === state.operationAliasRequestId && state.projectKey === projectKey) state.operationAliases = new Map();
  }
}

async function saveOperationAlias(node, input, button) {
  const projectKey = state.projectKey;
  const alias = input.value.trim();
  if (!projectKey || node?.kind !== "operation" || !alias) {
    showToast("Enter a readable project alias first.");
    input.focus();
    return;
  }
  const current = state.operationAliases.get(node.id);
  button.disabled = true;
  try {
    const saved = await postJSON("/api/consultation/operation-alias", {
      project_key: projectKey, operation_id: node.id, expected_version: current?.version || 0,
      idempotency_key: newIdempotencyKey(), alias,
    });
    if (state.projectKey !== projectKey) return;
    state.operationAliases.set(node.id, saved);
    renderGraph(false);
    selectNode(node.id);
    showToast("Project alias saved.");
  } catch (error) {
    if (error.status === 409 && state.projectKey === projectKey) await loadOperationAliases(projectKey);
    showToast(error.message || "Could not save the project alias.");
  } finally {
    if (state.projectKey === projectKey) button.disabled = false;
  }
}

function resetConsultationUI(message) {
  state.consultationResumeRequestId++;
  state.consultation = null;
  state.consultationCreateKey = "";
  state.contextAnnotationKey = "";
  elements.consultationStatus.textContent = message;
  elements.consultationReview.hidden = true;
  elements.consultationReviewContent.textContent = "";
  elements.consultationContext.hidden = true;
  elements.consultationContextEvidence.replaceChildren();
  elements.consultationContextList.replaceChildren();
  elements.consultationContextLabel.value = "";
  elements.consultationContextSave.hidden = false;
  elements.consultationAccept.disabled = true;
  elements.consultationAccept.hidden = false;
  elements.consultationCopyID.hidden = true;
  elements.consultationCandidate.hidden = true;
  for (const field of candidateProposalFields()) field.value = "";
  elements.candidateEvidence.replaceChildren();
  elements.candidateComparisons.replaceChildren();
  elements.candidateRecoveryList.replaceChildren();
  elements.candidateList.replaceChildren();
  elements.candidateSave.disabled = true;
  elements.candidateStatus.textContent = message;
  elements.consultationCreate.disabled = true;
  elements.consultationCreate.textContent = "Save session for review";
  updateConsultationResumeControl();
  updateComparisonAttachmentControl();
}

function updateConsultationResumeControl() {
  elements.consultationOpen.disabled = !state.projectKey;
  elements.consultationResume.disabled = !state.projectKey || !elements.consultationResumeID.value.trim();
}

function openConsultationResume() {
  if (!state.projectKey) return;
  state.evidenceTrigger = elements.consultationOpen;
  if (!elements.evidenceDialog.open) elements.evidenceDialog.showModal();
  elements.consultationResumeID.focus();
}

function addCurrentEvidenceSpan() {
  updateEvidencePacketControl();
  if (!state.evidence || elements.evidencePacketAdd.disabled) return false;
  if (state.evidencePacketDraft.projectKey !== state.projectKey) {
    state.evidencePacketDraft = { projectKey: state.projectKey, question: elements.evidencePacketQuestion.value, selections: [] };
  }
  state.evidencePacketDraft.selections.push({
    request_id: state.evidence.requestId, revision_id: state.evidence.revisionId, side: state.evidence.side,
    offset: Number(elements.evidencePacketOffset.value), length: Number(elements.evidencePacketLength.value),
  });
  renderEvidencePacketSelections();
  invalidateEvidencePacketPreview();
  return true;
}

function removeEvidencePacketSelection(index) {
  const draft = state.evidencePacketDraft;
  if (draft.projectKey !== state.projectKey || !Number.isInteger(index) || index < 0 || index >= draft.selections.length) return false;
  draft.selections.splice(index, 1);
  renderEvidencePacketSelections();
  invalidateEvidencePacketPreview();
  return true;
}

function clearEvidencePacketDraft() {
  state.evidencePacketDraft = { projectKey: state.projectKey, question: "", selections: [] };
  elements.evidencePacketQuestion.value = "";
  elements.evidencePacketContent.textContent = "";
  elements.evidencePacketContent.hidden = true;
  renderEvidencePacketSelections();
  invalidateEvidencePacketPreview();
}

async function previewEvidencePacket() {
  const evidence = state.evidence;
  const draft = state.evidencePacketDraft;
  updateEvidencePacketControl();
  if (!evidence || elements.evidencePacketPreview.disabled) return;
  const previewRequestId = ++state.packetPreviewRequestId;
  const question = draft.question;
  const selections = draft.selections.map(selection => ({ ...selection }));
  const selectionsKey = JSON.stringify(selections);
  evidence.packetPreviewLoading = true;
  elements.evidencePacketPreview.disabled = true;
  elements.evidencePacketContent.hidden = false;
  elements.evidencePacketContent.textContent = "Building a redacted local preview…";
  try {
    const packet = await postJSON("/api/evidence/packet-preview", {
      project_key: evidence.projectKey, question,
      selections,
    });
    if (state.evidence !== evidence || previewRequestId !== state.packetPreviewRequestId || evidence.projectKey !== state.projectKey ||
        question !== state.evidencePacketDraft.question || selectionsKey !== JSON.stringify(state.evidencePacketDraft.selections)) return;
    state.evidencePacketPreview = { question, selections, projectKey: evidence.projectKey };
    resetConsultationUI("Review every redacted excerpt, then save the session for a separate operator review.");
    state.consultationCreateKey = newIdempotencyKey();
    elements.evidencePacketContent.textContent = JSON.stringify(packet, null, 2);
  } catch (error) {
    if (state.evidence !== evidence || previewRequestId !== state.packetPreviewRequestId) return;
    elements.evidencePacketContent.textContent = error.message;
    showToast(`Packet preview unavailable: ${error.message}`);
  } finally {
    if (state.evidence === evidence && previewRequestId === state.packetPreviewRequestId) {
      evidence.packetPreviewLoading = false;
      updateEvidencePacketControl();
    }
  }
}

async function createOrReviewConsultation() {
  const preview = state.evidencePacketPreview;
  if (!preview || preview.projectKey !== state.projectKey) return;
  const key = state.consultationCreateKey || (state.consultationCreateKey = newIdempotencyKey());
  elements.consultationCreate.disabled = true;
  elements.consultationStatus.textContent = "Saving the redacted packet locally…";
  try {
    if (!state.consultation) {
      const created = await postJSON("/api/consultation/session", {
        project_key: preview.projectKey, question: preview.question, selections: preview.selections,
        idempotency_key: key,
      });
      if (state.evidencePacketPreview !== preview || state.projectKey !== preview.projectKey) return;
      state.consultation = { id: created.consultation_id, version: created.version, accepted: false, reviewLoaded: false, acceptKey: newIdempotencyKey() };
      elements.consultationResumeID.value = created.consultation_id;
      updateConsultationResumeControl();
    }
    const session = state.consultation;
    const query = new URLSearchParams({ project_key: preview.projectKey, session_id: session.id });
    const review = await getJSON(`/api/consultation/review?${query}`);
    if (state.evidencePacketPreview !== preview || state.projectKey !== preview.projectKey || state.consultation !== session) return;
    displayConsultationReview(review, session);
  } catch (error) {
    elements.consultationStatus.textContent = `Could not load the saved review: ${error.message}`;
    elements.consultationCreate.disabled = false;
    if (state.consultation) elements.consultationCreate.textContent = "Reload saved review";
  }
}

function displayConsultationReview(review, session) {
  session.version = review.version;
  session.projectKey = state.projectKey;
  session.accepted = Boolean(review.review_accepted);
  session.reviewLoaded = true;
  session.contextAnnotations = Array.isArray(review.context_annotations) ? review.context_annotations : [];
  session.evidenceReferences = Array.isArray(review.evidence_references) ? review.evidence_references : [];
  session.comparisons = Array.isArray(review.comparisons) ? review.comparisons : [];
  session.proposals = Array.isArray(review.proposals) ? review.proposals : [];
  session.validationReceipts = Array.isArray(review.validation_receipts) ? review.validation_receipts : [];
  session.candidates = Array.isArray(review.candidates) ? review.candidates : [];
  if (state.comparison) delete state.comparison.attachError;
  elements.consultationContextLabel.value = "";
  elements.consultationReviewContent.textContent = JSON.stringify({
    consultation_id: review.consultation_id, version: review.version, review_accepted: Boolean(review.review_accepted),
    question: review.question, evidence: review.evidence, context_annotations: review.context_annotations || [],
    comparisons: review.comparisons || [],
  }, null, 2);
  renderConsultationContext(review, session.accepted);
  renderConsultationCandidate(session);
  elements.consultationReview.hidden = false;
  elements.consultationAccept.hidden = session.accepted;
  elements.consultationAccept.disabled = session.accepted;
  elements.consultationCopyID.hidden = false;
  elements.consultationStatus.textContent = session.accepted
    ? `Accepted · consultation ${session.id} · Hermes can now resume this session.`
    : `Saved locally · consultation ${session.id} · Hermes cannot read this packet until you accept it below.`;
  updateEvidencePacketControl();
  updateComparisonAttachmentControl();
  renderCurrentCaptureComparison();
}

async function resumeConsultationByID() {
  const projectKey = state.projectKey;
  const resumeInput = elements.consultationResumeID.value.trim();
  if (!projectKey) return;
  let sessionID;
  try {
    sessionID = consultationResumeSessionID(resumeInput, projectKey);
  } catch (error) {
    elements.consultationStatus.textContent = `Could not resume this consultation: ${error.message}`;
    return;
  }
  if (!sessionID) return;
  resetConsultationUI("Loading the saved consultation for operator review…");
  const requestID = ++state.consultationResumeRequestId;
  elements.consultationResume.disabled = true;
  try {
    const query = new URLSearchParams({ project_key: projectKey, session_id: sessionID });
    const review = await getJSON(`/api/consultation/review?${query}`);
    if (requestID !== state.consultationResumeRequestId || state.projectKey !== projectKey ||
        elements.consultationResumeID.value.trim() !== resumeInput) return;
    if (review.consultation_id !== sessionID) throw new Error("The saved review did not match that consultation ID.");
    const preview = { projectKey, question: review.question, selections: [] };
    const session = {
      id: review.consultation_id, projectKey, version: review.version, accepted: Boolean(review.review_accepted),
      reviewLoaded: true, acceptKey: newIdempotencyKey(), contextAnnotations: [],
    };
    state.evidencePacketPreview = preview;
    state.consultation = session;
    state.consultationCreateKey = "";
    elements.consultationResumeID.value = session.id;
    updateConsultationResumeControl();
    displayConsultationReview(review, session);
    elements.consultationStatus.textContent = session.accepted
      ? `Accepted · consultation ${session.id} reopened · Hermes can resume this session.`
      : `Saved locally · consultation ${session.id} reopened · review it before allowing Hermes access.`;
  } catch (error) {
    if (requestID !== state.consultationResumeRequestId || state.projectKey !== projectKey ||
        elements.consultationResumeID.value.trim() !== resumeInput) return;
    elements.consultationStatus.textContent = `Could not resume this consultation: ${error.message}`;
    elements.consultationReview.hidden = true;
  } finally {
    if (requestID === state.consultationResumeRequestId && state.projectKey === projectKey) {
      elements.consultationResume.disabled = !elements.consultationResumeID.value.trim();
    }
  }
}

function consultationResumeSessionID(value, projectKey) {
  let sessionID = String(value || "").trim();
  if (!sessionID) return "";
  if (sessionID.startsWith("{")) {
    let resumeDetails;
    try { resumeDetails = JSON.parse(sessionID); }
    catch { throw new Error("Paste a consultation ID or valid Hermes resume details."); }
    if (!resumeDetails || Array.isArray(resumeDetails) || typeof resumeDetails !== "object" ||
        typeof resumeDetails.project_key !== "string" || typeof resumeDetails.session_id !== "string") {
      throw new Error("Resume details must contain a project key and consultation ID.");
    }
    if (resumeDetails.project_key !== projectKey) {
      throw new Error("Those resume details belong to a different project. Select that project first.");
    }
    sessionID = resumeDetails.session_id.trim();
  }
  if (sessionID.length > 128) throw new Error("The consultation ID is too long.");
  return sessionID;
}

function renderConsultationContext(review, accepted) {
  elements.consultationContext.hidden = false;
  elements.consultationContextList.replaceChildren();
  state.contextEvidenceReferences = Array.isArray(review.evidence_references) ? review.evidence_references : [];
  state.contextEvidenceAccepted = accepted;
  state.contextFlowOrder = [];
  renderContextEvidenceOptions([]);
  const annotations = Array.isArray(review.context_annotations) ? review.context_annotations : [];
  for (const annotation of annotations) {
    const selectedIDs = Array.isArray(annotation.evidence_reference_ids)
      ? [...new Set(annotation.evidence_reference_ids.filter(id => typeof id === "string" && id.length > 0))]
      : [];
    const selectedCount = selectedIDs.length;
    const references = BBGraphFocus.contextAnnotationEvidenceReferences(
      annotation, review.evidence_references || [], review.evidence,
    );
    const unresolvedCount = Math.max(0, selectedCount - references.length);
    const item = el("details", "context-label-item");
    item.append(el("summary", "", `${annotation.kind}: ${annotation.label} · ${references.length} linked evidence item${references.length === 1 ? "" : "s"}${unresolvedCount ? ` · ${unresolvedCount} unresolved` : ""} · analyst-provided${annotation.kind === "flow" ? " · analyst-defined order; not observed causality" : ""}`));
    const referenceList = el("ul", "context-label-references");
    if (references.length) {
      for (const reference of references) {
        const row = el("li", "context-label-reference");
        const span = reference.spanStart !== null && reference.spanEnd !== null
          ? ` · bytes [${reference.spanStart}, ${reference.spanEnd})`
          : " · byte span unknown";
        const flowStep = reference.flowStep ? `Flow step ${reference.flowStep} · ` : "";
        row.append(el("div", "", `${flowStep}Evidence ${reference.evidenceIndex + 1} · ${reference.side} · Caido ${reference.requestId}${span}`));
        if (reference.available) {
          const excerpt = el("pre", "context-evidence-excerpt", reference.excerpt || "The selected redacted excerpt is empty.");
          excerpt.tabIndex = 0;
          excerpt.setAttribute("aria-label", `Redacted excerpt from evidence ${reference.evidenceIndex + 1}`);
          row.append(excerpt);
          if (reference.excerptTruncated) row.append(el("div", "", "Displayed preview is capped at 4 KiB."));
        } else {
          row.append(el("div", "", "The selected excerpt is unavailable in the saved evidence packet."));
        }
        referenceList.append(row);
      }
    }
    if (annotation.kind === "flow") referenceList.prepend?.(el("li", "", "analyst-defined order; not observed causality"));
    if (unresolvedCount) {
      referenceList.append(el("li", "", `${unresolvedCount} evidence reference${unresolvedCount === 1 ? "" : "s"} could not be resolved.`));
    } else if (!references.length) {
      referenceList.append(el("li", "", "Evidence links are unavailable for this label."));
    }
    item.append(referenceList);
    elements.consultationContextList.append(item);
  }
  if (!annotations.length) elements.consultationContextList.append(el("div", "context-label-item", "No analyst context labels yet."));
  elements.consultationContextKind.disabled = accepted;
  elements.consultationContextLabel.disabled = accepted;
  elements.consultationContextSave.hidden = accepted;
  updateContextAnnotationControl();
}

function updateContextAnnotationControl() {
  const hasSelectedEvidence = elements.consultationContextEvidence.querySelector("input:checked") !== null;
  elements.consultationContextSave.disabled = elements.consultationContextKind.disabled ||
    !elements.consultationContextLabel.value.trim() || !hasSelectedEvidence;
}

function selectedContextEvidenceReferenceIDs() {
  return [...elements.consultationContextEvidence.querySelectorAll("input:checked")].map(input => input.value);
}

function orderedContextEvidenceReferenceIDs(selectedIDs, evidenceReferences, kind, flowOrder = []) {
  const available = new Set((evidenceReferences || []).map(reference => reference?.evidence_reference_id)
    .filter(id => typeof id === "string" && id.length > 0));
  const selected = new Set((selectedIDs || []).filter(id => available.has(id)));
  const evidenceOrder = (evidenceReferences || []).map(reference => reference?.evidence_reference_id)
    .filter(id => selected.has(id));
  if (kind !== "flow") return evidenceOrder;
  const ordered = [...new Set((flowOrder || []).filter(id => selected.has(id)))];
  for (const id of evidenceOrder) if (!ordered.includes(id)) ordered.push(id);
  return ordered;
}

function renderContextEvidenceOptions(selectedIDs = selectedContextEvidenceReferenceIDs()) {
  elements.consultationContextEvidence.replaceChildren();
  const kind = elements.consultationContextKind.value;
  const order = orderedContextEvidenceReferenceIDs(selectedIDs, state.contextEvidenceReferences, kind, state.contextFlowOrder);
  if (kind === "flow") state.contextFlowOrder = order;
  const position = new Map(order.map((id, index) => [id, index]));
  for (const reference of state.contextEvidenceReferences) {
    const referenceID = reference?.evidence_reference_id;
    if (typeof referenceID !== "string" || !referenceID) continue;
    const row = el("div", "context-evidence-row");
    const label = el("label");
    const checkbox = el("input");
    checkbox.type = "checkbox";
    checkbox.value = referenceID;
    checkbox.checked = selectedIDs.includes(referenceID);
    checkbox.disabled = state.contextEvidenceAccepted;
    checkbox.addEventListener("change", () => {
      const selected = selectedContextEvidenceReferenceIDs();
      if (kind === "flow") state.contextFlowOrder = orderedContextEvidenceReferenceIDs(selected, state.contextEvidenceReferences, kind, state.contextFlowOrder);
      state.contextAnnotationKey = "";
      renderContextEvidenceOptions(selected);
      updateContextAnnotationControl();
    });
    const step = kind === "flow" && position.has(referenceID) ? `Flow step ${position.get(referenceID) + 1} · ` : "";
    label.append(checkbox, document.createTextNode(`${step}Evidence ${Number(reference.evidence_index) + 1} · ${reference.side} · Caido ${reference.request_id}`));
    row.append(label);
    if (kind === "flow") {
      for (const [direction, delta] of [["Move up", -1], ["Move down", 1]]) {
        const button = el("button", "context-order-button", direction);
        button.type = "button";
        button.setAttribute("aria-label", `${direction} flow evidence ${Number(reference.evidence_index) + 1}`);
        const index = position.get(referenceID);
        button.disabled = state.contextEvidenceAccepted || index === undefined || index + delta < 0 || index + delta >= order.length;
        button.addEventListener("click", () => moveContextEvidence(referenceID, delta));
        row.append(button);
      }
    }
    elements.consultationContextEvidence.append(row);
  }
  if (kind === "flow") elements.consultationContextEvidence.append(el("p", "context-flow-note", "analyst-defined order; not observed causality"));
}

function moveContextEvidence(referenceID, delta) {
  const selected = selectedContextEvidenceReferenceIDs();
  const order = orderedContextEvidenceReferenceIDs(selected, state.contextEvidenceReferences, "flow", state.contextFlowOrder);
  const index = order.indexOf(referenceID);
  const next = index + delta;
  if (index < 0 || next < 0 || next >= order.length) return;
  [order[index], order[next]] = [order[next], order[index]];
  state.contextFlowOrder = order;
  state.contextAnnotationKey = "";
  renderContextEvidenceOptions(selected);
  updateContextAnnotationControl();
}

async function saveContextAnnotation() {
  const preview = state.evidencePacketPreview;
  const session = state.consultation;
  if (!preview || !session || !session.reviewLoaded || session.accepted || preview.projectKey !== state.projectKey ||
      elements.consultationContextSave.disabled) return;
  const evidenceReferenceIDs = orderedContextEvidenceReferenceIDs(
    selectedContextEvidenceReferenceIDs(), state.contextEvidenceReferences,
    elements.consultationContextKind.value, state.contextFlowOrder,
  );
  state.contextAnnotationKey ||= newIdempotencyKey();
  const requestKey = state.contextAnnotationKey;
  elements.consultationContextSave.disabled = true;
  elements.consultationContextKind.disabled = true;
  elements.consultationContextLabel.disabled = true;
  for (const checkbox of elements.consultationContextEvidence.querySelectorAll("input")) checkbox.disabled = true;
  for (const button of elements.consultationContextEvidence.querySelectorAll("button")) button.disabled = true;
  elements.consultationAccept.disabled = true;
  elements.consultationStatus.textContent = "Saving analyst context to the local review…";
  let saved = false;
  try {
    await postJSON("/api/consultation/context-annotation", {
      project_key: preview.projectKey, session_id: session.id, expected_version: session.version,
      idempotency_key: requestKey, kind: elements.consultationContextKind.value,
      label: elements.consultationContextLabel.value, evidence_reference_ids: evidenceReferenceIDs,
    });
    saved = true;
    if (state.consultation !== session || state.projectKey !== preview.projectKey || state.evidencePacketPreview !== preview) return;
    const query = new URLSearchParams({ project_key: preview.projectKey, session_id: session.id });
    const review = await getJSON(`/api/consultation/review?${query}`);
    if (state.consultation !== session || state.projectKey !== preview.projectKey || state.evidencePacketPreview !== preview) return;
    session.version = review.version;
    session.contextAnnotations = Array.isArray(review.context_annotations) ? review.context_annotations : [];
    state.contextAnnotationKey = "";
    elements.consultationContextLabel.value = "";
    elements.consultationReviewContent.textContent = JSON.stringify({
      consultation_id: review.consultation_id, version: review.version,
      question: review.question, evidence: review.evidence, context_annotations: review.context_annotations || [],
    }, null, 2);
    renderConsultationContext(review, false);
    elements.consultationAccept.disabled = false;
    elements.consultationStatus.textContent = `Saved locally · ${session.contextAnnotations.length} analyst context label${session.contextAnnotations.length === 1 ? "" : "s"} included in the review.`;
  } catch (error) {
    elements.consultationStatus.textContent = saved
      ? `Context was saved, but the review could not reload: ${error.message}. Reload the saved review before continuing.`
      : `Context label was not saved: ${error.message}`;
    elements.consultationContextKind.disabled = false;
    elements.consultationContextLabel.disabled = false;
    renderContextEvidenceOptions(selectedContextEvidenceReferenceIDs());
    elements.consultationAccept.disabled = false;
    updateContextAnnotationControl();
    updateEvidencePacketControl();
  }
}

async function acceptConsultationReview() {
  const preview = state.evidencePacketPreview;
  const session = state.consultation;
  if (!preview || !session || !session.reviewLoaded || session.accepted || preview.projectKey !== state.projectKey) return;
  elements.consultationAccept.disabled = true;
  elements.consultationStatus.textContent = "Recording your approval for Hermes to read this saved packet…";
  try {
    const accepted = await postJSON("/api/consultation/accept-review", {
      project_key: preview.projectKey, session_id: session.id, expected_version: session.version,
      idempotency_key: session.acceptKey,
    });
    if (state.consultation !== session || state.projectKey !== preview.projectKey) return;
    session.version = accepted.version;
    session.accepted = Boolean(accepted.review_accepted);
    state.contextEvidenceAccepted = session.accepted;
    elements.consultationAccept.hidden = true;
    elements.consultationContextKind.disabled = true;
    elements.consultationContextLabel.disabled = true;
    for (const checkbox of elements.consultationContextEvidence.querySelectorAll("input")) checkbox.disabled = true;
    for (const button of elements.consultationContextEvidence.querySelectorAll("button")) button.disabled = true;
    elements.consultationContextSave.hidden = true;
    elements.consultationStatus.textContent = `Accepted · consultation ${session.id} · Hermes can now resume this session.`;
    renderConsultationCandidate(session);
    updateComparisonAttachmentControl();
    renderCurrentCaptureComparison();
  } catch (error) {
    elements.consultationStatus.textContent = `Approval was not recorded: ${error.message}`;
    elements.consultationAccept.disabled = false;
    updateEvidencePacketControl();
  }
}

function candidateProposalFields() {
  return [elements.candidateTitle, elements.candidateImpact, elements.candidateSummary,
    elements.candidateRationale, elements.candidateAlternatives, elements.candidateNextChecks,
    elements.candidateLimitations];
}

function validCandidateReferenceID(value) {
  return typeof value === "string" && /^[A-Za-z0-9_-]{43}$/.test(value);
}

function verifiedCandidateReferences(session) {
  const seen = new Set();
  return (Array.isArray(session?.evidenceReferences) ? session.evidenceReferences : []).filter(reference => {
    const id = reference?.evidence_reference_id;
    if (!validCandidateReferenceID(id) || seen.has(id) || reference.source_integrity_status !== "verified" ||
        !/^[a-f0-9]{64}$/.test(String(reference.sha256 || ""))) return false;
    seen.add(id);
    return true;
  });
}

function consultationCandidateComparisons(session) {
  const seen = new Set();
  return (Array.isArray(session?.comparisons) ? session.comparisons : []).filter(record => {
    const id = record?.comparison_id;
    if (!validCandidateReferenceID(id) || seen.has(id)) return false;
    seen.add(id);
    return true;
  });
}

function renderCandidateChoices(container, entries, getID, getLabel) {
  container.replaceChildren();
  for (const entry of entries) {
    const label = el("label");
    const checkbox = el("input");
    checkbox.type = "checkbox";
    checkbox.value = getID(entry);
    checkbox.addEventListener("change", updateCandidateControl);
    label.append(checkbox, document.createTextNode(getLabel(entry)));
    container.append(label);
  }
  if (!entries.length) container.append(el("span", "", "No eligible items in this consultation."));
}

function checkedCandidateIDs(container) {
  return [...(container.children || [])].map(row => row.children?.[0])
    .filter(input => input?.type === "checkbox" && input.checked).map(input => input.value);
}

function candidateLines(field, maximumCount, maximumLength) {
  const values = field.value.split(/\r?\n/).map(value => value.trim()).filter(Boolean);
  if (values.length > maximumCount || values.some(value => value.length > maximumLength)) return null;
  return values;
}

function candidateProposalFromInputs() {
  const alternatives = candidateLines(elements.candidateAlternatives, 5, 1024);
  const nextChecks = candidateLines(elements.candidateNextChecks, 8, 512);
  const limitations = candidateLines(elements.candidateLimitations, 8, 512);
  if (alternatives === null || nextChecks === null || limitations === null) return null;
  const proposal = {
    title: elements.candidateTitle.value.trim(), summary: elements.candidateSummary.value.trim(),
    rationale: elements.candidateRationale.value.trim(), impact: elements.candidateImpact.value.trim(),
    alternatives, next_checks: nextChecks, limitations,
  };
  if (!proposal.title || proposal.title.length > 160 || !proposal.summary || proposal.summary.length > 2048 ||
      !proposal.rationale || proposal.rationale.length > 4096 || proposal.impact.length > 1024) return null;
  return proposal;
}

function sameCandidateIDs(left, right) {
  return Array.isArray(left) && Array.isArray(right) && left.length === right.length &&
    left.every((id, index) => id === right[index]);
}

function matchingCandidateReceipt(session, proposal) {
  return (session?.validationReceipts || []).find(receipt => receipt?.status === "references_validated" &&
    receipt.proposal_id === proposal?.proposal_id && receipt.proposal_digest === proposal?.proposal_digest &&
    sameCandidateIDs(receipt.evidence_reference_ids, proposal.evidence_reference_ids) &&
    sameCandidateIDs(receipt.comparison_ids, proposal.comparison_ids));
}

function candidateProposalIsBoundToSession(session, proposal) {
  const referenceIDs = Array.isArray(proposal?.evidence_reference_ids) ? proposal.evidence_reference_ids : [];
  const comparisonIDs = Array.isArray(proposal?.comparison_ids) ? proposal.comparison_ids : [];
  const references = new Set(verifiedCandidateReferences(session).map(reference => reference.evidence_reference_id));
  const comparisons = new Set(consultationCandidateComparisons(session).map(record => record.comparison_id));
  return referenceIDs.length > 0 && referenceIDs.every(id => references.has(id)) && comparisonIDs.every(id => comparisons.has(id));
}

function appendCandidateRecord(records, record, idField) {
  const existing = Array.isArray(records) ? records : [];
  return [...existing.filter(item => item?.[idField] !== record?.[idField]), record];
}

function renderConsultationCandidate(session) {
  elements.consultationCandidate.hidden = !session?.reviewLoaded;
  const references = verifiedCandidateReferences(session);
  const comparisons = consultationCandidateComparisons(session);
  renderCandidateChoices(elements.candidateEvidence, references, reference => reference.evidence_reference_id, reference =>
    `Evidence ${Number(reference.evidence_index) + 1} · ${reference.side} · ${reference.evidence_reference_id}`);
  renderCandidateChoices(elements.candidateComparisons, comparisons, record => record.comparison_id, record =>
    `Comparison · ${record.comparison_id}`);

  elements.candidateRecoveryList.replaceChildren();
  const proposals = Array.isArray(session?.proposals) ? session.proposals : [];
  const candidates = Array.isArray(session?.candidates) ? session.candidates : [];
  if (!proposals.length) {
    elements.candidateRecoveryList.append(el("div", "context-label-item", "No saved proposals or validation receipts in this consultation."));
  }
  for (const proposal of proposals) {
    const item = el("div", "context-label-item candidate-record");
    item.append(el("strong", "", `Saved proposal ${proposal.proposal_id} · ${proposal.evidence_reference_ids?.length || 0} evidence references · ${proposal.comparison_ids?.length || 0} comparisons`));
    const receipt = matchingCandidateReceipt(session, proposal);
    if (receipt) item.append(el("div", "", `Matching valid receipt: ${receipt.receipt_id}`));
    const savedCandidate = candidates.find(candidate => candidate.proposal_id === proposal.proposal_id && candidate.state === "candidate");
    if (savedCandidate) {
      item.append(el("div", "", `Already saved as candidate ${savedCandidate.candidate_id}.`));
    } else {
      const continueButton = el("button", "tool-button recovery-continue", receipt
        ? "Save candidate from validated proposal" : "Validate proposal and save candidate");
      continueButton.type = "button";
      continueButton.candidateEligible = candidateProposalIsBoundToSession(session, proposal);
      continueButton.disabled = !continueButton.candidateEligible;
      continueButton.addEventListener("click", () => { void continueConsultationCandidateProposal(proposal.proposal_id); });
      item.append(continueButton);
      if (!continueButton.candidateEligible) item.append(el("div", "", "This proposal includes references outside the currently verified session evidence."));
    }
    elements.candidateRecoveryList.append(item);
  }

  elements.candidateList.replaceChildren();
  if (!candidates.length) {
    elements.candidateList.append(el("div", "context-label-item", "No saved candidates in this consultation."));
  }
  for (const candidate of candidates) {
    const item = el("div", "context-label-item candidate-record");
    item.append(el("strong", "", `State: ${candidate.state} · ${candidate.candidate_id} · version ${candidate.version}`));
    item.append(el("div", "", `Evidence reference IDs: ${(candidate.citations || []).map(citation => citation.evidence_reference_id).join(", ") || "none"}`));
    item.append(el("div", "", `Comparison IDs: ${(candidate.comparison_ids || []).join(", ") || "none"}`));
    elements.candidateList.append(item);
  }
  updateCandidateControl();
}

function updateCandidateControl() {
  const session = state.consultation;
  const current = Boolean(session && session.projectKey === state.projectKey &&
    state.evidencePacketPreview?.projectKey === state.projectKey && session.reviewLoaded);
  const canEdit = current && session.accepted && !session.candidateWorkflowBusy;
  for (const field of candidateProposalFields()) field.disabled = !canEdit;
  for (const container of [elements.candidateEvidence, elements.candidateComparisons]) {
    for (const row of container.children || []) {
      const checkbox = row.children?.[0];
      if (checkbox?.type === "checkbox") checkbox.disabled = !canEdit;
    }
  }
  for (const item of elements.candidateRecoveryList.children || []) {
    for (const child of item.children || []) {
      if (child.className === "tool-button recovery-continue") child.disabled = !canEdit || !child.candidateEligible;
    }
  }
  const validReferences = new Set(verifiedCandidateReferences(session).map(reference => reference.evidence_reference_id));
  const validComparisons = new Set(consultationCandidateComparisons(session).map(record => record.comparison_id));
  const selectedReferences = checkedCandidateIDs(elements.candidateEvidence);
  const selectedComparisons = checkedCandidateIDs(elements.candidateComparisons);
  const validSelection = selectedReferences.length > 0 && new Set(selectedReferences).size === selectedReferences.length &&
    selectedReferences.every(id => validReferences.has(id)) && new Set(selectedComparisons).size === selectedComparisons.length &&
    selectedComparisons.every(id => validComparisons.has(id));
  const proposal = candidateProposalFromInputs();
  elements.candidateSave.disabled = !canEdit || !validSelection || !proposal;

  if (!session || session.projectKey !== state.projectKey) {
    elements.candidateStatus.textContent = "Resume a consultation in the selected project before creating a candidate.";
  } else if (!session.reviewLoaded) {
    elements.candidateStatus.textContent = session.candidateStatus || "Session state is stale. Resume this consultation before continuing.";
  } else if (!session.accepted) {
    elements.candidateStatus.textContent = "Accept the operator review before creating a candidate.";
  } else if (session.candidateWorkflowBusy) {
    elements.candidateStatus.textContent = "Validating citations and saving the candidate locally…";
  } else if (!validReferences.size) {
    elements.candidateStatus.textContent = "This consultation has no verified evidence references available for a candidate.";
  } else {
    elements.candidateStatus.textContent = session.candidateStatus ||
      "Select at least one verified evidence reference and complete the required fields. A candidate is not a confirmed finding.";
  }
}

async function runCandidateWorkflow(session, projectKey, { proposal: proposalDraft, evidenceReferenceIDs, comparisonIDs, existingProposal, existingReceipt }) {
  if (!session || session.projectKey !== projectKey || !session.reviewLoaded || !session.accepted || session.candidateWorkflowBusy) return;
  session.candidateWorkflowBusy = true;
  session.candidateStatus = "";
  updateCandidateControl();
  try {
    let proposal = existingProposal;
    if (!proposal) {
      proposal = await postJSON("/api/consultation/proposal", {
        project_key: projectKey, session_id: session.id, expected_version: session.version,
        idempotency_key: session.proposalKey || (session.proposalKey = newIdempotencyKey()),
        proposal: proposalDraft, evidence_reference_ids: evidenceReferenceIDs, comparison_ids: comparisonIDs,
      });
      if (state.consultation !== session || state.projectKey !== projectKey) return;
      session.version = proposal.session_version;
      session.proposals = appendCandidateRecord(session.proposals, proposal, "proposal_id");
    }

    let receipt = existingReceipt;
    if (!receipt) {
      receipt = await postJSON("/api/consultation/validate", {
        project_key: projectKey, session_id: session.id, expected_version: session.version,
        idempotency_key: session.validationKey || (session.validationKey = newIdempotencyKey()),
        proposal_id: proposal.proposal_id,
      });
      if (state.consultation !== session || state.projectKey !== projectKey) return;
      if (receipt.status !== "references_validated") throw new Error("The server did not validate the candidate references.");
      session.version = receipt.session_version;
      session.validationReceipts = appendCandidateRecord(session.validationReceipts, receipt, "receipt_id");
    }

    const candidate = await postJSON("/api/consultation/candidate", {
      project_key: projectKey, session_id: session.id, expected_version: session.version,
      idempotency_key: session.candidateKey || (session.candidateKey = newIdempotencyKey()),
      proposal_id: proposal.proposal_id, receipt_id: receipt.receipt_id,
    });
    if (state.consultation !== session || state.projectKey !== projectKey) return;
    if (candidate.state !== "candidate") throw new Error("The saved record did not return the candidate state.");
    session.version = candidate.session_version;
    session.candidates = appendCandidateRecord(session.candidates, candidate, "candidate_id");
    session.proposalKey = "";
    session.validationKey = "";
    session.candidateKey = "";
    for (const field of candidateProposalFields()) field.value = "";
    session.candidateStatus = `Saved candidate ${candidate.candidate_id} · state candidate. This does not confirm impact or a vulnerability.`;
    renderConsultationCandidate(session);
  } catch (error) {
    if (state.consultation !== session || state.projectKey !== projectKey) return;
    session.reviewLoaded = false;
    session.proposalKey = "";
    session.validationKey = "";
    session.candidateKey = "";
    session.candidateStatus = error.status === 409
      ? "The session version changed. Resume this consultation to refresh it before continuing."
      : `Candidate workflow stopped: ${error.message}. Resume the consultation before retrying.`;
  } finally {
    if (state.consultation === session && state.projectKey === projectKey) {
      session.candidateWorkflowBusy = false;
      updateCandidateControl();
    }
  }
}

async function saveConsultationCandidate() {
  const session = state.consultation;
  const projectKey = state.projectKey;
  if (!session || session.projectKey !== projectKey || !session.reviewLoaded || !session.accepted ||
      elements.candidateSave.disabled) return;
  const proposal = candidateProposalFromInputs();
  const evidenceReferenceIDs = checkedCandidateIDs(elements.candidateEvidence);
  const comparisonIDs = checkedCandidateIDs(elements.candidateComparisons);
  const availableReferences = new Set(verifiedCandidateReferences(session).map(reference => reference.evidence_reference_id));
  const availableComparisons = new Set(consultationCandidateComparisons(session).map(record => record.comparison_id));
  if (!proposal || evidenceReferenceIDs.length === 0 || evidenceReferenceIDs.some(id => !availableReferences.has(id)) ||
      comparisonIDs.some(id => !availableComparisons.has(id))) return;
  return runCandidateWorkflow(session, projectKey, { proposal, evidenceReferenceIDs, comparisonIDs });
}

async function continueConsultationCandidateProposal(proposalID) {
  const session = state.consultation;
  const projectKey = state.projectKey;
  if (!session || session.projectKey !== projectKey || !session.reviewLoaded || !session.accepted || session.candidateWorkflowBusy) return;
  const proposal = (session.proposals || []).find(item => item?.proposal_id === proposalID);
  if (!proposal || !candidateProposalIsBoundToSession(session, proposal) ||
      (session.candidates || []).some(candidate => candidate.proposal_id === proposalID && candidate.state === "candidate")) return;
  const receipt = matchingCandidateReceipt(session, proposal);
  return runCandidateWorkflow(session, projectKey, { existingProposal: proposal, existingReceipt: receipt });
}

function comparisonIsReferencedBySession(session, comparison) {
  const references = Array.isArray(session?.evidenceReferences) ? session.evidenceReferences : [];
  const hasExactVerifiedRevision = (requestId, revisionId) => references.some(reference =>
    reference?.request_id === requestId && reference?.revision_id === revisionId &&
    reference?.source_integrity_status === "verified" && /^[a-f0-9]{64}$/.test(String(reference?.sha256 || "")));
  return hasExactVerifiedRevision(comparison.leftRequestId, comparison.leftRevisionId) &&
    hasExactVerifiedRevision(comparison.rightRequestId, comparison.rightRevisionId);
}

function updateComparisonAttachmentControl() {
  const comparison = state.comparison;
  const session = state.consultation;
  const visible = Boolean(comparison?.result);
  elements.comparisonAttach.hidden = !visible;
  if (!visible) {
    elements.comparisonAttachButton.disabled = true;
    elements.comparisonAttachStatus.textContent = "";
    return;
  }
  const stored = comparison.attachedComparisonId || (session?.comparisons || []).find(record =>
    record?.result?.left_request_id === comparison.leftRequestId &&
    record?.result?.left_revision_id === comparison.leftRevisionId &&
    record?.result?.right_request_id === comparison.rightRequestId &&
    record?.result?.right_revision_id === comparison.rightRevisionId)?.comparison_id;
  if (stored) {
    comparison.attachedComparisonId = stored;
    elements.comparisonAttachButton.disabled = true;
    elements.comparisonAttachStatus.textContent = `Saved in consultation ${session?.id || ""} as ${stored}. Hermes receives the bounded comparison metadata, not the displayed raw excerpts.`;
    return;
  }
  if (!session || !session.reviewLoaded) {
    elements.comparisonAttachButton.disabled = true;
    elements.comparisonAttachStatus.textContent = comparison.attachError || "Resume an accepted consultation for this project. Both exact captures must be included in its evidence packet.";
    return;
  }
  if (session.projectKey !== state.projectKey || !session.accepted) {
    elements.comparisonAttachButton.disabled = true;
    elements.comparisonAttachStatus.textContent = "Review and accept this project's saved consultation before attaching a comparison.";
    return;
  }
  if (!comparisonIsReferencedBySession(session, comparison)) {
    elements.comparisonAttachButton.disabled = true;
    elements.comparisonAttachStatus.textContent = "This session does not contain verified references to both exact capture revisions.";
    return;
  }
  elements.comparisonAttachButton.disabled = false;
  elements.comparisonAttachStatus.textContent = comparison.attachError || `Ready to save this exact comparison to accepted consultation ${session.id}.`;
}

async function attachComparisonToConsultation() {
  const comparison = state.comparison;
  const session = state.consultation;
  if (!comparison?.result || !session || elements.comparisonAttachButton.disabled) return;
  const projectKey = state.projectKey;
  const comparisonKey = session.comparisonKey || (session.comparisonKey = newIdempotencyKey());
  elements.comparisonAttachButton.disabled = true;
  elements.comparisonAttachStatus.textContent = "Saving the bounded comparison metadata to the accepted consultation…";
  try {
    const record = await postJSON("/api/consultation/compare", {
      project_key: projectKey, session_id: session.id, expected_version: session.version,
      idempotency_key: comparisonKey,
      left_request_id: comparison.leftRequestId, left_revision_id: comparison.leftRevisionId,
      right_request_id: comparison.rightRequestId, right_revision_id: comparison.rightRevisionId,
    });
    if (state.comparison !== comparison || state.consultation !== session || state.projectKey !== projectKey) return;
    session.version = record.session_version;
    session.comparisonKey = "";
    session.comparisons = [...(session.comparisons || []), record];
    comparison.attachedComparisonId = record.comparison_id;
    delete comparison.attachError;
    renderConsultationCandidate(session);
    updateComparisonAttachmentControl();
  } catch (error) {
    if (state.comparison !== comparison || state.consultation !== session || state.projectKey !== projectKey) return;
    comparison.attachError = error.status === 409
      ? "The saved consultation changed. Resume it again to refresh the version before retrying."
      : `Comparison was not saved: ${error.message}`;
    if (error.status === 409) {
      session.reviewLoaded = false;
      session.comparisonKey = "";
    }
    updateComparisonAttachmentControl();
  }
}

function setComparisonBaseline(item, button) {
  const requestId = String(item?.request_id ?? "");
  const revisionId = String(item?.revision_id ?? "");
  if (!state.projectKey || !requestId || requestId.length > 512 || !/^[a-f0-9]{64}$/.test(revisionId) ||
      item?.record_source !== "encrypted_local_capture" || item?.raw_stored_locally !== true) {
    showToast("An exact local historical revision is required as the comparison baseline.");
    return;
  }
  state.comparisonBaseline = {
    projectKey: state.projectKey, requestId, revisionId,
    graphObservationID: String(item?.graph_observation_id || ""),
  };
  if (state.selectedNode?.kind === "observation") {
    renderInspector(state.selectedNode);
  } else {
    button.textContent = "Current baseline";
    button.disabled = true;
    const comparisonButton = button.parentElement?.querySelector(".capture-compare");
    if (comparisonButton) comparisonButton.disabled = true;
  }
  showToast(`Comparison baseline set to request ${requestId}.`);
}

async function openComparison(item, trigger = null) {
  const baseline = state.comparisonBaseline;
  const rightRequestId = String(item?.request_id ?? "");
  const rightRevisionId = String(item?.revision_id ?? "");
  if (!baseline || baseline.projectKey !== state.projectKey || !rightRequestId ||
      !/^[a-f0-9]{64}$/.test(rightRevisionId) || item?.record_source !== "encrypted_local_capture" || item?.raw_stored_locally !== true) {
    showToast("Choose two exact local historical revisions in the same project.");
    return;
  }
  if (baseline.requestId === rightRequestId && baseline.revisionId === rightRevisionId) {
    showToast("Choose a different capture from the comparison baseline.");
    return;
  }
  clearEvidence(false, true);
  const comparison = {
    projectKey: state.projectKey, leftRequestId: baseline.requestId, leftRevisionId: baseline.revisionId,
    leftGraphObservationID: baseline.graphObservationID || "",
    rightRequestId, rightRevisionId, rightGraphObservationID: String(item?.graph_observation_id || ""),
  };
  state.comparison = comparison;
  updateComparisonAttachmentControl();
  const requestID = ++state.evidenceRequestId;
  state.evidenceTrigger = trigger;
  elements.evidenceTitle.textContent = "Capture comparison";
  state.packetPreviewRequestId++;
  elements.evidencePacketControls.hidden = true;
  elements.evidencePacketContent.textContent = "";
  elements.evidencePacketContent.hidden = true;
  elements.evidenceLabel.textContent = `Baseline request ${baseline.requestId} vs request ${rightRequestId}`;
  elements.evidenceMeta.textContent = "Comparing exact local historical revisions…";
  elements.evidenceContent.textContent = "";
  elements.evidenceContent.hidden = true;
  elements.evidenceCompareContent.hidden = false;
  elements.evidenceCompareContent.textContent = "";
  elements.evidenceSides.hidden = true;
  elements.evidenceMore.hidden = true;
  elements.evidenceFoot.textContent = "Loading deterministic request and response byte comparisons. No data is sent to a model.";
  if (!elements.evidenceDialog.open) elements.evidenceDialog.showModal();

  const query = new URLSearchParams({
    project_key: comparison.projectKey, left_request_id: comparison.leftRequestId,
    left_revision_id: comparison.leftRevisionId, right_request_id: comparison.rightRequestId,
    right_revision_id: comparison.rightRevisionId,
  });
  try {
    const result = await getJSON(`/api/compare?${query}`);
    if (state.comparison !== comparison || requestID !== state.evidenceRequestId || comparison.projectKey !== state.projectKey) return;
    comparison.result = result;
    elements.evidenceMeta.textContent = `Exact revisions · ${comparison.leftRevisionId} → ${comparison.rightRevisionId}`;
    renderCurrentCaptureComparison();
    elements.evidenceFoot.textContent = result.coverage_note || "Exact local byte comparison; unavailable or incomplete messages are identified per side.";
    updateComparisonAttachmentControl();
  } catch (error) {
    if (state.comparison !== comparison || requestID !== state.evidenceRequestId) return;
    elements.evidenceMeta.textContent = "The selected revisions could not be compared.";
    elements.evidenceCompareContent.textContent = "";
    elements.evidenceFoot.textContent = error.message;
    showToast(`Capture comparison unavailable: ${error.message}`);
  }
}

function renderCurrentCaptureComparison() {
  const comparison = state.comparison;
  if (!comparison?.result || comparison.projectKey !== state.projectKey) return;
  elements.evidenceCompareContent.textContent = formatCaptureComparison(comparison.result, comparison);
}

function acceptedComparisonContextForSide(comparison, side) {
  const isLeft = side === "left";
  const graphObservationID = isLeft ? comparison.leftGraphObservationID : comparison.rightGraphObservationID;
  const requestId = isLeft ? comparison.leftRequestId : comparison.rightRequestId;
  const revisionId = isLeft ? comparison.leftRevisionId : comparison.rightRevisionId;
  if (!graphObservationID || !requestId || !revisionId) return [];
  const node = { id: graphObservationID, kind: "observation" };
  const provenance = (state.graph?.capture_provenance || []).filter(item =>
    item?.graph_observation_id === graphObservationID && String(item.request_id) === requestId &&
    item.revision_id === revisionId);
  return BBGraphFocus.contextAnnotationsForObservation(node, provenance, state.consultation, comparison.projectKey);
}

function formatComparisonAnalystContext(comparison, side, title) {
  const annotations = acceptedComparisonContextForSide(comparison, side);
  const lines = [`${title} ANALYST CONTEXT`];
  if (!annotations.length) {
    lines.push("No analyst-provided context is linked to this exact revision in the accepted consultation.");
    return lines.join("\n");
  }
  for (const annotation of annotations) {
    const annotationVersion = annotation.annotationVersion === null ? "unknown" : String(annotation.annotationVersion);
    lines.push(`${annotation.kind}: ${annotation.label} · analyst-provided · consultation ${annotation.sessionID} · accepted version ${annotation.sessionVersion} · annotation version ${annotationVersion}`);
    if (annotation.kind === "flow") lines.push("analyst-defined order; not observed causality");
    for (const reference of annotation.evidenceReferences) {
      lines.push(`${reference.flowStep ? `Flow step ${reference.flowStep} · ` : ""}Evidence ${reference.evidenceIndex + 1} · ${reference.side} · Caido request ${reference.requestId} · revision ${reference.revisionId} · reference ${reference.referenceID}`);
    }
  }
  return lines.join("\n");
}

function formatCaptureComparison(result, comparison = state.comparison) {
  const renderRevision = (label, revision) => {
    if (!revision?.sha256) return null;
    const truncation = revision.truncated === true ? "yes" : revision.truncated === false ? "no" : "unknown";
    return `${label} · request ${revision.request_id || "unknown"} · revision ${revision.revision_id || "unknown"} · ${formatBytes(revision.size)} · SHA-256 ${revision.sha256} · ${revision.wire_fidelity || "fidelity unknown"} · parse ${readableParseState(revision.parse_state || "unknown")} · truncated ${truncation}${revision.captured_at ? ` · captured ${revision.captured_at}` : " · capture time unknown"}`;
  };
  const renderSide = (name, side) => {
    const lines = [`${name.toUpperCase()}`];
    if (!side?.available) {
      lines.push(`Not compared · ${side?.reason || "exact bytes unavailable"}`);
    } else if (side.equal) {
      lines.push(`Identical stored bytes · ${formatBytes(side.compared_bytes)} compared`);
    } else {
      lines.push(`Different · first byte difference at offset ${side.first_difference_offset} · ${formatBytes(side.compared_bytes)} compared`);
      lines.push(`Baseline context · ${side.left_excerpt}`);
      lines.push(`Selected context · ${side.right_excerpt}`);
    }
    const left = renderRevision("Baseline", side?.left);
    const right = renderRevision("Selected", side?.right);
    if (left) lines.push(left);
    if (right) lines.push(right);
    return lines.join("\n");
  };
  const sections = [renderSide("Request", result.request), renderSide("Response", result.response)];
  const session = state.consultation;
  if (comparison?.projectKey === state.projectKey && session?.projectKey === state.projectKey &&
      session.accepted === true && session.reviewLoaded === true) {
    sections.push(
      formatComparisonAnalystContext(comparison, "left", "BASELINE (A)"),
      formatComparisonAnalystContext(comparison, "right", "SELECTED (B)"),
    );
  }
  return sections.join("\n\n");
}

async function loadProjects(append = false) {
  elements.project.disabled = true;
  if (!append) {
    setStatus("Loading local projects", "loading");
    renderEmpty("Loading the local catalog", "Checking projects registered on this device.");
    elements.endpointList.replaceChildren(emptyList("Loading…", "Reading the local project registry."));
  }
  const selectedKey = state.projectKey;
  try {
    const query = new URLSearchParams({ limit: "200" });
    if (append && state.projectCursor) query.set("after_project_key", state.projectCursor);
    const data = await getJSON(`/api/projects?${query}`);
    const page = Array.isArray(data.projects) ? data.projects : [];
    state.projects = append ? [...state.projects, ...page] : page;
    state.projectCursor = data.next_after_project_key || null;
    elements.project.replaceChildren();
    if (!state.projects.length) {
      state.projectCursor = null;
      elements.project.append(new Option("No local projects", ""));
      elements.project.disabled = true;
      clearProjectView();
      elements.endpointCount.textContent = "0";
      elements.surfaceFoot.textContent = "Local index is empty";
      elements.endpointList.replaceChildren(emptyList("No data", "The local project registry is empty."));
      elements.coverage.textContent = "Coverage: no project";
      setStatus("No registered projects");
      return;
    }
    for (const item of state.projects) {
      const name = item.display_name || `Caido · ${short(item.project_id || item.project_key, 18)}`;
      elements.project.append(new Option(name, item.project_key));
    }
    if (state.projectCursor) elements.project.append(new Option("Load more projects…", "__load_more__"));
    elements.project.disabled = false;
    if (append) {
      elements.project.value = selectedKey;
      return;
    }
    await selectProject(state.projects[0].project_key);
  } catch (error) {
    if (append) {
      elements.project.disabled = false;
      elements.project.value = selectedKey;
      showToast(`Could not load more projects: ${error.message}`);
      return;
    }
    elements.project.replaceChildren(new Option("Loading error", ""));
    elements.project.disabled = true;
    clearProjectView();
    renderEmpty("Could not open the local catalog", "Check project database compatibility and refresh the page.");
    elements.endpointList.replaceChildren(emptyList("Read error", error.message));
    elements.coverage.textContent = "Coverage: unavailable";
    setStatus("Project read error", "error");
  }
}

async function loadRuntimeCapabilities() {
  try {
    const capabilities = await getJSON("/api/runtime-capabilities");
    const presentation = BBRuntimeCapabilities.evidenceVaultPresentation(capabilities.evidence_vault_key_state);
    elements.evidenceVaultStatus.textContent = presentation.label;
    elements.evidenceVaultStatus.title = presentation.title;
    elements.evidenceVaultStatus.dataset.state = presentation.state;
  } catch {
    const presentation = BBRuntimeCapabilities.evidenceVaultPresentation("unknown");
    elements.evidenceVaultStatus.textContent = presentation.label;
    elements.evidenceVaultStatus.title = presentation.title;
    elements.evidenceVaultStatus.dataset.state = presentation.state;
  }
}

function clearProjectView() {
  state.evidencePacketDraft = { projectKey: "", question: "", selections: [] };
  clearEvidence();
  state.comparisonBaseline = null;
  resetSurfaceRequest();
  resetGraphNavigation();
  state.projectKey = "";
  elements.consultationResumeID.value = "";
  updateConsultationResumeControl();
  state.operationAliasRequestId++;
  state.operationAliases = new Map();
  state.projectCursor = null;
  state.endpoints = [];
  state.endpointCursor = null;
  state.surfaceFilter = elements.filter.value.trim();
  state.totalOperations = 0;
  state.surfaceGaps = [];
  clearOperationBackfillPreview("Select a project, then request an explicit preview.");
  elements.operationBackfillOpen.disabled = true;
  state.graph = null;
  clearFieldValueTraversalStatus();
  state.selectedEndpoint = null;
  state.selectedNode = null;
  state.focusedNodeId = null;
  elements.graphProject.textContent = "SELECT A PROJECT";
  elements.graphTitle.textContent = "Observed behavior relationships";
  elements.graphSubtitle.textContent = "Select an endpoint to open its graph neighborhood.";
  elements.graphCounts.replaceChildren(countText(0, "nodes"), el("b"), countText(0, "edges"));
  elements.endpointCount.textContent = "0";
  elements.loadMore.hidden = true;
  renderEndpointList();
  renderInspectorEmpty();
  renderEmpty("No registered projects", "Add a local project and import its Caido history first.");
}

async function selectProject(projectKey) {
  if (!projectKey) {
    clearProjectView();
    return;
  }
  if (state.projectKey !== projectKey) {
    state.evidencePacketDraft = { projectKey, question: "", selections: [] };
    clearEvidence();
    state.comparisonBaseline = null;
    elements.consultationResumeID.value = "";
    updateConsultationResumeControl();
  }
  resetSurfaceRequest();
  resetGraphNavigation();
  state.projectKey = projectKey;
  clearOperationBackfillPreview("Preview cleared because the selected project changed.");
  elements.operationBackfillOpen.disabled = false;
  updateConsultationResumeControl();
  state.operationAliasRequestId++;
  state.operationAliases = new Map();
  state.endpoints = [];
  state.endpointCursor = null;
  state.surfaceFilter = elements.filter.value.trim();
  state.totalOperations = 0;
  state.surfaceGaps = [];
  state.graph = null;
  clearFieldValueTraversalStatus();
  state.selectedEndpoint = null;
  state.selectedNode = null;
  state.focusedNodeId = null;
  elements.project.value = projectKey;
  const project = state.projects.find(item => item.project_key === projectKey);
  elements.graphProject.textContent = project ? (project.display_name || `CAIDO · ${short(project.project_id || projectKey, 18)}`).toUpperCase() : "LOCAL PROJECT";
  elements.graphTitle.textContent = "Observed behavior relationships";
  elements.graphSubtitle.textContent = "Select an endpoint to open its graph neighborhood.";
  elements.graphCounts.replaceChildren(countText(0, "nodes"), el("b"), countText(0, "edges"));
  elements.inspectIndex.textContent = "—";
  renderInspectorEmpty();
  renderEmpty("Loading the project surface", "Reading a bounded page from the local structural index.");
  renderEndpointList();
  setStatus("Loading local index", "loading");
  void loadOperationAliases(projectKey);
  await loadSurface(false);
}

function countText(value, label) {
  const span = el("span");
  span.append(document.createTextNode(String(value) + " "));
  span.append(el("small", "", label));
  return span;
}

function resetSurfaceRequest() {
  // Invalidate before aborting: a transport can finish despite cancellation.
  state.surfaceRequestId++;
  state.surfaceAbort?.abort();
  state.surfaceAbort = null;
  clearTimeout(state.surfaceDebounce);
  state.surfaceDebounce = null;
  state.loadingSurface = false;
  elements.loadMore.disabled = false;
  elements.loadMore.replaceChildren(document.createTextNode("Load more "), el("span", "", "↓"));
}

async function loadSurface(append) {
  if (!state.projectKey || state.loadingSurface) return;
  const projectKey = state.projectKey;
  const filter = state.surfaceFilter;
  const requestId = ++state.surfaceRequestId;
  const controller = new AbortController();
  state.surfaceAbort = controller;
  state.loadingSurface = true;
  elements.loadMore.disabled = true;
  elements.loadMore.textContent = "Loading…";
  const query = new URLSearchParams({ project_key: projectKey, filter, limit: "40" });
  if (append && state.endpointCursor) query.set("cursor", state.endpointCursor);
  try {
    const data = await getJSON(`/api/surface?${query}`, controller.signal);
    if (requestId !== state.surfaceRequestId || state.projectKey !== projectKey || state.surfaceFilter !== filter || controller.signal.aborted) return;
    const rows = Array.isArray(data.operations) ? data.operations : [];
    state.endpoints = append ? [...state.endpoints, ...rows] : rows;
    state.endpointCursor = data.next_cursor || null;
    state.totalOperations = Number(data.total_operations || 0);
    state.surfaceGaps = Array.isArray(data.coverage_gaps) ? data.coverage_gaps : [];
    state.coverageState = data.coverage_state || "partial";
    renderEndpointList();
    elements.endpointCount.textContent = state.totalOperations;
    elements.surfaceFoot.textContent = data.truncated ? `${state.totalOperations} matching operations · more available` : `${state.totalOperations} local operations · Caido coverage is partial`;
    elements.coverage.textContent = `Coverage: ${String(state.coverageState).toUpperCase()}`;
    elements.updated.textContent = `Updated ${new Date().toLocaleTimeString("en-US", { hour: "2-digit", minute: "2-digit" })}`;
    if (!state.endpoints.length) {
      renderEmpty(filter ? "No matching operations" : "No operations in the local index", filter ? "Search a host, method, route, field name, or header name." : "Import HTTP history for the selected project, then refresh.");
      elements.endpointList.replaceChildren(emptyList(filter ? "No matches" : "Index is empty", filter ? "No indexed operation matches this filter." : "The project opened, but it has no local structural records yet."));
      setStatus("No indexed requests");
    } else if (!append || !state.selectedEndpoint) {
      renderEmpty("Select an endpoint", "Choose a route on the left. The graph uses linked observations and available sources.");
      if (!append && !state.selectedEndpoint) setStatus("Select an endpoint");
    }
  } catch (error) {
    if (requestId !== state.surfaceRequestId || state.projectKey !== projectKey || state.surfaceFilter !== filter || error.name === "AbortError") return;
    if (append && error.status === 409) {
      state.endpointCursor = null;
      state.endpoints = [];
      state.totalOperations = 0;
      state.selectedEndpoint = null;
      state.graph = null;
      state.selectedNode = null;
      state.focusedNodeId = null;
      state.routeScope = false;
      resetGraphNavigation();
      state.loadingSurface = false;
      state.surfaceRequestId++;
      elements.endpointCount.textContent = "0";
      elements.graphTitle.textContent = "Observed behavior relationships";
      elements.graphSubtitle.textContent = "Select an operation to open its graph neighborhood.";
      elements.graphCounts.replaceChildren(countText(0, "nodes"), el("b"), countText(0, "edges"));
      elements.captureSelect.replaceChildren();
      renderInspectorEmpty();
      renderEndpointList();
      renderEmpty("Local index changed", "Reloading the first page for the current operation filter.");
      setStatus("Refreshing operation catalog", "loading");
      void loadSurface(false);
      return;
    }
    if (!append) {
      elements.endpointList.replaceChildren(emptyList("Could not read the index", error.message));
      renderEmpty("Project data is unavailable", "Check the schema version, local database integrity, and BehaviorGraph availability.");
    }
    setStatus("Local index read error", "error");
  } finally {
    if (requestId === state.surfaceRequestId && state.projectKey === projectKey && state.surfaceFilter === filter) {
      state.surfaceAbort = null;
      state.loadingSurface = false;
      elements.loadMore.disabled = false;
      elements.loadMore.replaceChildren(document.createTextNode("Load more "), el("span", "", "↓"));
      elements.loadMore.hidden = !state.endpointCursor;
    }
  }
}

function renderEndpointList() {
  const rows = state.endpoints;
  elements.endpointList.replaceChildren();
  if (!rows.length) {
    elements.endpointList.append(emptyList(state.surfaceFilter ? "No matches" : "Surface not loaded", state.surfaceFilter ? "Adjust the filter." : "Discovered routes will appear here."));
  }
  for (const endpoint of rows) {
    const key = endpointKey(endpoint);
    const button = el("button", `endpoint-item${state.selectedEndpoint && endpointKey(state.selectedEndpoint) === key ? " active" : ""}`);
    button.type = "button";
    button.setAttribute("aria-pressed", state.selectedEndpoint && endpointKey(state.selectedEndpoint) === key ? "true" : "false");
    button.setAttribute("aria-label", `${endpoint.method} ${endpoint.host}${displayPath(endpoint.route)}, ${endpoint.observation_count} matching requests`);
    button.title = MASKED_PATH_HELP;
    const method = el("span", `method-chip method-${String(endpoint.method || "get").toLowerCase()}`, endpoint.method || "?");
    const copy = el("span", "endpoint-copy");
    copy.append(el("span", "endpoint-host", endpoint.host || "unknown host"));
    copy.append(el("span", "endpoint-route", displayPath(endpoint.route || "/")));
    const meta = el("span", "endpoint-meta");
    meta.append(document.createTextNode(`${endpoint.observation_count || 0} observations`));
    const dot = el("i"); meta.append(dot);
    meta.append(document.createTextNode(`${endpoint.graph_mapped_observation_count || 0} in graph`));
    copy.append(meta);
    button.append(method, copy);
    button.addEventListener("click", () => openEndpoint(endpoint));
    elements.endpointList.append(button);
  }
  elements.endpointCount.textContent = state.totalOperations || "0";
  elements.loadMore.hidden = !state.endpointCursor;
}

function emptyList(title, copy) {
  const item = el("div", "empty-list");
  item.append(el("strong", "", title), document.createTextNode(copy));
  return item;
}

function endpointKey(endpoint) { return [endpoint.host, endpoint.port, endpoint.route, endpoint.method].join("\u0000"); }

async function openEndpoint(endpoint) {
  resetGraphNavigation();
  state.selectedEndpoint = endpoint;
  state.routeScope = false;
  state.focusedNodeId = null;
  state.hiddenKinds = new Set();
  state.selectedNode = null;
  state.graph = null;
  renderEndpointList();
  renderInspectorEmpty();
  const seeds = (endpoint.sampled_graph_seed_ids || []).slice(0, 20);
  elements.captureSelect.replaceChildren();
  seeds.forEach((id, index) => elements.captureSelect.append(new Option(`Capture ${index + 1} of ${seeds.length} sampled`, id)));
  elements.captureSelect.hidden = seeds.length < 2;
  elements.graphTitle.textContent = displayPath(endpoint.route || "/");
  elements.graphTitle.title = MASKED_PATH_HELP;
  elements.graphSubtitle.textContent = `${endpoint.method}  ·  ${endpoint.host}${endpoint.port ? `:${endpoint.port}` : ""}  ·  ${endpoint.observation_count || 0} requests in loaded surface  ·  ${seeds.length} sampled captures`;
  elements.graphCounts.title = "Node and edge counts for the current graph slice of this route; these are not project-wide request totals.";
  if (!seeds.length) {
    elements.graphCounts.replaceChildren(countText(0, "nodes"), el("b"), countText(0, "edges"));
    renderEmpty("No graph mapping for this endpoint", "The index contains this route, but its observations are not linked to BehaviorGraph yet.");
    elements.inspector.replaceChildren(emptyList("No node to inspect", "No graph observations were found for this endpoint."));
    setStatus("Observations are not linked to the graph");
    return;
  }
  await loadGraph([seeds[0]], { focusNodeId: seeds[0] });
}

function resetGraphNavigation() {
  state.graphAbort?.abort();
  state.graphRequestId++;
  state.graphHistory = [];
  state.graphSeeds = [];
  state.loadingGraph = false;
  elements.graphBack.disabled = true;
  elements.graphMore.disabled = true;
  elements.graphMore.hidden = true;
  elements.graphDeeper.disabled = true;
  elements.captureSelect.hidden = true;
}

function graphNavigationSnapshot() {
  return { graph: state.graph, seeds: [...state.graphSeeds], view: state.graphView,
    focus: state.focusedNodeId, routeScope: state.routeScope, hiddenKinds: new Set(state.hiddenKinds), selectedNodeId: state.selectedNode?.id };
}

function restoreGraphNavigation(previous) {
  state.graph = previous.graph;
  state.graphSeeds = previous.seeds;
  state.graphView = previous.view;
  state.focusedNodeId = previous.focus;
  state.routeScope = previous.routeScope;
  state.hiddenKinds = previous.hiddenKinds;
  if ([...elements.captureSelect.options].some(option => option.value === previous.seeds[0])) elements.captureSelect.value = previous.seeds[0];
  renderGraph();
  if (previous.selectedNodeId) selectNode(previous.selectedNodeId);
  elements.graphBack.disabled = state.graphHistory.length === 0;
  updateGraphMoreControl();
  elements.graphDeeper.disabled = state.loadingGraph || state.graphView === "full";
  setStatus(`Returned to ${state.graphView === "direct" ? "direct relationships" : "deeper slice"} · coverage is partial`, "ready");
}

function updateGraphMoreControl() {
  const canContinue = Boolean(state.graph?.next_cursor) && (state.graphView === "direct" || state.graphView === "full");
  elements.graphMore.hidden = !canContinue;
  elements.graphMore.disabled = state.loadingGraph || !canContinue;
  elements.graphMore.textContent = state.graphView === "full" ? "More omitted branches" : "More neighbors";
  elements.graphMore.title = state.graphView === "full"
    ? "Continue rendered omissions and native assertion, frontier, card, and provenance branches in the pinned graph generation"
    : "Continue with the next generation-pinned neighbor page";
}

async function loadGraph(seeds, { view = "direct", focusNodeId = null, remember = false, cursor = "", appendPage = false } = {}) {
  if (!state.projectKey || !Array.isArray(seeds) || !seeds.length) return;
  const previous = graphNavigationSnapshot();
  const projectKey = state.projectKey;
  const requestId = ++state.graphRequestId;
  state.graphAbort?.abort();
  const controller = new AbortController();
  state.graphAbort = controller;
  state.loadingGraph = true;
  elements.graphDeeper.disabled = true;
  if (!appendPage) renderEmpty(view === "full" ? "Loading a deeper graph slice" : "Loading direct relationships", view === "full" ? "Reading and validating the project snapshot. Large projects can take longer." : "Reading the selected node and its immediate connections.");
  elements.graphMore.disabled = true;
  setStatus(appendPage ? view === "full" ? "Loading omitted graph branches" : "Loading more neighbors" : view === "full" ? "Loading deeper slice" : "Loading direct relationships", "loading");
  const query = new URLSearchParams({ project_key: projectKey, view });
  const seedLimit = view === "full" ? 80 : 20;
  for (const seed of seeds.slice(0, seedLimit)) query.append("seed", seed);
  if (cursor) query.set("cursor", cursor);
  try {
    const graph = await getJSON(`/api/graph?${query}`, controller.signal);
    if (requestId !== state.graphRequestId || state.projectKey !== projectKey) return;
    if (remember && previous.graph) {
      state.graphHistory.push(previous);
      if (state.graphHistory.length > 8) state.graphHistory.shift();
    }
    state.graph = appendPage && previous.graph && view === "direct"
      ? mergeGraphPages(previous.graph, graph)
      : graph;
    renderFieldValueTraversalStatus(state.graph.field_value_traversal);
    state.graphSeeds = Array.isArray(graph.seed_ids) ? [...graph.seed_ids] : [...seeds];
    state.graphView = view;
    state.focusedNodeId = appendPage ? previous.focus : focusNodeId;
    state.routeScope = false;
    state.hiddenKinds = appendPage ? previous.hiddenKinds : new Set();
    state.selectedNode = null;
    renderGraph(!appendPage);
    const first = state.renderedEntities.find(item => item.id === focusNodeId) || state.renderedEntities.find(item => seeds.includes(item.id)) || state.renderedEntities[0];
    if (first) selectNode(first.id);
    else renderInspectorEmpty();
    const partial = Boolean(state.graph.incomplete || state.graph.truncated);
    const label = appendPage ? view === "full" ? "Omitted graph branches" : "More direct relationships" : view === "direct" ? "Direct relationships" : "Deeper graph slice";
    setStatus(partial ? `${label} · coverage is partial` : label, "ready");
    elements.graphState.title = partial
      ? "This is a bounded graph view; full-project integrity is not assessed. This status is separate from the selected capture's Parse state."
      : "Graph slice loaded.";
    if (partial) {
      const sampledEntities = Array.isArray(graph.provenance_truncated_entity_ids)
        ? graph.provenance_truncated_entity_ids.length
        : 0;
      elements.coverage.textContent = graph.provenance_truncated
        ? `Graph coverage: partial · source references sampled on ${sampledEntities} entities`
        : "Graph coverage: partial";
    }
    const provenance = graph.capture_provenance?.find(item => item.graph_observation_id === seeds[0]);
    const option = [...elements.captureSelect.options].find(item => item.value === seeds[0]);
    if (option && provenance?.request_id) option.textContent = `Caido #${provenance.request_id}`;
  } catch (error) {
    if (requestId !== state.graphRequestId || error.name === "AbortError") return;
    if (appendPage && error.status === 409) {
      const reloadView = previous.view || view;
      state.graph = { ...(previous.graph || {}) };
      delete state.graph.next_cursor;
      state.graphHistory = [];
      elements.graphBack.disabled = true;
      setStatus(reloadView === "full" ? "Graph changed · refreshing the full slice" : "Graph changed · refreshing direct relationships", "loading");
      void loadGraph(seeds, { view: reloadView, focusNodeId: previous.focus, remember: false });
      return;
    }
    if (previous.graph) {
      restoreGraphNavigation(previous);
      showToast(`Could not load relationships: ${error.message}`);
      return;
    }
    state.graph = null;
    clearFieldValueTraversalStatus();
    renderEmpty("Could not build the graph", error.message || "Check compatibility with the local BehaviorGraph database.");
    elements.graphCounts.replaceChildren(countText(0, "nodes"), el("b"), countText(0, "edges"));
    elements.inspector.replaceChildren(emptyList("Graph unavailable", "Select another endpoint or check graph database integrity."));
    setStatus("Graph read error", "error");
  } finally {
    if (requestId === state.graphRequestId) {
      state.loadingGraph = false;
      elements.graphBack.disabled = state.graphHistory.length === 0;
      updateGraphMoreControl();
      elements.graphDeeper.disabled = !state.graph || state.graphView === "full";
    }
  }
}

function mergeGraphPages(current, next) {
  const continuationScopes = new Set(["rendered_missing_assertion_branches", "rendered_and_native_candidate_branches"]);
  const currentTraversal = current.field_value_traversal;
  const nextTraversal = next.field_value_traversal;
  const fieldValueTraversal = nextTraversal?.state === "not_applicable" &&
    currentTraversal?.state && currentTraversal.state !== "not_applicable"
    ? currentTraversal : nextTraversal || currentTraversal;
  if (continuationScopes.has(current.continuation_scope) || continuationScopes.has(next.continuation_scope)) {
    return { ...next, field_value_traversal: fieldValueTraversal };
  }
  const merged = { ...current, ...next, field_value_traversal: fieldValueTraversal };
  for (const key of new Set([...Object.keys(current), ...Object.keys(next)])) {
    if (!Array.isArray(current[key]) && !Array.isArray(next[key])) continue;
    const values = [...(current[key] || []), ...(next[key] || [])];
    const seen = new Set();
    merged[key] = values.filter(value => {
      let stable;
      if (value && typeof value === "object") {
        if (value.id !== undefined) stable = `id:${value.id}`;
        else if (value.graph_observation_id || value.revision_id) stable = `provenance:${value.graph_observation_id || ""}:${value.revision_id || ""}`;
        else if (value.sha256 || value.record_locator) stable = `source:${value.sha256 || ""}:${value.record_locator || ""}`;
        else stable = `object:${JSON.stringify(value)}`;
      } else stable = `value:${JSON.stringify(value)}`;
      if (seen.has(stable)) return false;
      seen.add(stable);
      return true;
    });
  }
  if (next.next_cursor) merged.next_cursor = next.next_cursor;
  else delete merged.next_cursor;
  merged.truncated = Boolean(next.next_cursor || next.truncated);
  merged.neighborhood_candidate_limit_reached = Boolean(next.next_cursor || next.neighborhood_candidate_limit_reached);
  merged.client_merged_pages = Number(current.client_merged_pages || 1) + 1;
  merged.result_page_ids = [...(current.result_page_ids || [current.result_id]).filter(Boolean), next.result_id].filter(Boolean);
  delete merged.result_id;
  return merged;
}

function renderEmpty(title, copy) {
  elements.canvasEmpty.hidden = false;
  elements.emptyTitle.textContent = title;
  elements.emptyCopy.textContent = copy;
  elements.graphScene.replaceChildren();
  state.renderedEntities = [];
  state.renderedAssertions = [];
  state.layoutPositions = new Map();
}

function renderGraph(resetView = true) {
  const graph = state.graph || {};
  const allEntities = Array.isArray(graph.entities) ? graph.entities : [];
  const allAssertions = Array.isArray(graph.assertions) ? graph.assertions : [];
  const scoped = state.focusedNodeId
    ? BBGraphFocus.focusedNeighborhood(allEntities, allAssertions, state.focusedNodeId)
    : state.routeScope ? selectedEndpointNeighborhood(allEntities, allAssertions) : null;
  const entities = scoped?.entities || allEntities;
  const assertions = scoped?.assertions || allAssertions;
  renderGraphFilters(entities);
  if (!allEntities.length) {
    state.renderedEntities = [];
    state.renderedAssertions = [];
    renderEmpty("Graph neighborhood is empty", "No related entities were found for the selected seed IDs.");
    elements.graphCounts.replaceChildren(countText(0, "nodes"), el("b"), countText(0, "edges"));
    return;
  }
  const visible = entities.filter(node => !state.hiddenKinds.has(node.kind));
  const visibleIds = new Set(visible.map(node => node.id));
  const visibleAssertions = assertions.filter(edge => visibleIds.has(edge.subject) && visibleIds.has(edge.object));
  const connectedIds = new Set();
  for (const edge of visibleAssertions) { connectedIds.add(edge.subject); connectedIds.add(edge.object); }
  const seedIds = new Set(state.graph?.seed_ids || []);
  const renderedEntities = visible.filter(node => connectedIds.has(node.id) || seedIds.has(node.id) || node.id === state.focusedNodeId || isSelectedEndpointNode(node));
  const renderedIds = new Set(renderedEntities.map(node => node.id));
  const renderedAssertions = visibleAssertions.filter(edge => renderedIds.has(edge.subject) && renderedIds.has(edge.object));
  state.renderedEntities = renderedEntities;
  state.renderedAssertions = renderedAssertions;
  renderGraphFilters(entities, renderedEntities);
  if (!renderedEntities.length) {
    state.selectedNode = null;
    state.layoutPositions = new Map();
    elements.graphCanvas.setAttribute("viewBox", "0 0 960 560");
    elements.graphScene.replaceChildren();
    elements.canvasEmpty.hidden = false;
    elements.emptyTitle.textContent = "Nodes are hidden by filters";
    elements.emptyCopy.textContent = "Enable a node type or choose “All nodes” to see relationships.";
    const scopeLabel = state.focusedNodeId ? "near focus" : state.routeScope ? "in overview" : "in slice";
    renderGraphCounts(0, 0, allEntities.length);
    elements.graphCounts.title = state.focusedNodeId
      ? `Direct relationships of the selected node: ${entities.length} nodes and ${assertions.length} edges. The full traversal contains ${allEntities.length} nodes and ${allAssertions.length} edges.`
      : `The selected view contains ${entities.length} nodes and ${assertions.length} edges; the full traversal contains ${allEntities.length} nodes and ${allAssertions.length} edges.`;
    renderInspectorEmpty();
    return;
  }
  elements.canvasEmpty.hidden = true;
  const scopeLabel = state.focusedNodeId ? "near focus" : state.routeScope ? "in overview" : "in slice";
  renderGraphCounts(renderedEntities.length, renderedAssertions.length, allEntities.length);
  elements.graphCounts.title = state.focusedNodeId
    ? `Direct relationships of the selected node: ${renderedEntities.length} of ${entities.length} nodes. The full traversal contains ${allEntities.length} nodes and ${allAssertions.length} edges.`
    : state.routeScope
      ? `Selected route overview: ${renderedEntities.length} of ${entities.length} nodes. The full traversal contains ${allEntities.length} nodes and ${allAssertions.length} edges.`
      : `Showing ${renderedEntities.length} of ${entities.length} nodes and ${renderedAssertions.length} edges in this graph slice.`;
  elements.graphScene.replaceChildren();
  const layout = layoutGraph(renderedEntities, renderedAssertions);
  state.layoutPositions = layout.positions;
  const edgeLayer = svgEl("g", { class: "edge-layer" });
  for (const edge of renderedAssertions) {
    const from = layout.positions.get(edge.subject);
    const to = layout.positions.get(edge.object);
    if (!from || !to) continue;
    const dx = to.x - from.x, dy = to.y - from.y;
    const length = Math.max(1, Math.hypot(dx, dy));
    const offset = Math.min(80, length * 0.17);
    const sx = from.x + dx / length * offset, sy = from.y + dy / length * offset;
    const ex = to.x - dx / length * offset, ey = to.y - dy / length * offset;
    const bend = Math.min(56, Math.abs(dy) * .18 + 10);
    const curve = svgEl("path", { d: `M${sx},${sy} Q${(sx + ex) / 2 - bend},${(sy + ey) / 2} ${ex},${ey}`, class: `edge-path${edge.evidence_state === "contested" ? " edge-contested" : ""}` });
    edgeLayer.append(curve);
    if (state.edgeLabelsVisible) {
      const label = svgEl("text", { x: (sx + ex) / 2, y: (sy + ey) / 2 - 7, class: "edge-label", "text-anchor": "middle" });
      label.textContent = relationLabels[edge.predicate] || String(edge.predicate || "relationship").replaceAll("_", " ");
      edgeLayer.append(label);
    }
  }
  elements.graphScene.append(edgeLayer);
  for (const node of renderedEntities) {
    const point = layout.positions.get(node.id);
    if (point) elements.graphScene.append(renderNode(node, point));
  }
  elements.graphCanvas.setAttribute("viewBox", `0 0 ${layout.width} ${layout.height}`);
  if (resetView) { state.zoom = 1; state.panX = 0; state.panY = 0; }
  applyTransform();
  if (!state.selectedNode || !renderedEntities.some(node => node.id === state.selectedNode.id)) {
    state.selectedNode = null;
    const replacement = renderedEntities.find(isSelectedEndpointNode) || renderedEntities.find(node => seedIds.has(node.id)) || renderedEntities[0];
    if (replacement) selectNode(replacement.id);
    else renderInspectorEmpty();
  }
}

function renderGraphCounts(shown, edges, loaded) {
  const omitted = state.graph?.missing_branches?.length || 0;
  elements.graphCounts.replaceChildren(countText(shown, "shown"), el("b"), countText(edges, "edges"), el("b"), countText(loaded, "loaded"));
  if (omitted) elements.graphCounts.append(el("b"), countText(omitted, "omitted"));
  if (state.graph?.neighborhood_candidate_limit_reached || state.graph?.missing_branches_truncated) {
    elements.graphCounts.append(el("b"), el("span", "graph-more", "more available"));
  }
  if (state.graph?.provenance_truncated) {
    const ids = Array.isArray(state.graph.provenance_truncated_entity_ids)
      ? state.graph.provenance_truncated_entity_ids
      : [];
    const sample = el("span", "graph-more", `${ids.length} entities sampled`);
    const limit = Number.isInteger(state.graph.entity_source_ref_limit)
      ? `${state.graph.entity_source_ref_limit}`
      : "a bounded number of";
    sample.title = `Source references are sampled to at most ${limit} per entity. Use the deeper graph slice to request full provenance, subject to its limits.`;
    elements.graphCounts.append(el("b"), sample);
  }
}

function renderGraphFilters(entities, renderedEntities = state.renderedEntities) {
  if (!elements.graphFilters) return;
  const counts = new Map();
  for (const node of entities) counts.set(node.kind || "entity", (counts.get(node.kind || "entity") || 0) + 1);
  const renderedCounts = new Map();
  for (const node of renderedEntities || []) renderedCounts.set(node.kind || "entity", (renderedCounts.get(node.kind || "entity") || 0) + 1);
  const kinds = [...counts.keys()].sort((a, b) => (filterLabels[a] || a).localeCompare(filterLabels[b] || b, "en"));
  elements.graphFilters.replaceChildren();
  const overview = elements.graphOverview;
  const showAll = elements.graphShowAll;
  if (overview) {
    overview.disabled = kinds.length === 0;
    overview.setAttribute("aria-pressed", String(!state.focusedNodeId && kinds.length > 0 && isOverviewView(kinds)));
  }
  if (showAll) {
    showAll.disabled = kinds.length === 0;
    showAll.setAttribute("aria-pressed", String(!state.focusedNodeId && kinds.length > 0 && kinds.every(kind => !state.hiddenKinds.has(kind))));
  }
  for (const kind of kinds) {
    const count = counts.get(kind) || 0;
    const visible = !state.hiddenKinds.has(kind);
    const chip = el("button", `graph-filter${visible ? " active" : ""}`);
    chip.type = "button";
    chip.setAttribute("aria-pressed", String(visible));
    const actuallyShown = renderedCounts.get(kind) || 0;
    chip.title = visible
      ? `${filterLabels[kind] || nodeLabels[kind] || kind}: ${count} in slice, ${actuallyShown} shown`
      : `${filterLabels[kind] || nodeLabels[kind] || kind}: hidden, ${count} in slice`;
    chip.append(document.createTextNode(`${filterLabels[kind] || nodeLabels[kind] || kind} `), el("b", "", count));
    chip.addEventListener("click", () => {
      if (state.hiddenKinds.has(kind)) state.hiddenKinds.delete(kind);
      else state.hiddenKinds.add(kind);
      renderGraph(false);
    });
    elements.graphFilters.append(chip);
  }
  const hidden = kinds.filter(kind => state.hiddenKinds.has(kind));
  if (elements.graphFilterSummary) {
    const focusedNode = state.focusedNodeId && state.graph?.entities?.find(node => node.id === state.focusedNodeId);
    elements.graphFilterSummary.textContent = focusedNode
      ? `Direct relationships · ${displayTitle(focusedNode)}`
      : hidden.length
      ? `Hidden types: ${hidden.map(kind => filterLabels[kind] || nodeLabels[kind] || kind).join(", ")}`
      : "All types are shown";
  }
}

function isOverviewView(kinds = []) {
  return OVERVIEW_HIDDEN_KINDS.every(kind => !kinds.includes(kind) || state.hiddenKinds.has(kind)) &&
    kinds.every(kind => OVERVIEW_HIDDEN_KINDS.includes(kind) || !state.hiddenKinds.has(kind));
}

function isSelectedEndpointNode(node) {
  if (node.kind !== "endpoint" || !state.selectedEndpoint) return false;
  const endpointIds = selectedEndpointIds();
  if (endpointIds.size) return endpointIds.has(node.id);
  const data = node.data || {};
  const routeMatches = normalizedRoute(data.path) === normalizedRoute(state.selectedEndpoint.route);
  const origin = String(data.origin || "").toLowerCase();
  const hostMatches = !origin || origin.includes(String(state.selectedEndpoint.host || "").toLowerCase());
  const portMatches = !origin || !state.selectedEndpoint.port || origin.endsWith(`:${state.selectedEndpoint.port}`);
  return routeMatches && hostMatches && portMatches && String(data.method || "").toUpperCase() === String(state.selectedEndpoint.method || "").toUpperCase();
}

function selectedEndpointIds() {
  const seedIds = new Set(state.selectedEndpoint?.sampled_graph_seed_ids || []);
  if (!seedIds.size) return new Set();
  return new Set((state.graph?.entities || [])
    .filter(node => node.kind === "observation" && seedIds.has(node.id))
    .map(node => node.data?.endpoint_id)
    .filter(Boolean));
}

function normalizedRoute(value) {
  return displayPath(value).replace(/\/{2,}/g, "/").toLowerCase();
}

function selectGraphView(view) {
  state.routeScope = view === "overview";
  state.focusedNodeId = null;
  state.hiddenKinds = new Set(view === "overview" ? OVERVIEW_HIDDEN_KINDS : []);
  renderGraph(true);
  const node = state.renderedEntities.find(isSelectedEndpointNode) || state.renderedEntities.find(item => (state.graph?.seed_ids || []).includes(item.id)) || state.renderedEntities[0];
  if (node) selectNode(node.id);
  const label = state.routeScope ? "Endpoint overview" : "Full graph slice";
  const partial = Boolean(state.graph?.incomplete || state.graph?.truncated);
  setStatus(partial ? `${label} · coverage is partial` : label, "ready");
}

async function focusOnNode(node) {
  if (!state.graph || !state.graph.entities?.some(item => item.id === node.id)) return;
  await loadGraph([node.id], { focusNodeId: node.id, remember: true });
}

function selectedEndpointNeighborhood(entities, assertions) {
  const endpointIds = selectedEndpointIds();
  const endpoints = entities.filter(node => node.kind === "endpoint" && (endpointIds.size ? endpointIds.has(node.id) : isSelectedEndpointNode(node)));
  if (!endpoints.length) return null;
  const byId = new Map(entities.map(node => [node.id, node]));
  const included = new Set(endpoints.map(node => node.id));
  const addNonEndpointNeighbors = nodeId => {
    for (const edge of assertions) {
      const otherId = edge.subject === nodeId ? edge.object : edge.object === nodeId ? edge.subject : null;
      const other = otherId ? byId.get(otherId) : null;
      if (other && other.kind !== "endpoint") included.add(other.id);
    }
  };
  for (const endpoint of endpoints) addNonEndpointNeighbors(endpoint.id);
  for (const node of entities) {
    if (node.kind === "observation" && endpoints.some(endpoint => node.data?.endpoint_id === endpoint.id)) {
      included.add(node.id);
      addNonEndpointNeighbors(node.id);
    }
  }
  const scopedEntities = entities.filter(node => included.has(node.id));
  const scopedIds = new Set(scopedEntities.map(node => node.id));
  const scopedAssertions = assertions.filter(edge => scopedIds.has(edge.subject) && scopedIds.has(edge.object));
  return { entities: scopedEntities, assertions: scopedAssertions };
}

function layoutGraph(entities, assertions) {
  const ids = new Set(entities.map(node => node.id));
  const seedIds = (state.graph?.seed_ids || []).filter(id => ids.has(id));
  const selectedEndpoint = entities.find(isSelectedEndpointNode);
  const root = (state.focusedNodeId && ids.has(state.focusedNodeId) ? state.focusedNodeId : null) || seedIds[0] || selectedEndpoint?.id || entities[0]?.id;
  return BBGraphFocus.layeredGridLayout(entities, assertions, root, displayTitle);
}

function renderNode(node, point) {
  const kind = String(node.kind || "entity");
  const group = svgEl("g", { class: `node-group type-${safeClass(kind)}${state.selectedNode?.id === node.id ? " selected" : ""}`, transform: `translate(${point.x - 88} ${point.y - 34})`, tabindex: "0", role: "button", "data-node-id": node.id, "aria-label": `${nodeLabels[kind] || kind}, ${displayTitle(node)}` });
  group.append(svgEl("rect", { class: "node-card", x: 0, y: 0, width: 176, height: 68, rx: 8 }));
  group.append(svgEl("rect", { class: "node-accent", x: 0, y: 12, width: 3, height: 44 }));
  const label = svgEl("text", { class: "node-kind", x: 13, y: 17 }); label.textContent = nodeLabels[kind] || kind.toUpperCase(); group.append(label);
  const title = svgEl("text", { class: "node-title", x: 13, y: 37 }); title.textContent = truncate(displayTitle(node), 22); group.append(title);
  const subtitle = svgEl("text", { class: "node-subtitle", x: 13, y: 54 }); subtitle.textContent = truncate(displaySubtitle(node), 25); group.append(subtitle);
  group.addEventListener("click", event => { event.stopPropagation(); selectNode(node.id); });
  group.addEventListener("keydown", event => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); selectNode(node.id); } });
  return group;
}

function displayTitle(node) {
  const data = node.data || {};
  if (node.kind === "endpoint") return `${data.method || "?"} ${displayPath(data.path || "/")}`;
  if (node.kind === "operation") return BBGraphFocus.operationAliasFor(node, state.operationAliases) || `${data.method || "?"} ${displayPath(data.route_family || "/")}`;
  if (node.kind === "observation") return `${data.observation_kind === "live_capture" ? "Live capture" : "Capture"}${data.http_status ? ` · HTTP ${data.http_status}` : ""}`;
  if (node.kind === "parameter") return displayPath(data.path || data.name || "Parameter");
  if (node.kind === "field_occurrence") return displayPath(data.path || "Field occurrence");
  if (node.kind === "value_reference") return "Project value match";
  if (node.kind === "url_reference") return "Project URL match";
  if (node.kind === "request_variant") return "Request shape";
  if (node.kind === "response_variant") return "Response shape";
  if (node.kind === "asset") return data.origin || "Host";
  return data.label || data.name || node.kind || "Entity";
}

function displaySubtitle(node) {
  const data = node.data || {};
  if (node.kind === "observation") return data.observed_at && data.observed_at !== "unknown" ? data.observed_at : "Capture time unavailable";
  if (node.kind === "endpoint") return (data.query_names || []).slice(0, 4).join(" · ") || "Route template · values masked";
  if (node.kind === "operation") return `${data.route_family_version || "unversioned"} · heuristic route family`;
  if (node.kind === "parameter") return [data.type, data.location].filter(Boolean).join(" · ") || "Structural field";
  if (node.kind === "field_occurrence") {
    const occurrence = Number.isInteger(data.occurrence_index) ? `occurrence ${data.occurrence_index + 1}` : "occurrence";
    const span = data.value_span || data.source_span;
    return [data.location, occurrence, span ? formatSourceSpan(span) : "source span unavailable"].filter(Boolean).join(" · ");
  }
  if (node.kind === "value_reference") return "Same decoded value · project scoped";
  if (node.kind === "url_reference") return "Same normalized URL · project scoped";
  if (node.kind === "request_variant" || node.kind === "response_variant") {
    const headers = Array.isArray(data.header_metadata) ? data.header_metadata.length : 0;
    const fields = Array.isArray(data.fields) ? data.fields.length : 0;
    const parts = [node.kind === "request_variant" ? data.method : (data.status ? `HTTP ${data.status}` : "HTTP response")];
    if (data.content_type) parts.push(data.content_type === "unknown" ? "type unavailable" : data.content_type);
    parts.push(`${headers} headers`, `${fields} fields`);
    if (data.body_bytes !== null && data.body_bytes !== undefined) parts.push(`body ${formatBytes(data.body_bytes)}`);
    if (data.parse_state !== "complete") parts.push("parse incomplete");
    return parts.filter(Boolean).join(" · ");
  }
  if (node.kind === "asset") return data.program ? `Project ${short(data.program, 12)}` : "Observed host";
  return node.id || "";
}

function selectNode(nodeId) {
  const node = (state.graph?.entities || []).find(item => item.id === nodeId);
  if (!node) return;
  state.selectedNode = node;
  for (const group of elements.graphScene.querySelectorAll(".node-group")) group.classList.toggle("selected", group.getAttribute("data-node-id") === nodeId);
  renderInspector(BBGraphFocus.withObservationParseState(node, state.graph?.capture_provenance));
}

function centerNodeInGraph(nodeId) {
  const view = elements.graphCanvas.viewBox.baseVal;
  const zoom = Math.max(3, state.zoom);
  const transform = BBGraphFocus.focusedTransform(state.layoutPositions.get(nodeId), view.width, view.height, zoom);
  if (!transform) return;
  state.zoom = Math.min(MAX_GRAPH_ZOOM, zoom);
  state.panX = transform.panX;
  state.panY = transform.panY;
  applyTransform();
}

function renderInspector(node) {
  elements.inspector.replaceChildren();
  elements.inspectIndex.textContent = String((state.graph?.entities || []).findIndex(item => item.id === node.id) + 1).padStart(2, "0");
  const kind = String(node.kind || "entity");
  const badge = el("span", "detail-kind");
  badge.classList.add(`kind-${safeClass(kind)}`);
  const dot = el("i");
  badge.append(dot, document.createTextNode(nodeLabels[kind] || kind.toUpperCase()));
    elements.inspector.append(badge, el("h3", "detail-title", displayTitle(node)), el("div", "detail-id", node.id || "ID unavailable"));
  const rows = safeDetails(node);
  if (node.kind === "observation") {
    const note = el("div", "detail-explainer");
    note.append(el("strong", "", "What this means"));
    note.append(el("span", "", "This record represents one saved request from Caido. HTTP status, parser completeness, and analysis state describe different properties."));
    elements.inspector.append(note);
    for (const explanation of BBGraphFocus.observationNotes(node.data || {})) {
      const detail = el("div", "detail-explainer");
      detail.append(el("strong", "", explanation.title), el("span", "", explanation.body));
      elements.inspector.append(detail);
    }
  }
  if (node.kind === "operation" || node.kind === "endpoint") {
    const uncertainty = BBGraphFocus.routeFamilyUncertainty(
      node.kind === "operation" ? node.data?.route_family : node.data?.path
    );
    if (uncertainty) {
      const note = el("div", "detail-explainer");
      note.append(el("strong", "", `${node.kind === "operation" ? "Route-family grouping" : "Endpoint route mask"} · ${uncertainty.maskedSegments} masked`));
      note.append(el("span", "", uncertainty.message));
      elements.inspector.append(note);
    }
  }
  if (node.kind === "value_reference") {
    const note = el("div", "detail-explainer");
    note.append(el("strong", "", "What this link means"));
    note.append(el("span", "", "The decoded query value matched under this project's keyed equality signature. This is a correlation only; it does not establish causality, object identity, ownership, or data flow."));
    elements.inspector.append(note);
  }
  if (node.kind === "url_reference") {
    const note = el("div", "detail-explainer");
    note.append(el("strong", "", "Matching navigation URLs"));
    note.append(el("span", "", "Expand this node to see captures with the same request target, Referer, or redirect URL. Several captures can match. URL equality does not establish request order, an initiating request, or causality."));
    elements.inspector.append(note);
  }
  if (rows.length) {
    const section = el("section", "detail-section");
    section.append(el("div", "detail-section-title", "Observed properties"));
    const dl = el("dl");
    for (const [label, value] of rows) {
      const row = el("div", "detail-row"); row.append(el("dt", "", label), el("dd", "", value)); dl.append(row);
    }
    section.append(dl); elements.inspector.append(section);
  }
  if (node.kind === "endpoint" && Array.isArray(node.data?.query_names) && node.data.query_names.length) {
    const section = el("section", "detail-section");
    section.append(el("div", "detail-section-title", "Field names"));
    const tags = el("div", "tag-list");
    for (const name of node.data.query_names.slice(0, 40)) tags.append(el("span", "field-tag", name));
    section.append(tags); elements.inspector.append(section);
  }
  if (node.kind === "endpoint") {
    elements.inspector.append(el("p", "detail-help", "This is a grouped route template. Select a linked historical capture and choose Open stored HTTP to inspect its exact path in the local evidence view. Live-only records remain in Caido."));
  }
  if (node.kind === "operation") renderOperationAliasEditor(node);
  if (node.kind === "observation") {
    renderProvenance(node);
    renderObservationRequestOrder(node);
    renderAcceptedContextAnnotations(node);
  }
  renderAnalysisCards(node);
  const center = el("button", "detail-action", "Center and zoom to node");
  center.type = "button";
  center.disabled = !state.layoutPositions.has(node.id);
  center.title = "Center the selected node and zoom to at least 300%. Use the zoom controls for more detail.";
  center.addEventListener("click", () => centerNodeInGraph(node.id));
  elements.inspector.append(center);
  elements.inspector.append(el("p", "detail-help", "Centers this node in the graph and zooms in for inspection."));
  const action = el("button", "detail-action", "Load direct relationships →");
  action.type = "button";
  action.disabled = !/^[a-z][a-z0-9_]*-[a-f0-9]{64}$/.test(node.id || "");
  action.addEventListener("click", () => { void focusOnNode(node); });
  elements.inspector.append(action);
  elements.inspector.append(el("p", "detail-help", "Loads this node and its immediate connections. Use Back to return to the previous graph."));
}

function renderAnalysisCards(node) {
  const cards = BBGraphFocus.analysisCardsForNode(node, state.graph?.cards);
  const candidatesTruncated = Boolean(state.graph?.analysis_card_candidates_truncated);
  if (!cards.length && !candidatesTruncated) return;
  const section = el("section", "detail-section");
  section.append(el("div", "detail-section-title", `Analysis cards · ${cards.length}`));
  if (!cards.length) {
    section.append(el("p", "detail-help", "The bounded graph query omitted additional matching cards; none of the loaded cards is attached to this node."));
  }
  const shown = cards.slice(0, 8);
  for (const card of shown) {
    const item = el("div", "provenance-item");
    item.append(el("b", "", `${card.analysis_state || "unknown analysis state"} · ${card.id}`));
    const requestParse = card.request_analysis?.parse_state || "unknown";
    const responseParse = card.response_analysis?.parse_state || "unknown";
    const responseStatus = card.response_analysis?.status;
    const effect = card.business_action?.known_effect || "unknown";
    const hypotheses = Array.isArray(card.hypotheses) ? card.hypotheses.length : 0;
    const missing = Array.isArray(card.missing_evidence) ? card.missing_evidence.filter(value => typeof value === "string") : [];
    item.append(el("span", "", `Request parse · ${requestParse} · Response parse · ${responseParse}${responseStatus ? ` · HTTP ${responseStatus}` : ""}`));
    item.append(el("span", "", `Known effect · ${boundedInspectorText(effect, 160)} · Hypotheses · ${hypotheses}`));
    for (const detail of missing.slice(0, 3)) {
      item.append(el("span", "", `Missing evidence · ${boundedInspectorText(detail, 240)}`));
    }
    if (missing.length > 3) item.append(el("span", "", `and ${missing.length - 3} more missing-evidence notes`));
    section.append(item);
  }
  if (cards.length > shown.length || candidatesTruncated) {
    section.append(el("p", "detail-help", "The view is bounded. Narrow the graph selection or open the full analytical slice for additional cards."));
  }
  section.append(el("p", "detail-help", "Analysis cards summarize available parsing and context. They do not confirm a vulnerability or impact."));
  elements.inspector.append(section);
}

function boundedInspectorText(value, maxLength) {
  const text = typeof value === "string" ? value : String(value ?? "");
  return text.length > maxLength ? `${text.slice(0, maxLength)}…` : text;
}

function renderOperationAliasEditor(node) {
  const section = el("section", "detail-section");
  section.append(el("div", "detail-section-title", "Project alias"));
  section.append(el("p", "detail-help", "A readable name for this operation in the selected project. The source route family and evidence IDs stay unchanged."));
  const input = el("input", "detail-alias-input");
  input.type = "text";
  input.maxLength = 96;
  input.autocomplete = "off";
  input.value = state.operationAliases.get(node.id)?.alias || "";
  input.placeholder = "e.g. Update account profile";
  input.setAttribute("aria-label", "Readable project alias for this operation");
  const button = el("button", "detail-action", "Save project alias");
  button.type = "button";
  button.addEventListener("click", () => { void saveOperationAlias(node, input, button); });
  section.append(input, button);
  elements.inspector.append(section);
}

function safeDetails(node) {
  const data = node.data || {};
  const candidates = [
    ["Method", data.method], ["Source", data.origin], ["Path", data.path ? displayPath(data.path) : undefined],
    ["Route family", data.route_family ? displayPath(data.route_family) : undefined], ["Family version", data.route_family_version],
    ["HTTP status", data.http_status ?? data.status], ["Record source", data.observation_kind ? readableObservationKind(data.observation_kind) : undefined],
    ["Time", data.observed_at],
    ["Content-Type", data.content_type], ["Headers", data.header_metadata], ["Field names", data.fields],
    ["Body size", data.body_bytes === null || data.body_bytes === undefined ? undefined : formatBytes(data.body_bytes)],
    ["Parse state", data.parse_state ? readableParseState(data.parse_state) : node.kind === "observation" ? "unknown" : undefined],
    ["Analysis state", data.analysis_state],
    ["Field type", data.type], ["Location", data.location],
    ["Field path", node.kind === "field_occurrence" ? data.path : undefined],
    ["Occurrence", node.kind === "field_occurrence" && Number.isInteger(data.occurrence_index) ? data.occurrence_index + 1 : undefined],
    ["Source bytes", node.kind === "field_occurrence" && data.source_span ? formatSourceSpan(data.source_span) : undefined],
    ["Value bytes", node.kind === "field_occurrence" && data.value_span ? formatSourceSpan(data.value_span) : undefined],
    ["Match basis", node.kind === "value_reference" ? "same project-keyed decoded value" : undefined],
    ["Match basis", node.kind === "url_reference" ? "same normalized URL within this project" : undefined],
    ["Context", data.context?.scenario],
  ];
  return candidates.filter(([, value]) => value !== undefined && value !== null && value !== "").map(([label, value]) => [label, value === "unknown" ? (label === "Time" ? "not specified" : "unknown") : formatValue(value)]);
}

function readableParseState(value) {
  return ({ complete: "complete", incomplete: "incomplete", partial: "partial", partial_prefix: "truncated", unknown_extent: "unknown length", unknown: "unknown" })[value] || value;
}

function readableObservationKind(value) {
  return ({ historical_capture: "Historical capture from Caido", live_capture: "Live capture" })[value] || value;
}

function renderProvenance(node) {
  const items = (state.graph?.capture_provenance || []).filter(item => item.graph_observation_id === node.id);
  const section = el("section", "detail-section");
  section.append(el("div", "detail-section-title", `Caido references · ${items.length}`));
  section.append(el("p", "detail-help", "Open a local historical revision to inspect exact stored bytes. Live content remains in Caido."));
  if (!items.length) {
    section.append(el("div", "provenance-item", "No local link to the source capture is attached."));
  } else {
    for (const item of items.slice(0, 12)) {
      const line = el("div", "provenance-item");
      line.append(el("b", "", `Request ID · ${item.request_id || "—"}`));
      line.append(el("span", "", `${readableRecordSource(item.record_source)} · ${formatBytes(item.request_size)}${item.first_seen_at ? ` · ${item.first_seen_at}` : ""}`));
      if (item.record_source === "encrypted_local_capture" && item.raw_stored_locally === true) {
        const revisionId = String(item.revision_id || "");
        const isBaseline = state.comparisonBaseline?.projectKey === state.projectKey &&
          state.comparisonBaseline.requestId === String(item.request_id) && state.comparisonBaseline.revisionId === revisionId;
        const baselineButton = el("button", "detail-action capture-baseline", isBaseline ? "Current baseline" : "Set as baseline");
        baselineButton.type = "button";
        baselineButton.disabled = isBaseline || !/^[a-f0-9]{64}$/.test(revisionId);
        baselineButton.addEventListener("click", () => setComparisonBaseline(item, baselineButton));
        const compareButton = el("button", "detail-action capture-compare", "Compare with baseline →");
        compareButton.type = "button";
        compareButton.disabled = !state.comparisonBaseline || state.comparisonBaseline.projectKey !== state.projectKey || isBaseline;
        compareButton.addEventListener("click", () => { void openComparison(item, compareButton); });
        line.append(baselineButton, compareButton);
        const button = el("button", "detail-action evidence-open", "Open stored HTTP →");
        button.type = "button";
        button.addEventListener("click", () => { void openEvidence(item, button); });
        line.append(button);
      } else {
        line.append(el("span", "evidence-unavailable", "Raw bytes are not stored in this local project."));
      }
      section.append(line);
    }
  }
  elements.inspector.append(section);
}

function renderAcceptedContextAnnotations(node) {
  const session = state.consultation;
  const annotations = BBGraphFocus.contextAnnotationsForObservation(
    node, state.graph?.capture_provenance, session, state.projectKey,
  );
  if (!annotations.length) return;

  const section = el("section", "detail-section");
  section.append(el("div", "detail-section-title", `Accepted analyst context · ${annotations.length}`));
  section.append(el("p", "detail-help", "Operator-provided labels are contextual metadata. They do not establish actor identity, ownership, causality, or effect."));
  for (const annotation of annotations) {
    const item = el("div", "provenance-item context-annotation-item");
    const version = annotation.annotationVersion === null ? "unknown" : String(annotation.annotationVersion);
    item.append(el("b", "", `${annotation.kind}: ${annotation.label} · analyst-provided`));
    item.append(el("span", "", `Consultation ${annotation.sessionID} · accepted version ${annotation.sessionVersion} · annotation version ${version}`));
    if (annotation.kind === "flow") item.append(el("span", "", "analyst-defined order; not observed causality"));
    for (const reference of annotation.evidenceReferences) {
      item.append(el("span", "", `${reference.flowStep ? `Flow step ${reference.flowStep} · ` : ""}Evidence ${reference.evidenceIndex + 1} · ${reference.side} · Caido request ${reference.requestId} · revision ${reference.revisionId} · reference ${reference.referenceID}`));
    }
    section.append(item);
  }
  elements.inspector.append(section);
}

function renderObservationRequestOrder(node) {
  const order = BBGraphFocus.observationRequestOrder(node, state.graph?.entities, state.graph?.assertions);
  if (!order) return;
  const section = el("section", "detail-section request-order-section");
  section.append(el("div", "detail-section-title", "Request ordering"));
  if (order.status === "supported") {
    for (const relation of order.relations) {
      const item = el("div", "provenance-item request-order-item");
      item.append(el("b", "", `Direction · ${relation.subjectID} → ${relation.objectID}`));
      item.append(el("span", "", `Basis · precedes · ${relation.originKind} / ${relation.evidenceState} · assertion ${relation.assertionID}`));
      for (const source of relation.sourceIDs) {
        item.append(el("span", "", `Source ID · SHA-256 ${source.sha256} · record locator ${source.recordLocator}`));
      }
      section.append(item);
    }
  }
  if (order.initiatingRequestIDs.length) {
    section.append(el("p", "detail-help", `Earlier request in recorded order · ${order.initiatingRequestIDs.join(", ")}`));
  } else {
    section.append(el("p", "detail-help", "Initiating request unknown in this graph slice"));
    section.append(el("p", "detail-help", order.status === "supported"
      ? "This slice has source-backed outgoing precedes links but no incoming link that identifies an earlier request."
      : "No source-backed precedes assertion with both exact observation IDs is available in this slice."));
  }
  section.append(el("p", "detail-help", "A precedes assertion records request order only. Timestamps and graph proximity do not prove causality."));
  elements.inspector.append(section);
}

function readableRecordSource(value) {
  return ({ encrypted_local_capture: "encrypted local capture", caido_history: "Caido history", local: "local record" })[value] || value || "local record";
}

function formatValue(value) {
  if (value === "unknown") return "unknown";
  if (typeof value === "string") return truncate(value, 180);
  if (Array.isArray(value)) return value.map(item => item === "unknown" ? "unknown" : String(item)).slice(0, 12).join(", ");
  if (typeof value === "object") return "structural context";
  return String(value);
}

function formatSourceSpan(span) {
  if (!span || !Number.isInteger(span.start) || !Number.isInteger(span.end) || span.start < 0 || span.end < span.start) {
    return "unavailable";
  }
  return `bytes [${span.start}, ${span.end})`;
}

function displayPath(value) {
  return String(value || "/")
    .replaceAll("{value}", "{parameter}")
    .replace(/(^|\/)segment_[a-f0-9]{16}(?=\/|$)/gi, "$1{parameter}");
}

function formatBytes(value) {
  const number = Number(value || 0);
  return number < 1024 ? `${number} B` : `${(number / 1024).toFixed(1)} KiB`;
}

function renderInspectorEmpty() {
  elements.inspector.replaceChildren();
  elements.inspectIndex.textContent = "—";
  const empty = el("div", "inspector-empty");
  empty.append(el("span", "inspect-target", "⌖"), el("strong", "", "Nothing selected"), el("span", "", "Select a graph node to view its properties and sources."));
  elements.inspector.append(empty);
}

function applyTransform() {
  const scene = elements.graphScene;
  const view = elements.graphCanvas.viewBox.baseVal;
  scene.setAttribute("transform", `translate(${state.panX} ${state.panY}) translate(${view.width / 2} ${view.height / 2}) scale(${state.zoom}) translate(${-view.width / 2} ${-view.height / 2})`);
  elements.zoom.textContent = `${Math.round(state.zoom * 100)}%`;
  elements.zoomIn.disabled = state.zoom >= MAX_GRAPH_ZOOM;
  elements.zoomOut.disabled = state.zoom <= MIN_GRAPH_ZOOM;
}

function setZoom(nextZoom, clientX, clientY) {
  const previousZoom = state.zoom;
  const zoom = Math.max(MIN_GRAPH_ZOOM, Math.min(MAX_GRAPH_ZOOM, nextZoom));
  if (zoom === previousZoom) return;
  if (Number.isFinite(clientX) && Number.isFinite(clientY)) {
    const svg = elements.graphCanvas;
    const ctm = svg.getScreenCTM();
    if (ctm) {
      const point = svg.createSVGPoint();
      point.x = clientX;
      point.y = clientY;
      const anchor = point.matrixTransform(ctm.inverse());
      const view = svg.viewBox.baseVal;
      state.panX += (previousZoom - zoom) * (anchor.x - view.width / 2);
      state.panY += (previousZoom - zoom) * (anchor.y - view.height / 2);
    }
  }
  state.zoom = zoom;
  applyTransform();
}

function stepZoom(direction) {
  const step = state.zoom < 2 ? 0.25 : state.zoom < 5 ? 0.5 : 1;
  setZoom(state.zoom + direction * step);
}

function setGraphFocus(enabled) {
  state.graphFocus = Boolean(enabled);
  elements.workspace.classList.toggle("graph-focus", state.graphFocus);
  elements.focusGraph.setAttribute("aria-pressed", String(state.graphFocus));
  elements.focusGraph.querySelector("span").textContent = state.graphFocus ? "Panels" : "Expand";
  elements.focusGraph.title = state.graphFocus ? "Show catalog and inspector" : "Expand graph to full screen";
  requestAnimationFrame(applyTransform);
}

function short(value, max) { const text = String(value || ""); return text.length > max ? `${text.slice(0, max - 1)}…` : text; }
function truncate(value, max) { return short(value, max); }
function safeClass(value) { return String(value).replace(/[^a-z0-9_-]/gi, "-"); }

elements.project.addEventListener("change", () => {
  if (elements.project.value === "__load_more__") {
    void loadProjects(true);
    return;
  }
  void selectProject(elements.project.value);
});
elements.operationBackfillOpen.addEventListener("click", () => {
  if (!state.projectKey) return;
  elements.operationBackfillDialog.showModal();
  clearOperationBackfillPreview();
});
elements.operationBackfillClose.addEventListener("click", () => elements.operationBackfillDialog.close());
elements.operationBackfillDialog.addEventListener("close", () => elements.operationBackfillOpen.focus());
elements.operationBackfillPreview.addEventListener("click", () => { void previewOperationBackfill(); });
elements.operationBackfillApply.addEventListener("click", () => { void applyOperationBackfill(); });
elements.operationBackfillMaxCaptures.addEventListener("input", () => clearOperationBackfillPreview("Limits changed; request a new preview before applying."));
elements.operationBackfillMaxBytes.addEventListener("input", () => clearOperationBackfillPreview("Limits changed; request a new preview before applying."));
elements.filter.addEventListener("input", () => {
  const filter = elements.filter.value.trim();
  if (filter === state.surfaceFilter) return;
  state.surfaceFilter = filter;
  resetSurfaceRequest();
  state.endpoints = [];
  state.endpointCursor = null;
  state.totalOperations = 0;
  state.selectedEndpoint = null;
  state.graph = null;
  state.selectedNode = null;
  state.focusedNodeId = null;
  resetGraphNavigation();
  elements.graphTitle.textContent = "Observed behavior relationships";
  elements.graphSubtitle.textContent = "Select an operation to open its graph neighborhood.";
  elements.graphCounts.replaceChildren(countText(0, "nodes"), el("b"), countText(0, "edges"));
  renderInspectorEmpty();
  renderEmpty("Searching local operations", "Results match host, method, route, field names, or header names.");
  elements.surfaceFoot.textContent = "Updating local operation filter…";
  elements.endpointCount.textContent = "0";
  renderEndpointList();
  if (!state.projectKey) return;
  setStatus("Searching local operations", "loading");
  state.surfaceDebounce = setTimeout(() => {
    state.surfaceDebounce = null;
    void loadSurface(false);
  }, 250);
});
elements.loadMore.addEventListener("click", () => loadSurface(true));
elements.captureSelect.addEventListener("change", () => { void loadGraph([elements.captureSelect.value], { focusNodeId: elements.captureSelect.value, remember: true }); });
elements.graphBack.addEventListener("click", () => {
  const previous = state.graphHistory.pop();
  if (!previous) return;
  state.graphAbort?.abort(); state.graphRequestId++; state.loadingGraph = false;
  restoreGraphNavigation(previous);
});
elements.graphMore.addEventListener("click", () => {
  const cursor = state.graph?.next_cursor;
  if (cursor) return loadGraph(state.graphSeeds, { view: state.graphView, cursor, appendPage: true, remember: true });
});
elements.graphDeeper.addEventListener("click", () => { void loadGraph(state.graphSeeds, { view: "full", remember: true }); });
$("#refresh-button").addEventListener("click", () => loadProjects());
elements.zoomIn.addEventListener("click", () => stepZoom(1));
elements.zoomOut.addEventListener("click", () => stepZoom(-1));
$("#fit-graph").addEventListener("click", () => { state.zoom = 1; state.panX = 0; state.panY = 0; applyTransform(); });
elements.focusGraph.addEventListener("click", () => setGraphFocus(!state.graphFocus));
elements.graphOverview?.addEventListener("click", () => selectGraphView("overview"));
elements.graphShowAll?.addEventListener("click", () => selectGraphView("all"));
elements.edgeLabels?.addEventListener("click", () => {
  state.edgeLabelsVisible = !state.edgeLabelsVisible;
  elements.edgeLabels.setAttribute("aria-pressed", String(state.edgeLabelsVisible));
  elements.edgeLabels.setAttribute("aria-label", state.edgeLabelsVisible ? "Hide edge labels" : "Show edge labels");
  elements.edgeLabels.title = state.edgeLabelsVisible ? "Hide edge labels" : "Show edge labels";
  renderGraph(false);
});
elements.evidenceClose.addEventListener("click", () => clearEvidence());
elements.evidenceDialog.addEventListener("close", () => {
  const trigger = state.evidenceTrigger;
  clearEvidence(false);
  if (trigger?.isConnected) trigger.focus();
});
elements.evidenceRequest.addEventListener("click", () => selectEvidenceSide("request"));
elements.evidenceResponse.addEventListener("click", () => selectEvidenceSide("response"));
elements.evidenceMore.addEventListener("click", () => { void loadEvidenceRange(true); });
elements.comparisonAttachButton.addEventListener("click", () => { void attachComparisonToConsultation(); });
elements.evidencePacketQuestion.addEventListener("input", () => {
  state.evidencePacketDraft.question = elements.evidencePacketQuestion.value;
  invalidateEvidencePacketPreview();
});
elements.evidencePacketOffset.addEventListener("input", invalidateEvidencePacketPreview);
elements.evidencePacketLength.addEventListener("input", invalidateEvidencePacketPreview);
elements.evidencePacketAdd.addEventListener("click", addCurrentEvidenceSpan);
elements.evidencePacketClear.addEventListener("click", clearEvidencePacketDraft);
elements.evidencePacketPreview.addEventListener("click", () => { void previewEvidencePacket(); });
elements.consultationCreate.addEventListener("click", () => { void createOrReviewConsultation(); });
elements.consultationResumeID.addEventListener("input", () => {
  state.consultationResumeRequestId++;
  updateConsultationResumeControl();
});
elements.consultationResume.addEventListener("click", () => { void resumeConsultationByID(); });
elements.consultationOpen.addEventListener("click", openConsultationResume);
elements.consultationAccept.addEventListener("click", () => { void acceptConsultationReview(); });
elements.candidateSave.addEventListener("click", () => { void saveConsultationCandidate(); });
for (const field of candidateProposalFields()) field.addEventListener("input", updateCandidateControl);
elements.consultationContextKind.addEventListener("change", () => {
  elements.consultationContextLabel.placeholder = `${elements.consultationContextKind.value.replaceAll("_", "-")}-a`;
  const selected = selectedContextEvidenceReferenceIDs();
  if (elements.consultationContextKind.value === "flow") {
    state.contextFlowOrder = orderedContextEvidenceReferenceIDs(selected, state.contextEvidenceReferences, "flow", state.contextFlowOrder);
  }
  renderContextEvidenceOptions(selected);
  state.contextAnnotationKey = "";
  updateContextAnnotationControl();
});
elements.consultationContextLabel.addEventListener("input", () => {
  state.contextAnnotationKey = "";
  updateContextAnnotationControl();
});
elements.consultationContextSave.addEventListener("click", () => { void saveContextAnnotation(); });
elements.consultationCopyID.addEventListener("click", async () => {
  if (!state.consultation?.id) return;
  const resumeArguments = JSON.stringify({ project_key: state.projectKey, session_id: state.consultation.id });
  try {
    await navigator.clipboard.writeText(resumeArguments);
    showToast("Resume arguments copied. In Hermes, call bb_consult_resume with them.");
  } catch {
    showToast(`Hermes resume arguments: ${resumeArguments}`);
  }
});
elements.graphCanvas.addEventListener("pointerdown", event => {
  if (event.target.closest?.(".node-group")) return;
  event.preventDefault();
  const view = elements.graphCanvas.viewBox.baseVal;
  const rect = elements.graphCanvas.getBoundingClientRect();
  state.drag = { x: event.clientX, y: event.clientY, scaleX: view.width / rect.width, scaleY: view.height / rect.height, panX: state.panX, panY: state.panY, pointer: event.pointerId };
  elements.graphCanvas.setPointerCapture(event.pointerId);
});
elements.graphCanvas.addEventListener("pointermove", event => {
  if (!state.drag || state.drag.pointer !== event.pointerId) return;
  state.panX = state.drag.panX + (event.clientX - state.drag.x) * state.drag.scaleX;
  state.panY = state.drag.panY + (event.clientY - state.drag.y) * state.drag.scaleY;
  applyTransform();
});
elements.graphCanvas.addEventListener("pointerup", event => {
  if (state.drag?.pointer === event.pointerId) state.drag = null;
});
elements.graphCanvas.addEventListener("wheel", event => {
  if (!state.graph) return;
  event.preventDefault();
  setZoom(state.zoom * (event.deltaY < 0 ? 1.15 : 1 / 1.15), event.clientX, event.clientY);
}, { passive: false });
document.addEventListener("keydown", event => {
  if (event.key === "/" && !["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement?.tagName)) { event.preventDefault(); elements.filter.focus(); }
  if (["+", "="].includes(event.key) && !["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement?.tagName)) { stepZoom(1); }
  if (event.key === "-" && !["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement?.tagName)) { stepZoom(-1); }
  if (event.key === "0" && !["INPUT", "TEXTAREA"].includes(document.activeElement?.tagName)) { state.zoom = 1; state.panX = 0; state.panY = 0; applyTransform(); }
  if (event.key === "Escape" && state.graphFocus) setGraphFocus(false);
});

void loadRuntimeCapabilities();
loadProjects();
