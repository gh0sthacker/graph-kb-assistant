// Package graph builds a deterministic, redacted structural projection of
// captured HTTP exchanges. Message values are parsed in memory only.
package graph

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"bb-graph-assistant/internal/caido"
)

const maxParsedBodyBytes = 2 << 20

const RouteFamilyV1Version = "route-family-v1"

var (
	fieldNamePattern        = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$.-]{0,63}$`)
	secretNamePattern       = regexp.MustCompile(`(?i)(token|secret|password|passwd|cookie|authorization|session|api[_-]?key|credential|email|phone|ssn)`)
	httpMethodPattern       = regexp.MustCompile(`^[A-Z]{1,16}$`)
	headerNamePattern       = regexp.MustCompile(`^[a-z0-9!#$%&'*+.^_` + "`" + `|~0-9-]{1,128}$`)
	pseudoHeaderNamePattern = regexp.MustCompile(`^:[a-z0-9!#$%&'*+.^_` + "`" + `|~-]{1,128}$`)
)

type SearchRecord struct {
	Method              string   `json:"method"`
	Host                string   `json:"host"`
	Port                int      `json:"port"`
	Route               string   `json:"route"`
	ResponseStatus      *int     `json:"response_status"`
	RequestContentType  string   `json:"request_content_type"`
	ResponseContentType string   `json:"response_content_type"`
	FieldNames          []string `json:"field_names"`
	HeaderNames         []string `json:"header_names"`
	CapturedAt          *string  `json:"captured_at"`
}

type ExchangeProjection struct {
	ObservationID string       `json:"observation_id"`
	ParseState    string       `json:"parse_state"`
	Search        SearchRecord `json:"search_record"`
}

// ProjectExchange converts a Caido exchange into safe endpoint and field
// metadata. It intentionally discards all header/body values after parsing.
func ProjectExchange(exchange caido.HttpExchange) (ExchangeProjection, error) {
	if err := exchange.Validate(); err != nil {
		return ExchangeProjection{}, errors.New("Caido exchange is invalid")
	}
	observationID, err := exchange.Ref.ObservationID()
	if err != nil {
		return ExchangeProjection{}, errors.New("Caido exchange identity is invalid")
	}
	fallbackMethod, lineTarget := requestLine(exchange.RequestRaw)
	method := strings.ToUpper(firstNonEmpty(exchange.Method, fallbackMethod, "UNKNOWN"))
	if !httpMethodPattern.MatchString(method) {
		method = "UNKNOWN"
	}
	target := firstNonEmpty(exchange.Target, lineTarget, "/")
	parsed, _ := url.Parse(target)
	if parsed == nil {
		parsed = &url.URL{Path: "/"}
	}
	host := normalizeProjectionHost(firstNonEmpty(exchange.Host, parsed.Hostname(), "unknown"))
	scheme := strings.ToLower(firstNonEmpty(exchange.Scheme, parsed.Scheme, "unknown"))
	if scheme != "http" && scheme != "https" {
		scheme = "unknown"
	}
	port := exchange.Port
	if port == 0 {
		if parsedPort := parsed.Port(); parsedPort != "" {
			if parsedNumber, parseErr := parsePort(parsedPort); parseErr == nil {
				port = parsedNumber
			}
		}
	}
	if port == 0 && scheme == "https" {
		port = 443
	} else if port == 0 && scheme == "http" {
		port = 80
	}
	if port < 0 || port > 65535 {
		port = 0
	}

	requestHeaders, requestBody, requestBoundary, requestHeadersComplete := splitMessage(exchange.RequestRaw)
	requestContentType := contentType(requestHeaders)
	responseHeaders := []headerField{}
	responseBody := []byte(nil)
	responseBoundary := false
	responseHeadersComplete := false
	responseContentType := "unknown"
	if exchange.HasResponse {
		responseHeaders, responseBody, responseBoundary, responseHeadersComplete = splitMessage(exchange.ResponseRaw)
		responseContentType = contentType(responseHeaders)
	}

	fields := make(map[string]struct{})
	parseComplete := true
	queryValues, queryErr := url.ParseQuery(parsed.RawQuery)
	if queryErr != nil {
		parseComplete = false
	}
	queryNames := sortedKeys(queryValues)
	for _, name := range queryNames {
		fields["req:"+safeField(name)] = struct{}{}
	}
	reqBodyFields, reqBodyComplete := bodyFieldPaths(requestBody, requestContentType, requestHeaders)
	for name := range reqBodyFields {
		fields["req:"+name] = struct{}{}
	}
	responseBodyFields := map[string]struct{}{}
	responseBodyComplete := false
	if exchange.HasResponse && responseBoundary {
		responseBodyFields, responseBodyComplete = bodyFieldPaths(responseBody, responseContentType, responseHeaders)
	}
	for name := range responseBodyFields {
		fields["resp:"+name] = struct{}{}
	}
	fieldNames := mapKeys(fields)
	headerSet := make(map[string]struct{})
	for _, header := range append(append([]headerField(nil), requestHeaders...), responseHeaders...) {
		headerSet[safeField(strings.ToLower(header.name))] = struct{}{}
	}
	headerNames := mapKeys(headerSet)

	status := exchange.ResponseStatus
	if status == nil && exchange.HasResponse {
		status = responseStatus(exchange.ResponseRaw)
	}
	parseComplete = parseComplete && requestBoundary && requestHeadersComplete && reqBodyComplete &&
		exchange.HasResponse && responseBoundary && responseHeadersComplete && responseBodyComplete && status != nil &&
		isExplicitFalse(exchange.RequestTruncated) && isExplicitFalse(exchange.ResponseTruncated) &&
		!exchange.ResponseStatusMetadataMismatch && !exchange.MalformedHeaderNamePresent
	state := "partial"
	if parseComplete {
		state = "complete"
	}

	return ExchangeProjection{
		ObservationID: observationID,
		ParseState:    state,
		Search: SearchRecord{
			Method: method, Host: host, Port: port, Route: RouteTemplate(target),
			ResponseStatus: status, RequestContentType: requestContentType,
			ResponseContentType: responseContentType, FieldNames: fieldNames,
			HeaderNames: headerNames, CapturedAt: exchange.CapturedAt,
		},
	}, nil
}

func normalizeProjectionHost(raw string) string {
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	} else if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = raw[1 : len(raw)-1]
	}
	normalized, err := caido.NormalizeRequestHost(raw)
	if err != nil {
		return "unknown"
	}
	return normalized
}

func RouteTemplate(target string) string {
	path := target
	if parsed, err := url.Parse(target); err == nil {
		path = parsed.Path
	}
	if path == "" {
		path = "/"
	}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		_, static := historicalRouteWords[lower]
		versionLike := len(lower) >= 2 && lower[0] == 'v' && len(lower) <= 3
		if versionLike {
			for _, r := range lower[1:] {
				if r < '0' || r > '9' {
					versionLike = false
					break
				}
			}
		}
		if static || versionLike {
			parts[i] = lower
		} else {
			parts[i] = "{value}"
		}
	}
	result := strings.Join(parts, "/")
	if len(result) > 2048 {
		result = result[:2048]
	}
	return result
}

// RouteFamilyV1 returns a versioned, value-free route family for operation
// grouping. It intentionally uses the existing static route vocabulary so
// endpoint identity and historic route labels remain unchanged.
func RouteFamilyV1(target string) (string, bool) {
	if target == "" || !historicalTargetValid(target) {
		return "", false
	}
	parsed, err := historicalParseURL(target)
	if err != nil || parsed.Opaque != "" {
		return "", false
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if len(path) > 2048 || len(strings.Split(path, "/")) > 4096 {
		return "", false
	}
	return routeFamilyFromEscapedPath(path), true
}

func routeFamilyFromEscapedPath(path string) string {
	parts := strings.Split(path, "/")
	for index, part := range parts {
		if part == "" {
			continue
		}
		decoded, valid := historicalURLUnquote(part, false, true)
		if !valid {
			parts[index] = "{value}"
			continue
		}
		lower := strings.ToLower(decoded)
		_, static := historicalRouteWords[lower]
		versionLike := len(lower) >= 2 && len(lower) <= 3 && lower[0] == 'v'
		if versionLike {
			for _, char := range lower[1:] {
				if char < '0' || char > '9' {
					versionLike = false
					break
				}
			}
		}
		if static || versionLike {
			parts[index] = lower
		} else {
			parts[index] = "{value}"
		}
	}
	return strings.Join(parts, "/")
}

// DeriveLiveKeys mirrors the existing Python project-keyed signature scheme.
func DeriveLiveKeys(projectKey string, rootKey []byte) (indexKey, responseKey []byte, err error) {
	if len(rootKey) != 32 || len(projectKey) != 64 {
		return nil, nil, errors.New("invalid graph key material")
	}
	if _, decodeErr := hex.DecodeString(projectKey); decodeErr != nil {
		return nil, nil, errors.New("invalid graph project key")
	}
	indexKey = hmacDigest(rootKey, []byte("bb-assistant-live-index-v1\x00"+projectKey))
	responseKey = hmacDigest(indexKey, []byte("response-signature-v1"))
	return indexKey, responseKey, nil
}

// DeriveFieldValueKey creates a domain-separated key for within-project field
// equality tokens. It must never be used as an encryption key or exposed.
func DeriveFieldValueKey(indexKey []byte) ([]byte, error) {
	if len(indexKey) != 32 {
		return nil, errors.New("invalid field-value index key")
	}
	return hmacDigest(indexKey, []byte("bb-assistant-field-value-key-v1")), nil
}

func fieldValueToken(key []byte, value string) string {
	material := []byte("bb-graph-field-equality-v1\x00urlencoded-string\x00" + value)
	digest := hmacDigest(key, material)
	clear(material)
	token := hex.EncodeToString(digest)
	clear(digest)
	return token
}

func VariantSignature(exchange caido.HttpExchange, request bool, indexKey, responseKey []byte) (string, error) {
	projectKey, err := exchange.Ref.Project.StorageKey()
	if err != nil {
		return "", errors.New("invalid graph project identity")
	}
	key, domain, raw, truncated := indexKey, "bb-graph-request-v1\x00", exchange.RequestRaw, exchange.RequestTruncated
	if !request {
		if !exchange.HasResponse {
			return "", nil
		}
		key, domain, raw, truncated = responseKey, "bb-graph-response-v1\x00", exchange.ResponseRaw, exchange.ResponseTruncated
	}
	if len(key) != 32 {
		return "", errors.New("invalid graph signature key")
	}
	material := make([]byte, 0, len(domain)+len(projectKey)+len(raw)+16)
	material = append(material, domain...)
	material = append(material, projectKey...)
	material = append(material, 0)
	material = append(material, raw...)
	material = append(material, 0)
	material = append(material, []byte("extent=")...)
	material = append(material, truncationText(truncated)...)
	digest := hmacDigest(key, material)
	clear(material)
	signature := hex.EncodeToString(digest)
	clear(digest)
	return signature, nil
}

func URLIndexKey(value, base string, key []byte) (string, error) {
	normalized, err := NormalizeURL(value, base)
	if err != nil {
		return "", err
	}
	if len(key) != 32 {
		return "", errors.New("invalid graph index key")
	}
	material := make([]byte, 0, len("bb-assistant-url-index-v1\x00")+len(normalized))
	material = append(material, "bb-assistant-url-index-v1\x00"...)
	material = append(material, normalized...)
	digest := hmacDigest(key, material)
	clear(material)
	indexKey := hex.EncodeToString(digest)
	clear(digest)
	return indexKey, nil
}

func NormalizeURL(value, base string) (string, error) {
	if value == "" {
		return "", errors.New("URL is empty")
	}
	ref, err := url.Parse(value)
	if err != nil {
		return "", errors.New("URL cannot be parsed")
	}
	if base != "" {
		baseURL, parseErr := url.Parse(base)
		if parseErr != nil {
			return "", errors.New("base URL cannot be parsed")
		}
		ref = baseURL.ResolveReference(ref)
	}
	ref.Scheme = strings.ToLower(ref.Scheme)
	host, hostErr := caido.NormalizeRequestHost(strings.ToLower(ref.Hostname()))
	if ref.Scheme != "http" && ref.Scheme != "https" || hostErr != nil || ref.User != nil {
		return "", errors.New("URL is not an allowed HTTP endpoint")
	}
	port := ref.Port()
	if port != "" && !((ref.Scheme == "http" && port == "80") || (ref.Scheme == "https" && port == "443")) {
		ref.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		ref.Host = "[" + host + "]"
	} else {
		ref.Host = host
	}
	if ref.Path == "" {
		ref.Path = "/"
	}
	ref.Fragment = ""
	ref.RawFragment = ""
	return ref.String(), nil
}

func EndpointFromURL(value, base string) (string, int, string, error) {
	ref, err := url.Parse(value)
	if err != nil {
		return "", 0, "", errors.New("URL cannot be parsed")
	}
	if base != "" {
		baseURL, parseErr := url.Parse(base)
		if parseErr != nil {
			return "", 0, "", errors.New("base URL cannot be parsed")
		}
		ref = baseURL.ResolveReference(ref)
	}
	scheme := strings.ToLower(ref.Scheme)
	host := strings.ToLower(ref.Hostname())
	normalizedHost, hostErr := caido.NormalizeRequestHost(host)
	if (scheme != "http" && scheme != "https") || hostErr != nil || ref.User != nil {
		return "", 0, "", errors.New("URL is not a valid HTTP endpoint")
	}
	host = normalizedHost
	port := 80
	if scheme == "https" {
		port = 443
	}
	if ref.Port() != "" {
		parsedPort, parseErr := parsePort(ref.Port())
		if parseErr != nil || parsedPort < 1 || parsedPort > 65535 {
			return "", 0, "", errors.New("URL port is invalid")
		}
		port = parsedPort
	}
	return host, port, RouteTemplate(ref.Path), nil
}

func hmacDigest(key, value []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return mac.Sum(nil)
}

func truncationText(value *bool) string {
	if value == nil {
		return "None"
	}
	if *value {
		return "True"
	}
	return "False"
}

type headerField struct{ name, value string }

func requestLine(raw []byte) (string, string) {
	line := firstLine(raw, 4096)
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return "", ""
	}
	method := strings.ToUpper(parts[0])
	if !httpMethodPattern.MatchString(method) {
		method = ""
	}
	return method, parts[1]
}

func splitMessage(raw []byte) ([]headerField, []byte, bool, bool) {
	index := bytes.Index(raw, []byte("\r\n\r\n"))
	delim := 4
	if index < 0 {
		index = bytes.Index(raw, []byte("\n\n"))
		delim = 2
	}
	if index < 0 {
		return nil, nil, false, false
	}
	lines := bytes.Split(raw[:index], []byte("\n"))
	complete := len(lines) <= 257
	if len(lines) > 257 {
		lines = lines[:257]
	}
	pseudoHeadersAllowed := supportsPseudoHeaders(string(lines[0]))
	headers := make([]headerField, 0, len(lines)-1)
	for _, line := range lines[1:] {
		line = bytes.TrimSuffix(line, []byte("\r"))
		separator := bytes.IndexByte(line, ':')
		if separator < 0 {
			complete = false
			continue
		}
		if separator == 0 {
			pseudoSeparator := bytes.IndexByte(line[1:], ':')
			if pseudoSeparator < 0 {
				complete = false
				continue
			}
			separator = pseudoSeparator + 1
		}
		name, value := line[:separator], line[separator+1:]
		trimmedName := bytes.TrimSpace(name)
		if !bytes.Equal(name, trimmedName) {
			complete = false
		}
		name = trimmedName
		nameText := strings.ToLower(string(name))
		validHeaderName := headerNamePattern.MatchString(nameText)
		validPseudoHeaderName := pseudoHeadersAllowed && pseudoHeaderNamePattern.MatchString(nameText)
		if !validHeaderName && !validPseudoHeaderName {
			complete = false
			continue
		}
		headers = append(headers, headerField{name: nameText, value: strings.TrimSpace(string(value))})
	}
	return headers, raw[index+delim:], true, complete
}

func supportsPseudoHeaders(messageLine string) bool {
	for _, field := range strings.Fields(messageLine) {
		if field == "HTTP/2" || field == "HTTP/3" {
			return true
		}
	}
	return false
}

func contentType(headers []headerField) string {
	for _, header := range headers {
		if header.name != "content-type" {
			continue
		}
		mediaType, _, err := mime.ParseMediaType(header.value)
		if err != nil || !regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`).MatchString(strings.ToLower(mediaType)) {
			return "unknown"
		}
		return safeMediaType(strings.ToLower(mediaType))
	}
	return "unknown"
}

func safeMediaType(mediaType string) string {
	if isCanonicalMediaType(mediaType) {
		return mediaType
	}
	if strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json") {
		return "application/*+json"
	}
	return "unknown"
}

func isCanonicalMediaType(mediaType string) bool {
	switch mediaType {
	case "application/json", "application/x-www-form-urlencoded", "multipart/form-data",
		"text/html", "application/xhtml+xml", "text/plain", "application/octet-stream",
		"image/png", "image/jpeg", "application/pdf", "application/*+json", "unknown":
		return true
	default:
		return false
	}
}

func responseStatus(raw []byte) *int {
	line := firstLine(raw, 256)
	parts := strings.Fields(line)
	if len(parts) < 2 || !strings.HasPrefix(strings.ToUpper(parts[0]), "HTTP/") || len(parts[1]) != 3 {
		return nil
	}
	status := 0
	for _, r := range parts[1] {
		if r < '0' || r > '9' {
			return nil
		}
		status = status*10 + int(r-'0')
	}
	if status < 100 || status > 599 {
		return nil
	}
	return &status
}

func bodyFieldPaths(body []byte, mediaType string, headers []headerField) (map[string]struct{}, bool) {
	fields := make(map[string]struct{})
	if len(body) > maxParsedBodyBytes {
		return fields, false
	}
	mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		charset, supported := historicalBodyCharset(headers)
		if !supported {
			return fields, false
		}
		decoded, valid := historicalDecodeBody(body, charset)
		if !valid {
			return fields, false
		}
		analysis := historicalHTMLProjectionFields(decoded, "body", historicalMaxParameterFields)
		for _, field := range analysis.fields {
			fields[field.path] = struct{}{}
		}
		return fields, analysis.complete && !historicalConflictingContentType(headers)
	}
	if mediaType == "application/x-www-form-urlencoded" {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return fields, false
		}
		for key := range values {
			fields[safeField(key)] = struct{}{}
		}
		return fields, true
	}
	if mediaType == "multipart/form-data" {
		parsedType, parameters, err := mime.ParseMediaType(historicalContentTypeHeader(headers))
		if err != nil || strings.ToLower(parsedType) != "multipart/form-data" {
			return fields, false
		}
		parts, complete := parseHistoricalMultipart(body, parameters["boundary"], historicalMaxParameterFields)
		for _, part := range parts {
			fields[safeField(part.name)] = struct{}{}
		}
		return fields, complete && !historicalConflictingContentType(headers)
	}
	if mediaType != "application/json" && !(strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json")) {
		return fields, len(body) == 0
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return fields, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fields, false
	}
	type entry struct {
		value any
		path  string
		depth int
	}
	pending := []entry{{value: document, path: "$", depth: 0}}
	count := 0
	for len(pending) > 0 {
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		if current.depth > 32 || count > 5000 {
			return fields, false
		}
		switch value := current.value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(value))
			for key := range value {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				count++
				path := current.path + "." + safeField(key)
				if len(path) > 512 {
					path = path[:512]
				}
				fields[path] = struct{}{}
				pending = append(pending, entry{value: value[key], path: path, depth: current.depth + 1})
			}
		case []any:
			count += len(value)
			if count > 5000 {
				return fields, false
			}
			for _, child := range value {
				pending = append(pending, entry{value: child, path: current.path + "[]", depth: current.depth + 1})
			}
		}
	}
	return fields, true
}

func safeField(name string) string {
	if fieldNamePattern.MatchString(name) && !secretNamePattern.MatchString(name) {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "field_" + hex.EncodeToString(sum[:])[:16]
}

func firstLine(raw []byte, max int) string {
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	if len(line) > max {
		line = line[:max]
	}
	return strings.TrimSuffix(string(line), "\r")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func isExplicitFalse(value *bool) bool { return value != nil && !*value }

func sortedKeys(values map[string][]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func mapKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func parsePort(value string) (int, error) {
	var port int
	if value == "" {
		return 0, errors.New("empty port")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid port")
		}
		port = port*10 + int(r-'0')
		if port > 65535 {
			return 0, errors.New("invalid port")
		}
	}
	return port, nil
}

// Describe is a small diagnostic helper that returns only non-sensitive graph
// identity and parser coverage, never request or response content.
func Describe(p ExchangeProjection) string {
	return fmt.Sprintf("observation=%s parse=%s route=%s", p.ObservationID, p.ParseState, p.Search.Route)
}
