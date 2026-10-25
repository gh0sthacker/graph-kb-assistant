package graph

import (
	"reflect"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
)

func TestRouteFamilyV1WithReasonUsesFixedCodes(t *testing.T) {
	for _, test := range []struct {
		name, target, wantFamily, wantReason string
	}{
		{name: "valid route", target: "https://fixture.invalid/api/items/42", wantFamily: "/api/items/{value}"},
		{name: "request-line-sized path", target: "https://fixture.invalid/" + strings.Repeat("a", 4095), wantFamily: "/{value}"},
		{name: "opaque URI", target: "mailto:person@example.invalid", wantReason: "opaque_target"},
		{name: "invalid target", target: "https://fixture.invalid/\x01", wantReason: "invalid_target"},
		{name: "overlong route", target: "https://fixture.invalid/" + strings.Repeat("a", 4096), wantReason: "path_too_long"},
	} {
		t.Run(test.name, func(t *testing.T) {
			family, reason := RouteFamilyV1WithReason(test.target)
			if reason != test.wantReason {
				t.Fatalf("RouteFamilyV1WithReason() reason = %q, want %q", reason, test.wantReason)
			}
			if (family != "") != (test.wantReason == "") {
				t.Fatalf("RouteFamilyV1WithReason() family = %q for reason %q", family, reason)
			}
			if family != test.wantFamily {
				t.Fatalf("RouteFamilyV1WithReason() family = %q, want %q", family, test.wantFamily)
			}
		})
	}
}

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

func TestProjectExchangeCanonicalizesIPv6AndRejectsMalformedDNSHosts(t *testing.T) {
	for _, test := range []struct {
		name   string
		host   string
		target string
		want   string
	}{
		{
			name:   "expanded ipv6 host header",
			host:   "[2001:0db8:0:0:0:0:0:1]",
			target: "http://[2001:db8::1]:3000/",
			want:   "2001:db8::1",
		},
		{
			name:   "ipv6 host header with port",
			host:   "[2001:db8::1]:3000",
			target: "http://[2001:db8::1]:3000/",
			want:   "2001:db8::1",
		},
		{
			name:   "malformed dotted dns host",
			host:   "foo..bar",
			target: "https://foo..bar/",
			want:   "unknown",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			exchange := caido.HttpExchange{
				Ref: caido.ExchangeRef{
					Project:   caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"},
					RequestID: "fixture-request",
				},
				RequestRaw:           []byte("GET / HTTP/1.1\r\nHost: fixture\r\n\r\n"),
				Method:               "GET",
				Scheme:               "http",
				Host:                 test.host,
				Port:                 3000,
				Target:               test.target,
				RequestWireFidelity:  "unknown",
				ResponseWireFidelity: "unknown",
			}
			projection, err := ProjectExchange(exchange)
			if err != nil {
				t.Fatalf("ProjectExchange() error = %v", err)
			}
			if projection.Search.Host != test.want {
				t.Errorf("ProjectExchange() host = %q, want %q", projection.Search.Host, test.want)
			}
		})
	}
}

func TestEndpointFromURLCanonicalizesIPv6Host(t *testing.T) {
	host, port, route, err := EndpointFromURL("https://[2001:0db8:0:0:0:0:0:1]:8443/api/items/42", "")
	if err != nil {
		t.Fatalf("EndpointFromURL() error = %v", err)
	}
	if host != "2001:db8::1" || port != 8443 || route != "/api/items/{value}" {
		t.Errorf("EndpointFromURL() = host %q port %d route %q; want canonical IPv6 host, port 8443 and templated route", host, port, route)
	}
}

func TestNormalizeURLKeepsIPv6AuthorityWellFormed(t *testing.T) {
	got, err := NormalizeURL("https://[2001:0db8:0:0:0:0:0:1]:8443/a", "")
	if err != nil {
		t.Fatalf("NormalizeURL() error = %v", err)
	}
	if got != "https://[2001:db8::1]:8443/a" {
		t.Errorf("NormalizeURL() = %q, want canonical bracketed IPv6 URL", got)
	}
}

func TestHistoricalOriginKeepsIPv6HostInsteadOfAliasingIt(t *testing.T) {
	got := historicalOriginAt("https://[2001:0db8:0:0:0:0:0:1]:8443/a", Record{}, "fixture.host")
	if got != "https://[2001:db8::1]:8443" {
		t.Errorf("historicalOriginAt() = %q, want canonical IPv6 origin", got)
	}
}
