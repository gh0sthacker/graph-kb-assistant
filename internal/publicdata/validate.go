// Package publicdata validates graph metadata before it is persisted outside
// the encrypted historical capture vault or copied into model-visible state.
package publicdata

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	strongSensitiveValueMarkers       = [...]string{"password", "passwd", "api_key", "api-key", "apikey", "access_token", "access-token", "accesstoken", "refresh_token", "refresh-token", "refreshtoken", "csrf_token", "csrf-token", "csrftoken", "session_id", "session-id", "session_cookie", "session-cookie", "session_key", "session-key", "secret_key", "secret-key", "authorization", "jwt", "phone", "mobile", "ssn", "credential", "private_key", "private-key", "privatekey", "private key", "bearer", "eyj", "-----begin"}
	weakSensitiveAssignmentPattern    = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:session|token|cookie|secret|email)(?:[_-]?(?:id|cookie|key))?["']?\s*[=:]\s*\S`)
	credentialAlphabetPattern         = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)
	bareKeyProseValuePattern          = regexp.MustCompile(`^[A-Za-z]+(?:[._-][A-Za-z]+)+$`)
	hexCredentialPattern              = regexp.MustCompile(`^[A-Fa-f0-9]{16,}$`)
	authorizationSchemePattern        = regexp.MustCompile(`(?i)^(?:bearer|basic|digest|negotiate)(?:\s|$)`)
	secretAssignmentPattern           = regexp.MustCompile(`(?i)\b([A-Za-z0-9_-]*(?:password|passwd|api[_-]?key|access[_-]?token|refresh[_-]?token|csrf[_-]?token|session(?:[_-]?(?:id|cookie|key))?|cookie|secret(?:[_-]?key)?|authorization|token|jwt))\b["']?\s*[=:]\s*("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s&;,"']+)`)
	jsonSecretAssignmentPattern       = regexp.MustCompile(`(?i)"([A-Za-z0-9_-]*(?:password|passwd|api[_-]?key|access[_-]?token|refresh[_-]?token|csrf[_-]?token|session(?:[_-]?(?:id|cookie|key))?|cookie|secret(?:[_-]?key)?|authorization|token|jwt))"\s*:\s*("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^,\s}\]]+)`)
	sensitivePIIAssignmentPattern     = regexp.MustCompile(`(?i)\b[A-Za-z0-9_-]*(email|phone|mobile|ssn|credential|private[_-]?key)\b["']?\s*[=:]\s*("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s&;,"']+)`)
	jsonSensitivePIIAssignmentPattern = regexp.MustCompile(`(?i)"[A-Za-z0-9_-]*(email|phone|mobile|ssn|credential|private[_-]?key)"\s*:\s*("(?:\\.|[^"\\])*"|[^,\s}\]]+)`)
	secretOtherValuePattern           = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}|\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b|-----BEGIN [A-Z ]*PRIVATE KEY-----|\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	sensitiveHeaderPattern            = regexp.MustCompile(`(?i)(authorization|cookie|token|secret|password|passwd|credential|api[_-]?key|session|csrf|xsrf)`)
	headerFieldPattern                = regexp.MustCompile(`(?im)^([A-Za-z0-9-]{1,128}):[ 	]*([^\r\n]*)`)
	privateKeyBlockPattern            = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
	privateKeys                       = map[string]struct{}{
		"raw": {}, "body": {}, "headers": {}, "header_values": {}, "request_body": {},
		"response_body": {}, "field_values": {}, "token": {}, "session_id": {},
		"session_cookie": {}, "credential": {}, "credentials": {}, "pii": {},
		"authorization": {}, "cookie": {}, "set_cookie": {}, "password": {},
		"passwd": {}, "access_token": {}, "refresh_token": {}, "csrf_token": {},
		"api_key": {}, "apikey": {}, "private_key": {}, "raw_request": {},
		"raw_response": {}, "private_oracle": {}, "ground_truth": {},
		"known_vulnerabilities": {}, "session_store": {},
	}
)

// Validate converts a Go value to JSON and validates the JSON representation.
// This also handles defined map/slice types without coupling callers to them.
func Validate(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return errors.New("public data could not be encoded")
	}
	return ValidateJSON(encoded)
}

// ValidateJSON rejects duplicate JSON keys and private or secret-like graph
// metadata before the bytes are persisted or exposed to another component.
func ValidateJSON(encoded []byte) error {
	if len(encoded) == 0 || !utf8.Valid(encoded) {
		return errors.New("public data JSON is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return errors.New("public data JSON is malformed")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("public data JSON has trailing content")
	}
	if err := walk(value, ""); err != nil {
		return err
	}
	return nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("public data JSON nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("public data object key is invalid")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, errors.New("public data has a duplicate key")
			}
			child, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
			return nil, errors.New("public data object is unterminated")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			child, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') {
			return nil, errors.New("public data array is unterminated")
		}
		return array, nil
	default:
		return nil, errors.New("public data JSON delimiter is invalid")
	}
}

func walk(value any, parent string) error {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			normalized := strings.ReplaceAll(strings.ToLower(key), "-", "_")
			if _, private := privateKeys[normalized]; private && !emptyOrRedacted(child) {
				return errors.New("public data contains a private field")
			}
			if (parent == "fields" || parent == "header_metadata") &&
				(normalized == "value" || normalized == "values") && !emptyOrRedacted(child) {
				return errors.New("public data contains a private field value")
			}
			if isBareSensitiveKey(normalized) {
				if text, ok := child.(string); ok && !emptyOrRedacted(child) && isSensitiveAssignmentValue(normalized, text) {
					return errors.New("public data contains a secret-like value")
				}
			}
			if err := walk(child, key); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range node {
			if err := walk(child, parent); err != nil {
				return err
			}
		}
	case string:
		if containsSecretLikeValue(node) {
			return errors.New("public data contains a secret-like value")
		}
		decoded := node
		for range 3 {
			if unescaped, err := url.QueryUnescape(decoded); err == nil {
				decoded = unescaped
			}
		}
		if containsSecretLikeValue(decoded) {
			return errors.New("public data contains a secret-like value")
		}
		if strings.HasPrefix(decoded, "http://") || strings.HasPrefix(decoded, "https://") {
			parsed, err := url.Parse(decoded)
			if err != nil || parsed.User != nil {
				return errors.New("public data contains an unsafe URL")
			}
			for _, values := range parsed.Query() {
				for _, item := range values {
					if item != "" && item != "[REDACTED]" && item != "<REDACTED>" {
						return errors.New("public data contains an unsafe URL")
					}
				}
			}
		}
	}
	return nil
}

func emptyOrRedacted(value any) bool {
	if value == nil {
		return true
	}
	stringValue, ok := value.(string)
	return ok && (stringValue == "" || stringValue == "[REDACTED]" || stringValue == "<REDACTED>")
}

func containsSecretLikeValue(value string) bool {
	// Most graph strings are routes, names, opaque IDs, or bounded shape
	// metadata. Run the backtracking assignment detectors only when a strong
	// marker, an assignment-position weak marker, or a known credential pattern
	// is present. This keeps validation linear for large safe strings.
	if !hasSensitiveValueMarker(value) {
		return false
	}
	for _, match := range headerFieldPattern.FindAllStringSubmatchIndex(value, -1) {
		if len(match) < 6 || match[2] < 0 || match[3] < 0 || match[4] < 0 || match[5] < 0 {
			continue
		}
		sensitiveName := value[match[2]:match[3]]
		headerValue := strings.TrimSpace(value[match[4]:match[5]])
		if !sensitiveHeaderPattern.MatchString(sensitiveName) || !isSensitiveHeaderValue(sensitiveName, headerValue) {
			continue
		}
		if headerValue != "" && !strings.EqualFold(headerValue, "[REDACTED]") && !strings.EqualFold(headerValue, "<REDACTED>") {
			return true
		}
	}
	if secretOtherValuePattern.MatchString(value) {
		return true
	}
	for _, item := range []struct {
		pattern        *regexp.Regexp
		keyFrom        int
		keyTo          int
		valueFrom      int
		valueTo        int
		checkBareValue bool
	}{
		{secretAssignmentPattern, 2, 3, 4, 5, true}, {jsonSecretAssignmentPattern, 2, 3, 4, 5, true},
		{sensitivePIIAssignmentPattern, 2, 3, 4, 5, false}, {jsonSensitivePIIAssignmentPattern, 2, 3, 4, 5, false},
	} {
		for _, match := range item.pattern.FindAllStringSubmatchIndex(value, -1) {
			if len(match) <= item.valueTo || match[item.keyFrom] < 0 || match[item.keyTo] < 0 || match[item.valueFrom] < 0 || match[item.valueTo] < 0 {
				return true
			}
			key := value[match[item.keyFrom]:match[item.keyTo]]
			assignmentValue := strings.Trim(value[match[item.valueFrom]:match[item.valueTo]], `"'`)
			for range 3 {
				decoded, err := url.QueryUnescape(assignmentValue)
				if err != nil || decoded == assignmentValue {
					break
				}
				assignmentValue = decoded
			}
			if item.checkBareValue && !isSensitiveAssignmentValue(key, assignmentValue) {
				continue
			}
			if !strings.EqualFold(assignmentValue, "[REDACTED]") && !strings.EqualFold(assignmentValue, "<REDACTED>") {
				return true
			}
		}
	}
	return false
}

func hasSensitiveValueMarker(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range strongSensitiveValueMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if weakSensitiveAssignmentPattern.MatchString(value) || secretOtherValuePattern.MatchString(value) {
		return true
	}
	// regexp's case-insensitive matching also treats a few non-ASCII runes as
	// simple folds of ASCII letters. Preserve that conservative behavior when a
	// string contains non-ASCII bytes instead of using the ASCII marker gate.
	for index := 0; index < len(value); index++ {
		if value[index] >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

func isSensitiveAssignmentValue(key, value string) bool {
	normalizedKey := strings.ReplaceAll(strings.ToLower(key), "-", "_")
	if !isBareSensitiveKey(normalizedKey) {
		return true
	}
	value = strings.TrimSpace(strings.Trim(value, `"'`))
	if normalizedKey == "authorization" && authorizationSchemePattern.MatchString(value) {
		return true
	}
	return looksLikeCredential(value) || isOpaqueBareKeyValue(value)
}

// isBareSensitiveKey reports whether a normalized key is one of the short
// sensitive names whose value may still be ordinary prose.
func isBareSensitiveKey(normalizedKey string) bool {
	switch normalizedKey {
	case "session", "token", "cookie", "jwt", "secret", "authorization":
		return true
	default:
		return false
	}
}

// isSensitiveHeaderValue keeps the header rule strict, except for header names
// that are a bare sensitive key: prose such as "session: review_later" reaches
// this shape, so the value must look like credential material to qualify.
func isSensitiveHeaderValue(name, value string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
	if !isBareSensitiveKey(normalized) {
		return true
	}
	return isSensitiveAssignmentValue(normalized, value)
}

// isOpaqueBareKeyValue catches short or low-variety values under a bare
// sensitive key that are still credential material: anything twelve or more
// bytes long, anything containing a digit, or anything using characters
// outside the word alphabet of letters, underscore, hyphen, and dot. Values
// made of plain words joined by separators ("review_later",
// "verify-consultation") keep their human-readable shape and are not treated as
// secrets on length alone.
func isOpaqueBareKeyValue(value string) bool {
	value = strings.TrimSpace(strings.Trim(value, `"'`))
	if value == "" || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return false
	}
	var digit, foreign bool
	for index := 0; index < len(value); index++ {
		switch character := value[index]; {
		case character >= '0' && character <= '9':
			digit = true
		case character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z', character == '_', character == '-', character == '.':
		default:
			foreign = true
		}
	}
	if digit || foreign {
		return true
	}
	return len(value) >= 12 && !bareKeyProseValuePattern.MatchString(value)
}

func looksLikeCredential(value string) bool {
	value = strings.TrimSpace(strings.Trim(value, `"'`))
	if len(value) < 12 || strings.IndexFunc(value, unicode.IsSpace) >= 0 || !credentialAlphabetPattern.MatchString(value) {
		return false
	}
	if hexCredentialPattern.MatchString(value) {
		return true
	}
	frequencies := make(map[rune]int)
	var lower, upper, digit, other bool
	for _, character := range value {
		frequencies[character]++
		switch {
		case character >= 'a' && character <= 'z':
			lower = true
		case character >= 'A' && character <= 'Z':
			upper = true
		case character >= '0' && character <= '9':
			digit = true
		default:
			other = true
		}
	}
	if len(frequencies) < 6 {
		return false
	}
	entropy := 0.0
	for _, count := range frequencies {
		probability := float64(count) / float64(len(value))
		entropy -= probability * math.Log2(probability)
	}
	classes := 0
	for _, present := range []bool{lower, upper, digit, other} {
		if present {
			classes++
		}
	}
	return entropy >= 3.2 && classes >= 2
}
