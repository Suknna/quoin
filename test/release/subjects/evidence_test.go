// Package subjects hosts the T39 acceptance: the release-subject builder and
// offline pre-qualification gate exercised through real buildx builders, a
// real local registry, real Helm OCI pushes and real X.509/DSSE signatures,
// with structured runtime and cleanup evidence under QUOIN_EVIDENCE_DIR.
// Tests skip unless QUOIN_EVIDENCE_DIR is set so `go test ./...` stays cheap
// in ordinary development.
package subjects_test

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

type evidence struct {
	t         *testing.T
	dir       string
	commands  []commandRecord
	artifacts map[string]string
	gitCommit string
	startedAt time.Time
}

type commandRecord struct {
	Name     string   `json:"name"`
	Argv     []string `json:"argv"`
	ExitCode int      `json:"exitCode"`
	Duration string   `json:"duration"`
	Log      string   `json:"log"`
	SHA256   string   `json:"logSha256"`
}

func newEvidence(t *testing.T, dir string) *evidence {
	t.Helper()
	commit, err := exec.Command("git", "-C", repoRoot(t), "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	recorder := &evidence{t: t, dir: dir, gitCommit: strings.TrimSpace(string(commit)), startedAt: time.Now(), artifacts: map[string]string{}}
	status, err := exec.Command("git", "-C", repoRoot(t), "status", "--short").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	dirty := sha256.Sum256(status)
	recorder.observe("git-state.json", map[string]any{
		"commit":       recorder.gitCommit,
		"dirty":        strings.TrimSpace(string(status)) != "",
		"statusSha256": hex.EncodeToString(dirty[:]),
	})
	return recorder
}

// tolerantRemove is the failure-path safety net: removing an already-gone
// resource must never mask the original failure.
func tolerantRemove(argv ...string) {
	_ = exec.Command(argv[0], argv[1:]...).Run()
}

// runTolerant records a command without failing the test on its exit code.
func (recorder *evidence) runTolerant(name string, argv ...string) int {
	recorder.t.Helper()
	started := time.Now()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = repoRoot(recorder.t)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	runErr := command.Run()
	code := 0
	if exit, ok := runErr.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if runErr != nil {
		code = -1
	}
	logPath := filepath.Join(recorder.dir, name+".log")
	os.WriteFile(logPath, output.Bytes(), 0o644)
	sum := sha256.Sum256(output.Bytes())
	recorder.commands = append(recorder.commands, commandRecord{
		Name: name, Argv: argv, ExitCode: code,
		Duration: time.Since(started).Round(time.Millisecond).String(),
		Log:      logPath, SHA256: hex.EncodeToString(sum[:]),
	})
	return code
}

// run executes one command, records exit code, output and the output digest,
// and fails the test when the exit code deviates from the expectation.
func (recorder *evidence) run(name string, stdin *strings.Reader, wantExit int, argv ...string) string {
	recorder.t.Helper()
	started := time.Now()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = repoRoot(recorder.t)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if stdin != nil {
		command.Stdin = stdin
	}
	runErr := command.Run()
	code := 0
	if exit, ok := runErr.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if runErr != nil {
		code = -1
	}
	logPath := filepath.Join(recorder.dir, name+".log")
	if err := os.WriteFile(logPath, output.Bytes(), 0o644); err != nil {
		recorder.t.Fatal(err)
	}
	sum := sha256.Sum256(output.Bytes())
	recorder.commands = append(recorder.commands, commandRecord{
		Name: name, Argv: argv, ExitCode: code,
		Duration: time.Since(started).Round(time.Millisecond).String(),
		Log:      logPath, SHA256: hex.EncodeToString(sum[:]),
	})
	fmt.Printf("[%s] exit=%d %.1fs\n", name, code, time.Since(started).Seconds())
	if wantExit == 0 && code != 0 {
		recorder.t.Fatalf("%s exited %d (want 0):\n%s", name, code, tail(output.String(), 40))
	}
	if wantExit != 0 && code != wantExit {
		recorder.t.Fatalf("%s exited %d (want %d):\n%s", name, code, wantExit, tail(output.String(), 40))
	}
	return output.String()
}

// output runs a command purely to capture its trimmed stdout.
func (recorder *evidence) output(argv ...string) string {
	recorder.t.Helper()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = repoRoot(recorder.t)
	output, err := command.Output()
	if err != nil {
		recorder.t.Fatalf("%v failed: %v", argv, err)
	}
	return strings.TrimSpace(string(output))
}

// observe writes one structured observation document with its digest.
func (recorder *evidence) observe(name string, document any) {
	recorder.t.Helper()
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		recorder.t.Fatal(err)
	}
	path := filepath.Join(recorder.dir, name)
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		recorder.t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	recorder.artifacts[name] = hex.EncodeToString(sum[:])
}

func (recorder *evidence) writeRuntimeEvidence(document map[string]any) {
	document["commands"] = recorder.commands
	for name, digest := range recorder.artifacts {
		if document["artifactDigests"] == nil {
			document["artifactDigests"] = map[string]string{}
		}
		document["artifactDigests"].(map[string]string)[name] = digest
	}
	document["gitCommit"] = recorder.gitCommit
	document["startedAt"] = recorder.startedAt.UTC().Format(time.RFC3339)
	document["finishedAt"] = time.Now().UTC().Format(time.RFC3339)
	recorder.observe("runtime-evidence.json", document)
}

func repoRoot(t *testing.T) string {
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

func tail(text string, lines int) string {
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(all) <= lines {
		return text
	}
	return strings.Join(all[len(all)-lines:], "\n")
}
