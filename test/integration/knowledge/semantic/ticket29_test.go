// Package semantictest runs T29's opt-in real Runtime/Plinth/Chromium
// acceptance for semantic retrieval and source invalidation.
package semantictest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// TestTicket29 preserves raw command, UI observation and cleanup evidence.
// It never obtains provider credentials or writes product tables itself.
// The deterministic drift/cursor/invalidation-race proofs run inside the
// go-test-all leg below; this wrapper adds the real-process path.
func TestTicket29(t *testing.T) {
	dir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if dir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T29 real-process acceptance is disabled")
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
	if actual != filepath.Join(repo, ".artifacts", "tickets", "T29") {
		t.Fatalf("T29 evidence directory must be canonical: %s", actual)
	}
	// The exact acceptance block runs earlier legs before this wrapper; their
	// logs and exit-code rows are retained evidence and must survive (the
	// wrapper only adds its own commands to the same ledger).
	if err := os.MkdirAll(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	before := sourceDigest(t, repo)
	commands := []commandEvidence{}
	writeManifest := func() {
		observations := json.RawMessage("null")
		if body, err := os.ReadFile(filepath.Join(actual, "t29-knowledge-observations.json")); err == nil {
			observations = body
		}
		body, _ := json.MarshalIndent(map[string]any{"gitCommit": strings.TrimSpace(output(t, repo, "git", "rev-parse", "HEAD")), "sourceDigestBefore": before, "sourceDigestAfter": sourceDigest(t, repo), "commands": commands, "observations": observations}, "", "  ")
		_ = os.WriteFile(filepath.Join(actual, "runtime-evidence.json"), append(body, '\n'), 0o644)
	}
	defer writeManifest()
	run := func(name string, ticket bool, argv ...string) {
		started := time.Now().UTC()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = repo
		cmd.Env = filteredEnv()
		if ticket {
			cmd.Env = append(cmd.Env, "QUOIN_TICKET=T29", "QUOIN_EVIDENCE_DIR="+actual)
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
		// The exit-code ledger is shared with the outer acceptance block:
		// wrapper rows land in the same tsv under their prefixed names.
		if ledger, ledgerErr := os.OpenFile(filepath.Join(actual, "exit-codes.tsv"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ledgerErr == nil {
			_, _ = ledger.WriteString(fmt.Sprintf("%s\t%d\n", name, code))
			_ = ledger.Close()
		}
		commands = append(commands, commandEvidence{name, argv, code, log, hex.EncodeToString(sum[:]), started.Format(time.RFC3339Nano), ended.Format(time.RFC3339Nano)})
		writeManifest()
		if runErr != nil {
			t.Fatalf("%s exited %d:\n%s", name, code, out.String())
		}
	}
	// The wrapper is also runnable standalone (QUOIN_EVIDENCE_DIR only), so
	// it re-runs the deterministic legs itself — under wrapper- prefixed
	// names so the exact acceptance block's own same-named logs and exit
	// rows are never overwritten.
	run("wrapper-verify-contracts", false, "./ci/verify-contracts")
	run("wrapper-go-test-all", false, "go", "test", "./...", "-count=1")
	run("wrapper-go-vet", false, "go", "vet", "./...")
	run("wrapper-web-test", false, "pnpm", "--dir", "web", "test")
	run("wrapper-web-build", false, "pnpm", "--dir", "web", "build")
	run("wrapper-ticket29-playwright", true, "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-29", "--project=chromium")
	if before != sourceDigest(t, repo) {
		t.Fatal("verified source changed during T29 acceptance")
	}
	for _, name := range []string{"t29-knowledge-observations.json", "cleanup.json"} {
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
	// The production web build intentionally regenerates the tracked embedded
	// bundle. It is derived output, not acceptance-owned source; every other
	// handwritten path — including the content of already-dirty files and the
	// untracked additions — must stay byte-identical across the run.
	var sourceLines []string
	for _, line := range strings.Split(output(t, repo, "git", "status", "--short"), "\n") {
		if strings.Contains(line, "internal/gen/web/dist/") {
			continue
		}
		sourceLines = append(sourceLines, line)
	}
	sourceLines = append(sourceLines, output(t, repo, "git", "diff", "HEAD", "--", ".", ":(exclude)internal/gen/web/dist"))
	for _, untracked := range strings.Split(strings.TrimSpace(output(t, repo, "git", "ls-files", "--others", "--exclude-standard", "--", ".", ":(exclude)internal/gen/web/dist")), "\n") {
		if untracked == "" {
			continue
		}
		// Hash the content, not just path and size: equal-length edits must
		// still change the digest.
		body, err := os.ReadFile(filepath.Join(repo, untracked))
		if err != nil {
			t.Fatal(err)
		}
		untrackedSum := sha256.Sum256(body)
		sourceLines = append(sourceLines, untracked, hex.EncodeToString(untrackedSum[:]))
	}
	sum := sha256.Sum256([]byte(strings.Join(sourceLines, "\n")))
	return hex.EncodeToString(sum[:])
}
