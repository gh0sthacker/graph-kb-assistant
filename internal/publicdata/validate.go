// Package publicdata validates graph metadata before it is persisted outside
// the encrypted historical capture vault or copied into model-visible state.
package publicdata

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	secretAssignmentPattern           = regexp.MustCompile(`(?i)\b([A-Za-z0-9_-]*(?:password|passwd|api[_-]?key|access[_-]?token|refresh[_-]?token|csrf[_-]?token|session(?:[_-]?(?:id|cookie|key))?|cookie|secret(?:[_-]?key)?|authorization|token|jwt))\b["']?\s*[=:]\s*("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s&;,"']+)`)
	jsonSecretAssignmentPattern       = regexp.MustCompile(`(?i)"([A-Za-z0-9_-]*(?:password|passwd|api[_-]?key|access[_-]?token|refresh[_-]?token|csrf[_-]?token|session(?:[_-]?(?:id|cookie|key))?|cookie|secret(?:[_-]?key)?|authorization|token|jwt))"\s*:\s*("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^,\s}\]]+)`)
	sensitivePIIAssignmentPattern     = regexp.MustCompile(`(?i)\b[A-Za-z0-9_-]*(email|phone|mobile|ssn|credential|private[_-]?key)\b["']?\s*[=:]\s*("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s&;,"']+)`)
	jsonSensitivePIIAssignmentPattern = regexp.MustCompile(`(?i)"[A-Za-z0-9_-]*(email|phone|mobile|ssn|credential|private[_-]?key)"\s*:\s*("(?:\\.|[^"\\])*"|[^,\s}\]]+)`)
	secretOtherValuePattern           = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}|\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b|-----BEGIN [A-Z ]*PRIVATE KEY-----|\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	sensitiveHeaderPattern            = regexp.MustCompile(`(?i)(authorization|cookie|token|secret|password|passwd|credential|api[_-]?key|session|csrf|xsrf)`)
	headerFieldPattern                = regexp.MustCompile(`(?im)^([A-Za-z0-9-]{1,128}):[ \t]*([^\r\n]*)`)
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
	if secretOtherValuePattern.MatchString(value) {
		return true
	}
	for _, item := range []struct {
		pattern   *regexp.Regexp
		valueFrom int
		valueTo   int
	}{
		{secretAssignmentPattern, 4, 5}, {jsonSecretAssignmentPattern, 4, 5},
		{sensitivePIIAssignmentPattern, 4, 5}, {jsonSensitivePIIAssignmentPattern, 4, 5},
	} {
		for _, match := range item.pattern.FindAllStringSubmatchIndex(value, -1) {
			if len(match) <= item.valueTo || match[item.valueFrom] < 0 || match[item.valueTo] < 0 {
				return true
			}
			assignmentValue := strings.Trim(value[match[item.valueFrom]:match[item.valueTo]], `"'`)
			for range 3 {
				decoded, err := url.QueryUnescape(assignmentValue)
				if err != nil || decoded == assignmentValue {
					break
				}
				assignmentValue = decoded
			}
			if !strings.EqualFold(assignmentValue, "[REDACTED]") && !strings.EqualFold(assignmentValue, "<REDACTED>") {
				return true
			}
		}
	}
	return false
}
