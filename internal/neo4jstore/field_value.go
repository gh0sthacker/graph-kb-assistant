package neo4jstore

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// ReadProjectFieldValueTraversalSeeds finds bounded cross-capture paths from
// one observation through its query occurrences and project-keyed value
// equality links. It returns graph record IDs only; equality signatures and
// request values never cross this storage boundary.
func (s *Store) ReadProjectFieldValueTraversalSeeds(
	ctx context.Context,
	projectKey, observationID string,
	maxPaths int,
) ([]string, bool, int64, error) {
	if s == nil || s.driver == nil || !projectKeyPattern.MatchString(projectKey) ||
		!neighborhoodIDPattern.MatchString(observationID) || !strings.HasPrefix(observationID, "observation-") ||
		maxPaths < 1 || maxPaths > maxFieldValueTraversalPaths {
		return nil, false, 0, errors.New("Neo4j field-value traversal selection is invalid")
	}

	type traversalResult struct {
		ids        []string
		truncated  bool
		generation int64
	}
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	value, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		generation, err := fieldValueIndexGeneration(ctx, tx, projectKey)
		if err != nil {
			return nil, err
		}
		result, err := tx.Run(ctx, fieldValueTraversalSeedsQuery, map[string]any{
			"project_key": projectKey, "observation_id": observationID, "limit_plus_one": maxPaths + 1,
		})
		if err != nil {
			return nil, errors.New("Neo4j field-value traversal query failed")
		}
		type path struct{ sourceOccurrence, valueReference, peerOccurrence, peerObservation string }
		paths := make([]path, 0, maxPaths+1)
		for result.Next(ctx) {
			row := result.Record()
			values := make([]string, 4)
			for index, key := range []string{"source_occurrence_id", "value_reference_id", "peer_occurrence_id", "peer_observation_id"} {
				value, _ := row.Get(key)
				values[index], _ = value.(string)
			}
			if !validGraphRecordID(values[0], "field_occurrence") || !validGraphRecordID(values[1], "value_reference") ||
				!validGraphRecordID(values[2], "field_occurrence") || !validGraphRecordID(values[3], "observation") {
				return nil, errors.New("Neo4j field-value traversal returned an invalid graph identity")
			}
			paths = append(paths, path{values[0], values[1], values[2], values[3]})
			if len(paths) > maxPaths {
				break
			}
		}
		if err := result.Err(); err != nil {
			return nil, errors.New("Neo4j field-value traversal could not finish")
		}
		if _, err := result.Consume(ctx); err != nil {
			return nil, errors.New("Neo4j field-value traversal could not finish")
		}
		truncated := len(paths) > maxPaths
		if truncated {
			paths = paths[:maxPaths]
		}
		ids := map[string]struct{}{observationID: {}}
		for _, path := range paths {
			for _, id := range []string{path.sourceOccurrence, path.valueReference, path.peerOccurrence, path.peerObservation} {
				ids[id] = struct{}{}
			}
		}
		ordered := make([]string, 0, len(ids))
		for id := range ids {
			ordered = append(ordered, id)
		}
		sort.Strings(ordered)
		for index, id := range ordered {
			if id == observationID {
				ordered[0], ordered[index] = ordered[index], ordered[0]
				break
			}
		}
		finalGeneration, err := neighborhoodGeneration(ctx, tx, projectKey)
		if err != nil || finalGeneration != generation {
			return nil, ErrNeighborhoodCursorStale
		}
		return traversalResult{ids: ordered, truncated: truncated, generation: generation}, nil
	})
	if err != nil {
		return nil, false, 0, err
	}
	result, ok := value.(traversalResult)
	if !ok {
		return nil, false, 0, errors.New("Neo4j field-value traversal result is invalid")
	}
	return result.ids, result.truncated, result.generation, nil
}

func fieldValueIndexGeneration(ctx context.Context, tx neo4j.ManagedTransaction, projectKey string) (int64, error) {
	result, err := tx.Run(ctx, `
		MATCH (project:GraphProject {project_key: $project_key})
		RETURN project.schema_version AS schema_version,
		       project.generation AS generation,
		       project.field_value_index_version AS field_value_index_version,
		       project.field_value_index_generation AS field_value_index_generation`,
		map[string]any{"project_key": projectKey},
	)
	if err != nil || !result.Next(ctx) {
		return 0, ErrFieldValueIndexUnavailable
	}
	row := result.Record()
	schemaValue, _ := row.Get("schema_version")
	generationValue, _ := row.Get("generation")
	versionValue, _ := row.Get("field_value_index_version")
	indexedGenerationValue, _ := row.Get("field_value_index_generation")
	schema, schemaOK := schemaValue.(int64)
	generation, generationOK := generationValue.(int64)
	current := signatureIndexMarkerCurrent(versionValue, generationValue, indexedGenerationValue)
	if _, err := result.Consume(ctx); err != nil || !schemaOK || schema != 2 || !generationOK || !current {
		return 0, ErrFieldValueIndexUnavailable
	}
	return generation, nil
}

func validGraphRecordID(value, kind string) bool {
	return neighborhoodIDPattern.MatchString(value) && strings.HasPrefix(value, kind+"-")
}

const fieldValueTraversalSeedsQuery = `
	MATCH (seed:GraphRecord {project_key: $project_key, record_id: $observation_id, record_type: 'entity', kind: 'observation'})
	MATCH (seedEdge:GraphRecord {project_key: $project_key, record_type: 'assertion', predicate: 'has_occurrence'})
	MATCH (seedEdge)-[:SUBJECT {project_key: $project_key}]->(seed)
	MATCH (seedEdge)-[:OBJECT {project_key: $project_key}]->(sourceOccurrence:GraphRecord {project_key: $project_key, record_type: 'entity', kind: 'field_occurrence'})
	MATCH (sourceValueEdge:GraphRecord {project_key: $project_key, record_type: 'assertion', predicate: 'value_used_in'})
	MATCH (sourceValueEdge)-[:OBJECT {project_key: $project_key}]->(sourceOccurrence)
	MATCH (sourceValueEdge)-[:SUBJECT {project_key: $project_key}]->(sourceValue:GraphRecord {project_key: $project_key, record_type: 'entity', kind: 'value_reference'})
	WITH DISTINCT seed, sourceOccurrence, sourceValue.field_equality_signature AS equality_signature
	WHERE equality_signature IS NOT NULL
	MATCH (valueReference:GraphRecord {project_key: $project_key, record_type: 'entity', kind: 'value_reference', field_equality_signature: equality_signature})
	MATCH (peerValueEdge:GraphRecord {project_key: $project_key, record_type: 'assertion', predicate: 'value_used_in'})
	MATCH (peerValueEdge)-[:SUBJECT {project_key: $project_key}]->(valueReference)
	MATCH (peerValueEdge)-[:OBJECT {project_key: $project_key}]->(peerOccurrence:GraphRecord {project_key: $project_key, record_type: 'entity', kind: 'field_occurrence'})
	WHERE peerOccurrence.record_id <> sourceOccurrence.record_id
	MATCH (peerCaptureEdge:GraphRecord {project_key: $project_key, record_type: 'assertion', predicate: 'has_occurrence'})
	MATCH (peerCaptureEdge)-[:OBJECT {project_key: $project_key}]->(peerOccurrence)
	MATCH (peerCaptureEdge)-[:SUBJECT {project_key: $project_key}]->(peerObservation:GraphRecord {project_key: $project_key, record_type: 'entity', kind: 'observation'})
	WHERE peerObservation.record_id <> seed.record_id
	RETURN DISTINCT sourceOccurrence.record_id AS source_occurrence_id,
	       valueReference.record_id AS value_reference_id,
	       peerOccurrence.record_id AS peer_occurrence_id,
	       peerObservation.record_id AS peer_observation_id
	ORDER BY source_occurrence_id, value_reference_id, peer_occurrence_id, peer_observation_id
	LIMIT $limit_plus_one`
