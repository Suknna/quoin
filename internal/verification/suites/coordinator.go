package suites

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/evidence"
	"github.com/Suknna/quoin/internal/verification/result"
)

// FactsSchemaKind mirrors the runner's executor facts contract: the
// action/assert phase writes it to $QUOIN_VERIFY_FACTS and this
// coordinator — never the executor — compares the reported actuals
// against the catalog expectations.
const FactsSchemaKind = "quoin-verify-facts-v1"

// Phase names of the catalog's four-phase contract.
const (
	PhaseSetup    = "setup"
	PhaseAction   = "action"
	PhaseAssert   = "assert"
	PhaseTeardown = "teardown"
)

// Coordinator executes release-qualification cells for one target and
// accumulates the evidence items; its verdict comes from the frozen
// profile alone.
type Coordinator struct {
	Catalog      *catalog.Catalog
	Profile      *result.Profile
	Target       Target
	OutputDir    string
	RepoRoot     string
	InvocationID string
	ToolVersion  string
	Subject      result.Subject
	Now          func() time.Time
	Items        []evidence.Item

	byScenario map[string][]evidence.Item
}

// NotRunItem records a dependent cell as not_run with the causal IDs of
// the failed dependencies (VERIFY-CATALOG-002); the caller continues
// with independent scenarios.
func (coordinator *Coordinator) NotRunItem(scenario *catalog.Scenario, cell catalog.Cell, causes []string) evidence.Item {
	environment := coordinator.environmentOf(scenario, cell)
	stamp := coordinator.now().UTC().Format(time.RFC3339Nano)
	item := evidence.Item{
		ScenarioID: scenario.ID, CellID: cell.ID,
		InputDigest: inputDigest(scenario, cell, coordinator.Catalog),
		Outcome:     coordinator.Profile.Outcome("not_run"), Category: "not_run",
		StartedAt: stamp, FinishedAt: stamp,
		AuthoritativeRecordedAt: stamp, AuthoritativeTimeSource: "ci_runner_clock",
		EnvironmentDigest: evidence.Digest([]byte(evidence.CanonicalJSON(environment))),
		Environment:       environment,
		ToolVersion:       coordinator.ToolVersion,
		ArgvSanitized:     []string{},
		Assertions:        []evidence.Assertion{},
		Attachments:       []evidence.Attachment{},
		Cleanup:           evidence.Cleanup{Required: scenario.Cleanup.Required, Outcome: "not_run", Assertions: []evidence.Assertion{}},
		CausalIDs:         causes,
		ProofRefs:         scenario.ProofRefs,
	}
	item.ResultDigest = resultDigestOf(item)
	coordinator.record(item)
	return item
}

// DependencyCauses returns the causal references blocking a scenario:
// every dependency scenario of this invocation without fully passing
// applicable results, or a synthetic reference when a dependency had no
// applicable result at all.
func (coordinator *Coordinator) DependencyCauses(scenario *catalog.Scenario) []string {
	var causes []string
	for _, dependency := range scenario.DependsOn {
		dependencyItems := coordinator.byScenario[dependency]
		if len(dependencyItems) == 0 {
			causes = append(causes, fmt.Sprintf("%s:%s:no-applicable-result", coordinator.InvocationID, dependency))
			continue
		}
		for _, item := range dependencyItems {
			if item.Outcome != "passed" {
				causes = append(causes, fmt.Sprintf("%s:%s:%s", coordinator.InvocationID, item.ScenarioID, item.CellID))
			}
		}
	}
	return causes
}

// ExecuteCell runs one cell's four phases through the resolved catalog
// commands. Teardown always executes (VERIFY-VERDICT-003); independent
// cells are unaffected by earlier failures because the coordinator
// never aborts.
func (coordinator *Coordinator) ExecuteCell(scenario *catalog.Scenario, cell *catalog.Cell, backend string, env []string) (evidence.Item, error) {
	if coordinator.byScenario == nil {
		coordinator.byScenario = map[string][]evidence.Item{}
	}
	workdir := filepath.Join(coordinator.OutputDir, "cells", scenario.ID, cell.ID)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return evidence.Item{}, err
	}
	started := coordinator.now().UTC()
	// Phase deadlines use the real clock so a burned-down budget can
	// never silently skip teardown.
	deadline := time.Now().Add(time.Duration(scenario.TimeoutSeconds) * time.Second)
	parameters, _ := json.Marshal(cell.Parameters)
	factsPath := filepath.Join(workdir, "facts.json")

	phases := []struct{ name, command string }{
		{PhaseSetup, scenario.Phases.Setup},
		{PhaseAction, scenario.Phases.Action},
		{PhaseAssert, scenario.Phases.Assert},
	}
	outcomes := map[string]*phaseOutcome{}
	argv := []string{}
	aborted := false
	for _, phase := range phases {
		resolved, err := ResolvePhase(phase.command, backend)
		if err != nil {
			return evidence.Item{}, err
		}
		// A failed setup or action aborts the remaining functional
		// phases as not-run: driving assert against an environment whose
		// action never completed would misattribute environment failure
		// as product failure. Teardown below still always executes
		// (VERIFY-VERDICT-003).
		if aborted {
			outcomes[phase.name] = &phaseOutcome{command: resolved, failed: true}
			continue
		}
		outcome := coordinator.runPhase(resolved, scenario, cell, workdir, factsPath, string(parameters), deadline, env, phase.name)
		outcomes[phase.name] = outcome
		if resolved != "" && resolved != "no-op" {
			argv = append(argv, resolved)
		}
		if outcome.failed || outcome.timedOut || (outcome.exitCode != nil && *outcome.exitCode != 0) {
			aborted = true
		}
	}
	assertPhase := outcomes[PhaseAssert]

	teardownBudget := time.Now().Add(time.Duration(scenario.TimeoutSeconds) * time.Second)
	teardownResolved, err := ResolvePhase(scenario.Phases.Teardown, backend)
	if err != nil {
		return evidence.Item{}, err
	}
	teardown := coordinator.runPhase(teardownResolved, scenario, cell, workdir, factsPath, string(parameters), teardownBudget, env, PhaseTeardown)
	if teardownResolved != "" && teardownResolved != "no-op" {
		argv = append(argv, teardownResolved)
	}

	facts, factsValid := readFacts(factsPath)
	environment := coordinator.environmentOf(scenario, *cell)
	item := evidence.Item{
		ScenarioID: scenario.ID, CellID: cell.ID,
		InputDigest:             inputDigest(scenario, *cell, coordinator.Catalog),
		StartedAt:               started.Format(time.RFC3339Nano),
		FinishedAt:              coordinator.now().UTC().Format(time.RFC3339Nano),
		AuthoritativeRecordedAt: coordinator.now().UTC().Format(time.RFC3339Nano),
		AuthoritativeTimeSource: "ci_runner_clock",
		Environment:             environment,
		EnvironmentDigest:       evidence.Digest([]byte(evidence.CanonicalJSON(environment))),
		ToolVersion:             coordinator.ToolVersion,
		ExitCode:                assertPhase.exitCode,
		ArgvSanitized:           argv,
		CausalIDs:               []string{},
		ProofRefs:               scenario.ProofRefs,
		Cleanup:                 cleanupOutcome(scenario, teardown),
	}

	category := "passed"
	for _, assertion := range cell.Assertions {
		evaluated := evaluateAssertion(assertion, assertPhase, facts, factsValid)
		if evaluated.Result != "passed" {
			category = "functional_assertion_failed"
		}
		item.Assertions = append(item.Assertions, evaluated)
	}
	if phaseInterruption(outcomes) {
		category = "infrastructure_interrupted"
	}
	if teardown.failed {
		if scenario.Cleanup.Required {
			category = "cleanup_residue"
		} else if category == "passed" {
			category = "cleanup_indeterminate"
		}
	}
	item.Category = category
	item.Outcome = coordinator.Profile.Outcome(category)

	attachments, err := writeAttachments(scenario, workdir, outcomes, teardown, factsPath, factsValid)
	if err != nil {
		return evidence.Item{}, err
	}
	item.Attachments = attachments
	item.ResultDigest = resultDigestOf(item)
	coordinator.record(item)
	return item, nil
}

func (coordinator *Coordinator) record(item evidence.Item) {
	coordinator.Items = append(coordinator.Items, item)
	if coordinator.byScenario == nil {
		coordinator.byScenario = map[string][]evidence.Item{}
	}
	scenario := coordinator.Catalog.Scenario(item.ScenarioID)
	if scenario != nil {
		coordinator.byScenario[scenario.ID] = append(coordinator.byScenario[scenario.ID], item)
	}
}

func (coordinator *Coordinator) now() time.Time {
	if coordinator.Now != nil {
		return coordinator.Now()
	}
	return time.Now()
}

// Aggregate returns the suite verdict of everything executed so far.
func (coordinator *Coordinator) Aggregate() string {
	return coordinator.Profile.Aggregate(coordinator.requiredItems())
}

// requiredItems drops diagnostic items from the required set
// (VERIFY-VERDICT-005) before aggregation.
func (coordinator *Coordinator) requiredItems() []evidence.Item {
	required := make([]evidence.Item, 0, len(coordinator.Items))
	for _, item := range coordinator.Items {
		scenario := coordinator.Catalog.Scenario(item.ScenarioID)
		if scenario != nil && scenario.Requirement == catalog.RequirementDiagnostic {
			continue
		}
		required = append(required, item)
	}
	return required
}

// EnvironmentItems returns the environment matrix digest inputs for
// statements: the per-item environments actually executed.
func (coordinator *Coordinator) EnvironmentItems() []evidence.Item {
	return coordinator.requiredItems()
}

type phaseOutcome struct {
	command  string
	exitCode *int
	timedOut bool
	failed   bool
	stdout   bytes.Buffer
	stderr   bytes.Buffer
}

// runPhase executes one resolved phase command through bash with the
// frozen environment contract and a deadline from the scenario budget.
func (coordinator *Coordinator) runPhase(command string, scenario *catalog.Scenario, cell *catalog.Cell, workdir, factsPath, parameters string, deadline time.Time, extraEnv []string, phase string) *phaseOutcome {
	outcome := &phaseOutcome{command: command}
	if command == "" || command == "no-op" {
		code := 0
		outcome.exitCode = &code
		return outcome
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		outcome.timedOut = true
		outcome.failed = true
		return outcome
	}
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	// Kill the whole process group on timeout: an orphaned child holding
	// the output pipe would otherwise stall collection.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	cmd.Dir = coordinator.RepoRoot
	cmd.Env = append(append(os.Environ(), extraEnv...),
		"QUOIN_VERIFY_PHASE="+phase,
		"QUOIN_VERIFY_LAYER="+catalog.LayerReleaseQualification,
		"QUOIN_VERIFY_SCENARIO="+scenario.ID,
		"QUOIN_VERIFY_CELL="+cell.ID,
		"QUOIN_VERIFY_PARAMETERS="+parameters,
		"QUOIN_VERIFY_FACTS="+factsPath,
		"QUOIN_VERIFY_WORKDIR="+workdir,
	)
	cmd.Stdout = &outcome.stdout
	cmd.Stderr = &outcome.stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			outcome.timedOut = true
			outcome.failed = true
			return outcome
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			outcome.failed = true
			return outcome
		}
	}
	code := cmd.ProcessState.ExitCode()
	outcome.exitCode = &code
	return outcome
}

func phaseInterruption(outcomes map[string]*phaseOutcome) bool {
	for _, name := range []string{PhaseSetup, PhaseAction, PhaseAssert} {
		outcome := outcomes[name]
		if outcome == nil || outcome.failed || outcome.timedOut {
			return true
		}
	}
	return false
}

func cleanupOutcome(scenario *catalog.Scenario, teardown *phaseOutcome) evidence.Cleanup {
	cleanup := evidence.Cleanup{Required: scenario.Cleanup.Required, Assertions: []evidence.Assertion{}}
	switch {
	case teardown.command == "no-op" || teardown.command == "":
		cleanup.Outcome = "not_run"
	case teardown.timedOut || teardown.failed:
		if scenario.Cleanup.Required {
			cleanup.Outcome = "residue"
		} else {
			cleanup.Outcome = "not_run"
		}
	case teardown.exitCode != nil && *teardown.exitCode == 0:
		cleanup.Outcome = "clean"
	default:
		if scenario.Cleanup.Required {
			cleanup.Outcome = "residue"
		} else {
			cleanup.Outcome = "not_run"
		}
	}
	return cleanup
}

type factValue struct {
	Actual any `json:"actual"`
}

type factCheck struct {
	Name   string `json:"name"`
	Result string `json:"result"`
}

type factsDocument struct {
	SchemaKind string               `json:"schema_kind"`
	Assertions map[string]factValue `json:"assertions"`
	Checks     []factCheck          `json:"checks"`
}

// evaluateAssertion compares declared expectations against
// machine-observed facts. The coordinator owns the comparison;
// executors only report actuals (VERIFY-VERDICT-004).
func evaluateAssertion(assertion catalog.Assertion, assertPhase *phaseOutcome, facts *factsDocument, factsValid bool) evidence.Assertion {
	evaluated := evidence.Assertion{ID: assertion.ID, Kind: assertion.Kind, Expected: assertion.Expected}
	switch assertion.Kind {
	case "exit_code":
		evaluated.Actual = nil
		if assertPhase.exitCode != nil {
			evaluated.Actual = *assertPhase.exitCode
		}
		evaluated.Result = "failed"
		if code, ok := evaluated.Actual.(int); ok && code == assertionInt(assertion.Expected) {
			evaluated.Result = "passed"
		}
	case "schema_valid":
		evaluated.Actual = factsValid
		evaluated.Result = map[bool]string{true: "passed", false: "failed"}[factsValid]
	case "state", "protocol_event", "http_response":
		// Deployment scenarios report protocol and HTTP observations the
		// same deterministic way: the executor freezes the observed
		// actual; the catalog expectation stays the comparison authority.
		if !factsValid || facts == nil {
			evaluated.Actual = nil
			evaluated.Result = "failed"
			evaluated.DetailCode = "facts_unavailable"
			break
		}
		fact, ok := facts.Assertions[assertion.ID]
		if !ok {
			evaluated.Actual = nil
			evaluated.Result = "failed"
			evaluated.DetailCode = "fact_missing"
			break
		}
		evaluated.Actual = fact.Actual
		if evidence.CanonicalJSON(fact.Actual) == evidence.CanonicalJSON(assertion.Expected) {
			evaluated.Result = "passed"
		} else {
			evaluated.Result = "failed"
		}
	default:
		evaluated.Actual = nil
		evaluated.Result = "failed"
		evaluated.DetailCode = "unsupported_assertion_kind"
	}
	return evaluated
}

func assertionInt(expected any) int {
	switch value := expected.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	}
	return -1
}

func readFacts(path string) (*factsDocument, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var document factsDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, false
	}
	if document.SchemaKind != FactsSchemaKind {
		return nil, false
	}
	for _, check := range document.Checks {
		if check.Name == "" || (check.Result != "passed" && check.Result != "failed") {
			return nil, false
		}
	}
	return &document, true
}

// WriteFacts persists an executor's facts document for the assert
// comparison.
func WriteFacts(path string, assertions map[string]any, checks []map[string]string) error {
	document := map[string]any{
		"schema_kind": FactsSchemaKind,
		"assertions":  map[string]any{},
		"checks":      checks,
	}
	for id, actual := range assertions {
		document["assertions"].(map[string]any)[id] = map[string]any{"actual": actual}
	}
	body, err := json.Marshal(document)
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func (coordinator *Coordinator) environmentOf(scenario *catalog.Scenario, cell catalog.Cell) evidence.Environment {
	environment := evidence.Environment{
		Architecture:    cell.Architecture,
		ToolchainDigest: evidence.Digest([]byte(coordinator.ToolVersion)),
		CapabilityIDs:   cell.RequiredCapabilities,
	}
	if declared := coordinator.Catalog.Environment(cell.EnvironmentID); declared != nil {
		switch declared.Kind {
		case "docker_compose":
			environment.Backend = "compose"
		case "kubernetes":
			environment.Backend = "kubernetes"
		case "process_harness":
			environment.Backend = "process"
		case "contract_harness":
			environment.Backend = "contract"
		}
	}
	return environment
}

func inputDigest(scenario *catalog.Scenario, cell catalog.Cell, loaded *catalog.Catalog) string {
	return evidence.Digest([]byte(evidence.CanonicalJSON(map[string]any{
		"scenario": scenario.ID, "cell": cell.ID, "parameters": cell.Parameters,
	})))
}

func resultDigestOf(item evidence.Item) string {
	assertions := make([]map[string]any, 0, len(item.Assertions))
	for _, assertion := range item.Assertions {
		assertions = append(assertions, map[string]any{"id": assertion.ID, "result": assertion.Result})
	}
	return evidence.Digest([]byte(evidence.CanonicalJSON(map[string]any{
		"scenario": item.ScenarioID, "cell": item.CellID,
		"outcome": item.Outcome, "category": item.Category,
		"assertions": assertions, "cleanup": item.Cleanup.Outcome,
	})))
}

// writeAttachments persists stdout/stderr/structured-result exactly as
// the contract-gate runner does, so downstream evidence consumers see
// one shape.
func writeAttachments(scenario *catalog.Scenario, workdir string, outcomes map[string]*phaseOutcome, teardown *phaseOutcome, factsPath string, factsValid bool) ([]evidence.Attachment, error) {
	var stdout, stderr bytes.Buffer
	for _, name := range []string{PhaseSetup, PhaseAction, PhaseAssert} {
		outcome := outcomes[name]
		if outcome == nil {
			continue
		}
		fmt.Fprintf(&stdout, "=== %s ===\n%s", name, outcome.stdout.String())
		fmt.Fprintf(&stderr, "=== %s ===\n%s", name, outcome.stderr.String())
	}
	fmt.Fprintf(&stdout, "=== %s ===\n%s", PhaseTeardown, teardown.stdout.String())
	fmt.Fprintf(&stderr, "=== %s ===\n%s", PhaseTeardown, teardown.stderr.String())

	attachments := []evidence.Attachment{}
	allowed := map[string]bool{}
	for _, kind := range scenario.Evidence.Attachments {
		allowed[kind] = true
	}
	if allowed[evidence.AttachmentStdout] {
		written, err := evidence.Write(workdir, "stdout.txt", evidence.AttachmentStdout, "text/plain", stdout.Bytes(), false)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, written)
	}
	if allowed[evidence.AttachmentStderr] {
		written, err := evidence.Write(workdir, "stderr.txt", evidence.AttachmentStderr, "text/plain", stderr.Bytes(), false)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, written)
	}
	if allowed[evidence.AttachmentStructuredResult] {
		body := []byte(`{"schema_kind":"` + FactsSchemaKind + `","error":"facts not written"}`)
		if factsValid {
			if raw, err := os.ReadFile(factsPath); err == nil {
				body = raw
			}
		}
		written, err := evidence.Write(workdir, "structured-result.json", evidence.AttachmentStructuredResult, "application/json", body, false)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, written)
	}
	return attachments, nil
}

// SortedTestNames lists the executed items' test names in stable
// order (statement passed/warned/failed projections).
func SortedTestNames(items []evidence.Item) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.TestName())
	}
	sort.Strings(names)
	return names
}

// Statement assembles the in-toto Test Result for the executed item
// set, reusing the frozen statement shape. The verdict is always the
// profile aggregation over the required items.
func (coordinator *Coordinator) Statement(catalogDigest, profileDigest string) result.Statement {
	required := coordinator.requiredItems()
	passed, warned, failed := result.TestNameSets(required)
	started, finished := windowOf(required)
	matrix := result.EnvironmentMatrixDigest(required)
	subject := coordinator.Subject
	if subject.Name == "" {
		subject = result.Subject{Name: "quoin-release", Digest: map[string]string{"sha256": "unset"}}
	}
	return result.Statement{
		Type:          result.StatementType,
		PredicateType: result.PredicateType,
		Subject:       []result.Subject{subject},
		Predicate: result.Predicate{
			Result:      coordinator.Profile.Aggregate(required),
			PassedTests: passed, WarnedTests: warned, FailedTests: failed,
			Quoin: result.QuoinExtension{
				ProfileVersion: result.ProfileVersion,
				InvocationID:   coordinator.InvocationID,
				Layer:          catalog.LayerReleaseQualification,
				StartedAt:      started, FinishedAt: finished,
				Environment: matrix,
			},
		},
	}
}

func windowOf(items []evidence.Item) (string, string) {
	if len(items) == 0 {
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		return stamp, stamp
	}
	started, finished := items[0].StartedAt, items[0].FinishedAt
	for _, item := range items {
		if item.StartedAt < started {
			started = item.StartedAt
		}
		if item.FinishedAt > finished {
			finished = item.FinishedAt
		}
	}
	return started, finished
}

// String renders a short phase summary for logs.
func (outcome *phaseOutcome) String() string {
	code := "none"
	if outcome.exitCode != nil {
		code = fmt.Sprint(*outcome.exitCode)
	}
	state := "ran"
	if outcome.timedOut {
		state = "timedOut"
	} else if outcome.failed {
		state = "failedToRun"
	}
	return fmt.Sprintf("%s(%s,%s)", state, code, strings.TrimSpace(outcome.stderr.String()[:min(120, outcome.stderr.Len())]))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
