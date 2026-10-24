const assert = require("node:assert/strict");
const test = require("node:test");

const { focusedNeighborhood, focusedTransform } = require("./assets/graph-focus.js");

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
