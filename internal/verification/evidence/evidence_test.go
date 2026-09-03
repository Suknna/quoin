package evidence_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/verification/evidence"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const evidenceSchema = "../../../docs/specs/quoin-v1/contracts/schemas/verification-evidence.schema.json"

func validIndex() evidence.Index {
	exit := 0
	return evidence.Index{
		ContractVersion:           1,
		InvocationID:              "contract-gate-1",
		Layer:                     "contract_gate",
		SubjectDigest:             evidence.Digest([]byte("subject")),
		VerificationCatalogDigest: evidence.Digest([]byte("catalog")),
		ResultProfileDigest:       evidence.Digest([]byte("profile")),
		GeneratedAt:               "2026-09-01T00:00:00Z",
		RedactionProfile:          evidence.RedactionProfile,
		Items: []evidence.Item{{
			ScenarioID: "contracts.machine", CellID: "default",
			InputDigest:  evidence.Digest([]byte("input")),
			ResultDigest: evidence.Digest([]byte("result")),
			Outcome:      "passed", Category: "passed",
			StartedAt: "2026-09-01T00:00:00Z", FinishedAt: "2026-09-01T00:01:00Z",
			AuthoritativeRecordedAt: "2026-09-01T00:01:00Z",
			AuthoritativeTimeSource: "ci_runner_clock",
			EnvironmentDigest:       evidence.Digest([]byte("env")),
			Environment: evidence.Environment{
				Backend: "contract", Architecture: "not_applicable",
				ToolchainDigest: evidence.Digest([]byte("toolchain")),
				CapabilityIDs:   []string{"environment.contract-harness"},
			},
			ToolVersion:   "quoin-verify/test",
			ArgvSanitized: []string{"ci/verify-contracts", "--phase", "assert"},
			ExitCode:      &exit,
			Assertions: []evidence.Assertion{
				{ID: "command-exit-zero", Kind: "exit_code", Expected: 0, Actual: 0, Result: "passed"},
				{ID: "structured-result-valid", Kind: "schema_valid", Expected: true, Actual: true, Result: "passed"},
			},
			Attachments: []evidence.Attachment{{
				Kind: "stdout", SHA256: evidence.Digest([]byte("log")), SizeBytes: 3,
				MediaType: "text/plain", Locator: "contracts.machine/default/stdout.txt",
				RetentionClass: "generated", Sensitive: false,
			}},
			Cleanup:   evidence.Cleanup{Required: false, Outcome: "not_run", Assertions: []evidence.Assertion{}},
			CausalIDs: []string{}, ProofRefs: []string{},
		}},
	}
}

func validateAgainstEvidenceSchema(t *testing.T, document any) error {
	t.Helper()
	body, err := os.ReadFile(evidenceSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument any
	if err := json.Unmarshal(body, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(evidenceSchema, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(evidenceSchema)
	if err != nil {
		t.Fatal(err)
	}
	return schema.Validate(document)
}

func TestValidIndexPassesFrozenSchema(t *testing.T) {
	index := validIndex()
	var document any
	body, _ := json.Marshal(index)
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if err := validateAgainstEvidenceSchema(t, document); err != nil {
		t.Fatalf("valid evidence index rejected: %v", err)
	}
}

func TestSchemaRejectsCategoryOutcomeMismatch(t *testing.T) {
	index := validIndex()
	index.Items[0].Outcome = "failed"
	index.Items[0].Category = "passed"
	var document any
	body, _ := json.Marshal(index)
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if err := validateAgainstEvidenceSchema(t, document); err == nil {
		t.Fatal("schema accepted outcome/category mismatch")
	}
}

func TestSchemaRejectsGeneratedRetentionForStructuredResult(t *testing.T) {
	index := validIndex()
	index.Items[0].Attachments[0].Kind = "structured_result"
	index.Items[0].Attachments[0].RetentionClass = "generated"
	var document any
	body, _ := json.Marshal(index)
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if err := validateAgainstEvidenceSchema(t, document); err == nil {
		t.Fatal("schema accepted long-term kind with generated retention")
	}
}

func TestAttachmentWriteRecordsDigestAndRetention(t *testing.T) {
	root := t.TempDir()
	attachment, err := evidence.Write(root, "stdout.txt", evidence.AttachmentStdout, "text/plain", []byte("hello"), false)
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err := evidence.DigestFile(filepath.Join(root, "stdout.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if attachment.SHA256 != digest || attachment.SizeBytes != size || size != 5 {
		t.Fatalf("attachment digest/size drift: %+v vs %s/%d", attachment, digest, size)
	}
	if attachment.RetentionClass != evidence.RetentionGenerated {
		t.Fatalf("stdout must be generated retention, got %s", attachment.RetentionClass)
	}
	structured, err := evidence.Write(root, "facts.json", evidence.AttachmentStructuredResult, "application/json", []byte("{}"), false)
	if err != nil {
		t.Fatal(err)
	}
	if structured.RetentionClass != evidence.RetentionLongTerm {
		t.Fatalf("structured_result must be long_term retention, got %s", structured.RetentionClass)
	}
}

func TestCanonicalJSONIsKeyStable(t *testing.T) {
	left := evidence.CanonicalJSON(map[string]any{"b": 1, "a": map[string]any{"z": true, "y": []any{1, 2}}})
	right := evidence.CanonicalJSON(map[string]any{"a": map[string]any{"y": []any{1, 2}, "z": true}, "b": 1})
	if left != right || !strings.HasPrefix(left, "{\"a\"") {
		t.Fatalf("canonical JSON unstable:\n%s\n%s", left, right)
	}
}
