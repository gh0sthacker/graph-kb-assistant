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
      options: [], listeners: {},
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
  assert.match(source, /\nloadProjects\(\);\s*$/);
  vm.runInContext(source.replace(/\nloadProjects\(\);\s*$/, "") +
    "\nglobalThis.app = { state, elements, selectProject, clearProjectView, loadSurface, mergeGraphPages, loadGraph, openEvidence, selectEvidenceSide, addCurrentEvidenceSpan, removeEvidencePacketSelection, clearEvidencePacketDraft, previewEvidencePacket, setComparisonBaseline, openComparison, formatCaptureComparison, displayTitle, saveOperationAlias, renderInspector, updateGraphMoreControl, clickGraphMore: () => elements.graphMore.listeners.click?.() };", context);
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
    request_id: "239", revision_id: revisionId, side: "request", offset: 0,
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
    request_id: "239", revision_id: revisionId, side: "response", offset: 0,
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

test("local evidence viewer refuses non-local and live-only records without requesting bytes", async () => {
  const app = createApp();
  app.state.projectKey = "a".repeat(64);
  await app.openEvidence({ request_id: 239, record_source: "caido_history", raw_stored_locally: false });
  assert.equal(app.requests.length, 0);
  assert.equal(app.elements.evidenceDialog.open, false);
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
    projectKey, requestId: "239", revisionId: baselineRevision,
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
    request_id: "239", revision_id: revisionId, side: "request", offset: 0,
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
    request_id: "239", revision_id: revisionId, side: "response", offset: 0,
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
    request_id: "240", revision_id: secondRevisionId, side: "request", offset: 0,
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
    request_id: "large", revision_id: revisionId, side: "request", offset: 0,
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
    request_id: "239", revision_id: revisionId, side: "request", offset: 0,
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
    revision_id: revisionId, side: "response", total_size: 8, returned_size: 8,
    next_offset: null, data: "HTTP/1.1", encoding: "utf-8", parse_state: "complete", wire_fidelity: "source_bytes",
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
