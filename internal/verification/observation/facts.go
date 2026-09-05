package observation

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Suknna/quoin/internal/verification/evidence"
	"github.com/Suknna/quoin/internal/verification/result"
	"github.com/Suknna/quoin/internal/verification/suites"
)

// ResultsSchema is the stable kind of the automation matrix results the
// Playwright @ticket-41 spec writes once per invocation.
const ResultsSchema = "quoin-ui-automation-results-v1"

// AssertionResult is one automated aspect outcome of one cell
// (dom-contracts, keyboard-navigation, focus-visibility-and-occlusion,
// target-size, responsive-reflow, axe-rules, domain-behavior).
type AssertionResult struct {
	Result string `json:"result"` // passed | failed
	Detail string `json:"detail,omitempty"`
}

// AutomatedCell is the recorded automation pass of one matrix cell.
type AutomatedCell struct {
	CellID     string                     `json:"cell_id"`
	Browser    BrowserSubject             `json:"browser"`
	StartedAt  string                     `json:"started_at"`
	FinishedAt string                     `json:"finished_at"`
	Assertions map[string]AssertionResult `json:"assertions"`
	Artifacts  map[string]string          `json:"artifacts"` // screenshot / trace paths
}

// AutomationResults is the whole matrix pass document.
type AutomationResults struct {
	Schema       string          `json:"schema"`
	InvocationID string          `json:"invocation_id"`
	GeneratedAt  string          `json:"generated_at"`
	Cells        []AutomatedCell `json:"cells"`
}

// LoadAutomationResults reads and shape-checks the results document;
// duplicate cell records are rejected because cell selection must never
// be order-dependent.
func LoadAutomationResults(path string) (*AutomationResults, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var results AutomationResults
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("parse automation results %s: %w", path, err)
	}
	if results.Schema != ResultsSchema {
		return nil, fmt.Errorf("%s carries schema %q, expected %q", path, results.Schema, ResultsSchema)
	}
	if results.InvocationID == "" {
		return nil, fmt.Errorf("%s carries no invocation binding", path)
	}
	seen := map[string]bool{}
	for _, cell := range results.Cells {
		if seen[cell.CellID] {
			return nil, fmt.Errorf("%s carries duplicate records for cell %s", path, cell.CellID)
		}
		seen[cell.CellID] = true
	}
	return &results, nil
}

// EvaluateAutomatedCell proves one executed cell against the catalog:
// exact assertion set with the closed result vocabulary and a browser
// binding equal to the frozen subject. It is the single evaluation both
// the facts writer and the workflow summarize use, so a malformed or
// unbound record can never be reported as passed.
func EvaluateAutomatedCell(matrix *Matrix, results *AutomationResults, subject BrowserSubject) (*AutomatedCell, error) {
	cell := matrix.Cell(subject.CellID)
	if cell == nil {
		return nil, fmt.Errorf("subject cell %s is outside the %s matrix", subject.CellID, matrix.ScenarioID)
	}
	executed := results.Cell(subject.CellID)
	if executed == nil {
		return nil, fmt.Errorf("automation results carry no executed cell %s", subject.CellID)
	}
	if executed.Browser.SHA256 != subject.SHA256 || executed.Browser.Build != subject.Build {
		return nil, fmt.Errorf("cell %s executed browser %s/%s does not match the frozen subject %s/%s", subject.CellID, executed.Browser.SHA256, executed.Browser.Build, subject.SHA256, subject.Build)
	}
	if executed.Browser.BrowserSubject != cell.Key.BrowserSubject || executed.Browser.Architecture != cell.Key.Architecture {
		return nil, fmt.Errorf("cell %s executed browser subject/arch %s/%s contradicts the catalog cell", subject.CellID, executed.Browser.BrowserSubject, executed.Browser.Architecture)
	}
	if len(executed.Assertions) != len(cell.Assertions) {
		return nil, fmt.Errorf("cell %s answers %d assertions, the catalog declares %d", subject.CellID, len(executed.Assertions), len(cell.Assertions))
	}
	for _, assertion := range cell.Assertions {
		result, ok := executed.Assertions[assertion.ID]
		if !ok {
			return nil, fmt.Errorf("cell %s automation produced no result for assertion %q", subject.CellID, assertion.ID)
		}
		if result.Result != FieldResultPassed && result.Result != FieldResultFailed {
			return nil, fmt.Errorf("cell %s assertion %q carries %q: outside the closed vocabulary", subject.CellID, assertion.ID, result.Result)
		}
	}
	return executed, nil
}

// Cell returns one cell's automation result.
func (results *AutomationResults) Cell(cellID string) *AutomatedCell {
	for index := range results.Cells {
		if results.Cells[index].CellID == cellID {
			return &results.Cells[index]
		}
	}
	return nil
}

// cellFacts builds the quoin-verify-facts-v1 assertions map for one cell
// from id→actual pairs, checking the closed assertion vocabulary of the
// catalog cell first: a fact for an undeclared assertion never reaches the
// coordinator, and a missing one fails closed.
func cellFacts(cell Cell, actuals map[string]string) (map[string]any, []map[string]string, error) {
	facts := map[string]any{}
	checks := make([]map[string]string, 0, len(cell.Assertions))
	for _, assertion := range cell.Assertions {
		actual, ok := actuals[assertion.ID]
		if !ok {
			return nil, nil, fmt.Errorf("cell %s automation/observation produced no fact for assertion %q", cell.ID, assertion.ID)
		}
		facts[assertion.ID] = actual
		checks = append(checks, map[string]string{"name": assertion.ID, "result": actual})
	}
	return facts, checks, nil
}

// WriteAutomationFacts validates one cell's automation result against the
// catalog cell (aspect set) and the cell's frozen browser subject (digest,
// build, viewport, motion mode), then writes the coordinator-compatible
// facts document. The coordinator stays the comparison authority: the
// facts only freeze the machine-observed actuals.
func WriteAutomationFacts(matrix *Matrix, results *AutomationResults, subject BrowserSubject, factsPath string) error {
	cell := matrix.Cell(subject.CellID)
	if cell == nil {
		return fmt.Errorf("subject cell %s is outside the %s matrix", subject.CellID, matrix.ScenarioID)
	}
	executed, err := EvaluateAutomatedCell(matrix, results, subject)
	if err != nil {
		return err
	}
	actuals := map[string]string{}
	for id, assertion := range executed.Assertions {
		actuals[id] = assertion.Result
	}
	facts, checks, err := cellFacts(*cell, actuals)
	if err != nil {
		return err
	}
	return suites.WriteFacts(factsPath, facts, checks)
}

// WriteObservationFacts writes the coordinator-compatible facts document
// for one typed observation cell (human_observation assertions): the
// recorded typed values are the machine-observed actuals; the coordinator
// or the local evaluation stays the comparison authority.
func WriteObservationFacts(matrix *Matrix, ledger *Ledger, cellID, factsPath string) (CellOutcome, error) {
	cell := matrix.Cell(cellID)
	if cell == nil {
		return CellOutcome{}, fmt.Errorf("cell %s is outside the %s matrix", cellID, matrix.ScenarioID)
	}
	outcome, ok := ledger.EvaluateCell(*cell)
	if !ok {
		return CellOutcome{}, fmt.Errorf("cell %s has no recorded typed observation", cellID)
	}
	actuals := map[string]string{}
	for _, assertion := range cell.Assertions {
		actuals[assertion.ID] = outcome.Results[assertion.ID]
	}
	facts, checks, err := cellFacts(*cell, actuals)
	if err != nil {
		return CellOutcome{}, err
	}
	if err := suites.WriteFacts(factsPath, facts, checks); err != nil {
		return CellOutcome{}, err
	}
	return outcome, nil
}

// ObservationSummary projects the ledger over the whole matrix using the
// frozen result-profile summary shape. Cells without an observation count
// as warned candidates: the caller decides whether the invocation may
// close (a delegated native runner still owes them).
func (ledger *Ledger) ObservationSummary(matrix *Matrix) (result.ObservationSummary, int) {
	summary := result.ObservationSummary{RequiredCells: len(matrix.Cells)}
	missing := 0
	for _, cell := range matrix.Cells {
		outcome, ok := ledger.EvaluateCell(cell)
		if !ok {
			missing++
			continue
		}
		switch outcome.Outcome {
		case "passed":
			summary.PassedCells++
		case "failed":
			summary.FailedCells++
		default:
			summary.WarnedCells++
		}
	}
	// not_run cells aggregate as WARNED candidates (VERIFY-VERDICT-002);
	// the missing count lets the caller distinguish delegated from ignored.
	summary.WarnedCells += missing
	return summary, missing
}

// SubjectDigestFile digests an evidence attachment (sha256 + size bytes).
func SubjectDigestFile(path string) (string, int64, error) {
	return evidence.DigestFile(path)
}
