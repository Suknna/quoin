package ui

// Shared evidence recorder and host probes for the T41 acceptance run,
// following the recorder shape of the T37/T40 acceptance paths: every
// real command is captured with its exit code and log path, every
// observation is digest-bound, and cleanup phases merge into the same
// cleanup.json the Playwright teardown writes (never overwriting it).

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type commandRecord struct {
	Name     string   `json:"name"`
	Args     []string `json:"args"`
	ExitCode int      `json:"exitCode"`
	Duration string   `json:"duration"`
	Log      string   `json:"log"`
}

type artifactRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type ticketEvidence struct {
	dir       string
	commands  []commandRecord
	artifacts []artifactRecord
}

func newEvidence(t *testing.T, dir string) *ticketEvidence {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &ticketEvidence{dir: dir}
}

// run executes a real command, tees combined output into the evidence
// tree and records the exit code; wantExit -1 accepts any code.
func (recorder *ticketEvidence) run(t *testing.T, name string, env []string, wantExit int, argv ...string) int {
	t.Helper()
	started := time.Now()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = repoRoot()
	if env != nil {
		cmd.Env = env
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		exitError, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s failed to run: %v", name, runErr)
		}
		code = exitError.ExitCode()
	}
	logPath := filepath.Join(recorder.dir, name+".log")
	if err := os.WriteFile(logPath, output.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder.commands = append(recorder.commands, commandRecord{
		Name: name, Args: argv, ExitCode: code,
		Duration: time.Since(started).Round(time.Millisecond).String(),
		Log:      logPath,
	})
	if wantExit >= 0 && code != wantExit {
		tail := output.String()
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		t.Fatalf("%s exited %d (want %d)\n%s", name, code, wantExit, tail)
	}
	return code
}

// observe writes a JSON observation into the evidence tree.
func (recorder *ticketEvidence) observe(t *testing.T, name string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(recorder.dir, name)
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// note records a file as a digested evidence artifact.
func (recorder *ticketEvidence) note(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("evidence artifact %s: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	recorder.artifacts = append(recorder.artifacts, artifactRecord{
		Path: path, SHA256: sha256Hex(body), Bytes: info.Size(),
	})
}

// cleanupPhase merges one phase into cleanup.json. The Playwright
// global teardown owns earlier phases of the same file; a Go-side
// overwrite would destroy those dispositions.
type cleanupResource struct {
	Name               string `json:"name"`
	Kind               string `json:"kind"`
	RemovalCommand     string `json:"removalCommand,omitempty"`
	ProbeCommand       string `json:"probeCommand,omitempty"`
	ObservedFinalState string `json:"observedFinalState"`
}

func appendCleanupPhase(t *testing.T, evidenceDir, phase string, resources []cleanupResource, failures []string) error {
	t.Helper()
	path := filepath.Join(evidenceDir, "cleanup.json")
	document := map[string]any{"phases": []any{}}
	if body, err := os.ReadFile(path); err == nil {
		parsed := map[string]any{}
		if json.Unmarshal(body, &parsed) == nil {
			if phases, ok := parsed["phases"].([]any); ok {
				document["phases"] = phases
			}
		}
	}
	phases, _ := document["phases"].([]any)
	phases = append(phases, map[string]any{
		"phase": phase, "resources": resources, "failures": failures,
	})
	document["phases"] = phases
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

// dockerInventory captures the docker resources this acceptance owns so
// the cleanup proof compares against the pre-run baseline and never
// touches foreign (k3s, unrelated) resources.
type dockerInventory struct {
	Containers []string `json:"containers"`
	Networks   []string `json:"networks"`
	Volumes    []string `json:"volumes"`
}

func captureInventory() dockerInventory {
	return dockerInventory{
		Containers: dockerLines("docker", "ps", "-a", "--format", "{{.Names}}"),
		Networks:   dockerLines("docker", "network", "ls", "--format", "{{.Name}}"),
		Volumes:    dockerLines("docker", "volume", "ls", "--format", "{{.Name}}"),
	}
}

func dockerLines(name string, args ...string) []string {
	output, err := exec.Command(name, args...).Output()
	if err != nil {
		return nil
	}
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// ownedResources lists the docker resources this invocation owns: the
// shared compose project of the e2e stack and its three plain-run
// fixtures (the same ownership set teardown.mjs removes).
func ownedResources() (containers, networks, volumes []string) {
	containers = dockerLines("docker", "ps", "-a", "--filter", "label=com.docker.compose.project=quoin", "--format", "{{.Names}}")
	containers = append(containers, dockerLines("docker", "ps", "-a", "--format", "{{.Names}}")...)
	networks = dockerLines("docker", "network", "ls", "--filter", "label=com.docker.compose.project=quoin", "--format", "{{.Name}}")
	volumes = dockerLines("docker", "volume", "ls", "--filter", "label=com.docker.compose.project=quoin", "--format", "{{.Name}}")
	return containers, networks, volumes
}

func sha256Hex(body []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func gitOutput(args ...string) string {
	output, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func gitCommit() string {
	return gitOutput("rev-parse", "HEAD")
}

func dirtyDigest() string {
	return sha256Hex([]byte(gitOutput("status", "--porcelain")))
}
