package catalog_test

// The catalog authority tests drive the frozen machine contract end to end:
// every negative class the build gate must reject (VERIFY-COVERAGE-002,
// VERIFY-CATALOG-002/003) is constructed as a mutated catalog document,
// schema-validated against the real frozen JSON Schema, and then checked
// against the cross-contract validator.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/verification/catalog"
)

const repoContracts = "../../../docs/specs/quoin-v1/contracts"

// validFixture is a minimal but schema-valid contract-gate catalog with a
// dependency edge and two validation roots.
func validFixture() string {
	return `contract_version: 1
catalog_id: quoin-v1-verification
catalog_state: frozen
result_profile_id: quoin-test-result-v1
contract_refs:
  connection_probes_sha256: ` + dummyDigest + `
  result_profile_sha256: ` + dummyDigest + `
capability_definitions:
  - id: environment.process-harness
    kind: environment
validation_roots:
  - id: ARCH-VALIDATION-001
    source: docs/specs/quoin-v1/architecture.md
  - id: HTTP-VALIDATION-001
    source: docs/specs/quoin-v1/http-api.md
environments:
  - id: process-harness
    kind: process_harness
    native: false
    requires_release_artifacts: false
scenarios:
  - id: alpha.base
    title: Base scenario
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
    status: active
    layer: contract_gate
    requirement: required
    executor: {kind: ci, entrypoint: ci/verify-alpha}
    phases:
      setup: ci/verify-alpha --phase setup
      action: ci/verify-alpha --phase action
      assert: ci/verify-alpha --phase assert
      teardown: no-op
    depends_on: []
    proof_refs: []
    required_capabilities: []
    evidence:
      test_names: [alpha.base.default]
      attachments: [structured_result, stdout, stderr]
      redaction_profile: verification-redaction-v1
    cleanup: {required: false, entrypoint: none, success_assertions: []}
    timeout_seconds: 300
  - id: alpha.dependent
    title: Dependent scenario
    validation_roots: [HTTP-VALIDATION-001]
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
    status: active
    layer: contract_gate
    requirement: required
    executor: {kind: ci, entrypoint: ci/verify-beta}
    phases:
      setup: ci/verify-beta --phase setup
      action: ci/verify-beta --phase action
      assert: ci/verify-beta --phase assert
      teardown: no-op
    depends_on: [alpha.base]
    proof_refs: []
    required_capabilities: []
    evidence:
      test_names: [alpha.dependent.default]
      attachments: [structured_result, stdout, stderr]
      redaction_profile: verification-redaction-v1
    cleanup: {required: false, entrypoint: none, success_assertions: []}
    timeout_seconds: 300
`
}

const dummyDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func writeCatalog(t *testing.T, body string) string {
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
	path := filepath.Join(dir, "verification-catalog.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadFixture(t *testing.T, body string) *catalog.Catalog {
	t.Helper()
	loaded, err := catalog.LoadAndValidate(writeCatalog(t, body))
	if err != nil {
		t.Fatalf("fixture catalog rejected: %v", err)
	}
	return loaded
}

func TestRealCatalogPassesFrozenSchemaAndCrossChecks(t *testing.T) {
	path := filepath.Join(repoContracts, "verification-catalog.yaml")
	if _, err := catalog.LoadAndValidate(path); err != nil {
		t.Fatalf("frozen catalog rejected: %v", err)
	}
}

func TestSchemaRejectsMalformedCatalog(t *testing.T) {
	broken := strings.Replace(validFixture(), "catalog_state: frozen", "catalog_state: wishful", 1)
	_, err := catalog.LoadAndValidate(writeCatalog(t, broken))
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("schema violation not reported as such: %v", err)
	}
}

func TestCrossContractNegatives(t *testing.T) {
	cases := []struct {
		name string
		mut  func(string) string
		code string
	}{
		{"duplicate scenario id", func(b string) string {
			return b + strings.Replace(scenarioBlock(b, "alpha.dependent"), "alpha.dependent", "alpha.base", -1)
		}, "duplicate_scenario_id"},
		{"dangling dependency", func(b string) string {
			return strings.Replace(b, "depends_on: [alpha.base]", "depends_on: [alpha.ghost]", 1)
		}, "dangling_dependency"},
		{"self dependency", func(b string) string {
			return strings.Replace(b, "depends_on: [alpha.base]", "depends_on: [alpha.dependent]", 1)
		}, "self_dependency"},
		{"dependency cycle", func(b string) string {
			return strings.Replace(b, "depends_on: []", "depends_on: [alpha.dependent]", 1)
		}, "dependency_cycle"},
		{"cross layer dependency", func(b string) string {
			changed := strings.Replace(b, "    layer: contract_gate", "    layer: release_qualification", 1)
			return strings.Replace(changed, "depends_on: [alpha.base]", "depends_on: [alpha.base]", 1)
		}, "cross_layer_dependency"},
		{"dangling proof ref", func(b string) string {
			return strings.Replace(b, "    proof_refs: []", "    proof_refs: [alpha.ghost]", 1)
		}, "dangling_proof_ref"},
		{"same layer proof ref", func(b string) string {
			return strings.Replace(b, "depends_on: [alpha.base]\n    proof_refs: []", "depends_on: []\n    proof_refs: [alpha.base]", 1)
		}, "proof_ref_not_lower_layer"},
		{"dangling validation root", func(b string) string {
			return strings.Replace(b, "validation_roots: [HTTP-VALIDATION-001]", "validation_roots: [GHOST-ROOT-001]", 1)
		}, "dangling_validation_root"},
		{"uncovered validation root", func(b string) string {
			return strings.Replace(b, "  - id: ARCH-VALIDATION-001", "  - id: SEC-VALIDATION-009\n    source: docs/specs/quoin-v1/security.md\n  - id: ARCH-VALIDATION-001", 1)
		}, "uncovered_validation_root"},
		{"unknown capability", func(b string) string {
			return strings.Replace(b, "required_capabilities: [environment.process-harness]", "required_capabilities: [environment.ghost]", 1)
		}, "unknown_capability"},
		{"unknown environment", func(b string) string {
			return strings.Replace(b, "environment_id: process-harness", "environment_id: ghost-harness", 1)
		}, "unknown_environment"},
		{"applicability not closed", func(b string) string {
			// The dependent runs unconditionally (always) while its
			// dependency is gated to a deployment target that a contract
			// gate invocation never has: the prerequisite can be absent.
			return strings.Replace(b, "applicability: {mode: always}", "applicability: {mode: deployment_target, backend: compose, architecture: linux/amd64}", 1)
		}, "applicability_not_closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := catalog.LoadAndValidate(writeCatalog(t, tc.mut(validFixture())))
			if err == nil {
				t.Fatalf("mutation accepted")
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("expected violation %q, got: %v", tc.code, err)
			}
		})
	}
}

// scenarioBlock extracts the alpha.dependent scenario body from a fixture.
func scenarioBlock(body, id string) string {
	start := strings.Index(body, "  - id: "+id)
	if start < 0 {
		return ""
	}
	return body[start:]
}

func TestApplicableCellsResolveAlwaysWithoutTarget(t *testing.T) {
	loaded := loadFixture(t, validFixture())
	base := loaded.Scenario("alpha.base")
	cells := catalog.ApplicableCells(base, nil)
	if len(cells) != 1 || cells[0].ID != "default" {
		t.Fatalf("always cell not applicable without target: %+v", cells)
	}
	dependent := loaded.Scenario("alpha.dependent")
	if got := catalog.ExecutionOrder(loaded, catalog.LayerContractGate); len(got) != 2 || got[0] != "alpha.base" || got[1] != "alpha.dependent" {
		t.Fatalf("execution order wrong: %+v", got)
	}
	_ = dependent
}

func TestDeploymentTargetCellsNeedTarget(t *testing.T) {
	body := strings.Replace(validFixture(),
		"applicability: {mode: always}",
		"applicability: {mode: deployment_target, backend: compose, architecture: linux/amd64}", 1)
	// The dependent would no longer be applicability-covered by the gated
	// dependency, so drop the edge: this test exercises cell resolution only.
	body = strings.Replace(body, "depends_on: [alpha.base]", "depends_on: []", 1)
	loaded := loadFixture(t, body)
	cells := catalog.ApplicableCells(loaded.Scenario("alpha.base"), nil)
	if len(cells) != 0 {
		t.Fatalf("deployment_target cell applicable without target: %+v", cells)
	}
	cells = catalog.ApplicableCells(loaded.Scenario("alpha.base"), &catalog.Target{Backend: "compose", Architecture: "linux/amd64"})
	if len(cells) != 1 {
		t.Fatalf("deployment_target cell not applicable on matching target: %+v", cells)
	}
	cells = catalog.ApplicableCells(loaded.Scenario("alpha.base"), &catalog.Target{Backend: "kubernetes", Architecture: "linux/amd64"})
	if len(cells) != 0 {
		t.Fatalf("deployment_target cell applicable on non-matching target: %+v", cells)
	}
}

func TestRetiredScenariosStayOutOfExecution(t *testing.T) {
	body := strings.Replace(validFixture(), "    status: active\n    layer: contract_gate\n    requirement: required\n    executor: {kind: ci, entrypoint: ci/verify-alpha}",
		"    status: retired\n    successor: alpha.dependent\n    retirement_reason: superseded\n    layer: contract_gate\n    requirement: required\n    executor: {kind: ci, entrypoint: ci/verify-alpha}", 1)
	// The retired scenario still owns its validation root so the catalog
	// stays covered; retired scenarios are skipped by execution only.
	body = strings.Replace(body, "validation_roots: [HTTP-VALIDATION-001]", "validation_roots: [ARCH-VALIDATION-001, HTTP-VALIDATION-001]", 1)
	loaded, err := catalog.LoadAndValidate(writeCatalog(t, body))
	if err != nil {
		t.Fatalf("retired catalog rejected: %v", err)
	}
	if got := catalog.ExecutionOrder(loaded, catalog.LayerContractGate); len(got) != 1 || got[0] != "alpha.dependent" {
		t.Fatalf("retired scenario scheduled: %+v", got)
	}
}

func TestDiagnosticScenariosRequireTriggerCategories(t *testing.T) {
	body := strings.Replace(validFixture(), "    requirement: required\n    executor: {kind: ci, entrypoint: ci/verify-beta}", "    requirement: diagnostic\n    diagnostic_trigger_categories: [failed]\n    executor: {kind: ci, entrypoint: ci/verify-beta}", 1)
	// Diagnostics do not cover roots, so the remaining required scenario
	// (alpha.base) must own both roots.
	body = strings.Replace(body, "validation_roots: [ARCH-VALIDATION-001]", "validation_roots: [ARCH-VALIDATION-001, HTTP-VALIDATION-001]", 1)
	loaded, err := catalog.LoadAndValidate(writeCatalog(t, body))
	if err != nil {
		t.Fatalf("diagnostic catalog rejected: %v", err)
	}
	scenario := loaded.Scenario("alpha.dependent")
	if scenario.Requirement != "diagnostic" || len(scenario.DiagnosticTriggerCategories) != 1 {
		t.Fatalf("diagnostic metadata lost: %+v", scenario)
	}
}
