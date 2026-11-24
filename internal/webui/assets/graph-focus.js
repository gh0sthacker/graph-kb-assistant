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

  function observationRequestOrder(node, entities, assertions) {
    const unknown = { status: "unknown", initiatingRequestIDs: [], relations: [] };
    if (!node || node.kind !== "observation" || typeof node.id !== "string" || !node.id) return null;
    const entityByID = new Map((Array.isArray(entities) ? entities : [])
      .filter(entity => typeof entity?.id === "string" && entity.id && typeof entity.kind === "string")
      .map(entity => [entity.id, entity]));
    if (entityByID.get(node.id)?.kind !== "observation") return unknown;

    const relations = [];
    const seenAssertions = new Set();
    for (const assertion of Array.isArray(assertions) ? assertions : []) {
      if (assertion?.predicate !== "precedes" || typeof assertion.id !== "string" ||
          !/^assertion-[a-f0-9]{64}$/.test(assertion.id) || seenAssertions.has(assertion.id)) continue;
      const subjectID = assertion.subject;
      const objectID = assertion.object;
      if (typeof subjectID !== "string" || typeof objectID !== "string" || subjectID === objectID ||
          (subjectID !== node.id && objectID !== node.id) ||
          entityByID.get(subjectID)?.kind !== "observation" || entityByID.get(objectID)?.kind !== "observation") continue;
      const sourceIDs = [];
      const seenSources = new Set();
      if (Array.isArray(assertion.source_refs)) {
        for (const source of assertion.source_refs) {
          if (typeof source?.path !== "string" || !source.path.trim() ||
              typeof source.sha256 !== "string" || !/^[a-f0-9]{64}$/.test(source.sha256) ||
              typeof source.record_locator !== "string" || !source.record_locator.trim()) continue;
          const key = `${source.sha256}\u0000${source.record_locator}`;
          if (seenSources.has(key)) continue;
          seenSources.add(key);
          sourceIDs.push({ sha256: source.sha256, recordLocator: source.record_locator });
        }
      }
      if (!sourceIDs.length || !["observed", "corroborated"].includes(assertion.evidence_state) ||
          !["parsed", "operator_asserted", "check_result"].includes(assertion.origin_kind)) continue;
      seenAssertions.add(assertion.id);
      relations.push({
        assertionID: assertion.id, subjectID, objectID,
        direction: subjectID === node.id ? "outgoing" : "incoming",
        originKind: assertion.origin_kind, evidenceState: assertion.evidence_state, sourceIDs,
      });
    }
    relations.sort((left, right) => left.assertionID.localeCompare(right.assertionID));
    const initiatingRequestIDs = [...new Set(relations
      .filter(relation => relation.direction === "incoming")
      .map(relation => relation.subjectID))].sort();
    return relations.length ? { status: "supported", initiatingRequestIDs, relations } : unknown;
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

  function contextAnnotationEvidenceReferences(annotation, evidenceReferences, evidencePacket) {
    if (!annotation || !Array.isArray(annotation.evidence_reference_ids) || !Array.isArray(evidenceReferences)) return [];
    const selectedIDs = [...new Set(annotation.evidence_reference_ids.filter(id => typeof id === "string" && id.length > 0))];
    const selected = new Set(selectedIDs);
    const packetRows = Array.isArray(evidencePacket?.evidence) ? evidencePacket.evidence : [];
    let references = evidenceReferences
      .filter(reference => selected.has(reference?.evidence_reference_id) &&
        Number.isInteger(reference?.evidence_index) && reference.evidence_index >= 0 &&
        (reference.side === "request" || reference.side === "response") &&
        reference.request_id !== undefined && reference.request_id !== null && String(reference.request_id).length > 0)
      .sort((left, right) => left.evidence_index - right.evidence_index);
    if (annotation.kind === "flow") {
      const byID = new Map(references.map(reference => [reference.evidence_reference_id, reference]));
      references = selectedIDs.map(id => byID.get(id)).filter(Boolean);
    }
    return references
      .map(reference => {
        const packetEvidence = packetRows[reference.evidence_index];
        const exactPacketMatch = packetEvidence &&
          packetEvidence.request_id === reference.request_id &&
          packetEvidence.revision_id === reference.revision_id &&
          packetEvidence.side === reference.side &&
          packetEvidence.span_start === reference.span_start &&
          packetEvidence.span_end === reference.span_end;
        const available = exactPacketMatch && packetEvidence.available === true;
        const rawExcerpt = available && typeof packetEvidence.excerpt === "string" ? packetEvidence.excerpt : "";
        let excerpt = "";
        let excerptBytes = 0;
        const encoder = new TextEncoder();
        for (const character of rawExcerpt) {
          const characterBytes = encoder.encode(character).length;
          if (excerptBytes + characterBytes > 4096) break;
          excerpt += character;
          excerptBytes += characterBytes;
        }
        const result = {
          evidenceIndex: reference.evidence_index,
          side: reference.side,
          requestId: String(reference.request_id),
          spanStart: Number.isSafeInteger(reference.span_start) && reference.span_start >= 0 ? reference.span_start : null,
          spanEnd: Number.isSafeInteger(reference.span_end) && reference.span_end >= reference.span_start ? reference.span_end : null,
          available: Boolean(available),
          excerpt,
          excerptTruncated: excerpt.length < rawExcerpt.length,
        };
        if (annotation.kind === "flow") result.flowStep = selectedIDs.indexOf(reference.evidence_reference_id) + 1;
        return result;
      });
  }

  function contextAnnotationsForObservation(node, provenance, session, projectKey) {
    if (!node || node.kind !== "observation" || typeof node.id !== "string" || !node.id ||
        typeof projectKey !== "string" || !projectKey || !session ||
        session.projectKey !== projectKey || session.accepted !== true || session.reviewLoaded !== true ||
        typeof session.id !== "string" || !session.id || !Number.isInteger(session.version) || session.version < 1 ||
        !Array.isArray(provenance) || !Array.isArray(session.evidenceReferences) ||
        !Array.isArray(session.contextAnnotations)) return [];

    const captureRevisions = new Set(provenance
      .filter(item => item?.graph_observation_id === node.id &&
        item.request_id !== undefined && item.request_id !== null && String(item.request_id).length > 0 &&
        typeof item.revision_id === "string" && item.revision_id.length > 0)
      .map(item => `${String(item.request_id)}\u0000${item.revision_id}`));
    if (!captureRevisions.size) return [];

    const matchedReferences = session.evidenceReferences
      .filter(reference => typeof reference?.evidence_reference_id === "string" && reference.evidence_reference_id.length > 0 &&
        Number.isInteger(reference.evidence_index) && reference.evidence_index >= 0 &&
        (reference.side === "request" || reference.side === "response") &&
        reference.request_id !== undefined && reference.request_id !== null && String(reference.request_id).length > 0 &&
        typeof reference.revision_id === "string" && reference.revision_id.length > 0 &&
        captureRevisions.has(`${String(reference.request_id)}\u0000${reference.revision_id}`))
      .sort((left, right) => left.evidence_index - right.evidence_index ||
        left.evidence_reference_id.localeCompare(right.evidence_reference_id));
    if (!matchedReferences.length) return [];

    const result = [];
    const seen = new Set();
    for (const annotation of session.contextAnnotations) {
      if (typeof annotation?.context_annotation_id !== "string" || !annotation.context_annotation_id ||
          typeof annotation.kind !== "string" || !annotation.kind.trim() ||
          typeof annotation.label !== "string" || !annotation.label.trim() ||
          annotation.origin !== "analyst" || annotation.evidence_state !== "analyst_provided" ||
          !Array.isArray(annotation.evidence_reference_ids)) continue;
      const selectedIDs = [...new Set(annotation.evidence_reference_ids.filter(id => typeof id === "string" && id.length > 0))];
      const selectedSet = new Set(selectedIDs);
      let evidenceReferences = matchedReferences
        .filter(reference => selectedSet.has(reference.evidence_reference_id))
        .map(reference => ({
          referenceID: reference.evidence_reference_id,
          evidenceIndex: reference.evidence_index,
          side: reference.side,
          requestId: String(reference.request_id),
          revisionId: reference.revision_id,
        }));
      if (annotation.kind === "flow") {
        const byID = new Map(evidenceReferences.map(reference => [reference.referenceID, reference]));
        evidenceReferences = selectedIDs.map(id => byID.get(id)).filter(Boolean)
          .map((reference, index) => ({ ...reference, flowStep: index + 1 }));
      }
      if (!evidenceReferences.length) continue;
      const key = `${annotation.context_annotation_id}\u0000${evidenceReferences.map(reference => reference.referenceID).join("\u0000")}`;
      if (seen.has(key)) continue;
      seen.add(key);
      result.push({
        annotationID: annotation.context_annotation_id,
        annotationVersion: Number.isInteger(annotation.session_version) && annotation.session_version > 0
          ? annotation.session_version : null,
        kind: annotation.kind,
        label: annotation.label,
        evidenceReferences,
        sessionID: session.id,
        sessionVersion: session.version,
      });
    }
    return result.sort((left, right) => {
      const leftIndex = left.evidenceReferences[0]?.evidenceIndex ?? Number.MAX_SAFE_INTEGER;
      const rightIndex = right.evidenceReferences[0]?.evidenceIndex ?? Number.MAX_SAFE_INTEGER;
      return leftIndex - rightIndex || left.annotationID.localeCompare(right.annotationID);
    });
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

  function analysisCardsForNode(node, cards) {
    if (!node || typeof node !== "object" || !Array.isArray(cards)) return [];
    const data = node.data && typeof node.data === "object" ? node.data : {};
    const cardID = typeof data.card_id === "string" ? data.card_id : "";
    const captureID = typeof data.capture_id === "string" ? data.capture_id : "";
    const seen = new Set();
    return cards
      .filter(card => card && typeof card.id === "string" && (
        (cardID && card.id === cardID) ||
        (captureID && card.capture_id === captureID) ||
        (node.kind === "endpoint" && card.endpoint?.id === node.id)
      ))
      .sort((left, right) => left.id.localeCompare(right.id))
      .filter(card => {
        if (seen.has(card.id)) return false;
        seen.add(card.id);
        return true;
      });
  }

  const api = { focusedNeighborhood, focusedTransform, layeredGridLayout, observationNotes, observationRequestOrder, operationAliasFor, analysisCardsForNode, contextAnnotationEvidenceReferences, contextAnnotationsForObservation, routeFamilyUncertainty, withObservationParseState };
  if (typeof module === "object" && module.exports) module.exports = api;
  root.BBGraphFocus = api;
})(globalThis);
