const assert = require("node:assert/strict");
const test = require("node:test");

const { focusedNeighborhood, focusedTransform, observationNotes } = require("./assets/graph-focus.js");

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
  assert.match(notes[1].body, /does not mean the HTTP exchange has no context/i);
});

test("observationNotes do not add uncertainty notes to a complete, annotated capture", () => {
  assert.deepEqual(observationNotes({
    parse_state: "complete",
    context: { scenario: "checkout" },
    observed_at: "2026-10-24T12:00:00Z",
  }), []);
});
