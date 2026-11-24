package graph

import (
	"strings"
	"testing"
)

func TestHistoricalGraphPathPreservesPythonV1ProductRouteIdentity(t *testing.T) {
	source := Record{
		"path":           "/synthetic/receipt.json",
		"sha256":         strings.Repeat("a", 64),
		"record_locator": "$.records[0]",
	}

	for _, routeWord := range []string{"product", "products"} {
		t.Run(routeWord, func(t *testing.T) {
			path, incomplete := historicalGraphPath("https://example.invalid/api/"+routeWord+"/123", source)
			if incomplete {
				t.Fatal("historicalGraphPath() reported an incomplete route")
			}
			segments := strings.Split(path, "/")
			if len(segments) != 4 || segments[1] != "api" || !strings.HasPrefix(segments[2], "segment_") {
				t.Fatalf("historicalGraphPath() did not preserve the Python v1 opaque segment: %q", path)
			}
		})
	}

	if got, want := RouteTemplate("/api/products/123"), "/api/products/{value}"; got != want {
		t.Fatalf("RouteTemplate() = %q, want %q", got, want)
	}
}
