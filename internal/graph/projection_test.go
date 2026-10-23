package graph

import (
	"reflect"
	"testing"

	"bb-graph-assistant/internal/caido"
)

func TestSplitMessagePreservesHTTP2PseudoHeaders(t *testing.T) {
	raw := []byte("GET /items?id=1 HTTP/2\r\n:method: GET\r\n:scheme: https\r\n:authority: fixture.invalid\r\n:path: /items?id=1\r\naccept: application/json\r\n\r\n")
	headers, body, boundary, complete := splitMessage(raw)
	if !boundary || !complete || len(body) != 0 {
		t.Fatalf("splitMessage() boundary/complete/body = %v/%v/%d, want true/true/0", boundary, complete, len(body))
	}
	want := []headerField{
		{name: ":method", value: "GET"},
		{name: ":scheme", value: "https"},
		{name: ":authority", value: "fixture.invalid"},
		{name: ":path", value: "/items?id=1"},
		{name: "accept", value: "application/json"},
	}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("splitMessage() headers = %#v, want %#v", headers, want)
	}
}

func TestSplitMessageMarksPseudoHeadersInHTTP1Incomplete(t *testing.T) {
	raw := []byte("GET / HTTP/1.1\r\n:method: GET\r\n:authority: fixture.invalid\r\n\r\n")
	_, _, boundary, complete := splitMessage(raw)
	if !boundary || complete {
		t.Fatalf("splitMessage() boundary/complete = %v/%v, want true/false", boundary, complete)
	}
}

func TestSplitMessageMarksInvalidHeaderLinesIncomplete(t *testing.T) {
	raw := []byte("GET / HTTP/1.1\r\nvalid: yes\r\nX-Invalid/Name: preserved\r\nnot a header\r\n\r\n")
	_, _, boundary, complete := splitMessage(raw)
	if !boundary || complete {
		t.Fatalf("splitMessage() boundary/complete = %v/%v, want true/false", boundary, complete)
	}
}

func TestProjectExchangeMarksResponseStatusMetadataMismatchPartial(t *testing.T) {
	requestTruncated, responseTruncated, status := false, false, 201
	exchange := caido.HttpExchange{
		Ref: caido.ExchangeRef{
			Project:   caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"},
			RequestID: "fixture-request",
		},
		RequestRaw:                     []byte("GET / HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"),
		ResponseRaw:                    []byte("HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n"),
		HasResponse:                    true,
		Method:                         "GET",
		Scheme:                         "https",
		Host:                           "fixture.invalid",
		Port:                           443,
		Target:                         "https://fixture.invalid/",
		ResponseStatus:                 &status,
		RequestTruncated:               &requestTruncated,
		ResponseTruncated:              &responseTruncated,
		RequestWireFidelity:            "reconstructed_text",
		ResponseWireFidelity:           "reconstructed_text",
		ResponseStatusMetadataMismatch: true,
	}
	projection, err := ProjectExchange(exchange)
	if err != nil {
		t.Fatalf("ProjectExchange() error = %v", err)
	}
	if projection.ParseState != "partial" {
		t.Fatalf("ProjectExchange() parse state = %q, want partial", projection.ParseState)
	}
	if projection.Search.ResponseStatus == nil || *projection.Search.ResponseStatus != 201 {
		t.Fatalf("ProjectExchange() response status = %v, want detailed response status 201", projection.Search.ResponseStatus)
	}
}

func TestProjectExchangeMarksMalformedHeaderNamePartial(t *testing.T) {
	requestTruncated, responseTruncated, status := false, false, 200
	exchange := caido.HttpExchange{
		Ref: caido.ExchangeRef{
			Project:   caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"},
			RequestID: "fixture-request",
		},
		RequestRaw:                 []byte("GET / HTTP/1.1\r\nHost: fixture.invalid\r\nX:Bad: yes\r\n\r\n"),
		ResponseRaw:                []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"),
		HasResponse:                true,
		Method:                     "GET",
		Scheme:                     "https",
		Host:                       "fixture.invalid",
		Port:                       443,
		Target:                     "https://fixture.invalid/",
		ResponseStatus:             &status,
		RequestTruncated:           &requestTruncated,
		ResponseTruncated:          &responseTruncated,
		RequestWireFidelity:        "reconstructed_text",
		ResponseWireFidelity:       "reconstructed_text",
		MalformedHeaderNamePresent: true,
	}
	projection, err := ProjectExchange(exchange)
	if err != nil {
		t.Fatalf("ProjectExchange() error = %v", err)
	}
	if projection.ParseState != "partial" {
		t.Fatalf("ProjectExchange() parse state = %q, want partial", projection.ParseState)
	}
}
