// Package backup runs T32's opt-in real Compose and Chromium acceptance path.
package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type commandRecord struct {
	Argv      []string `json:"argv"`
	ExitCode  int      `json:"exitCode"`
	Log       string   `json:"log"`
	SHA256    string   `json:"sha256"`
	StartedAt string   `json:"startedAt"`
	EndedAt   string   `json:"endedAt"`
}

// TestTicket32 owns no product fixtures. It delegates boot, browser operation,
// physical-volume fault injection and cleanup to the established Playwright
// harness, then closes the evidence envelope with immutable command facts.
func TestTicket32(t *testing.T) {
	dir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if dir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T32 real-process acceptance is disabled")
	}
	for _, name := range []string{"docker", "pnpm"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable: %v", name, err)
		}
	}
	repo := repositoryRoot(t)
	actual, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if actual != filepath.Join(repo, ".artifacts", "tickets", "T32") {
		t.Fatalf("T32 evidence directory must be canonical: %s", actual)
	}
	// The enclosing ticket command owns evidence lifecycle and has already
	// created a fresh canonical directory. Do not remove it here: doing so
	// would erase raw logs from the contract, Go and vet gates that precede
	// this real-process leg.
	if err := os.MkdirAll(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	before := sourceDigest(t, repo)
	commands := []commandRecord{}
	defer func() { writeEvidence(t, actual, before, sourceDigest(t, repo), commands) }()
	run := func(name string, ticket bool, argv ...string) {
		started := time.Now().UTC()
		command := exec.Command(argv[0], argv[1:]...)
		command.Dir = repo
		command.Env = baseEnvironment()
		if ticket {
			command.Env = append(command.Env, "QUOIN_TICKET=T32", "QUOIN_EVIDENCE_DIR="+actual)
		}
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		runErr := command.Run()
		ended := time.Now().UTC()
		code := 0
		if exit, ok := runErr.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if runErr != nil {
			code = -1
		}
		log := filepath.Join(actual, "test-ticket32-"+name+".log")
		if writeErr := os.WriteFile(log, output.Bytes(), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		sum := sha256.Sum256(output.Bytes())
		commands = append(commands, commandRecord{Argv: argv, ExitCode: code, Log: log, SHA256: hex.EncodeToString(sum[:]), StartedAt: started.Format(time.RFC3339Nano), EndedAt: ended.Format(time.RFC3339Nano)})
		if runErr != nil {
			t.Fatalf("%s exited %d:\n%s", name, code, output.String())
		}
	}
	run("verify-contracts", false, "./ci/verify-contracts")
	run("go-test-all", false, "go", "test", "./...", "-count=1")
	run("go-vet", false, "go", "vet", "./...")
	run("pnpm-install", false, "pnpm", "--dir", "web", "install", "--frozen-lockfile")
	run("web-typecheck", false, "pnpm", "--dir", "web", "typecheck")
	run("web-lint", false, "pnpm", "--dir", "web", "lint")
	run("web-test", false, "pnpm", "--dir", "web", "test")
	run("web-build", false, "pnpm", "--dir", "web", "build")
	run("playwright", true, "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-32", "--project=chromium")
	for _, name := range []string{"t32-backup-observations.json", "cleanup.json"} {
		info, statErr := os.Stat(filepath.Join(actual, name))
		if statErr != nil || info.Size() == 0 {
			t.Fatalf("required evidence missing: %s", name)
		}
	}
	if before != sourceDigest(t, repo) {
		t.Fatal("verified source changed during T32 acceptance")
	}
}

func repositoryRoot(t *testing.T) string {
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

func sourceDigest(t *testing.T, repo string) string {
	t.Helper()
	// Hash current bytes of every tracked and untracked non-ignored input, not
	// merely `git status`: modifying an already-dirty tracked file must still
	// invalidate the real-process evidence envelope.
	command := exec.Command("git", "ls-files", "-co", "--exclude-standard", "-z")
	command.Dir = repo
	body, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	paths := strings.Split(strings.TrimSuffix(string(body), "\x00"), "\x00")
	var material bytes.Buffer
	for _, path := range paths {
		if path == "" {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(repo, path))
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		// A tracked file can be deleted by a build replacement while the working
		// tree is intentionally dirty. Keep its path in the digest and hash an
		// empty body: this detects the deletion without failing before acceptance
		// can record the real source state.
		material.WriteString(path)
		material.WriteByte(0)
		if readErr == nil {
			material.Write(content)
		}
		material.WriteByte(0)
	}
	sum := sha256.Sum256(material.Bytes())
	return hex.EncodeToString(sum[:])
}

func baseEnvironment() []string {
	var environment []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "QUOIN_TICKET=") && !strings.HasPrefix(value, "QUOIN_EVIDENCE_DIR=") {
			environment = append(environment, value)
		}
	}
	return environment
}

func writeEvidence(t *testing.T, evidence, before, after string, commands []commandRecord) {
	t.Helper()
	observations := json.RawMessage("null")
	if body, err := os.ReadFile(filepath.Join(evidence, "t32-backup-observations.json")); err == nil {
		observations = body
	}
	body, err := json.MarshalIndent(map[string]any{
		"gitCommit":          strings.TrimSpace(gitOutput(t, "rev-parse", "HEAD")),
		"sourceDigestBefore": before,
		"sourceDigestAfter":  after,
		"commands":           commands,
		"observations":       observations,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "runtime-evidence.json"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	body, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
