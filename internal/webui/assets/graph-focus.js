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

  const api = { focusedNeighborhood, focusedTransform };
  if (typeof module === "object" && module.exports) module.exports = api;
  root.BBGraphFocus = api;
})(globalThis);
