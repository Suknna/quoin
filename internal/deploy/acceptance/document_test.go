package acceptance

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const fixturePath = "../../../docs/specs/quoin-v1/contracts/examples/deployment-verification-request.json"

func TestValidateRequestAcceptsFrozenExample(t *testing.T) {
	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequest(body); err != nil {
		t.Fatalf("frozen helper request must validate: %v", err)
	}
}

func TestValidateReportAcceptsSyntheticPassedAndNotRunItems(t *testing.T) {
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	report := Report{
		SchemaVersion: 1, DocumentType: "helper_report", InvocationID: "42",
		ManifestDigest: digest, ItemSetDigest: digest, ReleaseSubjectDigest: digest,
		CatalogDigest: digest, ResultProfileDigest: digest, DeploymentConfigDigest: digest,
		PublicOriginDigest: digest, Backend: "compose", Architecture: "linux/amd64",
		HelperRequestDigest: digest, StartedAt: "2026-08-11T00:01:00Z", FinishedAt: "2026-08-11T00:02:00Z",
		Items: []ReportItem{
			{ItemID: "101", ScenarioID: "deployment.verify-only", CellID: "compose-linux-amd64", InputDigest: digest, ResultDigest: digest,
				Outcome: "passed", Category: "passed", StartedAt: "2026-08-11T00:01:00Z", FinishedAt: "2026-08-11T00:02:00Z",
				ArgvSanitized: []string{"quoin-deploy", "compose", "verify"}, ExitCode: 0,
				Assertions:  []Assertion{{ID: "verification", Expected: true, Actual: true, Result: "passed"}},
				Attachments: []Attachment{{Kind: "log", SHA256: digest, SizeBytes: 1, MediaType: "text/plain"}}, CleanupOutcome: "clean"},
			{ItemID: "102", ScenarioID: "connection.probe", CellID: "compose-linux-amd64", InputDigest: digest, ResultDigest: digest,
				Outcome: "warned", Category: "not_run", StartedAt: "2026-08-11T00:01:00Z", FinishedAt: "2026-08-11T00:02:00Z",
				ArgvSanitized: []string{"quoin-deploy", "compose", "verify"}, ExitCode: 0, Assertions: []Assertion{},
				Attachments: []Attachment{{Kind: "log", SHA256: digest, SizeBytes: 1, MediaType: "text/plain"}}, CleanupOutcome: "not_run"},
		},
	}
	path := filepath.Join(t.TempDir(), "helper-report.yaml")
	body, err := WriteReport(path, report)
	if err != nil {
		t.Fatalf("synthetic helper report must validate and write: %v", err)
	}
	if err := ValidateReport(body); err != nil {
		t.Fatalf("written report must validate: %v", err)
	}
}

func TestRunWritesClosedOverReportAndInvokesGenericVerifierOnce(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "deployment.yaml")
	if err := os.WriteFile(configPath, []byte("document: compose-install\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDigest, err := digestFileForTest(configPath)
	if err != nil {
		t.Fatal(err)
	}
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	request := Request{
		SchemaVersion: 1, DocumentType: "helper_request", InvocationID: "42", ManifestDigest: digest, ItemSetDigest: digest,
		ReleaseSubjectDigest: digest, CatalogDigest: digest, ResultProfileDigest: digest, DeploymentConfigDigest: configDigest,
		PublicOriginDigest: digest, Backend: "compose", Architecture: runtime.GOOS + "/" + runtime.GOARCH,
		GeneratedAt: "2026-08-11T00:00:00Z", DeadlineAt: "2026-08-11T08:00:00Z",
		Items: []RequestItem{{ItemID: "101", ScenarioID: "deployment.verify-only", CellID: "compose-linux-amd64", InputDigest: digest,
			TypedLocator: map[string]any{"kind": "deployment", "releaseSubjectDigest": digest, "deploymentConfigDigest": configDigest, "publicOriginDigest": digest, "backend": "compose", "architecture": runtime.GOOS + "/" + runtime.GOARCH}}},
	}
	requestBody, err := yaml.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(directory, "request.yaml")
	if err := os.WriteFile(requestPath, requestBody, 0o600); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(directory, "report.yaml")
	calls := 0
	exit := Run(RunRequest{Backend: "compose", ConfigPath: configPath, HelperRequestPath: requestPath, ReportPath: reportPath,
		Verify: func(genericPath string) int {
			calls++
			if err := os.WriteFile(genericPath, []byte(`{"checks":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return ExitSuccess
		}})
	if exit != ExitSuccess || calls != 1 {
		t.Fatalf("Run exit=%d verifier calls=%d, want 0 and 1", exit, calls)
	}
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateReport(body); err != nil {
		t.Fatalf("helper report must validate: %v", err)
	}
	var report Report
	if err := yaml.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if report.HelperRequestDigest != sha256Hex(requestBody) || report.Items[0].Outcome != "passed" {
		t.Fatalf("report did not close over request and verifier outcome: %+v", report)
	}
}

func TestHelperRequestDigestStableForExactBytes(t *testing.T) {
	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	first, second := sha256Hex(body), sha256Hex(append([]byte(nil), body...))
	if first != second || len(first) != 64 {
		t.Fatalf("exact request bytes must produce stable SHA-256: %q != %q", first, second)
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("digest must not be empty")
	}
}

func digestFileForTest(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return sha256Hex(body), nil
}
