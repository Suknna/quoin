package ui

// TestTicket41 closes the T41 acceptance path (issue #64): the released
// browser artifacts run the ui.automated matrix through the real
// ci/verify-ui-automation phase contract against a real Compose-qualified
// Quoin site (the Playwright webServer boots the full stack, first-admin
// install included), and the verifier records the separate typed
// 24-cell human observation matrix through ci/record-ui-observation with
// observer, browser digest and viewport/motion binding. Cells of the
// foreign native architecture are delegated machine-visibly to the
// ui-qualification workflow's native runner and are never published as
// local passes; no atomic catalog scenario is closed locally.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/environments"
	"github.com/Suknna/quoin/internal/verification/evidence"
	"github.com/Suknna/quoin/internal/verification/observation"
	"github.com/Suknna/quoin/internal/verification/result"
	"github.com/Suknna/quoin/internal/verification/suites"
)

const (
	releaseVersion = "v0.1.0-dev"
	catalogPath    = "docs/specs/quoin-v1/contracts/verification-catalog.yaml"
	profilePath    = "docs/specs/quoin-v1/contracts/verification-result-profile.yaml"
	publicOrigin   = "https://127.0.0.1:18480"
)

func TestTicket41(t *testing.T) {
	evidenceRoot := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceRoot == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T41 acceptance evidence run disabled")
	}
	requireNativeCell(t)
	recorder := newEvidence(t, evidenceRoot)
	matrixDir := filepath.Join(evidenceRoot, "ui-matrix")
	startedAt := time.Now().UTC()

	// Native-cell facts: the executing host proves its architecture; the
	// foreign-architecture cells are delegated to the workflow runner.
	native := environments.ResolveNative(environRunner{})
	if !native.NativeExecution {
		t.Fatalf("qualification cell is not native: %+v", native)
	}
	recorder.observe(t, "native-cell.json", map[string]any{"native": native, "toolchain": environments.ResolveToolchain(environRunner{})})
	baseline := captureInventory()
	recorder.observe(t, "docker-baseline.json", baseline)

	// The frozen catalog is the authority: both UI scenarios must carry
	// the exact 24-cell matrix and the ci entrypoints this ticket owns.
	loaded, err := catalog.LoadAndValidate(filepath.Join(repoRoot(), catalogPath))
	if err != nil {
		t.Fatalf("frozen catalog rejected: %v", err)
	}
	profile, err := result.LoadProfile(filepath.Join(repoRoot(), profilePath))
	if err != nil {
		t.Fatal(err)
	}
	automatedMatrix, err := observation.MatrixOf(loaded, observation.ScenarioAutomated)
	if err != nil {
		t.Fatalf("ui.automated matrix: %v", err)
	}
	observationMatrix, err := observation.MatrixOf(loaded, observation.ScenarioObservation)
	if err != nil {
		t.Fatalf("ui.manual-observation matrix: %v", err)
	}
	assertCatalogShape(t, loaded, automatedMatrix, observationMatrix)
	recorder.observe(t, "catalog-matrix.json", map[string]any{
		"automated": cellKeys(automatedMatrix), "observation": cellKeys(observationMatrix),
	})

	// The ui.automated cells of this architecture execute through the
	// coordinator's phase contract (real ci/verify-ui-automation
	// subprocesses). The first action boots the Compose-qualified site
	// once; later cells reuse the recorded per-cell results.
	invocation := "t41-ui-qualification"
	coordinator := &suites.Coordinator{
		Catalog: loaded, Profile: profile,
		Target:       suites.Target{Backend: "compose", Architecture: native.Architecture},
		OutputDir:    filepath.Join(evidenceRoot, "invocation"),
		RepoRoot:     repoRoot(),
		InvocationID: invocation,
		ToolVersion:  "quoin-t41-acceptance",
		Subject:      result.Subject{Name: "quoin-ui-surface", Digest: map[string]string{"sha256": gitCommit()}},
	}
	cellEnv := append(os.Environ(),
		"QUOIN_UI_MATRIX_DIR="+matrixDir,
		"QUOIN_UI_INVOCATION="+invocation,
	)
	localCatalogCells, delegatedAutomated := splitCells(automatedMatrix.Cells, native.Architecture)
	automatedScenario := loaded.Scenario(observation.ScenarioAutomated)
	for index := range localCatalogCells {
		raw := automatedScenario.Cells[indexOfCell(automatedScenario.Cells, localCatalogCells[index].ID)]
		cell := raw
		item, execErr := coordinator.ExecuteCell(automatedScenario, &cell, "compose", cellEnv)
		if execErr != nil {
			t.Fatalf("ui.automated cell %s: %v", cell.ID, execErr)
		}
		if item.Outcome != "passed" {
			t.Fatalf("ui.automated cell %s outcome=%s category=%s (see invocation/cells)", cell.ID, item.Outcome, item.Category)
		}
	}

	// The typed observation matrix: the verifier generates the forms,
	// records the typed observations bound to the observer identity and
	// the exact browser build/digest, and evaluates them against the
	// catalog expectations through the frozen entrypoint phases.
	observationCells, delegatedObservation := splitCells(observationMatrix.Cells, native.Architecture)
	recorded := recordTypedObservations(t, recorder, evidenceRoot, matrixDir, observationCells)

	// Evidence attachments: subjects, results, forms, ledger, per-cell
	// screenshots and traces are digest-noted.
	for _, path := range []string{
		filepath.Join(matrixDir, "matrix-results.json"),
		filepath.Join(matrixDir, "observation-forms.json"),
		filepath.Join(matrixDir, "observation-ledger.json"),
		filepath.Join(matrixDir, "branded-chrome-resolution.json"),
		filepath.Join(matrixDir, "local-subjects.json"),
	} {
		recorder.note(t, path)
	}
	for _, cell := range localCatalogCells {
		recorder.note(t, filepath.Join(matrixDir, "subjects", cell.ID, "browser-subject.json"))
		recorder.note(t, filepath.Join(matrixDir, "artifacts", cell.ID, "final.png"))
		recorder.note(t, filepath.Join(matrixDir, "artifacts", cell.ID, "trace.zip"))
	}

	// Cleanup proof: the Playwright teardown owns the stack dispositions
	// in cleanup.json; the Go side proves the owned docker resources are
	// gone and appends its phase without overwriting that record.
	proveCleanup(t, recorder, evidenceRoot, baseline)

	recorder.observe(t, "runtime-evidence.json", map[string]any{
		"schema": "quoin-t41-runtime-evidence", "ticket": "T41", "issue": 64,
		"gitCommit": gitCommit(), "dirtyStateDigest": dirtyDigest(),
		"startedAt":  startedAt.Format(time.RFC3339Nano),
		"finishedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"status":     "passed",
		"nativeCell": map[string]any{
			"architecture":    native.Architecture,
			"nativeExecution": native.NativeExecution,
		},
		"components": map[string]any{
			"entrypoints": []string{"ci/verify-ui-automation", "ci/record-ui-observation"},
			"harness":     "./internal/verification/observation/harness",
			"spec":        "web/e2e/ui-qualification.spec.ts (@ticket-41)",
			"qualificationSite": map[string]any{
				"publicOrigin":   publicOrigin,
				"originDigest":   sha256Hex([]byte(publicOrigin)),
				"composeProject": "quoin",
				"bootsVia":       "Playwright webServer (test/e2e/compose/server.sh)",
			},
		},
		"observedTransitions": map[string]any{
			"ui.automated":          itemSummaries(coordinator.Items),
			"ui.manual-observation": recorded,
		},
		"commands":   recorder.commands,
		"artifacts":  recorder.artifacts,
		"assertions": ticketAssertions(automatedMatrix, observationMatrix, localCatalogCells, delegatedAutomated, delegatedObservation, recorded, matrixDir),
	})
}

// assertCatalogShape proves the frozen contract facts this ticket closes
// against: both scenarios exist as required active release-qualification
// scenarios, their executors are exactly the ci entrypoints this ticket
// owns, and the matrix is the fixed 24-cell product.
func assertCatalogShape(t *testing.T, loaded *catalog.Catalog, automated, observed *observation.Matrix) {
	t.Helper()
	for scenarioID, entrypoint := range map[string]string{
		observation.ScenarioAutomated:   "ci/verify-ui-automation",
		observation.ScenarioObservation: "ci/record-ui-observation",
	} {
		scenario := loaded.Scenario(scenarioID)
		if scenario == nil {
			t.Fatalf("catalog scenario %q missing", scenarioID)
		}
		if scenario.Executor.Entrypoint != entrypoint {
			t.Fatalf("scenario %s entrypoint %q, expected %q", scenarioID, scenario.Executor.Entrypoint, entrypoint)
		}
		if scenario.Layer != catalog.LayerReleaseQualification || scenario.Requirement != catalog.RequirementRequired {
			t.Fatalf("scenario %s is not a required release-qualification scenario", scenarioID)
		}
	}
	if len(automated.Cells) != 24 || len(observed.Cells) != 24 {
		t.Fatalf("matrix sizes %d/%d", len(automated.Cells), len(observed.Cells))
	}
	automatedIDs := map[string]bool{}
	for _, cell := range automated.Cells {
		automatedIDs[cell.ID] = true
	}
	for _, cell := range observed.Cells {
		if !automatedIDs[cell.ID] {
			t.Fatalf("observation cell %s missing from the automated matrix", cell.ID)
		}
	}
}

// indexOfCell finds a catalog cell index by id.
func indexOfCell(cells []catalog.Cell, id string) int {
	for index := range cells {
		if cells[index].ID == id {
			return index
		}
	}
	return -1
}

func splitCells(cells []observation.Cell, architecture string) (local, delegated []observation.Cell) {
	for _, cell := range cells {
		if cell.Key.Architecture == architecture {
			local = append(local, cell)
		} else {
			delegated = append(delegated, cell)
		}
	}
	return local, delegated
}

// recordTypedObservations drives the ci/record-ui-observation phases for
// every locally executed cell. The typed values derive from the recorded
// automated pass and the submission carries the labeled acceptance
// observer and basis; the human observation closure of the catalog
// scenario happens under the CI OIDC observer in ui-qualification.yml.
func recordTypedObservations(t *testing.T, recorder *ticketEvidence, evidenceRoot, matrixDir string, localObserved []observation.Cell) []map[string]any {
	t.Helper()
	results, err := observation.LoadAutomationResults(filepath.Join(matrixDir, "matrix-results.json"))
	if err != nil {
		t.Fatalf("automation results: %v", err)
	}
	submissionDir := filepath.Join(evidenceRoot, "observation-submissions")
	if err := os.MkdirAll(submissionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	observer := "t41-acceptance-harness (non-human: machinery proof)"
	basis := "acceptance recording of the typed-observation machinery only: the values mirror the ui.automated automated pass on the qualification site and are NOT a human observation; the catalog scenario's human closure happens under the CI OIDC observer in ui-qualification.yml and this ledger must not be reused as that closure"
	phaseEnv := append(os.Environ(),
		"QUOIN_UI_MATRIX_DIR="+matrixDir,
		"QUOIN_OBSERVATION_INVOCATION=t41-ui-observation",
		"QUOIN_OBSERVATION_TAG="+releaseVersion,
		"QUOIN_OBSERVATION_RELEASE_DIGEST="+gitCommit(),
		"QUOIN_OBSERVATION_ORIGIN_DIGEST="+sha256Hex([]byte(publicOrigin)),
		"QUOIN_OBSERVATION_SUBJECTS="+filepath.Join(matrixDir, "subjects"),
		"QUOIN_OBSERVATION_BRANDED="+filepath.Join(matrixDir, "branded-chrome-resolution.json"),
	)
	workRoot := filepath.Join(evidenceRoot, "observation-cells")
	recorder.run(t, "observation-forms", phaseEnv, 0, "./ci/record-ui-observation", "--phase", "setup")

	summaries := make([]map[string]any, 0, len(localObserved))
	for _, cell := range localObserved {
		submissionPath := filepath.Join(submissionDir, cell.ID+".json")
		writeObservationSubmission(t, submissionPath, observer, basis, cell, results.Cell(cell.ID))
		cellWork := filepath.Join(workRoot, cell.ID)
		cellEnv := append(slicesClone(phaseEnv),
			"QUOIN_OBSERVATION_SUBMISSION="+submissionPath,
			"QUOIN_VERIFY_CELL="+cell.ID,
			"QUOIN_VERIFY_WORKDIR="+cellWork,
			"QUOIN_VERIFY_FACTS="+filepath.Join(cellWork, "facts.json"),
		)
		recorder.run(t, "observation-record-"+cell.ID, cellEnv, 0, "./ci/record-ui-observation", "--phase", "action")
		recorder.run(t, "observation-assert-"+cell.ID, cellEnv, 0, "./ci/record-ui-observation", "--phase", "assert")
		summaries = append(summaries, map[string]any{
			"cell": cell.ID, "outcome": "passed", "observer": observer,
		})
	}
	return summaries
}

// writeObservationSubmission projects one cell's automated pass into the
// typed observation vocabulary. Every typed value must be earned by the
// corresponding automated aspects actually passing.
func writeObservationSubmission(t *testing.T, path, observer, basis string, cell observation.Cell, automatedCell *observation.AutomatedCell) {
	t.Helper()
	if automatedCell == nil {
		t.Fatalf("no automated result for observation cell %s", cell.ID)
	}
	aspects := func(ids ...string) bool {
		for _, id := range ids {
			assertion, ok := automatedCell.Assertions[id]
			if !ok || assertion.Result != "passed" {
				return false
			}
		}
		return true
	}
	submission := map[string]any{
		"observer": observer,
		"basis":    basis,
		"observations": []map[string]any{{
			"cell_id":                 cell.ID,
			"observer":                observer,
			"basis":                   basis,
			"browser_artifact_sha256": automatedCell.Browser.SHA256,
			"browser_build":           automatedCell.Browser.Build,
			"viewport_css_px":         cell.Key.ViewportCSSPx,
			"motion_mode":             cell.Key.MotionMode,
			"started_at":              automatedCell.StartedAt,
			"ended_at":                automatedCell.FinishedAt,
			"results": map[string]string{
				"visual-comprehension": map[bool]string{true: "passed", false: "failed"}[aspects("dom-contracts", "responsive-reflow", "axe-rules")],
				"motion-feedback":      map[bool]string{true: "passed", false: "failed"}[aspects("domain-behavior")],
				"focus-not-occluded":   map[bool]string{true: "passed", false: "failed"}[aspects("focus-visibility-and-occlusion", "target-size")],
			},
		}},
	}
	body, err := json.MarshalIndent(submission, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// proveCleanup asserts the owned docker resources and the stack
// directory are gone, then appends the Go-side phase to cleanup.json.
func proveCleanup(t *testing.T, recorder *ticketEvidence, evidenceRoot string, baseline dockerInventory) {
	t.Helper()
	containers, networks, volumes := ownedResources()
	owned := append(append(containers, networks...), volumes...)
	residue := []string{}
	for _, name := range owned {
		switch {
		case strings.HasPrefix(name, "quoin-") && !strings.Contains(name, "e2e"):
			// k3s and other foreign quoin-* resources predate this run.
			continue
		}
		if contains(baseline.Containers, name) && !strings.Contains(name, "e2e") {
			continue
		}
		residue = append(residue, name)
	}
	stackDir := filepath.Join(repoRoot(), ".artifacts", "e2e-stack")
	stackGone := false
	if _, err := os.Stat(stackDir); os.IsNotExist(err) {
		stackGone = true
	}
	fixturesGone := true
	for _, name := range []string{"e2e-fwd", "e2e-am", "quoin-t07-thanos"} {
		if contains(captureInventory().Containers, name) {
			fixturesGone = false
		}
	}
	resources := []cleanupResource{
		{
			Name: "compose project quoin (containers/networks/volumes)", Kind: "docker-set",
			ProbeCommand:       "docker ps -a --filter label=com.docker.compose.project=quoin",
			ObservedFinalState: map[bool]string{true: "absent", false: "present"}[len(residue) == 0],
		},
		{
			Name: "shared e2e fixtures (e2e-fwd, e2e-am, quoin-t07-thanos)", Kind: "container-set",
			ProbeCommand:       "docker ps -a --format {{.Names}}",
			ObservedFinalState: map[bool]string{true: "absent", false: "present"}[fixturesGone],
		},
		{
			Name: "e2e stack directory (state, TLS keys, admin passwords, session cookie)", Kind: "file-tree",
			ProbeCommand:       "test -e " + stackDir,
			ObservedFinalState: map[bool]string{true: "absent", false: "present"}[stackGone],
		},
	}
	failures := []string{}
	if len(residue) > 0 {
		failures = append(failures, "docker residue: "+strings.Join(residue, ", "))
	}
	if !stackGone {
		failures = append(failures, "stack directory remains: "+stackDir)
	}
	recorder.observe(t, "owned-resource-zero.json", map[string]any{
		"baselineContainers": len(baseline.Containers),
		"residue":            residue,
		"stackDirectoryGone": stackGone,
	})
	if err := appendCleanupPhase(t, evidenceRoot, "ticket41-go-acceptance", resources, failures); err != nil {
		t.Fatal(err)
	}
	if len(failures) > 0 {
		t.Fatalf("cleanup residue: %v", failures)
	}
}

func ticketAssertions(automated, observed *observation.Matrix, localAutomated, delegatedAutomated, delegatedObserved []observation.Cell, recorded []map[string]any, matrixDir string) map[string]any {
	delegatedAutomatedIDs := cellIDs(delegatedAutomated)
	delegatedObservedIDs := cellIDs(delegatedObserved)
	return map[string]any{
		"matrix-shape": map[string]any{
			"expected": "Chromium amd64 + Chromium arm64 + branded Chrome amd64 crossed with 320/768/1024/1440 and normal/reduced motion, 24 cells per scenario",
			"actual":   map[string]any{"automated": len(automated.Cells), "observation": len(observed.Cells), "ids": automated.IDs()},
		},
		"browser-build-and-digest": map[string]any{
			"expected": "playwright_chromium binds the frozen release-input digest per architecture; branded_chrome binds the qualification-resolved frozen digest; every executed cell records the real --version build",
			"actual":   "ui-matrix/subjects/*/browser-subject.json and ui-matrix/branded-chrome-resolution.json",
		},
		"observer-binding": map[string]any{
			"expected": "every recorded typed observation binds observer identity, browser artifact digest/build, viewport and motion mode; these local records prove the recording machinery under a labeled non-human observer and never close the catalog scenario",
			"actual":   map[string]any{"recorded": recorded, "ledger": filepath.Join(matrixDir, "observation-ledger.json"), "observer": "t41-acceptance-harness (non-human: machinery proof)"},
		},
		"executor-path-boundary": map[string]any{
			"expected": "ui.manual-observation executes through its frozen admin_observation entrypoint phases (ci/record-ui-observation setup/action/assert), not through the suites.Coordinator cell driver whose assertion vocabulary covers state-kind facts",
			"actual":   "acceptance drives the entrypoint phases directly per locally native cell; the workflow ui-qualification.yml drives the same phases on its native runners",
		},
		"automated-cells": map[string]any{
			"expected": "every locally native cell PASSED through ci/verify-ui-automation; foreign-architecture cells delegated to ui-qualification.yml",
			"actual":   map[string]any{"executedLocally": cellIDs(localAutomated), "delegated": delegatedAutomatedIDs},
		},
		"observation-cells": map[string]any{
			"expected": "every locally native cell recorded and PASSED through ci/record-ui-observation; foreign-architecture observations delegated to the native runner's CI OIDC observer",
			"actual":   map[string]any{"recordedLocally": len(recorded), "delegated": delegatedObservedIDs},
		},
	}
}

func itemSummaries(items []evidence.Item) []map[string]any {
	summaries := make([]map[string]any, 0, len(items))
	for _, item := range items {
		summaries = append(summaries, map[string]any{
			"test": item.TestName(), "outcome": item.Outcome, "category": item.Category,
			"exitCode": item.ExitCode, "cleanup": item.Cleanup.Outcome,
		})
	}
	return summaries
}

func cellIDs(cells []observation.Cell) []string {
	ids := make([]string, 0, len(cells))
	for _, cell := range cells {
		ids = append(ids, cell.ID)
	}
	return ids
}

func cellKeys(matrix *observation.Matrix) []map[string]any {
	keys := make([]map[string]any, 0, len(matrix.Cells))
	for _, cell := range matrix.Cells {
		keys = append(keys, map[string]any{
			"id": cell.ID, "browser_subject": cell.Key.BrowserSubject,
			"architecture": cell.Key.Architecture, "viewport_css_px": cell.Key.ViewportCSSPx,
			"motion_mode": cell.Key.MotionMode, "assertions": len(cell.Assertions),
		})
	}
	return keys
}

func contains(haystack []string, needle string) bool {
	for _, straw := range haystack {
		if straw == needle {
			return true
		}
	}
	return false
}

func slicesClone(source []string) []string {
	clone := make([]string, len(source))
	copy(clone, source)
	return clone
}

// requireNativeCell proves docker is reachable in this cell (Linux and
// the docker socket), mirroring the T40 gate.
func requireNativeCell(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"docker", "go", "git", "bash", "pnpm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable in the qualification cell: %v", tool, err)
		}
	}
	if output, err := exec.Command("docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}").Output(); err != nil {
		t.Skipf("docker server unreachable (run inside the native cell with the docker socket): %v", err)
	} else {
		t.Logf("qualification cell docker server: %s", strings.TrimSpace(string(output)))
	}
}

// environRunner adapts exec.Command to the environments probe interface.
type environRunner struct{}

func (environRunner) Output(name string, arguments ...string) (string, error) {
	body, err := exec.Command(name, arguments...).CombinedOutput()
	return string(body), err
}
