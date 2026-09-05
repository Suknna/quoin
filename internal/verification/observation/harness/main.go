// Command harness is the deterministic CLI behind ci/verify-ui-automation
// and ci/record-ui-observation. It resolves the frozen browser subjects,
// generates the catalog-bound observation forms, records and validates the
// typed observation ledger and writes the coordinator-compatible facts
// documents. Executing browsers (Playwright) stays with the CI entrypoint
// itself; this binary owns every binding and comparison rule.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/observation"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "subject":
		err = subjectCommand(os.Args[2:])
	case "subject-complete":
		err = subjectCompleteCommand(os.Args[2:])
	case "cells":
		err = cellsCommand(os.Args[2:])
	case "form":
		err = formCommand(os.Args[2:])
	case "record":
		err = recordCommand(os.Args[2:])
	case "assert-observation":
		err = assertObservationCommand(os.Args[2:])
	case "assert-automation":
		err = assertAutomationCommand(os.Args[2:])
	case "summarize":
		err = summarizeCommand(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "observation harness: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: harness <subject|subject-complete|cells|form|record|assert-observation|assert-automation|summarize> [flags]")
	os.Exit(2)
}

// loadCatalog loads and fully validates the frozen catalog; a drifted or
// invalid catalog must fail the cell before anything runs.
func loadCatalog(path string) (*catalog.Catalog, error) {
	return catalog.LoadAndValidate(path)
}

// writeJSON marshals indented JSON to a path, creating parent directories;
// "-" writes to stdout.
func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if path == "-" {
		_, err := os.Stdout.Write(append(body, '\n'))
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// subjectCommand resolves the artifact binding of one cell from the frozen
// authorities (release lock or branded resolution). The CI entrypoint then
// downloads, verifies the digest, extracts and runs `<executable>
// --version`, and completes the document via subject-complete.
func subjectCommand(args []string) error {
	flags := flag.NewFlagSet("subject", flag.ExitOnError)
	cell := flags.String("cell", "", "catalog cell id")
	branded := flags.String("branded-resolution", "", "frozen branded-chrome resolution json")
	out := flags.String("out", "", "output browser-subject json path")
	_ = flags.Parse(args)
	if *cell == "" || *out == "" {
		return fmt.Errorf("--cell and --out are required")
	}
	subject, err := observation.ResolveSubject(*cell, *branded)
	if err != nil {
		return err
	}
	return writeJSON(*out, subject)
}

// subjectCompleteCommand validates that the downloaded artifact digest
// matches the frozen authority and stamps the observed build string and
// executable path into the subject document.
func subjectCompleteCommand(args []string) error {
	flags := flag.NewFlagSet("subject-complete", flag.ExitOnError)
	path := flags.String("subject", "", "browser-subject json written by 'subject'")
	sha256Hex := flags.String("sha256", "", "observed sha256 of the downloaded artifact")
	build := flags.String("build", "", "observed <executable> --version output")
	executable := flags.String("executable", "", "verified executable path")
	_ = flags.Parse(args)
	if *path == "" {
		return fmt.Errorf("--subject is required")
	}
	body, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var subject observation.BrowserSubject
	if err := json.Unmarshal(body, &subject); err != nil {
		return err
	}
	if subject.SHA256 != *sha256Hex {
		return fmt.Errorf("downloaded artifact digest %s does not match the frozen digest %s", *sha256Hex, subject.SHA256)
	}
	subject.Build = *build
	subject.ExecutablePath = *executable
	return writeJSON(*path, subject)
}

// formCommand generates the observation forms of the whole matrix bound to
// the invocation. Locally executed cells read their completed
// browser-subject documents from subjectsDir; cells whose architecture
// this host cannot run resolve their frozen digest from the release lock
// (or the branded resolution) so the form exists for the native runner,
// with the observed build left to that runner.
func formCommand(args []string) error {
	flags := flag.NewFlagSet("form", flag.ExitOnError)
	catalogPath := flags.String("catalog", "docs/specs/quoin-v1/contracts/verification-catalog.yaml", "frozen catalog path")
	invocation := flags.String("invocation", "", "invocation id")
	tag := flags.String("tag", "", "release tag")
	releaseDigest := flags.String("release-subject-digest", "", "release subject digest")
	originDigest := flags.String("public-origin-digest", "", "qualification site public origin digest")
	subjectsDir := flags.String("subjects", "", "directory of per-cell browser-subject json")
	branded := flags.String("branded-resolution", "", "frozen branded-chrome resolution json")
	architecture := flags.String("architecture", "", "generate forms only for this architecture (linux/amd64 | linux/arm64)")
	out := flags.String("out", "", "output forms json path")
	_ = flags.Parse(args)
	if *invocation == "" || *releaseDigest == "" || *subjectsDir == "" || *out == "" {
		return fmt.Errorf("--invocation, --release-subject-digest, --subjects and --out are required")
	}
	loaded, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	matrix, err := observation.MatrixOf(loaded, observation.ScenarioObservation)
	if err != nil {
		return err
	}
	binding := observation.FormBinding{
		Tag:                  *tag,
		InvocationID:         *invocation,
		ReleaseSubjectDigest: *releaseDigest,
		PublicOriginDigest:   *originDigest,
		BrowserSubjects:      map[string]observation.BrowserSubject{},
	}
	for _, cell := range matrix.Cells {
		if *architecture != "" && cell.Key.Architecture != *architecture {
			//A native runner generates the forms of its own cells only;
			//the foreign architecture's forms belong to its native runner.
			continue
		}
		subject, err := observation.LoadSubject(filepath.Join(*subjectsDir, cell.ID, "browser-subject.json"))
		if err != nil {
			subject, err = observation.ResolveSubject(cell.ID, *branded)
			if err != nil {
				return fmt.Errorf("cell %s: %w", cell.ID, err)
			}
		}
		binding.BrowserSubjects[cell.ID] = subject
	}
	forms, err := observation.Forms(matrix, binding)
	if err != nil {
		return err
	}
	return writeJSON(*out, map[string]any{
		"schema":        observation.FormSchema,
		"invocation_id": *invocation,
		"forms":         forms,
	})
}

// loadCompletedSubjects reads the completed browser-subject documents of
// the cells that actually executed on this runner; a missing document
// means the cell never ran here.
func loadCompletedSubjects(dir string, matrix *observation.Matrix) (map[string]observation.BrowserSubject, error) {
	subjects := map[string]observation.BrowserSubject{}
	if dir == "" {
		return subjects, nil
	}
	for _, cell := range matrix.Cells {
		subject, err := observation.LoadSubject(filepath.Join(dir, cell.ID, "browser-subject.json"))
		if err != nil {
			continue
		}
		subjects[cell.ID] = subject
	}
	return subjects, nil
}

// cellsCommand prints the fixed matrix (cell ids with typed keys) as JSON
// so CI entrypoints enumerate cells from the catalog instead of keeping a
// second copy of the required-cell list.
func cellsCommand(args []string) error {
	flags := flag.NewFlagSet("cells", flag.ExitOnError)
	catalogPath := flags.String("catalog", "docs/specs/quoin-v1/contracts/verification-catalog.yaml", "frozen catalog path")
	scenario := flags.String("scenario", observation.ScenarioAutomated, "matrix scenario id")
	_ = flags.Parse(args)
	loaded, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	matrix, err := observation.MatrixOf(loaded, *scenario)
	if err != nil {
		return err
	}
	cells := make([]map[string]any, 0, len(matrix.Cells))
	for _, cell := range matrix.Cells {
		cells = append(cells, map[string]any{
			"cell_id":         cell.ID,
			"browser_subject": cell.Key.BrowserSubject,
			"architecture":    cell.Key.Architecture,
			"viewport_css_px": cell.Key.ViewportCSSPx,
			"motion_mode":     cell.Key.MotionMode,
		})
	}
	return writeJSON("-", map[string]any{"scenario": matrix.ScenarioID, "cells": cells})
}

// recordCommand appends a typed observation submission to the invocation
// ledger after full closed-field validation.
func recordCommand(args []string) error {
	flags := flag.NewFlagSet("record", flag.ExitOnError)
	catalogPath := flags.String("catalog", "docs/specs/quoin-v1/contracts/verification-catalog.yaml", "frozen catalog path")
	invocation := flags.String("invocation", "", "invocation id")
	ledgerPath := flags.String("ledger", "", "ledger json path")
	submissionPath := flags.String("submission", "", "typed submission json path")
	formsPath := flags.String("forms", "", "generated observation forms json path")
	singleCell := flags.String("cell", "", "restrict the submission to exactly this catalog cell")
	expectedObserver := flags.String("expected-observer", "", "invocation observer identity the submission must carry")
	_ = flags.Parse(args)
	if *invocation == "" || *ledgerPath == "" || *submissionPath == "" || *formsPath == "" {
		return fmt.Errorf("--invocation, --ledger, --submission and --forms are required")
	}
	loaded, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	matrix, err := observation.MatrixOf(loaded, observation.ScenarioObservation)
	if err != nil {
		return err
	}
	formsBody, err := os.ReadFile(*formsPath)
	if err != nil {
		return err
	}
	var formsDocument struct {
		Forms []observation.Form `json:"forms"`
	}
	if err := json.Unmarshal(formsBody, &formsDocument); err != nil {
		return fmt.Errorf("parse forms %s: %w", *formsPath, err)
	}
	body, err := os.ReadFile(*submissionPath)
	if err != nil {
		return err
	}
	var submission observation.Submission
	if err := json.Unmarshal(body, &submission); err != nil {
		return fmt.Errorf("parse submission %s: %w", *submissionPath, err)
	}
	ledger, err := observation.LoadLedger(*ledgerPath, *invocation, observation.ScenarioObservation, matrix)
	if err != nil {
		return err
	}
	if err := ledger.Record(matrix, formsDocument.Forms, submission, *singleCell, *expectedObserver); err != nil {
		return err
	}
	return ledger.Write(*ledgerPath)
}

// assertObservationCommand evaluates one cell's recorded observation and
// writes the coordinator-compatible facts document; exit 1 when the cell
// has no observation or the typed values fail the catalog expectations.
func assertObservationCommand(args []string) error {
	flags := flag.NewFlagSet("assert-observation", flag.ExitOnError)
	catalogPath := flags.String("catalog", "docs/specs/quoin-v1/contracts/verification-catalog.yaml", "frozen catalog path")
	invocation := flags.String("invocation", "", "invocation id")
	ledgerPath := flags.String("ledger", "", "ledger json path")
	cell := flags.String("cell", "", "catalog cell id")
	factsPath := flags.String("facts", "", "facts json output path")
	_ = flags.Parse(args)
	if *invocation == "" || *ledgerPath == "" || *cell == "" || *factsPath == "" {
		return fmt.Errorf("--invocation, --ledger, --cell and --facts are required")
	}
	loaded, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	matrix, err := observation.MatrixOf(loaded, observation.ScenarioObservation)
	if err != nil {
		return err
	}
	ledger, err := observation.LoadLedger(*ledgerPath, *invocation, observation.ScenarioObservation, matrix)
	if err != nil {
		return err
	}
	outcome, err := observation.WriteObservationFacts(matrix, ledger, *cell, *factsPath)
	if err != nil {
		return err
	}
	fmt.Printf("cell %s observation %s (observer %s)\n", *cell, outcome.Outcome, outcome.Observer)
	if outcome.Outcome != "passed" {
		return fmt.Errorf("cell %s typed observation failed: %s", *cell, outcome.DetailCode)
	}
	return nil
}

// assertAutomationCommand validates one cell's automation result against
// the frozen subject and writes the coordinator-compatible facts document.
func assertAutomationCommand(args []string) error {
	flags := flag.NewFlagSet("assert-automation", flag.ExitOnError)
	catalogPath := flags.String("catalog", "docs/specs/quoin-v1/contracts/verification-catalog.yaml", "frozen catalog path")
	resultsPath := flags.String("results", "", "automation matrix results json")
	subjectPath := flags.String("subject", "", "cell browser-subject json")
	factsPath := flags.String("facts", "", "facts json output path")
	_ = flags.Parse(args)
	if *resultsPath == "" || *subjectPath == "" || *factsPath == "" {
		return fmt.Errorf("--results, --subject and --facts are required")
	}
	loaded, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	matrix, err := observation.MatrixOf(loaded, observation.ScenarioAutomated)
	if err != nil {
		return err
	}
	results, err := observation.LoadAutomationResults(*resultsPath)
	if err != nil {
		return err
	}
	subject, err := observation.LoadSubject(*subjectPath)
	if err != nil {
		return err
	}
	if err := observation.WriteAutomationFacts(matrix, results, subject, *factsPath); err != nil {
		return err
	}
	executed := results.Cell(subject.CellID)
	for id, assertion := range executed.Assertions {
		if assertion.Result != "passed" {
			return fmt.Errorf("cell %s automated assertion %q failed: %s", subject.CellID, id, assertion.Detail)
		}
	}
	return nil
}

// summarizeCommand projects a ledger and an automation results document
// over the full matrix for the workflow aggregate job.
func summarizeCommand(args []string) error {
	flags := flag.NewFlagSet("summarize", flag.ExitOnError)
	catalogPath := flags.String("catalog", "docs/specs/quoin-v1/contracts/verification-catalog.yaml", "frozen catalog path")
	invocation := flags.String("invocation", "", "invocation id")
	ledgerPath := flags.String("ledger", "", "ledger json path")
	resultsPath := flags.String("results", "", "automation matrix results json")
	subjectsDir := flags.String("subjects", "", "directory of per-cell browser-subject json")
	out := flags.String("out", "", "summary json output path")
	_ = flags.Parse(args)
	if *invocation == "" || *out == "" {
		return fmt.Errorf("--invocation and --out are required")
	}
	if *resultsPath != "" && *subjectsDir == "" {
		return fmt.Errorf("--subjects is required with --results: automated cells are evaluated through their frozen subjects")
	}
	loaded, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	summary := map[string]any{
		"schema":        "quoin-ui-matrix-summary-v1",
		"invocation_id": *invocation,
		"generated_at":  time.Now().UTC().Format(time.RFC3339Nano),
	}
	if *resultsPath != "" {
		automatedMatrix, err := observation.MatrixOf(loaded, observation.ScenarioAutomated)
		if err != nil {
			return err
		}
		results, err := observation.LoadAutomationResults(*resultsPath)
		if err != nil {
			return err
		}
		subjects, err := loadCompletedSubjects(*subjectsDir, automatedMatrix)
		if err != nil {
			return err
		}
		cells := map[string]string{}
		for _, cell := range automatedMatrix.Cells {
			subject, completed := subjects[cell.ID]
			if !completed {
				cells[cell.ID] = "not_run"
				continue
			}
			executed, err := observation.EvaluateAutomatedCell(automatedMatrix, results, subject)
			switch {
			case err != nil:
				return err
			case executed == nil:
				cells[cell.ID] = "not_run"
			default:
				verdict := "passed"
				for _, assertion := range executed.Assertions {
					if assertion.Result != "passed" {
						verdict = "failed"
					}
				}
				cells[cell.ID] = verdict
			}
		}
		summary["ui.automated"] = cells
	}
	if *ledgerPath != "" {
		observationMatrix, err := observation.MatrixOf(loaded, observation.ScenarioObservation)
		if err != nil {
			return err
		}
		ledger, err := observation.LoadLedger(*ledgerPath, *invocation, observation.ScenarioObservation, observationMatrix)
		if err != nil {
			return err
		}
		projection, missing := ledger.ObservationSummary(observationMatrix)
		cells := map[string]string{}
		for _, cell := range observationMatrix.Cells {
			outcome, ok := ledger.EvaluateCell(cell)
			switch {
			case !ok:
				cells[cell.ID] = "not_run"
			default:
				cells[cell.ID] = outcome.Outcome
			}
		}
		summary["ui.manual-observation"] = cells
		summary["observationSummary"] = projection
		summary["missingObservations"] = missing
	}
	return writeJSON(*out, summary)
}
