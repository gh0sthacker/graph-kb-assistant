package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

const (
	maxSliceOutputBytes     = 8 << 20
	maxSliceOmissions       = 1_000
	maxSliceOmittedCards    = 1_000
	maxSliceMissingEvidence = 1_000
)

var sliceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*-[a-f0-9]{64}$`)

var contextKeys = []string{"program", "origin", "actor", "session_version", "tenant", "role", "object_state", "scenario", "scenario_step"}

var allowedPredicates = map[string]struct{}{
	"exposes": {}, "has_parameter": {}, "invokes": {}, "references": {}, "returns": {},
	"owned_by": {}, "visible_to": {}, "value_used_in": {}, "requires": {}, "produces": {},
	"supports": {}, "contradicts": {}, "contains": {}, "precedes": {}, "uses_request_variant": {},
	"received_response": {}, "referrer_context_for": {}, "redirects_to": {},
}

var allowedOrigins = map[string]struct{}{"parsed": {}, "operator_asserted": {}, "model_proposed": {}, "check_result": {}}

var allowedEvidence = map[string]struct{}{"proposed": {}, "observed": {}, "corroborated": {}, "contested": {}}

// SliceOptions bounds a graph traversal. The CLI applies the narrower MVP
// limits; the reusable package enforces them as well for non-CLI callers.
type SliceOptions struct {
	SeedIDs  []string
	MaxNodes int
	MaxEdges int
	MaxDepth int
	Context  map[string]any
}

// Slice returns one deterministic, bounded graph neighborhood. It never
// changes graph or capture data and never promotes a lead to a finding.
func (r *Reader) Slice(ctx context.Context, options SliceOptions) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("BehaviorGraph reader is unavailable")
	}
	if len(options.SeedIDs) == 0 || options.MaxNodes < 1 || options.MaxNodes > 100 ||
		options.MaxEdges < 0 || options.MaxEdges > 300 || options.MaxDepth < 0 || options.MaxDepth > 6 {
		return nil, errors.New("invalid graph slice budget or missing seeds")
	}
	seeds := append([]string(nil), options.SeedIDs...)
	seenSeeds := make(map[string]struct{}, len(seeds))
	for _, seed := range seeds {
		if !sliceIDPattern.MatchString(seed) {
			return nil, errors.New("graph seed ID is malformed")
		}
		if _, exists := seenSeeds[seed]; exists {
			return nil, errors.New("graph seeds must be unique")
		}
		seenSeeds[seed] = struct{}{}
	}
	sort.Strings(seeds)
	if len(seeds) > options.MaxNodes || len(seeds) > 80 {
		return nil, errors.New("graph seed count exceeds the node budget")
	}
	effectiveContext := cloneMap(options.Context)
	if effectiveContext == nil {
		effectiveContext = make(map[string]any)
	}
	if err := validateContext(effectiveContext, r.program); err != nil {
		return nil, errors.New("graph context is malformed")
	}
	if err := validatePublic(effectiveContext); err != nil {
		return nil, errors.New("graph context contains unsafe metadata")
	}

	nodes := make(map[string]Record, len(r.entities)+len(r.assertions))
	for id, entity := range r.entities {
		nodes[id] = entity
	}
	for id, assertion := range r.assertions {
		copyAssertion := cloneRecord(assertion)
		copyAssertion["kind"] = "assertion"
		nodes[id] = copyAssertion
	}
	for _, seed := range seeds {
		candidate, ok := nodes[seed]
		if !ok {
			return nil, errors.New("graph seed was not found in the selected project")
		}
		candidateContext, err := recordContext(candidate, r.program)
		if err != nil || !contextsCompatible(candidateContext, effectiveContext) {
			return nil, errors.New("graph seeds have incompatible contexts")
		}
		bindContext(candidateContext, effectiveContext)
	}

	adjacency := make(map[string][]string)
	for id, assertion := range r.assertions {
		for _, endpoint := range []string{assertion["subject"].(string), assertion["object"].(string)} {
			adjacency[endpoint] = append(adjacency[endpoint], id)
		}
	}
	for node := range adjacency {
		sort.Strings(adjacency[node])
	}

	selectedNodes := make(map[string]struct{}, len(seeds))
	selectedEdges := make(map[string]struct{})
	depth := make(map[string]int)
	frontier := append([]string(nil), seeds...)
	for _, seed := range seeds {
		selectedNodes[seed] = struct{}{}
		depth[seed] = 0
	}
	omissions := make(map[string]Record)
	allOmittedIDs := make(map[string]struct{})
	omissionOverflow := 0
	mandatoryOmissionOverflow := false
	addOmission := func(id string, omission Record) {
		allOmittedIDs[id] = struct{}{}
		if _, exists := omissions[id]; exists {
			omissions[id] = omission
			return
		}
		if len(omissions) < maxSliceOmissions {
			omissions[id] = omission
		} else {
			omissionOverflow++
			if omission["mandatory_contradiction"] == true {
				mandatoryOmissionOverflow = true
			}
		}
	}
	for index := 0; index < len(frontier); index++ {
		current := frontier[index]
		incidentSet := make(map[string]struct{})
		for _, edgeID := range adjacency[current] {
			incidentSet[edgeID] = struct{}{}
		}
		if _, currentIsAssertion := r.assertions[current]; currentIsAssertion {
			incidentSet[current] = struct{}{}
		}
		incident := make([]string, 0, len(incidentSet))
		for edgeID := range incidentSet {
			incident = append(incident, edgeID)
		}
		sort.Slice(incident, func(i, j int) bool {
			return assertionPriority(r.assertions[incident[i]], incident[i]) < assertionPriority(r.assertions[incident[j]], incident[j])
		})
		for _, edgeID := range incident {
			if _, done := selectedEdges[edgeID]; done {
				continue
			}
			edge := r.assertions[edgeID]
			trial := cloneMap(effectiveContext)
			compatible := true
			for _, candidateID := range []string{edgeID, edge["subject"].(string), edge["object"].(string)} {
				candidate, err := recordContext(nodes[candidateID], r.program)
				if err != nil || !contextsCompatible(candidate, trial) {
					compatible = false
					break
				}
				bindContext(candidate, trial)
			}
			newNodes := make([]string, 0, 2)
			for _, endpoint := range []string{edge["subject"].(string), edge["object"].(string)} {
				if _, exists := selectedNodes[endpoint]; !exists {
					newNodes = append(newNodes, endpoint)
				}
			}
			sort.Strings(newNodes)
			reason := ""
			if !compatible {
				reason = "context_incompatible"
			} else if depth[current] >= options.MaxDepth {
				reason = "max_depth"
			} else if len(selectedEdges) >= options.MaxEdges {
				reason = "max_edges"
			} else if len(selectedNodes)+len(newNodes) > options.MaxNodes {
				reason = "max_nodes"
			}
			mandatory := edge["predicate"] == "contradicts" || edge["evidence_state"] == "contested"
			if reason != "" {
				addOmission(edgeID, Record{"assertion_id": edgeID, "reason": reason, "mandatory_contradiction": mandatory})
				continue
			}
			selectedEdges[edgeID] = struct{}{}
			effectiveContext = trial
			delete(omissions, edgeID)
			for _, endpoint := range newNodes {
				selectedNodes[endpoint] = struct{}{}
				depth[endpoint] = depth[current] + 1
				frontier = append(frontier, endpoint)
			}
			if _, exists := depth[edgeID]; !exists {
				depth[edgeID] = depth[current]
				frontier = append(frontier, edgeID)
			}
		}
	}

	evidenceFrontier := make(map[string]struct{}, len(selectedNodes)+len(selectedEdges)+len(omissions))
	for id := range selectedNodes {
		evidenceFrontier[id] = struct{}{}
	}
	for id := range selectedEdges {
		evidenceFrontier[id] = struct{}{}
	}
	for id := range omissions {
		evidenceFrontier[id] = struct{}{}
	}
	for id := range allOmittedIDs {
		evidenceFrontier[id] = struct{}{}
	}
	assertionIDs := make([]string, 0, len(r.assertions))
	for id := range r.assertions {
		assertionIDs = append(assertionIDs, id)
	}
	sort.Strings(assertionIDs)
	for {
		added := false
		for _, id := range assertionIDs {
			edge := r.assertions[id]
			if _, selected := selectedEdges[id]; selected {
				continue
			}
			if _, omitted := omissions[id]; omitted {
				continue
			}
			mandatory := edge["predicate"] == "contradicts" || edge["evidence_state"] == "contested"
			if !mandatory {
				continue
			}
			_, subjectSelected := evidenceFrontier[edge["subject"].(string)]
			_, objectSelected := evidenceFrontier[edge["object"].(string)]
			if !subjectSelected && !objectSelected {
				continue
			}
			compatible := true
			trial := cloneMap(effectiveContext)
			for _, candidateID := range []string{id, edge["subject"].(string), edge["object"].(string)} {
				candidate, err := recordContext(nodes[candidateID], r.program)
				if err != nil || !contextsCompatible(candidate, trial) {
					compatible = false
					break
				}
				bindContext(candidate, trial)
			}
			reason := "mandatory_evidence_branch_omitted"
			if !compatible {
				reason = "context_incompatible"
			}
			addOmission(id, Record{"assertion_id": id, "reason": reason, "mandatory_contradiction": true})
			evidenceFrontier[id] = struct{}{}
			added = true
		}
		if !added {
			break
		}
	}

	selectedAssertionNodes := make(map[string]struct{})
	for id := range selectedNodes {
		if _, ok := r.assertions[id]; ok {
			selectedAssertionNodes[id] = struct{}{}
		}
	}
	chosenEntities := make([]Record, 0)
	for id := range selectedNodes {
		if record, ok := r.entities[id]; ok {
			chosenEntities = append(chosenEntities, record)
		}
	}
	sortRecords(chosenEntities)
	chosenEdges := make([]Record, 0, len(selectedEdges))
	for id := range selectedEdges {
		chosenEdges = append(chosenEdges, r.assertions[id])
	}
	sortRecords(chosenEdges)
	assertionNodes := make([]Record, 0, len(selectedAssertionNodes))
	for id := range selectedAssertionNodes {
		assertionNodes = append(assertionNodes, r.assertions[id])
	}
	sortRecords(assertionNodes)

	observationSources := make(map[string]struct{})
	for _, entity := range chosenEntities {
		if entity["kind"] != "observation" {
			continue
		}
		captureID := nestedString(entity, "data", "capture_id")
		for _, ref := range recordRefs(entity) {
			observationSources[captureID+"\x00"+sourcePair(ref)] = struct{}{}
		}
	}
	selectedCards := make([]Record, 0)
	omittedCards := make([]Record, 0)
	omittedCardOverflow := 0
	for _, card := range r.cards {
		endpointID := nestedString(card, "endpoint", "id")
		_, endpointSelected := selectedNodes[endpointID]
		associated := endpointSelected
		if !associated {
			captureID, _ := card["capture_id"].(string)
			for _, ref := range recordRefs(card) {
				if _, found := observationSources[captureID+"\x00"+sourcePair(ref)]; found {
					associated = true
					break
				}
			}
		}
		if !associated {
			continue
		}
		cardContext, _ := card["context"].(map[string]any)
		if !contextsCompatible(cardContext, effectiveContext) {
			if len(omittedCards) < maxSliceOmittedCards {
				omittedCards = append(omittedCards, Record{"card_id": card["id"], "reason": "context_incompatible"})
			} else {
				omittedCardOverflow++
			}
		} else if len(selectedCards) >= options.MaxNodes {
			if len(omittedCards) < maxSliceOmittedCards {
				omittedCards = append(omittedCards, Record{"card_id": card["id"], "reason": "max_cards"})
			} else {
				omittedCardOverflow++
			}
		} else {
			selectedCards = append(selectedCards, card)
			bindContext(cardContext, effectiveContext)
		}
	}
	sortRecords(selectedCards)
	sort.Slice(omittedCards, func(i, j int) bool {
		return fmt.Sprint(omittedCards[i]["card_id"]) < fmt.Sprint(omittedCards[j]["card_id"])
	})

	allSelected := make([]Record, 0, len(chosenEntities)+len(chosenEdges)+len(assertionNodes)+len(selectedCards))
	allSelected = append(allSelected, chosenEntities...)
	allSelected = append(allSelected, chosenEdges...)
	allSelected = append(allSelected, assertionNodes...)
	allSelected = append(allSelected, selectedCards...)
	refs := uniqueRecordRefs(allSelected)
	refsTruncated := len(refs) > maxSliceSourceRefs
	if refsTruncated {
		refs = refs[:maxSliceSourceRefs]
	}
	sourceIntegrity, sourceStates, sourceBudgetExceeded := r.sourceIntegrity(refs)
	sources := make([]Record, 0, len(refs))
	freshSources := make(map[string]string)
	for _, ref := range refs {
		state := sourceStates[sourcePair(ref)]
		freshness := "unknown"
		if state == "current" {
			freshness = "current"
		} else if state == "stale" || state == "missing" {
			freshness = "stale"
		}
		copyRef := cloneRecord(ref)
		copyRef["integrity"] = state
		copyRef["freshness"] = freshness
		sources = append(sources, copyRef)
		freshSources[sourcePair(ref)] = state
	}
	entityFreshness := make(map[string]string, len(chosenEntities))
	for _, record := range chosenEntities {
		entityFreshness[record["id"].(string)] = sourceFreshness(recordRefs(record), freshSources)
	}
	assertionFreshness := make(map[string]string, len(chosenEdges)+len(assertionNodes))
	for _, record := range chosenEdges {
		assertionFreshness[record["id"].(string)] = sourceFreshness(recordRefs(record), freshSources)
	}
	for _, record := range assertionNodes {
		assertionFreshness[record["id"].(string)] = sourceFreshness(recordRefs(record), freshSources)
	}
	cardFreshness := make(map[string]string, len(selectedCards))
	for _, record := range selectedCards {
		cardFreshness[record["id"].(string)] = sourceFreshness(recordRefs(record), freshSources)
	}

	missingBranches := make([]Record, 0, len(omissions))
	for _, record := range omissions {
		missingBranches = append(missingBranches, record)
	}
	sort.Slice(missingBranches, func(i, j int) bool {
		return fmt.Sprint(missingBranches[i]["assertion_id"]) < fmt.Sprint(missingBranches[j]["assertion_id"])
	})
	unknownConditions := make([]string, 0)
	for _, entity := range chosenEntities {
		if entity["kind"] != "condition" {
			continue
		}
		data, _ := entity["data"].(map[string]any)
		if value, present := data["value"]; !present || isUnknown(value) {
			unknownConditions = append(unknownConditions, entity["id"].(string))
		}
	}
	sort.Strings(unknownConditions)
	missingEvidenceSet := make(map[string]struct{})
	missingEvidenceOverflow := 0
	incompleteCard := false
	for _, card := range selectedCards {
		if card["analysis_state"] == "incomplete" {
			incompleteCard = true
		}
		if items, ok := card["missing_evidence"].([]any); ok {
			for _, item := range items {
				if value, ok := item.(string); ok {
					if _, exists := missingEvidenceSet[value]; exists {
						continue
					}
					if len(missingEvidenceSet) < maxSliceMissingEvidence {
						missingEvidenceSet[value] = struct{}{}
					} else {
						missingEvidenceOverflow++
					}
				}
			}
		}
	}
	missingEvidence := make([]string, 0, len(missingEvidenceSet))
	for item := range missingEvidenceSet {
		missingEvidence = append(missingEvidence, item)
	}
	sort.Strings(missingEvidence)
	truncated := refsTruncated
	for _, omission := range missingBranches {
		reason := omission["reason"]
		if reason == "max_nodes" || reason == "max_edges" || reason == "max_depth" || reason == "mandatory_evidence_branch_omitted" {
			truncated = true
		}
	}
	for _, card := range omittedCards {
		if card["reason"] == "max_cards" {
			truncated = true
		}
	}
	truncated = truncated || omissionOverflow > 0 || omittedCardOverflow > 0 || missingEvidenceOverflow > 0 || sourceBudgetExceeded
	incomplete := len(missingBranches) > 0 || len(omittedCards) > 0 || len(missingEvidence) > 0 || len(unknownConditions) > 0 || incompleteCard || refsTruncated || omissionOverflow > 0 || omittedCardOverflow > 0 || missingEvidenceOverflow > 0 || sourceBudgetExceeded
	for _, state := range sourceStates {
		if state != "current" {
			incomplete = true
			break
		}
	}

	supporting := make([]string, 0)
	conflicting := make([]string, 0)
	for _, edge := range chosenEdges {
		id := edge["id"].(string)
		if edge["predicate"] == "supports" {
			supporting = append(supporting, id)
		}
		if edge["predicate"] == "contradicts" || edge["evidence_state"] == "contested" {
			conflicting = append(conflicting, id)
		}
	}
	result := map[string]any{
		"schema_version": 1, "program": r.program, "seed_ids": seeds, "context": effectiveContext,
		"entities": chosenEntities, "assertions": chosenEdges, "cards": selectedCards,
		"assertion_nodes": assertionNodes, "supporting_evidence": supporting,
		"conflicting_evidence": conflicting, "sources": sources,
		"source_integrity": sourceIntegrity,
		"freshness":        map[string]any{"entities": entityFreshness, "assertions": assertionFreshness, "cards": cardFreshness},
		"counts":           map[string]int{"nodes": len(selectedNodes), "entities": len(chosenEntities), "edges": len(chosenEdges), "cards": len(selectedCards)},
		"budgets":          map[string]int{"max_nodes": options.MaxNodes, "max_edges": options.MaxEdges, "max_depth": options.MaxDepth, "max_cards": options.MaxNodes},
		"truncated":        truncated, "incomplete": incomplete,
		"mandatory_contradiction_omitted": hasMandatoryOmission(missingBranches) || mandatoryOmissionOverflow,
		"missing_branches":                missingBranches, "omitted_cards": omittedCards,
		"missing_evidence": missingEvidence, "unknown_conditions": unknownConditions,
		"missing_branches_truncated":       omissionOverflow > 0,
		"omitted_cards_truncated":          omittedCardOverflow > 0,
		"missing_evidence_truncated":       missingEvidenceOverflow > 0,
		"source_integrity_budget_exceeded": sourceBudgetExceeded,
		"limitations": []string{
			"Source freshness describes local receipt-file integrity, not current server behavior.",
			"Record locators anchor provenance; semantic applicability still needs review.",
			"Graph evidence states do not verify findings or authorize actions.",
			"Slice coverage is limited to the imported inventory; reachability is not data flow.",
			"Unknown context remains unknown; excluded contexts require separate slices.",
		},
	}
	encoded, err := canonicalJSON(result)
	if err != nil {
		return nil, errors.New("graph slice could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	result["slice_id"] = "slice-" + hex.EncodeToString(digest[:])
	encoded, err = canonicalJSON(result)
	if err != nil || len(encoded) > maxSliceOutputBytes {
		return nil, errors.New("graph slice output exceeds its safe size limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.New("graph slice was canceled")
	}
	return result, nil
}

func assertionPriority(record Record, id string) string {
	if record["predicate"] == "contradicts" || record["evidence_state"] == "contested" {
		return "0:" + id
	}
	if record["predicate"] == "supports" {
		return "1:" + id
	}
	return "2:" + id
}

func recordContext(record Record, program string) (map[string]any, error) {
	if record == nil {
		return nil, errors.New("graph record is missing")
	}
	if record["kind"] == "assertion" {
		context, ok := record["context"].(map[string]any)
		if !ok {
			return nil, errors.New("assertion context is invalid")
		}
		context = cloneMap(context)
		return context, validateContext(context, program)
	}
	data, _ := record["data"].(map[string]any)
	context := make(map[string]any)
	if nested, ok := data["context"].(map[string]any); ok {
		context = cloneMap(nested)
	}
	for _, key := range contextKeys {
		if value, ok := data[key]; ok {
			if _, alreadySet := context[key]; !alreadySet {
				context[key] = value
			}
		}
	}
	return context, validateContext(context, program)
}

func contextsCompatible(candidate, selected map[string]any) bool {
	for _, key := range contextKeys {
		left, leftOK := candidate[key]
		right, rightOK := selected[key]
		if !leftOK || !rightOK || isUnknown(left) || isUnknown(right) {
			continue
		}
		if fmt.Sprintf("%T", left) != fmt.Sprintf("%T", right) || left != right {
			return false
		}
	}
	return true
}

func bindContext(candidate, selected map[string]any) {
	for _, key := range contextKeys {
		if selected[key] == nil || selected[key] == "" || selected[key] == "unknown" {
			if value, ok := candidate[key]; ok && !isUnknown(value) {
				selected[key] = value
			}
		}
	}
}

func isUnknown(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && (text == "" || text == "unknown")
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneRecord(value Record) Record {
	result := make(Record, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func nestedString(record map[string]any, first, second string) string {
	nested, ok := record[first].(map[string]any)
	if !ok {
		return ""
	}
	value, _ := nested[second].(string)
	return value
}

func recordRefs(record Record) []Record {
	refs, _ := record["source_refs"].([]Record)
	return refs
}

func uniqueRecordRefs(records []Record) []Record {
	seen := make(map[string]Record)
	for _, record := range records {
		for _, ref := range recordRefs(record) {
			encoded, _ := canonicalJSON(ref)
			seen[string(encoded)] = ref
		}
	}
	result := make([]Record, 0, len(seen))
	for _, ref := range seen {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool {
		left := sourcePair(result[i]) + "\x00" + fmt.Sprint(result[i]["record_locator"])
		right := sourcePair(result[j]) + "\x00" + fmt.Sprint(result[j]["record_locator"])
		return left < right
	})
	return result
}

func sortRecords(records []Record) {
	sort.Slice(records, func(i, j int) bool { return fmt.Sprint(records[i]["id"]) < fmt.Sprint(records[j]["id"]) })
}

func hasMandatoryOmission(records []Record) bool {
	for _, record := range records {
		if record["mandatory_contradiction"] == true {
			return true
		}
	}
	return false
}
