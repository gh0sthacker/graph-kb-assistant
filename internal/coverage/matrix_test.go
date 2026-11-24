package coverage

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCanFinishAndMetrics(t *testing.T) {
	m := Matrix{Run: "run-1"}
	m.SetBudget("xss", 2)
	m.SetBudget("sqli", 1)
	m.SetBudget("idor", 0)
	if ok, open := m.CanFinish(); ok || !reflect.DeepEqual(open, []string{"sqli", "xss"}) {
		t.Fatalf("initial CanFinish() = %v, %v", ok, open)
	}
	m.Record("xss", Confirmed, "request-1")
	m.Record("sqli", RuledOut, "request-2")
	if ok, open := m.CanFinish(); ok || !reflect.DeepEqual(open, []string{"xss"}) {
		t.Fatalf("under-budget confirmed CanFinish() = %v, %v", ok, open)
	}
	m.Record("xss", RuledOut, "request-3")
	if ok, open := m.CanFinish(); !ok || len(open) != 0 {
		t.Fatalf("complete CanFinish() = %v, %v", ok, open)
	}
	if got := m.Pct(); got < 66.66 || got > 66.67 {
		t.Fatalf("Pct() = %v", got)
	}
	if got := m.FindingsPerCall(4); got != 0.25 {
		t.Fatalf("FindingsPerCall(4) = %v", got)
	}
	for _, calls := range []int{0, -1} {
		if got := m.FindingsPerCall(calls); got != 0 {
			t.Fatalf("FindingsPerCall(%d) = %v", calls, got)
		}
	}
	m.SetBudget("blocked", 2)
	m.Record("blocked", BlockedFirstParty, "first-party unavailable")
	if ok, open := m.CanFinish(); ok || !reflect.DeepEqual(open, []string{"blocked"}) {
		t.Fatalf("blocked under budget = %v, %v", ok, open)
	}
	m.Record("blocked", BlockedFirstParty, "")
	if ok, open := m.CanFinish(); !ok || len(open) != 0 {
		t.Fatalf("blocked budget met = %v, %v", ok, open)
	}
	m.SetBudget("untested", 1)
	m.Record("untested", Untested, "no usable test")
	if ok, open := m.CanFinish(); ok || !reflect.DeepEqual(open, []string{"untested"}) {
		t.Fatalf("untested budget met = %v, %v", ok, open)
	}
	m.SetBudget("untested", -2)
	if m.Cells["untested"].Budget != 0 {
		t.Fatal("negative budget was not clamped")
	}
	m.SetBudget("", 1)
	if m.Cells[""] != nil {
		t.Fatal("empty class budget accepted")
	}
}

func TestEmptyMetrics(t *testing.T) {
	m := Matrix{}
	if got := m.Pct(); got != 0 {
		t.Fatalf("Pct() = %v", got)
	}
	if got := m.FindingsPerCall(1); got != 0 {
		t.Fatalf("FindingsPerCall(1) = %v", got)
	}
}

func TestJSONSchema(t *testing.T) {
	m := Matrix{Run: "demo"}
	m.SetBudget("xss", 1)
	m.SetBudget("idor", 2)
	m.SetBudget("sqli", 1)
	m.Record("xss", Confirmed, "req-1")
	m.Record("sqli", RuledOut, "req-2")
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"run":"demo","cells":[{"class":"idor","status":"untested","tests":0,"budget":2,"evidence":[]},{"class":"sqli","status":"ruled_out","tests":1,"budget":1,"evidence":["req-2"]},{"class":"xss","status":"confirmed","tests":1,"budget":1,"evidence":["req-1"]}],"coverage_pct":66.66666666666667,"can_finish":false,"open":["idor"]}`
	if string(encoded) != want {
		t.Fatalf("JSON = %s; want %s", encoded, want)
	}
	encoded, err = json.Marshal(Matrix{Run: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"run":"empty","cells":[],"coverage_pct":0,"can_finish":true,"open":[]}` {
		t.Fatalf("empty JSON = %s", encoded)
	}
}

func TestRecordOutcomes(t *testing.T) {
	var m Matrix
	for _, outcome := range []string{"confirmed", "ruled_out", "untested", "blocked_first_party"} {
		m.Record(outcome, outcome, "evidence-"+outcome)
		cell := m.Cells[outcome]
		if cell == nil || cell.Class != outcome || cell.Status != outcome || cell.Tests != 1 || !reflect.DeepEqual(cell.Evidence, []string{"evidence-" + outcome}) {
			t.Fatalf("Record(%q) = %+v", outcome, cell)
		}
	}
	m.Record("confirmed", "ruled_out", "second-attempt")
	if cell := m.Cells["confirmed"]; cell.Status != "confirmed" || cell.Tests != 2 || len(cell.Evidence) != 2 {
		t.Fatalf("confirmed finding was erased or attempt not counted: %+v", cell)
	}
	m.Record("bad", "invalid", "")
	m.Record("", "confirmed", "")
	if len(m.Cells) != 4 {
		t.Fatalf("invalid record created a cell: %+v", m.Cells)
	}
}
