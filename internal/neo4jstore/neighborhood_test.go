package neo4jstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestNeighborhoodCursorRequiresPinnedGenerationAndAssertionAnchor(t *testing.T) {
	valid := NeighborhoodCursor{Generation: 3, Priority: 1, AfterAssertionID: "assertion-" + strings.Repeat("a", 64)}
	if err := validateNeighborhoodCursor(&valid); err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}
	for _, invalid := range []NeighborhoodCursor{
		{Generation: 0, Priority: 1, AfterAssertionID: valid.AfterAssertionID},
		{Generation: 3, Priority: 2, AfterAssertionID: valid.AfterAssertionID},
		{Generation: 3, Priority: 1, AfterAssertionID: "endpoint-" + strings.Repeat("a", 64)},
	} {
		if err := validateNeighborhoodCursor(&invalid); err == nil {
			t.Fatalf("accepted invalid neighborhood cursor: %#v", invalid)
		}
	}
	if err := validateNeighborhoodCursor(nil); err != nil {
		t.Fatalf("initial page cursor rejected: %v", err)
	}
}

func TestNeighborhoodRecordChecksumsAndIdentity(t *testing.T) {
	id := "endpoint-" + strings.Repeat("1", 64)
	payload := `{"data":{"method":"GET"},"id":"` + id + `","kind":"endpoint"}`
	hash := sha256.Sum256([]byte(payload))
	checksum := hex.EncodeToString(hash[:])
	if _, err := decodeNeighborhoodRecord(id, "entity", payload, checksum); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	for _, test := range []struct{ name, id, kind, payload, checksum string }{
		{"checksum", id, "entity", payload, strings.Repeat("0", 64)},
		{"identity", "endpoint-" + strings.Repeat("2", 64), "entity", payload, checksum},
		{"record type", id, "assertion", payload, checksum},
		{"unsupported type", id, "other", payload, checksum},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeNeighborhoodRecord(test.id, test.kind, test.payload, test.checksum); err == nil {
				t.Fatal("accepted an invalid graph record")
			}
		})
	}
}

func TestNeighborhoodSelectionRejectsInvalidArguments(t *testing.T) {
	project := strings.Repeat("a", 64)
	id := "endpoint-" + strings.Repeat("1", 64)
	for _, test := range []struct {
		name, project string
		ids           []string
		budget        int
	}{
		{"project", "invalid", []string{id}, 1},
		{"empty", project, nil, 1},
		{"too many seeds", project, make([]string, 21), 1},
		{"identity", project, []string{"endpoint-invalid"}, 1},
		{"source seed", project, []string{"source-" + strings.Repeat("1", 64)}, 1},
		{"card seed", project, []string{"card-" + strings.Repeat("1", 64)}, 1},
		{"assertion seed", project, []string{"assertion-" + strings.Repeat("1", 64)}, 1},
		{"zero budget", project, []string{id}, 0},
		{"large budget", project, []string{id}, 301},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := neighborhoodSeeds(test.project, test.ids, test.budget); err == nil {
				t.Fatal("accepted invalid neighborhood arguments")
			}
		})
	}
	ids, err := neighborhoodSeeds(project, []string{id, id}, 300)
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("duplicate seeds were not normalized: %v, %v", ids, err)
	}
	if _, err := (*Store)(nil).ReadProjectNeighborhood(context.Background(), project, []string{id}, 1); err == nil {
		t.Fatal("nil store accepted")
	}
}

func TestNeighborhoodSourceRequiresCanonicalIdentityAndChecksum(t *testing.T) {
	source := behaviorgraph.Record{"path": "/synthetic/receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.record"}
	payload, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	checksum := hex.EncodeToString(hash[:])
	if _, err := decodeNeighborhoodRecord("source-"+checksum, "source", string(payload), checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeNeighborhoodRecord("source-"+strings.Repeat("b", 64), "source", string(payload), checksum); err == nil {
		t.Fatal("accepted a source whose canonical content does not match its ID")
	}
	if _, err := decodeNeighborhoodRecord("source-"+checksum, "source", string(payload), strings.Repeat("b", 64)); err == nil {
		t.Fatal("accepted a source with a corrupt checksum")
	}
}

func TestNeighborhoodTopologyRequiresExactEndpointAndSingleRole(t *testing.T) {
	id, left, right := "assertion-a", "endpoint-left", "endpoint-right"
	records := map[string]neighborhoodRecord{
		id:   {id: id, kind: "assertion", value: behaviorgraph.Record{"subject": left, "object": right}},
		left: {id: left, kind: "entity"}, right: {id: right, kind: "entity"},
	}
	seen := make(map[string]map[string]string)
	if !validNeighborhoodTopology(records, seen, id, "SUBJECT", left) || !validNeighborhoodTopology(records, seen, id, "OBJECT", right) {
		t.Fatal("rejected exact assertion topology")
	}
	if validNeighborhoodTopology(records, seen, id, "SUBJECT", left) {
		t.Fatal("accepted a duplicate SUBJECT relationship")
	}
	for _, test := range []struct{ id, role, endpoint string }{
		{id, "SUBJECT", right}, {id, "OBJECT", left}, {id, "OTHER", left}, {left, "SUBJECT", left}, {id, "OBJECT", "missing"},
	} {
		if validNeighborhoodTopology(records, make(map[string]map[string]string), test.id, test.role, test.endpoint) {
			t.Fatalf("accepted inconsistent topology: %#v", test)
		}
	}
}

func TestNeighborhoodEnforcesCumulativeRecordAndByteCaps(t *testing.T) {
	id := "endpoint-" + strings.Repeat("1", 64)
	payload := `{"data":{},"id":"` + id + `","kind":"endpoint"}`
	digest := sha256.Sum256([]byte(payload))
	hash := hex.EncodeToString(digest[:])
	read := &neighborhoodRead{records: make(map[string]neighborhoodRecord), bytes: maxNeighborhoodBytes - len(payload)}
	if _, err := read.add(id, "entity", payload, hash); err != nil {
		t.Fatalf("exact byte boundary rejected: %v", err)
	}
	if _, err := read.add(id, "entity", payload, hash); err == nil {
		t.Fatal("repeated payload reads bypassed the cumulative byte cap")
	}
	read = &neighborhoodRead{records: make(map[string]neighborhoodRecord)}
	for i := 0; i < maxNeighborhoodRecords; i++ {
		read.records[string(rune(i))] = neighborhoodRecord{}
	}
	if _, err := read.add(id, "entity", payload, hash); err == nil {
		t.Fatal("accepted more records than the hard cap")
	}
}

// neighborhoodBudgetTx distinguishes metadata-only preflight queries from
// queries that transfer complete payload_json strings. It lets regression
// tests prove an oversized response is rejected before payload retrieval.
type neighborhoodBudgetTx struct {
	neo4j.ManagedTransaction
	recordID       string
	recordKind     string
	recordPayload  string
	recordChecksum string
	ownerID        string
	sourceID       string
	sourcePayload  string
	sourceChecksum string
	fullFetches    int
}

func (tx *neighborhoodBudgetTx) Run(_ context.Context, query string, params map[string]any) (neo4j.Result, error) {
	rows := make([]*neo4j.Record, 0)
	if strings.Contains(query, "payload_char_count") {
		if _, ok := params["record_ids"]; ok {
			rows = append(rows, &neo4j.Record{
				Keys:   []string{"record_id", "record_type", "payload_char_count", "payload_sha256"},
				Values: []any{tx.recordID, tx.recordKind, int64(len([]rune(tx.recordPayload))), tx.recordChecksum},
			})
		} else {
			rows = append(rows, &neo4j.Record{
				Keys:   []string{"owner_id", "record_id", "record_type", "payload_char_count", "payload_sha256", "invalid_links"},
				Values: []any{tx.ownerID, tx.sourceID, "source", int64(len([]rune(tx.sourcePayload))), tx.sourceChecksum, false},
			})
		}
		return &neighborhoodProvenanceResult{rows: rows, position: -1}, nil
	}
	tx.fullFetches++
	if _, ok := params["record_ids"]; ok {
		rows = append(rows, &neo4j.Record{
			Keys:   []string{"record_id", "record_type", "payload_json", "payload_sha256"},
			Values: []any{tx.recordID, tx.recordKind, tx.recordPayload, tx.recordChecksum},
		})
	} else if _, ok := params["source_ids"]; ok {
		rows = append(rows, &neo4j.Record{
			Keys:   []string{"record_id", "record_type", "payload_json", "payload_sha256"},
			Values: []any{tx.sourceID, "source", tx.sourcePayload, tx.sourceChecksum},
		})
	} else {
		rows = append(rows, &neo4j.Record{
			Keys:   []string{"owner_id", "record_id", "record_type", "payload_json", "payload_sha256", "invalid_links"},
			Values: []any{tx.ownerID, tx.sourceID, "source", tx.sourcePayload, tx.sourceChecksum, false},
		})
	}
	return &neighborhoodProvenanceResult{rows: rows, position: -1}, nil
}

func TestNeighborhoodLoadPreflightsPayloadBudgetBeforeFetchingRecords(t *testing.T) {
	id := "endpoint-" + strings.Repeat("1", 64)
	payload := `{"data":{},"id":"` + id + `","kind":"endpoint"}`
	digest := sha256.Sum256([]byte(payload))
	checksum := hex.EncodeToString(digest[:])
	read := &neighborhoodRead{
		project: strings.Repeat("a", 64), records: make(map[string]neighborhoodRecord),
		bytes: maxNeighborhoodBytes - len(payload) + 1,
	}
	tx := &neighborhoodBudgetTx{recordID: id, recordKind: "entity", recordPayload: payload, recordChecksum: checksum}
	if err := read.load(context.Background(), tx, []string{id}); err == nil {
		t.Fatal("accepted records whose selected payload exceeds the remaining byte budget")
	}
	if tx.fullFetches != 0 {
		t.Fatalf("fetched %d full payload result(s) before rejecting the byte budget", tx.fullFetches)
	}
}

func TestNeighborhoodLoadFetchesRecordsAfterSuccessfulBudgetPreflight(t *testing.T) {
	id := "endpoint-" + strings.Repeat("1", 64)
	payload := `{"data":{},"id":"` + id + `","kind":"endpoint"}`
	digest := sha256.Sum256([]byte(payload))
	checksum := hex.EncodeToString(digest[:])
	read := &neighborhoodRead{project: strings.Repeat("a", 64), records: make(map[string]neighborhoodRecord)}
	tx := &neighborhoodBudgetTx{recordID: id, recordKind: "entity", recordPayload: payload, recordChecksum: checksum}
	if err := read.load(context.Background(), tx, []string{id}); err != nil {
		t.Fatalf("valid record failed its size-first read: %v", err)
	}
	if tx.fullFetches != 1 || read.records[id].hash != checksum || read.bytes != len(payload) {
		t.Fatalf("record payload was not fetched and accounted for after preflight: fetches=%d bytes=%d", tx.fullFetches, read.bytes)
	}
}

func TestNeighborhoodAssertionProvenancePreflightsPayloadBudgetBeforeFetchingSources(t *testing.T) {
	owner := "assertion-" + strings.Repeat("1", 64)
	source := behaviorgraph.Record{"path": "/synthetic/receipt.json", "sha256": strings.Repeat("b", 64), "record_locator": "$.record"}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	checksum := hex.EncodeToString(digest[:])
	sourceID := "source-" + checksum
	read := &neighborhoodRead{
		project: strings.Repeat("a", 64), records: map[string]neighborhoodRecord{
			owner: {id: owner, kind: "assertion", value: behaviorgraph.Record{"id": owner}},
		},
		bytes: maxNeighborhoodBytes - len(encoded) + 1,
	}
	tx := &neighborhoodBudgetTx{
		ownerID: owner, sourceID: sourceID, sourcePayload: string(encoded), sourceChecksum: checksum,
	}
	if err := read.loadAssertionProvenance(context.Background(), tx, []string{owner}); err == nil {
		t.Fatal("accepted mandatory assertion provenance that exceeds the remaining byte budget")
	}
	if tx.fullFetches != 0 {
		t.Fatalf("fetched %d full payload result(s) before rejecting the assertion-provenance budget", tx.fullFetches)
	}
}

func TestNeighborhoodAssertionProvenanceFetchesSourcesAfterSuccessfulBudgetPreflight(t *testing.T) {
	owner := "assertion-" + strings.Repeat("1", 64)
	source := behaviorgraph.Record{"path": "/synthetic/receipt.json", "sha256": strings.Repeat("b", 64), "record_locator": "$.record"}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	checksum := hex.EncodeToString(digest[:])
	sourceID := "source-" + checksum
	read := &neighborhoodRead{
		project: strings.Repeat("a", 64), records: map[string]neighborhoodRecord{
			owner: {id: owner, kind: "assertion", value: behaviorgraph.Record{"id": owner}},
		},
	}
	tx := &neighborhoodBudgetTx{
		ownerID: owner, sourceID: sourceID, sourcePayload: string(encoded), sourceChecksum: checksum,
	}
	if err := read.loadAssertionProvenance(context.Background(), tx, []string{owner}); err != nil {
		t.Fatalf("valid assertion provenance failed its size-first read: %v", err)
	}
	refs, ok := read.records[owner].value["source_refs"].([]behaviorgraph.Record)
	if tx.fullFetches != 1 || !ok || len(refs) != 1 || len(read.records[sourceID].value) != 3 || read.bytes != len(encoded) {
		t.Fatalf("mandatory provenance was not fetched and accounted for after preflight: fetches=%d refs=%#v bytes=%d", tx.fullFetches, refs, read.bytes)
	}
}

// This driver fixture exercises provenance selection without a database. Each
// entity shares the same sources, as endpoints/variants do across captures.
type neighborhoodProvenanceTx struct {
	neo4j.ManagedTransaction
	rows map[string][]*neo4j.Record
}

func (tx *neighborhoodProvenanceTx) Run(_ context.Context, query string, params map[string]any) (neo4j.Result, error) {
	rows := make([]*neo4j.Record, 0)
	if ids, ok := params["source_ids"].([]string); ok {
		byID := make(map[string]*neo4j.Record)
		for _, ownerRows := range tx.rows {
			for _, row := range ownerRows {
				byID[row.Values[1].(string)] = row
			}
		}
		for _, id := range ids {
			if row := byID[id]; row != nil {
				rows = append(rows, row)
			}
		}
	} else if keys, anchors := params["source_ref_keys"].([]string); anchors {
		for _, key := range keys {
			parts := strings.Split(key, "|")
			if len(parts) != 2 {
				continue
			}
			for _, row := range tx.rows[parts[0]] {
				if row.Values[1] == parts[1] {
					payload := row.Values[3].(string)
					rows = append(rows, &neo4j.Record{
						Keys:   []string{"owner_id", "record_id", "record_type", "payload_char_count", "payload_sha256", "invalid_links"},
						Values: []any{row.Values[0], row.Values[1], row.Values[2], int64(len([]rune(payload))), row.Values[4], row.Values[5]},
					})
					break
				}
			}
		}
	} else if perOwnerLimit, batched := params["limit_each"].(int); batched {
		excluded := make(map[string]bool)
		excludedKeys, _ := params["excluded_source_ref_keys"].([]string)
		for _, key := range excludedKeys {
			excluded[key] = true
		}
		for _, owner := range params["owner_ids"].([]string) {
			ownerRows := append([]*neo4j.Record(nil), tx.rows[owner]...)
			filtered := ownerRows[:0]
			for _, row := range ownerRows {
				if !excluded[owner+"|"+fmt.Sprint(row.Values[1])] {
					filtered = append(filtered, row)
				}
			}
			ownerRows = filtered
			sort.Slice(ownerRows, func(i, j int) bool { return fmt.Sprint(ownerRows[i].Values[1]) < fmt.Sprint(ownerRows[j].Values[1]) })
			if len(ownerRows) > perOwnerLimit {
				ownerRows = ownerRows[:perOwnerLimit]
			}
			if len(ownerRows) == 0 {
				rows = append(rows, &neo4j.Record{
					Keys:   []string{"owner_id", "record_id", "record_type", "payload_char_count", "payload_sha256", "invalid_links"},
					Values: []any{owner, nil, nil, nil, nil, false},
				})
				continue
			}
			rows = append(rows, ownerRows...)
		}
	} else {
		for _, owner := range params["owner_ids"].([]string) {
			rows = append(rows, tx.rows[owner]...)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if fmt.Sprint(rows[i].Values[0]) != fmt.Sprint(rows[j].Values[0]) {
			return fmt.Sprint(rows[i].Values[0]) < fmt.Sprint(rows[j].Values[0])
		}
		return fmt.Sprint(rows[i].Values[1]) < fmt.Sprint(rows[j].Values[1])
	})
	if limit := params["limit"].(int); len(rows) > limit {
		rows = rows[:limit]
	}
	if strings.Contains(query, "payload_char_count") {
		for index, row := range rows {
			copyRow := &neo4j.Record{Keys: append([]string(nil), row.Keys...), Values: append([]any(nil), row.Values...)}
			copyRow.Keys[3] = "payload_char_count"
			if payload, ok := row.Values[3].(string); ok {
				copyRow.Values[3] = int64(len([]rune(payload)))
			}
			rows[index] = copyRow
		}
	}
	return &neighborhoodProvenanceResult{rows: rows, position: -1}, nil
}

type neighborhoodProvenanceResult struct {
	neo4j.Result
	rows     []*neo4j.Record
	position int
}

func (r *neighborhoodProvenanceResult) Next(context.Context) bool {
	r.position++
	return r.position < len(r.rows)
}
func (r *neighborhoodProvenanceResult) Record() *neo4j.Record { return r.rows[r.position] }
func (*neighborhoodProvenanceResult) Err() error              { return nil }
func (*neighborhoodProvenanceResult) Consume(context.Context) (neo4j.ResultSummary, error) {
	return nil, nil
}

func neighborhoodProvenanceRow(owner string, source behaviorgraph.Record) *neo4j.Record {
	payload, _ := json.Marshal(source)
	digest := sha256.Sum256(payload)
	hash := hex.EncodeToString(digest[:])
	return &neo4j.Record{
		Keys:   []string{"owner_id", "record_id", "record_type", "payload_json", "payload_sha256", "invalid_links"},
		Values: []any{owner, "source-" + hash, "source", string(payload), hash, false},
	}
}

func TestNeighborhoodSamplesHighDegreeEntityProvenance(t *testing.T) {
	read := &neighborhoodRead{project: strings.Repeat("a", 64), records: make(map[string]neighborhoodRecord)}
	tx := &neighborhoodProvenanceTx{rows: make(map[string][]*neo4j.Record)}
	owners := make([]string, 0, 3)
	for owner := 0; owner < 3; owner++ {
		id := fmt.Sprintf("endpoint-%064x", owner+1)
		owners = append(owners, id)
		read.records[id] = neighborhoodRecord{id: id, kind: "entity", value: behaviorgraph.Record{"id": id, "kind": "endpoint", "data": map[string]any{}}}
		for index := 0; index < 7_000; index++ {
			source := behaviorgraph.Record{"path": "/synthetic/receipt.json", "sha256": strings.Repeat("b", 64), "record_locator": fmt.Sprintf("$.records[%d]", index)}
			tx.rows[id] = append(tx.rows[id], neighborhoodProvenanceRow(id, source))
		}
	}
	if err := read.loadProvenance(context.Background(), tx, owners, nil, nil); err != nil {
		t.Fatalf("small neighborhood failed because common entities have 21,000 provenance links: %v", err)
	}
	for _, id := range owners {
		refs := read.records[id].value["source_refs"].([]behaviorgraph.Record)
		if len(refs) == 0 || len(refs) > maxNeighborhoodEntityRefs {
			t.Fatalf("entity provenance was not bounded: got %d refs", len(refs))
		}
	}
	if got := read.truncatedEntityIDs(); len(got) != 3 || !sort.StringsAreSorted(got) {
		t.Fatalf("sampled entity provenance was not explicitly identified: %v", got)
	}
}

func neighborhoodUnitProvenance(owner string, count int) []*neo4j.Record {
	rows := make([]*neo4j.Record, 0, count)
	for index := 0; index < count; index++ {
		source := behaviorgraph.Record{"path": "/synthetic/receipt.json", "sha256": strings.Repeat("b", 64), "record_locator": fmt.Sprintf("$.records[%06d]", index)}
		rows = append(rows, neighborhoodProvenanceRow(owner, source))
	}
	return rows
}

func neighborhoodUnitEntity(owner string) *neighborhoodRead {
	return &neighborhoodRead{project: strings.Repeat("a", 64), records: map[string]neighborhoodRecord{
		owner: {id: owner, kind: "entity", value: behaviorgraph.Record{"id": owner, "kind": "endpoint", "data": map[string]any{}}},
	}}
}

func TestNeighborhoodEntityProvenanceContinuesAfterExcludedReferences(t *testing.T) {
	owner := "endpoint-" + strings.Repeat("1", 64)
	tx := &neighborhoodProvenanceTx{rows: map[string][]*neo4j.Record{owner: neighborhoodUnitProvenance(owner, 5)}}
	first := neighborhoodUnitEntity(owner)
	if err := first.loadEntityProvenanceBatch(context.Background(), tx, []string{owner}, 2, maxNeighborhoodBytes, nil, nil); err != nil {
		t.Fatalf("load first provenance page: %v", err)
	}
	firstRefs := first.records[owner].value["source_refs"].([]behaviorgraph.Record)
	if len(firstRefs) != 2 || first.truncatedEntities[owner] != true {
		t.Fatalf("first provenance page = %d refs, truncated=%v", len(firstRefs), first.truncatedEntityIDs())
	}
	firstIDs := neighborhoodSourceRecordIDs(firstRefs)
	excluded := []string{owner + "|" + firstIDs[0], owner + "|" + firstIDs[1]}
	anchor := []string{owner + "|" + firstIDs[0]}
	second := neighborhoodUnitEntity(owner)
	if err := second.loadEntityProvenanceBatch(context.Background(), tx, []string{owner}, 3, maxNeighborhoodBytes, excluded, anchor); err != nil {
		t.Fatalf("load second provenance page: %v", err)
	}
	secondRefs := second.records[owner].value["source_refs"].([]behaviorgraph.Record)
	if len(secondRefs) != 3 || second.truncatedEntities[owner] != true {
		t.Fatalf("second provenance page = %d refs, truncated=%v", len(secondRefs), second.truncatedEntityIDs())
	}
	secondIDs := neighborhoodSourceRecordIDs(secondRefs)
	if !slices.Contains(secondIDs, firstIDs[0]) || slices.Contains(secondIDs, firstIDs[1]) {
		t.Fatalf("second page did not retain exactly its anchor: %v", secondIDs)
	}
	for _, id := range secondIDs {
		if id != firstIDs[0] && slices.Contains(firstIDs, id) {
			t.Fatalf("second page repeated an already loaded reference %q", id)
		}
	}
	allExcluded := append(append([]string(nil), excluded...), owner+"|"+secondIDs[1], owner+"|"+secondIDs[2])
	last := neighborhoodUnitEntity(owner)
	if err := last.loadEntityProvenanceBatch(context.Background(), tx, []string{owner}, 3, maxNeighborhoodBytes, allExcluded, anchor); err != nil {
		t.Fatalf("load final provenance page: %v", err)
	}
	lastRefs := last.records[owner].value["source_refs"].([]behaviorgraph.Record)
	if len(lastRefs) != 2 || last.truncatedEntities[owner] {
		t.Fatalf("final provenance page = %d refs, truncated=%v", len(lastRefs), last.truncatedEntityIDs())
	}
}

func neighborhoodSourceRecordIDs(refs []behaviorgraph.Record) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		payload, _ := json.Marshal(ref)
		digest := sha256.Sum256(payload)
		ids = append(ids, "source-"+hex.EncodeToString(digest[:]))
	}
	sort.Strings(ids)
	return ids
}

func TestNeighborhoodEntityProvenanceReportsExactAndExceededLimits(t *testing.T) {
	owner := "endpoint-" + strings.Repeat("1", 64)
	for _, count := range []int{2, 4, 5} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			read := neighborhoodUnitEntity(owner)
			tx := &neighborhoodProvenanceTx{rows: map[string][]*neo4j.Record{owner: neighborhoodUnitProvenance(owner, count)}}
			if err := read.loadEntityProvenance(context.Background(), tx, owner, 4, maxNeighborhoodBytes); err != nil {
				t.Fatal(err)
			}
			refs := read.records[owner].value["source_refs"].([]behaviorgraph.Record)
			if len(refs) != min(count, 4) || read.truncatedEntities[owner] != (count > 4) {
				t.Fatalf("incorrect provenance coverage: refs=%d truncated=%v", len(refs), read.truncatedEntityIDs())
			}
		})
	}
}

func TestNeighborhoodEntityProvenanceRejectsMissingAndNullSourceRows(t *testing.T) {
	owner := "endpoint-" + strings.Repeat("1", 64)
	for name, rows := range map[string][]*neo4j.Record{
		"missing": {},
		"optional match null": {{
			Keys:   []string{"owner_id", "record_id", "record_type", "payload_char_count", "payload_sha256", "invalid_links"},
			Values: []any{owner, nil, nil, nil, nil, false},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			read := neighborhoodUnitEntity(owner)
			tx := &neighborhoodProvenanceTx{rows: map[string][]*neo4j.Record{owner: rows}}
			if err := read.loadEntityProvenance(context.Background(), tx, owner, 4, maxNeighborhoodBytes); err == nil {
				t.Fatal("accepted an entity without any validated source reference")
			}
		})
	}
}

func TestNeighborhoodEntityProvenanceHonorsRemainingByteAndRecordCaps(t *testing.T) {
	owner := "endpoint-" + strings.Repeat("1", 64)
	for _, budget := range []string{"bytes", "records"} {
		t.Run(budget, func(t *testing.T) {
			read := neighborhoodUnitEntity(owner)
			rows := neighborhoodUnitProvenance(owner, 3)
			byteBudget := maxNeighborhoodBytes
			if budget == "bytes" {
				byteBudget = len(rows[0].Values[3].(string)) * 8
				read.bytes = maxNeighborhoodBytes - byteBudget
			} else {
				for index := 0; len(read.records) < maxNeighborhoodRecords-1; index++ {
					id := fmt.Sprintf("existing-%d", index)
					read.records[id] = neighborhoodRecord{id: id}
				}
			}
			tx := &neighborhoodProvenanceTx{rows: map[string][]*neo4j.Record{owner: rows}}
			if err := read.loadEntityProvenance(context.Background(), tx, owner, 4, byteBudget); err != nil {
				t.Fatal(err)
			}
			refs := read.records[owner].value["source_refs"].([]behaviorgraph.Record)
			if len(refs) == 0 || len(refs) >= len(rows) || !read.truncatedEntities[owner] || read.bytes > maxNeighborhoodBytes || len(read.records) > maxNeighborhoodRecords {
				t.Fatalf("optional provenance did not stop safely at its %s budget", budget)
			}
		})
	}
}

func TestNeighborhoodProvenanceRetainsAllAssertionRefsBeforeEntitySamples(t *testing.T) {
	owner, assertion := "endpoint-"+strings.Repeat("1", 64), "assertion-"+strings.Repeat("2", 64)
	read := neighborhoodUnitEntity(owner)
	read.records[assertion] = neighborhoodRecord{id: assertion, kind: "assertion", value: behaviorgraph.Record{"id": assertion}}
	tx := &neighborhoodProvenanceTx{rows: map[string][]*neo4j.Record{
		owner: neighborhoodUnitProvenance(owner, 100), assertion: neighborhoodUnitProvenance(assertion, 100),
	}}
	if err := read.loadProvenance(context.Background(), tx, []string{owner, assertion}, nil, nil); err != nil {
		t.Fatal(err)
	}
	refs := read.records[assertion].value["source_refs"].([]behaviorgraph.Record)
	if len(refs) != 100 || !sort.SliceIsSorted(refs, func(i, j int) bool { return neighborhoodSourceKey(refs[i]) < neighborhoodSourceKey(refs[j]) }) || len(read.truncatedEntityIDs()) != 1 || read.truncatedEntityIDs()[0] != owner {
		t.Fatal("assertion provenance was truncated or its identity ordering changed")
	}
}

func TestNeighborhoodSampledSourceChecksumsRemainMandatory(t *testing.T) {
	owner := "endpoint-" + strings.Repeat("1", 64)
	read := neighborhoodUnitEntity(owner)
	rows := neighborhoodUnitProvenance(owner, 1)
	rows[0].Values[4] = strings.Repeat("0", 64)
	tx := &neighborhoodProvenanceTx{rows: map[string][]*neo4j.Record{owner: rows}}
	if err := read.loadProvenance(context.Background(), tx, []string{owner}, nil, nil); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("sampled source corruption was not rejected: %v", err)
	}
}
