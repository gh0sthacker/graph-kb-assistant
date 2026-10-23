const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

// Exercise the shipped request/state functions, including their real rendering
// calls, with an in-memory DOM and a manually completed fetch transport. The
// transport deliberately ignores abort so stale-response guards are required.
function createApp() {
  const nodes = new Map();
  function node() {
    return {
      textContent: "", value: "", hidden: false, disabled: false, children: [],
      classList: { add() {}, remove() {}, toggle() {} },
      append(...children) { this.children.push(...children); },
      replaceChildren(...children) { this.children = children; },
      setAttribute() {}, addEventListener() {},
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
    document, URLSearchParams, AbortController, setTimeout, clearTimeout,
    fetch(url, options) {
      return new Promise((resolve, reject) => {
        requests.push({
          url: new URL(url, "http://local.test"), signal: options.signal,
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
    "\nglobalThis.app = { state, elements, selectProject, clearProjectView, loadSurface, mergeGraphPages, loadGraph };", context);
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
  return { endpoints: [endpoint(host)], coverage_state: "partial", coverage_gaps: [`${host}-gap`], ...extra };
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

test("project switch aborts A, starts B immediately, and keeps B loading when A finishes", async () => {
  const app = createApp();
  const pendingA = app.selectProject("A");
  const pendingB = app.selectProject("B");

  assert.equal(app.requests.length, 2, "the pending A request must not suppress B");
  assert.equal(app.requests[0].signal.aborted, true);
  assert.equal(app.requests[1].url.searchParams.get("project_key"), "B");
  app.requests[0].resolve(page("a.test", { next_after: { historical_row_id: 10, live_row_id: 1 } }));
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
  const cursorB = { historical_row_id: 22, live_row_id: 3 };
  app.requests[1].resolve(page("b.test", { next_after: cursorB, coverage_state: "complete", truncated: true }));
  await pendingB;
  const statusB = app.elements.status.textContent;
  const updatedB = app.elements.updated.textContent;
  app.requests[0].resolve(page("a.test", { coverage_state: "unknown" }));
  await pendingA;

  assert.equal(app.state.endpoints[0].host, "b.test");
  assert.deepEqual(snapshot(app.state.endpointCursor), cursorB);
  assert.deepEqual(snapshot(app.state.surfaceGaps), ["b.test-gap"]);
  assert.equal(app.state.coverageState, "complete");
  assert.equal(app.elements.coverage.textContent, "Coverage: COMPLETE");
  assert.equal(app.elements.surfaceFoot.textContent, "Page limit reached · more available");
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
  app.requests[0].resolve(page("a.test", { next_after: { historical_row_id: 1, live_row_id: 1 } }));
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

test("pagination keeps the current cursor, prevents duplicate loads, and merges endpoint counts", async () => {
  const app = createApp();
  const first = app.selectProject("A");
  const cursor = { historical_row_id: 18, live_row_id: 7 };
  app.requests[0].resolve(page("a.test", { next_after: cursor }));
  await first;
  const selectedEndpoint = app.state.endpoints[0];
  app.state.selectedEndpoint = selectedEndpoint;
  const append = app.loadSurface(true);
  await app.loadSurface(true);
  assert.equal(app.requests.length, 2);
  assert.equal(app.requests[1].url.searchParams.get("after_history"), "18");
  assert.equal(app.requests[1].url.searchParams.get("after_live"), "7");
  app.requests[1].resolve({ endpoints: [endpoint("a.test", "/route", 2), endpoint("a.test", "/second")], coverage_state: "partial" });
  await append;

  assert.equal(app.state.endpoints.length, 2);
  assert.equal(app.state.endpoints.find(row => row.route === "/route").observation_count, 3);
  assert.equal(app.state.selectedEndpoint, selectedEndpoint);
  assert.equal(app.state.endpointCursor, null);
  assert.equal(app.elements.loadMore.hidden, true);
  assert.equal(app.state.loadingSurface, false);
});

test("an append failure keeps the current page and cursor available for retry", async () => {
  const app = createApp();
  const first = app.selectProject("A");
  const cursor = { historical_row_id: 18, live_row_id: 7 };
  app.requests[0].resolve(page("a.test", { next_after: cursor }));
  await first;
  const append = app.loadSurface(true);
  app.requests[1].reject(new Error("synthetic read failure"));
  await append;
  assert.equal(app.state.endpoints[0].host, "a.test");
  assert.deepEqual(snapshot(app.state.endpointCursor), cursor);
  assert.equal(app.elements.loadMore.hidden, false);
  assert.equal(app.elements.loadMore.disabled, false);
  assert.equal(app.elements.status.textContent, "Local index read error");
});

test("switching projects during append rejects old rows and starts B without A's cursor", async () => {
  const app = createApp();
  const first = app.selectProject("A");
  app.requests[0].resolve(page("a.test", { next_after: { historical_row_id: 18, live_row_id: 7 } }));
  await first;
  const appendA = app.loadSurface(true);
  const pendingB = app.selectProject("B");
  assert.equal(app.requests.length, 3);
  assert.equal(app.requests[1].signal.aborted, true);
  assert.equal(app.requests[2].url.searchParams.has("after_history"), false);
  assert.equal(app.requests[2].url.searchParams.has("after_live"), false);
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
  app.requests[0].resolve(page("a.test", { next_after: { historical_row_id: 18, live_row_id: 7 } }));
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
