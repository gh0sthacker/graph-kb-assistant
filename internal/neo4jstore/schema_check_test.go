package neo4jstore

import (
	"context"
	"strings"
	"testing"
)

func TestCheckSchemaRunsOnlyReadOnlyCatalogQueries(t *testing.T) {
	var queries []string
	err := checkSchemaWithQueries(context.Background(), func(_ context.Context, query string) ([]string, error) {
		queries = append(queries, query)
		upper := strings.ToUpper(query)
		if strings.Contains(upper, "CREATE ") || strings.Contains(upper, "DROP ") || strings.Contains(upper, "DELETE ") || strings.Contains(upper, " SET ") {
			t.Fatalf("schema check attempted a write query: %q", query)
		}
		switch query {
		case "SHOW CONSTRAINTS YIELD name RETURN name":
			return append([]string(nil), requiredConstraintNames...), nil
		case "SHOW INDEXES YIELD name RETURN name":
			return append([]string(nil), requiredIndexNames...), nil
		default:
			return nil, nil
		}
	})
	if err != nil {
		t.Fatalf("checkSchemaWithQueries(): %v", err)
	}
	if len(queries) != 2 || queries[0] != "SHOW CONSTRAINTS YIELD name RETURN name" || queries[1] != "SHOW INDEXES YIELD name RETURN name" {
		t.Fatalf("schema check queries = %#v", queries)
	}
}

func TestCheckSchemaReportsMissingConstraintOrIndex(t *testing.T) {
	err := checkSchemaWithQueries(context.Background(), func(_ context.Context, query string) ([]string, error) {
		if query == "SHOW CONSTRAINTS YIELD name RETURN name" {
			return nil, nil
		}
		return append([]string(nil), requiredIndexNames...), nil
	})
	if err == nil || !strings.Contains(err.Error(), "graph_record_identity") {
		t.Fatalf("missing schema error = %v; want missing graph_record_identity", err)
	}
}
