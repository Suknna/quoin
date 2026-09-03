package verification

// T37 fixture builders: mutated catalogs for the build-gate negatives and
// executable causality/timeout fixtures driven through the real
// quoin-verify binary. Fixture entrypoints are absolute script paths baked
// into the fixture catalog, so the runner executes them exactly like the
// frozen ci/verify-* entrypoints.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTicketFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o700); err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join(repoContracts, "schemas", "verification-catalog.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schemas", "verification-catalog.schema.json"), schema, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schemas", "verification-result.schema.json"),
		mustRead(t, filepath.Join(repoContracts, "schemas", "verification-result.schema.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schemas", "verification-evidence.schema.json"),
		mustRead(t, filepath.Join(repoContracts, "schemas", "verification-evidence.schema.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verification-result-profile.yaml"),
		mustRead(t, filepath.Join(repoContracts, "verification-result-profile.yaml")), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "verification-catalog.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func t37FixtureCatalog() string {
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
` + t37Scenario("t37.base", "[]") + t37Scenario("t37.dependent", "[t37.base]")
}

func t37Scenario(id, depends string) string {
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
    layer: contract_gate
    requirement: required
    executor: {kind: ci, entrypoint: fixture}
    phases:
      setup: /bin/true --phase setup
      action: /bin/true --phase action
      assert: /bin/true --phase assert
      teardown: no-op
    depends_on: ` + depends + `
    proof_refs: []
    required_capabilities: []
    evidence:
      test_names: [` + id + `.default]
      attachments: [structured_result, stdout, stderr]
      redaction_profile: verification-redaction-v1
    cleanup: {required: false, entrypoint: none, success_assertions: []}
    timeout_seconds: 60
`
}

// writeCausalityFixture builds a runnable catalog: one scenario fails its
// assert phase, one independent scenario passes, one depends on the failing
// scenario, and the failed scenario's teardown writes a marker.
func writeCausalityFixture(t *testing.T) string {
	t.Helper()
	return writeRunnableFixture(t, map[string]string{
		"t37.failing":     failScript,
		"t37.independent": passScript,
		"t37.dependent":   passScript,
	}, "t37.dependent: [t37.failing]", 60)
}

func writeTimeoutFixture(t *testing.T) string {
	t.Helper()
	return writeRunnableFixture(t, map[string]string{
		"t37.hang":  hangScript,
		"t37.other": passScript,
	}, "", 1)
}

func writeRunnableFixture(t *testing.T, scripts map[string]string, dependent string, timeout int) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoints := map[string]string{}
	for scenario, body := range scripts {
		path := filepath.Join(bin, scenario+".sh")
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		entrypoints[scenario] = path
	}
	marker := filepath.Join(dir, "teardown.marker")
	scenarioIDs := make([]string, 0, len(scripts))
	for scenario := range scripts {
		scenarioIDs = append(scenarioIDs, scenario)
	}
	// Deterministic order for the YAML body.
	for i := 0; i < len(scenarioIDs); i++ {
		for j := i + 1; j < len(scenarioIDs); j++ {
			if scenarioIDs[j] < scenarioIDs[i] {
				scenarioIDs[i], scenarioIDs[j] = scenarioIDs[j], scenarioIDs[i]
			}
		}
	}
	var builder strings.Builder
	builder.WriteString(fixtureHeader())
	for _, scenario := range scenarioIDs {
		depends := "[]"
		if dependent != "" && strings.HasPrefix(dependent, scenario+":") {
			depends = strings.TrimSpace(strings.TrimPrefix(dependent, scenario+":"))
		}
		entry := entrypoints[scenario]
		builder.WriteString(fixtureScenario(scenario, depends, entry, marker, timeout))
	}
	path := filepath.Join(dir, "verification-catalog.yaml")
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	copyContractSupport(t, dir)
	return dir
}

const passScript = `#!/bin/sh
case "$QUOIN_VERIFY_PHASE" in
  assert) printf '%s' '{"schema_kind":"quoin-verify-facts-v1","assertions":{"ok-state":{"actual":true}},"checks":[{"name":"fixture","result":"passed"}]}' > "$QUOIN_VERIFY_FACTS"; exit 0 ;;
  teardown) exit 0 ;;
  *) exit 0 ;;
esac
`

// failScript fails only in assert and records its teardown run.
const failScript = `#!/bin/sh
case "$QUOIN_VERIFY_PHASE" in
  assert) printf '%s' '{"schema_kind":"quoin-verify-facts-v1","assertions":{"ok-state":{"actual":true}},"checks":[{"name":"fixture","result":"passed"}]}' > "$QUOIN_VERIFY_FACTS"; exit 3 ;;
  teardown) echo "teardown:$QUOIN_VERIFY_SCENARIO" >> "$3"; exit 0 ;;
  *) exit 0 ;;
esac
`

// hangScript sleeps past the scenario timeout in every phase.
const hangScript = `#!/bin/sh
sleep 30
exit 0
`

func fixtureHeader() string {
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
`
}

func fixtureScenario(id, depends, entry, marker string, timeout int) string {
	teardown := "no-op"
	if id == "t37.failing" {
		teardown = entry + " --phase teardown " + marker
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
    layer: contract_gate
    requirement: required
    executor: {kind: ci, entrypoint: fixture}
    phases:
      setup: ` + entry + ` --phase setup
      action: ` + entry + ` --phase action
      assert: ` + entry + ` --phase assert
      teardown: ` + teardown + `
    depends_on: ` + depends + `
    proof_refs: []
    required_capabilities: []
    evidence:
      test_names: [` + id + `.default]
      attachments: [structured_result, stdout, stderr]
      redaction_profile: verification-redaction-v1
    cleanup: {required: false, entrypoint: none, success_assertions: []}
    timeout_seconds: ` + itoa(timeout) + `
`
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func copyContractSupport(t *testing.T, dir string) {
	t.Helper()
	schemas := filepath.Join(dir, "schemas")
	if err := os.MkdirAll(schemas, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"verification-catalog.schema.json", "verification-result.schema.json", "verification-evidence.schema.json"} {
		if err := os.WriteFile(filepath.Join(schemas, name), mustRead(t, filepath.Join(repoContracts, "schemas", name)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "verification-result-profile.yaml"), mustRead(t, filepath.Join(repoContracts, "verification-result-profile.yaml")), 0o600); err != nil {
		t.Fatal(err)
	}
}
