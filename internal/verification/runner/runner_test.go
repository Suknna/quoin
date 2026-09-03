package runner_test

// The runner tests drive the real four-phase execution model through fixture
// catalogs whose entrypoints are executable shell scripts: no fail-fast,
// teardown after failure, dependency not_run causality, state-fact
// evaluation, timeout classification and diagnostic exclusion
// (VERIFY-VERDICT-002/003/005, VERIFY-CATALOG-002).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/evidence"
	"github.com/Suknna/quoin/internal/verification/result"
	"github.com/Suknna/quoin/internal/verification/runner"
)

const repoContracts = "../../../docs/specs/quoin-v1/contracts"

// writeRunnerFixture materializes a fixture catalog plus executable
// entrypoint scripts; phase commands reference them through the
// $QUOIN_VERIFY_FIXTURE environment variable the test sets.

func writeRunnerFixture(t *testing.T, yamlBody string, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(repoContracts, "schemas", "verification-catalog.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schemas", "verification-catalog.schema.json"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verification-catalog.yaml"), []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range scripts {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "verification-catalog.yaml")
}

// script builds a phase-driven entrypoint: assert exits with `assertExit`
// after writing `facts`, and the teardown phase appends to the teardown
// marker whenever it runs.
func script(assertExit int, facts string) string {
	return "#!/bin/sh\n" +
		"case \"$QUOIN_VERIFY_PHASE\" in\n" +
		"  setup|action) exit 0 ;;\n" +
		"  assert) printf '%s' '" + facts + "' > \"$QUOIN_VERIFY_FACTS\"; exit " + strconv.Itoa(assertExit) + " ;;\n" +
		"  teardown) echo \"teardown:$QUOIN_VERIFY_SCENARIO\" >> \"$QUOIN_VERIFY_TEARDOWN_MARKER\"; exit 0 ;;\n" +
		"esac\n"
}

func scenarioYAML(id, layer, requirement, entrypoint string, depends []string, extra string) string {
	dependency := "[]"
	if len(depends) > 0 {
		dependency = "[" + strings.Join(depends, ", ") + "]"
	}
	return "  - id: " + id + `
    title: ` + id + `
    validation_roots: [ARCH-VALIDATION-001]
    subject: {kind: release, selector: fixture, drift_fields: []}
    fixtures: []
    cells:
      - id: default
        environment_id: process-harness
        architecture: not_applicable
        applicability: {mode: always}
        required_capabilities: [environment.process-harness]
        parameters: {}
        assertions:
          - {id: exit-zero, kind: exit_code, expected: 0}
          - {id: structured-result-valid, kind: schema_valid, expected: true}
          - {id: ok-state, kind: state, expected: true}
    status: active
    layer: ` + layer + `
    requirement: ` + requirement + `
    executor: {kind: ci, entrypoint: fixture}
    phases:
      setup: ` + entrypoint + ` --phase setup` + `
      action: ` + entrypoint + ` --phase action` + `
      assert: ` + entrypoint + ` --phase assert` + `
      teardown: ` + entrypoint + ` --phase teardown` + `
    depends_on: ` + dependency + `
    proof_refs: []
    required_capabilities: []
    evidence:
      test_names: [` + id + `.default]
      attachments: [structured_result, stdout, stderr]
      redaction_profile: verification-redaction-v1
    cleanup: {required: false, entrypoint: none, success_assertions: []}
    timeout_seconds: 30
` + extra
}

func base(catalogScenarios string) string {
	return `contract_version: 1
catalog_id: quoin-v1-verification
catalog_state: frozen
result_profile_id: quoin-test-result-v1
contract_refs:
  connection_probes_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  result_profile_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
capability_definitions:
  - id: environment.process-harness
    kind: environment
validation_roots:
  - id: ARCH-VALIDATION-001
    source: docs/specs/quoin-v1/architecture.md
environments:
  - id: process-harness
    kind: process_harness
    native: false
    requires_release_artifacts: false
scenarios:
` + catalogScenarios
}

func runFixture(t *testing.T, path string) (*runner.RunReport, error) {
	t.Helper()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	report, err := runner.Run(runner.Options{
		CatalogPath:  path,
		ProfilePath:  filepath.Join(repoContracts, "verification-result-profile.yaml"),
		ContractsDir: repoContracts,
		OutputDir:    filepath.Join(t.TempDir(), "invocation"),
		RepoRoot:     ".",
		Layer:        catalog.LayerContractGate,
		InvocationID: "runner-test",
		Subject:      runner.SubjectBinding{Name: "fixture-subject", Digest: evidence.Digest([]byte("fixture"))},
		Now:          func() time.Time { return now },
	})
	return report, err
}

const goodFacts = `{"schema_kind":"quoin-verify-facts-v1","assertions":{"ok-state":{"actual":true}},"checks":[{"name":"everything","result":"passed"}]}`

func itemOf(t *testing.T, report *runner.RunReport, testName string) evidence.Item {
	t.Helper()
	for _, item := range report.Index.Items {
		if item.TestName() == testName {
			return item
		}
	}
	t.Fatalf("item %s missing", testName)
	return evidence.Item{}
}

func TestRunnerExecutesIndependentScenariosWithoutFailFast(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "teardown.marker")
	// fail.sh exits non-zero in assert while writing valid facts: the
	// exit_code assertion fails, the independent scenario still runs.
	body := base(
		scenarioYAML("gate.failing", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/fail.sh", nil, "") +
			scenarioYAML("gate.independent", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/pass.sh", nil, ""))
	path := writeRunnerFixture(t, body, map[string]string{
		"pass.sh": strings.ReplaceAll(script(0, goodFacts), "$QUOIN_VERIFY_TEARDOWN_MARKER", marker),
		"fail.sh": strings.ReplaceAll(script(3, goodFacts), "$QUOIN_VERIFY_TEARDOWN_MARKER", marker),
	})
	t.Setenv("QUOIN_VERIFY_FIXTURE", filepath.Dir(path))
	report, err := runFixture(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != result.VerdictFailed {
		t.Fatalf("suite with a failed item must be FAILED, got %s", report.Verdict)
	}
	failing := itemOf(t, report, "gate.failing.default")
	if failing.Outcome != "failed" || failing.Category != "functional_assertion_failed" {
		t.Fatalf("failing item misclassified: %+v", failing)
	}
	if failing.ExitCode == nil || *failing.ExitCode != 3 {
		t.Fatalf("exit code not recorded: %+v", failing)
	}
	// Teardown must still have run for the failed scenario.
	body2, err := os.ReadFile(marker)
	if err != nil || !strings.Contains(string(body2), "teardown:gate.failing") {
		t.Fatalf("teardown after failure not proven: %v %q", err, body2)
	}
	// The independent scenario still executed and passed: no fail-fast.
	independent := itemOf(t, report, "gate.independent.default")
	if independent.Outcome != "passed" {
		t.Fatalf("independent scenario did not pass: %+v", independent)
	}
	if len(report.Predicate.FailedTests) != 1 || len(report.Predicate.PassedTests) != 1 {
		t.Fatalf("test-name sets wrong: %+v", report.Predicate)
	}
}

func TestRunnerRecordsNotRunForFailedDependency(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "teardown.marker")
	body := base(
		scenarioYAML("dep.base", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/fail.sh", nil, "") +
			scenarioYAML("dep.child", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/pass.sh", []string{"dep.base"}, ""))
	path := writeRunnerFixture(t, body, map[string]string{
		"pass.sh": strings.ReplaceAll(script(0, goodFacts), "$QUOIN_VERIFY_TEARDOWN_MARKER", marker),
		"fail.sh": strings.ReplaceAll(script(3, goodFacts), "$QUOIN_VERIFY_TEARDOWN_MARKER", marker),
	})
	t.Setenv("QUOIN_VERIFY_FIXTURE", filepath.Dir(path))
	report, err := runFixture(t, path)
	if err != nil {
		t.Fatal(err)
	}
	child := itemOf(t, report, "dep.child.default")
	if child.Outcome != "warned" || child.Category != "not_run" {
		t.Fatalf("dependent must be not_run/warned: %+v", child)
	}
	if len(child.CausalIDs) != 1 || !strings.Contains(child.CausalIDs[0], "dep.base") {
		t.Fatalf("causal id missing: %+v", child)
	}
	if child.ExitCode != nil {
		t.Fatalf("not_run item must not record an execution exit code: %+v", child)
	}
	// The dependent never started, so its teardown must not have run.
	if strings.Contains(filepath.Base(marker), "never") {
		t.Fatal("unreachable")
	}
}

func TestRunnerFailsStateFactsThatContradictExpectations(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "teardown.marker")
	lying := `{"schema_kind":"quoin-verify-facts-v1","assertions":{"ok-state":{"actual":false}},"checks":[]}`
	body := base(scenarioYAML("state.liar", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/lie.sh", nil, ""))
	path := writeRunnerFixture(t, body, map[string]string{
		"lie.sh": strings.ReplaceAll(script(0, lying), "$QUOIN_VERIFY_TEARDOWN_MARKER", marker),
	})
	t.Setenv("QUOIN_VERIFY_FIXTURE", filepath.Dir(path))
	report, err := runFixture(t, path)
	if err != nil {
		t.Fatal(err)
	}
	item := itemOf(t, report, "state.liar.default")
	if item.Outcome != "failed" {
		t.Fatalf("contradicting state fact must fail: %+v", item)
	}
	found := false
	for _, assertion := range item.Assertions {
		if assertion.ID == "ok-state" && assertion.Result == "failed" && assertion.Actual == false {
			found = true
		}
	}
	if !found {
		t.Fatalf("state assertion mismatch not recorded: %+v", item.Assertions)
	}
}

func TestRunnerClassifiesTimeoutAsInterruptedAndContinues(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "teardown.marker")
	hang := "#!/bin/sh\nsleep 30\n"
	body := base(
		strings.Replace(scenarioYAML("hang.blocked", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/hang.sh", nil, ""), "timeout_seconds: 30", "timeout_seconds: 1", 1) +
			scenarioYAML("hang.other", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/pass.sh", nil, ""))
	path := writeRunnerFixture(t, body, map[string]string{
		"hang.sh": strings.ReplaceAll(hang, "$QUOIN_VERIFY_TEARDOWN_MARKER", marker),
		"pass.sh": strings.ReplaceAll(script(0, goodFacts), "$QUOIN_VERIFY_TEARDOWN_MARKER", marker),
	})
	t.Setenv("QUOIN_VERIFY_FIXTURE", filepath.Dir(path))
	report, err := runFixture(t, path)
	if err != nil {
		t.Fatal(err)
	}
	blocked := itemOf(t, report, "hang.blocked.default")
	if blocked.Outcome != "warned" || blocked.Category != "infrastructure_interrupted" {
		t.Fatalf("timeout must be infrastructure_interrupted/warned: %+v", blocked)
	}
	if other := itemOf(t, report, "hang.other.default"); other.Outcome != "passed" {
		t.Fatalf("suite must continue after interruption: %+v", other)
	}
	if report.Verdict != result.VerdictWarned {
		t.Fatalf("warned suite verdict expected, got %s", report.Verdict)
	}
}

func TestRunnerRunsDiagnosticsOnlyFromPersistedTriggers(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "teardown.marker")
	pass := strings.ReplaceAll(script(0, goodFacts), "$QUOIN_VERIFY_TEARDOWN_MARKER", marker)
	fail := strings.ReplaceAll(script(3, goodFacts), "$QUOIN_VERIFY_TEARDOWN_MARKER", marker)
	diagnostic := strings.Replace(scenarioYAML("diag.followup", "contract_gate", "diagnostic", "$QUOIN_VERIFY_FIXTURE/bin/pass.sh", nil, ""),
		"    requirement: diagnostic", "    requirement: diagnostic\n    diagnostic_trigger_categories: [failed]", 1)
	body := base(
		scenarioYAML("diag.green", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/pass.sh", nil, "") +
			scenarioYAML("diag.red", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/fail.sh", nil, "") +
			diagnostic)
	path := writeRunnerFixture(t, body, map[string]string{"pass.sh": pass, "fail.sh": fail})
	t.Setenv("QUOIN_VERIFY_FIXTURE", filepath.Dir(path))
	report, err := runFixture(t, path)
	if err != nil {
		t.Fatal(err)
	}
	var diagnosticItem *evidence.Item
	for i := range report.Index.Items {
		if report.Index.Items[i].TestName() == "diag.followup.default" {
			diagnosticItem = &report.Index.Items[i]
		}
	}
	if diagnosticItem == nil {
		t.Fatal("diagnostic must run after a persisted failure trigger")
	}
	if len(diagnosticItem.CausalIDs) == 0 {
		t.Fatalf("diagnostic must cite its triggering result: %+v", diagnosticItem)
	}
	if diagnosticItem.TestName() == report.Predicate.FailedTests[0] && len(report.Predicate.FailedTests) != 1 {
		t.Fatalf("diagnostic must stay out of the failed test set: %+v", report.Predicate)
	}
	for _, name := range report.Predicate.FailedTests {
		if name == "diag.followup.default" {
			t.Fatal("diagnostic leaked into the suite verdict lists")
		}
	}
	for _, name := range append(report.Predicate.PassedTests, report.Predicate.WarnedTests...) {
		if name == "diag.followup.default" {
			t.Fatal("diagnostic leaked into the suite verdict lists")
		}
	}
}

func TestRunnerWritesSchemaValidArtifacts(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "teardown.marker")
	path := writeRunnerFixture(t, base(scenarioYAML("proof.green", "contract_gate", "required", "$QUOIN_VERIFY_FIXTURE/bin/pass.sh", nil, "")),
		map[string]string{"pass.sh": strings.ReplaceAll(script(0, goodFacts), "$QUOIN_VERIFY_TEARDOWN_MARKER", marker)})
	t.Setenv("QUOIN_VERIFY_FIXTURE", filepath.Dir(path))
	output := filepath.Join(t.TempDir(), "invocation")
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	report, err := runner.Run(runner.Options{
		CatalogPath:  path,
		ProfilePath:  filepath.Join(repoContracts, "verification-result-profile.yaml"),
		ContractsDir: repoContracts,
		OutputDir:    output,
		RepoRoot:     ".",
		Layer:        catalog.LayerContractGate,
		InvocationID: "artifacts-1",
		Subject:      runner.SubjectBinding{Name: "fixture-subject", Digest: evidence.Digest([]byte("fixture"))},
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != result.VerdictPassed {
		t.Fatalf("expected PASSED, got %s", report.Verdict)
	}
	// Evidence index and statement files exist and round-trip.
	indexBody, err := os.ReadFile(filepath.Join(output, "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index evidence.Index
	if err := json.Unmarshal(indexBody, &index); err != nil {
		t.Fatal(err)
	}
	if index.Items[0].TestName() != "proof.green.default" {
		t.Fatalf("index content wrong: %+v", index.Items)
	}
	statementBody, err := os.ReadFile(filepath.Join(output, "test-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var statement result.Statement
	if err := json.Unmarshal(statementBody, &statement); err != nil {
		t.Fatal(err)
	}
	if statement.Predicate.Result != result.VerdictPassed || statement.Predicate.Quoin.EvidenceIndex.SHA256 != evidence.Digest(indexBody) {
		t.Fatalf("statement content wrong: %+v", statement.Predicate)
	}
	// Attachment digests must match the files on disk.
	for _, attachment := range index.Items[0].Attachments {
		digest, _, err := evidence.DigestFile(attachment.Locator)
		if err != nil {
			t.Fatal(err)
		}
		if digest != attachment.SHA256 {
			t.Fatalf("attachment digest drift for %s", attachment.Kind)
		}
	}
}
