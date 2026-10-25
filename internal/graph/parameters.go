package graph

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	historicalMaxParameterFields = 2_000
	historicalMaxJSONDepth       = 64
	historicalMaxFormPairs       = historicalMaxParameterFields + 1
	historicalMaxParameterPath   = 4_096
)

var historicalArrayIndexPattern = regexp.MustCompile(`\[[0-9]+\]`)

type historicalParameterField struct {
	path       string
	typeName   string
	location   string
	metadata   Record
	occurrence *historicalFieldOccurrence
}

type historicalFieldOccurrence struct {
	index              int
	sourceSpan         Record
	valueSpan          Record
	valueToken         string
	valueTokenVersion  string
	valueNormalization string
}

type historicalBodyAnalysis struct {
	fields          []historicalParameterField
	html            Record
	characteristics Record
	complete        bool
}

var errHistoricalBodyFieldLimit = errors.New("historical body field limit exceeded")

func historicalMessageBodyFields(raw []byte, location string, source Record, remaining int, truncated *bool, fieldValueKey []byte, revisionBound bool, wireFidelity string) historicalBodyAnalysis {
	headers, body, boundary, headersComplete := splitMessage(raw)
	if !boundary {
		return historicalBodyAnalysis{}
	}
	if len(body) > maxParsedBodyBytes {
		return historicalBodyAnalysis{}
	}
	bodyOffset, offsetAvailable := historicalMessageBodyOffset(raw)
	if !offsetAvailable {
		return historicalBodyAnalysis{}
	}
	originalBody := body
	contentType := historicalContentTypeHeader(headers)
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if mediaType != "multipart/form-data" && len(body) > 0 {
		charset, supported := historicalBodyCharset(headers)
		if !supported {
			return historicalBodyAnalysis{}
		}
		var valid bool
		body, valid = historicalDecodeBody(body, charset)
		if !valid {
			return historicalBodyAnalysis{}
		}
	}
	spanAvailable := revisionBound && bodyOffset <= (1<<30)-len(body) && headersComplete && !historicalConflictingContentType(headers) &&
		historicalBodyExtentComplete(headers, body) && isExplicitFalse(truncated) && bytes.Equal(originalBody, body)
	analysis := historicalBodyParameterFields(body, contentType, location, source, remaining,
		bodyOffset, spanAvailable, fieldValueKey, wireFidelity)
	analysis.complete = analysis.complete && headersComplete && !historicalConflictingContentType(headers) &&
		historicalBodyExtentComplete(headers, body) && isExplicitFalse(truncated)
	return analysis
}

func historicalMessageBodyOffset(raw []byte) (int, bool) {
	if index := bytes.Index(raw, []byte("\r\n\r\n")); index >= 0 {
		return index + 4, true
	}
	if index := bytes.Index(raw, []byte("\n\n")); index >= 0 {
		return index + 2, true
	}
	return 0, false
}

func historicalMessageOffsetBasis(location string) string {
	switch location {
	case "request.body":
		return "request_message"
	case "response.body":
		return "response_message"
	default:
		return ""
	}
}

func historicalBodyExtentComplete(headers []headerField, body []byte) bool {
	contentLengthSeen := false
	for _, header := range headers {
		switch header.name {
		case "transfer-encoding":
			return false
		case "content-length":
			if contentLengthSeen {
				return false
			}
			contentLengthSeen = true
			contentLength, err := strconv.ParseInt(strings.TrimSpace(header.value), 10, 64)
			if err != nil || contentLength < 0 || contentLength != int64(len(body)) {
				return false
			}
		}
	}
	return true
}

func historicalConflictingContentType(headers []headerField) bool {
	var first string
	seen := false
	for _, header := range headers {
		if header.name != "content-type" {
			continue
		}
		value := strings.TrimSpace(header.value)
		if !seen {
			first = value
			seen = true
			continue
		}
		if value != first {
			return true
		}
	}
	return false
}

func historicalBodyCharset(headers []headerField) (string, bool) {
	for _, header := range headers {
		if header.name != "content-type" {
			continue
		}
		_, parameters, err := mime.ParseMediaType(header.value)
		if err != nil {
			return "", false
		}
		charset := strings.ToLower(strings.TrimSpace(parameters["charset"]))
		switch charset {
		case "", "utf-8", "utf8":
			return "utf-8", true
		case "us-ascii", "ascii":
			return "ascii", true
		case "iso-8859-1", "latin-1":
			return "latin-1", true
		default:
			return "", false
		}
	}
	return "utf-8", true
}

func historicalContentTypeHeader(headers []headerField) string {
	for _, header := range headers {
		if header.name == "content-type" {
			return strings.TrimSpace(header.value)
		}
	}
	return ""
}

func historicalDecodeBody(body []byte, charset string) ([]byte, bool) {
	switch charset {
	case "utf-8":
		return body, utf8.Valid(body)
	case "ascii":
		for _, value := range body {
			if value > 0x7f {
				return nil, false
			}
		}
		return body, true
	case "latin-1":
		var decoded strings.Builder
		decoded.Grow(len(body) * 2)
		for _, value := range body {
			decoded.WriteRune(rune(value))
		}
		return []byte(decoded.String()), true
	default:
		return nil, false
	}
}

func historicalBodyParameterFields(body []byte, contentType, location string, source Record, remaining, bodyOffset int, sourceSpanAvailable bool, fieldValueKey []byte, wireFidelity string) historicalBodyAnalysis {
	if remaining < 0 {
		return historicalBodyAnalysis{}
	}
	if len(body) == 0 {
		return historicalBodyAnalysis{complete: true}
	}
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		return historicalBodyAnalysis{}
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case mediaType == "application/x-www-form-urlencoded":
		fields, complete := historicalFormParameterFields(body, location, source, remaining,
			bodyOffset, sourceSpanAvailable, fieldValueKey, wireFidelity)
		return historicalBodyAnalysis{fields: fields, complete: complete}
	case mediaType == "application/json", strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json"):
		fields, complete := historicalJSONParameterFields(body, location, source, remaining,
			bodyOffset, sourceSpanAvailable, fieldValueKey, wireFidelity)
		return historicalBodyAnalysis{fields: fields, complete: complete}
	case mediaType == "text/html", mediaType == "application/xhtml+xml":
		htmlAnalysis := historicalHTMLBodyFields(body, location, source, remaining)
		return historicalBodyAnalysis{
			fields: htmlAnalysis.fields, html: htmlAnalysis.metadata,
			characteristics: htmlAnalysis.characteristics, complete: htmlAnalysis.complete,
		}
	case mediaType == "multipart/form-data":
		fields, complete := historicalMultipartParameterFields(body, parameters["boundary"], location, source, remaining)
		return historicalBodyAnalysis{fields: fields, complete: complete}
	default:
		return historicalBodyAnalysis{}
	}
}

type historicalMultipartField struct {
	name        string
	index       int
	typeName    string
	bytes       *int64
	contentType string
	filename    bool
}

func historicalMultipartParameterFields(body []byte, boundary, location string, source Record, remaining int) ([]historicalParameterField, bool) {
	parts, complete := parseHistoricalMultipart(body, boundary, remaining)
	fields := make([]historicalParameterField, 0, len(parts))
	occurrences := make(map[string][]int)
	maskedNames := make(map[string]string)
	for _, part := range parts {
		partLocation := location + ".part[" + strconv.Itoa(part.index) + "]"
		name, err := historicalSafeMultipartFieldName(part.name, source, location, maskedNames)
		if err != nil {
			complete = false
			continue
		}
		occurrence := len(occurrences[name])
		occurrences[name] = append(occurrences[name], len(fields))
		path := location + "[" + strconv.Quote(name) + "][" + strconv.Itoa(occurrence) + "]"
		metadata := Record{"bytes": nil, "content_type": part.contentType}
		if part.bytes != nil {
			metadata["bytes"] = *part.bytes
		}
		if part.filename {
			filename, labelErr := historicalOpaqueLabel(source, partLocation+".filename", "file")
			if labelErr != nil {
				complete = false
				continue
			}
			metadata["filename"] = filename
		}
		fields = append(fields, historicalParameterField{
			path: path, typeName: part.typeName,
			location: location, metadata: metadata,
		})
	}
	for _, indices := range occurrences {
		if len(indices) == 1 {
			index := indices[0]
			fields[index].path = historicalTemplateParameterPath(fields[index].path)
		}
	}
	return fields, complete
}

func parseHistoricalMultipart(body []byte, boundary string, maxFields int) ([]historicalMultipartField, bool) {
	if boundary == "" || len(boundary) > 200 || maxFields < 0 || maxFields > historicalMaxParameterFields {
		return nil, false
	}
	fields := make([]historicalMultipartField, 0, minInt(maxFields, 32))
	complete := true
	totalDecodedBytes := int64(0)
	partIndex := 0
	visitedParts := 0
	var walk func(*multipart.Reader, int) bool
	walk = func(reader *multipart.Reader, depth int) bool {
		if depth > 16 {
			complete = false
			return false
		}
		for {
			part, err := reader.NextRawPart()
			if err == io.EOF {
				return true
			}
			if err != nil {
				complete = false
				return false
			}
			if visitedParts >= maxFields {
				complete = false
				return false
			}
			visitedParts++

			partContentType := "text/plain"
			if values := part.Header.Values("Content-Type"); len(values) > 1 {
				complete = false
			} else if len(values) == 1 {
				parsed, _, parseErr := mime.ParseMediaType(values[0])
				if parseErr != nil {
					complete = false
					partContentType = "unknown"
				} else {
					partContentType = strings.ToLower(parsed)
				}
			}
			if values := part.Header.Values("Content-Transfer-Encoding"); len(values) > 1 {
				complete = false
			}
			transferEncoding := ""
			if values := part.Header.Values("Content-Transfer-Encoding"); len(values) > 0 {
				transferEncoding = strings.ToLower(strings.TrimSpace(values[0]))
			}

			if strings.HasPrefix(strings.ToLower(partContentType), "multipart/") {
				_, parameters, parseErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
				if parseErr != nil || parameters["boundary"] == "" || !multipartTransferEncodingSupported(transferEncoding) {
					complete = false
					if _, drainErr := io.Copy(io.Discard, io.LimitReader(part, int64(maxParsedBodyBytes)+1)); drainErr != nil {
						complete = false
					}
					continue
				}
				if !walk(multipart.NewReader(part, parameters["boundary"]), depth+1) {
					complete = false
				}
				continue
			}

			if partIndex >= maxFields {
				complete = false
				return false
			}
			index := partIndex
			partIndex++
			name := ""
			hasFilename := false
			dispositions := part.Header.Values("Content-Disposition")
			if len(dispositions) != 1 {
				complete = false
			} else {
				_, parameters, parseErr := mime.ParseMediaType(dispositions[0])
				if parseErr != nil {
					complete = false
				} else if value, exists := parameters["name"]; exists {
					name = value
				} else {
					complete = false
				}
				_, hasFilename = parameters["filename"]
			}
			payloadSize, payloadComplete := historicalMultipartPayloadSize(part, transferEncoding, int64(maxParsedBodyBytes)-totalDecodedBytes)
			if payloadComplete && payloadSize != nil {
				totalDecodedBytes += *payloadSize
			}
			if !payloadComplete {
				complete = false
			}
			fieldType := "string"
			if hasFilename {
				fieldType = "file"
			}
			fields = append(fields, historicalMultipartField{
				name: name, index: index, typeName: fieldType, bytes: payloadSize,
				contentType: safeMediaType(partContentType), filename: hasFilename,
			})
		}
	}
	root := multipart.NewReader(bytes.NewReader(body), boundary)
	if !walk(root, 1) {
		complete = false
	}
	return fields, complete
}

func multipartTransferEncodingSupported(value string) bool {
	switch value {
	case "", "7bit", "8bit", "binary":
		return true
	default:
		return false
	}
}

func historicalMultipartPayloadSize(part io.Reader, transferEncoding string, remaining int64) (*int64, bool) {
	if remaining < 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(part, int64(maxParsedBodyBytes)+1))
		return nil, false
	}
	var decoded io.Reader
	switch transferEncoding {
	case "", "7bit", "8bit", "binary":
		decoded = part
	case "base64":
		decoded = base64.NewDecoder(base64.StdEncoding, part)
	case "quoted-printable":
		decoded = quotedprintable.NewReader(part)
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(part, int64(maxParsedBodyBytes)+1))
		return nil, false
	}
	limit := remaining
	if limit > int64(maxParsedBodyBytes) {
		limit = int64(maxParsedBodyBytes)
	}
	count, err := io.Copy(io.Discard, io.LimitReader(decoded, limit+1))
	if err != nil || count > limit {
		return nil, false
	}
	return &count, true
}

func historicalFormParameterFields(body []byte, location string, source Record, remaining, bodyOffset int, sourceSpanAvailable bool, fieldValueKey []byte, wireFidelity string) ([]historicalParameterField, bool) {
	if !utf8.Valid(body) {
		return nil, false
	}
	if bytes.Count(body, []byte("&")) >= historicalMaxFormPairs {
		return nil, false
	}
	pairs := strings.Split(string(body), "&")
	fields := make([]historicalParameterField, 0, minInt(remaining, len(pairs)))
	occurrences := make(map[string][]int)
	fieldIndex := 0
	pairOffset := 0
	for _, pair := range pairs {
		if pair == "" {
			pairOffset++
			continue
		}
		index := fieldIndex
		fieldIndex++
		key, value, hasValue := strings.Cut(pair, "=")
		if !historicalValidFormEscapes(key) || hasValue && !historicalValidFormEscapes(value) {
			return nil, false
		}
		name, valid := historicalURLUnquote(key, true, true)
		if !valid {
			return nil, false
		}
		if hasValue {
			if _, valid = historicalURLUnquote(value, true, true); !valid {
				return nil, false
			}
		}
		safeName, err := historicalSafeFieldName(name, source, location+".key["+strconv.Itoa(index)+"]")
		if err != nil {
			return nil, false
		}
		if len(fields) >= remaining {
			return finalizeHistoricalFormOccurrences(fields, occurrences), false
		}
		occurrence := len(occurrences[safeName])
		occurrences[safeName] = append(occurrences[safeName], len(fields))
		path := location + "[\"" + safeName + "\"][" + strconv.Itoa(occurrence) + "]"
		fieldOccurrence := &historicalFieldOccurrence{index: occurrence}
		if sourceSpanAvailable {
			fieldOccurrence.sourceSpan = Record{
				"start": bodyOffset + pairOffset, "end": bodyOffset + pairOffset + len(pair),
				"unit": "byte", "offset_basis": historicalMessageOffsetBasis(location), "wire_fidelity": wireFidelity,
			}
			if hasValue {
				valueStart := bodyOffset + pairOffset + len(key) + 1
				fieldOccurrence.valueSpan = Record{
					"start": valueStart, "end": bodyOffset + pairOffset + len(pair),
					"unit": "byte", "offset_basis": historicalMessageOffsetBasis(location), "wire_fidelity": wireFidelity,
				}
				if len(fieldValueKey) == 32 && safeName == name && !secretNamePattern.MatchString(name) && len(value) <= 4_096 {
					decoded, decodeErr := url.QueryUnescape(value)
					if decodeErr == nil && len(decoded) > 0 && len(decoded) <= 4_096 && safeEqualityString(decoded) {
						fieldOccurrence.valueToken = fieldValueTokenForNormalization(fieldValueKey, decoded, "decoded_utf8_scalar_v2")
						fieldOccurrence.valueTokenVersion = "field-equality-v2"
						fieldOccurrence.valueNormalization = "decoded_utf8_scalar_v2"
					}
				}
			}
		}
		fields = append(fields, historicalParameterField{
			path: path, typeName: "string", location: location, occurrence: fieldOccurrence,
		})
		pairOffset += len(pair) + 1
	}
	return finalizeHistoricalFormOccurrences(fields, occurrences), true
}

func historicalValidFormEscapes(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] != '%' {
			continue
		}
		if index+2 >= len(value) || !isHexadecimalDigit(value[index+1]) || !isHexadecimalDigit(value[index+2]) {
			return false
		}
		index += 2
	}
	return true
}

// finalizeHistoricalFormOccurrences retains source-order indices for duplicate
// names and keeps the legacy wildcard path for singleton fields.
func finalizeHistoricalFormOccurrences(fields []historicalParameterField, occurrences map[string][]int) []historicalParameterField {
	for _, indices := range occurrences {
		if len(indices) != 1 {
			for occurrenceIndex, fieldIndex := range indices {
				if fields[fieldIndex].occurrence != nil {
					fields[fieldIndex].occurrence.index = occurrenceIndex
				}
			}
			continue
		}
		index := indices[0]
		fields[index].path = historicalTemplateParameterPath(fields[index].path)
		if fields[index].occurrence != nil {
			fields[index].occurrence.index = 0
		}
	}
	return fields
}

func historicalJSONParameterFields(body []byte, location string, source Record, remaining, bodyOffset int, sourceSpanAvailable bool, fieldValueKey []byte, wireFidelity string) ([]historicalParameterField, bool) {
	if !utf8.Valid(body) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	fields := make([]historicalParameterField, 0, minInt(remaining, 64))
	var visit func(path string, depth int) error
	appendField := func(path, typeName string, occurrence *historicalFieldOccurrence) error {
		if len(path) > historicalMaxParameterPath {
			return errors.New("historical parameter path limit exceeded")
		}
		if len(fields) >= remaining {
			return errHistoricalBodyFieldLimit
		}
		fields = append(fields, historicalParameterField{
			path: path, typeName: typeName, location: location, occurrence: occurrence,
		})
		return nil
	}
	visit = func(path string, depth int) error {
		if depth > historicalMaxJSONDepth {
			return errors.New("historical JSON depth limit exceeded")
		}
		tokenStartOffset := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				if err := appendField(path, "object", nil); err != nil {
					return err
				}
				seen := make(map[string]struct{})
				for keyIndex := 0; decoder.More(); keyIndex++ {
					keyToken, err := decoder.Token()
					if err != nil {
						return err
					}
					key, ok := keyToken.(string)
					if !ok {
						return errors.New("invalid historical JSON object key")
					}
					if _, exists := seen[key]; exists {
						return errors.New("duplicate historical JSON object key")
					}
					seen[key] = struct{}{}
					safeName, err := historicalSafeFieldName(key, source, location+path+".key["+strconv.Itoa(keyIndex)+"]")
					if err != nil {
						return err
					}
					if err := visit(path+"["+strconv.Quote(safeName)+"]", depth+1); err != nil {
						return err
					}
				}
				closeToken, err := decoder.Token()
				if err != nil || closeToken != json.Delim('}') {
					return errors.New("invalid historical JSON object")
				}
			case '[':
				if err := appendField(path, "array", nil); err != nil {
					return err
				}
				for index := 0; decoder.More(); index++ {
					if err := visit(path+"["+strconv.Itoa(index)+"]", depth+1); err != nil {
						return err
					}
				}
				closeToken, err := decoder.Token()
				if err != nil || closeToken != json.Delim(']') {
					return errors.New("invalid historical JSON array")
				}
			default:
				return errors.New("invalid historical JSON delimiter")
			}
			return nil
		}
		typeName := "unknown"
		switch token.(type) {
		case nil:
			typeName = "null"
		case bool:
			typeName = "boolean"
		case json.Number:
			typeName = "number"
		case string:
			typeName = "string"
		default:
			return errors.New("invalid historical JSON value")
		}
		var occurrence *historicalFieldOccurrence
		if sourceSpanAvailable {
			start, startOK := historicalJSONTokenStart(body, tokenStartOffset)
			end := int(decoder.InputOffset())
			if !startOK || end <= start || end > len(body) {
				return errors.New("historical JSON scalar span could not be bound")
			}
			basis := historicalMessageOffsetBasis(location)
			if basis == "" {
				return errors.New("historical JSON body location is invalid")
			}
			span := Record{
				"start": bodyOffset + start, "end": bodyOffset + end,
				"unit": "byte", "offset_basis": basis, "wire_fidelity": wireFidelity,
			}
			occurrence = &historicalFieldOccurrence{sourceSpan: span, valueSpan: cloneRecord(span)}
			if value, isString := token.(string); isString && len(fieldValueKey) == 32 &&
				len(value) > 0 && len(value) <= 4_096 && safeEqualityString(value) && !secretNamePattern.MatchString(path) {
				occurrence.valueToken = fieldValueTokenForNormalization(fieldValueKey, value, "decoded_utf8_scalar_v2")
				occurrence.valueTokenVersion = "field-equality-v2"
				occurrence.valueNormalization = "decoded_utf8_scalar_v2"
			}
		}
		return appendField(path, typeName, occurrence)
	}

	if err := visit("$", 0); err != nil {
		if errors.Is(err, errHistoricalBodyFieldLimit) {
			return nil, false
		}
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return finalizeHistoricalJSONOccurrences(fields), true
}

func historicalJSONTokenStart(body []byte, offset int64) (int, bool) {
	if offset < 0 || offset > int64(len(body)) {
		return 0, false
	}
	for index := int(offset); index < len(body); index++ {
		switch value := body[index]; value {
		case ' ', '\t', '\r', '\n', ',', ':':
			continue
		case '"', 't', 'f', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			return index, true
		default:
			return 0, false
		}
	}
	return 0, false
}

// finalizeHistoricalJSONOccurrences retains array indices when a templated
// field occurs more than once and keeps legacy paths for singleton fields.
func finalizeHistoricalJSONOccurrences(fields []historicalParameterField) []historicalParameterField {
	indicesByTemplate := make(map[string][]int, len(fields))
	for index, field := range fields {
		template := historicalTemplateParameterPath(field.path)
		indicesByTemplate[template] = append(indicesByTemplate[template], index)
	}
	for template, indices := range indicesByTemplate {
		if len(indices) == 1 {
			fields[indices[0]].path = template
			if fields[indices[0]].occurrence != nil {
				fields[indices[0]].occurrence.index = 0
			}
			continue
		}
		for occurrenceIndex, fieldIndex := range indices {
			if fields[fieldIndex].occurrence != nil {
				fields[fieldIndex].occurrence.index = occurrenceIndex
			}
		}
	}
	return fields
}

func historicalSafeFieldName(name string, source Record, location string) (string, error) {
	if fieldNamePattern.MatchString(name) && !historicalRiskNamePattern.MatchString(name) {
		return name, nil
	}
	return historicalOpaqueLabel(source, location, "field")
}

func historicalSafeMultipartFieldName(name string, source Record, location string, aliases map[string]string) (string, error) {
	if fieldNamePattern.MatchString(name) && !historicalRiskNamePattern.MatchString(name) && !secretNamePattern.MatchString(name) {
		return name, nil
	}
	if alias, exists := aliases[name]; exists {
		return alias, nil
	}
	alias, err := historicalOpaqueLabel(source, location+".masked-field["+strconv.Itoa(len(aliases))+"]", "field")
	if err != nil {
		return "", err
	}
	aliases[name] = alias
	return alias, nil
}

func historicalTemplateParameterPath(path string) string {
	return historicalArrayIndexPattern.ReplaceAllString(path, "[]")
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
