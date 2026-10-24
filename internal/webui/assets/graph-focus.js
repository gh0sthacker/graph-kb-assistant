(function (root) {
  function focusedNeighborhood(entities, assertions, focusNodeId) {
    const byId = new Map(entities.map(node => [node.id, node]));
    if (!byId.has(focusNodeId)) return { entities: [], assertions: [] };

    const included = new Set([focusNodeId]);
    const directAssertions = [];
    for (const assertion of assertions) {
      if (assertion.subject !== focusNodeId && assertion.object !== focusNodeId) continue;
      const neighborId = assertion.subject === focusNodeId ? assertion.object : assertion.subject;
      if (!byId.has(neighborId)) continue;
      included.add(neighborId);
      directAssertions.push(assertion);
    }

    return {
      entities: entities.filter(node => included.has(node.id)),
      assertions: directAssertions,
    };
  }

  function focusedTransform(position, width, height, zoom) {
    if (!position || !Number.isFinite(position.x) || !Number.isFinite(position.y) ||
        !Number.isFinite(width) || !Number.isFinite(height) || !Number.isFinite(zoom) ||
        width <= 0 || height <= 0 || zoom <= 0) return null;
    return {
      panX: zoom * (width / 2 - position.x),
      panY: zoom * (height / 2 - position.y),
    };
  }

  function observationNotes(data) {
    const record = data && typeof data === "object" ? data : {};
    const notes = [];
    const parseState = record.parse_state || record.analysis_state;
    if (parseState === "incomplete") {
      notes.push({ title: "Parsing is incomplete", body: "The parser could not verify that the request and response were fully captured. Some graph fields may be missing. HTTP status is separate; open the stored HTTP record when available to inspect the exchange." });
    } else if (parseState === "partial_prefix") {
      notes.push({ title: "Only a prefix was parsed", body: "The stored data contains only a prefix, so later request or response fields may be missing from this graph." });
    } else if (parseState === "unknown_extent" || parseState === "unknown") {
      notes.push({ title: "Parse completeness is unknown", body: "The available metadata does not establish whether the full request and response were captured. Some graph fields may be missing." });
    }
    const scenario = record.context && typeof record.context === "object" ? record.context.scenario : record.context;
    if (scenario === undefined || scenario === null || scenario === "" || scenario === "unknown") {
      notes.push({ title: "Context is not recorded", body: "No analyst scenario is attached to this capture. This does not mean the HTTP exchange has no context." });
    }
    if (record.observed_at === undefined || record.observed_at === null || record.observed_at === "" || record.observed_at === "unknown") {
      notes.push({ title: "Time is not recorded", body: "The local observation index does not include a timestamp for this capture." });
    }
    return notes;
  }

  const api = { focusedNeighborhood, focusedTransform, observationNotes };
  if (typeof module === "object" && module.exports) module.exports = api;
  root.BBGraphFocus = api;
})(globalThis);
