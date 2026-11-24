package graph

import (
	"errors"
	"sort"
	"strings"
)

// MaxFieldValueTraversalPaths bounds how many matching captures are added to
// one focused observation graph. Each path contributes at most four IDs.
const MaxFieldValueTraversalPaths = 4

// FieldValueTraversalSeeds returns a deterministic, project-local seed set for
// the path observation -> field occurrence -> keyed value match -> matching
// field occurrence -> observation. The returned IDs contain no query values
// or equality signatures.
func (r *Reader) FieldValueTraversalSeeds(observationID string, maxPaths int) ([]string, bool, error) {
	if r == nil || r.db == nil || maxPaths < 1 || maxPaths > MaxFieldValueTraversalPaths ||
		!sliceIDPattern.MatchString(observationID) || !strings.HasPrefix(observationID, "observation-") {
		return nil, false, errors.New("field value traversal selection is invalid")
	}
	seed, exists := r.entities[observationID]
	if !exists || seed["kind"] != "observation" {
		return nil, false, errors.New("field value traversal observation was not found in the selected project")
	}

	occurrencesByObservation := make(map[string][]string)
	observationsByOccurrence := make(map[string][]string)
	occurrencesByValue := make(map[string][]string)
	valuesByOccurrence := make(map[string][]string)
	for _, assertion := range r.assertions {
		subject := assertion["subject"].(string)
		object := assertion["object"].(string)
		switch assertion["predicate"] {
		case "has_occurrence":
			if r.entities[subject]["kind"] == "observation" && r.entities[object]["kind"] == "field_occurrence" {
				occurrencesByObservation[subject] = append(occurrencesByObservation[subject], object)
				observationsByOccurrence[object] = append(observationsByOccurrence[object], subject)
			}
		case "value_used_in":
			if r.entities[subject]["kind"] == "value_reference" && r.entities[object]["kind"] == "field_occurrence" {
				occurrencesByValue[subject] = append(occurrencesByValue[subject], object)
				valuesByOccurrence[object] = append(valuesByOccurrence[object], subject)
			}
		}
	}

	type matchPath struct {
		occurrenceA string
		value       string
		occurrenceB string
		observation string
	}
	pathsByKey := make(map[string]matchPath)
	for _, occurrenceA := range occurrencesByObservation[observationID] {
		for _, valueID := range valuesByOccurrence[occurrenceA] {
			for _, occurrenceB := range occurrencesByValue[valueID] {
				if occurrenceB == occurrenceA {
					continue
				}
				for _, observationB := range observationsByOccurrence[occurrenceB] {
					if observationB == observationID {
						continue
					}
					path := matchPath{occurrenceA, valueID, occurrenceB, observationB}
					key := strings.Join([]string{path.occurrenceA, path.value, path.occurrenceB, path.observation}, "\x00")
					pathsByKey[key] = path
				}
			}
		}
	}
	pathKeys := make([]string, 0, len(pathsByKey))
	for key := range pathsByKey {
		pathKeys = append(pathKeys, key)
	}
	sort.Strings(pathKeys)
	truncated := len(pathKeys) > maxPaths
	if truncated {
		pathKeys = pathKeys[:maxPaths]
	}

	selected := map[string]struct{}{observationID: {}}
	for _, key := range pathKeys {
		path := pathsByKey[key]
		selected[path.occurrenceA] = struct{}{}
		selected[path.value] = struct{}{}
		selected[path.occurrenceB] = struct{}{}
		selected[path.observation] = struct{}{}
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Keep the original request first so the focused graph retains its anchor.
	for i, id := range ids {
		if id == observationID {
			ids[0], ids[i] = ids[i], ids[0]
			break
		}
	}
	return ids, truncated, nil
}
