package graph

import (
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
	historicalHostPattern     = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
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
	return buildHistoricalBundle(exchange, program, source, "", nil)
}

// BuildHistoricalBundleWithSignatures adds project-keyed message variants to
// the historical observation. Signatures are HMACs computed from the selected
// project's vault key; raw message bytes are never copied into the graph.
func BuildHistoricalBundleWithSignatures(exchange caido.HttpExchange, program string, source Record, requestSignature string, responseSignature *string) (Record, string, error) {
	if !graphDigestPattern.MatchString(requestSignature) || exchange.HasResponse != (responseSignature != nil) ||
		responseSignature != nil && !graphDigestPattern.MatchString(*responseSignature) {
		return nil, "", errors.New("historical graph message signature is invalid")
	}
	return buildHistoricalBundle(exchange, program, source, requestSignature, responseSignature)
}

func buildHistoricalBundle(exchange caido.HttpExchange, program string, source Record, requestSignature string, responseSignature *string) (Record, string, error) {
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
	pathFieldCount := 0
	queryParseIncomplete, pathParseIncomplete := true, true
	var pathField historicalParameterField
	hasPathField := false
	if target != "" && historicalTargetValid(target) {
		origin = historicalOrigin(target, ref)
		var graphPathIncomplete bool
		path, graphPathIncomplete = historicalGraphPath(target, ref)
		queryNames, queryFields, queryParseIncomplete, queryFieldCount = historicalQueryFields(target, ref)
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
	)
	requestBodyFields, requestBodyComplete := requestBodyAnalysis.fields, requestBodyAnalysis.complete
	var responseBodyFields []historicalParameterField
	responseBodyComplete := false
	var responseBodyAnalysis historicalBodyAnalysis
	if exchange.HasResponse {
		responseBodyAnalysis = historicalMessageBodyFields(
			exchange.ResponseRaw, "response.body", ref, historicalMaxParameterFields, exchange.ResponseTruncated,
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
	if pathParseIncomplete {
		cardMissing = append(cardMissing, "Request path parameters could not be mapped within the mapper limit.")
	}
	if !requestBodyComplete || !responseBodyComplete {
		cardMissing = append(cardMissing, "Request or response body fields could not be mapped completely because of missing data, unsupported media, truncation, or parser limits.")
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
		"observed_at": context["observed_at"], "analysis_state": "incomplete",
		"freshness": "unknown", "context": context,
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
		if _, exists := parameterIDs[parameterID]; exists {
			continue
		}
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
	if !historicalHostPattern.MatchString(host) || historicalRiskNamePattern.MatchString(host) {
		alias, aliasErr := historicalOpaqueLabel(source, hostLocation, "host")
		if aliasErr != nil {
			return "unknown"
		}
		host = alias + ".invalid"
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
	origin := scheme + "://" + host
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

func historicalQueryFields(target string, source Record) ([]string, []historicalParameterField, bool, int) {
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
	for _, pair := range pairs {
		if pair == "" {
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
			fieldSet[safeName] = append(fieldSet[safeName], historicalParameterField{
				typeName: "string", location: "query",
			})
		} else {
			fieldLimitExceeded = true
		}
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
		// Parameter values are discarded. The per-name ordinal preserves each
		// repeated input as a separate graph occurrence without exposing values.
		for index, occurrence := range occurrences {
			occurrence.path = "query[\"" + name + "\"][" + strconv.Itoa(index) + "]"
			fields = append(fields, occurrence)
		}
	}
	sort.Strings(names)
	return names, fields, fieldLimitExceeded, fieldIndex
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
