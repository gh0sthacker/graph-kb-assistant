package publicdata

import (
	"bytes"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// TextRedactionSpan reports a byte range in the original input without
// retaining the value that was removed.
type TextRedactionSpan struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Rule  string `json:"rule"`
}

// RedactText removes common credential and personal-data patterns before text
// can enter a model-visible packet. Callers must still validate the final
// packet and treat this as a heuristic policy, not proof that text is clean.
func RedactText(input string) (string, []TextRedactionSpan, error) {
	if !utf8.ValidString(input) {
		return "", nil, errors.New("text is not valid UTF-8")
	}
	spans := make([]TextRedactionSpan, 0)
	for _, match := range headerFieldPattern.FindAllStringSubmatchIndex(input, -1) {
		if len(match) < 6 || match[2] < 0 || match[4] < 0 || match[5] <= match[4] {
			continue
		}
		if sensitiveHeaderPattern.MatchString(input[match[2]:match[3]]) {
			spans = append(spans, TextRedactionSpan{Start: match[4], End: match[5], Rule: "sensitive-header"})
		}
	}
	for _, assignment := range []struct {
		pattern        *regexp.Regexp
		keyFrom        int
		keyTo          int
		valueFrom      int
		valueTo        int
		checkBareValue bool
		rule           string
	}{
		{secretAssignmentPattern, 2, 3, 4, 5, true, "credential-assignment"},
		{jsonSecretAssignmentPattern, 2, 3, 4, 5, true, "credential-assignment"},
		{sensitivePIIAssignmentPattern, 2, 3, 4, 5, false, "personal-data-assignment"},
		{jsonSensitivePIIAssignmentPattern, 2, 3, 4, 5, false, "personal-data-assignment"},
	} {
		for _, match := range assignment.pattern.FindAllStringSubmatchIndex(input, -1) {
			if len(match) <= assignment.valueTo || match[assignment.keyFrom] < 0 || match[assignment.keyTo] < 0 || match[assignment.valueFrom] < 0 || match[assignment.valueTo] <= match[assignment.valueFrom] {
				continue
			}
			key := input[match[assignment.keyFrom]:match[assignment.keyTo]]
			value := input[match[assignment.valueFrom]:match[assignment.valueTo]]
			if assignment.checkBareValue && !isSensitiveAssignmentValue(key, value) {
				continue
			}
			start, end := match[assignment.valueFrom], match[assignment.valueTo]
			if end-start >= 2 && (input[start] == '"' && input[end-1] == '"' || input[start] == '\'' && input[end-1] == '\'') {
				start++
				end--
			}
			if end > start {
				spans = append(spans, TextRedactionSpan{Start: start, End: end, Rule: assignment.rule})
			}
		}
	}
	for _, match := range secretOtherValuePattern.FindAllStringIndex(input, -1) {
		spans = append(spans, TextRedactionSpan{Start: match[0], End: match[1], Rule: "secret-like-value"})
	}
	for _, match := range privateKeyBlockPattern.FindAllStringIndex(input, -1) {
		spans = append(spans, TextRedactionSpan{Start: match[0], End: match[1], Rule: "private-key-block"})
	}
	if len(spans) == 0 {
		return input, []TextRedactionSpan{}, nil
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Start == spans[j].Start {
			return spans[i].End < spans[j].End
		}
		return spans[i].Start < spans[j].Start
	})
	type redactionGroup struct {
		start, end int
		rules      map[string]struct{}
	}
	merged := make([]redactionGroup, 0, len(spans))
	for _, span := range spans {
		if span.Start < 0 || span.End > len(input) || span.End <= span.Start {
			continue
		}
		if len(merged) == 0 || span.Start > merged[len(merged)-1].end {
			merged = append(merged, redactionGroup{start: span.Start, end: span.End, rules: map[string]struct{}{span.Rule: {}}})
			continue
		}
		last := &merged[len(merged)-1]
		if span.End > last.end {
			last.end = span.End
		}
		last.rules[span.Rule] = struct{}{}
	}
	var output bytes.Buffer
	output.Grow(len(input))
	cursor := 0
	redactions := make([]TextRedactionSpan, 0, len(merged))
	for _, span := range merged {
		output.WriteString(input[cursor:span.start])
		output.WriteString("[REDACTED]")
		cursor = span.end
		rules := make([]string, 0, len(span.rules))
		for rule := range span.rules {
			rules = append(rules, rule)
		}
		sort.Strings(rules)
		redactions = append(redactions, TextRedactionSpan{Start: span.start, End: span.end, Rule: strings.Join(rules, "+")})
	}
	output.WriteString(input[cursor:])
	return output.String(), redactions, nil
}
