package login

// TestTicket20 owns the replayable, real-process acceptance entrypoint. It
// delegates the browser interaction to the Chromium Playwright spec rather
// than emulating RFB in-process: that spec starts Compose, registers Lintel,
// creates the identity through the public API, and drives noVNC to a profile
// publish. Evidence is intentionally limited to metadata and command output;
// it never writes login input, RFB bytes, screenshots, or Playwright traces.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type acceptanceCommand struct {
	Name     string `json:"name"`
	ExitCode int    `json:"exitCode"`
	Duration string `json:"duration"`
	Log      string `json:"log"`
	SHA256   string `json:"sha256"`
}

func TestTicket20(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T20 acceptance run disabled")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	resetEvidenceDir(t, evidenceDir)
	testBootstrapFailureCleanup(t, evidenceDir)
	resetEvidenceDir(t, evidenceDir)

	commands := []acceptanceCommand{}
	run := func(name, command string, args ...string) {
		t.Helper()
		started := time.Now()
		cmd := exec.Command(command, args...)
		cmd.Dir = repoRoot(t)
		cmd.Env = acceptanceEnvironment(evidenceDir, name == "ticket20-playwright")
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		err := cmd.Run()
		if name == "ticket20-playwright" {
			stackDirectory := filepath.Join(repoRoot(t), ".artifacts", "e2e-stack-t20")
			if removeErr := os.RemoveAll(stackDirectory); removeErr != nil {
				t.Fatalf("remove T20 Playwright stack and failure output: %v", removeErr)
			}
		}
		exitCode := 0
		if err != nil {
			if status, ok := err.(*exec.ExitError); ok {
				exitCode = status.ExitCode()
			} else {
				exitCode = -1
			}
		}
		logPath := filepath.Join(evidenceDir, name+".log")
		if writeErr := os.WriteFile(logPath, output.Bytes(), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		digest := sha256.Sum256(output.Bytes())
		commands = append(commands, acceptanceCommand{Name: name, ExitCode: exitCode, Duration: time.Since(started).Round(time.Millisecond).String(), Log: logPath, SHA256: hex.EncodeToString(digest[:])})
		if err != nil {
			t.Fatalf("%s exited %d:\n%s", name, exitCode, output.String())
		}
	}

	// TestTicket20 owns a fresh evidence directory and runs the fixed matrix
	// itself. Child full-repo tests clear QUOIN_EVIDENCE_DIR so this entrypoint
	// is skipped rather than recursively re-entered.
	run("verify-contracts", "go", "run", "./ci/contracts/verify")
	run("go-test-all", "go", "test", "./...")
	run("go-vet", "go", "vet", "./...")
	run("pnpm-install", "pnpm", "--dir", "web", "install", "--frozen-lockfile")
	run("web-typecheck", "pnpm", "--dir", "web", "typecheck")
	run("web-lint", "pnpm", "--dir", "web", "lint")
	run("web-test", "pnpm", "--dir", "web", "test")
	run("web-build", "pnpm", "--dir", "web", "build")
	sourceBefore := workspaceContentDigest(t)
	// The frozen Lintel unit harness fixes the technical-fault and explicit
	// unauthenticated boundaries; the following Playwright command then covers
	// the real process and user path without substituting a mock browser.
	run("probe-outcome-matrix", "go", "test", "./internal/lintel/runtime", "-run", "TestPublish(RunsTypedURLProbeAndInstallsProfile|ProbeUnavailableIsIndeterminateAndDoesNotInstallProfile)$", "-count=1")
	// The filtered test has a real Compose webServer, real Lintel Chromium,
	// Xvfb and x0vncserver, plus the actual noVNC RFB implementation. It also
	// opts out of Playwright trace/screenshot/video in the spec itself.
	run("ticket20-playwright", "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-20", "--project=chromium")

	sourceAfter := workspaceContentDigest(t)
	if sourceAfter != sourceBefore {
		t.Fatalf("verified source changed during T20 acceptance: before=%s after=%s", sourceBefore, sourceAfter)
	}
	commit := strings.TrimSpace(commandOutput(t, "git", "rev-parse", "HEAD"))
	observationsPath := filepath.Join(evidenceDir, "t20-browser-observations.json")
	componentsPath := filepath.Join(evidenceDir, "t20-components.log")
	_ = evidenceFile(t, observationsPath)
	components := evidenceFile(t, componentsPath)
	runtimeEvidence := map[string]any{
		"gitCommit":             commit,
		"sourceContentDigest":   sourceAfter,
		"sourceDigestBeforeRun": sourceBefore,
		"commands":              commands,
		"components": map[string]any{
			"browser":                "Chromium + Xvfb + x0vncserver in the real Lintel Compose image; noVNC @1.7.0 in the operator UI",
			"transport":              "same-Origin authenticated WebSocket → Quoin BrowserTunnel → Lintel loopback RFB",
			"versionsAndImageDigest": map[string]string{"rawLog": componentsPath, "sha256": sha256Text(components)},
		},
		"rawEvidence": map[string]string{},
		"expectedVersusActual": map[string]string{
			"bootstrap-failure-cleanup": "expected intentional bootstrap exit 97 runs the same scoped teardown; actual test asserts no owned containers, networks, volumes, images, host helpers, or stack directory remain before the successful replay",
			"headed-browser":            "expected real Chromium/Xvfb/x0vncserver/noVNC; actual component versions and UI observations are SHA-256 bound above",
			"origin-session":            "expected foreign Origin and anonymous Session rejection; actual WebSocket observations record both rejected while the same-Origin Session receives the RFB canvas",
			"catalog":                   "expected Quoin/Lintel frozen catalog binding; actual UI observation records equality of the identity revision and public Catalog digest/version",
			"authenticated":             "expected publish after URL-prefix probe; actual UI operation reaches Succeeded",
			"unauthenticated":           "real T20 fixture verifies unauthenticated publish preserves AuthenticationRequired with no profile; deterministic Lintel rejection test covers the typed boundary",
			"indeterminate":             "covered by deterministic Lintel probe-unavailable test in probe-outcome-matrix; no profile generation is committed",
			"recording":                 "expected no login/RFB/page/trace/screenshot recording; actual T20 spec disables trace/screenshot/video and evidence contains metadata only",
		},
		"redactions": "No passwords, Session cookies, RFB frames, screenshots, traces, or page content are retained.",
	}

	assertT20ResourcesCleaned(t)
	cleanupPath := filepath.Join(evidenceDir, "cleanup.json")
	cleanup := map[string]any{}
	if body, err := os.ReadFile(cleanupPath); err == nil {
		if err := json.Unmarshal(body, &cleanup); err != nil {
			t.Fatalf("read Playwright cleanup evidence: %v", err)
		}
	}
	cleanup["verification"] = map[string]any{
		"containers":  "Playwright global teardown removed the isolated T20 Compose stack; docker ps contains no T20-owned container names.",
		"networks":    "docker network ls contains no quoin-t20-owned network.",
		"volumes":     "docker volume ls has no volume labelled com.docker.compose.project=quoin-t20.",
		"images":      "docker image ls contains no quoin-t20-owned image.",
		"hostHelpers": "TLS proxy, readiness server and temporary T20 E2E stack directory are absent.",
		"credentials": "temporary admin credentials lived only in the deleted E2E stack directory and were never added to evidence.",
		"artifacts":   "no browser traces, screenshots, videos, RFB frames, or profile contents were retained.",
		"checkedAt":   time.Now().UTC().Format(time.RFC3339),
	}
	writeJSON(t, cleanupPath, cleanup)
	runtimeEvidence["rawEvidence"] = evidenceDigests(t, evidenceDir)
	writeJSON(t, filepath.Join(evidenceDir, "runtime-evidence.json"), runtimeEvidence)
}

func testBootstrapFailureCleanup(t *testing.T, evidenceDir string) {
	t.Helper()
	command := exec.Command("bash", "test/e2e/compose/server.sh")
	command.Dir = repoRoot(t)
	command.Env = append(acceptanceEnvironment(evidenceDir, true), "QUOIN_T20_TEST_BOOTSTRAP_FAILURE=1", "QUOIN_T20_TEST_FAILURE_ARTIFACT=1")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err == nil {
		t.Fatal("intentional T20 bootstrap failure unexpectedly succeeded")
	} else if status, ok := err.(*exec.ExitError); !ok || status.ExitCode() != 97 {
		t.Fatalf("intentional T20 bootstrap failure exited unexpectedly: %v\n%s", err, output.String())
	}
	assertT20ResourcesCleaned(t)
	if _, err := os.Stat(filepath.Join(repoRoot(t), ".artifacts", "e2e-stack-t20", "playwright-output", "error-context.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("T20 bootstrap teardown retained a page-derived failure artifact: %v", err)
	}
	t.Log("T20 bootstrap-failure cleanup: intentional exit=97; no owned containers, networks, volumes, images, host helpers, stack directory, or failure page artifact remained")
}

func assertT20ResourcesCleaned(t *testing.T) {
	t.Helper()
	remaining := commandOutput(t, "docker", "ps", "-a", "--format", "{{.Names}}")
	for _, owned := range []string{"quoin-t20-", "quoin-t20-auth-fixture"} {
		if strings.Contains(remaining, owned) {
			t.Fatalf("T20 cleanup left owned container: %s", owned)
		}
	}
	if networks := strings.TrimSpace(commandOutput(t, "docker", "network", "ls", "--filter", "name=quoin-t20", "--format", "{{.Name}}")); networks != "" {
		t.Fatalf("T20 cleanup left owned network(s): %s", networks)
	}
	if volumes := strings.TrimSpace(commandOutput(t, "docker", "volume", "ls", "--filter", "label=com.docker.compose.project=quoin-t20", "--format", "{{.Name}}")); volumes != "" {
		t.Fatalf("T20 cleanup left owned volume(s): %s", volumes)
	}
	imageRows := strings.Fields(commandOutput(t, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}"))
	for _, image := range imageRows {
		if strings.HasPrefix(image, "quoin-t20/") {
			t.Fatalf("T20 cleanup left owned image: %s", image)
		}
	}
	if processes := commandOutput(t, "ps", "-eo", "args"); strings.Contains(processes, ".artifacts/e2e-stack-t20/tls-proxy.mjs") || strings.Contains(processes, ".artifacts/e2e-stack-t20/ready.py") {
		t.Fatal("T20 cleanup left a TLS proxy or readiness server running")
	}
	if _, err := os.Stat(filepath.Join(repoRoot(t), ".artifacts", "e2e-stack-t20")); !os.IsNotExist(err) {
		t.Fatalf("T20 cleanup left E2E stack directory: %v", err)
	}
}

func resetEvidenceDir(t *testing.T, evidenceDir string) {
	t.Helper()
	root := repoRoot(t)
	wanted := filepath.Join(root, ".artifacts", "tickets", "T20")
	actual, err := filepath.Abs(evidenceDir)
	if err != nil {
		t.Fatal(err)
	}
	if actual != wanted {
		t.Fatalf("T20 evidence directory must be %s, got %s", wanted, actual)
	}
	if err := os.RemoveAll(actual); err != nil {
		t.Fatalf("remove prior T20 evidence: %v", err)
	}
	if err := os.MkdirAll(actual, 0o755); err != nil {
		t.Fatalf("create T20 evidence: %v", err)
	}
}

func acceptanceEnvironment(evidenceDir string, browserRun bool) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "QUOIN_TICKET=") && !strings.HasPrefix(value, "QUOIN_EVIDENCE_DIR=") {
			env = append(env, value)
		}
	}
	if browserRun {
		return append(env, "QUOIN_TICKET=T20", "QUOIN_EVIDENCE_DIR="+evidenceDir)
	}
	return append(env, "QUOIN_TICKET=", "QUOIN_EVIDENCE_DIR=")
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

func commandOutput(t *testing.T, command string, args ...string) string {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v", command, strings.Join(args, " "), err)
	}
	return string(out)
}

func evidenceFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("required T20 evidence %s: %v", path, err)
	}
	if len(body) == 0 {
		t.Fatalf("required T20 evidence %s is empty", path)
	}
	return string(body)
}

func evidenceDigests(t *testing.T, evidenceDir string) map[string]string {
	t.Helper()
	// This test owns a fresh directory. A fixed allowlist makes accidental
	// debug captures (screenshots, page titles, window dumps, or prior runs)
	// fail closed instead of being silently hash-endorsed as T20 evidence.
	allowed := map[string]bool{
		"verify-contracts.log": true, "go-test-all.log": true, "go-vet.log": true,
		"pnpm-install.log": true, "web-typecheck.log": true, "web-lint.log": true,
		"web-test.log": true, "web-build.log": true, "probe-outcome-matrix.log": true,
		"ticket20-playwright.log": true, "playwright-server.log": true,
		"runtime-process.log": true, "t20-components.log": true,
		"t20-browser-observations.json": true, "cleanup.json": true,
	}
	digests := map[string]string{}
	err := filepath.WalkDir(evidenceDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Base(path) == "runtime-evidence.json" {
			return nil
		}
		relative, err := filepath.Rel(evidenceDir, path)
		if err != nil {
			return err
		}
		if !allowed[relative] {
			return fmt.Errorf("unexpected T20 evidence file %q", relative)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digests[relative] = sha256Text(string(body))
		return nil
	})
	if err != nil {
		t.Fatalf("digest T20 raw evidence: %v", err)
	}
	for path := range allowed {
		if _, exists := digests[path]; !exists {
			t.Fatalf("required T20 evidence file %q is missing", path)
		}
	}
	return digests
}

func sha256Text(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// workspaceContentDigest binds the evidence to every current source byte:
// committed-delta patches plus content and path of each untracked source file.
// Unlike `git status --porcelain`, editing an already-modified file changes it.
func workspaceContentDigest(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	patch := commandOutput(t, "git", "diff", "--binary", "HEAD")
	untracked := strings.Fields(commandOutput(t, "git", "ls-files", "--others", "--exclude-standard"))
	sort.Strings(untracked)
	hash := sha256.New()
	_, _ = hash.Write([]byte("patch\x00" + patch + "\x00untracked\x00"))
	for _, path := range untracked {
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read untracked source %s: %v", path, err)
		}
		_, _ = hash.Write([]byte(path + "\x00"))
		_, _ = hash.Write(body)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
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
