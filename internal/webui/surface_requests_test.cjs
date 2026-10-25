const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const { webcrypto } = require("node:crypto");
const BBGraphFocus = require("./assets/graph-focus.js");

// Exercise the shipped request/state functions, including their real rendering
// calls, with an in-memory DOM and a manually completed fetch transport. The
// transport deliberately ignores abort so stale-response guards are required.
function createApp() {
  const nodes = new Map();
  function node() {
    return {
      textContent: "", value: "", hidden: false, disabled: false, open: false, isConnected: true, focused: false, children: [],
      options: [], listeners: {}, dataset: {},
      classList: { add() {}, remove() {}, toggle() {} },
      append(...children) { this.children.push(...children); },
      replaceChildren(...children) { this.children = children; },
      setAttribute() {}, addEventListener(name, callback) { this.listeners[name] = callback; },
      showModal() { this.open = true; },
      close() { this.open = false; this.listeners.close?.(); },
      focus() { this.focused = true; },
      querySelector() { return node(); },
    };
  }
  const document = {
    querySelector(selector) {
      if (!nodes.has(selector)) nodes.set(selector, node());
      return nodes.get(selector);
    },
    createElement: node, createElementNS: node,
    createTextNode: text => ({ textContent: text }),
    addEventListener() {},
  };
  const requests = [];
  const context = vm.createContext({
    document, URLSearchParams, AbortController, crypto: webcrypto, BBGraphFocus, setTimeout, clearTimeout,
    fetch(url, options) {
      const parsedURL = new URL(url, "http://local.test");
      if (parsedURL.pathname === "/api/consultation/operation-aliases") {
        return Promise.resolve({ ok: true, json: async () => [] });
      }
      return new Promise((resolve, reject) => {
        requests.push({
          url: parsedURL, signal: options.signal,
          method: options.method || "GET", headers: options.headers || {}, body: options.body,
          resolve(data) { resolve({ ok: true, json: async () => data }); },
          fail(status, message) { resolve({ ok: false, status, json: async () => ({ error: message }) }); },
          reject,
        });
      });
    },
  });
  const source = fs.readFileSync(path.join(__dirname, "assets/app.js"), "utf8");
  assert.match(source, /\nvoid loadRuntimeCapabilities\(\);\nloadProjects\(\);\s*$/);
  vm.runInContext(source.replace(/\nvoid loadRuntimeCapabilities\(\);\nloadProjects\(\);\s*$/, "") +
    "\nglobalThis.app = { state, elements, selectProject, clearProjectView, loadSurface, mergeGraphPages, loadGraph, openEvidence, selectEvidenceSide, addCurrentEvidenceSpan, removeEvidencePacketSelection, clearEvidencePacketDraft, previewEvidencePacket, setComparisonBaseline, openComparison, attachComparisonToConsultation, formatCaptureComparison, displayTitle, displaySubtitle, safeDetails, nodeLabels, relationLabels, saveOperationAlias, renderInspector, renderConsultationContext, resumeConsultationByID, acceptConsultationReview, saveConsultationCandidate, continueConsultationCandidateProposal, updateCandidateControl, renderConsultationCandidate, updateGraphMoreControl, clickGraphMore: () => elements.graphMore.listeners.click?.() };", context);
  const app = context.app;
  app.state.projects = ["A", "B"].map(project_key => ({ project_key, display_name: project_key }));
  return { ...app, requests };
}

function endpoint(host, route = "/route", count = 1) {
  return {
    host, port: 443, method: "GET", route, observation_count: count,
    field_names: ["id"], header_names: ["accept"], sampled_graph_seed_ids: [`${host}-seed`],
  };
}

function page(host, extra = {}) {
  return { operations: [endpoint(host)], total_operations: 1, coverage_state: "partial", coverage_gaps: [`${host}-gap`], ...extra };
}

function snapshot(value) { return JSON.parse(JSON.stringify(value)); }

test("saved-consultation entry point opens the review panel without selecting another capture", () => {
  const app = createApp();
  app.elements.consultationOpen.listeners.click();
  assert.equal(app.elements.evidenceDialog.open, false);
  app.state.projectKey = "a".repeat(64);
  app.elements.consultationOpen.listeners.click();
  assert.equal(app.elements.evidenceDialog.open, true);
  assert.equal(app.elements.consultationResumeID.focused, true);
});

test("saved-consultation entry point is enabled after the first project is selected", async () => {
  const app = createApp();
  const projectKey = "b".repeat(64);
  const selecting = app.selectProject(projectKey);
  assert.equal(app.state.projectKey, projectKey);
  assert.equal(app.elements.consultationOpen.disabled, false);

  const surface = app.requests.find(request => request.url.pathname === "/api/surface");
  surface.resolve({ operations: [], total_operations: 0, coverage_state: "partial", coverage_gaps: [] });
  await selecting;
});

test("field value correlation nodes show exact spans and explain equality without exposing the signature", () => {
  const app = createApp();
  const signature = "hmac:synthetic-private-equality-signature";
  const occurrence = {
    id: "field_occurrence-" + "a".repeat(64), kind: "field_occurrence",
    data: {
      path: "query.account_id", location: "query", occurrence_index: 1,
      source_span: { start: 12, end: 34 }, value_span: { start: 23, end: 34 },
    },
  };
  const valueMatch = {
    id: "value_reference-" + "b".repeat(64), kind: "value_reference",
    data: { equality_signature: signature },
  };

  assert.equal(app.nodeLabels.field_occurrence, "FIELD OCCURRENCE");
  assert.equal(app.nodeLabels.value_reference, "VALUE MATCH");
  assert.equal(app.displayTitle(occurrence), "query.account_id");
  assert.match(app.displaySubtitle(occurrence), /occurrence 2 · bytes \[23, 34\)/);
  assert.equal(app.relationLabels.value_used_in, "same project-keyed value; causality unknown");
  assert.deepEqual(snapshot(app.safeDetails(occurrence)).filter(([label]) => /bytes|Occurrence|Field path/.test(label)), [
    ["Field path", "query.account_id"], ["Occurrence", "2"],
    ["Source bytes", "bytes [12, 34)"], ["Value bytes", "bytes [23, 34)"],
  ]);
  assert.equal(JSON.stringify(snapshot(app.safeDetails(valueMatch))).includes(signature), false);

  app.renderInspector(valueMatch);
  const renderedText = JSON.stringify(snapshot(app.elements.inspector.children));
  assert.match(renderedText, /correlation only/);
  assert.match(renderedText, /does not establish causality/);
  assert.equal(renderedText.includes(signature), false);
});

test("saved consultation resumes by ID after the original evidence selection is gone", async () => {
  const app = createApp();
  app.state.projectKey = "a".repeat(64);
  app.elements.consultationResumeID.value = "saved-session-123";

  const resuming = app.resumeConsultationByID();
  const request = app.requests.at(-1);
  assert.equal(request.method, "GET");
  assert.equal(request.url.pathname, "/api/consultation/review");
  assert.equal(request.url.searchParams.get("project_key"), app.state.projectKey);
  assert.equal(request.url.searchParams.get("session_id"), "saved-session-123");
  request.resolve({
    consultation_id: "saved-session-123", version: 4, review_accepted: true,
    question: "Compare the saved synthetic requests.", evidence: { evidence: [] },
    evidence_references: [], context_annotations: [],
  });
  await resuming;

  assert.equal(app.state.consultation.id, "saved-session-123");
  assert.equal(app.state.consultation.version, 4);
  assert.equal(app.state.consultation.accepted, true);
  assert.equal(app.state.evidencePacketPreview.projectKey, app.state.projectKey);
  assert.equal(app.elements.consultationReview.hidden, false);
  assert.match(app.elements.consultationReviewContent.textContent, /Compare the saved synthetic requests/);
  assert.equal(app.elements.consultationAccept.hidden, true);
});

test("resuming an unaccepted consultation keeps the explicit Hermes review gate", async () => {
  const app = createApp();
  app.state.projectKey = "a".repeat(64);
  app.elements.consultationResumeID.value = "pending-session-456";
  const resuming = app.resumeConsultationByID();
  app.requests.at(-1).resolve({
    consultation_id: "pending-session-456", version: 2, review_accepted: false,
    question: "Review before access.", evidence: { evidence: [] },
    evidence_references: [], context_annotations: [],
  });
  await resuming;

  assert.equal(app.state.consultation.accepted, false);
  assert.equal(app.elements.consultationAccept.hidden, false);
  assert.equal(app.elements.consultationAccept.disabled, false);
  assert.match(app.elements.consultationStatus.textContent, /review it before allowing Hermes access/);
});

test("resume accepts the exact JSON arguments copied for Hermes", async () => {
  const app = createApp();
  app.state.projectKey = "a".repeat(64);
  app.elements.consultationResumeID.value = JSON.stringify({
    project_key: app.state.projectKey, session_id: "copied-session-789",
  });
  const resuming = app.resumeConsultationByID();
  const request = app.requests.at(-1);
  assert.equal(request.url.searchParams.get("session_id"), "copied-session-789");
  request.resolve({
    consultation_id: "copied-session-789", version: 1, review_accepted: false,
    question: "Copied resume details.", evidence: {}, evidence_references: [], context_annotations: [],
  });
  await resuming;
  assert.equal(app.state.consultation.id, "copied-session-789");
  assert.equal(app.elements.consultationResumeID.value, "copied-session-789");
});

test("resume arguments from a different project never issue a request", async () => {
  const app = createApp();
  app.state.projectKey = "a".repeat(64);
  app.elements.consultationResumeID.value = JSON.stringify({
    project_key: "b".repeat(64), session_id: "other-project-session",
  });
  await app.resumeConsultationByID();
  assert.equal(app.requests.length, 0);
  assert.equal(app.state.consultation, null);
  assert.match(app.elements.consultationStatus.textContent, /Select that project first/);
});

test("a late consultation resume cannot overwrite a different project", async () => {
  const app = createApp();
  app.state.projectKey = "a".repeat(64);
  app.elements.consultationResumeID.value = "saved-session-123";
  app.elements.consultationReview.hidden = true;

  const resuming = app.resumeConsultationByID();
  const request = app.requests.at(-1);
  app.state.projectKey = "b".repeat(64);
  request.resolve({
    consultation_id: "saved-session-123", version: 1, review_accepted: false,
    question: "Old project", evidence: {}, evidence_references: [],
  });
  await resuming;

  assert.equal(app.state.consultation, null);
  assert.equal(app.state.evidencePacketPreview, null);
  assert.equal(app.elements.consultationReview.hidden, true);
});

test("changing the consultation ID invalidates an older pending resume", async () => {
  const app = createApp();
  app.state.projectKey = "a".repeat(64);
  app.elements.consultationResumeID.value = "session-a";
  app.elements.consultationReview.hidden = true;

  const resuming = app.resumeConsultationByID();
  const request = app.requests.at(-1);
  app.elements.consultationResumeID.value = "session-b";
  app.elements.consultationResumeID.listeners.input();
  request.resolve({
    consultation_id: "session-a", version: 1, review_accepted: false,
    question: "Stale session", evidence: {}, evidence_references: [],
  });
  await resuming;

  assert.equal(app.state.consultation, null);
  assert.equal(app.state.evidencePacketPreview, null);
  assert.equal(app.elements.consultationReview.hidden, true);
  assert.equal(app.elements.consultationResume.disabled, false);
});

test("graph continuation merges distinct rows, deduplicates overlaps, and keeps cursor provenance", () => {
  const app = createApp();
  const first = {
    entities: [{ id: "endpoint-a" }, { id: "observation-a" }],
    assertions: [{ id: "assertion-a" }],
    capture_provenance: [{ graph_observation_id: "observation-a", request_id: 10 }],
    limitations: ["first page"],
    next_cursor: "cursor-2", truncated: true, neighborhood_candidate_limit_reached: true,
    result_id: "result-1",
  };
  const second = {
    entities: [{ id: "observation-a" }, { id: "parameter-a" }],
    assertions: [{ id: "assertion-a" }, { id: "assertion-b" }],
    capture_provenance: [{ graph_observation_id: "observation-a", request_id: 10 }],
    limitations: ["second page"],
    next_cursor: "cursor-3", truncated: true, neighborhood_candidate_limit_reached: true,
    result_id: "result-2",
  };

  const merged = app.mergeGraphPages(first, second);

  assert.deepEqual(snapshot(merged.entities.map(row => row.id)), ["endpoint-a", "observation-a", "parameter-a"]);
  assert.deepEqual(snapshot(merged.assertions.map(row => row.id)), ["assertion-a", "assertion-b"]);
  assert.equal(merged.capture_provenance.length, 1);
  assert.deepEqual(snapshot(merged.limitations), ["first page", "second page"]);
  assert.equal(merged.next_cursor, "cursor-3");
  assert.equal(merged.client_merged_pages, 2);
  assert.deepEqual(snapshot(merged.result_page_ids), ["result-1", "result-2"]);

  const last = app.mergeGraphPages(merged, {
    entities: [{ id: "response-a" }], assertions: [{ id: "assertion-c" }],
    next_cursor: undefined, truncated: false, neighborhood_candidate_limit_reached: false,
    result_id: "result-3",
  });
  assert.equal("next_cursor" in last, false);
  assert.equal(last.truncated, false);
  assert.equal(last.neighborhood_candidate_limit_reached, false);
  assert.deepEqual(snapshot(last.result_page_ids), ["result-1", "result-2", "result-3"]);
});

test("expanded field-value seed IDs are retained across direct graph continuation", async () => {
  const app = createApp();
  const observationA = `observation-${"a".repeat(64)}`;
  const occurrenceA = `field_occurrence-${"b".repeat(64)}`;
  const valueMatch = `value_reference-${"c".repeat(64)}`;
  const occurrenceB = `field_occurrence-${"d".repeat(64)}`;
  const observationB = `observation-${"e".repeat(64)}`;
  const expandedSeeds = [observationA, occurrenceA, valueMatch, occurrenceB, observationB];
  const cursor = "synthetic-field-value-cursor";
  app.state.projectKey = "f".repeat(64);

  const firstPage = app.loadGraph([observationA], { view: "direct" });
  assert.deepEqual(app.requests[0].url.searchParams.getAll("seed"), [observationA]);
  app.requests[0].resolve({
    entities: [], assertions: [], seed_ids: expandedSeeds,
    next_cursor: cursor, truncated: true, incomplete: true, result_id: "field-value-page-1",
  });
  await firstPage;
  assert.deepEqual(snapshot(app.state.graphSeeds), expandedSeeds);

  const secondPage = app.loadGraph(app.state.graphSeeds, {
    view: "direct", cursor: app.state.graph.next_cursor, appendPage: true,
  });
  assert.deepEqual(app.requests[1].url.searchParams.getAll("seed"), expandedSeeds);
  assert.equal(app.requests[1].url.searchParams.get("cursor"), cursor);
  app.requests[1].resolve({
    entities: [], assertions: [], seed_ids: expandedSeeds,
    truncated: false, incomplete: false, result_id: "field-value-page-2",
  });
  await secondPage;
  assert.deepEqual(snapshot(app.state.graphSeeds), expandedSeeds);
});

test("field-value traversal states are visible and described as correlation only", async () => {
  const app = createApp();
  app.state.projectKey = "project-a";
  const cases = [
    { state: "expanded", traversal: { state: "expanded", truncated: false, path_node_count: 3 }, text: /Value correlation expanded · 3 path nodes/ },
    { state: "bounded", traversal: { state: "bounded", truncated: true, path_node_count: 4 }, text: /Value correlation bounded · 4 path nodes · more may exist/ },
    { state: "bounded", traversal: { state: "expanded", truncated: true, path_node_count: 2 }, text: /Value correlation bounded · 2 path nodes · more may exist/ },
    { state: "index_unavailable", traversal: { state: "index_unavailable", truncated: false, path_node_count: 0 }, text: /Value index unavailable · direct graph only/ },
    { state: "no_matches", traversal: { state: "no_matches", truncated: false, path_node_count: 0 }, text: /No matching same-project value correlations/ },
    { state: "not_applicable", traversal: { state: "not_applicable", truncated: false, path_node_count: 0 }, text: /Field-value traversal not applicable/ },
  ];

  for (const [index, item] of cases.entries()) {
    const loading = app.loadGraph([`observation-${index}`], { view: "direct" });
    app.requests.at(-1).resolve({
      entities: [], assertions: [], seed_ids: [`observation-${index}`],
      field_value_traversal: item.traversal,
    });
    await loading;
    assert.equal(app.elements.fieldValueStatus.dataset.state, item.state);
    assert.match(app.elements.fieldValueStatus.textContent, item.text);
    assert.match(app.elements.fieldValueStatus.title, /correlation only; it does not establish causality/);
  }
});

test("field-value traversal status survives continuation and clears on project switch", async () => {
  const app = createApp();
  app.state.projectKey = "project-a";
  const seed = `observation-${"a".repeat(64)}`;
  const firstPage = app.loadGraph([seed], { view: "direct" });
  app.requests.at(-1).resolve({
    entities: [], assertions: [], seed_ids: [seed], next_cursor: "field-cursor",
    truncated: true, incomplete: true,
    field_value_traversal: { state: "expanded", truncated: false, path_node_count: 3 },
  });
  await firstPage;
  const expectedStatus = "Value correlation expanded · 3 path nodes";
  assert.equal(app.elements.fieldValueStatus.textContent, expectedStatus);

  const nextPage = app.loadGraph(app.state.graphSeeds, {
    view: "direct", cursor: app.state.graph.next_cursor, appendPage: true,
  });
  app.requests.at(-1).resolve({
    entities: [], assertions: [], seed_ids: [seed], truncated: false, incomplete: false,
    field_value_traversal: { state: "not_applicable", truncated: false, path_node_count: 0 },
  });
  await nextPage;
  assert.equal(app.elements.fieldValueStatus.textContent, expectedStatus);
  assert.equal(app.state.graph.field_value_traversal.state, "expanded");

  const lateGraph = app.loadGraph([seed], { view: "direct" });
  const lateGraphRequest = app.requests.at(-1);
  const switching = app.selectProject("B");
  assert.equal(app.state.graph, null);
  assert.equal(app.elements.fieldValueStatus.hidden, true);
  lateGraphRequest.resolve({
    entities: [], assertions: [], seed_ids: [seed],
    field_value_traversal: { state: "bounded", truncated: true, path_node_count: 4 },
  });
  await lateGraph;
  const surfaceRequest = app.requests.findLast(request => request.url.pathname === "/api/surface");
  surfaceRequest.resolve({ operations: [], total_operations: 0, coverage_state: "partial", coverage_gaps: [] });
  await switching;
  assert.equal(app.elements.fieldValueStatus.hidden, true);
  assert.equal(app.elements.fieldValueStatus.textContent, "");
});

test("native candidate continuation displays the cumulative server graph page", () => {
  const app = createApp();
  const next = { entities: [{ id: "endpoint-next" }], continuation_scope: "rendered_and_native_candidate_branches" };
  const merged = app.mergeGraphPages({ entities: [{ id: "endpoint-old" }] }, next);
  assert.deepEqual(snapshot(merged.entities), [{ id: "endpoint-next" }]);
});

test("graph operation title prefers the project alias while preserving the original route data", () => {
  const app = createApp();
  const operation = { id: "operation-abc", kind: "operation", data: { method: "POST", route_family: "/api/{value}" } };
  assert.equal(app.displayTitle(operation), "POST /api/{parameter}");
  app.state.operationAliases.set(operation.id, { alias: "Update account profile", version: 1 });
  assert.equal(app.displayTitle(operation), "Update account profile");
  assert.equal(operation.data.route_family, "/api/{value}");
});

test("operation alias save is scoped to the selected project and refreshes the display label", async () => {
  const app = createApp();
  app.state.projectKey = "a".repeat(64);
  const operation = { id: `operation-${"b".repeat(64)}`, kind: "operation", data: { method: "GET", route_family: "/api/profile" } };
  const input = { value: "Account profile", focus() {} };
  const button = { disabled: false };
  const saving = app.saveOperationAlias(operation, input, button);
  const request = app.requests.at(-1);
  assert.equal(request.method, "POST");
  assert.equal(request.url.pathname, "/api/consultation/operation-alias");
  const body = JSON.parse(request.body);
  assert.equal(body.project_key, app.state.projectKey);
  assert.equal(body.operation_id, operation.id);
  assert.equal(body.expected_version, 0);
  assert.equal(body.alias, "Account profile");
  request.resolve({ project_key: body.project_key, operation_id: body.operation_id, version: 1, alias: body.alias, origin: "analyst" });
  await saving;
  assert.equal(app.state.operationAliases.get(operation.id).alias, "Account profile");
  assert.equal(app.displayTitle(operation), "Account profile");
  assert.equal(button.disabled, false);
});

test("consultation labels expose the exact evidence items they annotate without showing opaque IDs", () => {
  const app = createApp();
  app.renderConsultationContext({
    evidence_references: [
      { evidence_reference_id: "opaque-a", evidence_index: 0, side: "request", request_id: "239", revision_id: "rev-a", span_start: 4, span_end: 20 },
      { evidence_reference_id: "opaque-b", evidence_index: 1, side: "response", request_id: "240", revision_id: "rev-b", span_start: 8, span_end: 22 },
    ],
    evidence: { evidence: [
      { request_id: "239", revision_id: "rev-a", side: "request", span_start: 4, span_end: 20, available: true, excerpt: "GET /profile?id=[REDACTED]" },
      { request_id: "240", revision_id: "rev-b", side: "response", span_start: 8, span_end: 22, available: true, excerpt: "{\"id\":\"[REDACTED]\"}" },
    ] },
    context_annotations: [{
      kind: "actor", label: "actor-a", evidence_reference_ids: ["opaque-b", "opaque-a", "opaque-a", "opaque-missing"],
    }],
  }, false);

  const item = app.elements.consultationContextList.children[0];
  const textFrom = value => [value.textContent, ...(value.children || []).map(textFrom)].join(" ");
  const text = textFrom(item);
  assert.match(text, /actor: actor-a/);
  assert.match(text, /Evidence 1 · request · Caido 239/);
  assert.match(text, /Evidence 2 · response · Caido 240/);
  assert.match(text, /bytes \[4, 20\)/);
  assert.match(text, /GET \/profile\?id=\[REDACTED\]/);
  assert.match(text, /bytes \[8, 22\)/);
  assert.match(text, /\{"id":"\[REDACTED\]"\}/);
  assert.match(text, /1 unresolved/);
  assert.match(text, /1 evidence reference could not be resolved/);
  assert.doesNotMatch(text, /opaque-[ab]/);
});

test("local evidence viewer pins the exact historical revision and keeps HTTP bytes out of normal graph navigation", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const revisionId = "b".repeat(64);
  app.state.projectKey = projectKey;
  const trigger = { isConnected: true, focused: false, focus() { this.focused = true; } };
  const pending = app.openEvidence({
    request_id: 239, revision_id: revisionId,
    record_source: "encrypted_local_capture", raw_stored_locally: true,
  }, trigger);
  assert.equal(app.requests.length, 1);
  const first = app.requests[0];
  assert.equal(first.url.pathname, "/api/evidence");
  assert.equal(first.url.searchParams.get("project_key"), projectKey);
  assert.equal(first.url.searchParams.get("request_id"), "239");
  assert.equal(first.url.searchParams.get("revision_id"), revisionId);
  assert.equal(first.url.searchParams.get("side"), "request");
  assert.equal(first.url.searchParams.get("offset"), "0");
  assert.equal(first.url.searchParams.get("max_bytes"), String(1 << 20));
  first.resolve({
    project_key: projectKey, request_id: "239", revision_id: revisionId, side: "request", offset: 0,
    total_size: 15, returned_size: 15, next_offset: null, data: "GET /safe HTTP/1.1", encoding: "utf-8",
    wire_fidelity: "source_bytes", parse_state: "complete", request_truncated: false,
  });
  await pending;
  assert.equal(app.elements.evidenceDialog.open, true);
  assert.equal(app.elements.evidenceContent.textContent, "GET /safe HTTP/1.1");
  assert.match(app.elements.evidenceMeta.textContent, new RegExp(revisionId));
  assert.equal(app.state.evidence.revisionId, revisionId);

  app.selectEvidenceSide("response");
  assert.equal(app.requests.length, 2);
  assert.equal(app.requests[1].url.searchParams.get("revision_id"), revisionId);
  assert.equal(app.requests[1].url.searchParams.get("side"), "response");
  app.requests[1].resolve({
    project_key: projectKey, request_id: "239", revision_id: revisionId, side: "response", offset: 0,
    total_size: 0, returned_size: 0, next_offset: null, data: "", encoding: "utf-8",
    wire_fidelity: "source_bytes", parse_state: "complete", response_truncated: false,
  });
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(app.elements.evidenceContent.textContent, "");
  app.clearProjectView();
  assert.equal(app.state.evidence, null);
  assert.equal(app.elements.evidenceContent.textContent, "");
  assert.equal(trigger.focused, true, "closing the evidence dialog should restore focus to its opener");
});

test("local evidence viewer rejects response provenance and range mismatches", async () => {
  const projectKey = "a".repeat(64);
  const revisionId = "b".repeat(64);
  const valid = {
    project_key: projectKey, request_id: "239", revision_id: revisionId, side: "request", offset: 0,
    total_size: 15, returned_size: 15, next_offset: null, data: "GET /wrong HTTP/1.1", encoding: "utf-8",
  };
  const mismatches = [
    ["project", { project_key: "c".repeat(64) }],
    ["request", { request_id: "240" }],
    ["revision", { revision_id: "d".repeat(64) }],
    ["side", { side: "response" }],
    ["offset", { offset: 1 }],
    ["range continuation", { total_size: 30, next_offset: null }],
  ];

  for (const [name, mismatch] of mismatches) {
    const app = createApp();
    app.state.projectKey = projectKey;
    const pending = app.openEvidence({
      request_id: "239", revision_id: revisionId,
      record_source: "encrypted_local_capture", raw_stored_locally: true,
    });
    app.requests[0].resolve({ ...valid, ...mismatch });
    await pending;
    assert.equal(app.elements.evidenceContent.textContent, "", `${name} mismatch must not render bytes`);
    assert.match(app.elements.evidenceFoot.textContent, /provenance or range/i, `${name} mismatch should be explained`);
    assert.equal(app.state.evidence.revisionId, revisionId, `${name} mismatch must not replace the pinned revision`);
  }
});

test("local evidence viewer refuses non-local and live-only records without requesting bytes", async () => {
  const app = createApp();
  app.state.projectKey = "a".repeat(64);
  await app.openEvidence({ request_id: 239, record_source: "caido_history", raw_stored_locally: false });
  assert.equal(app.requests.length, 0);
  assert.equal(app.elements.evidenceDialog.open, false);

  app.openEvidence({
    request_id: 239, record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  app.openEvidence({
    request_id: 239, revision_id: "not-a-revision", record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  assert.equal(app.requests.length, 0, "unversioned records cannot fall back to the mutable current revision");
});

test("graph inspector shows analyst context only for an accepted exact evidence revision", () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const revisionId = "b".repeat(64);
  const observation = { id: `observation-${"f".repeat(64)}`, kind: "observation", data: { method: "GET", path: "/items" } };
  app.state.projectKey = projectKey;
  app.state.graph = {
    entities: [observation], assertions: [],
    capture_provenance: [{
      graph_observation_id: observation.id, request_id: "239", revision_id: revisionId,
    }],
  };
  app.state.consultation = {
    id: "consultation-1", projectKey, version: 4, accepted: true, reviewLoaded: true,
    evidenceReferences: [{
      evidence_reference_id: "evidence-ref-1", evidence_index: 0, side: "request",
      request_id: "239", revision_id: revisionId,
    }],
    contextAnnotations: [{
      context_annotation_id: "annotation-1", session_version: 2, kind: "actor", label: "actor-a",
      evidence_reference_ids: ["evidence-ref-1"], origin: "analyst", evidence_state: "analyst_provided",
      excerpt: "PRIVATE_RAW_TEXT_MUST_NOT_RENDER",
    }],
  };

  app.renderInspector(observation);
  const allChildren = root => [root, ...(root.children || []).flatMap(child => allChildren(child))];
  const rows = allChildren(app.elements.inspector).filter(item => item.className?.split(/\s+/).includes("context-annotation-item"));
  assert.equal(rows.length, 1);
  const rowText = rows.flatMap(row => row.children || []).map(child => child.textContent || "").join(" ");
  assert.match(rowText, /actor: actor-a · analyst-provided/);
  assert.match(rowText, /consultation-1/);
  assert.match(rowText, /evidence-ref-1/);
  assert.match(rowText, /request · Caido request 239/);
  assert.match(rowText, new RegExp(revisionId));
  assert.doesNotMatch(rowText, /PRIVATE_RAW_TEXT_MUST_NOT_RENDER/);

  app.state.consultation.accepted = false;
  app.renderInspector(observation);
  assert.equal(allChildren(app.elements.inspector).filter(item => item.className?.split(/\s+/).includes("context-annotation-item")).length, 0);
});

test("observation inspector shows request order only for a source-backed exact precedes assertion", () => {
  const app = createApp();
  const prior = { id: "observation-prior", kind: "observation", data: { observed_at: "2026-10-24T10:00:00Z" } };
  const selected = { id: "observation-selected", kind: "observation", data: { observed_at: "2026-10-24T10:01:00Z" } };
  const next = { id: "observation-next", kind: "observation" };
  const assertionID = `assertion-${"a".repeat(64)}`;
  const sha256 = "b".repeat(64);
  const assertion = {
    id: assertionID, subject: prior.id, predicate: "precedes", object: selected.id,
    origin_kind: "operator_asserted", evidence_state: "observed",
    source_refs: [{ path: "/tmp/synthetic-source.jsonl", sha256, record_locator: "line:17" }],
  };
  const allChildren = root => [root, ...(root.children || []).flatMap(child => allChildren(child))];
  const inspectorText = () => allChildren(app.elements.inspector).map(item => item.textContent || "").join(" ");

  app.state.graph = { entities: [prior, selected, next], assertions: [assertion] };
  app.renderInspector(selected);
  const incoming = inspectorText();
  assert.match(incoming, /Request ordering/);
  assert.match(incoming, /observation-prior → observation-selected/);
  assert.match(incoming, /Basis · precedes · operator_asserted \/ observed/);
  assert.match(incoming, new RegExp(assertionID));
  assert.match(incoming, new RegExp(`SHA-256 ${sha256}`));
  assert.match(incoming, /record locator line:17/);
  assert.match(incoming, /Earlier request in recorded order · observation-prior/);
  assert.match(incoming, /timestamps and graph proximity do not prove causality/i);
  assert.doesNotMatch(incoming, /synthetic-source\.jsonl/);

  app.state.graph = { entities: [selected, next], assertions: [assertion] };
  app.renderInspector(selected);
  const incomplete = inspectorText();
  assert.match(incomplete, /Initiating request unknown in this graph slice/);
  assert.doesNotMatch(incomplete, /observation-prior → observation-selected/);

  app.state.graph = { entities: [prior, selected, next], assertions: [{ ...assertion, source_refs: [] }] };
  app.renderInspector(selected);
  assert.match(inspectorText(), /Initiating request unknown in this graph slice/);

  app.state.graph = { entities: [prior, selected, next], assertions: [] };
  app.renderInspector(selected);
  const noEdge = inspectorText();
  assert.match(noEdge, /Initiating request unknown in this graph slice/);
  assert.match(noEdge, /timestamps and graph proximity do not prove causality/i);

  app.state.graph = { entities: [selected, next], assertions: [{
    ...assertion, id: `assertion-${"c".repeat(64)}`, subject: selected.id, object: next.id,
  }] };
  app.renderInspector(selected);
  const outgoing = inspectorText();
  assert.match(outgoing, /observation-selected → observation-next/);
  assert.match(outgoing, /Initiating request unknown in this graph slice/);
});

test("capture comparison pins exact revisions, reports fidelity and truncation, and clears baseline on project switch", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const baselineRevision = "b".repeat(64);
  const selectedRevision = "c".repeat(64);
  app.state.projectKey = projectKey;
  const observation = { id: `observation-${"f".repeat(64)}`, kind: "observation", data: { method: "GET", path: "/items" } };
  app.state.selectedNode = observation;
  app.state.graph = {
    entities: [observation], assertions: [],
    capture_provenance: [
      { graph_observation_id: observation.id, request_id: "239", revision_id: baselineRevision, record_source: "encrypted_local_capture", raw_stored_locally: true, request_size: 28 },
      { graph_observation_id: observation.id, request_id: "240", revision_id: selectedRevision, record_source: "encrypted_local_capture", raw_stored_locally: true, request_size: 28 },
    ],
  };
  app.renderInspector(observation);
  const allChildren = root => [root, ...(root.children || []).flatMap(child => allChildren(child))];
  const baselineButton = allChildren(app.elements.inspector).find(item => item.className === "detail-action capture-baseline" && item.textContent === "Set as baseline");
  assert.ok(baselineButton, "the graph inspector should offer a baseline action");
  baselineButton.listeners.click();
  assert.deepEqual(snapshot(app.state.comparisonBaseline), {
    projectKey, requestId: "239", revisionId: baselineRevision, graphObservationID: observation.id,
  });
  const renderedButtons = allChildren(app.elements.inspector);
  const baselineCompare = renderedButtons.find(item => item.className === "detail-action capture-compare" && item.disabled);
  const selectedCompare = renderedButtons.find(item => item.className === "detail-action capture-compare" && !item.disabled);
  assert.ok(baselineCompare, "the baseline row cannot be compared with itself");
  assert.ok(selectedCompare, "another exact revision becomes available for comparison");

  const pending = app.openComparison({
    request_id: "240", revision_id: selectedRevision,
    record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  assert.equal(app.requests.length, 1);
  assert.equal(app.requests[0].url.pathname, "/api/compare");
  assert.equal(app.requests[0].url.searchParams.get("project_key"), projectKey);
  assert.equal(app.requests[0].url.searchParams.get("left_request_id"), "239");
  assert.equal(app.requests[0].url.searchParams.get("left_revision_id"), baselineRevision);
  assert.equal(app.requests[0].url.searchParams.get("right_request_id"), "240");
  assert.equal(app.requests[0].url.searchParams.get("right_revision_id"), selectedRevision);
  app.requests[0].resolve({
    request: {
      available: true, equal: false, compared_bytes: 28, first_difference_offset: 14,
      left_excerpt: '"GET /item?id=1"', right_excerpt: '"GET /item?id=2"',
      left: { request_id: "239", revision_id: baselineRevision, size: 28, sha256: "d".repeat(64), parse_state: "incomplete", wire_fidelity: "source_bytes", truncated: true },
      right: { request_id: "240", revision_id: selectedRevision, size: 28, sha256: "e".repeat(64), parse_state: "complete", wire_fidelity: "reconstructed_text", truncated: false },
    },
    response: { available: false, reason: "Exact local bytes are unavailable for one or both selected revisions." },
    coverage_note: "Stored bytes only; truncation and source fidelity are reported per message.",
  });
  await pending;
  assert.equal(app.elements.evidenceTitle.textContent, "Capture comparison");
  assert.match(app.elements.evidenceCompareContent.textContent, /first byte difference at offset 14/);
  assert.match(app.elements.evidenceCompareContent.textContent, /wire_fidelity|source_bytes/);
  assert.match(app.elements.evidenceCompareContent.textContent, /truncated yes/);
  assert.match(app.elements.evidenceCompareContent.textContent, /truncated no/);
  assert.match(app.elements.evidenceCompareContent.textContent, /RESPONSE[\s\S]*Not compared/);
  assert.equal(app.elements.evidenceFoot.textContent, "Stored bytes only; truncation and source fidelity are reported per message.");

  const switchProject = app.selectProject("B");
  assert.equal(app.state.comparisonBaseline, null, "a project switch clears the pinned baseline");
  app.requests[1].resolve(page("b.test"));
  await switchProject;
});

test("capture comparison shows accepted context per exact side without changing the compare request", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const leftRevision = "b".repeat(64);
  const rightRevision = "c".repeat(64);
  const mismatchedRevision = "d".repeat(64);
  app.state.projectKey = projectKey;
  app.state.comparisonBaseline = {
    projectKey, requestId: "239", revisionId: leftRevision, graphObservationID: "observation-left",
  };
  app.state.graph = { capture_provenance: [
    { graph_observation_id: "observation-left", request_id: "239", revision_id: leftRevision },
    { graph_observation_id: "observation-right", request_id: "240", revision_id: rightRevision },
  ] };
  app.state.consultation = {
    id: "consultation-compare", projectKey, version: 6, accepted: true, reviewLoaded: true,
    evidenceReferences: [
      { evidence_reference_id: "ref-left", evidence_index: 0, side: "request", request_id: "239", revision_id: leftRevision },
      { evidence_reference_id: "ref-right", evidence_index: 1, side: "response", request_id: "240", revision_id: rightRevision },
      { evidence_reference_id: "ref-mismatch", evidence_index: 2, side: "request", request_id: "240", revision_id: mismatchedRevision },
    ],
    contextAnnotations: [
      { context_annotation_id: "annotation-left", session_version: 4, kind: "actor", label: "actor-a", evidence_reference_ids: ["ref-left"], origin: "analyst", evidence_state: "analyst_provided" },
      { context_annotation_id: "annotation-right", session_version: 5, kind: "flow", label: "flow-b", evidence_reference_ids: ["ref-right"], origin: "analyst", evidence_state: "analyst_provided", excerpt: "PRIVATE_CONTEXT_EXCERPT" },
      { context_annotation_id: "annotation-mismatch", session_version: 5, kind: "actor", label: "wrong-revision-label", evidence_reference_ids: ["ref-mismatch"], origin: "analyst", evidence_state: "analyst_provided" },
    ],
  };

  const pending = app.openComparison({
    request_id: "240", revision_id: rightRevision, graph_observation_id: "observation-right",
    record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  const compareRequest = app.requests[0];
  assert.deepEqual([...compareRequest.url.searchParams.keys()].sort(), [
    "left_request_id", "left_revision_id", "project_key", "right_request_id", "right_revision_id",
  ]);
  compareRequest.resolve({
    request: {
      available: true, equal: false, compared_bytes: 12, first_difference_offset: 7,
      left_excerpt: "baseline excerpt", right_excerpt: "selected excerpt",
      left: { request_id: "239", revision_id: leftRevision, sha256: "e".repeat(64), size: 12 },
      right: { request_id: "240", revision_id: rightRevision, sha256: "f".repeat(64), size: 12 },
    },
    response: { available: false, reason: "Exact response bytes are unavailable." },
    coverage_note: "Stored bytes only.",
  });
  await pending;

  const shown = app.elements.evidenceCompareContent.textContent;
  assert.match(shown, /BASELINE \(A\) ANALYST CONTEXT/);
  assert.match(shown, /actor: actor-a · analyst-provided/);
  assert.match(shown, /accepted version 6 · annotation version 4/);
  assert.match(shown, /ref-left/);
  assert.match(shown, /SELECTED \(B\) ANALYST CONTEXT/);
  assert.match(shown, /flow: flow-b · analyst-provided/);
  assert.match(shown, /accepted version 6 · annotation version 5/);
  assert.match(shown, /ref-right/);
  assert.doesNotMatch(shown, /wrong-revision-label|PRIVATE_CONTEXT_EXCERPT/);

  app.state.consultation.evidenceReferences[1].revision_id = mismatchedRevision;
  const mismatch = app.formatCaptureComparison(app.state.comparison.result, app.state.comparison);
  assert.match(mismatch, /actor: actor-a · analyst-provided/);
  assert.doesNotMatch(mismatch, /flow: flow-b · analyst-provided|ref-right/);

  app.state.consultation.accepted = false;
  const unaccepted = app.formatCaptureComparison(app.state.comparison.result, app.state.comparison);
  assert.doesNotMatch(unaccepted, /ANALYST CONTEXT|actor-a|flow-b/);
});

test("a graph comparison can be attached to a resumed accepted consultation only for its exact evidence revisions", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const baselineRevision = "b".repeat(64);
  const selectedRevision = "c".repeat(64);
  app.state.projectKey = projectKey;
  app.state.comparisonBaseline = { projectKey, requestId: "239", revisionId: baselineRevision };
  app.elements.consultationResumeID.value = "session-accepted";

  const comparison = app.openComparison({
    request_id: "240", revision_id: selectedRevision,
    record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  app.requests[0].resolve({
    request: {
      available: true, equal: false, compared_bytes: 28, first_difference_offset: 14,
      left_excerpt: '"GET /item?id=1"', right_excerpt: '"GET /item?id=2"',
      left: { request_id: "239", revision_id: baselineRevision, size: 28, sha256: "d".repeat(64) },
      right: { request_id: "240", revision_id: selectedRevision, size: 28, sha256: "e".repeat(64) },
    },
    response: { available: false, reason: "Exact response bytes are unavailable." },
    coverage_note: "Stored bytes only.",
  });
  await comparison;
  assert.equal(app.elements.comparisonAttach.hidden, false);
  assert.equal(app.elements.comparisonAttachButton.disabled, true);
  assert.match(app.elements.comparisonAttachStatus.textContent, /Resume an accepted consultation/);

  const resume = app.resumeConsultationByID();
  assert.equal(app.requests[1].url.pathname, "/api/consultation/review");
  app.requests[1].resolve({
    consultation_id: "session-accepted", version: 3, review_accepted: true,
    question: "Compare the selected synthetic captures.", evidence: [], context_annotations: [], comparisons: [],
    evidence_references: [
      { evidence_reference_id: "ref-left", request_id: "239", revision_id: baselineRevision, source_integrity_status: "verified", sha256: "d".repeat(64) },
      { evidence_reference_id: "ref-right", request_id: "240", revision_id: selectedRevision, source_integrity_status: "verified", sha256: "e".repeat(64) },
    ],
  });
  await resume;
  assert.equal(app.elements.comparisonAttachButton.disabled, false);
  const attach = app.attachComparisonToConsultation();
  assert.equal(app.requests[2].method, "POST");
  assert.equal(app.requests[2].url.pathname, "/api/consultation/compare");
  const request = JSON.parse(app.requests[2].body);
  assert.equal(request.project_key, projectKey);
  assert.equal(request.session_id, "session-accepted");
  assert.equal(request.expected_version, 3);
  assert.equal(request.left_request_id, "239");
  assert.equal(request.left_revision_id, baselineRevision);
  assert.equal(request.right_request_id, "240");
  assert.equal(request.right_revision_id, selectedRevision);
  assert.ok(request.idempotency_key);

  app.requests[2].resolve({ comparison_id: "comparison-saved", session_version: 4 });
  await attach;
  assert.equal(app.state.consultation.version, 4);
  assert.equal(app.elements.comparisonAttachButton.disabled, true);
  assert.match(app.elements.comparisonAttachStatus.textContent, /comparison-saved/);
  assert.equal(app.elements.comparisonAttachStatus.textContent.includes("GET /item?id=1"), false);
  assert.equal(app.elements.consultationReview.hidden, false);
});

test("candidate workflow validates selected session citations before saving an explicitly unconfirmed candidate", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const verifiedRef = "b".repeat(43);
  const unverifiedRef = "c".repeat(43);
  const comparisonID = "d".repeat(43);
  app.state.projectKey = projectKey;
  app.elements.consultationResumeID.value = "candidate-session";

  const resume = app.resumeConsultationByID();
  app.requests[0].resolve({
    consultation_id: "candidate-session", version: 2, review_accepted: false,
    question: "Compare synthetic behavior.", evidence: [], context_annotations: [], proposals: [], validation_receipts: [], candidates: [],
    evidence_references: [
      { evidence_reference_id: verifiedRef, evidence_index: 0, request_id: "239", revision_id: "e".repeat(64), side: "request", source_integrity_status: "verified", sha256: "f".repeat(64) },
      { evidence_reference_id: unverifiedRef, evidence_index: 1, request_id: "240", revision_id: "1".repeat(64), side: "request", source_integrity_status: "unavailable", sha256: "" },
    ],
    comparisons: [{ comparison_id: comparisonID, result: { left_request_id: "239", right_request_id: "240" } }],
  });
  await resume;
  assert.equal(app.elements.candidateEvidence.children.length, 1, "unverified references are not offered as citations");
  assert.equal(app.elements.candidateSave.disabled, true, "unaccepted sessions cannot create candidates");
  await app.saveConsultationCandidate();
  assert.equal(app.requests.length, 1, "the UI does not send a proposal before acceptance");

  const accept = app.acceptConsultationReview();
  assert.equal(app.requests[1].url.pathname, "/api/consultation/accept-review");
  app.requests[1].resolve({ version: 3, review_accepted: true });
  await accept;
  app.elements.candidateTitle.value = "Possible account response difference";
  app.elements.candidateSummary.value = "The two accepted captures show different response behavior.";
  app.elements.candidateRationale.value = "The comparison is repeatable in this synthetic fixture.";
  app.elements.candidateImpact.value = "Potentially exposes account-specific information.";
  app.elements.candidateEvidence.children[0].children[0].checked = true;
  app.elements.candidateComparisons.children[0].children[0].checked = true;
  app.updateCandidateControl();
  assert.equal(app.elements.candidateSave.disabled, false);

  const save = app.saveConsultationCandidate();
  assert.equal(app.requests[2].method, "POST");
  assert.equal(app.requests[2].url.pathname, "/api/consultation/proposal");
  const proposalRequest = JSON.parse(app.requests[2].body);
  assert.equal(proposalRequest.expected_version, 3);
  assert.deepEqual(proposalRequest.evidence_reference_ids, [verifiedRef]);
  assert.deepEqual(proposalRequest.comparison_ids, [comparisonID]);
  assert.equal(proposalRequest.proposal.title, "Possible account response difference");
  assert.ok(proposalRequest.idempotency_key);
  assert.equal(app.requests[2].body.includes("HTTP/1.1"), false, "raw HTTP excerpts are not copied into the proposal");

  app.requests[2].resolve({ proposal_id: "proposal-1", session_version: 4 });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(app.requests[3].url.pathname, "/api/consultation/validate");
  const validateRequest = JSON.parse(app.requests[3].body);
  assert.equal(validateRequest.expected_version, 4);
  assert.equal(validateRequest.proposal_id, "proposal-1");
  app.requests[3].resolve({ receipt_id: "receipt-1", session_version: 5, status: "references_valid" });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(app.requests[4].url.pathname, "/api/consultation/candidate");
  const candidateRequest = JSON.parse(app.requests[4].body);
  assert.equal(candidateRequest.expected_version, 5);
  assert.equal(candidateRequest.proposal_id, "proposal-1");
  assert.equal(candidateRequest.receipt_id, "receipt-1");
  assert.ok(candidateRequest.idempotency_key);
  app.requests[4].resolve({
    candidate_id: "candidate-1", version: 1, session_version: 6, state: "candidate",
    proposal_id: "proposal-1", receipt_id: "receipt-1",
    citations: [{ evidence_reference_id: verifiedRef }], comparison_ids: [comparisonID],
  });
  await save;
  assert.equal(app.state.consultation.version, 6);
  assert.match(app.elements.candidateStatus.textContent, /state candidate/);
  const listedCandidate = app.elements.candidateList.children[0].children.map(item => item.textContent).join(" ");
  assert.match(listedCandidate, /State: candidate/);
  assert.match(listedCandidate, new RegExp(verifiedRef));
  assert.match(listedCandidate, new RegExp(comparisonID));
  assert.doesNotMatch(listedCandidate, /verified|finding/i);
});

test("a candidate workflow conflict disables stale state and requires a fresh consultation resume", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const referenceID = "b".repeat(43);
  app.state.projectKey = projectKey;
  app.elements.consultationResumeID.value = "conflict-session";

  const resume = app.resumeConsultationByID();
  app.requests[0].resolve({
    consultation_id: "conflict-session", version: 7, review_accepted: true,
    question: "Check version fencing.", evidence: [], context_annotations: [], comparisons: [],
    evidence_references: [{ evidence_reference_id: referenceID, evidence_index: 0, side: "request", source_integrity_status: "verified", sha256: "c".repeat(64) }],
  });
  await resume;
  app.elements.candidateTitle.value = "Version conflict candidate";
  app.elements.candidateSummary.value = "Synthetic stale state.";
  app.elements.candidateRationale.value = "The server has a newer session version.";
  app.elements.candidateEvidence.children[0].children[0].checked = true;
  app.updateCandidateControl();

  const save = app.saveConsultationCandidate();
  assert.equal(app.requests[1].url.pathname, "/api/consultation/proposal");
  app.requests[1].fail(409, "consultation state changed");
  await save;
  assert.equal(app.state.consultation.reviewLoaded, false);
  assert.equal(app.elements.candidateSave.disabled, true);
  assert.match(app.elements.candidateStatus.textContent, /Resume this consultation/);
  await app.saveConsultationCandidate();
  assert.equal(app.requests.length, 2, "stale session state cannot be submitted again");

  const refresh = app.resumeConsultationByID();
  assert.equal(app.requests[2].url.pathname, "/api/consultation/review");
  app.requests[2].resolve({
    consultation_id: "conflict-session", version: 8, review_accepted: true,
    question: "Check version fencing.", evidence: [], context_annotations: [], comparisons: [],
    evidence_references: [{ evidence_reference_id: referenceID, evidence_index: 0, side: "request", source_integrity_status: "verified", sha256: "c".repeat(64) }],
  });
  await refresh;
  assert.equal(app.state.consultation.version, 8);
  assert.equal(app.state.consultation.reviewLoaded, true);
});

test("resuming after an interrupted proposal validates the saved proposal instead of creating a duplicate", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const referenceID = "b".repeat(43);
  const comparisonID = "c".repeat(43);
  const proposalID = "d".repeat(43);
  app.state.projectKey = projectKey;
  app.elements.consultationResumeID.value = "interrupted-proposal-session";
  const resume = app.resumeConsultationByID();
  app.requests[0].resolve({
    consultation_id: "interrupted-proposal-session", version: 5, review_accepted: true,
    question: "Resume the proposal stage.", evidence: [], context_annotations: [], validation_receipts: [], candidates: [],
    evidence_references: [{ evidence_reference_id: referenceID, evidence_index: 0, side: "request", source_integrity_status: "verified", sha256: "e".repeat(64) }],
    comparisons: [{ comparison_id: comparisonID }],
    proposals: [{ proposal_id: proposalID, proposal_digest: "f".repeat(64), evidence_reference_ids: [referenceID], comparison_ids: [comparisonID] }],
  });
  await resume;
  assert.match(app.elements.candidateRecoveryList.children[0].children[1].textContent, /Validate proposal and save candidate/);

  const continuation = app.continueConsultationCandidateProposal(proposalID);
  assert.equal(app.requests[1].url.pathname, "/api/consultation/validate");
  const validateRequest = JSON.parse(app.requests[1].body);
  assert.equal(validateRequest.expected_version, 5);
  assert.equal(validateRequest.proposal_id, proposalID);
  app.requests[1].resolve({ receipt_id: "g".repeat(43), proposal_id: proposalID, proposal_digest: "f".repeat(64),
    evidence_reference_ids: [referenceID], comparison_ids: [comparisonID], status: "references_valid", session_version: 6 });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(app.requests[2].url.pathname, "/api/consultation/candidate");
  assert.equal(JSON.parse(app.requests[2].body).expected_version, 6);
  app.requests[2].resolve({ candidate_id: "h".repeat(43), version: 1, session_version: 7, state: "candidate",
    proposal_id: proposalID, receipt_id: "g".repeat(43), citations: [{ evidence_reference_id: referenceID }], comparison_ids: [comparisonID] });
  await continuation;
  assert.equal(app.requests.filter(request => request.url.pathname === "/api/consultation/proposal").length, 0);
  assert.equal(app.state.consultation.version, 7);
  assert.equal(app.state.consultation.candidates.length, 1);
});

test("resuming with a matching valid receipt saves its candidate without validating again", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const referenceID = "b".repeat(43);
  const proposalID = "c".repeat(43);
  const receiptID = "d".repeat(43);
  const digest = "e".repeat(64);
  app.state.projectKey = projectKey;
  app.elements.consultationResumeID.value = "interrupted-receipt-session";
  const resume = app.resumeConsultationByID();
  app.requests[0].resolve({
    consultation_id: "interrupted-receipt-session", version: 6, review_accepted: true,
    question: "Resume the candidate stage.", evidence: [], context_annotations: [], comparisons: [], candidates: [],
    evidence_references: [{ evidence_reference_id: referenceID, evidence_index: 0, side: "request", source_integrity_status: "verified", sha256: "f".repeat(64) }],
    proposals: [{ proposal_id: proposalID, proposal_digest: digest, evidence_reference_ids: [referenceID], comparison_ids: [] }],
    validation_receipts: [{ receipt_id: receiptID, proposal_id: proposalID, proposal_digest: digest,
      evidence_reference_ids: [referenceID], comparison_ids: [], status: "references_valid" }],
  });
  await resume;
  assert.match(app.elements.candidateRecoveryList.children[0].children[1].textContent, /Matching valid receipt/);

  const continuation = app.continueConsultationCandidateProposal(proposalID);
  assert.equal(app.requests[1].url.pathname, "/api/consultation/candidate");
  const candidateRequest = JSON.parse(app.requests[1].body);
  assert.equal(candidateRequest.expected_version, 6);
  assert.equal(candidateRequest.proposal_id, proposalID);
  assert.equal(candidateRequest.receipt_id, receiptID);
  app.requests[1].resolve({ candidate_id: "g".repeat(43), version: 1, session_version: 7, state: "candidate",
    proposal_id: proposalID, receipt_id: receiptID, citations: [{ evidence_reference_id: referenceID }], comparison_ids: [] });
  await continuation;
  assert.equal(app.requests.some(request => request.url.pathname === "/api/consultation/validate"), false);
  assert.equal(app.requests.some(request => request.url.pathname === "/api/consultation/proposal"), false);
  assert.equal(app.state.consultation.version, 7);
});

test("evidence packet preview accumulates exact spans across captures and resets on project switch", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const revisionId = "b".repeat(64);
  const secondRevisionId = "c".repeat(64);
  app.state.projectKey = projectKey;
  const open = app.openEvidence({
    request_id: "239", revision_id: revisionId,
    record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  app.requests[0].resolve({
    project_key: projectKey, request_id: "239", revision_id: revisionId, side: "request", offset: 0,
    total_size: 64, returned_size: 64, next_offset: null, data: "GET /items?password=secret HTTP/1.1", encoding: "utf-8",
    wire_fidelity: "source_bytes", parse_state: "complete", request_truncated: false,
  });
  await open;
  app.elements.evidencePacketQuestion.value = "Explain password=private-value in this request.";
  app.elements.evidencePacketQuestion.listeners.input();
  app.elements.evidencePacketOffset.value = "4";
  app.elements.evidencePacketOffset.listeners.input();
  app.elements.evidencePacketLength.value = "24";
  app.elements.evidencePacketLength.listeners.input();
  assert.equal(app.elements.evidencePacketAdd.disabled, false);
  assert.equal(app.addCurrentEvidenceSpan(), true);
  assert.equal(app.state.evidencePacketDraft.selections.length, 1);

  const loadResponse = app.selectEvidenceSide("response");
  assert.equal(app.requests.length, 2);
  app.requests[1].resolve({
    project_key: projectKey, request_id: "239", revision_id: revisionId, side: "response", offset: 0,
    total_size: 64, returned_size: 64, next_offset: null, data: "HTTP/1.1 200 OK\r\nContent-Type: application/json", encoding: "utf-8",
    wire_fidelity: "source_bytes", parse_state: "complete", response_truncated: false,
  });
  await loadResponse;
  app.elements.evidencePacketOffset.value = "0";
  app.elements.evidencePacketOffset.listeners.input();
  app.elements.evidencePacketLength.value = "20";
  app.elements.evidencePacketLength.listeners.input();
  assert.equal(app.addCurrentEvidenceSpan(), true);

  const openSecond = app.openEvidence({
    request_id: "240", revision_id: secondRevisionId,
    record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  app.requests[2].resolve({
    project_key: projectKey, request_id: "240", revision_id: secondRevisionId, side: "request", offset: 0,
    total_size: 64, returned_size: 64, next_offset: null, data: "GET /profile HTTP/1.1", encoding: "utf-8",
    wire_fidelity: "source_bytes", parse_state: "complete", request_truncated: false,
  });
  await openSecond;
  assert.equal(app.elements.evidencePacketQuestion.value, "Explain password=private-value in this request.");
  app.elements.evidencePacketOffset.value = "0";
  app.elements.evidencePacketOffset.listeners.input();
  app.elements.evidencePacketLength.value = "18";
  app.elements.evidencePacketLength.listeners.input();
  assert.equal(app.addCurrentEvidenceSpan(), true);
  assert.equal(app.state.evidencePacketDraft.selections.length, 3);
  assert.match(app.elements.evidencePacketSelectionCount.textContent, /3 selected spans/);
  app.elements.evidencePacketOffset.value = "20";
  app.elements.evidencePacketOffset.listeners.input();
  app.elements.evidencePacketLength.value = "12";
  app.elements.evidencePacketLength.listeners.input();
  assert.equal(app.addCurrentEvidenceSpan(), true);
  assert.equal(app.state.evidencePacketDraft.selections.length, 4);
  assert.equal(app.elements.evidencePacketAdd.disabled, true);

  const preview = app.previewEvidencePacket();
  assert.equal(app.requests.length, 4);
  const call = app.requests[3];
  assert.equal(call.url.pathname, "/api/evidence/packet-preview");
  assert.equal(call.method, "POST");
  assert.equal(call.headers["Content-Type"], "application/json");
  const body = JSON.parse(call.body);
  assert.equal(body.project_key, projectKey);
  assert.equal(body.question, "Explain password=private-value in this request.");
  assert.deepEqual(snapshot(body.selections), [{
    request_id: "239", revision_id: revisionId, side: "request", offset: 4, length: 24,
  }, {
    request_id: "239", revision_id: revisionId, side: "response", offset: 0, length: 20,
  }, {
    request_id: "240", revision_id: secondRevisionId, side: "request", offset: 0, length: 18,
  }, {
    request_id: "240", revision_id: secondRevisionId, side: "request", offset: 20, length: 12,
  }]);
  call.resolve({
    version: "1", policy_version: "bb-evidence-redaction-v1", preview_only: true, operator_review_needed: true,
    question: "Explain password=[REDACTED] in this request.",
    evidence: [
      { available: true, excerpt: "/items?password=[REDACTED]", redactions: [{ start: 18, end: 24, rule: "credential-assignment" }] },
      { available: true, excerpt: "HTTP/1.1 200 OK", redactions: [] },
      { available: true, excerpt: "GET /profile", redactions: [] },
      { available: true, excerpt: "extra selected bytes", redactions: [] },
    ],
  });
  await preview;
  assert.match(app.elements.evidencePacketContent.textContent, /preview_only/);
  assert.match(app.elements.evidencePacketContent.textContent, /password=\[REDACTED\]/);
  assert.equal(app.elements.evidencePacketContent.hidden, false);
  assert.equal(app.elements.evidencePacketControls.hidden, false);

  const switchProject = app.selectProject("B");
  assert.equal(app.elements.evidencePacketControls.hidden, true);
  assert.equal(app.elements.evidencePacketQuestion.value, "");
  assert.equal(app.state.evidencePacketDraft.selections.length, 0);
  app.requests[4].resolve(page("b.test"));
  await switchProject;
});

test("evidence packet span budget disables additions beyond 8192 bytes", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const revisionId = "b".repeat(64);
  app.state.projectKey = projectKey;
  const open = app.openEvidence({
    request_id: "large", revision_id: revisionId,
    record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  app.requests[0].resolve({
    project_key: projectKey, request_id: "large", revision_id: revisionId, side: "request", offset: 0,
    total_size: 12000, returned_size: 64, next_offset: 64, data: "GET /large HTTP/1.1", encoding: "utf-8",
    wire_fidelity: "source_bytes", parse_state: "complete", request_truncated: false,
  });
  await open;
  app.elements.evidencePacketQuestion.value = "Review these selected bytes.";
  app.elements.evidencePacketQuestion.listeners.input();
  app.elements.evidencePacketLength.value = "4096";
  app.elements.evidencePacketLength.listeners.input();
  assert.equal(app.addCurrentEvidenceSpan(), true);
  app.elements.evidencePacketOffset.value = "4096";
  app.elements.evidencePacketOffset.listeners.input();
  assert.equal(app.addCurrentEvidenceSpan(), true);
  assert.equal(app.elements.evidencePacketSelectionCount.textContent, "2 selected spans · 8192/8192 bytes");
  app.elements.evidencePacketOffset.value = "8192";
  app.elements.evidencePacketOffset.listeners.input();
  app.elements.evidencePacketLength.value = "1";
  app.elements.evidencePacketLength.listeners.input();
  assert.equal(app.elements.evidencePacketAdd.disabled, true);
});

test("late evidence packet response cannot populate after a project switch", async () => {
  const app = createApp();
  const projectKey = "a".repeat(64);
  const revisionId = "b".repeat(64);
  app.state.projectKey = projectKey;
  const open = app.openEvidence({
    request_id: "239", revision_id: revisionId,
    record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  app.requests[0].resolve({
    project_key: projectKey, request_id: "239", revision_id: revisionId, side: "request", offset: 0,
    total_size: 64, returned_size: 64, next_offset: null, data: "GET /items HTTP/1.1", encoding: "utf-8",
    wire_fidelity: "source_bytes", parse_state: "complete", request_truncated: false,
  });
  await open;
  app.elements.evidencePacketQuestion.value = "Inspect this request.";
  app.elements.evidencePacketQuestion.listeners.input();
  app.elements.evidencePacketOffset.value = "0";
  app.elements.evidencePacketOffset.listeners.input();
  app.elements.evidencePacketLength.value = "12";
  app.elements.evidencePacketLength.listeners.input();
  assert.equal(app.addCurrentEvidenceSpan(), true);

  const preview = app.previewEvidencePacket();
  const packetRequest = app.requests[1];
  const switchProject = app.selectProject("B");
  app.requests[2].resolve(page("b.test"));
  await switchProject;
  packetRequest.resolve({
    version: "1", preview_only: true, operator_review_needed: true,
    evidence: [{ available: true, excerpt: "late response" }],
  });
  await preview;
  assert.equal(app.elements.evidencePacketContent.hidden, true);
  assert.equal(app.elements.evidencePacketContent.textContent, "");
  assert.equal(app.state.evidencePacketDraft.selections.length, 0);
});

test("late request bytes cannot replace the response side after a fast tab switch", async () => {
  const app = createApp();
  const revisionId = "b".repeat(64);
  app.state.projectKey = "a".repeat(64);
  const requestPending = app.openEvidence({
    request_id: 239, revision_id: revisionId,
    record_source: "encrypted_local_capture", raw_stored_locally: true,
  });
  app.selectEvidenceSide("response");
  assert.equal(app.requests.length, 2);
  app.requests[1].resolve({
    project_key: "a".repeat(64), request_id: "239", revision_id: revisionId, side: "response", offset: 0,
    total_size: 8, returned_size: 8, next_offset: null, data: "HTTP/1.1", encoding: "utf-8", parse_state: "complete", wire_fidelity: "source_bytes",
  });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(app.elements.evidenceContent.textContent, "HTTP/1.1");
  app.requests[0].resolve({
    revision_id: revisionId, side: "request", total_size: 11, returned_size: 11,
    next_offset: null, data: "GET /old HTTP", encoding: "utf-8", parse_state: "complete", wire_fidelity: "source_bytes",
  });
  await requestPending;
  assert.equal(app.elements.evidenceContent.textContent, "HTTP/1.1");
});

test("stale graph continuation reloads the first page without reusing its cursor", async () => {
  const app = createApp();
  const seed = `endpoint-${"b".repeat(64)}`;
  app.state.projectKey = "a".repeat(64);
  app.state.graphSeeds = [seed];
  app.state.graph = { entities: [], assertions: [], next_cursor: "stale-token", result_id: "old-page" };
  app.state.graphHistory = [
    { graph: { next_cursor: "older-stale-token" }, seeds: [seed], view: "direct" },
    { graph: { next_cursor: "previous-stale-token" }, seeds: [seed], view: "direct" },
  ];

  const pending = app.loadGraph([seed], { view: "direct", cursor: "stale-token", appendPage: true, remember: true });
  assert.equal(app.requests.length, 1);
  assert.equal(app.requests[0].url.searchParams.get("cursor"), "stale-token");
  app.requests[0].fail(409, "graph changed; reload the first page before continuing");
  await pending;

  assert.equal(app.requests.length, 2, "a fresh first-page read should start after the conflict");
  assert.equal(app.requests[1].url.searchParams.has("cursor"), false);
  assert.equal("next_cursor" in app.state.graph, false, "the stale cursor must no longer be offered");
  assert.equal(app.state.graphHistory.length, 0, "previous-generation pages must not be restored by Back");
  assert.equal(app.elements.graphBack.disabled, true);
  assert.equal(app.state.loadingGraph, true, "the fresh read should remain in progress");
});

test("full graph continuation replaces client state with the cumulative server slice", async () => {
  const app = createApp();
  const seed = `endpoint-${"b".repeat(64)}`;
  const omitted = `assertion-${"c".repeat(64)}`;
  const cursor = "A".repeat(43);
  app.state.projectKey = "a".repeat(64);
  app.state.graphSeeds = [seed];
  app.state.graphView = "full";
  app.state.graph = {
    entities: [], assertions: [], assertion_nodes: [],
    missing_branches: [{ assertion_id: omitted, reason: "max_edges" }],
    continuation_scope: "rendered_missing_assertion_branches", continuation_state: "available",
    next_cursor: cursor, truncated: true, incomplete: true, result_id: "result-1",
  };
  app.updateGraphMoreControl();
  assert.equal(app.elements.graphMore.hidden, false);
  assert.equal(app.elements.graphMore.textContent, "More omitted branches");

  const pending = app.clickGraphMore();
  assert.equal(app.requests.length, 1);
  assert.equal(app.requests[0].url.searchParams.get("view"), "full");
  assert.equal(app.requests[0].url.searchParams.get("cursor"), cursor);
  app.requests[0].resolve({
    entities: [{ id: "cumulative-entity" }], assertions: [{ id: "cumulative-assertion" }], assertion_nodes: [{ id: omitted }],
    missing_branches: [], continuation_scope: "rendered_missing_assertion_branches",
    continuation_state: "branches_complete", truncated: false, incomplete: false,
    result_id: "result-2",
  });
  await pending;

  assert.equal(app.state.graphView, "full");
  assert.equal(app.state.graph.continuation_state, "branches_complete");
  assert.deepEqual(snapshot(app.state.graph.entities.map(row => row.id)), ["cumulative-entity"]);
  assert.deepEqual(snapshot(app.state.graph.assertions.map(row => row.id)), ["cumulative-assertion"]);
  assert.equal("client_merged_pages" in app.state.graph, false, "full slices are already cumulative on the server");
  assert.deepEqual(snapshot(app.state.graph.missing_branches), []);
  assert.equal(app.state.graph.next_cursor, undefined);
  assert.equal(app.elements.graphMore.hidden, true);
});

test("full graph requests preserve their 80-seed budget", async () => {
  const app = createApp();
  const seeds = Array.from({ length: 25 }, (_, index) => `endpoint-${String(index).padStart(64, "0")}`);
  app.state.projectKey = "a".repeat(64);
  const pending = app.loadGraph(seeds, { view: "full" });
  assert.equal(app.requests.length, 1);
  assert.equal(app.requests[0].url.searchParams.getAll("seed").length, seeds.length);
  app.requests[0].resolve({ entities: [], assertions: [], incomplete: true });
  await pending;
  assert.equal(app.state.graphSeeds.length, seeds.length);
});

test("stale full-slice continuation reloads full view without its cursor", async () => {
  const app = createApp();
  const seed = `endpoint-${"b".repeat(64)}`;
  const cursor = "A".repeat(43);
  app.state.projectKey = "a".repeat(64);
  app.state.graphSeeds = [seed];
  app.state.graphView = "full";
  app.state.graph = { entities: [], assertions: [], next_cursor: cursor, continuation_scope: "rendered_missing_assertion_branches" };

  const pending = app.loadGraph([seed], { view: "full", cursor, appendPage: true, remember: true });
  assert.equal(app.requests[0].url.searchParams.get("view"), "full");
  app.requests[0].fail(409, "graph changed; reload the first page before continuing");
  await pending;

  assert.equal(app.requests.length, 2, "a fresh full-slice read should start after the conflict");
  assert.equal(app.requests[1].url.searchParams.get("view"), "full");
  assert.equal(app.requests[1].url.searchParams.has("cursor"), false);
});

test("project switch aborts A, starts B immediately, and keeps B loading when A finishes", async () => {
  const app = createApp();
  const pendingA = app.selectProject("A");
  const pendingB = app.selectProject("B");

  assert.equal(app.requests.length, 2, "the pending A request must not suppress B");
  assert.equal(app.requests[0].signal.aborted, true);
  assert.equal(app.requests[1].url.searchParams.get("project_key"), "B");
  app.requests[0].resolve(page("a.test", { next_cursor: "cursor-a" }));
  await pendingA;

  assert.deepEqual(snapshot(app.state.endpoints), []);
  assert.equal(app.state.endpointCursor, null);
  assert.equal(app.state.loadingSurface, true);
  assert.equal(app.elements.loadMore.disabled, true);
  assert.equal(app.elements.loadMore.textContent, "Loading…");
  app.requests[1].resolve(page("b.test"));
  await pendingB;
  assert.equal(app.state.endpoints[0].host, "b.test");
  assert.deepEqual(snapshot(app.state.surfaceGaps), ["b.test-gap"]);
  assert.equal(app.state.loadingSurface, false);
  assert.equal(app.elements.loadMore.disabled, false);
});

test("late A success cannot overwrite an already loaded B page or its coverage", async () => {
  const app = createApp();
  const pendingA = app.selectProject("A");
  const pendingB = app.selectProject("B");
  assert.equal(app.requests.length, 2);
  const cursorB = "cursor-b";
  app.requests[1].resolve(page("b.test", { next_cursor: cursorB, total_operations: 4, coverage_state: "complete", truncated: true }));
  await pendingB;
  const statusB = app.elements.status.textContent;
  const updatedB = app.elements.updated.textContent;
  app.requests[0].resolve(page("a.test", { coverage_state: "unknown" }));
  await pendingA;

  assert.equal(app.state.endpoints[0].host, "b.test");
  assert.equal(app.state.endpointCursor, cursorB);
  assert.deepEqual(snapshot(app.state.surfaceGaps), ["b.test-gap"]);
  assert.equal(app.state.coverageState, "complete");
  assert.equal(app.elements.coverage.textContent, "Coverage: COMPLETE");
  assert.equal(app.elements.surfaceFoot.textContent, "4 matching operations · more available");
  assert.equal(app.elements.status.textContent, statusB);
  assert.equal(app.elements.updated.textContent, updatedB);
  assert.equal(app.elements.loadMore.hidden, false);
});

test("A to B to A rejects an old request even when the project key matches again", async () => {
  const app = createApp();
  const oldA = app.selectProject("A");
  const pendingB = app.selectProject("B");
  const newA = app.selectProject("A");
  assert.equal(app.requests.length, 3);
  app.requests[0].resolve(page("old-a.test"));
  await oldA;
  app.requests[1].reject(new Error("stale B failure"));
  await pendingB;
  assert.equal(app.state.loadingSurface, true);
  assert.deepEqual(snapshot(app.state.endpoints), []);
  app.requests[2].resolve(page("new-a.test"));
  await newA;
  assert.equal(app.state.endpoints[0].host, "new-a.test");
});

test("clearing a project aborts and invalidates its pending surface request", async () => {
  const app = createApp();
  const pending = app.selectProject("A");
  app.clearProjectView();
  assert.equal(app.requests[0].signal?.aborted, true);
  assert.equal(app.state.loadingSurface, false);
  assert.equal(app.elements.loadMore.disabled, false);
  const clearedTitle = app.elements.emptyTitle.textContent;
  app.requests[0].resolve(page("a.test", { next_cursor: "cursor-a" }));
  await pending;
  assert.equal(app.state.projectKey, "");
  assert.deepEqual(snapshot(app.state.endpoints), []);
  assert.deepEqual(snapshot(app.state.surfaceGaps), []);
  assert.equal(app.state.endpointCursor, null);
  assert.equal(app.elements.loadMore.hidden, true);
  assert.equal(app.elements.emptyTitle.textContent, clearedTitle);
});

test("stale request rejection cannot report an error or clear the current loading state", async () => {
  const app = createApp();
  const pendingA = app.selectProject("A");
  const pendingB = app.selectProject("B");
  assert.equal(app.requests.length, 2);
  app.requests[0].reject(new Error("stale source unavailable"));
  await pendingA;
  assert.equal(app.elements.status.textContent, "Loading local index");
  assert.equal(app.state.loadingSurface, true);
  assert.equal(app.elements.loadMore.disabled, true);
  app.requests[1].resolve(page("b.test"));
  await pendingB;
});

test("server-side filter resets pagination and late responses from the old filter are ignored", async () => {
  const app = createApp();
  const initial = app.selectProject("A");
  app.requests[0].resolve(page("old.test", { next_cursor: "old-filter-cursor", truncated: true }));
  await initial;
  const oldFilterPage = app.loadSurface(true);
  assert.equal(app.requests.length, 2);

  app.elements.filter.value = "needle";
  app.elements.filter.listeners.input();
  assert.equal(app.requests[1].signal.aborted, true);
  assert.deepEqual(snapshot(app.state.endpoints), []);
  assert.equal(app.state.endpointCursor, null);
  await new Promise(resolve => setTimeout(resolve, 275));
  assert.equal(app.requests.length, 3);
  assert.equal(app.requests[2].url.searchParams.get("filter"), "needle");
  assert.equal(app.requests[2].url.searchParams.has("cursor"), false);
  app.requests[2].resolve({ operations: [endpoint("needle.test")], total_operations: 1, coverage_state: "partial" });
  await new Promise(resolve => setTimeout(resolve, 0));
  app.requests[1].resolve(page("late-old-filter.test", { total_operations: 99 }));
  await oldFilterPage;
  assert.deepEqual(snapshot(app.state.endpoints.map(row => row.host)), ["needle.test"]);
  assert.equal(app.state.totalOperations, 1);
});

test("pagination sends the opaque cursor and appends complete operation groups", async () => {
  const app = createApp();
  const first = app.selectProject("A");
  const cursor = "signed-page-cursor";
  app.requests[0].resolve(page("a.test", { next_cursor: cursor, truncated: true, total_operations: 3 }));
  await first;
  const selectedEndpoint = app.state.endpoints[0];
  app.state.selectedEndpoint = selectedEndpoint;
  const append = app.loadSurface(true);
  await app.loadSurface(true);
  assert.equal(app.requests.length, 2);
  assert.equal(app.requests[1].url.searchParams.get("cursor"), cursor);
  assert.equal(app.requests[1].url.searchParams.get("filter"), "");
  app.requests[1].resolve({ operations: [endpoint("b.test", "/second"), endpoint("c.test", "/third")], total_operations: 3, coverage_state: "partial" });
  await append;

  assert.equal(app.state.endpoints.length, 3);
  assert.equal(app.state.endpoints.find(row => row.route === "/route").observation_count, 1);
  assert.equal(app.state.selectedEndpoint, selectedEndpoint);
  assert.equal(app.state.endpointCursor, null);
  assert.equal(app.elements.loadMore.hidden, true);
  assert.equal(app.state.loadingSurface, false);
});

test("stale operation cursor clears mixed pages and reloads the first filtered page", async () => {
  const app = createApp();
  const first = app.selectProject("A");
  app.requests[0].resolve(page("a.test", { next_cursor: "stale-cursor", truncated: true }));
  await first;
  app.state.selectedEndpoint = app.state.endpoints[0];
  app.state.graph = { entities: [{ id: "old-node" }], assertions: [] };
  app.state.selectedNode = { id: "old-node" };
  app.elements.inspector.replaceChildren({ textContent: "old details" });
  const append = app.loadSurface(true);
  app.requests[1].fail(409, "local structural index changed; reload the first page");
  await append;
  assert.equal(app.requests.length, 3);
  assert.equal(app.requests[2].url.searchParams.has("cursor"), false);
  assert.deepEqual(snapshot(app.state.endpoints), []);
  assert.equal(app.state.selectedEndpoint, null);
  assert.equal(app.state.graph, null);
  assert.equal(app.state.selectedNode, null);
  assert.equal(app.elements.inspector.children[0].children[1].textContent, "Nothing selected");
  assert.equal(app.state.loadingSurface, true);
  app.requests[2].resolve(page("fresh.test"));
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.deepEqual(snapshot(app.state.endpoints.map(row => row.host)), ["fresh.test"]);
  assert.equal(app.state.loadingSurface, false);
});

test("an append failure keeps the current page and cursor available for retry", async () => {
  const app = createApp();
  const first = app.selectProject("A");
  const cursor = "retry-cursor";
  app.requests[0].resolve(page("a.test", { next_cursor: cursor }));
  await first;
  const append = app.loadSurface(true);
  app.requests[1].reject(new Error("synthetic read failure"));
  await append;
  assert.equal(app.state.endpoints[0].host, "a.test");
  assert.equal(app.state.endpointCursor, cursor);
  assert.equal(app.elements.loadMore.hidden, false);
  assert.equal(app.elements.loadMore.disabled, false);
  assert.equal(app.elements.status.textContent, "Local index read error");
});

test("switching projects during append rejects old rows and starts B without A's cursor", async () => {
  const app = createApp();
  const first = app.selectProject("A");
  app.requests[0].resolve(page("a.test", { next_cursor: "cursor-a" }));
  await first;
  const appendA = app.loadSurface(true);
  const pendingB = app.selectProject("B");
  assert.equal(app.requests.length, 3);
  assert.equal(app.requests[1].signal.aborted, true);
  assert.equal(app.requests[2].url.searchParams.has("cursor"), false);
  app.requests[1].resolve(page("a-second-page.test"));
  await appendA;
  assert.deepEqual(snapshot(app.state.endpoints), []);
  assert.equal(app.state.loadingSurface, true);
  assert.equal(app.elements.loadMore.disabled, true);
  app.requests[2].resolve(page("b.test"));
  await pendingB;
  assert.deepEqual(snapshot(app.state.endpoints).map(row => row.host), ["b.test"]);
});

test("clearing a loaded project removes its rendered endpoints and invalidates append", async () => {
  const app = createApp();
  const first = app.selectProject("A");
  app.requests[0].resolve(page("a.test", { next_cursor: "cursor-a" }));
  await first;
  assert.equal(app.elements.endpointList.children.some(child => child.className?.startsWith("endpoint-item")), true);
  const appendA = app.loadSurface(true);
  await app.selectProject("");
  assert.equal(app.requests[1].signal.aborted, true);
  assert.equal(app.elements.endpointList.children.some(child => child.className?.startsWith("endpoint-item")), false);
  app.requests[1].resolve(page("a-second-page.test"));
  await appendA;
  assert.deepEqual(snapshot(app.state.endpoints), []);
  assert.equal(app.state.endpointCursor, null);
  assert.equal(app.state.loadingSurface, false);
  assert.equal(app.elements.loadMore.hidden, true);
});
