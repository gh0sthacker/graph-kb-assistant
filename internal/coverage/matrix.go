// Package coverage tracks declared vulnerability-class coverage for a run.
// A coverage result is not a claim that the target has no vulnerabilities.
package coverage

import (
	"encoding/json"
	"sort"
)

const (
	Confirmed         = "confirmed"
	RuledOut          = "ruled_out"
	Untested          = "untested"
	BlockedFirstParty = "blocked_first_party"
)

// Cell records attempted tests and the declared test budget for one class.
// Evidence should contain references, not raw traffic or secrets.
type Cell struct {
	Class    string   `json:"class"`
	Status   string   `json:"status"`
	Tests    int      `json:"tests"`
	Budget   int      `json:"budget"`
	Evidence []string `json:"evidence"`
}

// Matrix is a run's declared class set. Its zero value is ready for recording.
// Callers must serialize mutations; Matrix is not concurrency-safe.
type Matrix struct {
	Run   string
	Cells map[string]*Cell
}

func validStatus(status string) bool {
	switch status {
	case Confirmed, RuledOut, Untested, BlockedFirstParty:
		return true
	}
	return false
}

func (m *Matrix) cell(class string) *Cell {
	if m.Cells == nil {
		m.Cells = make(map[string]*Cell)
	}
	if m.Cells[class] == nil {
		m.Cells[class] = &Cell{Class: class, Status: Untested, Evidence: []string{}}
	}
	return m.Cells[class]
}

// Record counts one attempt with a valid outcome and appends nonempty evidence.
// Confirmed findings are sticky: later negative or blocked attempts cannot erase
// them. Invalid outcomes and empty class identifiers are ignored.
func (m *Matrix) Record(class, outcome, evidence string) {
	if class == "" || !validStatus(outcome) {
		return
	}
	cell := m.cell(class)
	cell.Tests++
	if cell.Status != Confirmed {
		cell.Status = outcome
	}
	if evidence != "" {
		cell.Evidence = append(cell.Evidence, evidence)
	}
}

// SetBudget declares a class even before any tests run. Negative budgets are
// clamped to zero; an empty class is ignored.
func (m *Matrix) SetBudget(class string, n int) {
	if class == "" {
		return
	}
	if n < 0 {
		n = 0
	}
	m.cell(class).Budget = n
}

// CanFinish reports whether every declared test budget has been met. Untested
// classes with a positive budget remain open even after recorded attempts.
// Blocked attempts can exhaust a budget, but never establish a negative finding.
// Completion covers only the declared class set, not absence of vulnerabilities.
func (m Matrix) CanFinish() (bool, []string) {
	open := []string{}
	for class, cell := range m.Cells {
		if cell == nil || !validStatus(cell.Status) || cell.Tests < cell.Budget || (cell.Status == Untested && cell.Budget > 0) {
			open = append(open, class)
		}
	}
	sort.Strings(open)
	return len(open) == 0, open
}

// Pct is the percentage of declared classes with a confirmed or ruled-out
// outcome. Untested and blocked classes are never counted as covered, even
// when their budgets are zero or exhausted. An empty matrix has zero coverage.
func (m Matrix) Pct() float64 {
	if len(m.Cells) == 0 {
		return 0
	}
	resolved := 0
	for _, cell := range m.Cells {
		if cell != nil && (cell.Status == Confirmed || cell.Status == RuledOut) {
			resolved++
		}
	}
	return 100 * float64(resolved) / float64(len(m.Cells))
}

// FindingsPerCall returns confirmed classes divided by the number of calls.
// Nonpositive call counts have a zero rate.
func (m Matrix) FindingsPerCall(calls int) float64 {
	if calls <= 0 {
		return 0
	}
	findings := 0
	for _, cell := range m.Cells {
		if cell != nil && cell.Status == Confirmed {
			findings++
		}
	}
	return float64(findings) / float64(calls)
}

// MarshalJSON emits a stable, class-sorted cell array and computed completion
// fields. Empty arrays are [], never null. It does not mutate the matrix.
func (m Matrix) MarshalJSON() ([]byte, error) {
	classes := make([]string, 0, len(m.Cells))
	for class := range m.Cells {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	cells := make([]Cell, 0, len(classes))
	for _, class := range classes {
		cell := Cell{Class: class, Status: Untested, Evidence: []string{}}
		if m.Cells[class] != nil {
			cell = *m.Cells[class]
		}
		if cell.Evidence == nil {
			cell.Evidence = []string{}
		}
		cells = append(cells, cell)
	}
	canFinish, open := m.CanFinish()
	return json.Marshal(struct {
		Run         string   `json:"run"`
		Cells       []Cell   `json:"cells"`
		CoveragePct float64  `json:"coverage_pct"`
		CanFinish   bool     `json:"can_finish"`
		Open        []string `json:"open"`
	}{m.Run, cells, m.Pct(), canFinish, open})
}
