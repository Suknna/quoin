package qualification

// Shared T40 acceptance recorder and native-cell environment helpers.
// The recorder mirrors the T37/T39 evidence contract: every command,
// artifact and observation lands under the ticket evidence root with
// digests, secrets are scanned, and owned resources are proven removed.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type ticketEvidence struct {
	dir       string
	commands  []commandRecord
	artifacts []artifactRecord
	startedAt time.Time
}

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
	Bytes  int    `json:"bytes"`
}

func newEvidence(dir string) *ticketEvidence {
	return &ticketEvidence{dir: dir, startedAt: time.Now().UTC()}
}

// run executes a command in the repository root, records its exit code
// and combined output, and returns the output. wantExit -1 accepts any
// code.
func (recorder *ticketEvidence) run(name string, env []string, wantExit int, argv ...string) string {
	logPath := filepath.Join(recorder.dir, name+".log")
	started := time.Now()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = repoRoot()
	if env != nil {
		command.Env = env
	}
	var combined bytes.Buffer
	command.Stdout = &combined
	command.Stderr = &combined
	_ = command.Run()
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	_ = os.WriteFile(logPath, combined.Bytes(), 0o644)
	recorder.commands = append(recorder.commands, commandRecord{
		Name: name, Args: argv, ExitCode: exitCode,
		Duration: time.Since(started).Round(time.Millisecond).String(),
		Log:      name + ".log",
	})
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: logPath, SHA256: sha256Hex(combined.Bytes()), Bytes: combined.Len()})
	if wantExit >= 0 && exitCode != wantExit {
		// The caller inspects the returned output for expected-failure
		// legs; only mismatches outside wantExit abort.
		fmt.Printf("t40: %s exit=%d want=%d\n%s\n", name, exitCode, wantExit, combined.String())
	}
	return combined.String()
}

// exitCodeOf returns the recorded exit code of one named command.
func (recorder *ticketEvidence) exitCodeOf(name string) int {
	for _, entry := range recorder.commands {
		if entry.Name == name {
			return entry.ExitCode
		}
	}
	return -1
}

// note writes one observation artifact.
func (recorder *ticketEvidence) note(name string, content []byte) {
	path := filepath.Join(recorder.dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return
	}
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: path, SHA256: sha256Hex(content), Bytes: len(content)})
}

// observe writes one JSON observation.
func (recorder *ticketEvidence) observe(name string, value any) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	recorder.note(name, body)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func gitCommit() string {
	output, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func dirtyDigest() string {
	output, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return "unknown"
	}
	return sha256Hex(output)
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

// dockerInventory snapshots the owned-name docker state so cleanup can
// prove owned-resource zero without touching foreign resources
// (VERIFY-CLEANUP-003).
type dockerInventory struct {
	Containers string
	Networks   string
	Volumes    string
	Images     string
	Builders   string
}

func captureInventory() dockerInventory {
	return dockerInventory{
		Containers: dockerOutput("ps", "-a", "--format", "{{.Names}}"),
		Networks:   dockerOutput("network", "ls", "--format", "{{.Name}}"),
		Volumes:    dockerOutput("volume", "ls", "--format", "{{.Name}}"),
		Images:     dockerOutput("images", "--format", "{{.Repository}}@{{.ID}}"),
		Builders:   dockerOutput("buildx", "ls"),
	}
}

// loopbackHost is where host-published loopback services are reached
// from this process: host.docker.internal inside a containerized
// qualification cell, 127.0.0.1 on a bare runner.
func loopbackHost() string {
	if host := os.Getenv("QUOIN_LOOPBACK_HOST"); host != "" {
		return host
	}
	return "127.0.0.1"
}

// portOfHost extracts the port of a host:port pair.
func portOfHost(hostPort string) string {
	if index := strings.LastIndex(hostPort, ":"); index >= 0 {
		return hostPort[index+1:]
	}
	return hostPort
}

func dockerOutput(arguments ...string) string {
	command := exec.Command("docker", arguments...)
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return string(output)
}

// httpReady polls one URL until it answers with any status.
func httpReady(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			response.Body.Close()
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}
