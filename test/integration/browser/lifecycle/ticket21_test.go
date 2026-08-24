package lifecycle

// TestTicket21 is the replayable real-process acceptance entrypoint for the
// Browser Operation lifecycle. The browser command uses the isolated T20
// Compose fixture because it is the repository's only real Lintel Chromium /
// Xvfb / x0vncserver stack; all T21 evidence remains under .artifacts/T21.

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

func TestTicket21(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T21 real-process acceptance is disabled")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skipf("pnpm unavailable: %v", err)
	}
	resetEvidenceDirectory(t, evidenceDir)
	sourceBefore := workspaceDigest(t)
	commands := make([]commandEvidence, 0, 8)
	run := func(name, command string, args ...string) {
		t.Helper()
		started := time.Now()
		cmd := exec.Command(command, args...)
		cmd.Dir = repositoryRoot(t)
		// The child full test suite must not recursively execute this acceptance
		// entrypoint. Only Playwright receives its T21 evidence directory.
		cmd.Env = acceptanceEnv(evidenceDir, name == "ticket21-playwright")
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
		logPath := filepath.Join(evidenceDir, name+".log")
		if writeErr := os.WriteFile(logPath, output.Bytes(), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		digest := sha256.Sum256(output.Bytes())
		commands = append(commands, commandEvidence{Name: name, ExitCode: exitCode, Duration: time.Since(started).Round(time.Millisecond).String(), Log: logPath, SHA256: hex.EncodeToString(digest[:])})
		if err != nil {
			t.Fatalf("%s exited %d:\n%s", name, exitCode, output.String())
		}
	}

	run("verify-contracts", "go", "run", "./ci/contracts/verify")
	run("go-test-all", "go", "test", "./...")
	run("go-vet", "go", "vet", "./...")
	run("pnpm-install", "pnpm", "--dir", "web", "install", "--frozen-lockfile")
	run("web-typecheck", "pnpm", "--dir", "web", "typecheck")
	run("web-lint", "pnpm", "--dir", "web", "lint")
	run("web-test", "pnpm", "--dir", "web", "test")
	run("web-build", "pnpm", "--dir", "web", "build")
	// QUOIN_TICKET=T20 deliberately selects the existing isolated real-browser
	// fixture, while the grep runs only the T21 lifecycle scenario and writes
	// no page, RFB, trace, screenshot, or credential content to evidence.
	run("ticket21-playwright", "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-21", "--project=chromium")

	sourceAfter := workspaceDigest(t)
	if sourceBefore != sourceAfter {
		t.Fatalf("verified source changed during T21 acceptance: before=%s after=%s", sourceBefore, sourceAfter)
	}
	observations := requiredEvidence(t, filepath.Join(evidenceDir, "t21-lifecycle-observations.json"))
	if !strings.Contains(observations, "WaitingForCapacity") || !strings.Contains(observations, "Running") {
		t.Fatalf("lifecycle evidence lacks the required FIFO transition: %s", observations)
	}
	assertT21Cleanup(t)
	cleanupPath := filepath.Join(evidenceDir, "cleanup.json")
	cleanup := map[string]any{}
	if body, err := os.ReadFile(cleanupPath); err == nil {
		if err := json.Unmarshal(body, &cleanup); err != nil {
			t.Fatalf("decode Playwright cleanup evidence: %v", err)
		}
	}
	cleanup["ticket21Verification"] = map[string]string{
		"containers": "no quoin-t20-owned container remains after the reused isolated browser stack teardown",
		"networks":   "no quoin-t20-owned network remains after lifecycle cancellation and teardown",
		"processes":  "no T20 TLS proxy or readiness helper remains",
		"artifacts":  "no browser trace, screenshot, video, RFB frame, profile content, cookie, or credential was retained",
	}
	writeJSON(t, cleanupPath, cleanup)
	writeJSON(t, filepath.Join(evidenceDir, "runtime-evidence.json"), map[string]any{
		"gitCommit":          strings.TrimSpace(commandOutput(t, "git", "rev-parse", "HEAD")),
		"sourceDigestBefore": sourceBefore,
		"sourceDigestAfter":  sourceAfter,
		"commands":           commands,
		"realRuntime":        "Quoin gRPC Runtime control stream → Lintel Manager → Chromium/Xvfb/x0vncserver",
		"expectedVersusActual": map[string]string{
			"capacity-one-fifo":  "the second real manual-login operation reached WaitingForCapacity while the first was Running, then reached Running only after the first cancellation's typed physical cleanup acknowledgement",
			"capacity-authority": "the test is driven through public HTTP against a real Lintel; it cannot pass by replacing the browser manager with an in-process mock",
			"cleanup":            "both operations are cancelled and the fixture teardown proves no owned runtime or host helper survives",
		},
		"rawEvidence": evidenceDigests(t, evidenceDir),
	})
}

func acceptanceEnv(evidenceDir string, playwright bool) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "QUOIN_TICKET=") && !strings.HasPrefix(value, "QUOIN_EVIDENCE_DIR=") {
			env = append(env, value)
		}
	}
	if playwright {
		return append(env, "QUOIN_TICKET=T20", "QUOIN_EVIDENCE_DIR="+evidenceDir)
	}
	return append(env, "QUOIN_TICKET=", "QUOIN_EVIDENCE_DIR=")
}

func resetEvidenceDirectory(t *testing.T, evidenceDir string) {
	t.Helper()
	wanted := filepath.Join(repositoryRoot(t), ".artifacts", "tickets", "T21")
	actual, err := filepath.Abs(evidenceDir)
	if err != nil || actual != wanted {
		t.Fatalf("T21 evidence directory must be %s, got %s (err=%v)", wanted, actual, err)
	}
	if err := os.RemoveAll(actual); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actual, 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertT21Cleanup(t *testing.T) {
	t.Helper()
	if containers := commandOutput(t, "docker", "ps", "-a", "--format", "{{.Names}}"); strings.Contains(containers, "quoin-t20-") {
		t.Fatalf("T21 cleanup left a browser fixture container: %s", containers)
	}
	if networks := strings.TrimSpace(commandOutput(t, "docker", "network", "ls", "--filter", "name=quoin-t20", "--format", "{{.Name}}")); networks != "" {
		t.Fatalf("T21 cleanup left browser fixture network(s): %s", networks)
	}
	if processes := commandOutput(t, "ps", "-eo", "args"); strings.Contains(processes, ".artifacts/e2e-stack-t20/tls-proxy.mjs") || strings.Contains(processes, ".artifacts/e2e-stack-t20/ready.py") {
		t.Fatal("T21 cleanup left a TLS proxy or readiness helper")
	}
}

func requiredEvidence(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 {
		t.Fatalf("required T21 evidence %s: %v", path, err)
	}
	return string(body)
}

func evidenceDigests(t *testing.T, evidenceDir string) map[string]string {
	t.Helper()
	allowed := map[string]bool{
		"verify-contracts.log": true, "go-test-all.log": true, "go-vet.log": true,
		"pnpm-install.log": true, "web-typecheck.log": true, "web-lint.log": true,
		"web-test.log": true, "web-build.log": true, "ticket21-playwright.log": true,
		"playwright-server.log": true, "runtime-process.log": true, "t20-components.log": true, "cleanup.json": true, "t21-lifecycle-observations.json": true,
	}
	digests := map[string]string{}
	err := filepath.WalkDir(evidenceDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(path) == "runtime-evidence.json" {
			return err
		}
		relative, err := filepath.Rel(evidenceDir, path)
		if err != nil || !allowed[relative] {
			return fmt.Errorf("unexpected T21 evidence file %q", relative)
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
			t.Fatalf("required T21 evidence file missing: %s", name)
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
