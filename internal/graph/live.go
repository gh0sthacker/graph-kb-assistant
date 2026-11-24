package graph

import (
	"errors"

	"bb-graph-assistant/internal/caido"
)

// BuildLiveBundle creates a deterministic, redacted BehaviorGraph v2 bundle
// for one live exchange. It performs no I/O and returns the graph observation
// ID that identifies the generated observation.
func BuildLiveBundle(exchange caido.HttpExchange, program string, source Record) (Record, string, error) {
	bundle, observationID, err := BuildHistoricalBundle(exchange, program, source)
	if err != nil {
		return nil, "", errors.New("live graph bundle could not be built")
	}
	return finishLiveBundle(bundle, observationID)
}

// BuildLiveBundleWithSignatures adds keyed request and response variants to
// the redacted live observation. Raw message bytes remain in Caido.
func BuildLiveBundleWithSignatures(exchange caido.HttpExchange, program string, source Record, requestSignature string, responseSignature *string) (Record, string, error) {
	return BuildLiveBundleWithSignaturesAndFieldValueKey(exchange, program, source, requestSignature, responseSignature, nil, "")
}

// BuildLiveBundleWithSignaturesAndFieldValueKey adds safely parsed project-keyed
// field equality references while retaining the live-capture graph contract.
func BuildLiveBundleWithSignaturesAndFieldValueKey(exchange caido.HttpExchange, program string, source Record, requestSignature string, responseSignature *string, fieldValueKey []byte, revisionID string) (Record, string, error) {
	bundle, observationID, err := BuildHistoricalBundleWithSignaturesAndFieldValueKey(exchange, program, source, requestSignature, responseSignature, fieldValueKey, revisionID)
	if err != nil {
		return nil, "", errors.New("live graph bundle could not be built")
	}
	return finishLiveBundle(bundle, observationID)
}

func finishLiveBundle(bundle Record, observationID string) (Record, string, error) {
	entities, ok := bundle["entities"].([]Record)
	if !ok {
		return nil, "", errors.New("live graph bundle has an invalid entity shape")
	}
	observationFound := false
	for _, entity := range entities {
		if entity["kind"] != "observation" {
			continue
		}
		data, ok := entity["data"].(Record)
		if !ok {
			return nil, "", errors.New("live graph observation has an invalid data shape")
		}
		data["observation_kind"] = "live_capture"
		observationFound = true
		break
	}
	if !observationFound {
		return nil, "", errors.New("live graph observation is missing")
	}
	cards, ok := bundle["cards"].([]Record)
	if !ok || len(cards) != 1 {
		return nil, "", errors.New("live graph bundle has an invalid card shape")
	}
	card := cards[0]
	card["business_action"] = Record{
		"known_effect":      "unknown",
		"semantic_analysis": "not_implemented",
		"business_analysis": "not_implemented",
	}
	missingEvidence := []any{
		"Actor, session, tenant, role, and object context were not provided by the live exchange.",
		"Live request and response semantic analysis is not implemented by the Go mapper.",
		"Live business-action analysis is absent; the observed effect remains unknown.",
	}
	if previous, ok := card["missing_evidence"].([]any); ok && len(previous) > 2 {
		missingEvidence = append(missingEvidence, previous[2:]...)
	}
	card["missing_evidence"] = missingEvidence
	card["analysis_state"] = "incomplete"
	if requestAnalysis, ok := card["request_analysis"].(Record); ok {
		requestAnalysis["parse_state"] = "incomplete"
	} else {
		return nil, "", errors.New("live graph request analysis has an invalid shape")
	}
	if responseAnalysis, ok := card["response_analysis"].(Record); ok {
		responseAnalysis["parse_state"] = "incomplete"
	} else {
		return nil, "", errors.New("live graph response analysis has an invalid shape")
	}
	return bundle, observationID, nil
}
