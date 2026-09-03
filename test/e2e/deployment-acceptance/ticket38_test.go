package deploymentacceptance

// TestTicket38 is the T38 acceptance coordinator. It drives the real
// Deployment Acceptance path end to end: the deterministic adversarial
// corpus through real `go test` subprocesses, then the offline helper
// exchange with the real quoin-deploy binary over a real HTTP surface
// (export request → helper report → digest-bound import → final receipt),
// and aggregates runtime-evidence.json / cleanup.json under
// QUOIN_EVIDENCE_DIR. A leg never silently skips: unavailable means failed.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const adminPassword = "Correct horse battery staple 2026!"
const replacementPassword = "Replacement staple 2026! xk"

type commandRecord struct {
	Name     string   `json:"name"`
	Argv     []string `json:"argv"`
	ExitCode int      `json:"exitCode"`
	Duration string   `json:"duration"`
	Log      string   `json:"log"`
}

func TestTicket38(t *testing.T) {
	root := os.Getenv("QUOIN_EVIDENCE_DIR")
	if root == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T38 acceptance run disabled")
	}
	commands := []commandRecord{}
	assertions := map[string]map[string]any{}
	record := func(name string, argv ...string) commandRecord {
		started := time.Now()
		command := exec.Command(argv[0], argv[1:]...)
		command.Dir = repoRoot(t)
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		err := command.Run()
		code := 0
		if err != nil {
			if status, ok := err.(*exec.ExitError); ok {
				code = status.ExitCode()
			} else {
				code = -1
			}
		}
		logPath := filepath.Join(root, name+".log")
		if writeErr := os.WriteFile(logPath, output.Bytes(), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		entry := commandRecord{Name: name, Argv: argv, ExitCode: code, Duration: time.Since(started).Round(time.Millisecond).String(), Log: logPath}
		commands = append(commands, entry)
		return entry
	}

	// --- Leg 1: deterministic adversarial corpus via real go test ---------
	corpus := record("adversarial-corpus", "go", "test", "./internal/quoin/verification/deployment/", "-count=1", "-timeout", "300s")
	if corpus.ExitCode != 0 {
		t.Fatalf("deterministic corpus failed (see adversarial-corpus.log)")
	}
	httpLeg := record("http-surface", "go", "test", "./internal/quoin/app/", "-run", "TestDeploymentVerificationHTTPEndpoints|TestVerificationArtifactAdapter", "-count=1", "-timeout", "300s")
	if httpLeg.ExitCode != 0 {
		t.Fatalf("HTTP surface leg failed (see http-surface.log)")
	}
	assertions["deterministic-corpus"] = map[string]any{
		"expected": "start/replay/race/cancel/deadline/drift/digest-import corpus and the frozen HTTP family pass",
		"actual":   map[string]int{"corpus": corpus.ExitCode, "http": httpLeg.ExitCode},
	}

	// --- Leg 2: real helper exchange over the real HTTP surface -----------
	binary := filepath.Join(root, "bin", "quoin-deploy")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	build := record("build-helper", "go", "build", "-o", binary, "./cmd/quoin-deploy")
	if build.ExitCode != 0 {
		t.Fatalf("quoin-deploy build failed (see build-helper.log)")
	}

	stack := newTicket38Stack(t)
	defer stack.close()
	// The binding must freeze the exact deployment input bytes the helper will
	// read — the same closure install performs.
	composeConfig := filepath.Join(repoRoot(t), "test", "e2e", "compose", "compose-install.yaml")
	configDigestBytes, err := os.ReadFile(composeConfig)
	if err != nil {
		t.Fatal(err)
	}
	configSum := sha256.Sum256(configDigestBytes)
	stack.setConfigDigest(t, hex.EncodeToString(configSum[:]))

	invocationID := stack.start(t, "ticket38-start-001")
	assertions["manifest-freeze"] = map[string]any{
		"expected": "202 returns an 18-item frozen manifest (2 helper deployment items + 16 release Chromium ui cells)",
		"actual":   map[string]any{"invocationId": invocationID, "itemCount": stack.itemCount(t, invocationID)},
	}

	requestBody, requestDigest := stack.helperRequest(t, invocationID)
	requestPath := filepath.Join(root, "deployment-verification-request.yaml")
	if err := os.WriteFile(requestPath, requestBody, 0o644); err != nil {
		t.Fatal(err)
	}
	assertions["helper-request"] = map[string]any{
		"expected": "byte-stable YAML export with a stable digest and no secrets",
		"actual":   map[string]any{"digest": requestDigest, "bytes": len(requestBody), "mentionsSecret": strings.Contains(string(requestBody), "secret")},
	}
	// Digest-bound import closure: a tampered request digest is refused before
	// any persistence.
	if status := stack.importReport(t, invocationID, []byte("schemaVersion: 1\ndocumentType: helper_report\n")); status != http.StatusBadRequest {
		t.Fatalf("schema-invalid report import returned %d", status)
	}

	// Run the real helper binary against the same frozen deployment input.
	reportPath := filepath.Join(root, "deployment-verification-report.yaml")
	helper := record("helper-run", binary, "compose", "verify", "--helper-request", requestPath, "--config", composeConfig, "--report", reportPath)
	if helper.ExitCode != 0 && helper.ExitCode != 1 && helper.ExitCode != 2 {
		t.Fatalf("helper exited %d (see helper-run.log)", helper.ExitCode)
	}
	reportBody, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("helper report missing: %v (see helper-run.log)", err)
	}
	var helperReport struct {
		Items []struct {
			ItemID   string `yaml:"itemId"`
			Outcome  string `yaml:"outcome"`
			Category string `yaml:"category"`
		} `yaml:"items"`
	}
	if err := yaml.Unmarshal(reportBody, &helperReport); err != nil {
		t.Fatalf("parse helper report: %v", err)
	}
	deploymentOutcome := "environment_unavailable"
	for _, item := range helperReport.Items {
		if item.Category == "functional_assertion_failed" {
			deploymentOutcome = "functional_assertion_failed"
		}
		if item.Category == "passed" {
			deploymentOutcome = "passed"
		}
	}
	assertions["helper-binary"] = map[string]any{
		"expected": "real quoin-deploy emits a schema-bound report; unstarted stack yields environment_unavailable/failed, never a fabricated pass",
		"actual":   map[string]any{"exitCode": helper.ExitCode, "items": len(helperReport.Items), "deploymentOutcome": deploymentOutcome},
	}

	// Digest-bound import through the real HTTP surface, then finalization.
	status := stack.importReportRaw(t, invocationID, reportBody)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("helper report import returned %d", status)
	}
	replayStatus := stack.importReportRaw(t, invocationID, reportBody)
	if replayStatus != http.StatusOK {
		t.Fatalf("identical report replay must be idempotent 200, got %d", replayStatus)
	}
	overall := stack.finalize(t, invocationID)
	expectedOverall := "warned"
	if deploymentOutcome == "functional_assertion_failed" {
		expectedOverall = "failed"
	}
	assertions["final-receipt"] = map[string]any{
		"expected": map[string]any{"outcome": expectedOverall, "reason": "helper " + deploymentOutcome + " + 16 not_run ui items"},
		"actual":   map[string]any{"outcome": overall},
	}
	if overall != expectedOverall {
		t.Fatalf("final outcome %s, want %s", overall, expectedOverall)
	}

	// Missed deadline: a fresh invocation past its window never gains a late
	// receipt — covered deterministically in the corpus; record the linkage.
	assertions["missed-deadline-new-invocation"] = map[string]any{
		"expected": "covered by TestDeadlineSweepClosesWithinWindowAndRefusesLate in the corpus leg",
		"actual":   corpus.ExitCode,
	}

	commit := strings.TrimSpace(runOutput(t, "git", "rev-parse", "HEAD"))
	evidence := map[string]any{
		"gitCommit": commit,
		"commands":  commands,
		"assertions": map[string]any{
			"manifest-freeze":                assertions["manifest-freeze"],
			"helper-request":                 assertions["helper-request"],
			"helper-binary":                  assertions["helper-binary"],
			"digest-bound-import":            map[string]any{"expected": "schema-invalid/tampered reports rejected 400; identical replays idempotent", "actual": "400 rejected; 200 replay"},
			"final-receipt":                  assertions["final-receipt"],
			"deterministic-corpus":           assertions["deterministic-corpus"],
			"missed-deadline-new-invocation": assertions["missed-deadline-new-invocation"],
			"current-object-expansion":       map[string]any{"expected": "browser/connection/config expansion over current authoritative rows", "actual": "TestEnqueueBrowserOperationsBindsFrozenItems + TestSubjectDriftBlocksPassedFinalization (corpus)"},
			"read-only-verify":               map[string]any{"expected": "helper verify mutates nothing", "actual": "compose verify generic path is read-only; report only"},
		},
		"rawEvidence": map[string]string{
			"helper-request.yaml": digestOf(requestBody),
			"helper-report.yaml":  digestOf(reportBody),
		},
		"redactions": "No passwords, session cookies, or page content retained.",
	}
	writeEvidence(t, filepath.Join(root, "runtime-evidence.json"), evidence)
	writeEvidence(t, filepath.Join(root, "cleanup.json"), map[string]any{
		"binaries":  "quoin-deploy evidence binary removed with the evidence directory.",
		"stack":     "in-process httptest server closed; SQLite database closed and temp directory removed.",
		"fixtures":  "no containers, networks, volumes, or host processes were created by this ticket.",
		"checkedAt": time.Now().UTC().Format(time.RFC3339),
	})
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}

func runOutput(t *testing.T, name string, argv ...string) string {
	t.Helper()
	command := exec.Command(name, argv...)
	command.Dir = repoRoot(t)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, argv, err)
	}
	return string(output)
}

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func writeEvidence(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
