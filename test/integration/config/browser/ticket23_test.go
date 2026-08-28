package browser

// TestTicket23 is the replayable real-process acceptance entrypoint for
// Config Verification browser Journeys (issue #46). The deterministic layers
// (contracts, full Go suite, vet, web suite) run first; the Playwright leg
// boots the isolated T23 Compose stack with a real Lintel Chromium, drives a
// real manual login + profile publish, then executes the real
// page.status-marker.v1 Journeys through the public HTTP surface. All T23
// evidence stays under .artifacts/tickets/T23.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

type commandEvidence struct {
	Name     string `json:"name"`
	ExitCode int    `json:"exitCode"`
	Duration string `json:"duration"`
	Log      string `json:"log"`
	SHA256   string `json:"sha256"`
}

func init() {
	// Lift only Go's implicit package deadline for this opt-in test. The real
	// stack's documented cold-start readiness interval exceeds `go test`'s
	// injected ten-minute default; an explicit caller-supplied timeout is kept.
	for index, argument := range os.Args {
		if argument == "-test.timeout=10m" || argument == "-test.timeout=10m0s" {
			os.Args[index] = "-test.timeout=45m"
		}
	}
}

func TestTicket23(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T23 real-process acceptance is disabled")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skipf("pnpm unavailable: %v", err)
	}
	repository := repositoryRoot(t)
	wanted := filepath.Join(repository, ".artifacts", "tickets", "T23")
	actual, err := filepath.Abs(evidenceDir)
	if err != nil || actual != wanted {
		t.Fatalf("T23 evidence directory must be %s, got %s (err=%v)", wanted, actual, err)
	}
	lock := acquireTicket23Lock(t, filepath.Join(repository, ".artifacts", "tickets", ".browser-e2e.lock"))
	defer lock.release()
	resetEvidenceDirectory(t, actual)
	sourceBefore := workspaceDigest(t)
	commands := make([]commandEvidence, 0, 9)
	run := func(name, command string, args ...string) {
		t.Helper()
		started := time.Now()
		cmd := exec.Command(command, args...)
		cmd.Dir = repository
		// Children never re-enter this acceptance entrypoint; only the
		// Playwright leg receives the T23 ticket/evidence environment.
		cmd.Env = acceptanceEnv(actual, name == "ticket23-playwright")
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		err := cmd.Run()
		exitCode := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				exitCode = exit.ExitCode()
			} else {
				exitCode = -1
			}
		}
		logPath := filepath.Join(actual, name+".log")
		if writeErr := os.WriteFile(logPath, output.Bytes(), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		digest := sha256.Sum256(output.Bytes())
		commands = append(commands, commandEvidence{Name: name, ExitCode: exitCode, Duration: time.Since(started).Round(time.Millisecond).String(), Log: logPath, SHA256: hex.EncodeToString(digest[:])})
		if err != nil {
			t.Fatalf("%s exited %d:\n%s", name, exitCode, output.String())
		}
	}

	run("verify-contracts", "./ci/verify-contracts")
	run("go-test-all", "go", "test", "-json", "./...", "-count=1")
	run("go-vet", "go", "vet", "./...")
	run("pnpm-install", "pnpm", "--dir", "web", "install", "--frozen-lockfile")
	run("web-typecheck", "pnpm", "--dir", "web", "typecheck")
	run("web-lint", "pnpm", "--dir", "web", "lint")
	run("web-test", "pnpm", "--dir", "web", "test")
	run("web-build", "pnpm", "--dir", "web", "build")
	// The real-stack leg runs the isolated quoin-t23 Compose project: real
	// Quoin/Lintel gRPC, real Chromium executing the versioned Journeys, real
	// Stop/cleanup fences. No page, RFB, trace, or credential content is
	// retained in evidence.
	run("ticket23-playwright", "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-23", "--project=chromium")

	sourceAfter := workspaceDigest(t)
	if sourceBefore != sourceAfter {
		t.Fatalf("verified source changed during T23 acceptance: before=%s after=%s", sourceBefore, sourceAfter)
	}
	observations := requiredEvidence(t, filepath.Join(actual, "t23-journey-observations.json"))
	var parsed struct {
		HappyRun struct {
			State  string `json:"state"`
			Checks []struct {
				CheckKey  string `json:"checkKey"`
				Status    string `json:"status"`
				GapReason string `json:"gapReason"`
				Evidence  any    `json:"evidence"`
			} `json:"checks"`
		} `json:"happyRun"`
		AuthRun struct {
			State  string `json:"state"`
			Checks []struct {
				CheckKey  string `json:"checkKey"`
				Status    string `json:"status"`
				GapReason string `json:"gapReason"`
			} `json:"checks"`
		} `json:"authRun"`
		NoWholeRunRetry struct {
			StatusSeqDelta int `json:"statusSeqDelta"`
			BrokenSeqDelta int `json:"brokenSeqDelta"`
		} `json:"noWholeRunRetry"`
		CleanupClosure struct {
			IdentityReleased bool `json:"identityReleased"`
		} `json:"cleanupClosure"`
	}
	if err := json.Unmarshal([]byte(observations), &parsed); err != nil {
		t.Fatalf("decode T23 journey observations: %v", err)
	}
	byKey := map[string]struct {
		CheckKey  string `json:"checkKey"`
		Status    string `json:"status"`
		GapReason string `json:"gapReason"`
		Evidence  any    `json:"evidence"`
	}{}
	for _, check := range parsed.HappyRun.Checks {
		byKey[check.CheckKey] = check
	}
	if parsed.HappyRun.State != "Failed" {
		t.Fatalf("the mixed happy/failure run must settle Failed: %s", parsed.HappyRun.State)
	}
	if byKey["status-page"].Status != "ok" || byKey["status-page"].Evidence == nil {
		t.Fatalf("the happy Journey must retain primary structured Evidence: %#v", byKey["status-page"])
	}
	if byKey["broken-page"].Status != "gap" || byKey["broken-page"].GapReason != "journey_failed" {
		t.Fatalf("the failing Journey must close as journey_failed: %#v", byKey["broken-page"])
	}
	if parsed.AuthRun.State != "Failed" || len(parsed.AuthRun.Checks) == 0 {
		t.Fatalf("a never-logged-in identity must settle every check as a gap: %#v", parsed.AuthRun)
	}
	for _, check := range parsed.AuthRun.Checks {
		if check.Status != "gap" || check.GapReason != "authentication_required" {
			t.Fatalf("a never-logged-in identity must settle authentication_required: %#v", check)
		}
	}
	if parsed.NoWholeRunRetry.StatusSeqDelta < 1 || parsed.NoWholeRunRetry.StatusSeqDelta > 2 || parsed.NoWholeRunRetry.BrokenSeqDelta < 1 || parsed.NoWholeRunRetry.BrokenSeqDelta > 2 {
		t.Fatalf("each Journey page must be fetched within one journey execution (no whole-run retry): %#v", parsed.NoWholeRunRetry)
	}
	if !parsed.CleanupClosure.IdentityReleased {
		t.Fatal("the identity lock must be released after the journey Stop fence")
	}
	assertT23Cleanup(t)
	cleanupPath := filepath.Join(actual, "cleanup.json")
	cleanup := map[string]any{}
	if body, err := os.ReadFile(cleanupPath); err == nil {
		if err := json.Unmarshal(body, &cleanup); err != nil {
			t.Fatalf("decode Playwright cleanup evidence: %v", err)
		}
	}
	cleanup["ticket23Verification"] = map[string]string{
		"containers": "no quoin-t23-owned container remains after the isolated stack teardown",
		"networks":   "no quoin-t23-owned network remains after teardown",
		"processes":  "no T23 TLS proxy or readiness helper remains",
		"artifacts":  "no browser trace body, screenshot, video, RFB frame, profile content, cookie, or credential was retained in evidence",
	}
	writeJSON(t, cleanupPath, cleanup)
	writeJSON(t, filepath.Join(actual, "runtime-evidence.json"), map[string]any{
		"gitCommit":          strings.TrimSpace(commandOutput(t, "git", "rev-parse", "HEAD")),
		"sourceDigestBefore": sourceBefore,
		"sourceDigestAfter":  sourceAfter,
		"commands":           commands,
		"realRuntime":        "Quoin gRPC Runtime control stream → Lintel Manager → isolated Chromium/Xvfb executing the embedded page.status-marker.v1 Journey; manual login published the profile over the real noVNC path",
		"expectedVersusActual": map[string]string{
			"journey-happy":      "the real status-marker Journey navigated the authenticated profile to /status, passed admission+completion probes, and its check retained the primary structured Evidence",
			"journey-failure":    "the broken-page check closed as gap journey_failed through the frozen bounded selector wait; the mandatory trace is committed by the runtime upload ledger",
			"journey-auth":       "a never-logged-in identity settled deterministically as gap authentication_required without creating a journey operation",
			"revision-mismatch":  "catalog digest and profile Chromium revision mismatches are rejected fail-closed before any process side effect (lintel runtime package tests inside go-test-all: TestStartRejectsJourneyCatalogDigestMismatch, TestStartRejectsJourneyProfileRevisionMismatch)",
			"no-whole-run-retry": "each journey page was fetched exactly once and the run closed Failed; re-verification requires a new run by design",
			"trace-and-cleanup":  "the Stop fence confirmed physical teardown, the identity lock was released (currentOperation null), and teardown proved no owned container/network/helper survives",
		},
		"rawEvidence": evidenceDigests(t, actual),
	})
}

func acceptanceEnv(evidenceDir string, playwright bool) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "QUOIN_TICKET=") && !strings.HasPrefix(value, "QUOIN_EVIDENCE_DIR=") && !strings.HasPrefix(value, "QUOIN_BROWSER_E2E_LOCK_HELD=") {
			env = append(env, value)
		}
	}
	if playwright {
		return append(env, "QUOIN_TICKET=T23", "QUOIN_EVIDENCE_DIR="+evidenceDir, "QUOIN_BROWSER_E2E_LOCK_HELD=1")
	}
	return append(env, "QUOIN_TICKET=", "QUOIN_EVIDENCE_DIR=", "QUOIN_BROWSER_E2E_LOCK_HELD=")
}

func resetEvidenceDirectory(t *testing.T, evidenceDir string) {
	t.Helper()
	if err := os.RemoveAll(evidenceDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
}

type ticket23Lock struct{ file *os.File }

func (lock ticket23Lock) release() {
	if lock.file != nil {
		_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
		_ = lock.file.Close()
	}
}

// acquireTicket23Lock serializes the fixed loopback ports and stack directory
// against any other browser ticket acceptance running on the same host.
func acquireTicket23Lock(t *testing.T, path string) ticket23Lock {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return ticket23Lock{file: file}
}

func assertT23Cleanup(t *testing.T) {
	t.Helper()
	if containers := commandOutput(t, "docker", "ps", "-a", "--format", "{{.Names}}"); strings.Contains(containers, "quoin-t23-") {
		t.Fatalf("T23 cleanup left a browser fixture container: %s", containers)
	}
	if networks := strings.TrimSpace(commandOutput(t, "docker", "network", "ls", "--filter", "name=quoin-t23", "--format", "{{.Name}}")); networks != "" {
		t.Fatalf("T23 cleanup left browser fixture network(s): %s", networks)
	}
	if processes := commandOutput(t, "ps", "-eo", "args"); strings.Contains(processes, ".artifacts/e2e-stack-t23/tls-proxy.mjs") || strings.Contains(processes, ".artifacts/e2e-stack-t23/ready.py") {
		t.Fatal("T23 cleanup left a TLS proxy or readiness helper")
	}
	// Playwright writes its bookkeeping .last-run.json after globalTeardown
	// removed the stack; that single file is not test-owned residue.
	stack := filepath.Join(repositoryRoot(t), ".artifacts", "e2e-stack-t23")
	if entries, err := os.ReadDir(stack); err == nil {
		if len(entries) != 1 || entries[0].Name() != "playwright-output" {
			t.Fatalf("T23 cleanup left unexpected stack residue: %#v", entries)
		}
		if inner, readErr := os.ReadDir(filepath.Join(stack, "playwright-output")); readErr != nil || len(inner) != 1 || inner[0].Name() != ".last-run.json" {
			t.Fatalf("T23 cleanup left unexpected Playwright residue: %#v (err=%v)", inner, readErr)
		}
		if err := os.RemoveAll(stack); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func requiredEvidence(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 {
		t.Fatalf("required T23 evidence %s: %v", path, err)
	}
	return string(body)
}

func evidenceDigests(t *testing.T, evidenceDir string) map[string]string {
	t.Helper()
	allowed := map[string]bool{
		"verify-contracts.log": true, "go-test-all.log": true, "go-vet.log": true,
		"pnpm-install.log": true, "web-typecheck.log": true, "web-lint.log": true,
		"web-test.log": true, "web-build.log": true, "ticket23-playwright.log": true,
		"playwright-server.log": true, "runtime-process.log": true, "t23-components.log": true, "cleanup.json": true, "t23-journey-observations.json": true,
	}
	digests := map[string]string{}
	err := filepath.WalkDir(evidenceDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(path) == "runtime-evidence.json" {
			return err
		}
		relative, err := filepath.Rel(evidenceDir, path)
		if err != nil || !allowed[relative] {
			return fmt.Errorf("unexpected T23 evidence file %q", relative)
		}
		body, err := os.ReadFile(path)
		if err == nil {
			digests[relative] = sha256Text(body)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range allowed {
		if _, ok := digests[name]; !ok {
			t.Fatalf("required T23 evidence file missing: %s", name)
		}
	}
	return digests
}

func workspaceDigest(t *testing.T) string {
	t.Helper()
	patch := commandOutput(t, "git", "diff", "--binary", "HEAD")
	untracked := strings.Fields(commandOutput(t, "git", "ls-files", "--others", "--exclude-standard"))
	sort.Strings(untracked)
	hash := sha256.New()
	_, _ = hash.Write([]byte("patch\x00" + patch + "\x00untracked\x00"))
	for _, path := range untracked {
		body, err := os.ReadFile(filepath.Join(repositoryRoot(t), path))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = hash.Write([]byte(path + "\x00"))
		_, _ = hash.Write(body)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func commandOutput(t *testing.T, command string, args ...string) string {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = repositoryRoot(t)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v", command, strings.Join(args, " "), err)
	}
	return string(output)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("repository root not found")
		}
		directory = parent
	}
}

func sha256Text(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
