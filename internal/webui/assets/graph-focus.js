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

  function layeredGridLayout(entities, assertions, rootId, titleForNode = node => node.id) {
    if (!Array.isArray(entities) || entities.length === 0) {
      return { positions: new Map(), width: 960, height: 560 };
    }
    const byId = new Map(entities.map(node => [node.id, node]));
    const neighbors = new Map(entities.map(node => [node.id, []]));
    for (const edge of assertions) {
      if (!byId.has(edge.subject) || !byId.has(edge.object)) continue;
      neighbors.get(edge.subject).push(edge.object);
      neighbors.get(edge.object).push(edge.subject);
    }
    const root = byId.has(rootId) ? rootId : entities[0].id;
    const distances = new Map([[root, 0]]);
    const queue = [root];
    for (let index = 0; index < queue.length; index++) {
      for (const neighbor of neighbors.get(queue[index]) || []) {
        if (distances.has(neighbor)) continue;
        distances.set(neighbor, distances.get(queue[index]) + 1);
        queue.push(neighbor);
      }
    }
    const byDepth = new Map();
    for (const node of entities) {
      const depth = distances.get(node.id) ?? 0;
      if (!byDepth.has(depth)) byDepth.set(depth, []);
      byDepth.get(depth).push(node);
    }
    for (const group of byDepth.values()) group.sort((a, b) => String(titleForNode(a)).localeCompare(String(titleForNode(b))));

    const layers = [...byDepth].sort(([a], [b]) => a - b).map(([depth, nodes]) => {
      const columns = Math.ceil(nodes.length / 10);
      const rows = Math.ceil(nodes.length / columns);
      return { depth, nodes, columns, rows };
    });
    const maxRows = Math.max(...layers.map(layer => layer.rows));
    const height = Math.max(560, 140 + maxRows * 128);
    const positions = new Map();
    let layerX = 120;
    for (const layer of layers) {
      const startY = (height - layer.rows * 128) / 2 + 64;
      layer.nodes.forEach((node, index) => {
        const column = Math.floor(index / layer.rows);
        const row = index % layer.rows;
        positions.set(node.id, { x: layerX + column * 225, y: startY + row * 128 });
      });
      layerX += layer.columns * 225;
    }
    return { positions, width: Math.max(960, layerX + 120), height };
  }

  function observationNotes(data) {
    const record = data && typeof data === "object" ? data : {};
    const notes = [];
    const parseState = record.parse_state;
    if (parseState === "incomplete" || parseState === "partial") {
      notes.push({ title: "Parsing is incomplete", body: "The graph parser could not map all request and response structure, so some graph fields may be missing. This does not by itself mean the saved HTTP bytes are truncated. Open the stored HTTP record to inspect the message metadata." });
    } else if (parseState === "partial_prefix") {
      notes.push({ title: "Only a prefix was parsed", body: "The stored data contains only a prefix, so later request or response fields may be missing from this graph." });
    } else if (parseState === "unknown_extent" || parseState === "unknown") {
      notes.push({ title: "Parse completeness is unknown", body: "The available graph metadata does not include parser completeness, so some graph fields may be missing. Open the stored HTTP record to inspect the saved-message metadata." });
    } else if (!parseState) {
      notes.push({ title: "Parse completeness is not recorded", body: "This graph node has no parse-state field. Its analysis state is separate from parser completeness; open the stored HTTP record to inspect the saved-message metadata." });
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

  function routeFamilyUncertainty(routeFamily) {
    if (typeof routeFamily !== "string") return null;
    const maskedSegments = routeFamily.split("/").filter(segment =>
      segment === "{value}" || segment === "{parameter}"
    ).length;
    const message = maskedSegments > 0
      ? `${maskedSegments} masked path segment${maskedSegments === 1 ? " is" : "s are"} unresolved. Each could be a variable value or an unrecognized static route word; this grouping does not confirm parameter behavior.`
      : "No masked path segments are present. Grouping still uses a static-word vocabulary, so route families are heuristic.";
    return { maskedSegments, message };
  }

  function operationAliasFor(node, aliases) {
    if (!node || node.kind !== "operation" || !aliases || typeof aliases.get !== "function") return "";
    const item = aliases.get(node.id);
    const alias = typeof item === "string" ? item : item?.alias;
    return typeof alias === "string" && alias.trim() ? alias.trim() : "";
  }

  function withObservationParseState(node, provenance) {
    if (!node || node.kind !== "observation") return node;
    const data = node.data && typeof node.data === "object" ? node.data : {};
    if (data.parse_state && data.parse_state !== "unknown") return node;
    const source = Array.isArray(provenance)
      ? provenance.find(item => item?.graph_observation_id === node.id &&
        typeof item.parse_state === "string" && item.parse_state !== "unknown")
      : null;
    return { ...node, data: { ...data, parse_state: source?.parse_state || "unknown" } };
  }

  const api = { focusedNeighborhood, focusedTransform, layeredGridLayout, observationNotes, operationAliasFor, routeFamilyUncertainty, withObservationParseState };
  if (typeof module === "object" && module.exports) module.exports = api;
  root.BBGraphFocus = api;
})(globalThis);
