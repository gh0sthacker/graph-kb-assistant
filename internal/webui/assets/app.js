"use strict";

const $ = (selector, root = document) => root.querySelector(selector);
const MAX_GRAPH_ZOOM = 10;
const MIN_GRAPH_ZOOM = 0.35;
const MAX_EVIDENCE_PREVIEW_BYTES = 2 << 20;
const OVERVIEW_HIDDEN_KINDS = ["observation", "request_variant", "response_variant"];
const state = {
  projects: [], projectCursor: null, projectKey: "", endpoints: [], endpointCursor: null, surfaceGaps: [],
  surfaceFilter: "", surfaceDebounce: null, totalOperations: 0,
  graph: null, selectedEndpoint: null, selectedNode: null, focusedNodeId: null, graphFocus: false, loadingSurface: false,
  surfaceRequestId: 0, surfaceAbort: null,
  graphSeeds: [], graphView: "direct", graphHistory: [], graphRequestId: 0, graphAbort: null,
  loadingGraph: false, hiddenKinds: new Set(OVERVIEW_HIDDEN_KINDS), routeScope: true, edgeLabelsVisible: true,
  renderedEntities: [], renderedAssertions: [], zoom: 1, panX: 0, panY: 0, drag: null, toastTimer: null,
  evidence: null, evidenceRequestId: 0, evidenceTrigger: null, packetPreviewRequestId: 0,
  evidencePacketDraft: { projectKey: "", question: "", selections: [] }, comparisonBaseline: null, comparison: null,
  evidencePacketPreview: null, consultation: null, consultationCreateKey: "",
};

const elements = {
  project: $("#project-select"), endpointList: $("#endpoint-list"), endpointCount: $("#endpoint-count"),
  filter: $("#endpoint-filter"), loadMore: $("#load-more"), surfaceFoot: $("#surface-foot-text"),
  graphCanvas: $("#graph-canvas"), graphScene: $("#graph-scene"), canvasEmpty: $("#canvas-empty"),
  emptyTitle: $("#canvas-empty-title"), emptyCopy: $("#canvas-empty-copy"), graphState: $("#graph-state"),
  graphTitle: $("#graph-title"), graphSubtitle: $("#graph-subtitle"), graphProject: $("#graph-project-label"),
  graphCounts: $("#graph-counts"), inspector: $("#inspector-content"), inspectIndex: $("#inspect-index"),
  graphFilters: $("#graph-filters"), graphFilterSummary: $("#graph-filter-summary"),
  graphOverview: $("#graph-overview"), graphShowAll: $("#graph-show-all"), edgeLabels: $("#toggle-edge-labels"),
  status: $("#status-text"), coverage: $("#coverage-status"), updated: $("#last-updated"),
  zoom: $("#zoom-readout"), zoomIn: $("#zoom-in"), zoomOut: $("#zoom-out"),
  focusGraph: $("#focus-graph"), workspace: $(".workspace"),
  captureSelect: $("#capture-select"), graphBack: $("#graph-back"), graphMore: $("#graph-more"), graphDeeper: $("#graph-deeper"),
  toast: $("#toast"), viewport: $("#graph-viewport"),
  evidenceDialog: $("#evidence-dialog"), evidenceClose: $("#evidence-close"),
  evidenceLabel: $("#evidence-capture-label"), evidenceMeta: $("#evidence-meta"), evidenceContent: $("#evidence-content"),
  evidenceFoot: $("#evidence-foot"), evidenceMore: $("#evidence-more"), evidenceCompareContent: $("#evidence-compare-content"),
  evidenceTitle: $("#evidence-title"), evidenceSides: $(".evidence-sides"),
  evidencePacketControls: $("#evidence-packet-controls"), evidencePacketQuestion: $("#evidence-packet-question"),
  evidencePacketOffset: $("#evidence-packet-offset"), evidencePacketLength: $("#evidence-packet-length"),
  evidencePacketAdd: $("#evidence-packet-add"), evidencePacketSelectionCount: $("#evidence-packet-selection-count"),
  evidencePacketSelectionList: $("#evidence-packet-selection-list"), evidencePacketClear: $("#evidence-packet-clear"),
  evidencePacketPreview: $("#evidence-packet-preview"), evidencePacketContent: $("#evidence-packet-content"),
  consultationCreate: $("#consultation-create"), consultationCopyID: $("#consultation-copy-id"), consultationStatus: $("#consultation-status"),
  consultationReview: $("#consultation-review"), consultationReviewContent: $("#consultation-review-content"),
  consultationAccept: $("#consultation-accept"),
  evidenceRequest: $("#evidence-request"), evidenceResponse: $("#evidence-response"),
};

const nodeLabels = {
  observation: "HTTP REQUEST", endpoint: "ENDPOINT", parameter: "PARAMETER",
  asset: "HOST", actor: "ACTOR", object: "OBJECT", session_context: "SESSION CONTEXT",
  condition: "CONDITION", flow: "FLOW", classifier_signal: "SIGNAL",
  hypothesis: "HYPOTHESIS", request_variant: "REQUEST SHAPE", response_variant: "RESPONSE SHAPE",
};
const filterLabels = {
  observation: "Requests", endpoint: "Endpoints", parameter: "Parameters", asset: "Hosts",
  actor: "Actors", object: "Objects", session_context: "Contexts",
  condition: "Conditions", flow: "Flows", classifier_signal: "Signals",
  hypothesis: "Hypotheses", request_variant: "Request shapes", response_variant: "Response shapes",
};
const relationLabels = {
  exposes: "serves endpoint",
  invokes: "calls endpoint",
  has_parameter: "has parameter",
  returns: "returns parameter",
  uses_request_variant: "uses request shape",
  received_response: "received response shape",
  supports: "supports hypothesis",
  contradicts: "contradicts hypothesis",
  contains: "contains",
  precedes: "precedes",
};
const MASKED_PATH_HELP = "Dynamic path segments are masked because they can contain identifiers or secrets. Raw HTTP values remain in Caido.";
const kindColors = { observation: "#83c8cf", endpoint: "#e0b56b", parameter: "#b8a3da", asset: "#b2d88a" };

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

function clearEvidence(closeDialog = true) {
  state.evidenceRequestId++;
  state.packetPreviewRequestId++;
  state.evidence = null;
  state.comparison = null;
  state.evidencePacketPreview = null;
  resetConsultationUI("Preview the selected evidence before creating a consultation.");
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
  if (!state.projectKey || !requestId || requestId.length > 512 || item?.record_source !== "encrypted_local_capture" || item?.raw_stored_locally !== true) {
    showToast("Exact local HTTP bytes are unavailable for this record.");
    return Promise.resolve();
  }
  clearEvidence(false);
  const project = state.projectKey;
  const requestID = ++state.evidenceRequestId;
  state.evidence = {
    projectKey: project, requestId, requestID, revisionId: typeof item.revision_id === "string" ? item.revision_id : "",
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
  const requestID = evidence.requestID;
  evidence.loading = true;
  elements.evidenceMore.disabled = true;
  elements.evidenceFoot.textContent = append ? "Loading the next authenticated range…" : "Loading the authenticated message range…";
  const query = new URLSearchParams({
    project_key: evidence.projectKey, request_id: evidence.requestId, side: evidence.side,
    offset: String(append ? evidence.nextOffset : 0), max_bytes: String(1 << 20),
  });
  if (evidence.revisionId) query.set("revision_id", evidence.revisionId);
  try {
    const payload = await getJSON(`/api/evidence?${query}`);
    if (state.evidence !== evidence || requestID !== state.evidenceRequestId || evidence.projectKey !== state.projectKey) return;
    evidence.revisionId = payload.revision_id || evidence.revisionId;
    evidence.totalSize = Number(payload.total_size || 0);
    if (!evidence.packetDefaultsInitialized) {
      elements.evidencePacketOffset.value = "0";
      elements.evidencePacketLength.value = evidence.totalSize > 0 ? String(Math.min(4096, evidence.totalSize)) : "";
      evidence.packetDefaultsInitialized = true;
    }
    evidence.nextOffset = payload.next_offset === null || payload.next_offset === undefined ? null : Number(payload.next_offset);
    evidence.loadedBytes = (evidence.loadedBytes || 0) + Number(payload.returned_size || 0);
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
  elements.consultationCreate.disabled = !state.evidencePacketPreview || evidence?.packetPreviewLoading || Boolean(state.consultation?.reviewLoaded);
  elements.consultationCreate.textContent = state.consultation ? "Reload saved review" : "Save session for review";
}

function newIdempotencyKey() {
  const bytes = crypto.getRandomValues(new Uint8Array(24));
  return [...bytes].map(value => value.toString(16).padStart(2, "0")).join("");
}

function resetConsultationUI(message) {
  state.consultation = null;
  state.consultationCreateKey = "";
  elements.consultationStatus.textContent = message;
  elements.consultationReview.hidden = true;
  elements.consultationReviewContent.textContent = "";
  elements.consultationAccept.disabled = true;
  elements.consultationAccept.hidden = false;
  elements.consultationCopyID.hidden = true;
  elements.consultationCreate.disabled = true;
  elements.consultationCreate.textContent = "Save session for review";
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
    }
    const session = state.consultation;
    const query = new URLSearchParams({ project_key: preview.projectKey, session_id: session.id });
    const review = await getJSON(`/api/consultation/review?${query}`);
    if (state.evidencePacketPreview !== preview || state.projectKey !== preview.projectKey || state.consultation !== session) return;
    session.version = review.version;
    session.accepted = Boolean(review.review_accepted);
    session.reviewLoaded = true;
    elements.consultationReviewContent.textContent = JSON.stringify({
      consultation_id: review.consultation_id, version: review.version,
      question: review.question, evidence: review.evidence,
    }, null, 2);
    elements.consultationReview.hidden = false;
    elements.consultationAccept.hidden = session.accepted;
    elements.consultationAccept.disabled = session.accepted;
    elements.consultationCopyID.hidden = false;
    elements.consultationStatus.textContent = session.accepted
      ? `Accepted · consultation ${session.id} · Hermes can now resume this session.`
      : `Saved locally · consultation ${session.id} · Hermes cannot read this packet until you accept it below.`;
    updateEvidencePacketControl();
  } catch (error) {
    elements.consultationStatus.textContent = `Could not load the saved review: ${error.message}`;
    elements.consultationCreate.disabled = false;
    if (state.consultation) elements.consultationCreate.textContent = "Reload saved review";
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
    elements.consultationAccept.hidden = true;
    elements.consultationStatus.textContent = `Accepted · consultation ${session.id} · Hermes can now resume this session.`;
  } catch (error) {
    elements.consultationStatus.textContent = `Approval was not recorded: ${error.message}`;
    elements.consultationAccept.disabled = false;
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
  state.comparisonBaseline = { projectKey: state.projectKey, requestId, revisionId };
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
  clearEvidence(false);
  const comparison = {
    projectKey: state.projectKey, leftRequestId: baseline.requestId, leftRevisionId: baseline.revisionId,
    rightRequestId, rightRevisionId,
  };
  state.comparison = comparison;
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
    elements.evidenceMeta.textContent = `Exact revisions · ${comparison.leftRevisionId} → ${comparison.rightRevisionId}`;
    elements.evidenceCompareContent.textContent = formatCaptureComparison(result);
    elements.evidenceFoot.textContent = result.coverage_note || "Exact local byte comparison; unavailable or incomplete messages are identified per side.";
  } catch (error) {
    if (state.comparison !== comparison || requestID !== state.evidenceRequestId) return;
    elements.evidenceMeta.textContent = "The selected revisions could not be compared.";
    elements.evidenceCompareContent.textContent = "";
    elements.evidenceFoot.textContent = error.message;
    showToast(`Capture comparison unavailable: ${error.message}`);
  }
}

function formatCaptureComparison(result) {
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
  return [renderSide("Request", result.request), renderSide("Response", result.response)].join("\n\n");
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

function clearProjectView() {
  state.evidencePacketDraft = { projectKey: "", question: "", selections: [] };
  clearEvidence();
  state.comparisonBaseline = null;
  resetSurfaceRequest();
  resetGraphNavigation();
  state.projectKey = "";
  state.projectCursor = null;
  state.endpoints = [];
  state.endpointCursor = null;
  state.surfaceFilter = elements.filter.value.trim();
  state.totalOperations = 0;
  state.surfaceGaps = [];
  state.graph = null;
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
  }
  resetSurfaceRequest();
  resetGraphNavigation();
  state.projectKey = projectKey;
  state.endpoints = [];
  state.endpointCursor = null;
  state.surfaceFilter = elements.filter.value.trim();
  state.totalOperations = 0;
  state.surfaceGaps = [];
  state.graph = null;
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
      renderEmpty(filter ? "No matching operations" : "No operations in the local index", filter ? "Change the search text to see other host, method, and route combinations." : "Import HTTP history for the selected project, then refresh.");
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
    ? "Continue rendered omitted assertion branches in the pinned graph generation"
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
    state.graphSeeds = [...seeds];
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
  if (current.continuation_scope === "rendered_missing_assertion_branches" || next.continuation_scope === "rendered_missing_assertion_branches") return next;
  const merged = { ...current, ...next };
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
  const neighbors = new Map(entities.map(node => [node.id, []]));
  for (const edge of assertions) {
    if (ids.has(edge.subject) && ids.has(edge.object)) {
      neighbors.get(edge.subject).push(edge.object);
      neighbors.get(edge.object).push(edge.subject);
    }
  }
  const seedIds = (state.graph?.seed_ids || []).filter(id => ids.has(id));
  const selectedEndpoint = entities.find(isSelectedEndpointNode);
  const root = (state.focusedNodeId && ids.has(state.focusedNodeId) ? state.focusedNodeId : null) || seedIds[0] || selectedEndpoint?.id || entities[0]?.id;
  const distances = new Map([[root, 0]]), queue = [root];
  for (let i = 0; i < queue.length; i++) {
    for (const neighbor of neighbors.get(queue[i]) || []) {
      if (distances.has(neighbor)) continue;
      distances.set(neighbor, distances.get(queue[i]) + 1); queue.push(neighbor);
    }
  }
  const byDepth = new Map();
  for (const item of entities) {
    const depth = distances.get(item.id) ?? 0;
    if (!byDepth.has(depth)) byDepth.set(depth, []);
    byDepth.get(depth).push(item);
  }
  for (const group of byDepth.values()) group.sort((a, b) => displayTitle(a).localeCompare(displayTitle(b)));
  const maxDepth = Math.max(...byDepth.keys(), 1);
  const maxRows = Math.max(...[...byDepth.values()].map(group => group.length), 1);
  const width = Math.max(960, 240 + maxDepth * 225);
  const height = Math.max(560, 140 + maxRows * 128);
  const positions = new Map();
  for (const [depth, group] of byDepth) {
    const span = group.length * 128;
    const startY = (height - span) / 2 + 64;
    group.forEach((item, index) => positions.set(item.id, { x: 120 + depth * ((width - 240) / Math.max(1, maxDepth)), y: startY + index * 128 }));
  }
  return { positions, width, height };
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
  if (node.kind === "observation") return `${data.observation_kind === "live_capture" ? "Live capture" : "Capture"}${data.http_status ? ` · HTTP ${data.http_status}` : ""}`;
  if (node.kind === "parameter") return displayPath(data.path || data.name || "Parameter");
  if (node.kind === "request_variant") return "Request shape";
  if (node.kind === "response_variant") return "Response shape";
  if (node.kind === "asset") return data.origin || "Host";
  return data.label || data.name || node.kind || "Entity";
}

function displaySubtitle(node) {
  const data = node.data || {};
  if (node.kind === "observation") return data.observed_at && data.observed_at !== "unknown" ? data.observed_at : "Capture time unavailable";
  if (node.kind === "endpoint") return (data.query_names || []).slice(0, 4).join(" · ") || "Route template · values masked";
  if (node.kind === "parameter") return [data.type, data.location].filter(Boolean).join(" · ") || "Structural field";
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
  renderInspector(node);
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
    note.append(el("span", "", "This record represents one saved request from Caido. HTTP 200 is only the response status; it does not indicate an error or a complete parse."));
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
    elements.inspector.append(el("p", "detail-help", "This is a grouped route template. Dynamic values are not stored in the graph; find the exact request in Caido using the linked capture's request ID."));
  }
  if (node.kind === "observation") renderProvenance(node);
  const action = el("button", "detail-action", "Load direct relationships →");
  action.type = "button";
  action.disabled = !/^[a-z][a-z0-9_]*-[a-f0-9]{64}$/.test(node.id || "");
  action.addEventListener("click", () => { void focusOnNode(node); });
  elements.inspector.append(action);
  elements.inspector.append(el("p", "detail-help", "Loads this node and its immediate connections. Use Back to return to the previous graph."));
}

function safeDetails(node) {
  const data = node.data || {};
  const candidates = [
    ["Method", data.method], ["Source", data.origin], ["Path", data.path ? displayPath(data.path) : undefined],
    ["HTTP status", data.http_status ?? data.status], ["Record source", data.observation_kind ? readableObservationKind(data.observation_kind) : undefined],
    ["Time", data.observed_at],
    ["Content-Type", data.content_type], ["Headers", data.header_metadata], ["Field names", data.fields],
    ["Body size", data.body_bytes === null || data.body_bytes === undefined ? undefined : formatBytes(data.body_bytes)],
    ["Parse state", readableParseState(data.parse_state || data.analysis_state)],
    ["Field type", data.type], ["Location", data.location],
    ["Context", data.context?.scenario],
  ];
  return candidates.filter(([, value]) => value !== undefined && value !== null && value !== "").map(([label, value]) => [label, value === "unknown" ? (label === "Time" ? "not specified" : "unknown") : formatValue(value)]);
}

function readableParseState(value) {
  return ({ complete: "complete", incomplete: "incomplete", partial_prefix: "truncated", unknown_extent: "unknown length", unknown: "unknown" })[value] || value;
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
  renderEmpty("Searching local operations", "Results are filtered by host, method, and route.");
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
elements.consultationAccept.addEventListener("click", () => { void acceptConsultationReview(); });
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

loadProjects();
