package graph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"bb-graph-assistant/internal/caido"
)

var (
	historicalRiskNamePattern = regexp.MustCompile(`(?i)(bearer\s|eyJ[A-Za-z0-9_-]{8,}|password\s*[=:]|secret\s*[=:]|token\s*[=:]|api[_-]?key\s*[=:]|@|-----BEGIN|[a-f0-9]{24,}|[A-Za-z0-9_-]{32,})`)
	historicalSingleLetter    = regexp.MustCompile(`^[A-Za-z]$`)
	historicalRouteWords      = map[string]struct{}{
		"account": {}, "accounts": {}, "admin": {}, "api": {}, "approve": {}, "auth": {},
		"authorize": {}, "callback": {}, "cart": {}, "checkout": {}, "config": {},
		"dashboard": {}, "delete": {}, "doc": {}, "docs": {}, "document": {},
		"documents": {}, "download": {}, "edit": {}, "export": {}, "file": {},
		"files": {}, "graphql": {}, "health": {}, "home": {}, "index": {}, "invite": {},
		"item": {}, "items": {}, "list": {}, "login": {}, "logout": {}, "me": {},
		"object": {}, "objects": {}, "order": {}, "orders": {}, "password": {},
		"payment": {}, "payments": {}, "private": {}, "profile": {}, "public": {},
		"product": {}, "products": {}, "read": {}, "register": {}, "report": {}, "reports": {}, "reset": {},
		"rest": {}, "search": {}, "service": {}, "services": {}, "session": {},
		"sessions": {}, "settings": {}, "share": {}, "shared": {}, "status": {},
		"submit": {}, "tenant": {}, "tenants": {}, "token": {}, "update": {},
		"upload": {}, "user": {}, "users": {}, "v1": {}, "v2": {}, "v3": {}, "verify": {},
	}
)

// BuildHistoricalBundle creates a deterministic, redacted BehaviorGraph v2
// bundle for one already-validated historical exchange. It performs no I/O.
// The returned observation ID is the graph identity to persist beside the
// immutable capture revision.
func BuildHistoricalBundle(exchange caido.HttpExchange, program string, source Record) (Record, string, error) {
	return buildHistoricalBundle(exchange, program, source, "", nil, nil, "")
}

// BuildHistoricalBundleWithSignatures adds project-keyed message variants to
// the historical observation. Signatures are HMACs computed from the selected
// project's vault key; raw message bytes are never copied into the graph.
func BuildHistoricalBundleWithSignatures(exchange caido.HttpExchange, program string, source Record, requestSignature string, responseSignature *string) (Record, string, error) {
	return BuildHistoricalBundleWithSignaturesAndFieldValueKey(exchange, program, source, requestSignature, responseSignature, nil, "")
}

// BuildHistoricalBundleWithSignaturesAndFieldValueKey adds project-keyed
// equality references for safely parsed field occurrences. The field-value
// key must be separately derived from the project's index key.
func BuildHistoricalBundleWithSignaturesAndFieldValueKey(exchange caido.HttpExchange, program string, source Record, requestSignature string, responseSignature *string, fieldValueKey []byte, revisionID string) (Record, string, error) {
	if !graphDigestPattern.MatchString(requestSignature) || exchange.HasResponse != (responseSignature != nil) ||
		responseSignature != nil && !graphDigestPattern.MatchString(*responseSignature) ||
		len(fieldValueKey) != 0 && len(fieldValueKey) != 32 ||
		(len(fieldValueKey) == 32 || revisionID != "") && !graphDigestPattern.MatchString(revisionID) {
		return nil, "", errors.New("historical graph message signature is invalid")
	}
	return buildHistoricalBundle(exchange, program, source, requestSignature, responseSignature, fieldValueKey, revisionID)
}

func buildHistoricalBundle(exchange caido.HttpExchange, program string, source Record, requestSignature string, responseSignature *string, fieldValueKey []byte, revisionID string) (Record, string, error) {
	if !graphDigestPattern.MatchString(program) || validateSource(source) != nil {
		return nil, "", errors.New("historical graph project or source reference is invalid")
	}
	projectKey, err := exchange.Ref.Project.StorageKey()
	if err != nil || projectKey != program {
		return nil, "", errors.New("historical graph exchange belongs to another project")
	}
	if err := exchange.Validate(); err != nil {
		return nil, "", errors.New("historical graph exchange is invalid")
	}
	projection, err := ProjectExchange(exchange)
	if err != nil || projection.ObservationID == "" {
		return nil, "", errors.New("historical graph projection is invalid")
	}

	ref := Record{
		"path": source["path"], "sha256": source["sha256"],
		"record_locator": source["record_locator"],
	}
	refs := []Record{ref}
	_, requestTarget := requestLine(exchange.RequestRaw)
	target := firstNonEmpty(exchange.Target, requestTarget)
	origin, path := "unknown", "unknown"
	queryNames := []string{}
	var queryFields []historicalParameterField
	queryFieldCount := 0
	queryOccurrenceSpansAvailable := false
	pathFieldCount := 0
	queryParseIncomplete, pathParseIncomplete := true, true
	var pathField historicalParameterField
	hasPathField := false
	if target != "" && historicalTargetValid(target) {
		origin = historicalOrigin(target, ref)
		var graphPathIncomplete bool
		path, graphPathIncomplete = historicalGraphPath(target, ref)
		queryTarget, queryOffset, querySpanAvailable := historicalQueryTarget(exchange.RequestRaw, target)
		querySpanAvailable = querySpanAvailable && graphDigestPattern.MatchString(revisionID)
		queryOccurrenceSpansAvailable = querySpanAvailable
		queryNames, queryFields, queryParseIncomplete, queryFieldCount = historicalQueryFields(
			queryTarget, ref, fieldValueKey, queryOffset, querySpanAvailable, exchange.RequestWireFidelity,
		)
		if graphPathIncomplete {
			queryNames, queryFields, queryParseIncomplete = []string{}, nil, true
		}
		var pathFieldIncomplete bool
		pathField, hasPathField, pathFieldIncomplete, pathFieldCount = historicalPathField(target)
		if queryFieldCount >= 2_000 && hasPathField {
			hasPathField = false
			pathFieldIncomplete = true
		}
		if queryFieldCount+pathFieldCount > 2_000 {
			pathFieldIncomplete = true
		}
		pathParseIncomplete = graphPathIncomplete || pathFieldIncomplete
	}
	requestFieldCount := queryFieldCount + pathFieldCount
	requestFieldsRemaining := historicalMaxParameterFields - requestFieldCount
	if requestFieldsRemaining < 0 {
		requestFieldsRemaining = 0
	}
	requestBodyAnalysis := historicalMessageBodyFields(
		exchange.RequestRaw, "request.body", ref, requestFieldsRemaining, exchange.RequestTruncated,
		fieldValueKey, graphDigestPattern.MatchString(revisionID), exchange.RequestWireFidelity,
	)
	requestBodyFields, requestBodyComplete := requestBodyAnalysis.fields, requestBodyAnalysis.complete
	var responseBodyFields []historicalParameterField
	responseBodyComplete := false
	var responseBodyAnalysis historicalBodyAnalysis
	if exchange.HasResponse {
		responseBodyAnalysis = historicalMessageBodyFields(
			exchange.ResponseRaw, "response.body", ref, historicalMaxParameterFields, exchange.ResponseTruncated,
			fieldValueKey, graphDigestPattern.MatchString(revisionID), exchange.ResponseWireFidelity,
		)
		responseBodyFields, responseBodyComplete = responseBodyAnalysis.fields, responseBodyAnalysis.complete
	}
	method := projection.Search.Method
	endpointData := Record{
		"program": program, "origin": origin, "path": path,
		"query_names": stringsAsAny(queryNames), "method": method,
	}
	endpointIdentity := Record{
		"program": program, "origin": origin, "path": path,
		"query_names": stringsAsAny(queryNames), "method": method,
	}
	endpointID, err := historicalEntityID("endpoint", endpointIdentity)
	if err != nil {
		return nil, "", errors.New("historical graph endpoint identity could not be computed")
	}
	var operation Record
	var belongsToOperation Record
	if family, ok := RouteFamilyV1(target); ok && origin != "unknown" && method != "UNKNOWN" {
		operationData := Record{
			"program": program, "origin": origin, "method": method,
			"route_family_version": RouteFamilyV1Version, "route_family": family,
		}
		operationID, operationErr := historicalEntityID("operation", operationData)
		if operationErr != nil {
			return nil, "", errors.New("historical graph operation identity could not be computed")
		}
		operation = Record{"id": operationID, "kind": "operation", "data": operationData, "source_refs": refs}
		belongsToOperation, operationErr = historicalAssertion(program, endpointID, "belongs_to_operation", operationID, historicalUnknownContext(program, origin, exchange.CapturedAt), refs)
		if operationErr != nil {
			return nil, "", errors.New("historical graph operation provenance could not be computed")
		}
	}
	assetID, err := historicalEntityID("asset", Record{"program": program, "origin": origin})
	if err != nil {
		return nil, "", errors.New("historical graph asset identity could not be computed")
	}

	captureID := projection.ObservationID
	cardID, err := historicalEntityID("card", Record{
		"program": program, "source": ref, "capture_id": captureID,
	})
	if err != nil {
		return nil, "", errors.New("historical graph card identity could not be computed")
	}
	context := historicalUnknownContext(program, origin, exchange.CapturedAt)
	observationIdentity := Record{
		"program": program, "card_id": cardID, "source_refs": refs,
	}
	observationID, err := historicalEntityID("observation", observationIdentity)
	if err != nil {
		return nil, "", errors.New("historical graph observation identity could not be computed")
	}

	cardEndpoint := cloneRecord(endpointData)
	cardEndpoint["id"] = endpointID
	cardMissing := []any{
		"Actor, session, tenant, role, and object context were not provided by the historical exchange.",
		"Business-action semantics and body media other than JSON, form-urlencoded, multipart/form-data, HTML, and XHTML are not implemented by the Go mapper.",
	}
	if exchange.ResponseStatusMetadataMismatch {
		cardMissing = append(cardMissing, "Caido history status metadata differed from the fetched response line; the detailed response status was retained.")
	}
	if exchange.MalformedHeaderNamePresent {
		cardMissing = append(cardMissing, "One or more printable Caido header names were preserved in reconstructed text but could not be mapped as HTTP header fields.")
	}
	if queryParseIncomplete {
		cardMissing = append(cardMissing, "Query parameter names or field nodes could not be mapped within the mapper limit.")
	}
	if queryFieldCount > 0 && !queryOccurrenceSpansAvailable {
		cardMissing = append(cardMissing, "Query field occurrences could not be bound to exact request-message byte spans and a capture revision; equality links were omitted.")
	}
	if pathParseIncomplete {
		cardMissing = append(cardMissing, "Request path parameters could not be mapped within the mapper limit.")
	}
	if !requestBodyComplete || !responseBodyComplete {
		cardMissing = append(cardMissing, "Request or response body fields could not be mapped completely because of missing data, unsupported media, truncation, or parser limits.")
	}
	if !requestBodyAnalysis.occurrencesComplete || exchange.HasResponse && !responseBodyAnalysis.occurrencesComplete {
		cardMissing = append(cardMissing, "Some request or response body field occurrences could not be bound to revision-bound source byte spans; value links were omitted.")
	}
	responseAnalysis := Record{"parse_state": "incomplete", "status": nil}
	requestAnalysis := Record{"parse_state": "incomplete"}
	if projection.Search.ResponseStatus != nil {
		responseAnalysis["status"] = *projection.Search.ResponseStatus
	}
	if requestBodyAnalysis.html != nil {
		requestAnalysis["html"] = requestBodyAnalysis.html
		if requestBodyAnalysis.characteristics != nil {
			requestAnalysis["characteristics"] = requestBodyAnalysis.characteristics
		}
	}
	if responseBodyAnalysis.html != nil {
		responseAnalysis["html"] = responseBodyAnalysis.html
		if responseBodyAnalysis.characteristics != nil {
			responseAnalysis["characteristics"] = responseBodyAnalysis.characteristics
		}
	}
	card := Record{
		"schema_version": 1, "id": cardID, "capture_id": captureID,
		"program": program, "source_refs": refs, "endpoint": cardEndpoint,
		"context":           context,
		"request_analysis":  requestAnalysis,
		"response_analysis": responseAnalysis,
		"business_action":   Record{"known_effect": "unknown"},
		"hypotheses":        []any{}, "missing_evidence": cardMissing,
		"analysis_state": "incomplete",
	}

	asset := Record{"id": assetID, "kind": "asset", "data": Record{
		"program": program, "origin": origin,
	}, "source_refs": refs}
	endpoint := Record{"id": endpointID, "kind": "endpoint", "data": endpointData, "source_refs": refs}
	observationData := Record{
		"program": program, "capture_id": captureID, "card_id": cardID,
		"endpoint_id": endpointID, "observation_kind": "historical_capture",
		"observed_at": context["observed_at"], "parse_state": projection.ParseState,
		"analysis_state": "incomplete",
		"freshness":      "unknown", "context": context,
	}
	if projection.Search.ResponseStatus != nil {
		observationData["http_status"] = *projection.Search.ResponseStatus
	}
	observation := Record{"id": observationID, "kind": "observation", "data": observationData, "source_refs": refs}

	exposes, err := historicalAssertion(program, assetID, "exposes", endpointID, context, refs)
	if err != nil {
		return nil, "", errors.New("historical graph provenance identity could not be computed")
	}
	invokes, err := historicalAssertion(program, observationID, "invokes", endpointID, context, refs)
	if err != nil {
		return nil, "", errors.New("historical graph provenance identity could not be computed")
	}
	entities := []Record{asset, endpoint, observation}
	assertions := []Record{exposes, invokes}
	if operation != nil {
		entities = append(entities, operation)
		assertions = append(assertions, belongsToOperation)
	}
	if hasPathField {
		queryFields = append(queryFields, pathField)
	}
	type parameterCandidate struct {
		field   historicalParameterField
		side    string
		subject string
	}
	candidates := make([]parameterCandidate, 0, len(queryFields)+len(requestBodyFields)+len(responseBodyFields))
	for _, field := range append(queryFields, requestBodyFields...) {
		candidates = append(candidates, parameterCandidate{field: field, side: "request", subject: endpointID})
	}
	for _, field := range responseBodyFields {
		candidates = append(candidates, parameterCandidate{field: field, side: "response", subject: observationID})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].side != candidates[j].side {
			return candidates[i].side < candidates[j].side
		}
		if candidates[i].field.location != candidates[j].field.location {
			return candidates[i].field.location < candidates[j].field.location
		}
		if candidates[i].field.path != candidates[j].field.path {
			return candidates[i].field.path < candidates[j].field.path
		}
		return candidates[i].field.typeName < candidates[j].field.typeName
	})
	parameterIDs := make(map[string]struct{}, len(candidates))
	valueReferenceIDs := make(map[string]struct{})
	for _, candidate := range candidates {
		field := candidate.field
		parameterData := Record{
			"program": program, "endpoint_id": endpointID, "side": candidate.side,
			"path": field.path, "type": field.typeName, "location": field.location,
		}
		for key, value := range field.metadata {
			parameterData[key] = value
		}
		parameterID, idErr := historicalEntityID("parameter", parameterData)
		if idErr != nil {
			return nil, "", errors.New("historical graph parameter identity could not be computed")
		}
		if _, exists := parameterIDs[parameterID]; !exists {
			parameterIDs[parameterID] = struct{}{}
			parameter := Record{"id": parameterID, "kind": "parameter", "data": parameterData, "source_refs": refs}
			predicate := "has_parameter"
			if candidate.side == "response" {
				predicate = "returns"
			}
			hasParameter, assertionErr := historicalAssertion(program, candidate.subject, predicate, parameterID, context, refs)
			if assertionErr != nil {
				return nil, "", errors.New("historical graph parameter provenance could not be computed")
			}
			entities = append(entities, parameter)
			assertions = append(assertions, hasParameter)
		}
		if candidate.field.occurrence == nil || candidate.field.occurrence.sourceSpan == nil {
			continue
		}
		var valueSpan any
		if candidate.field.occurrence.valueSpan != nil {
			valueSpan = candidate.field.occurrence.valueSpan
		}
		fieldOccurrenceData := Record{
			"program": program, "capture_id": captureID, "revision_id": revisionID, "observation_id": observationID,
			"parameter_id": parameterID, "side": candidate.side,
			"location": field.location, "path": field.path,
			"occurrence_index": candidate.field.occurrence.index, "type": field.typeName,
			"source_span": candidate.field.occurrence.sourceSpan,
			"value_span":  valueSpan,
		}
		fieldOccurrenceID, occurrenceErr := historicalEntityID("field_occurrence", fieldOccurrenceData)
		if occurrenceErr != nil {
			return nil, "", errors.New("historical field occurrence identity could not be computed")
		}
		fieldOccurrence := Record{
			"id": fieldOccurrenceID, "kind": "field_occurrence",
			"data": fieldOccurrenceData, "source_refs": refs,
		}
		observedOccurrence, occurrenceErr := historicalAssertion(program, observationID, "has_occurrence", fieldOccurrenceID, context, refs)
		if occurrenceErr != nil {
			return nil, "", errors.New("historical field occurrence provenance could not be computed")
		}
		occurrenceOfParameter, occurrenceErr := historicalAssertion(program, fieldOccurrenceID, "occurrence_of", parameterID, context, refs)
		if occurrenceErr != nil {
			return nil, "", errors.New("historical field schema provenance could not be computed")
		}
		entities = append(entities, fieldOccurrence)
		assertions = append(assertions, observedOccurrence, occurrenceOfParameter)
		if candidate.field.occurrence.valueToken == "" {
			continue
		}
		valueData := Record{
			"program": program, "token_version": candidate.field.occurrence.valueTokenVersion,
			"value_type": "string", "normalization": candidate.field.occurrence.valueNormalization,
			"equality_signature": candidate.field.occurrence.valueToken,
		}
		valueReferenceID, valueErr := historicalEntityID("value_reference", valueData)
		if valueErr != nil {
			return nil, "", errors.New("historical value reference identity could not be computed")
		}
		if _, exists := valueReferenceIDs[valueReferenceID]; !exists {
			valueReferenceIDs[valueReferenceID] = struct{}{}
			entities = append(entities, Record{
				"id": valueReferenceID, "kind": "value_reference",
				"data": valueData, "source_refs": refs,
			})
		}
		valueContext := cloneRecord(context)
		valueContext["scenario"] = "keyed_value_equality"
		valueContext["scenario_step"] = "correlation_only"
		valueUsed, valueErr := historicalAssertion(program, valueReferenceID, "value_used_in", fieldOccurrenceID, valueContext, refs)
		if valueErr != nil {
			return nil, "", errors.New("historical value correlation provenance could not be computed")
		}
		assertions = append(assertions, valueUsed)
	}
	if requestSignature != "" {
		requestData := messageVariantData(program, "request_signature", exchange.RequestRaw, true, exchange.RequestTruncated, requestSignature)
		requestID, idErr := historicalEntityID("request_variant", Record{"program": program, "request_signature": requestSignature})
		if idErr != nil {
			return nil, "", errors.New("historical request variant identity could not be computed")
		}
		requestVariant := Record{"id": requestID, "kind": "request_variant", "data": requestData, "source_refs": refs}
		usesRequest, assertionErr := historicalAssertion(program, observationID, "uses_request_variant", requestID, context, refs)
		if assertionErr != nil {
			return nil, "", errors.New("historical request variant provenance could not be computed")
		}
		entities = append(entities, requestVariant)
		assertions = append(assertions, usesRequest)
	}
	if responseSignature != nil {
		responseData := messageVariantData(program, "response_signature", exchange.ResponseRaw, false, exchange.ResponseTruncated, *responseSignature)
		responseID, idErr := historicalEntityID("response_variant", Record{"program": program, "response_signature": *responseSignature})
		if idErr != nil {
			return nil, "", errors.New("historical response variant identity could not be computed")
		}
		responseVariant := Record{"id": responseID, "kind": "response_variant", "data": responseData, "source_refs": refs}
		receivedResponse, assertionErr := historicalAssertion(program, observationID, "received_response", responseID, context, refs)
		if assertionErr != nil {
			return nil, "", errors.New("historical response variant provenance could not be computed")
		}
		entities = append(entities, responseVariant)
		assertions = append(assertions, receivedResponse)
	}
	return Record{
		"schema_version": 1, "program": program,
		"sources":    []Record{ref},
		"entities":   entities,
		"assertions": assertions,
		"cards":      []Record{card},
	}, observationID, nil
}

func messageVariantData(program, signatureField string, raw []byte, request bool, truncated *bool, signature string) Record {
	headers, body, boundary, headersComplete := splitMessage(raw)
	contentTypeValue := contentType(headers)
	headerNames := make(map[string]struct{}, len(headers))
	for _, header := range headers {
		headerNames[safeField(strings.ToLower(header.name))] = struct{}{}
	}
	orderedHeaders := make([]string, 0, len(headerNames))
	for name := range headerNames {
		orderedHeaders = append(orderedHeaders, name)
	}
	sort.Strings(orderedHeaders)
	headerMetadata := make([]any, len(orderedHeaders))
	for index, name := range orderedHeaders {
		headerMetadata[index] = name
	}
	fieldSet := make(map[string]struct{})
	queryComplete := true
	data := Record{
		"program": program, signatureField: signature,
		"signature_state": signatureState(truncated), "content_type": contentTypeValue,
		"body_bytes": nil, "parse_state": "incomplete",
		"header_metadata": headerMetadata,
		"characteristics": Record{},
	}
	startLineComplete := true
	if request {
		method, target := requestLine(raw)
		startLineComplete = httpMethodPattern.MatchString(method)
		if !startLineComplete {
			method = "UNKNOWN"
		}
		data["method"] = method
		parsedTarget, err := url.Parse(target)
		if err != nil {
			queryComplete = false
		} else if queryValues, err := url.ParseQuery(parsedTarget.RawQuery); err != nil {
			queryComplete = false
		} else {
			for name := range queryValues {
				fieldSet[safeField(name)] = struct{}{}
			}
		}
	} else {
		status := responseStatus(raw)
		if status != nil {
			data["status"] = *status
		} else {
			startLineComplete = false
		}
	}
	bodyBytes := any(nil)
	parseComplete := false
	if boundary {
		bodyBytes = len(body)
		bodyFields, bodyComplete := bodyFieldPaths(body, contentTypeValue, headers)
		for name := range bodyFields {
			fieldSet[name] = struct{}{}
		}
		parseComplete = startLineComplete && headersComplete && bodyComplete && queryComplete && isExplicitFalse(truncated)
	}
	fields := make([]string, 0, len(fieldSet))
	for name := range fieldSet {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	fieldValues := make([]any, len(fields))
	for index, name := range fields {
		fieldValues[index] = name
	}
	data["body_bytes"] = bodyBytes
	data["fields"] = fieldValues
	if parseComplete {
		data["parse_state"] = "complete"
	}
	return data
}
func signatureState(truncated *bool) string {
	if truncated == nil {
		return "unknown_extent"
	}
	if *truncated {
		return "partial_prefix"
	}
	return "complete"
}

func historicalEntityID(kind string, identity Record) (string, error) {
	encoded, err := canonicalJSON(Record{"kind": kind, "identity": identity})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return kind + "-" + hex.EncodeToString(digest[:]), nil
}

func historicalAssertion(program, subject, predicate, object string, context Record, refs []Record) (Record, error) {
	assertionContext := cloneRecord(context)
	assertionContext["program"] = program
	refs = navigationSourceRefs(refs)
	assertion := Record{
		"subject": subject, "predicate": predicate, "object": object,
		"context": assertionContext, "origin_kind": "parsed",
		"evidence_state": "observed", "source_refs": refs,
	}
	identity := cloneRecord(assertion)
	id, err := historicalEntityID("assertion", identity)
	if err != nil {
		return nil, err
	}
	assertion["id"] = id
	return assertion, nil
}

func historicalUnknownContext(program, origin string, capturedAt *string) Record {
	observedAt := "unknown"
	if capturedAt != nil {
		if parsed, err := time.Parse(time.RFC3339Nano, *capturedAt); err == nil {
			observedAt = parsed.UTC().Format(time.RFC3339Nano)
		}
	}
	return Record{
		"program": program, "origin": origin,
		"actor": "unknown", "session_version": "unknown", "tenant": "unknown",
		"role": "unknown", "object": "unknown", "object_owner": "unknown",
		"object_state": "unknown", "access_policy": "unknown", "policy_source": "unknown",
		"expected_visibility": "unknown", "annotation_origin": "unknown",
		"scenario": "unknown", "scenario_step": "unknown", "observed_at": observedAt,
	}
}

func historicalOrigin(target string, source Record) string {
	return historicalOriginAt(target, source, "request.url.host")
}

func historicalOriginAt(target string, source Record, hostLocation string) string {
	if len(target) > 65_536 {
		return "unknown"
	}
	parsed, err := historicalParseURL(target)
	if err != nil {
		return "unknown"
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if (scheme != "http" && scheme != "https") || host == "" {
		return "unknown"
	}
	normalizedHost, hostErr := caido.NormalizeRequestHost(host)
	if hostErr != nil || historicalRiskNamePattern.MatchString(host) {
		alias, aliasErr := historicalOpaqueLabel(source, hostLocation, "host")
		if aliasErr != nil {
			return "unknown"
		}
		host = alias + ".invalid"
	} else {
		host = normalizedHost
	}
	port := ""
	if portText := parsed.Port(); portText != "" {
		portNumber, portErr := strconv.Atoi(portText)
		if portErr != nil || portNumber < 0 || portNumber > 65535 {
			return "unknown"
		}
		port = strconv.Itoa(portNumber)
	}
	defaultPort := "80"
	if scheme == "https" {
		defaultPort = "443"
	}
	originHost := host
	if strings.Contains(host, ":") {
		originHost = "[" + host + "]"
	}
	origin := scheme + "://" + originHost
	if port != "" && port != defaultPort && port != "0" {
		origin += ":" + port
	}
	return origin
}

func historicalTargetValid(target string) bool {
	if len(target) > 65_536 {
		return false
	}
	parsed, err := historicalParseURL(target)
	if err != nil {
		return false
	}
	if port := parsed.Port(); port != "" {
		portNumber, parseErr := strconv.Atoi(port)
		if parseErr != nil || portNumber < 0 || portNumber > 65535 {
			return false
		}
	}
	return true
}

func historicalGraphPath(target string, source Record) (string, bool) {
	if len(target) > 65_536 {
		return "unknown", true
	}
	parsed, err := historicalParseURL(target)
	if err != nil {
		return "unknown", true
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	segments := strings.Split(path, "/")
	if len(segments) > 4_096 {
		return "unknown", true
	}
	for index, segment := range segments {
		if segment == "" {
			continue
		}
		decoded, _ := historicalURLUnquote(segment, false, false)
		routeWord := strings.ToLower(strings.SplitN(decoded, ".", 2)[0])
		knownRoute := historicalGraphIdentityRouteWord(routeWord)
		if !fieldNamePattern.MatchString(decoded) || historicalRiskNamePattern.MatchString(decoded) || (!knownRoute && !historicalSingleLetter.MatchString(decoded)) {
			alias, aliasErr := historicalOpaqueLabel(source, "request.url.segment["+strconv.Itoa(index)+"]", "segment")
			if aliasErr != nil {
				return "unknown", true
			}
			segments[index] = alias
		} else {
			segments[index] = decoded
		}
	}
	return strings.Join(segments, "/"), false
}

// historicalGraphIdentityRouteWord preserves the route vocabulary used by
// the existing Python BehaviorGraph v1 mapper. That mapper treated product
// and products as opaque path segments; making them static here changes an
// imported endpoint ID and causes an immutable-history collision on replay.
// RouteTemplate may use the broader vocabulary for readable search routes.
func historicalGraphIdentityRouteWord(word string) bool {
	if word == "product" || word == "products" {
		return false
	}
	_, known := historicalRouteWords[word]
	return known
}

func historicalQueryTarget(rawRequest []byte, fallback string) (string, int, bool) {
	rawTarget, rawOffset, ok := historicalRequestTargetSpan(rawRequest)
	if !ok {
		return fallback, 0, false
	}
	rawURL, rawErr := historicalParseURL(rawTarget)
	fallbackURL, fallbackErr := historicalParseURL(fallback)
	if rawErr != nil || fallbackErr != nil || rawURL.RawQuery != fallbackURL.RawQuery {
		return fallback, 0, false
	}
	return rawTarget, rawOffset, true
}

func historicalRequestTargetSpan(raw []byte) (string, int, bool) {
	lineEnd := bytes.IndexByte(raw, '\n')
	if lineEnd < 0 {
		lineEnd = len(raw)
	}
	line := bytes.TrimSuffix(raw[:lineEnd], []byte("\r"))
	if len(line) == 0 || len(line) > 65_536 {
		return "", 0, false
	}
	index := 0
	for index < len(line) && (line[index] == ' ' || line[index] == '\t') {
		index++
	}
	for index < len(line) && line[index] != ' ' && line[index] != '\t' {
		index++
	}
	for index < len(line) && (line[index] == ' ' || line[index] == '\t') {
		index++
	}
	start := index
	for index < len(line) && line[index] != ' ' && line[index] != '\t' {
		index++
	}
	if start == index {
		return "", 0, false
	}
	return string(line[start:index]), start, true
}

func historicalQueryFields(target string, source Record, fieldValueKey []byte, requestTargetOffset int, sourceSpanAvailable bool, wireFidelity string) ([]string, []historicalParameterField, bool, int) {
	if len(target) > 65_536 {
		return []string{}, nil, true, 0
	}
	parsed, err := historicalParseURL(target)
	if err != nil {
		return []string{}, nil, true, 0
	}
	pairs := strings.Split(parsed.RawQuery, "&")
	if len(pairs) == 1 && pairs[0] == "" {
		return []string{}, nil, false, 0
	}
	if len(pairs) > 100_001 {
		return []string{}, nil, true, 0
	}
	namesSet := make(map[string]struct{}, len(pairs))
	fieldCapacity := len(pairs)
	if fieldCapacity > 2_000 {
		fieldCapacity = 2_000
	}
	fieldSet := make(map[string][]historicalParameterField, fieldCapacity)
	fieldIndex := 0
	fieldLimitExceeded := false
	queryStart := strings.IndexByte(target, '?')
	queryValueBase := requestTargetOffset
	if queryStart >= 0 {
		queryValueBase += queryStart + 1
	} else {
		sourceSpanAvailable = false
	}
	pairOffset := 0
	for _, pair := range pairs {
		if pair == "" {
			pairOffset++
			continue
		}
		key, value, hasValue := strings.Cut(pair, "=")
		name, nameValid := historicalURLUnquote(key, true, true)
		if !nameValid {
			return []string{}, nil, true, 0
		}
		if hasValue {
			_, valueValid := historicalURLUnquote(value, true, true)
			if !valueValid {
				return []string{}, nil, true, 0
			}
		}
		safeName, nameErr := historicalSafeQueryName(name, source, fieldIndex)
		if nameErr != nil {
			return []string{}, nil, true, 0
		}
		fieldIndex++
		namesSet[safeName] = struct{}{}
		if fieldIndex <= 2_000 {
			occurrenceIndex := len(fieldSet[safeName])
			occurrence := &historicalFieldOccurrence{index: occurrenceIndex}
			if sourceSpanAvailable {
				occurrence.sourceSpan = Record{
					"start": queryValueBase + pairOffset, "end": queryValueBase + pairOffset + len(pair),
					"unit": "byte", "offset_basis": "request_message", "wire_fidelity": wireFidelity,
				}
				if hasValue {
					valueStart := queryValueBase + pairOffset + len(key) + 1
					occurrence.valueSpan = Record{
						"start": valueStart, "end": valueStart + len(value),
						"unit": "byte", "offset_basis": "request_message", "wire_fidelity": wireFidelity,
					}
				}
			}
			if hasValue && sourceSpanAvailable && len(fieldValueKey) == 32 && safeName == name &&
				!secretNamePattern.MatchString(name) && len(value) <= 4_096 {
				decoded, decodeErr := url.QueryUnescape(value)
				if decodeErr == nil && len(decoded) > 0 && len(decoded) <= 4_096 && safeEqualityString(decoded) {
					occurrence.valueToken = fieldValueTokenForNormalization(fieldValueKey, decoded, "decoded_utf8_scalar_v2")
					occurrence.valueTokenVersion = "field-equality-v2"
					occurrence.valueNormalization = "decoded_utf8_scalar_v2"
				}
			}
			fieldSet[safeName] = append(fieldSet[safeName], historicalParameterField{
				typeName: "string", location: "query", occurrence: occurrence,
			})
		} else {
			fieldLimitExceeded = true
		}
		pairOffset += len(pair) + 1
	}
	names := make([]string, 0, len(namesSet))
	fields := make([]historicalParameterField, 0, minInt(fieldIndex, 2_000))
	for name, occurrences := range fieldSet {
		names = append(names, name)
		if len(occurrences) == 1 {
			// Keep the established identity for a singleton query field.
			occurrences[0].path = "query[\"" + name + "\"][]"
			fields = append(fields, occurrences[0])
			continue
		}
		// The per-name ordinal preserves each repeated input as a separate
		// structural field occurrence without exposing the parsed value.
		for index, occurrence := range occurrences {
			occurrence.path = "query[\"" + name + "\"][" + strconv.Itoa(index) + "]"
			if occurrence.occurrence != nil {
				occurrence.occurrence.index = index
			}
			fields = append(fields, occurrence)
		}
	}
	sort.Strings(names)
	return names, fields, fieldLimitExceeded, fieldIndex
}

func safeEqualityString(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func historicalSafeQueryName(name string, source Record, index int) (string, error) {
	location := "request.url.query[" + strconv.Itoa(index) + "]"
	return historicalSafeFieldName(name, source, location)
}

func historicalOpaqueLabel(source Record, location, prefix string) (string, error) {
	aliasID, err := historicalEntityID("alias", Record{"source": source, "location": location})
	if err != nil {
		return "", err
	}
	return prefix + "_" + strings.TrimPrefix(aliasID, "alias-")[:16], nil
}

func historicalPathField(target string) (historicalParameterField, bool, bool, int) {
	if len(target) > 65_536 {
		return historicalParameterField{}, false, true, 0
	}
	parsed, err := historicalParseURL(target)
	if err != nil {
		return historicalParameterField{}, false, true, 0
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	segments := strings.Split(path, "/")
	if len(segments) > 4_096 {
		return historicalParameterField{}, false, true, 0
	}
	fieldCount := 0
	for _, segment := range segments {
		if segment != "" {
			fieldCount++
		}
	}
	if fieldCount > 0 {
		return historicalParameterField{path: "path[]", typeName: "string", location: "path"}, true, false, fieldCount
	}
	return historicalParameterField{}, false, false, 0
}

func historicalParseURL(target string) (*url.URL, error) {
	return url.Parse(escapeInvalidPercent(target))
}

func escapeInvalidPercent(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '%' {
			builder.WriteByte(value[index])
			continue
		}
		if index+2 < len(value) && isHexadecimalDigit(value[index+1]) && isHexadecimalDigit(value[index+2]) {
			builder.WriteString(value[index : index+3])
			index += 2
			continue
		}
		builder.WriteString("%25")
	}
	return builder.String()
}

func historicalURLUnquote(value string, plusAsSpace, strictUTF8 bool) (string, bool) {
	decoded := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		if value[index] == '%' && index+2 < len(value) && isHexadecimalDigit(value[index+1]) && isHexadecimalDigit(value[index+2]) {
			decoded = append(decoded, hexadecimalValue(value[index+1])<<4|hexadecimalValue(value[index+2]))
			index += 2
			continue
		}
		if plusAsSpace && value[index] == '+' {
			decoded = append(decoded, ' ')
			continue
		}
		decoded = append(decoded, value[index])
	}
	if !utf8.Valid(decoded) {
		if strictUTF8 {
			return "", false
		}
		return strings.ToValidUTF8(string(decoded), "\uFFFD"), true
	}
	return string(decoded), true
}

func isHexadecimalDigit(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func hexadecimalValue(value byte) byte {
	switch {
	case value >= '0' && value <= '9':
		return value - '0'
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10
	default:
		return value - 'A' + 10
	}
}

func stringsAsAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}
