package publish

// Shared T42 acceptance recorder and environment helpers, mirroring the
// T39/T40 evidence contract: every command, artifact and observation
// lands under the ticket evidence root with digests, secrets are scanned,
// and owned resources are proven removed.

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
	"testing"
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

// run executes one command in the repository root, records exit code and
// combined output, and fails the test on exit-code mismatch (wantExit -1
// accepts any code).
func (recorder *ticketEvidence) run(t *testing.T, name string, env []string, wantExit int, argv ...string) string {
	t.Helper()
	logPath := filepath.Join(recorder.dir, name+".log")
	started := time.Now()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = repoRoot()
	if env != nil {
		command.Env = env
	}
	var combined bytes.Buffer
	command.Stdout, command.Stderr = &combined, &combined
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
	fmt.Printf("t42: %s exit=%d %.1fs\n", name, exitCode, time.Since(started).Seconds())
	if wantExit >= 0 && exitCode != wantExit {
		t.Fatalf("%s exited %d (want %d):\n%s", name, exitCode, wantExit, tailOf(combined.String(), 40))
	}
	return combined.String()
}

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

func tailOf(text string, lines int) string {
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(all) <= lines {
		return text
	}
	return strings.Join(all[len(all)-lines:], "\n")
}

// requireNativeCell42 proves the executing environment is the native
// Linux closure cell: docker reachable, the emulation handler present
// (arm64 builds are emulated build evidence), and the OCI/archive
// toolchain (skopeo and zstd) available.
func requireNativeCell42(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/ns"); err != nil {
		t.Skipf("closure runs inside the native Linux cell (Linux required): %v", err)
	}
	for _, tool := range []string{"docker", "go", "git", "bash", "skopeo", "zstd"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable in the closure cell: %v", tool, err)
		}
	}
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/qemu-aarch64"); err != nil {
		t.Skipf("linux/arm64 build emulation is not available (enable binfmt, e.g. docker run --privileged tonistiigi/binfmt --install arm64): %v", err)
	}
	if output, err := exec.Command("docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}").Output(); err != nil {
		t.Skipf("docker server unreachable (run inside the native cell with the docker socket): %v", err)
	} else {
		t.Logf("closure cell docker server: %s", strings.TrimSpace(string(output)))
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

func dockerOutput(arguments ...string) string {
	output, err := exec.Command("docker", arguments...).Output()
	if err != nil {
		return ""
	}
	return string(output)
}

// scanTree fails with the leaking path when a sentinel appears in the
// evidence tree.
func scanTree(root, sentinel string) string {
	if sentinel == "" {
		return ""
	}
	var leaked []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(body), sentinel) {
			leaked = append(leaked, path)
		}
		return nil
	})
	if len(leaked) != 0 {
		return strings.Join(leaked, ", ")
	}
	return ""
}

func removeDocker(name string) {
	_ = exec.Command("docker", "rm", "-f", name).Run()
}

func removeDockerNetwork(name string) {
	_ = exec.Command("docker", "network", "rm", name).Run()
}

// httpReady polls one URL until it answers with any status.
func httpReady(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		if response, err := client.Get(url); err == nil {
			response.Body.Close()
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}
