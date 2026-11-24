package graph

import (
	"bytes"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	xhtml "golang.org/x/net/html"
)

const historicalMaxHTMLDepth = 64

var (
	historicalHTMLVoidElements = map[string]struct{}{
		"area": {}, "base": {}, "br": {}, "col": {}, "embed": {}, "hr": {}, "img": {},
		"input": {}, "link": {}, "meta": {}, "param": {}, "source": {}, "track": {}, "wbr": {},
	}
	historicalHTMLInputTypes = map[string]struct{}{
		"text": {}, "password": {}, "hidden": {}, "file": {}, "email": {}, "number": {},
		"submit": {}, "checkbox": {}, "radio": {}, "input": {}, "select": {}, "textarea": {},
		"button": {}, "url": {}, "search": {}, "date": {}, "tel": {},
	}
)

type historicalHTMLAnalysis struct {
	fields              []historicalParameterField
	metadata            Record
	characteristics     Record
	complete            bool
	occurrencesComplete bool
}

type historicalHTMLSafeName func(name, location string) (string, error)

func historicalHTMLBodyFields(body []byte, location string, source Record, remaining, bodyOffset int, sourceSpanAvailable bool, wireFidelity string) historicalHTMLAnalysis {
	maskedNames := make(map[string]string)
	return scanHistoricalHTML(body, location, remaining, func(name, path string) (string, error) {
		return historicalSafeHTMLFieldName(name, source, path, maskedNames)
	}, source, bodyOffset, sourceSpanAvailable, wireFidelity)
}

func historicalHTMLProjectionFields(body []byte, location string, remaining int) historicalHTMLAnalysis {
	return scanHistoricalHTML(body, location, remaining, func(name, _ string) (string, error) {
		return safeField(name), nil
	}, nil, 0, false, "")
}

func scanHistoricalHTML(body []byte, location string, remaining int, safeName historicalHTMLSafeName, source Record, bodyOffset int, sourceSpanAvailable bool, wireFidelity string) historicalHTMLAnalysis {
	result := historicalHTMLAnalysis{
		fields:          make([]historicalParameterField, 0, minInt(maxInt(remaining, 0), 64)),
		metadata:        Record{"forms": []any{}, "links": []any{}},
		characteristics: Record{},
		complete:        remaining >= 0 && len(body) <= maxParsedBodyBytes,
	}
	if !result.complete {
		return result
	}

	tokenizer := xhtml.NewTokenizer(bytes.NewReader(body))
	tokenizer.SetMaxBuf(maxParsedBodyBytes)
	stack := make([]string, 0, historicalMaxHTMLDepth)
	forms := make([]any, 0)
	links := make([]any, 0)
	inputCount := 0
	inputOccurrenceCounts := make(map[string]int)
	hasPassword := false
	appendField := func(path, typeName string, occurrence *historicalFieldOccurrence) bool {
		if len(result.fields) >= remaining {
			result.complete = false
			return false
		}
		if len(path) > historicalMaxParameterPath {
			result.complete = false
			return false
		}
		result.fields = append(result.fields, historicalParameterField{
			path: historicalTemplateParameterPath(path), typeName: typeName, location: location, occurrence: occurrence,
		})
		return true
	}
	fieldOccurrence := func(index int, tokenStart, tokenEnd int64) *historicalFieldOccurrence {
		if !sourceSpanAvailable || location != "response.body" || tokenStart < 0 || tokenEnd <= tokenStart || tokenEnd > int64(len(body)) {
			return nil
		}
		start, end := bodyOffset+int(tokenStart), bodyOffset+int(tokenEnd)
		span := Record{
			"start": start, "end": end, "unit": "byte",
			"offset_basis": historicalMessageOffsetBasis(location), "wire_fidelity": wireFidelity,
		}
		return &historicalFieldOccurrence{index: index, sourceSpan: span}
	}
	stop := false
	tokenOffset := int64(0)
	for !stop {
		tokenType := tokenizer.Next()
		rawLength := int64(len(tokenizer.Raw()))
		tokenStart, tokenEnd := tokenOffset, tokenOffset+rawLength
		tokenOffset = tokenEnd
		switch tokenType {
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			token := tokenizer.Token()
			tag := strings.ToLower(token.Data)
			_, isVoid := historicalHTMLVoidElements[tag]
			if !isVoid {
				if len(stack) >= historicalMaxHTMLDepth {
					result.complete = false
					break
				}
				stack = append(stack, tag)
			}
			attributes := make(map[string]string, len(token.Attr))
			for _, attribute := range token.Attr {
				attributes[strings.ToLower(attribute.Key)] = attribute.Val
			}

			if tag == "form" {
				index := len(forms)
				actionValue, exists := attributes["action"]
				if !exists {
					actionValue = "/"
				}
				action, actionComplete := historicalHTMLURL(actionValue, source, location+".form["+strconv.Itoa(index)+"]", safeName)
				if !actionComplete {
					result.complete = false
				}
				method := strings.ToUpper(firstNonEmpty(attributes["method"], "GET"))
				if !httpMethodPattern.MatchString(method) {
					method = "UNKNOWN"
				}
				if !appendField("html.forms["+strconv.Itoa(index)+"]", "form", nil) {
					break
				}
				forms = append(forms, Record{"action": action, "method": method})
			}

			if tag == "input" || tag == "select" || tag == "textarea" || tag == "button" {
				index := inputCount
				inputCount++
				name, exists := attributes["name"]
				if !exists {
					name = "unnamed"
				}
				safe, err := safeName(name, location+".input["+strconv.Itoa(index)+"]")
				if err != nil {
					result.complete = false
					break
				}
				inputType := strings.ToLower(firstNonEmpty(attributes["type"], tag))
				if _, ok := historicalHTMLInputTypes[inputType]; !ok {
					inputType = "other"
				}
				hasPassword = hasPassword || inputType == "password"
				path := "html.inputs[" + strconv.Itoa(index) + "][" + strconv.Quote(safe) + "]"
				occurrenceKey := safe + "\x00" + inputType
				occurrenceIndex := inputOccurrenceCounts[occurrenceKey]
				inputOccurrenceCounts[occurrenceKey] = occurrenceIndex + 1
				occurrence := fieldOccurrence(occurrenceIndex, tokenStart, tokenEnd)
				if sourceSpanAvailable && location == "response.body" && occurrence == nil {
					result.complete = false
					break
				}
				if !appendField(path, "input:"+inputType, occurrence) {
					break
				}
			}

			if tag == "a" || tag == "link" {
				if href, exists := attributes["href"]; exists {
					index := len(links)
					link, linkComplete := historicalHTMLURL(href, source, location+".link["+strconv.Itoa(index)+"]", safeName)
					if !linkComplete {
						result.complete = false
					}
					if !appendField("html.links["+strconv.Itoa(index)+"]", "link", nil) {
						break
					}
					links = append(links, link)
				}
			}
			if tokenType == xhtml.SelfClosingTagToken && !isVoid {
				stack = stack[:len(stack)-1]
			}
		case xhtml.EndTagToken:
			tag := strings.ToLower(tokenizer.Token().Data)
			for index := len(stack) - 1; index >= 0; index-- {
				if stack[index] == tag {
					stack = stack[:index]
					break
				}
			}
		case xhtml.ErrorToken:
			if tokenizer.Err() != io.EOF {
				result.complete = false
			}
			stop = true
		}
		if !result.complete {
			stop = true
		}
	}
	if tokenizer.Err() != nil && tokenizer.Err() != io.EOF {
		result.complete = false
	}
	if result.complete && tokenOffset != int64(len(body)) {
		result.complete = false
	}

	result.metadata["forms"] = forms
	result.metadata["links"] = links
	text := string(body)
	result.characteristics = Record{
		"form_count": len(forms), "input_count": inputCount, "link_count": len(links),
		"has_password_input":       hasPassword,
		"login_page_signal":        hasPassword || historicalHTMLLoginSignal.MatchString(text),
		"error_page_signal":        historicalHTMLErrorSignal.MatchString(text),
		"possible_soft_404_signal": historicalHTMLNotFoundSignal.MatchString(text),
	}
	result.occurrencesComplete = result.complete && sourceSpanAvailable &&
		(location == "response.body" || inputCount == 0)
	return result
}

func historicalSafeHTMLFieldName(name string, source Record, location string, aliases map[string]string) (string, error) {
	if secretNamePattern.MatchString(name) {
		if source == nil {
			return safeField(name), nil
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
	return historicalSafeFieldName(name, source, location)
}

func historicalHTMLURL(value string, source Record, location string, safeName historicalHTMLSafeName) (Record, bool) {
	unknown := Record{"origin": "unknown", "path": "unknown", "query_names": []any{}}
	if value == "" || len(value) > 65_536 {
		return unknown, false
	}
	parsed, err := historicalParseURL(value)
	if err != nil || !historicalTargetValid(value) {
		return unknown, false
	}
	origin := historicalOriginAt(value, source, location+".host")
	pathValue := parsed.EscapedPath()
	if parsed.Opaque != "" {
		pathValue = parsed.Opaque
	}
	if pathValue == "" {
		pathValue = "/"
	}
	segments := strings.Split(pathValue, "/")
	if len(segments) > 4_096 {
		return Record{"origin": origin, "path": "unknown", "query_names": []any{}}, false
	}
	for index, segment := range segments {
		if segment == "" {
			continue
		}
		decoded, valid := historicalURLUnquote(segment, false, false)
		if !valid {
			return unknown, false
		}
		routeWord := strings.ToLower(strings.SplitN(decoded, ".", 2)[0])
		_, knownRoute := historicalRouteWords[routeWord]
		if !fieldNamePattern.MatchString(decoded) || historicalRiskNamePattern.MatchString(decoded) || (!knownRoute && !historicalSingleLetter.MatchString(decoded)) {
			alias, aliasErr := historicalHTMLSegmentName(decoded, source, location+".segment["+strconv.Itoa(index)+"]")
			if aliasErr != nil {
				return unknown, false
			}
			segments[index] = alias
		} else {
			segments[index] = decoded
		}
	}
	path := strings.Join(segments, "/")
	queryNames, complete := historicalHTMLQueryNames(parsed.RawQuery, location, safeName)
	queryValues := make([]any, len(queryNames))
	for index, name := range queryNames {
		queryValues[index] = name
	}
	return Record{"origin": origin, "path": path, "query_names": queryValues}, complete
}

func historicalHTMLQueryNames(rawQuery string, location string, safeName historicalHTMLSafeName) ([]string, bool) {
	if rawQuery == "" {
		return []string{}, true
	}
	pairs := strings.Split(rawQuery, "&")
	if len(pairs) > 100_001 {
		return []string{}, false
	}
	seen := make(map[string]struct{})
	queryIndex := 0
	for _, pair := range pairs {
		if pair == "" {
			continue
		}
		key, value, hasValue := strings.Cut(pair, "=")
		if !hasValue {
			value = ""
		}
		name, validName := historicalURLUnquote(key, true, true)
		_, validValue := historicalURLUnquote(value, true, true)
		if !validName || !validValue {
			return []string{}, false
		}
		safe, err := safeName(name, location+".query["+strconv.Itoa(queryIndex)+"]")
		if err != nil {
			return []string{}, false
		}
		queryIndex++
		seen[safe] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true
}

func historicalHTMLSegmentName(name string, source Record, location string) (string, error) {
	if fieldNamePattern.MatchString(name) && !historicalRiskNamePattern.MatchString(name) {
		routeWord := strings.ToLower(strings.SplitN(name, ".", 2)[0])
		_, knownRoute := historicalRouteWords[routeWord]
		if knownRoute || historicalSingleLetter.MatchString(name) {
			return name, nil
		}
	}
	if source == nil {
		return safeField(name), nil
	}
	return historicalOpaqueLabel(source, location, "segment")
}

var (
	historicalHTMLLoginSignal    = regexp.MustCompile(`(?i)\b(sign in|log in|login)\b`)
	historicalHTMLErrorSignal    = regexp.MustCompile(`(?i)\b(access denied|forbidden|internal server error|stack trace)\b`)
	historicalHTMLNotFoundSignal = regexp.MustCompile(`(?i)\b(not found|page does not exist|404)\b`)
)

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
