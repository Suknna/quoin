package observation

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// LedgerSchema is the stable kind of the append-only typed observation
// ledger of one invocation.
const LedgerSchema = "quoin-ui-observation-ledger-v1"

// CellObservation is the typed observation of exactly one matrix cell: it
// answers every form field with the closed result vocabulary and binds the
// observer, the browser artifact digest/build actually observed and the
// observation window. Basis labels the provenance of the submission (a
// named human observer path or a labeled acceptance harness); it never
// replaces the typed values.
type CellObservation struct {
	CellID                string            `json:"cell_id"`
	Observer              string            `json:"observer"`
	Basis                 string            `json:"basis"`
	BrowserArtifactSHA256 string            `json:"browser_artifact_sha256"`
	BrowserBuild          string            `json:"browser_build"`
	ViewportCSSPx         int               `json:"viewport_css_px"`
	MotionMode            string            `json:"motion_mode"`
	StartedAt             string            `json:"started_at"`
	EndedAt               string            `json:"ended_at"`
	Results               map[string]string `json:"results"`
	FormDigest            string            `json:"form_digest"`
}

// Submission is one typed observation submission: a set of cell
// observations under a shared observer identity.
type Submission struct {
	Observer     string            `json:"observer"`
	Basis        string            `json:"basis"`
	Observations []CellObservation `json:"observations"`
}

// Ledger is the append-only observation record of one invocation.
type Ledger struct {
	Schema       string            `json:"schema"`
	InvocationID string            `json:"invocation_id"`
	ScenarioID   string            `json:"scenario_id"`
	Records      []CellObservation `json:"records"`
}

// LoadLedger reads an existing ledger; a missing file yields an empty one
// so the first record creates it. Every persisted record is re-validated
// against the matrix (closed fields, browser binding, window) and
// duplicate cell records are rejected: an append-only authority must not
// carry two records for one cell of one invocation.
func LoadLedger(path, invocationID, scenarioID string, matrix *Matrix) (*Ledger, error) {
	ledger := &Ledger{Schema: LedgerSchema, InvocationID: invocationID, ScenarioID: scenarioID}
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ledger, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, ledger); err != nil {
		return nil, fmt.Errorf("parse ledger %s: %w", path, err)
	}
	if ledger.Schema != LedgerSchema {
		return nil, fmt.Errorf("ledger %s carries schema %q, expected %q", path, ledger.Schema, LedgerSchema)
	}
	if ledger.InvocationID != invocationID || ledger.ScenarioID != scenarioID {
		return nil, fmt.Errorf("ledger %s is bound to invocation %s/%s, refusing to append %s/%s", path, ledger.InvocationID, ledger.ScenarioID, invocationID, scenarioID)
	}
	persisted := map[string]bool{}
	for _, record := range ledger.Records {
		cell := matrix.Cell(record.CellID)
		if cell == nil {
			return nil, fmt.Errorf("ledger %s records cell %q outside the %s matrix", path, record.CellID, scenarioID)
		}
		if err := validateAgainstCell(*cell, record); err != nil {
			return nil, fmt.Errorf("ledger %s record %s: %w", path, record.CellID, err)
		}
		if persisted[record.CellID] {
			return nil, fmt.Errorf("ledger %s carries duplicate records for cell %s", path, record.CellID)
		}
		persisted[record.CellID] = true
	}
	return ledger, nil
}

// Write persists the ledger atomically (temp file + rename) so a crashed
// record command can never leave a torn append authority behind.
func (ledger *Ledger) Write(path string) error {
	body, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// Record validates a typed submission against the matrix and the frozen
// forms and appends it. Replay of an identical observation is idempotent;
// a different observation for an already-recorded cell is a conflict, not
// an overwrite (no second result for one cell per invocation). When
// singleCell is set the submission must target exactly that catalog cell,
// so a native runner can never record another runner's cells.
func (ledger *Ledger) Record(matrix *Matrix, forms []Form, submission Submission, singleCell string, expectedObserver string) error {
	if submission.Observer == "" {
		return fmt.Errorf("observation submission carries no observer identity")
	}
	if expectedObserver != "" && submission.Observer != expectedObserver {
		return fmt.Errorf("submission observer %q does not match the invocation observer %q", submission.Observer, expectedObserver)
	}
	if len(submission.Observations) == 0 {
		return fmt.Errorf("observation submission is empty")
	}
	byCell := map[string]Form{}
	for _, form := range forms {
		byCell[form.CellID] = form
	}
	if len(byCell) == 0 {
		return fmt.Errorf("no generated forms: record requires the catalog-bound observation forms of this invocation")
	}
	if singleCell != "" && len(submission.Observations) != 1 {
		return fmt.Errorf("cell-scoped recording expects exactly one observation, submission carries %d", len(submission.Observations))
	}
	existing := map[string]CellObservation{}
	for _, record := range ledger.Records {
		existing[record.CellID] = record
	}
	for _, observation := range submission.Observations {
		if observation.Observer != submission.Observer {
			return fmt.Errorf("observation of %s carries observer %q, submission observer is %q", observation.CellID, observation.Observer, submission.Observer)
		}
		if singleCell != "" && observation.CellID != singleCell {
			return fmt.Errorf("cell-scoped recording expected %s, submission targets %s", singleCell, observation.CellID)
		}
		cell := matrix.Cell(observation.CellID)
		if cell == nil {
			return fmt.Errorf("observation targets %q which is outside the %s matrix", observation.CellID, matrix.ScenarioID)
		}
		if err := validateAgainstCell(*cell, observation); err != nil {
			return err
		}
		form, bound := byCell[observation.CellID]
		if !bound {
			return fmt.Errorf("cell %s has no generated form in this invocation: generate the forms before recording", observation.CellID)
		}
		if err := validateAgainstForm(form, observation); err != nil {
			return err
		}
		observation.FormDigest = FormDigest(form)
		if previous, ok := existing[observation.CellID]; ok {
			if ObservationDigest(previous) == ObservationDigest(observation) {
				continue // identical replay: idempotent no-op
			}
			return fmt.Errorf("cell %s already has a different observation in this invocation: conflict, not overwrite", observation.CellID)
		}
		ledger.Records = append(ledger.Records, observation)
		existing[observation.CellID] = observation
	}
	return nil
}

// validateAgainstForm proves the recorded observation binds exactly the
// invocation, subject digest and browser artifact of the generated form;
// an observation claiming a different browser build never enters the
// ledger.
func validateAgainstForm(form Form, observation CellObservation) error {
	if form.InvocationID == "" || form.CellID != observation.CellID {
		return fmt.Errorf("form cell %s does not match observation %s", form.CellID, observation.CellID)
	}
	if observation.BrowserArtifactSHA256 != form.BrowserArtifactSHA256 {
		return fmt.Errorf("observation of %s binds artifact digest %s, the form froze %s", observation.CellID, observation.BrowserArtifactSHA256, form.BrowserArtifactSHA256)
	}
	if form.BrowserBuild != "" && observation.BrowserBuild != form.BrowserBuild {
		return fmt.Errorf("observation of %s binds build %q, the form froze %q", observation.CellID, observation.BrowserBuild, form.BrowserBuild)
	}
	return nil
}

// validateAgainstCell proves one observation answers exactly the closed
// field set with the closed vocabulary and binds the cell's browser
// subject digest, viewport and motion mode.
func validateAgainstCell(cell Cell, observation CellObservation) error {
	if len(observation.Results) != len(cell.Assertions) {
		return fmt.Errorf("observation of %s answers %d fields, the cell has %d", cell.ID, len(observation.Results), len(cell.Assertions))
	}
	for _, assertion := range cell.Assertions {
		result, ok := observation.Results[assertion.ID]
		if !ok {
			return fmt.Errorf("observation of %s is missing typed field %q", cell.ID, assertion.ID)
		}
		if result != FieldResultPassed && result != FieldResultFailed {
			return fmt.Errorf("observation of %s answers field %q with %q: outside the closed vocabulary %s|%s", cell.ID, assertion.ID, result, FieldResultPassed, FieldResultFailed)
		}
	}
	if observation.BrowserArtifactSHA256 == "" || observation.BrowserBuild == "" {
		return fmt.Errorf("observation of %s does not bind the observed browser artifact digest/build", cell.ID)
	}
	if observation.ViewportCSSPx != cell.Key.ViewportCSSPx || observation.MotionMode != cell.Key.MotionMode {
		return fmt.Errorf("observation of %s binds viewport %d/%s, the cell is %d/%s", cell.ID, observation.ViewportCSSPx, observation.MotionMode, cell.Key.ViewportCSSPx, cell.Key.MotionMode)
	}
	started, err := time.Parse(time.RFC3339, observation.StartedAt)
	if err != nil {
		return fmt.Errorf("observation of %s has malformed started_at: %w", cell.ID, err)
	}
	ended, err := time.Parse(time.RFC3339, observation.EndedAt)
	if err != nil {
		return fmt.Errorf("observation of %s has malformed ended_at: %w", cell.ID, err)
	}
	if ended.Before(started) {
		return fmt.Errorf("observation of %s ends before it starts", cell.ID)
	}
	return nil
}

// CellOutcome evaluates one cell's recorded observation against the frozen
// catalog expectations. The comparison lives here — with the deterministic
// verifier, never with the observer (VERIFY-VERDICT-004).
type CellOutcome struct {
	CellID     string
	Outcome    string // passed | failed
	Observer   string
	Results    map[string]string
	DetailCode string
}

// EvaluateCell returns the outcome of one cell; ok=false means the cell has
// no recorded observation yet (not_run, WARNED at suite level).
func (ledger *Ledger) EvaluateCell(cell Cell) (CellOutcome, bool) {
	for _, record := range ledger.Records {
		if record.CellID != cell.ID {
			continue
		}
		outcome := CellOutcome{CellID: cell.ID, Outcome: "passed", Observer: record.Observer, Results: record.Results}
		for _, assertion := range cell.Assertions {
			expected, ok := assertion.Expected.(string)
			if !ok {
				expected = fmt.Sprint(assertion.Expected)
			}
			if record.Results[assertion.ID] != expected {
				outcome.Outcome = "failed"
				outcome.DetailCode = "typed_observation_failed:" + assertion.ID
			}
		}
		return outcome, true
	}
	return CellOutcome{}, false
}
