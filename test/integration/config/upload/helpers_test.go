// Package upload hosts the T16 ticket acceptance run: the real compose
// stack (Quoin + Stele; Plinth/Lintel stay unregistered — the configuration
// path has no runtime dependency yet) proving the strict Business System
// upload/publish path plus the Label Contract draft/zero-system activation
// prerequisite through the real HTTP and SQLite authority. Evidence lands
// under .artifacts/tickets/T16/.
package upload

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	projectName = "quoin"
	quoinPort   = 18080
	stelePort   = 18081
)

type ticketEvidence struct {
	dir       string
	commands  []map[string]any
	artifacts []map[string]any
	env       []string
	stateRoot string
}

func (evidence *ticketEvidence) run(t *testing.T, name string, stdin io.Reader, command string, arguments ...string) string {
	t.Helper()
	started := time.Now()
	cmd := exec.Command(command, arguments...)
	cmd.Env = evidence.env
	cmd.Dir = repoRoot(t)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	exitCode := 0
	if err := cmd.Run(); err != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	logPath := filepath.Join(evidence.dir, name+".log")
	os.WriteFile(logPath, combined.Bytes(), 0o644)
	evidence.commands = append(evidence.commands, map[string]any{"name": name, "exitCode": exitCode, "duration": time.Since(started).Round(time.Millisecond).String()})
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": logPath, "sha256": sha256Hex(combined.Bytes()), "bytes": combined.Len()})
	if exitCode != 0 {
		t.Fatalf("%s: exit=%d output:\n%s", name, exitCode, combined.String())
	}
	return combined.String()
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	os.WriteFile(path, []byte(content), 0o644)
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(content)), "bytes": len(content)})
}

// writeRuntimeEvidence seals the run with the exact commands, image digests
// and the expected-versus-actual assertion map for the frozen acceptance
// families (strict lexical/schema/limit rejections, pointer transactions,
// command replay).
func (evidence *ticketEvidence) writeRuntimeEvidence(t *testing.T, commit, dirtyDigest string, observed map[string]any, expectedVersusActual map[string]string) {
	t.Helper()
	startedAt := time.Now().UTC()
	imageDigests := map[string]string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		out, err := exec.Command("docker", "image", "inspect", image, "--format", "{{index .RepoDigests 0}}").Output()
		if err == nil {
			imageDigests[image] = string(bytes.TrimSpace(out))
		}
	}
	os.WriteFile(filepath.Join(evidence.dir, "cleanup.json"), []byte(mustJSON(t, map[string]any{
		"containers":  "quoin compose project down --remove-orphans after the assertions; teardown log retained",
		"images":      "quoin/{quoin,plinth,lintel,stele}:v0.1.0-dev removed only when this run built them",
		"workRoot":    "temporary XDG_STATE_HOME + install config removed with the test temp root",
		"credentials": "admin passwords held only in process memory; never written to evidence (scanned)",
		"fixtures":    "no external fixtures owned by this run (Plinth/Lintel intentionally unregistered)",
		"timestamp":   startedAt.Format(time.RFC3339),
	})), 0o644)
	os.WriteFile(filepath.Join(evidence.dir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit":           commit,
		"gitDirtyStateDigest": dirtyDigest,
		"startedAt":           startedAt.Format(time.RFC3339),
		"commands":            evidence.commands,
		"artifacts":           evidence.artifacts,
		"components": map[string]any{
			"deployHelper": "cmd/quoin-deploy (go build -trimpath)",
			"imageDigests": imageDigests,
			"browser":      "chromium acceptance runs separately via `playwright --grep @ticket-16` into this evidence directory (playwright-report)",
		},
		"observed":             observed,
		"expectedVersusActual": expectedVersusActual,
		"redactions":           "admin passwords are not written to any evidence file",
	})), 0o644)
}

func scanForSecrets(t *testing.T, evidenceDir string, secrets ...string) {
	t.Helper()
	filepath.Walk(evidenceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, _ := os.ReadFile(path)
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(data, []byte(secret)) {
				t.Fatalf("secret material leaked into %s", path)
			}
		}
		return nil
	})
}

func cookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}
}

func httpPostExpect(t *testing.T, client *http.Client, target, origin, body string, want int) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if want != 0 {
		if response.StatusCode != want {
			t.Fatalf("%s: status=%d want=%d body=%.800s", target, response.StatusCode, want, raw)
		}
		return string(raw)
	}
	if response.StatusCode >= 400 {
		t.Fatalf("%s: status=%d body=%.800s", target, response.StatusCode, raw)
	}
	return string(raw)
}

func httpGet(t *testing.T, client *http.Client, target, origin string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if response.StatusCode >= 400 {
		t.Fatalf("%s: status=%d body=%.800s", target, response.StatusCode, raw)
	}
	return string(raw)
}

// httpPutExpect issues the frozen PUT password change (the auth contract
// uses PUT, unlike the POST commands).
func httpPutExpect(t *testing.T, client *http.Client, target, origin, body string, want int) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPut, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if want != 0 {
		if response.StatusCode != want {
			t.Fatalf("%s: status=%d want=%d body=%.800s", target, response.StatusCode, want, raw)
		}
		return string(raw)
	}
	if response.StatusCode >= 400 {
		t.Fatalf("%s: status=%d body=%.800s", target, response.StatusCode, raw)
	}
	return string(raw)
}

// uploadMultipart posts the strict-YAML multipart command through the real
// upload endpoint and returns (status, body).
func uploadMultipart(t *testing.T, client *http.Client, target, origin string, fields map[string]string, fileField, filename string, document []byte) (int, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile(fileField, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(document); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return response.StatusCode, string(raw)
}

func randomSecret(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(content), 0o644)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}

func outputOf(t *testing.T, command string, arguments ...string) string {
	t.Helper()
	out, err := exec.Command(command, arguments...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	return string(out)
}

// awaitCondition polls until the probe returns true or the timeout expires.
func awaitCondition(timeout time.Duration, probe func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probe() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return probe()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}
