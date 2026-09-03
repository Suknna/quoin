package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/evidence"
)

// Facts document contract shared with executor entrypoints. The action or
// assert phase writes it to $QUOIN_VERIFY_FACTS; the runner compares the
// reported actuals against the catalog expectations and never accepts an
// executor's self-declared result.
const factsSchemaKind = "quoin-verify-facts-v1"

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

type phaseOutcome struct {
	command  string
	exitCode *int
	timedOut bool
	failed   bool // could not start / did not run
	stdout   bytes.Buffer
	stderr   bytes.Buffer
}

func (state *invocation) notRunItem(scenario *catalog.Scenario, cell catalog.Cell, causes []string) evidence.Item {
	environment := state.environment(scenario, cell)
	started := state.now().UTC().Format(time.RFC3339Nano)
	item := evidence.Item{
		ScenarioID: scenario.ID, CellID: cell.ID,
		InputDigest:  state.inputDigest(scenario, cell),
		ResultDigest: "",
		Outcome:      "warned", Category: "not_run",
		StartedAt: started, FinishedAt: started,
		AuthoritativeRecordedAt: started,
		AuthoritativeTimeSource: "ci_runner_clock",
		EnvironmentDigest:       evidence.Digest([]byte(evidence.CanonicalJSON(environment))),
		Environment:             environment,
		ToolVersion:             state.opts.ToolVersion,
		ArgvSanitized:           []string{},
		ExitCode:                nil,
		Assertions:              []evidence.Assertion{},
		Attachments:             []evidence.Attachment{},
		Cleanup:                 evidence.Cleanup{Required: scenario.Cleanup.Required, Outcome: "not_run", Assertions: []evidence.Assertion{}},
		CausalIDs:               causes,
		ProofRefs:               scenario.ProofRefs,
	}
	item.ResultDigest = state.resultDigest(item)
	return item
}

func (state *invocation) executeCell(scenario *catalog.Scenario, cell *catalog.Cell) (evidence.Item, error) {
	workdir := filepath.Join(state.opts.OutputDir, "cells", scenario.ID, cell.ID)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return evidence.Item{}, err
	}
	started := state.now().UTC()
	// Phase deadlines use the real clock: the injected Now only stamps
	// evidence timestamps and must never shorten actual execution budgets.
	deadline := time.Now().Add(time.Duration(scenario.TimeoutSeconds) * time.Second)
	parameters, _ := json.Marshal(cell.Parameters)
	factsPath := filepath.Join(workdir, "facts.json")

	phases := []struct{ name, command string }{
		{"setup", scenario.Phases.Setup},
		{"action", scenario.Phases.Action},
		{"assert", scenario.Phases.Assert},
	}
	results := map[string]*phaseOutcome{}
	for _, phase := range phases {
		outcome := state.runPhase(phase.name, phase.command, scenario, cell, workdir, factsPath, string(parameters), deadline)
		results[phase.name] = outcome
	}
	assertPhase := results["assert"]

	// Teardown always executes, even after setup/action/assert failures
	// (VERIFY-VERDICT-003); it gets its own full budget so a burned-down
	// execution window can never skip it.
	teardownBudget := time.Now().Add(time.Duration(scenario.TimeoutSeconds) * time.Second)
	teardown := state.runPhase("teardown", scenario.Phases.Teardown, scenario, cell, workdir, factsPath, string(parameters), teardownBudget)

	facts, factsValid := readFacts(factsPath)

	item := evidence.Item{
		ScenarioID: scenario.ID, CellID: cell.ID,
		InputDigest:             state.inputDigest(scenario, *cell),
		StartedAt:               started.Format(time.RFC3339Nano),
		FinishedAt:              state.now().UTC().Format(time.RFC3339Nano),
		AuthoritativeRecordedAt: state.now().UTC().Format(time.RFC3339Nano),
		AuthoritativeTimeSource: "ci_runner_clock",
		Environment:             state.environment(scenario, *cell),
		ToolVersion:             state.opts.ToolVersion,
		ExitCode:                assertPhase.exitCode,
		CausalIDs:               []string{},
		ProofRefs:               scenario.ProofRefs,
		Cleanup:                 cleanupOutcome(scenario, teardown),
	}
	item.EnvironmentDigest = evidence.Digest([]byte(evidence.CanonicalJSON(item.Environment)))
	item.ArgvSanitized = argvOf(results, teardown)

	category := "passed"
	invariant := false
	for _, assertion := range cell.Assertions {
		evaluated := evaluateAssertion(assertion, assertPhase, facts, factsValid)
		if evaluated.Result != "passed" {
			if evaluated.DetailCode == "unsupported_assertion_kind" {
				invariant = true
			}
			category = "functional_assertion_failed"
		}
		item.Assertions = append(item.Assertions, evaluated)
	}
	if invariant {
		category = "verifier_invariant_violation"
	}
	if phaseInterruption(results) {
		category = "infrastructure_interrupted"
	}
	if teardown.failed {
		// A teardown that cannot run leaves cleanup indeterminate unless the
		// catalog made cleanup mandatory, where residue fails the item.
		if scenario.Cleanup.Required {
			category = "cleanup_residue"
		} else if category == "passed" {
			category = "cleanup_indeterminate"
		}
	}
	item.Category = category
	item.Outcome = state.profile.Outcome(category)

	attachments, err := state.writeAttachments(scenario, cell, workdir, results, teardown, factsPath, factsValid)
	if err != nil {
		return evidence.Item{}, err
	}
	item.Attachments = attachments
	item.ResultDigest = state.resultDigest(item)
	return item, nil
}

// runPhase executes one catalog phase command through bash with the
// documented environment contract and a deadline from the scenario budget.
func (state *invocation) runPhase(name, command string, scenario *catalog.Scenario, cell *catalog.Cell, workdir, factsPath, parameters string, deadline time.Time) *phaseOutcome {
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
	// the output pipe would otherwise stall collection until its own exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	cmd.Dir = state.opts.RepoRoot
	cmd.Env = append(os.Environ(),
		"QUOIN_VERIFY_PHASE="+name,
		"QUOIN_VERIFY_LAYER="+state.opts.Layer,
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

func phaseInterruption(results map[string]*phaseOutcome) bool {
	for _, name := range []string{"setup", "action", "assert"} {
		outcome := results[name]
		if outcome == nil {
			return true
		}
		if outcome.failed || outcome.timedOut {
			return true
		}
	}
	return false
}

func cleanupOutcome(scenario *catalog.Scenario, teardown *phaseOutcome) evidence.Cleanup {
	cleanup := evidence.Cleanup{Required: scenario.Cleanup.Required, Assertions: []evidence.Assertion{}}
	switch {
	case teardown.timedOut || teardown.failed:
		if scenario.Cleanup.Required {
			cleanup.Outcome = "residue"
		} else {
			cleanup.Outcome = "not_run"
		}
	case teardown.exitCode != nil && *teardown.exitCode == 0:
		cleanup.Outcome = "clean"
	case teardown.command == "no-op" || teardown.command == "":
		cleanup.Outcome = "not_run"
	default:
		if scenario.Cleanup.Required {
			cleanup.Outcome = "residue"
		} else {
			cleanup.Outcome = "not_run"
		}
	}
	return cleanup
}

// evaluateAssertion compares declared expectations against machine-observed
// facts. The runner owns the comparison; executors only report actuals.
func evaluateAssertion(assertion catalog.Assertion, assertPhase *phaseOutcome, facts *factsDocument, factsValid bool) evidence.Assertion {
	evaluated := evidence.Assertion{ID: assertion.ID, Kind: assertion.Kind, Expected: assertion.Expected}
	switch assertion.Kind {
	case "exit_code":
		evaluated.Actual = nil
		if assertPhase.exitCode != nil {
			evaluated.Actual = *assertPhase.exitCode
		}
		evaluated.Result = "failed"
		if actual, ok := evaluated.Actual.(int); ok && actual == assertionInt(assertion.Expected) {
			evaluated.Result = "passed"
		}
	case "schema_valid":
		evaluated.Actual = factsValid
		evaluated.Result = map[bool]string{true: "passed", false: "failed"}[factsValid]
	case "state":
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
	if document.SchemaKind != factsSchemaKind {
		return nil, false
	}
	for _, check := range document.Checks {
		if check.Name == "" || (check.Result != "passed" && check.Result != "failed") {
			return nil, false
		}
	}
	return &document, true
}

func (state *invocation) environment(scenario *catalog.Scenario, cell catalog.Cell) evidence.Environment {
	environment := evidence.Environment{
		Architecture:    cell.Architecture,
		ToolchainDigest: state.toolchainDigest,
		CapabilityIDs:   cell.RequiredCapabilities,
	}
	if declared := state.catalog.Environment(cell.EnvironmentID); declared != nil {
		environment.Backend = backendOf(declared.Kind)
	}
	return environment
}

func backendOf(kind string) string {
	switch kind {
	case "contract_harness":
		return "contract"
	case "process_harness":
		return "process"
	case "docker_compose":
		return "compose"
	case "kubernetes":
		return "kubernetes"
	case "real_external_system":
		return "real_external"
	case "human_browser":
		return "human_browser"
	}
	return "contract"
}

func (state *invocation) inputDigest(scenario *catalog.Scenario, cell catalog.Cell) string {
	return evidence.Digest([]byte(evidence.CanonicalJSON(map[string]any{
		"scenario": scenario.ID, "cell": cell.ID, "parameters": cell.Parameters,
		"catalog": digestOfFileOrEmpty(state.opts.CatalogPath),
	})))
}

func (state *invocation) resultDigest(item evidence.Item) string {
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

func argvOf(results map[string]*phaseOutcome, teardown *phaseOutcome) []string {
	argv := make([]string, 0, 5)
	for _, name := range []string{"setup", "action", "assert"} {
		if outcome := results[name]; outcome != nil && outcome.command != "" {
			argv = append(argv, outcome.command)
		}
	}
	if teardown != nil && teardown.command != "" {
		argv = append(argv, teardown.command)
	}
	return argv
}

func (state *invocation) writeAttachments(scenario *catalog.Scenario, cell *catalog.Cell, workdir string, results map[string]*phaseOutcome, teardown *phaseOutcome, factsPath string, factsValid bool) ([]evidence.Attachment, error) {
	root := filepath.Join(state.opts.OutputDir, "cells", scenario.ID, cell.ID)
	var stdout, stderr bytes.Buffer
	for _, name := range []string{"setup", "action", "assert"} {
		outcome := results[name]
		if outcome == nil {
			continue
		}
		fmt.Fprintf(&stdout, "=== %s ===\n%s", name, outcome.stdout.String())
		fmt.Fprintf(&stderr, "=== %s ===\n%s", name, outcome.stderr.String())
	}
	fmt.Fprintf(&stdout, "=== teardown ===\n%s", teardown.stdout.String())
	fmt.Fprintf(&stderr, "=== teardown ===\n%s", teardown.stderr.String())

	attachments := []evidence.Attachment{}
	for _, attachment := range scenario.Evidence.Attachments {
		switch attachment {
		case evidence.AttachmentStdout:
			written, err := evidence.Write(root, "stdout.txt", evidence.AttachmentStdout, "text/plain", stdout.Bytes(), false)
			if err != nil {
				return nil, err
			}
			attachments = append(attachments, written)
		case evidence.AttachmentStderr:
			written, err := evidence.Write(root, "stderr.txt", evidence.AttachmentStderr, "text/plain", stderr.Bytes(), false)
			if err != nil {
				return nil, err
			}
			attachments = append(attachments, written)
		case evidence.AttachmentStructuredResult:
			body := []byte(`{"schema_kind":"` + factsSchemaKind + `","error":"facts not written"}`)
			if factsValid {
				if raw, err := os.ReadFile(factsPath); err == nil {
					body = raw
				}
			}
			written, err := evidence.Write(root, "structured-result.json", evidence.AttachmentStructuredResult, "application/json", body, false)
			if err != nil {
				return nil, err
			}
			attachments = append(attachments, written)
		}
	}
	return attachments, nil
}

func validateJSONAgainstSchema(schemaPath string, body []byte) error {
	schemaBody, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	var schemaDocument any
	if err := json.Unmarshal(schemaBody, &schemaDocument); err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaPath, schemaDocument); err != nil {
		return err
	}
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		return err
	}
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return err
	}
	return schema.Validate(document)
}
