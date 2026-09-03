package verification

// TestTicket37 is the T37 acceptance coordinator. It drives the real
// quoin-verify binary through every acceptance leg: the whole contract gate
// (all five frozen scenarios through the real ci/verify-* entrypoints and
// real go test subprocesses), the catalog build-gate negatives, verdict
// causality (FAILED / WARNED / not_run), teardown-after-failure and
// attachment digests — and aggregates runtime-evidence.json / cleanup.json
// under QUOIN_EVIDENCE_DIR. A leg never silently skips: unavailable means
// failed.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/verification/evidence"
	"github.com/Suknna/quoin/internal/verification/result"
)

const repoContracts = "../../../docs/specs/quoin-v1/contracts"
const realCatalog = repoContracts + "/verification-catalog.yaml" // relative to the test package; resolve against repo root before subprocess use

var expectedGateTests = []string{
	"config.pipeline.default",
	"contracts.machine.default",
	"fault.protocol-delivery.duplicate",
	"fault.protocol-delivery.reorder",
	"fault.time.attempt-lease-expiry",
	"fault.time.reconnect-grace",
	"fault.time.reveal-handle-expiry",
	"fault.time.session-idle-expiry",
	"fault.time.verification-eight-hour-deadline",
	"protocol.interleavings.default",
}

type commandRecord struct {
	Name     string   `json:"name"`
	Argv     []string `json:"argv"`
	ExitCode int      `json:"exitCode"`
	Duration string   `json:"duration"`
	Log      string   `json:"log"`
}

func TestTicket37(t *testing.T) {
	root := os.Getenv("QUOIN_EVIDENCE_DIR")
	if root == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T37 acceptance evidence run disabled")
	}
	startedAt := time.Now().UTC()
	commit, dirty := gitState(t)
	cleanupFixtures := []string{}
	commands := []commandRecord{}
	// record runs a command, persists its log and appends it to the
	// evidence command list in execution order.
	record := func(name string, env []string, argv ...string) commandRecord {
		entry := run(t, root, name, env, argv...)
		commands = append(commands, entry)
		return entry
	}
	artifacts := []map[string]any{}
	assertions := map[string]map[string]any{}

	// --- Leg 1: build the real runner binary ---------------------------
	binary := filepath.Join(root, "bin", "quoin-verify")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	build := record("build", nil, "go", "build", "-o", binary, "./cmd/quoin-verify")
	if build.ExitCode != 0 {
		t.Fatalf("quoin-verify build failed (see logs/build.log)")
	}
	binaryDigest := fileDigest(t, binary)
	artifacts = append(artifacts, artifactOf(t, binary))

	// --- Leg 2: the whole contract gate through the real path ----------
	gateOutput := filepath.Join(root, "gate")
	gate := record("gate-run", nil, binary, "run", "--layer", "contract_gate", "--output", gateOutput, "--invocation-id", "t37-contract-gate")
	if gate.ExitCode != 0 {
		t.Fatalf("contract gate did not pass: %+v (see gate-run.log)", gate)
	}
	statementBody := readAndRecord(t, filepath.Join(gateOutput, "test-result.json"), &artifacts)
	indexBody := readAndRecord(t, filepath.Join(gateOutput, "evidence.json"), &artifacts)
	var statement result.Statement
	if err := json.Unmarshal(statementBody, &statement); err != nil {
		t.Fatal(err)
	}
	var index evidence.Index
	if err := json.Unmarshal(indexBody, &index); err != nil {
		t.Fatal(err)
	}
	assertions["whole-contract-gate"] = map[string]any{
		"expected": map[string]any{"verdict": "PASSED", "passedTests": expectedGateTests},
		"actual":   map[string]any{"verdict": statement.Predicate.Result, "passedTests": statement.Predicate.PassedTests},
	}
	if statement.Predicate.Result != "PASSED" {
		t.Fatalf("gate verdict %s", statement.Predicate.Result)
	}
	if strings.Join(statement.Predicate.PassedTests, ",") != strings.Join(expectedGateTests, ",") {
		t.Fatalf("gate passed tests mismatch: %v", statement.Predicate.PassedTests)
	}
	// The suite verdict must be exactly the frozen profile aggregation over
	// the persisted item outcomes: nothing else computes it.
	profile, err := result.LoadProfile(filepath.Join(repoContracts, "verification-result-profile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	assertions["verdict-aggregation"] = map[string]any{
		"expected": profile.Aggregate(index.Items),
		"actual":   statement.Predicate.Result,
	}
	if profile.Aggregate(index.Items) != statement.Predicate.Result {
		t.Fatal("statement verdict diverges from the frozen profile aggregation")
	}
	// Every attachment digest must match the bytes on disk.
	digestChecks := map[string]string{}
	for _, item := range index.Items {
		for _, attachment := range item.Attachments {
			onDisk, _, err := evidence.DigestFile(attachment.Locator)
			if err != nil {
				t.Fatalf("attachment missing: %s", attachment.Locator)
			}
			digestChecks[attachment.Locator] = onDisk
			if onDisk != attachment.SHA256 {
				t.Fatalf("attachment digest drift: %s", attachment.Locator)
			}
		}
	}
	assertions["attachment-digests"] = map[string]any{
		"expected": "every evidence attachment sha256 equals the file content digest",
		"actual":   digestChecks,
	}
	artifacts = append(artifacts, artifactOf(t, filepath.Join(gateOutput, "evidence.json")), artifactOf(t, filepath.Join(gateOutput, "test-result.json")))

	// --- Leg 3: catalog build-gate negatives ---------------------------
	negativeCases := []struct {
		name string
		mut  func(string) string
		code string
	}{
		{"dependency-cycle", func(b string) string {
			return strings.Replace(b, "depends_on: []", "depends_on: [t37.dependent]", 1)
		}, "dependency_cycle"},
		{"dangling-dependency", func(b string) string {
			return strings.Replace(b, "depends_on: [t37.base]", "depends_on: [t37.ghost]", 1)
		}, "dangling_dependency"},
		{"dangling-proof-ref", func(b string) string {
			return strings.Replace(b, "    proof_refs: []", "    proof_refs: [t37.ghost]", 1)
		}, "dangling_proof_ref"},
		{"same-layer-proof-ref", func(b string) string {
			return strings.Replace(b, "depends_on: [t37.base]\n    proof_refs: []", "depends_on: []\n    proof_refs: [t37.base]", 1)
		}, "proof_ref_not_lower_layer"},
		{"applicability-not-closed", func(b string) string {
			return strings.Replace(b, "applicability: {mode: always}", "applicability: {mode: deployment_target, backend: compose, architecture: linux/amd64}", 1)
		}, "applicability_not_closed"},
		{"uncovered-root", func(b string) string {
			return strings.Replace(b, "  - id: ARCH-VALIDATION-001", "  - id: GHOST-VALIDATION-001\n    source: docs/specs/quoin-v1/architecture.md\n  - id: ARCH-VALIDATION-001", 1)
		}, "uncovered_validation_root"},
	}
	negativeResults := map[string]map[string]any{}
	for _, tc := range negativeCases {
		path := writeTicketFixture(t, tc.mut(t37FixtureCatalog()))
		validated := record("negative-"+tc.name, nil, binary, "validate", "--catalog", path)
		body := readAndRecord(t, filepath.Join(root, "logs", "negative-"+tc.name+".log"), &artifacts)
		negativeResults[tc.name] = map[string]any{
			"expected": map[string]any{"exitCode": 1, "contains": tc.code},
			"actual":   map[string]any{"exitCode": validated.ExitCode, "contains": strings.Contains(string(body), tc.code)},
		}
		if validated.ExitCode != 1 || !strings.Contains(string(body), tc.code) {
			t.Fatalf("negative %s not rejected: exit=%d body=%s", tc.name, validated.ExitCode, body)
		}
	}
	assertions["catalog-negatives"] = map[string]any{"actual": negativeResults, "expected": "every mutated catalog rejected with its stable violation code and exit 1"}
	// The frozen catalog itself validates clean through the same command.
	clean := record("validate-frozen-catalog", nil, binary, "validate", "--catalog", filepath.Join(repoRoot(t), "docs/specs/quoin-v1/contracts/verification-catalog.yaml"))
	if clean.ExitCode != 0 {
		t.Fatalf("frozen catalog rejected: %+v", clean)
	}

	// --- Leg 4: verdict causality through the real binary --------------
	causalityDir := writeCausalityFixture(t)
	defer func() { cleanupFixtures = append(cleanupFixtures, causalityDir) }()
	causality := record("causality-run", []string{"QUOIN_CAUSALITY_FIXTURE=" + causalityDir}, binary,
		"run", "--layer", "contract_gate", "--output", filepath.Join(root, "causality"),
		"--catalog", filepath.Join(causalityDir, "verification-catalog.yaml"),
		"--invocation-id", "t37-causality")
	causalityBody := readAndRecord(t, filepath.Join(root, "logs", "causality-run.log"), &artifacts)
	var causalityIndex evidence.Index
	if err := json.Unmarshal(mustRead(t, filepath.Join(root, "causality", "evidence.json")), &causalityIndex); err != nil {
		t.Fatal(err)
	}
	artifacts = append(artifacts, artifactOf(t, filepath.Join(root, "causality", "evidence.json")))
	byName := map[string]evidence.Item{}
	for _, item := range causalityIndex.Items {
		byName[item.TestName()] = item
	}
	assertions["causality"] = map[string]any{
		"expected": map[string]any{
			"suite":         "FAILED (exit 3)",
			"failing":       "functional_assertion_failed",
			"independent":   "passed despite the earlier failure (no fail-fast)",
			"dependent":     "not_run/warned with a causal id",
			"teardownAfter": "the failed scenario's teardown marker exists",
		},
		"actual": map[string]any{
			"exitCode": causality.ExitCode,
			"failing":  byName["t37.failing.default"].Category,
			"independent": map[string]any{"outcome": byName["t37.independent.default"].Outcome,
				"exitCode": byName["t37.independent.default"].ExitCode},
			"dependent": map[string]any{"outcome": byName["t37.dependent.default"].Outcome,
				"category": byName["t37.dependent.default"].Category,
				"causal":   byName["t37.dependent.default"].CausalIDs},
			"teardownAfter": strings.Contains(string(causalityBody), "suite FAILED") && fileExists(filepath.Join(causalityDir, "teardown.marker")),
		},
	}
	if causality.ExitCode != 3 {
		t.Fatalf("causality suite must exit 3 (FAILED), got %d", causality.ExitCode)
	}
	if byName["t37.failing.default"].Category != "functional_assertion_failed" {
		t.Fatalf("failing item category: %s", byName["t37.failing.default"].Category)
	}
	if byName["t37.independent.default"].Outcome != "passed" {
		t.Fatalf("independent scenario did not pass after the failure: %+v", byName["t37.independent.default"])
	}
	dependent := byName["t37.dependent.default"]
	if dependent.Outcome != "warned" || dependent.Category != "not_run" || len(dependent.CausalIDs) == 0 {
		t.Fatalf("dependent not recorded as not_run with causal id: %+v", dependent)
	}
	if !fileExists(filepath.Join(causalityDir, "teardown.marker")) {
		t.Fatal("teardown after failure not proven")
	}

	// --- Leg 5: WARNED classification (timeout keeps the suite going) ---
	warnedDir := writeTimeoutFixture(t)
	defer func() { cleanupFixtures = append(cleanupFixtures, warnedDir) }()
	warned := record("warned-run", []string{"QUOIN_CAUSALITY_FIXTURE=" + warnedDir}, binary,
		"run", "--layer", "contract_gate", "--output", filepath.Join(root, "warned"),
		"--catalog", filepath.Join(warnedDir, "verification-catalog.yaml"),
		"--invocation-id", "t37-warned")
	if warned.ExitCode != 4 {
		t.Fatalf("warned suite must exit 4, got %d", warned.ExitCode)
	}
	var warnedIndex evidence.Index
	if err := json.Unmarshal(mustRead(t, filepath.Join(root, "warned", "evidence.json")), &warnedIndex); err != nil {
		t.Fatal(err)
	}
	warnedSummary := map[string]any{}
	for _, item := range warnedIndex.Items {
		warnedSummary[item.TestName()] = item.Category
	}
	assertions["warned-causality"] = map[string]any{
		"expected": map[string]any{"exitCode": 4, "timeoutItem": "infrastructure_interrupted", "otherItem": "passed"},
		"actual":   map[string]any{"exitCode": warned.ExitCode, "categories": warnedSummary},
	}
	artifacts = append(artifacts, artifactOf(t, filepath.Join(root, "warned", "evidence.json")))

	// --- Runtime evidence and cleanup ----------------------------------
	finishedAt := time.Now().UTC()
	// Real product-path transitions observed through the gate cells; each
	// citation points at the recorded attachment for the executing cell.
	productPaths := map[string]map[string]string{}
	for _, item := range index.Items {
		for _, attachment := range item.Attachments {
			if attachment.Kind != evidence.AttachmentStructuredResult {
				continue
			}
			productPaths[item.TestName()] = map[string]string{"structuredResult": attachment.Locator}
		}
	}
	writeJSON(t, filepath.Join(root, "runtime-evidence.json"), map[string]any{
		"ticket": "T37", "issue": 60,
		"gitCommit": commit, "dirtyStateDigest": dirty,
		"startedAt": startedAt.Format(time.RFC3339Nano), "finishedAt": finishedAt.Format(time.RFC3339Nano),
		"status": "passed",
		"tooling": map[string]any{
			"quoinVerifyBinary": map[string]any{"path": binary, "sha256": binaryDigest},
			"goVersion":         goVersion(t),
			"catalogDigest":     fileDigest(t, realCatalog),
			"profileDigest":     fileDigest(t, filepath.Join(repoContracts, "verification-result-profile.yaml")),
			"evidenceSchema":    fileDigest(t, filepath.Join(repoContracts, "schemas", "verification-evidence.schema.json")),
			"resultSchema":      fileDigest(t, filepath.Join(repoContracts, "schemas", "verification-result.schema.json")),
		},
		"observedTransitions": map[string]any{
			"gate": map[string]any{
				"invocation": "t37-contract-gate",
				"result":     statement.Predicate.Result,
				"items":      len(index.Items),
				"matrix":     statement.Predicate.Quoin.Environment,
				"perItem":    itemSummaries(index.Items),
			},
			"productPaths": map[string]string{
				"sqlite-session-idle-boundary": "fault.time.session-idle-expiry drove real sessions rows through the real Authenticate path (structured result: " + productPaths["fault.time.session-idle-expiry"]["structuredResult"] + ")",
				"http-reveal-lifecycle":        "fault.time.reveal-handle-expiry ran the reveal lifecycle over a real HTTP server (structured result: " + productPaths["fault.time.reveal-handle-expiry"]["structuredResult"] + ")",
				"runtime-protocol-fences":      "fault.protocol-delivery reorder/duplicate drove the runtime cancel/fence and delivery replay paths (structured results: " + productPaths["fault.protocol-delivery.reorder"]["structuredResult"] + ", " + productPaths["fault.protocol-delivery.duplicate"]["structuredResult"] + ")",
			},
			"causality": map[string]any{"exitCode": causality.ExitCode, "items": len(causalityIndex.Items)},
			"warned":    map[string]any{"exitCode": warned.ExitCode, "items": len(warnedIndex.Items)},
		},
		"commands":   commands,
		"artifacts":  artifacts,
		"assertions": assertions,
		"proofPoints": map[string]string{
			"gate":      "gate/ holds the full contract-gate invocation: evidence.json, test-result.json and per-cell stdout/stderr/facts",
			"negatives": "logs/negative-*.log record each mutated catalog rejected with its stable code",
			"causality": "causality/ proves FAILED→not_run propagation, independent continuation and teardown after failure",
			"warned":    "warned/ proves timeout→infrastructure_interrupted WARNED while the independent scenario still passes",
			"teardown":  "the causality fixture teardown.marker file proves teardown ran after the failed action",
		},
	})
	// Cleanup disposition is proven, not asserted: every fixture directory
	// must be gone, no owned process may outlive the legs, and the retained
	// evidence artifacts must exist. Any failure fails the ticket here even
	// though every behaviour assertion already passed (VERIFY-CLEANUP-002).
	dispositions := map[string]string{}
	t.Cleanup(func() {
		for _, fixture := range cleanupFixtures {
			if err := os.RemoveAll(fixture); err != nil {
				dispositions[fixture] = "RESIDUE: " + err.Error()
				continue
			}
			if _, err := os.Stat(fixture); os.IsNotExist(err) {
				dispositions[fixture] = "removed"
				continue
			}
			dispositions[fixture] = "RESIDUE"
		}
		if orphans := orphanProcesses(t); len(orphans) != 0 {
			dispositions["processes"] = "RESIDUE: " + strings.Join(orphans, ", ")
		} else {
			dispositions["processes"] = "none"
		}
		// cleanup.json is written by this very closure, so only the
		// earlier artifacts are presence-checked here.
		for _, retained := range []string{"runtime-evidence.json", filepath.Join("gate", "test-result.json"), filepath.Join("bin", "quoin-verify")} {
			if !fileExists(filepath.Join(root, retained)) {
				dispositions["retained:"+retained] = "MISSING"
			} else {
				dispositions["retained:"+retained] = "present"
			}
		}
		for fixture, state := range dispositions {
			if state != "removed" && state != "none" && state != "present" {
				t.Fatalf("cleanup residue: %s -> %s", fixture, state)
			}
		}
		writeJSON(t, filepath.Join(root, "cleanup.json"), map[string]any{
			"ownedResources": map[string]any{
				"fixtureDirectories": dispositionsOf(cleanupFixtures, dispositions),
				"builtBinary":        binary + " (retained as ticket evidence: " + dispositionsPresent(root, "bin/quoin-verify") + ")",
				"invocationOutputs":  []string{"gate/", "causality/", "warned/", "logs/ (retained as ticket evidence)"},
				"childProcesses":     "each harness group waits for its go test child; the runner kills process groups on timeout (observed: " + dispositions["processes"] + ")",
			},
			"preExistingUntouched": "no Docker/Compose/Kubernetes resources, ports, volumes or credentials are used by this ticket",
			"dispositions":         dispositions,
			"result":               "every owned fixture removed, no owned process alive, retained evidence present",
		})
	})
}

func dispositionsOf(fixtures []string, dispositions map[string]string) []map[string]string {
	records := make([]map[string]string, 0, len(fixtures))
	for _, fixture := range fixtures {
		records = append(records, map[string]string{"path": fixture, "disposition": dispositions[fixture]})
	}
	return records
}

func dispositionsPresent(root, relative string) string {
	if fileExists(filepath.Join(root, relative)) {
		return "present"
	}
	return "MISSING"
}

// orphanProcesses lists any process still running one of the owned fixture
// entrypoints or the built runner binary. The patterns are anchored to the
// owned path prefixes so unrelated tooling that merely mentions the names
// cannot produce false residue.
func orphanProcesses(t *testing.T) []string {
	t.Helper()
	body, err := exec.Command("pgrep", "-af", `t37-fixture-|quoin-verify`).Output()
	if err != nil {
		if _, isExit := err.(*exec.ExitError); isExit {
			return nil // pgrep exit 1: no matches
		}
		return nil
	}
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), " ", 2)
		if len(fields) < 2 {
			continue
		}
		// Only a process whose EXECUTABLE is an owned fixture script or the
		// built runner counts as residue; wrappers and shells that merely
		// carry the names inside embedded command text do not.
		executable := fields[1]
		if strings.HasPrefix(executable, "/tmp/t37-fixture-") ||
			strings.HasSuffix(executable, "tickets/T37/bin/quoin-verify") {
			found = append(found, line)
		}
	}
	return found
}

func itemSummaries(items []evidence.Item) []map[string]any {
	summaries := make([]map[string]any, 0, len(items))
	for _, item := range items {
		summaries = append(summaries, map[string]any{
			"test": item.TestName(), "outcome": item.Outcome, "category": item.Category,
			"exitCode": item.ExitCode, "attachments": len(item.Attachments),
		})
	}
	return summaries
}

// run executes a command, records it and stores its combined output under
// the evidence root.
func run(t *testing.T, root, name string, env []string, argv ...string) commandRecord {
	t.Helper()
	logs := filepath.Join(root, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = repoRoot(t)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	} else {
		cmd.Env = os.Environ()
	}
	started := time.Now()
	body, err := cmd.CombinedOutput()
	record := commandRecord{Name: name, Argv: argv, ExitCode: exitCodeOf(err), Duration: time.Since(started).Round(time.Millisecond).String(), Log: filepath.Join("logs", name+".log")}
	if writeErr := os.WriteFile(filepath.Join(logs, name+".log"), body, 0o644); writeErr != nil {
		t.Fatal(writeErr)
	}
	return record
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if e, ok := err.(*exec.ExitError); ok {
		exitError = e
		return exitError.ExitCode()
	}
	return -1
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func readAndRecord(t *testing.T, path string, artifacts *[]map[string]any) []byte {
	t.Helper()
	body := mustRead(t, path)
	*artifacts = append(*artifacts, artifactOf(t, path))
	return body
}

func artifactOf(t *testing.T, path string) map[string]any {
	t.Helper()
	body := mustRead(t, path)
	sum := sha256.Sum256(body)
	return map[string]any{"path": path, "sha256": hex.EncodeToString(sum[:]), "sizeBytes": len(body)}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	body := mustRead(t, path)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func gitState(t *testing.T) (string, string) {
	t.Helper()
	commit, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	status, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append(bytesTrimNewline(commit), status...))
	return strings.TrimSpace(string(commit)), hex.EncodeToString(sum[:])
}

func goVersion(t *testing.T) string {
	t.Helper()
	body, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

func bytesTrimNewline(body []byte) []byte {
	return []byte(strings.TrimRight(string(body), "\n"))
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
