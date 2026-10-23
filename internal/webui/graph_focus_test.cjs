const assert = require("node:assert/strict");
const test = require("node:test");

const { focusedNeighborhood } = require("./assets/graph-focus.js");

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
