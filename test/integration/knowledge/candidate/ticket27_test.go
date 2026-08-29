// Package candidate runs T27's opt-in real process, Runtime and UI acceptance leg.
package candidate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type commandEvidence struct {
	Name      string   `json:"name"`
	Argv      []string `json:"argv"`
	ExitCode  int      `json:"exitCode"`
	Log       string   `json:"log"`
	SHA256    string   `json:"sha256"`
	StartedAt string   `json:"startedAt"`
	EndedAt   string   `json:"endedAt"`
}

// TestTicket27 delegates stack ownership to the established Playwright
// harness, preserves its raw output, and rejects missing observation and
// cleanup evidence. It never writes product tables or supplies provider
// credentials.
func TestTicket27(t *testing.T) {
	dir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if dir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T27 real-process acceptance is disabled")
	}
	for _, binary := range []string{"docker", "pnpm"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s unavailable: %v", binary, err)
		}
	}
	repo := root(t)
	actual, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if actual != filepath.Join(repo, ".artifacts", "tickets", "T27") {
		t.Fatalf("T27 evidence directory must be canonical: %s", actual)
	}
	// Each real-process run owns a fresh evidence set; reusing an earlier
	// run's cleanup or observation file would make the attestation
	// ambiguous.
	if err := os.RemoveAll(actual); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	before := sourceDigest(t, repo)
	commands := []commandEvidence{}
	writeManifest := func() {
		var observed map[string]any
		if body, err := os.ReadFile(filepath.Join(actual, "t27-knowledge-observations.json")); err == nil {
			_ = json.Unmarshal(body, &observed)
		}
		body, _ := json.MarshalIndent(map[string]any{
			"gitCommit":           strings.TrimSpace(output(t, repo, "git", "rev-parse", "HEAD")),
			"sourceDigestBefore":  before,
			"sourceDigestAfter":   sourceDigest(t, repo),
			"commands":            commands,
			"acceptanceExitCodes": readExitCodes(t, actual),
			"observations":        observed,
			"rawEvidence":         evidenceDigests(t, actual),
		}, "", "  ")
		_ = os.WriteFile(filepath.Join(actual, "runtime-evidence.json"), append(body, '\n'), 0o644)
	}
	defer writeManifest()
	run := func(name string, ticketEnv bool, argv ...string) {
		started := time.Now().UTC()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = repo
		cmd.Env = filteredEnv()
		if ticketEnv {
			cmd.Env = append(cmd.Env, "QUOIN_TICKET=T27", "QUOIN_EVIDENCE_DIR="+actual)
		}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		runErr := cmd.Run()
		ended := time.Now().UTC()
		code := 0
		if exit, ok := runErr.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if runErr != nil {
			code = -1
		}
		log := filepath.Join(actual, name+".log")
		if err := os.WriteFile(log, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(out.Bytes())
		commands = append(commands, commandEvidence{Name: name, Argv: argv, ExitCode: code, Log: log, SHA256: hex.EncodeToString(sum[:]), StartedAt: started.Format(time.RFC3339Nano), EndedAt: ended.Format(time.RFC3339Nano)})
		writeExitCodes(t, actual, commands)
		writeManifest()
		if runErr != nil {
			t.Fatalf("%s exited %d:\n%s", name, code, out.String())
		}
	}
	// Keep the complete verification matrix and the real browser path in
	// one evidence envelope. Static children deliberately do not inherit
	// the T27 environment, so their own opt-in acceptance legs stay
	// skipped.
	run("verify-contracts", false, "./ci/verify-contracts")
	run("go-test-all", false, "go", "test", "./...", "-count=1")
	run("go-vet", false, "go", "vet", "./...")
	run("web-test", false, "pnpm", "--dir", "web", "test")
	run("web-build", false, "pnpm", "--dir", "web", "build")
	run("ticket27-playwright", true, "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-27", "--project=chromium")
	if before != sourceDigest(t, repo) {
		t.Fatal("verified source changed during T27 acceptance")
	}
	for _, name := range []string{"t27-knowledge-observations.json", "cleanup.json"} {
		if info, err := os.Stat(filepath.Join(actual, name)); err != nil || info.Size() == 0 {
			t.Fatalf("required evidence missing: %s", name)
		}
	}
}

func root(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatal("repository root not found")
		}
		d = parent
	}
}

func output(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	body, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func filteredEnv() []string {
	var kept []string
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "QUOIN_TICKET=") || strings.HasPrefix(entry, "QUOIN_EVIDENCE_DIR=") {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func sourceDigest(t *testing.T, repo string) string {
	t.Helper()
	body := output(t, repo, "git", "status", "--short")
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func writeExitCodes(t *testing.T, dir string, commands []commandEvidence) {
	t.Helper()
	path := filepath.Join(dir, "exit-codes.tsv")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	for _, entry := range commands {
		if _, err := writer.WriteString(entry.Name + "\t" + itoa(entry.ExitCode) + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
}

func readExitCodes(t *testing.T, dir string) map[string]int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "exit-codes.tsv"))
	if err != nil {
		return nil
	}
	codes := map[string]int{}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			continue
		}
		codes[parts[0]] = atoi(parts[1])
	}
	return codes
}

func evidenceDigests(t *testing.T, dir string) map[string]string {
	t.Helper()
	digests := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return digests
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(body)
		digests[entry.Name()] = hex.EncodeToString(sum[:])
	}
	return digests
}

func itoa(value int) string { return strconv.Itoa(value) }

func atoi(value string) int {
	result, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return result
}
