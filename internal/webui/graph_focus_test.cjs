const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const {
  focusedNeighborhood,
  focusedTransform,
  layeredGridLayout,
  contextAnnotationEvidenceReferences,
  contextAnnotationsForObservation,
  observationRequestOrder,
  observationNotes,
  operationAliasFor,
  analysisCardsForNode,
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

test("context labels resolve to exact evidence references and bounded redacted excerpts", () => {
  const references = contextAnnotationEvidenceReferences({
    evidence_reference_ids: ["ref-b", "missing", "ref-a", "ref-b"],
  }, [
    { evidence_reference_id: "ref-a", evidence_index: 0, side: "request", request_id: "239", revision_id: "rev-a", span_start: 4, span_end: 20, raw: "secret" },
    { evidence_reference_id: "ref-b", evidence_index: 1, side: "response", request_id: "240", revision_id: "rev-b", span_start: 8, span_end: 22, raw: "secret" },
  ], {
    evidence: [
      { request_id: "239", revision_id: "rev-a", side: "request", span_start: 4, span_end: 20, available: true, excerpt: "GET /items?id=[REDACTED]" },
      { request_id: "240", revision_id: "rev-b", side: "response", span_start: 8, span_end: 22, available: true, excerpt: "{\"id\":\"[REDACTED]\"}" },
    ],
  });

  assert.deepEqual(references, [
    { evidenceIndex: 0, side: "request", requestId: "239", spanStart: 4, spanEnd: 20, available: true, excerpt: "GET /items?id=[REDACTED]", excerptTruncated: false },
    { evidenceIndex: 1, side: "response", requestId: "240", spanStart: 8, spanEnd: 22, available: true, excerpt: "{\"id\":\"[REDACTED]\"}", excerptTruncated: false },
  ]);
  assert.equal(JSON.stringify(references).includes("secret"), false, "raw input fields must not be returned");
});

test("flow context references preserve saved operator order while other kinds keep evidence order", () => {
  const evidenceReferences = [
    { evidence_reference_id: "ref-a", evidence_index: 0, side: "request", request_id: "1", revision_id: "rev-a" },
    { evidence_reference_id: "ref-b", evidence_index: 1, side: "response", request_id: "2", revision_id: "rev-b" },
  ];
  const packet = { evidence: [] };
  const flow = contextAnnotationEvidenceReferences({
    kind: "flow", evidence_reference_ids: ["ref-b", "ref-a"],
  }, evidenceReferences, packet);
  const actor = contextAnnotationEvidenceReferences({
    kind: "actor", evidence_reference_ids: ["ref-b", "ref-a"],
  }, evidenceReferences, packet);

  assert.deepEqual(flow.map(reference => [reference.evidenceIndex, reference.flowStep]), [[1, 1], [0, 2]]);
  assert.deepEqual(actor.map(reference => [reference.evidenceIndex, reference.flowStep]), [[0, undefined], [1, undefined]]);
});

test("context evidence excerpt refuses unavailable spans and caps malformed long previews", () => {
  const references = contextAnnotationEvidenceReferences({ evidence_reference_ids: ["ref-a", "ref-b"] }, [
    { evidence_reference_id: "ref-a", evidence_index: 0, side: "request", request_id: "1", revision_id: "rev-a", span_start: 0, span_end: 9 },
    { evidence_reference_id: "ref-b", evidence_index: 1, side: "response", request_id: "2", revision_id: "rev-b", span_start: 1, span_end: 8 },
  ], {
    evidence: [
      { request_id: "1", revision_id: "rev-a", side: "request", span_start: 0, span_end: 9, available: false, excerpt: "must not be exposed" },
      { request_id: "2", revision_id: "rev-b", side: "response", span_start: 1, span_end: 8, available: true, excerpt: "x".repeat(5000) },
    ],
  });

  assert.equal(references[0].available, false);
  assert.equal(references[0].excerpt, "");
  assert.equal(references[1].excerpt.length, 4096);
  assert.equal(references[1].excerptTruncated, true);
});

test("context excerpt is hidden when packet evidence does not match the exact revision and span", () => {
  const references = contextAnnotationEvidenceReferences({ evidence_reference_ids: ["ref-a", "ref-b"] }, [
    { evidence_reference_id: "ref-a", evidence_index: 0, side: "response", request_id: "9", revision_id: "rev-a", span_start: 4, span_end: 12 },
    { evidence_reference_id: "ref-b", evidence_index: 1, side: "response", request_id: "10", revision_id: "rev-b", span_start: 4, span_end: 12 },
  ], {
    evidence: [
      { request_id: "9", revision_id: "rev-other", side: "response", span_start: 4, span_end: 12, available: true, excerpt: "wrong revision" },
      { request_id: "10", revision_id: "rev-b", side: "response", span_start: 4, span_end: 13, available: true, excerpt: "wrong span" },
    ],
  });

  assert.equal(references[0].available, false);
  assert.equal(references[0].excerpt, "");
  assert.equal(references[1].available, false);
  assert.equal(references[1].excerpt, "");
});

test("context excerpt display cap is measured in UTF-8 bytes", () => {
  const [reference] = contextAnnotationEvidenceReferences({ evidence_reference_ids: ["ref-a"] }, [
    { evidence_reference_id: "ref-a", evidence_index: 0, side: "request", request_id: "1", revision_id: "rev-a", span_start: 0, span_end: 5000 },
  ], {
    evidence: [{ request_id: "1", revision_id: "rev-a", side: "request", span_start: 0, span_end: 5000, available: true, excerpt: "é".repeat(5000) }],
  });

  assert.equal(new TextEncoder().encode(reference.excerpt).length, 4096);
  assert.equal(reference.excerptTruncated, true);
});

test("accepted analyst context resolves only exact project and observation revisions", () => {
  const observation = { id: "observation-a", kind: "observation" };
  const revision = "a".repeat(64);
  const session = {
    id: "consultation-1", projectKey: "project-a", version: 4, accepted: true, reviewLoaded: true,
    evidenceReferences: [{
      evidence_reference_id: "ref-a", evidence_index: 0, side: "request",
      request_id: "239", revision_id: revision,
    }],
    contextAnnotations: [{
      context_annotation_id: "annotation-a", session_version: 2, kind: "actor", label: "actor-a",
      evidence_reference_ids: ["ref-a"], origin: "analyst", evidence_state: "analyst_provided",
      excerpt: "must not be returned",
    }],
  };
  const provenance = [{ graph_observation_id: observation.id, request_id: "239", revision_id: revision }];

  assert.deepEqual(contextAnnotationsForObservation(observation, provenance, session, "project-a"), [{
    annotationID: "annotation-a", annotationVersion: 2, kind: "actor", label: "actor-a",
    evidenceReferences: [{
      referenceID: "ref-a", evidenceIndex: 0, side: "request", requestId: "239", revisionId: revision,
    }],
    sessionID: "consultation-1", sessionVersion: 4,
  }]);
  assert.equal(JSON.stringify(contextAnnotationsForObservation(observation, provenance, session, "project-a"))
    .includes("must not be returned"), false);
});

test("accepted flow context preserves analyst order for exact observation references", () => {
  const observation = { id: "observation-flow", kind: "observation" };
  const revision = "c".repeat(64);
  const session = {
    id: "consultation-flow", projectKey: "project-a", version: 2, accepted: true, reviewLoaded: true,
    evidenceReferences: [
      { evidence_reference_id: "request-ref", evidence_index: 0, side: "request", request_id: "239", revision_id: revision },
      { evidence_reference_id: "response-ref", evidence_index: 1, side: "response", request_id: "239", revision_id: revision },
    ],
    contextAnnotations: [{
      context_annotation_id: "flow-annotation", session_version: 2, kind: "flow", label: "submit-response",
      evidence_reference_ids: ["response-ref", "request-ref"], origin: "analyst", evidence_state: "analyst_provided",
    }],
  };
  const annotations = contextAnnotationsForObservation(observation, [
    { graph_observation_id: observation.id, request_id: "239", revision_id: revision },
  ], session, "project-a");

  assert.deepEqual(annotations[0].evidenceReferences.map(reference => [reference.evidenceIndex, reference.flowStep]), [[1, 1], [0, 2]]);
});

test("accepted analyst context rejects unaccepted, foreign, or mismatched evidence", () => {
  const observation = { id: "observation-a", kind: "observation" };
  const revision = "a".repeat(64);
  const session = {
    id: "consultation-1", projectKey: "project-a", version: 4, accepted: true, reviewLoaded: true,
    evidenceReferences: [{
      evidence_reference_id: "ref-a", evidence_index: 0, side: "response",
      request_id: "239", revision_id: revision,
    }],
    contextAnnotations: [{
      context_annotation_id: "annotation-a", session_version: 2, kind: "flow", label: "flow-a",
      evidence_reference_ids: ["ref-a"], origin: "analyst", evidence_state: "analyst_provided",
    }],
  };
  const exactProvenance = [{ graph_observation_id: observation.id, request_id: "239", revision_id: revision }];
  const empty = [];

  assert.deepEqual(contextAnnotationsForObservation(observation, exactProvenance, { ...session, accepted: false }, "project-a"), empty);
  assert.deepEqual(contextAnnotationsForObservation(observation, exactProvenance, { ...session, reviewLoaded: false }, "project-a"), empty);
  assert.deepEqual(contextAnnotationsForObservation(observation, exactProvenance, session, "project-b"), empty);
  assert.deepEqual(contextAnnotationsForObservation(observation, [
    { graph_observation_id: observation.id, request_id: "239", revision_id: "b".repeat(64) },
  ], session, "project-a"), empty);
  assert.deepEqual(contextAnnotationsForObservation(observation, [
    { graph_observation_id: "observation-other", request_id: "239", revision_id: revision },
  ], session, "project-a"), empty);
  assert.deepEqual(contextAnnotationsForObservation(observation, exactProvenance, {
    ...session, contextAnnotations: [{ ...session.contextAnnotations[0], evidence_reference_ids: ["ref-other"] }],
  }, "project-a"), empty);
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

test("observationRequestOrder reports a source-backed exact precedes edge", () => {
  const prior = { id: "observation-prior", kind: "observation", data: { observed_at: "2026-10-24T10:00:00Z" } };
  const selected = { id: "observation-selected", kind: "observation", data: { observed_at: "2026-10-24T10:01:00Z" } };
  const assertionID = `assertion-${"a".repeat(64)}`;
  const sha256 = "b".repeat(64);
  const assertion = {
    id: assertionID, subject: prior.id, predicate: "precedes", object: selected.id,
    origin_kind: "operator_asserted", evidence_state: "observed",
    source_refs: [{ path: "/tmp/synthetic-source.jsonl", sha256, record_locator: "line:17" }],
  };

  assert.deepEqual(observationRequestOrder(selected, [prior, selected], [assertion]), {
    status: "supported",
    initiatingRequestIDs: [prior.id],
    relations: [{
      assertionID, subjectID: prior.id, objectID: selected.id, direction: "incoming",
      originKind: "operator_asserted", evidenceState: "observed",
      sourceIDs: [{ sha256, recordLocator: "line:17" }],
    }],
  });

  const next = { id: "observation-next", kind: "observation" };
  const outgoing = observationRequestOrder(selected, [prior, selected, next], [{
    ...assertion, id: `assertion-${"c".repeat(64)}`, subject: selected.id, object: next.id,
  }]);
  assert.deepEqual(outgoing.initiatingRequestIDs, []);
  assert.equal(outgoing.relations[0].direction, "outgoing");
  assert.equal(outgoing.relations[0].subjectID, selected.id);
  assert.equal(outgoing.relations[0].objectID, next.id);
});

test("observationRequestOrder stays unknown when the graph slice lacks an exact endpoint", () => {
  const selected = { id: "observation-selected", kind: "observation", data: { observed_at: "2026-10-24T10:01:00Z" } };
  const assertion = {
    id: `assertion-${"a".repeat(64)}`, subject: "observation-omitted", predicate: "precedes", object: selected.id,
    origin_kind: "parsed", evidence_state: "observed",
    source_refs: [{ path: "/tmp/synthetic-source.jsonl", sha256: "b".repeat(64), record_locator: "line:17" }],
  };

  assert.deepEqual(observationRequestOrder(selected, [selected], [assertion]), {
    status: "unknown", initiatingRequestIDs: [], relations: [],
  });
  assert.deepEqual(observationRequestOrder(selected, [selected], []), {
    status: "unknown", initiatingRequestIDs: [], relations: [],
  });
  assert.deepEqual(observationRequestOrder(selected, undefined, undefined), {
    status: "unknown", initiatingRequestIDs: [], relations: [],
  });
});

test("observationRequestOrder rejects missing source refs and proposed or contested edges", () => {
  const prior = { id: "observation-prior", kind: "observation" };
  const selected = { id: "observation-selected", kind: "observation" };
  const source = { path: "/tmp/synthetic-source.jsonl", sha256: "b".repeat(64), record_locator: "line:17" };
  const base = {
    id: `assertion-${"a".repeat(64)}`, subject: prior.id, predicate: "precedes", object: selected.id,
    origin_kind: "operator_asserted", evidence_state: "observed", source_refs: [source],
  };
  const unknown = { status: "unknown", initiatingRequestIDs: [], relations: [] };

  assert.deepEqual(observationRequestOrder(selected, [prior, selected], [{ ...base, source_refs: [] }]), unknown);
  assert.deepEqual(observationRequestOrder(selected, [prior, selected], [{ ...base, source_refs: [{ ...source, sha256: "invalid" }] }]), unknown);
  assert.deepEqual(observationRequestOrder(selected, [prior, selected], [{ ...base, evidence_state: "proposed" }]), unknown);
  assert.deepEqual(observationRequestOrder(selected, [prior, selected], [{ ...base, evidence_state: "contested" }]), unknown);
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

test("analysis cards resolve to an observation by exact card or capture identity", () => {
  const node = { id: "observation-a", kind: "observation", data: { card_id: "card-a", capture_id: "capture-a" } };
  const cards = [
    { id: "card-c", capture_id: "capture-c", endpoint: { id: "endpoint-a" } },
    { id: "card-b", capture_id: "capture-a", endpoint: { id: "endpoint-b" } },
    { id: "card-a", capture_id: "capture-other", endpoint: { id: "endpoint-c" } },
    { id: "card-b", capture_id: "capture-a", endpoint: { id: "endpoint-b" } },
  ];

  assert.deepEqual(analysisCardsForNode(node, cards).map(card => card.id), ["card-a", "card-b"]);
  assert.deepEqual(analysisCardsForNode({ id: "endpoint-a", kind: "endpoint" }, cards).map(card => card.id), ["card-c"]);
  assert.deepEqual(analysisCardsForNode({ id: "parameter-a", kind: "parameter" }, cards), []);
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
