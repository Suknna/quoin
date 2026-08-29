// Package retry runs T26's opt-in real process, Runtime, Chromium, and UI acceptance leg.
package retry

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

// TestTicket26 delegates stack ownership to the established Playwright
// harness, preserves its raw output, and rejects missing observation/cleanup
// evidence. It never writes product tables or supplies provider credentials.
func TestTicket26(t *testing.T) {
	dir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if dir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T26 real-process acceptance is disabled")
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
	if actual != filepath.Join(repo, ".artifacts", "tickets", "T26") {
		t.Fatalf("T26 evidence directory must be canonical: %s", actual)
	}
	// Each real-process run owns a fresh evidence set. Reusing an earlier run's
	// cleanup or observation file would make the attestation ambiguous.
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
		if body, err := os.ReadFile(filepath.Join(actual, "t26-inspection-observations.json")); err == nil {
			_ = json.Unmarshal(body, &observed)
		}
		body, _ := json.MarshalIndent(map[string]any{"gitCommit": strings.TrimSpace(output(t, repo, "git", "rev-parse", "HEAD")), "sourceDigestBefore": before, "sourceDigestAfter": sourceDigest(t, repo), "commands": commands, "acceptanceExitCodes": readExitCodes(t, actual), "components": componentIdentity(t, actual), "observations": observed, "events": observed["events"], "rawEvidence": evidenceDigests(t, actual)}, "", "  ")
		_ = os.WriteFile(filepath.Join(actual, "runtime-evidence.json"), append(body, '\n'), 0o644)
	}
	defer writeManifest()
	run := func(name string, ticketEnv bool, argv ...string) {
		started := time.Now().UTC()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = repo
		cmd.Env = filteredEnv()
		if ticketEnv {
			cmd.Env = append(cmd.Env, "QUOIN_TICKET=T26", "QUOIN_EVIDENCE_DIR="+actual)
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
	// Keep the complete specified verification matrix and the real browser path
	// in one evidence envelope. Static children deliberately do not inherit the
	// T26 environment, so their own opt-in acceptance test remains skipped.
	run("verify-contracts", false, "./ci/verify-contracts")
	run("go-test-all", false, "go", "test", "./...", "-count=1")
	run("go-vet", false, "go", "vet", "./...")
	run("web-test", false, "pnpm", "--dir", "web", "test")
	run("web-build", false, "pnpm", "--dir", "web", "build")
	run("ticket26-playwright", true, "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-26", "--project=chromium")
	if before != sourceDigest(t, repo) {
		t.Fatal("verified source changed during T26 acceptance")
	}
	for _, name := range []string{"t26-inspection-observations.json", "cleanup.json"} {
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
		p := filepath.Dir(d)
		if p == d {
			t.Fatal("repository root not found")
		}
		d = p
	}
}
func output(t *testing.T, dir, command string, args ...string) string {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	body, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
func filteredEnv() []string {
	out := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "QUOIN_TICKET=") && !strings.HasPrefix(value, "QUOIN_EVIDENCE_DIR=") {
			out = append(out, value)
		}
	}
	return out
}
func writeExitCodes(t *testing.T, dir string, commands []commandEvidence) {
	t.Helper()
	var lines []string
	for _, command := range commands {
		lines = append(lines, command.Name+"\t"+strconv.Itoa(command.ExitCode))
	}
	if err := os.WriteFile(filepath.Join(dir, "exit-codes.tsv"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write acceptance exit codes: %v", err)
	}
}

func readExitCodes(t *testing.T, dir string) map[string]int {
	t.Helper()
	codes := map[string]int{}
	file, err := os.Open(filepath.Join(dir, "exit-codes.tsv"))
	if err != nil {
		return codes
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 2 {
			continue
		}
		if code, err := strconv.Atoi(fields[1]); err == nil {
			codes[fields[0]] = code
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read acceptance exit codes: %v", err)
	}
	return codes
}

func sourceDigest(t *testing.T, repo string) string {
	t.Helper()
	// The T26 image bootstrap always emits the tracked Web distribution. It is
	// neither product source nor fixture input, and the acceptance's final
	// workspace evidence retains it separately. Every other tracked/untracked
	// source path must remain unchanged during the real-process run.
	diff := output(t, repo, "git", "diff", "--binary", "HEAD", "--", ".", ":(exclude)internal/gen/web/dist/**")
	status := output(t, repo, "git", "status", "--porcelain=v1", "--untracked-files=all")
	status = strings.Join(filterGeneratedWebDistribution(strings.Split(status, "\n")), "\n")
	// git diff has no hunks for untracked files and porcelain only identifies
	// their paths. Hash their bytes too so the dirty-state fence detects a
	// source edit made during the real-process acceptance.
	untracked := output(t, repo, "git", "ls-files", "--others", "--exclude-standard", "-z")
	var untrackedDigest strings.Builder
	for _, path := range strings.Split(untracked, "\x00") {
		if path == "" || strings.HasPrefix(path, "internal/gen/web/dist/") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read untracked source %s: %v", path, err)
		}
		sum := sha256.Sum256(body)
		untrackedDigest.WriteString(path)
		untrackedDigest.WriteByte(0)
		untrackedDigest.WriteString(hex.EncodeToString(sum[:]))
		untrackedDigest.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(diff + "\x00" + status + "\x00" + untrackedDigest.String()))
	return hex.EncodeToString(sum[:])
}

func filterGeneratedWebDistribution(lines []string) []string {
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.Contains(line, "internal/gen/web/dist/") {
			kept = append(kept, line)
		}
	}
	return kept
}
func componentIdentity(t *testing.T, dir string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "t26-components.log"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	components := map[string]string{}
	for index := 0; index+1 < len(lines); index += 2 {
		key := strings.TrimSuffix(strings.TrimSpace(lines[index]), ":")
		value := strings.TrimSpace(lines[index+1])
		if key != "" && value != "" {
			components[key] = value
		}
	}
	return components
}

func evidenceDigests(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(path) == "runtime-evidence.json" {
			return err
		}
		body, err := os.ReadFile(path)
		if err == nil {
			sum := sha256.Sum256(body)
			rel, _ := filepath.Rel(dir, path)
			got[rel] = hex.EncodeToString(sum[:])
		}
		return nil
	})
	return got
}
