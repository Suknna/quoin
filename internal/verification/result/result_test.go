package result_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Suknna/quoin/internal/verification/evidence"
	"github.com/Suknna/quoin/internal/verification/result"
)

const contractsDir = "../../../docs/specs/quoin-v1/contracts"

func loadProfile(t *testing.T) *result.Profile {
	t.Helper()
	profile, err := result.LoadProfile(filepath.Join(contractsDir, "verification-result-profile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestProfileMapsFrozenCategories(t *testing.T) {
	profile := loadProfile(t)
	cases := map[string]string{
		"passed":                      "passed",
		"not_run":                     "warned",
		"subject_drift":               "warned",
		"cleanup_indeterminate":       "warned",
		"functional_assertion_failed": "failed",
		"cleanup_residue":             "failed",
		"verifier_conflict":           "failed",
	}
	for category, outcome := range cases {
		if got := profile.Outcome(category); got != outcome {
			t.Fatalf("category %s mapped to %s, want %s", category, got, outcome)
		}
	}
	if got := profile.Outcome("category_that_does_not_exist"); got != "failed" {
		t.Fatalf("unknown category must be a failed invariant violation, got %s", got)
	}
}

func passedItem(scenario, cell string) evidence.Item {
	return evidence.Item{ScenarioID: scenario, CellID: cell, Outcome: "passed", Category: "passed"}
}

func TestAggregationFollowsFrozenRules(t *testing.T) {
	profile := loadProfile(t)
	if got := profile.Aggregate([]evidence.Item{passedItem("a", "one"), passedItem("b", "two")}); got != result.VerdictPassed {
		t.Fatalf("all passed must aggregate PASSED, got %s", got)
	}
	warned := passedItem("a", "one")
	warned.Outcome, warned.Category = "warned", "not_run"
	if got := profile.Aggregate([]evidence.Item{passedItem("b", "two"), warned}); got != result.VerdictWarned {
		t.Fatalf("single warned must aggregate WARNED, got %s", got)
	}
	failed := passedItem("a", "one")
	failed.Outcome, failed.Category = "failed", "functional_assertion_failed"
	if got := profile.Aggregate([]evidence.Item{passedItem("b", "two"), warned, failed}); got != result.VerdictFailed {
		t.Fatalf("any failed must aggregate FAILED, got %s", got)
	}
	unknown := passedItem("a", "one")
	unknown.Outcome = "mysterious"
	if got := profile.Aggregate([]evidence.Item{unknown}); got != result.VerdictFailed {
		t.Fatalf("unknown outcome must aggregate FAILED, got %s", got)
	}
}

func TestTimeClosureBoundaries(t *testing.T) {
	profile := loadProfile(t)
	started := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if profile.TimeClosure.ObservationDeadlineSeconds != 8*3600 {
		t.Fatalf("frozen deadline must be eight hours, got %d", profile.TimeClosure.ObservationDeadlineSeconds)
	}
	deadline := profile.Deadline(started)
	if !deadline.Equal(started.Add(8 * time.Hour)) {
		t.Fatalf("deadline arithmetic drifted: %s", deadline)
	}
	// before boundary: still inside the window
	if err := profile.CheckTimeClosure(started, started.Add(time.Hour), started.Add(time.Hour+59*time.Minute)); err != nil {
		t.Fatalf("in-window closure rejected: %v", err)
	}
	// at boundary: snapshot/finalization exactly at the deadline are valid
	if err := profile.CheckTimeClosure(started, deadline, deadline); err != nil {
		t.Fatalf("at-deadline closure rejected: %v", err)
	}
	// after boundary: no late finalization may exist
	if err := profile.CheckTimeClosure(started, deadline, deadline.Add(time.Second)); err == nil {
		t.Fatal("finalization after the deadline accepted")
	}
	if err := profile.CheckTimeClosure(started, deadline.Add(time.Second), deadline.Add(time.Second)); err == nil {
		t.Fatal("snapshot after the deadline accepted")
	}
	if err := profile.CheckTimeClosure(started, started.Add(time.Hour), started.Add(59*time.Minute)); err == nil {
		t.Fatal("finalization before snapshot accepted")
	}
	if err := profile.CheckTimeClosure(started, started.Add(-time.Second), started.Add(time.Second)); err == nil {
		t.Fatal("snapshot before start accepted")
	}
}

func validStatement() result.Statement {
	return result.Statement{
		Type:          result.StatementType,
		PredicateType: result.PredicateType,
		Subject: []result.Subject{{
			Name:   "quoin-source",
			Digest: map[string]string{"sha256": evidence.Digest([]byte("subject"))},
		}},
		Predicate: result.Predicate{
			Result: result.VerdictPassed,
			Configuration: []result.ResourceDescriptor{{
				Name:   "verification-catalog.yaml",
				Digest: map[string]string{"sha256": evidence.Digest([]byte("catalog"))},
			}},
			PassedTests: []string{"contracts.machine.default"},
			WarnedTests: []string{},
			FailedTests: []string{},
			Quoin: result.QuoinExtension{
				ProfileVersion: result.ProfileVersion,
				InvocationID:   "contract-gate-1",
				Layer:          "contract_gate",
				StartedAt:      "2026-09-01T00:00:00Z",
				FinishedAt:     "2026-09-01T00:10:00Z",
				Environment:    result.EnvironmentMatrix{MatrixDigest: evidence.Digest([]byte("matrix")), CellCount: 1},
				EvidenceIndex: result.EvidenceIndexRef{
					SHA256: evidence.Digest([]byte("index")), MediaType: "application/vnd.quoin.verification-evidence.v1+json", SizeBytes: 42,
				},
				ObservationSummary: result.ObservationSummary{RequiredCells: 1, PassedCells: 1},
			},
		},
	}
}

func validateStatement(t *testing.T, statement result.Statement) error {
	t.Helper()
	schemaPath := filepath.Join(contractsDir, "schemas", "verification-result.schema.json")
	body, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument any
	if err := json.Unmarshal(body, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaPath, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	return schema.Validate(document)
}

func TestValidStatementPassesFrozenSchema(t *testing.T) {
	if err := validateStatement(t, validStatement()); err != nil {
		t.Fatalf("valid statement rejected: %v", err)
	}
}

func TestSchemaRejectsVerdictOutsideVocabulary(t *testing.T) {
	statement := validStatement()
	statement.Predicate.Result = "MOSTLY_FINE"
	if err := validateStatement(t, statement); err == nil {
		t.Fatal("schema accepted unknown verdict")
	}
}

func TestSchemaRejectsDeploymentAcceptanceWithoutClosure(t *testing.T) {
	statement := validStatement()
	statement.Predicate.Quoin.Layer = "deployment_acceptance"
	if err := validateStatement(t, statement); err == nil {
		t.Fatal("schema accepted deployment_acceptance without closure block")
	}
}

func TestMatrixDigestCoversEveryCell(t *testing.T) {
	items := []evidence.Item{
		{ScenarioID: "fault.time", CellID: "reconnect-grace", Environment: evidence.Environment{Backend: "process", Architecture: "not_applicable", ToolchainDigest: "a", CapabilityIDs: []string{"environment.process-harness"}}},
		{ScenarioID: "contracts.machine", CellID: "default", Environment: evidence.Environment{Backend: "contract", Architecture: "not_applicable", ToolchainDigest: "a", CapabilityIDs: []string{"environment.contract-harness"}}},
	}
	matrix := result.EnvironmentMatrixDigest(items)
	if matrix.CellCount != 2 {
		t.Fatalf("cell count must reflect the full matrix, got %d", matrix.CellCount)
	}
	reordered := []evidence.Item{items[1], items[0]}
	if other := result.EnvironmentMatrixDigest(reordered); other.MatrixDigest != matrix.MatrixDigest {
		t.Fatal("matrix digest must be order-stable")
	}
	changed := items[0]
	changed.Environment.CapabilityIDs = []string{"environment.process-harness", "fault.protocol-fixture"}
	if other := result.EnvironmentMatrixDigest([]evidence.Item{changed, items[1]}); other.MatrixDigest == matrix.MatrixDigest {
		t.Fatal("matrix digest must change with capability sets")
	}
}
