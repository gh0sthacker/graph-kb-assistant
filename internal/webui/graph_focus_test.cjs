const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const {
  focusedNeighborhood,
  focusedTransform,
  layeredGridLayout,
  observationNotes,
  operationAliasFor,
  withObservationParseState,
  routeFamilyUncertainty,
} = require("./assets/graph-focus.js");

test("routeFamilyUncertainty explains that masked segments are unresolved", () => {
  const result = routeFamilyUncertainty("/api/{value}/search/{value}");

  assert.deepEqual(result, {
    maskedSegments: 2,
    message: "2 masked path segments are unresolved. Each could be a variable value or an unrecognized static route word; this grouping does not confirm parameter behavior.",
  });
});

test("routeFamilyUncertainty keeps heuristic caveat when no segments are masked", () => {
  const result = routeFamilyUncertainty("/api/products/search");

  assert.equal(result.maskedSegments, 0);
  assert.match(result.message, /grouping still uses a static-word vocabulary/i);
});

test("routeFamilyUncertainty also counts legacy endpoint template segments", () => {
  const result = routeFamilyUncertainty("/rest/user/{parameter}/");

  assert.equal(result.maskedSegments, 1);
  assert.match(result.message, /variable value or an unrecognized static route word/i);
});

test("routeFamilyUncertainty rejects missing route-family values", () => {
  assert.equal(routeFamilyUncertainty(undefined), null);
  assert.equal(routeFamilyUncertainty(42), null);
});

test("operation aliases are display-only and apply only to operation nodes", () => {
  const aliases = new Map([["operation-abc", { alias: "Account settings", version: 2 }]]);
  assert.equal(operationAliasFor({ id: "operation-abc", kind: "operation" }, aliases), "Account settings");
  assert.equal(operationAliasFor({ id: "endpoint-abc", kind: "endpoint" }, aliases), "");
  assert.equal(operationAliasFor({ id: "operation-missing", kind: "operation" }, aliases), "");
});

test("desktop workspace keeps the graph viewport inside the browser window", () => {
  const css = fs.readFileSync(path.join(__dirname, "assets", "style.css"), "utf8");
  const desktopShellRule = css.match(/\.app-shell\{([^}]+)\}/)?.[1] || "";

  assert.match(desktopShellRule, /height:100vh/);
  assert.match(desktopShellRule, /overflow:hidden/);
});

test("focusedNeighborhood keeps the selected node and its direct neighbors", () => {
  const entities = [
    { id: "endpoint-a", kind: "endpoint" },
    { id: "observation-a", kind: "observation" },
    { id: "parameter-a", kind: "parameter" },
    { id: "response-a", kind: "response_variant" },
  ];
  const assertions = [
    { id: "edge-endpoint-observation", subject: "endpoint-a", object: "observation-a" },
    { id: "edge-endpoint-parameter", subject: "endpoint-a", object: "parameter-a" },
    { id: "edge-observation-response", subject: "observation-a", object: "response-a" },
  ];

  const focused = focusedNeighborhood(entities, assertions, "endpoint-a");

  assert.deepEqual(focused.entities.map(node => node.id), ["endpoint-a", "observation-a", "parameter-a"]);
  assert.deepEqual(focused.assertions.map(edge => edge.id), ["edge-endpoint-observation", "edge-endpoint-parameter"]);
});

test("focusedNeighborhood returns an empty view for a node absent from the loaded slice", () => {
  const focused = focusedNeighborhood([{ id: "endpoint-a" }], [], "missing-node");

  assert.deepEqual(focused, { entities: [], assertions: [] });
});

test("focusedTransform centers a node at the requested zoom", () => {
  assert.deepEqual(focusedTransform({ x: 200, y: 100 }, 1000, 600, 3), {
    panX: 900,
    panY: 600,
  });
});

test("focusedTransform rejects incomplete or invalid geometry", () => {
  assert.equal(focusedTransform(null, 1000, 600, 3), null);
  assert.equal(focusedTransform({ x: 20, y: 30 }, 0, 600, 3), null);
  assert.equal(focusedTransform({ x: 20, y: 30 }, 1000, 600, Number.NaN), null);
});

test("observationNotes distinguish incomplete parsing from missing context and time", () => {
  const notes = observationNotes({
    parse_state: "incomplete",
    context: { scenario: "unknown" },
    observed_at: "unknown",
  });

  assert.deepEqual(notes.map(note => note.title), ["Parsing is incomplete", "Context is not recorded", "Time is not recorded"]);
  assert.match(notes[0].body, /request and response/i);
  assert.match(notes[0].body, /does not by itself mean.*truncated/i);
  assert.match(notes[1].body, /does not mean the HTTP exchange has no context/i);
});

test("observationNotes do not add uncertainty notes to a complete, annotated capture", () => {
  assert.deepEqual(observationNotes({
    parse_state: "complete",
    context: { scenario: "checkout" },
    observed_at: "2026-10-24T12:00:00Z",
  }), []);
});

test("analysis state does not masquerade as parse completeness", () => {
  const notes = observationNotes({
    analysis_state: "incomplete",
    context: { scenario: "checkout" },
    observed_at: "2026-10-24T12:00:00Z",
  });

  assert.deepEqual(notes.map(note => note.title), ["Parse completeness is not recorded"]);
  assert.match(notes[0].body, /analysis state is separate/i);
});

test("observation inspector uses parse state from the matching current capture provenance", () => {
  const node = { id: "observation-a", kind: "observation", data: { analysis_state: "incomplete" } };
  const provenance = [
    { graph_observation_id: "observation-a", parse_state: "unknown", revision_mapping: "revision" },
    { graph_observation_id: "observation-a", parse_state: "complete", revision_mapping: "current" },
  ];

  const enriched = withObservationParseState(node, provenance);

  assert.equal(enriched.data.parse_state, "complete");
  assert.equal(enriched.data.analysis_state, "incomplete");
  assert.equal(node.data.parse_state, undefined, "the graph node remains unchanged");
  assert.equal(withObservationParseState(node, [provenance[0]]).data.parse_state, "unknown");
});

test("layeredGridLayout wraps a high-degree layer into a bounded grid", () => {
  const entities = [{ id: "root", kind: "observation" }];
  const assertions = [];
  for (let index = 0; index < 99; index++) {
    const id = `neighbor-${String(index).padStart(2, "0")}`;
    entities.push({ id, kind: "parameter" });
    assertions.push({ id: `edge-${index}`, subject: "root", object: id });
  }

  const layout = layeredGridLayout(entities, assertions, "root", node => node.id);
  const root = layout.positions.get("root");
  const leafPoints = entities.slice(1).map(node => layout.positions.get(node.id));

  assert.equal(layout.positions.size, 100);
  assert.equal(root.x, 120);
  assert.equal(root.y, layout.height / 2);
  assert.equal(new Set(leafPoints.map(point => point.x)).size, 10);
  assert(layout.height < 1600, `layout height ${layout.height} should wrap the 99-node layer`);
  assert(layout.width < 2800, `layout width ${layout.width} should remain bounded`);
  assert.equal(new Set(leafPoints.map(point => `${point.x},${point.y}`)).size, 99);
});
