package graph

import (
	"errors"
	"sort"

	"bb-graph-assistant/internal/caido"
)

// AddNavigationURLReferences joins captures through project-keyed URL identity.
// The shared node retains every matching capture, including duplicates and
// captures imported on other pages. It does not select an initiating request.
func AddNavigationURLReferences(bundle Record, exchange caido.HttpExchange, observationID string, indexKey []byte) error {
	program, ok := bundle["program"].(string)
	exchangeProgram, err := exchange.Ref.Project.StorageKey()
	if !ok || err != nil || program != exchangeProgram {
		return errors.New("navigation URL reference belongs to another project")
	}
	refs, ok := observationSourceRefs(bundle, observationID)
	if !ok {
		return errors.New("navigation URL reference observation is unavailable")
	}
	requestKey, refererKey, redirectKey, err := NavigationURLIndexKeys(exchange, indexKey)
	if err != nil {
		return err
	}
	entities, entitiesOK := bundle["entities"].([]Record)
	assertions, assertionsOK := bundle["assertions"].([]Record)
	if !entitiesOK || !assertionsOK {
		return errors.New("navigation URL reference bundle is malformed")
	}
	entityIDs := make(map[string]bool, len(entities))
	assertionIDs := make(map[string]bool, len(assertions))
	for _, entity := range entities {
		id, _ := entity["id"].(string)
		entityIDs[id] = true
	}
	for _, assertion := range assertions {
		id, _ := assertion["id"].(string)
		assertionIDs[id] = true
	}
	for _, target := range []struct {
		key, role, predicate string
		incoming             bool
	}{
		{requestKey, "request", "uses_request_url", false},
		{refererKey, "referrer", "referrer_url_matches", true},
		{redirectKey, "redirect", "redirect_url_matches", false},
	} {
		if target.key == "" {
			continue
		}
		data := Record{
			"program": program, "token_version": "project-url-hmac-v1",
			"normalization": "http_url_index_v1", "equality_signature": target.key,
		}
		id, err := historicalEntityID("url_reference", data)
		if err != nil {
			return errors.New("navigation URL reference identity could not be computed")
		}
		if !entityIDs[id] {
			entities = append(entities, Record{"id": id, "kind": "url_reference", "data": data, "source_refs": refs})
			entityIDs[id] = true
		}
		subject, object := observationID, id
		if target.incoming {
			subject, object = object, subject
		}
		context := Record{
			"program": program, "link_basis": "project_keyed_url_equality",
			"reference_role": target.role, "causality": "url_match_only",
		}
		assertion, err := historicalAssertion(program, subject, target.predicate, object, context, navigationSourceRefs(refs))
		if err != nil {
			return errors.New("navigation URL reference assertion could not be computed")
		}
		if !assertionIDs[assertion["id"].(string)] {
			assertions = append(assertions, assertion)
			assertionIDs[assertion["id"].(string)] = true
		}
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i]["id"].(string) < entities[j]["id"].(string) })
	sort.Slice(assertions, func(i, j int) bool { return assertions[i]["id"].(string) < assertions[j]["id"].(string) })
	bundle["entities"], bundle["assertions"] = entities, assertions
	return nil
}
