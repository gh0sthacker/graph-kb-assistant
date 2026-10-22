package graph

import (
	"reflect"
	"strings"
	"testing"
)

func TestRecordDifferencePathsReportsSchemaPathsWithoutValues(t *testing.T) {
	left := Record{
		"id":   "observation-id",
		"kind": "observation",
		"data": Record{
			"context": Record{
				"origin":      "https://private.example.invalid",
				"observed_at": "2026-10-22T10:00:00Z",
			},
		},
	}
	right := Record{
		"id":   "observation-id",
		"kind": "observation",
		"data": Record{
			"context": Record{
				"origin":      "https://other.example.invalid",
				"observed_at": "2026-10-22T10:01:00Z",
			},
		},
	}

	got := recordDifferencePaths(left, right, "")
	want := []string{"data.context.observed_at", "data.context.origin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recordDifferencePaths() = %v, want %v", got, want)
	}
	joined := strings.Join(got, ",")
	for _, sensitive := range []string{"private.example", "other.example", "10:00:00", "10:01:00"} {
		if strings.Contains(joined, sensitive) {
			t.Fatalf("field diagnostics exposed a value: %q", joined)
		}
	}
}

func TestRecordDifferencePathsBoundsReportedFields(t *testing.T) {
	left, right := Record{}, Record{}
	for index := 0; index < 24; index++ {
		key := string(rune('a' + index))
		left[key] = "old"
		right[key] = "new"
	}

	got := recordDifferencePaths(left, right, "")
	if len(got) != 16 {
		t.Fatalf("recordDifferencePaths() returned %d paths, want 16", len(got))
	}
}
