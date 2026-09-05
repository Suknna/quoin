package observation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"gopkg.in/yaml.v3"
)

// The frozen catalog is the authority for every test in this package: the
// 24-cell matrix, the assertion vocabularies and the expectations are read
// from it, never re-declared here.
const frozenCatalog = "../../../docs/specs/quoin-v1/contracts/verification-catalog.yaml"

func loadFrozen(t *testing.T) *catalog.Catalog {
	t.Helper()
	loaded, err := catalog.LoadAndValidate(frozenCatalog)
	if err != nil {
		t.Fatalf("frozen catalog rejected: %v", err)
	}
	return loaded
}

func TestMatrixOfDerivesTheFrozen24Cells(t *testing.T) {
	loaded := loadFrozen(t)
	for _, scenarioID := range []string{ScenarioAutomated, ScenarioObservation} {
		matrix, err := MatrixOf(loaded, scenarioID)
		if err != nil {
			t.Fatalf("%s: %v", scenarioID, err)
		}
		if len(matrix.Cells) != 24 {
			t.Fatalf("%s carries %d cells, expected the fixed 24", scenarioID, len(matrix.Cells))
		}
		// The frozen product: two chromium architectures + branded amd64,
		// four viewports, two motion modes — never multiplied by a backend.
		counts := map[string]int{}
		for _, cell := range matrix.Cells {
			counts[cell.Key.BrowserSubject+"/"+cell.Key.Architecture]++
		}
		if counts[SubjectPlaywrightChromium+"/linux/amd64"] != 8 ||
			counts[SubjectPlaywrightChromium+"/linux/arm64"] != 8 ||
			counts[SubjectBrandedChrome+"/linux/amd64"] != 8 {
			t.Fatalf("%s subject/arch distribution is not the frozen matrix: %v", scenarioID, counts)
		}
	}
}

func TestMatrixOfRejectsDriftedCells(t *testing.T) {
	loaded := loadFrozen(t)
	if loaded.Scenario(ScenarioObservation) == nil {
		t.Fatal("observation scenario missing")
	}
	withScenario := func(mutate func(scenario *catalog.Scenario)) func(*catalog.Catalog) {
		return func(document *catalog.Catalog) {
			mutate(document.Scenario(ScenarioObservation))
		}
	}
	mutations := []struct {
		name   string
		mutate func(*catalog.Catalog)
	}{
		{"unknown-id", withScenario(func(scenario *catalog.Scenario) {
			scenario.Cells[0].ID = "firefox-linux-amd64-320-normal"
		})},
		{"parameter-mismatch", withScenario(func(scenario *catalog.Scenario) {
			scenario.Cells[0].Parameters["viewport_width_css_px"] = 999
		})},
		{"motion-mismatch", withScenario(func(scenario *catalog.Scenario) {
			scenario.Cells[0].Parameters["motion_mode"] = "auto"
		})},
		{"architecture-mismatch", withScenario(func(scenario *catalog.Scenario) {
			scenario.Cells[0].Architecture = "linux/arm64"
		})},
		{"dropped-cell", withScenario(func(scenario *catalog.Scenario) {
			scenario.Cells = scenario.Cells[1:]
		})},
		{"duplicate-cell", withScenario(func(scenario *catalog.Scenario) {
			scenario.Cells[1].ID = scenario.Cells[0].ID
		})},
		{"empty-assertions", withScenario(func(scenario *catalog.Scenario) {
			scenario.Cells[0].Assertions = nil
		})},
	}
	for _, mutation := range mutations {
		clone := roundTripCatalog(t, loaded, mutation.mutate)
		if _, err := MatrixOf(clone, ScenarioObservation); err == nil {
			t.Fatalf("%s mutation accepted", mutation.name)
		}
	}
}

// roundTripCatalog clones the loaded catalog through YAML round-trips,
// applies the mutation and reloads it with strict decoding; the mutation
// tests exercise MatrixOf's own rules, so the schema gate stays with the
// frozen original. Both passes go through catalog.Load so the scenario
// index exists when the mutation runs.
func roundTripCatalog(t *testing.T, loaded *catalog.Catalog, mutate func(*catalog.Catalog)) *catalog.Catalog {
	t.Helper()
	first, err := yaml.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(t.TempDir(), "clone.yaml")
	if err := os.WriteFile(firstPath, first, 0o644); err != nil {
		t.Fatal(err)
	}
	clone, err := catalog.Load(firstPath)
	if err != nil {
		t.Fatalf("round-tripped catalog no longer parses: %v", err)
	}
	mutate(clone)
	mutated, err := yaml.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	mutatedPath := filepath.Join(t.TempDir(), "mutated.yaml")
	if err := os.WriteFile(mutatedPath, mutated, 0o644); err != nil {
		t.Fatal(err)
	}
	mutatedClone, err := catalog.Load(mutatedPath)
	if err != nil {
		t.Fatalf("mutated catalog no longer parses: %v", err)
	}
	return mutatedClone
}

func TestFormsBindEveryCellToSubjectAndInvocation(t *testing.T) {
	loaded := loadFrozen(t)
	matrix, err := MatrixOf(loaded, ScenarioObservation)
	if err != nil {
		t.Fatal(err)
	}
	binding := FormBinding{
		Tag:                  "v0.1.0-test",
		InvocationID:         "inv-1",
		ReleaseSubjectDigest: "deadbeef",
		PublicOriginDigest:   "cafebabe",
		BrowserSubjects:      map[string]BrowserSubject{},
	}
	for _, cell := range matrix.Cells {
		binding.BrowserSubjects[cell.ID] = BrowserSubject{
			Schema: SubjectSchema, CellID: cell.ID,
			BrowserSubject: cell.Key.BrowserSubject, Architecture: cell.Key.Architecture,
			SHA256: strings.Repeat("a", 64), Build: "Chromium 151 test", ExecutablePath: "/tmp/chrome",
		}
	}
	forms, err := Forms(matrix, binding)
	if err != nil {
		t.Fatal(err)
	}
	if len(forms) != 24 {
		t.Fatalf("generated %d forms", len(forms))
	}
	for _, form := range forms {
		if form.BrowserArtifactSHA256 == "" || form.BrowserBuild == "" || form.InvocationID != "inv-1" {
			t.Fatalf("form %s is not fully bound: %+v", form.CellID, form)
		}
		if len(form.Fields) != 3 {
			t.Fatalf("form %s carries %d typed fields, expected the three human_observation fields", form.CellID, len(form.Fields))
		}
	}
	// A subject without the frozen artifact digest must fail closed; an
	// empty build string is allowed (delegated native runner completes it).
	incomplete := binding
	incomplete.BrowserSubjects[matrix.Cells[0].ID] = BrowserSubject{CellID: matrix.Cells[0].ID}
	if _, err := Forms(matrix, incomplete); err == nil {
		t.Fatal("form generated for a cell without the frozen artifact digest")
	}
	unverifiedBuild := binding
	unverifiedBuild.BrowserSubjects[matrix.Cells[0].ID] = BrowserSubject{
		CellID: matrix.Cells[0].ID, SHA256: strings.Repeat("a", 64),
	}
	if _, err := Forms(matrix, unverifiedBuild); err != nil {
		t.Fatalf("form rejected for a delegated cell without an observed build: %v", err)
	}
}

func TestLedgerRecordsAndValidatesClosedFields(t *testing.T) {
	loaded := loadFrozen(t)
	matrix, err := MatrixOf(loaded, ScenarioObservation)
	if err != nil {
		t.Fatal(err)
	}
	cell := matrix.Cells[0]
	const artifactDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const buildString = "Chromium 151 test"
	forms := generatedForms(t, matrix, artifactDigest, buildString)
	submission := Submission{
		Observer: "acceptance-harness",
		Basis:    "labeled deterministic acceptance basis",
		Observations: []CellObservation{{
			CellID:                cell.ID,
			Observer:              "acceptance-harness",
			Basis:                 "labeled deterministic acceptance basis",
			BrowserArtifactSHA256: artifactDigest,
			BrowserBuild:          buildString,
			ViewportCSSPx:         cell.Key.ViewportCSSPx,
			MotionMode:            cell.Key.MotionMode,
			StartedAt:             "2026-09-05T10:00:00Z",
			EndedAt:               "2026-09-05T10:02:00Z",
			Results:               map[string]string{},
		}},
	}
	for _, assertion := range cell.Assertions {
		submission.Observations[0].Results[assertion.ID] = FieldResultPassed
	}
	ledger := &Ledger{Schema: LedgerSchema, InvocationID: "inv-1", ScenarioID: ScenarioObservation}
	if err := ledger.Record(matrix, forms, submission, "", ""); err != nil {
		t.Fatalf("valid submission rejected: %v", err)
	}
	if len(ledger.Records) != 1 {
		t.Fatalf("ledger carries %d records", len(ledger.Records))
	}
	if ledger.Records[0].FormDigest == "" {
		t.Fatal("record does not bind the generated form digest")
	}
	// Identical replay is idempotent.
	if err := ledger.Record(matrix, forms, submission, "", ""); err != nil {
		t.Fatalf("idempotent replay rejected: %v", err)
	}
	if len(ledger.Records) != 1 {
		t.Fatalf("replay appended a second record")
	}
	// A different observation for the same cell is a conflict. The clone
	// must deep-copy the observation: the Results map is otherwise shared
	// with the recorded submission.
	conflicting := submission
	conflicting.Observations = []CellObservation{cloneObservation(submission.Observations[0])}
	conflicting.Observations[0].Results[cell.Assertions[0].ID] = FieldResultFailed
	if err := ledger.Record(matrix, forms, conflicting, "", ""); err == nil {
		t.Fatal("conflicting observation accepted")
	}

	// Closed-field negatives, each against a fresh unobserved cell so the
	// duplicate/conflict rule cannot mask the field violations.
	unobserved := func() Cell {
		for _, candidate := range matrix.Cells {
			seen := false
			for _, record := range ledger.Records {
				if record.CellID == candidate.ID {
					seen = true
				}
			}
			if !seen {
				return candidate
			}
		}
		t.Fatal("no unobserved cell left")
		return Cell{}
	}
	negatives := []func(*CellObservation){
		func(o *CellObservation) { o.Results[cell.Assertions[0].ID] = "looks-fine-to-me" },
		func(o *CellObservation) { delete(o.Results, cell.Assertions[0].ID) },
		func(o *CellObservation) { o.Results["extra-field"] = FieldResultPassed },
		func(o *CellObservation) { o.BrowserArtifactSHA256 = "" },
		func(o *CellObservation) { o.BrowserBuild = "" },
		func(o *CellObservation) { o.ViewportCSSPx = 1280 },
		func(o *CellObservation) { o.EndedAt = "2026-09-05T09:59:00Z" },
		func(o *CellObservation) { o.Observer = "someone-else" },
		func(o *CellObservation) { o.BrowserArtifactSHA256 = strings.Repeat("b", 64) },
	}
	for _, mutate := range negatives {
		fresh := unobserved()
		broken := submission
		broken.Observations = []CellObservation{cloneObservation(submission.Observations[0])}
		broken.Observations[0].CellID = fresh.ID
		broken.Observations[0].ViewportCSSPx = fresh.Key.ViewportCSSPx
		broken.Observations[0].MotionMode = fresh.Key.MotionMode
		mutate(&broken.Observations[0])
		if err := ledger.Record(matrix, forms, broken, "", ""); err == nil {
			t.Fatal("invalid submission accepted")
		}
	}
	// An unknown cell id is rejected outright.
	unknown := submission
	unknown.Observations = []CellObservation{cloneObservation(submission.Observations[0])}
	unknown.Observations[0].CellID = "firefox-linux-amd64-320-normal"
	if err := ledger.Record(matrix, forms, unknown, "", ""); err == nil {
		t.Fatal("unknown cell accepted")
	}
	// An empty observer is rejected before any cell is examined.
	anonymous := submission
	anonymous.Observer = ""
	if err := ledger.Record(matrix, forms, anonymous, "", ""); err == nil {
		t.Fatal("anonymous submission accepted")
	}
	// Recording without generated forms fails closed.
	if err := (&Ledger{Schema: LedgerSchema, ScenarioID: ScenarioObservation}).Record(matrix, nil, submission, "", ""); err == nil {
		t.Fatal("recording accepted without generated forms")
	}
	// Cell-scoped recording binds the submission to exactly that cell.
	scoped := submission
	scoped.Observations = []CellObservation{cloneObservation(submission.Observations[0])}
	scopedCell := unobserved()
	scoped.Observations[0].CellID = scopedCell.ID
	scoped.Observations[0].ViewportCSSPx = scopedCell.Key.ViewportCSSPx
	scoped.Observations[0].MotionMode = scopedCell.Key.MotionMode
	if err := ledger.Record(matrix, forms, scoped, cell.ID, ""); err == nil {
		t.Fatal("cell-scoped recording accepted a foreign cell")
	}
	// The invocation observer override rejects a different submission identity.
	if err := ledger.Record(matrix, forms, scoped, "", "ci-oidc:expected"); err == nil {
		t.Fatal("expected-observer mismatch accepted")
	}
}

func cloneObservation(source CellObservation) CellObservation {
	body, _ := json.Marshal(source)
	var clone CellObservation
	_ = json.Unmarshal(body, &clone)
	return clone
}

// generatedForms builds the invocation forms for the ledger tests with a
// uniform verified subject binding.
func generatedForms(t *testing.T, matrix *Matrix, artifactDigest, buildString string) []Form {
	t.Helper()
	binding := FormBinding{
		Tag:                  "v0.1.0-test",
		InvocationID:         "inv-1",
		ReleaseSubjectDigest: "deadbeef",
		PublicOriginDigest:   "cafebabe",
		BrowserSubjects:      map[string]BrowserSubject{},
	}
	for _, cell := range matrix.Cells {
		binding.BrowserSubjects[cell.ID] = BrowserSubject{
			Schema: SubjectSchema, CellID: cell.ID,
			BrowserSubject: cell.Key.BrowserSubject, Architecture: cell.Key.Architecture,
			SHA256: artifactDigest, Build: buildString, ExecutablePath: "/tmp/chrome",
		}
	}
	forms, err := Forms(matrix, binding)
	if err != nil {
		t.Fatal(err)
	}
	return forms
}

func TestEvaluateCellComparesAgainstCatalogExpectation(t *testing.T) {
	loaded := loadFrozen(t)
	matrix, err := MatrixOf(loaded, ScenarioObservation)
	if err != nil {
		t.Fatal(err)
	}
	cell := matrix.Cells[0]
	passing := CellObservation{
		CellID: cell.ID, Observer: "o", BrowserArtifactSHA256: strings.Repeat("a", 64), BrowserBuild: "b",
		ViewportCSSPx: cell.Key.ViewportCSSPx, MotionMode: cell.Key.MotionMode,
		StartedAt: "2026-09-05T10:00:00Z", EndedAt: "2026-09-05T10:02:00Z",
		Results: map[string]string{},
	}
	for _, assertion := range cell.Assertions {
		passing.Results[assertion.ID] = "passed"
	}
	ledger := &Ledger{Records: []CellObservation{passing}}
	if outcome, ok := ledger.EvaluateCell(cell); !ok || outcome.Outcome != "passed" {
		t.Fatalf("passing observation evaluated %+v ok=%v", outcome, ok)
	}
	failing := passing
	failing.Results[cell.Assertions[1].ID] = "failed"
	ledgerFailed := &Ledger{Records: []CellObservation{failing}}
	if outcome, _ := ledgerFailed.EvaluateCell(cell); outcome.Outcome != "failed" {
		t.Fatalf("failed typed value evaluated %q", outcome.Outcome)
	}
	if _, ok := (&Ledger{}).EvaluateCell(cell); ok {
		t.Fatal("unobserved cell evaluated as observed")
	}
}

func TestObservationSummaryCountsNotRunAsWarned(t *testing.T) {
	loaded := loadFrozen(t)
	matrix, err := MatrixOf(loaded, ScenarioObservation)
	if err != nil {
		t.Fatal(err)
	}
	summary, missing := (&Ledger{}).ObservationSummary(matrix)
	if summary.RequiredCells != 24 || missing != 24 || summary.WarnedCells != 24 {
		t.Fatalf("empty ledger summary %+v missing=%d", summary, missing)
	}
}

func TestSubjectResolvesFromFrozenLocks(t *testing.T) {
	// playwright_chromium resolves per architecture from the release lock.
	subject, err := ResolveSubject("playwright-chromium-linux-amd64-320-normal", "")
	if err != nil {
		t.Fatal(err)
	}
	if subject.URL == "" || len(subject.SHA256) != 64 || subject.Bytes == 0 {
		t.Fatalf("amd64 subject not resolved from the lock: %+v", subject)
	}
	if _, err := ResolveSubject("playwright-chromium-linux-arm64-1440-reduced", ""); err != nil {
		t.Fatal(err)
	}
	// branded_chromium without a frozen resolution fails closed.
	if _, err := ResolveSubject("branded-chrome-linux-amd64-320-normal", ""); err == nil {
		t.Fatal("branded cell resolved without a frozen resolution")
	}
	// A frozen resolution document unlocks the branded cells only on amd64.
	resolution := t.TempDir() + "/branded.json"
	if err := os.WriteFile(resolution, []byte(`{
		"schema": "quoin-branded-chrome-resolution-v1",
		"resolved_at": "2026-09-05T08:00:00Z", "channel": "Stable",
		"version": "152.0.7977.82",
		"url": "https://example/chrome-linux64.zip",
		"sha256": "0704631fb3e4f741092e08f55272f90abc3e307f991f05f332924364415b02e0",
		"bytes": 194031103
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	subject, err = ResolveSubject("branded-chrome-linux-amd64-1024-normal", resolution)
	if err != nil {
		t.Fatal(err)
	}
	if subject.Version != "152.0.7977.82" {
		t.Fatalf("branded subject version %q", subject.Version)
	}
}

func TestAutomationFactsEnforceBrowserBinding(t *testing.T) {
	loaded := loadFrozen(t)
	matrix, err := MatrixOf(loaded, ScenarioAutomated)
	if err != nil {
		t.Fatal(err)
	}
	cell := matrix.Cells[0]
	subject := BrowserSubject{
		Schema: SubjectSchema, CellID: cell.ID,
		BrowserSubject: cell.Key.BrowserSubject, Architecture: cell.Key.Architecture,
		SHA256: strings.Repeat("b", 64), Build: "Chromium 151 test", ExecutablePath: "/tmp/chrome",
	}
	executed := AutomatedCell{
		CellID: cell.ID,
		Browser: BrowserSubject{
			Schema: SubjectSchema, CellID: cell.ID,
			BrowserSubject: cell.Key.BrowserSubject, Architecture: cell.Key.Architecture,
			SHA256: subject.SHA256, Build: subject.Build,
		},
		Assertions: map[string]AssertionResult{},
	}
	for _, assertion := range cell.Assertions {
		executed.Assertions[assertion.ID] = AssertionResult{Result: "passed"}
	}
	results := &AutomationResults{Schema: ResultsSchema, InvocationID: "inv-1", Cells: []AutomatedCell{executed}}
	factsPath := filepath.Join(t.TempDir(), "facts.json")
	if err := WriteAutomationFacts(matrix, results, subject, factsPath); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(factsPath)
	if err != nil {
		t.Fatal(err)
	}
	var facts struct {
		SchemaKind string `json:"schema_kind"`
		Assertions map[string]struct {
			Actual string `json:"actual"`
		} `json:"assertions"`
	}
	if err := json.Unmarshal(body, &facts); err != nil {
		t.Fatal(err)
	}
	if facts.SchemaKind != "quoin-verify-facts-v1" {
		t.Fatalf("facts schema_kind %q", facts.SchemaKind)
	}
	if len(facts.Assertions) != len(cell.Assertions) {
		t.Fatalf("facts carry %d assertions, cell declares %d", len(facts.Assertions), len(cell.Assertions))
	}
	for id, fact := range facts.Assertions {
		if fact.Actual != "passed" {
			t.Fatalf("assertion %q actual %q", id, fact.Actual)
		}
	}
	// A different executed browser digest must fail closed.
	drifted := results
	driftedCell := executed
	driftedCell.Browser.SHA256 = strings.Repeat("c", 64)
	drifted.Cells = []AutomatedCell{driftedCell}
	if err := WriteAutomationFacts(matrix, drifted, subject, factsPath); err == nil {
		t.Fatal("facts written for a drifted browser digest")
	}
}

func TestFormDigestIsStable(t *testing.T) {
	form := Form{Schema: FormSchema, InvocationID: "inv", CellID: "c", ViewportCSSPx: 320}
	first := FormDigest(form)
	if first != FormDigest(form) {
		t.Fatal("form digest unstable")
	}
	if len(first) != 64 {
		t.Fatalf("digest %q is not bare hex64", first)
	}
}
