package neo4jstore

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	maxAnalyticalCandidateAssertions = 1_000
	maxAnalyticalCandidateCards      = 200
	maxAnalyticalFrontierNodes       = 128
	analyticalClosureHops            = 2
)

// AnalyticalSliceOptions bounds the local candidate graph loaded for one
// analysis slice. The shared graph Reader applies the requested node, edge,
// and depth budgets after the bounded candidates are validated.
type AnalyticalSliceOptions struct {
	MaxNodes int
	MaxEdges int
	MaxDepth int
}

// AnalyticalSlice is a selected-query projection, not a whole-project
// snapshot. Candidate counts are capped at the configured limit plus one so
// callers can distinguish a complete frontier from a truncated one.
type AnalyticalSlice struct {
	Snapshot   behaviorgraph.Record
	Generation int64
	// CandidateAssertions is the assertion count materialized into the bounded snapshot.
	CandidateAssertions          int
	CandidateAssertionsTruncated bool
	CandidateCards               int
	CandidateCardsTruncated      bool
	FrontierHops                 int
	FrontierNodeLimitReached     bool
	FrontierNodesOmitted         int
	ProvenanceTruncated          bool
	TruncatedEntityIDs           []string
	EntitySourceRefLimit         int
}

// ReadProjectAnalyticalSlice reads a bounded graph frontier, its assertion
// endpoints, source provenance, and cards that share evidence with the loaded
// records. It never loads the full project snapshot and does not establish
// global contradiction or project integrity.
func (s *Store) ReadProjectAnalyticalSlice(ctx context.Context, projectKey string, seedIDs []string, options AnalyticalSliceOptions) (AnalyticalSlice, error) {
	seeds, err := analyticalSliceSeeds(projectKey, seedIDs, options)
	if err != nil {
		return AnalyticalSlice{}, err
	}
	if s == nil || s.driver == nil {
		return AnalyticalSlice{}, errors.New("Neo4j store is unavailable")
	}
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	value, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return readProjectAnalyticalSliceTx(ctx, tx, projectKey, seeds, options)
	})
	if err != nil {
		return AnalyticalSlice{}, err
	}
	result, ok := value.(AnalyticalSlice)
	if !ok {
		return AnalyticalSlice{}, errors.New("Neo4j analytical slice result is invalid")
	}
	return result, nil
}

func analyticalSliceSeeds(projectKey string, ids []string, options AnalyticalSliceOptions) ([]string, error) {
	if !projectKeyPattern.MatchString(projectKey) || len(ids) < 1 || len(ids) > 80 ||
		options.MaxNodes < 1 || options.MaxNodes > 100 || options.MaxEdges < 0 || options.MaxEdges > 300 ||
		options.MaxDepth < 0 || options.MaxDepth > 6 || len(ids) > options.MaxNodes {
		return nil, errors.New("Neo4j analytical slice selection is invalid")
	}
	seen := make(map[string]bool, len(ids))
	seeds := make([]string, 0, len(ids))
	for _, id := range ids {
		if !neighborhoodIDPattern.MatchString(id) || strings.HasPrefix(id, "source-") || strings.HasPrefix(id, "card-") {
			return nil, errors.New("Neo4j analytical slice seed identity is invalid")
		}
		if !seen[id] {
			seen[id] = true
			seeds = append(seeds, id)
		}
	}
	sort.Strings(seeds)
	if len(seeds) != len(ids) {
		return nil, errors.New("Neo4j analytical slice seeds must be unique")
	}
	return seeds, nil
}

func readProjectAnalyticalSliceTx(ctx context.Context, tx neo4j.ManagedTransaction, project string, seeds []string, options AnalyticalSliceOptions) (AnalyticalSlice, error) {
	generation, err := neighborhoodGeneration(ctx, tx, project)
	if err != nil {
		return AnalyticalSlice{}, err
	}
	read := &neighborhoodRead{project: project, records: make(map[string]neighborhoodRecord)}
	if err := read.load(ctx, tx, seeds); err != nil {
		return AnalyticalSlice{}, err
	}

	seenAssertions := make(map[string]bool)
	for _, seed := range seeds {
		if strings.HasPrefix(seed, "assertion-") {
			seenAssertions[seed] = true
		}
	}
	frontier := append([]string(nil), seeds...)
	seenFrontier := make(map[string]bool, len(seeds))
	for _, id := range seeds {
		seenFrontier[id] = true
	}
	result := AnalyticalSlice{Generation: generation, FrontierHops: options.MaxDepth + analyticalClosureHops}
	for hop := 0; hop < result.FrontierHops; hop++ {
		if len(frontier) == 0 {
			break
		}
		remaining := maxAnalyticalCandidateAssertions - len(seenAssertions)
		candidates, truncated, err := readAnalyticalAssertionCandidates(ctx, tx, project, frontier, seenAssertions, remaining)
		if err != nil {
			return AnalyticalSlice{}, err
		}
		for _, id := range candidates {
			seenAssertions[id] = true
		}
		if truncated {
			result.CandidateAssertionsTruncated = true
		}
		if err := read.load(ctx, tx, candidates); err != nil {
			return AnalyticalSlice{}, err
		}
		if err := read.closeAssertionDependencies(ctx, tx, maxAnalyticalCandidateAssertions); err != nil {
			return AnalyticalSlice{}, err
		}
		if truncated {
			break
		}
		if hop+1 >= result.FrontierHops {
			break
		}

		frontier = frontier[:0]
		for id, record := range read.records {
			if record.kind != "entity" && record.kind != "assertion" {
				continue
			}
			if !seenFrontier[id] {
				seenFrontier[id] = true
				frontier = append(frontier, id)
			}
		}
		sort.Strings(frontier)
		frontierPage, omitted := analyticalFrontierPage(frontier, maxAnalyticalFrontierNodes)
		frontier = frontierPage
		if omitted > 0 {
			result.FrontierNodeLimitReached = true
			result.FrontierNodesOmitted += omitted
		}
	}
	for _, record := range read.records {
		if record.kind == "assertion" {
			result.CandidateAssertions++
		}
	}

	ownerIDs := make([]string, 0, len(read.records))
	assertionIDs := make([]string, 0, len(seenAssertions))
	for id, record := range read.records {
		if record.kind == "entity" || record.kind == "assertion" {
			ownerIDs = append(ownerIDs, id)
		}
		if record.kind == "assertion" {
			assertionIDs = append(assertionIDs, id)
		}
	}
	sort.Strings(ownerIDs)
	sort.Strings(assertionIDs)
	cardIDs, cardsTruncated, err := readAnalyticalCardCandidates(ctx, tx, project, read, ownerIDs, maxAnalyticalCandidateCards)
	if err != nil {
		return AnalyticalSlice{}, err
	}
	result.CandidateCards, result.CandidateCardsTruncated = len(cardIDs), cardsTruncated
	if err := read.load(ctx, tx, cardIDs); err != nil {
		return AnalyticalSlice{}, err
	}
	if err := read.closeCardEndpointDependencies(ctx, tx, maxAnalyticalCandidateCards); err != nil {
		return AnalyticalSlice{}, err
	}
	if err := read.checkTopology(ctx, tx, assertionIDs); err != nil {
		return AnalyticalSlice{}, err
	}
	allOwners := make([]string, 0, len(read.records))
	for id, record := range read.records {
		if record.kind == "entity" || record.kind == "assertion" || record.kind == "card" {
			allOwners = append(allOwners, id)
		}
	}
	sort.Strings(allOwners)
	if err := read.loadProvenance(ctx, tx, allOwners); err != nil {
		return AnalyticalSlice{}, err
	}
	result.TruncatedEntityIDs = read.truncatedEntityIDs()
	result.ProvenanceTruncated = len(result.TruncatedEntityIDs) != 0
	result.EntitySourceRefLimit = maxNeighborhoodEntityRefs

	entities := make([]behaviorgraph.Record, 0)
	assertions := make([]behaviorgraph.Record, 0)
	cards := make([]behaviorgraph.Record, 0)
	sources := make([]behaviorgraph.Record, 0)
	for _, record := range read.records {
		switch record.kind {
		case "entity":
			entities = append(entities, record.value)
		case "assertion":
			assertions = append(assertions, record.value)
		case "card":
			cards = append(cards, record.value)
		case "source":
			sources = append(sources, record.value)
		}
	}
	for _, values := range [][]behaviorgraph.Record{entities, assertions, cards, sources} {
		sort.Slice(values, func(i, j int) bool { return recordSortKey(values[i]) < recordSortKey(values[j]) })
	}
	result.Snapshot = behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": project,
		"sources": sources, "entities": entities, "assertions": assertions, "cards": cards,
	}
	if behaviorgraph.ValidatePublicData(result.Snapshot) != nil {
		return AnalyticalSlice{}, errors.New("Neo4j analytical slice contains unsafe metadata")
	}
	finalGeneration, err := neighborhoodGeneration(ctx, tx, project)
	if err != nil || finalGeneration != generation {
		return AnalyticalSlice{}, errors.New("Neo4j project changed while the analytical slice was read")
	}
	return result, nil
}

func readAnalyticalAssertionCandidates(ctx context.Context, tx neo4j.ManagedTransaction, project string, frontier []string, seen map[string]bool, remaining int) ([]string, bool, error) {
	if remaining < 0 {
		return nil, false, errors.New("Neo4j analytical assertion budget is invalid")
	}
	if len(frontier) == 0 {
		return []string{}, false, nil
	}
	perNodePageSize, err := analyticalAssertionShare(remaining, len(frontier))
	if err != nil {
		return nil, false, err
	}
	// An assertion may touch the same entity in both endpoint roles. Probe up
	// to two rows per unique candidate so those duplicate roles do not consume
	// the per-node candidate page.
	perNodeProbeLimit := max(1, 2*perNodePageSize+1)
	rowLimit := len(frontier) * perNodeProbeLimit
	result, err := tx.Run(ctx, `
		UNWIND $record_ids AS record_id
		MATCH (node:GraphRecord {project_key: $project_key, record_id: record_id})
		WHERE node.record_type IN ['entity', 'assertion']
		CALL {
			WITH node
			MATCH (node)<-[:SUBJECT|OBJECT {project_key: $project_key}]-(incident:GraphRecord {project_key: $project_key, record_type: 'assertion'})
			WITH incident.record_id AS assertion_id
			WHERE NOT assertion_id IN $seen_ids
			RETURN assertion_id ORDER BY assertion_id LIMIT $per_node_limit
		}
		RETURN node.record_id AS frontier_id, assertion_id
		ORDER BY frontier_id, assertion_id LIMIT $limit`, map[string]any{
		"project_key": project, "record_ids": frontier, "seen_ids": mapKeys(seen),
		"per_node_limit": perNodeProbeLimit, "limit": rowLimit + 1,
	})
	if err != nil {
		return nil, false, errors.New("Neo4j analytical assertion frontier is unavailable")
	}
	frontierSet := make(map[string]bool, len(frontier))
	for _, id := range frontier {
		frontierSet[id] = true
	}
	byFrontier := make(map[string][]string, len(frontier))
	rows := 0
	for result.Next(ctx) {
		frontierValue, _ := result.Record().Get("frontier_id")
		assertionValue, _ := result.Record().Get("assertion_id")
		frontierID, frontierOK := frontierValue.(string)
		assertionID, assertionOK := assertionValue.(string)
		rows++
		if !frontierOK || !frontierSet[frontierID] || !assertionOK || !neighborhoodIDPattern.MatchString(assertionID) ||
			!strings.HasPrefix(assertionID, "assertion-") || rows > rowLimit {
			return nil, false, errors.New("Neo4j analytical assertion candidate is malformed")
		}
		byFrontier[frontierID] = append(byFrontier[frontierID], assertionID)
	}
	if result.Err() != nil {
		return nil, false, errors.New("Neo4j analytical assertion frontier could not be read")
	}
	truncated := false
	ids := make([]string, 0, min(rowLimit, remaining+1))
	for _, frontierID := range frontier {
		candidates := byFrontier[frontierID]
		localUnique := make([]string, 0, len(candidates))
		for _, id := range candidates {
			if len(localUnique) == 0 || localUnique[len(localUnique)-1] != id {
				localUnique = append(localUnique, id)
			}
		}
		candidates = localUnique
		if len(candidates) > perNodePageSize {
			truncated = true
			candidates = candidates[:perNodePageSize]
		}
		ids = append(ids, candidates...)
	}
	sort.Strings(ids)
	unique := ids[:0]
	for _, id := range ids {
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	page, pageTruncated, err := analyticalCandidatePage(unique, remaining)
	return page, truncated || pageTruncated, err
}

// analyticalAssertionShare spreads the remaining global assertion budget
// across the current frontier. This avoids silently imposing a second,
// arbitrary per-node ceiling when the overall candidate budget has room.
func analyticalAssertionShare(remaining, frontierCount int) (int, error) {
	if remaining < 0 || frontierCount < 0 {
		return 0, errors.New("Neo4j analytical assertion budget is invalid")
	}
	if remaining == 0 || frontierCount == 0 {
		return 0, nil
	}
	share := remaining / frontierCount
	if remaining%frontierCount != 0 {
		share++
	}
	return min(share, remaining), nil
}

func analyticalFrontierPage(ids []string, limit int) ([]string, int) {
	if limit < 0 {
		return []string{}, len(ids)
	}
	if len(ids) <= limit {
		return ids, 0
	}
	return ids[:limit], len(ids) - limit
}

func analyticalCandidatePage(ids []string, remaining int) ([]string, bool, error) {
	if remaining < 0 {
		return nil, false, errors.New("Neo4j analytical assertion budget is invalid")
	}
	if len(ids) > remaining {
		return ids[:remaining], true, nil
	}
	return ids, false, nil
}

func (r *neighborhoodRead) closeCardEndpointDependencies(ctx context.Context, tx neo4j.ManagedTransaction, budget int) error {
	missing := make(map[string]bool)
	for _, record := range r.records {
		if record.kind != "card" {
			continue
		}
		endpoint, ok := record.value["endpoint"].(map[string]any)
		if !ok {
			return errors.New("Neo4j analytical card endpoint is malformed")
		}
		value, present := endpoint["id"]
		if !present || value == nil {
			continue
		}
		id, ok := value.(string)
		if !ok || !neighborhoodIDPattern.MatchString(id) || !strings.HasPrefix(id, "endpoint-") {
			return errors.New("Neo4j analytical card endpoint identity is malformed")
		}
		if existing, exists := r.records[id]; exists {
			if existing.kind != "entity" || existing.value["kind"] != "endpoint" {
				return errors.New("Neo4j analytical card endpoint is inconsistent")
			}
			continue
		}
		missing[id] = true
	}
	if len(missing) > budget {
		return errors.New("Neo4j analytical card endpoint dependencies exceed the candidate budget")
	}
	ids := make([]string, 0, len(missing))
	for id := range missing {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if err := r.load(ctx, tx, ids); err != nil {
		return err
	}
	for _, id := range ids {
		endpoint, exists := r.records[id]
		if !exists || endpoint.kind != "entity" || endpoint.value["kind"] != "endpoint" {
			return errors.New("Neo4j analytical card endpoint is inconsistent")
		}
	}
	return nil
}

func readAnalyticalCardCandidates(ctx context.Context, tx neo4j.ManagedTransaction, project string, read *neighborhoodRead, ownerIDs []string, limit int) ([]string, bool, error) {
	if len(ownerIDs) == 0 {
		return []string{}, false, nil
	}
	endpointIDs, captureIDs := make([]string, 0), make([]string, 0)
	for _, ownerID := range ownerIDs {
		owner, ok := read.records[ownerID]
		if !ok {
			return nil, false, errors.New("Neo4j analytical card owner is missing")
		}
		if owner.kind == "entity" && owner.value["kind"] == "endpoint" {
			endpointIDs = append(endpointIDs, ownerID)
		}
		if owner.kind == "entity" && owner.value["kind"] == "observation" {
			if data, ok := owner.value["data"].(map[string]any); ok {
				if captureID, ok := data["capture_id"].(string); ok && captureID != "" {
					captureIDs = append(captureIDs, captureID)
				}
			}
		}
	}
	sort.Strings(endpointIDs)
	sort.Strings(captureIDs)
	result, err := tx.Run(ctx, `
		MATCH (card:GraphRecord {project_key: $project_key, record_type: 'card'})
		WHERE card.card_endpoint_id IN $endpoint_ids OR card.card_capture_id IN $capture_ids OR EXISTS {
			MATCH (owner:GraphRecord {project_key: $project_key})-[:EVIDENCED_BY {project_key: $project_key}]->(source:GraphRecord {project_key: $project_key, record_type: 'source'})
			MATCH (card)-[:EVIDENCED_BY {project_key: $project_key}]->(source)
			WHERE owner.record_id IN $owner_ids
			RETURN source
		}
		RETURN DISTINCT card.record_id AS card_id ORDER BY card_id LIMIT $limit`, map[string]any{
		"project_key": project, "owner_ids": ownerIDs, "endpoint_ids": endpointIDs,
		"capture_ids": captureIDs, "limit": limit + 1,
	})
	if err != nil {
		return nil, false, errors.New("Neo4j analytical card frontier is unavailable")
	}
	ids := make([]string, 0, limit+1)
	for result.Next(ctx) {
		value, _ := result.Record().Get("card_id")
		id, ok := value.(string)
		if !ok || !neighborhoodIDPattern.MatchString(id) || !strings.HasPrefix(id, "card-") || len(ids) >= limit+1 {
			return nil, false, errors.New("Neo4j analytical card candidate is malformed")
		}
		ids = append(ids, id)
	}
	if result.Err() != nil {
		return nil, false, errors.New("Neo4j analytical card frontier could not be read")
	}
	if len(ids) > limit {
		return ids[:limit], true, nil
	}
	return ids, false, nil
}

func mapKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
