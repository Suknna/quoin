// Package concurrency hosts the T15 ticket acceptance run: the real
// compose stack (Quoin + registered Plinth + Lintel + Stele) with the
// deterministic fixture model provider, driving head-concurrency, Stop,
// Retry and latest-turn Undo through the real HTTP/stream/SQLite path.
// Evidence lands under .artifacts/tickets/T15/.
package concurrency

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	projectName = "quoin"
	quoinPort   = 18080
	stelePort   = 18081
	// fixturePort is unique to this package: a stale 18443 listener from a
	// prior run must never impersonate this run's fixture.
	fixturePort = 28443
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

func (evidence *ticketEvidence) logCommand(t *testing.T, name string, cmd *exec.Cmd) {
	t.Helper()
	started := time.Now()
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
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	os.WriteFile(path, []byte(content), 0o644)
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(content)), "bytes": len(content)})
}

// writeRuntimeEvidence seals the run: exact commands with exit codes,
// component versions/digests, structured observations and the
// expected-versus-actual assertions for the four required race families.
func (evidence *ticketEvidence) writeRuntimeEvidence(t *testing.T, commit, dirtyDigest string, observed map[string]any, expectedVersusActual map[string]string) {
	t.Helper()
	startedAt := time.Now().UTC()
	imageDigests := map[string]string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		out, err := exec.Command("docker", "image", "inspect", image, "--format", "{{index .RepoDigests 0}}").Output()
		if err == nil {
			imageDigests[image] = strings.TrimSpace(string(out))
		}
	}
	os.WriteFile(filepath.Join(evidence.dir, "cleanup.json"), []byte(mustJSON(t, map[string]any{
		"containers":  "quoin compose project down --remove-orphans; the host fixture provider process killed and reaped by the test",
		"images":      "quoin/{quoin,plinth,lintel,stele}:v0.1.0-dev removed only when this run built them",
		"workRoot":    "temporary XDG_STATE_HOME + secrets removed with the test temp root",
		"credentials": "admin passwords and the fixture API key held only in process memory; nothing written to evidence",
		"timestamp":   startedAt.Format(time.RFC3339),
	})), 0o644)
	os.WriteFile(filepath.Join(evidence.dir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit":           commit,
		"gitDirtyStateDigest": dirtyDigest,
		"startedAt":           startedAt.Format(time.RFC3339),
		"commands":            evidence.commands,
		"artifacts":           evidence.artifacts,
		"components": map[string]any{
			"deployHelper":    "cmd/quoin-deploy (go build -trimpath)",
			"fixtureProvider": "test/fixtures/model-provider (deterministic OpenAI-compatible; slow investigation branch for the race windows, killed and restarted for the retry outage)",
			"imageDigests":    imageDigests,
			"browser":         "chromium acceptance runs separately via `playwright --grep @ticket-15` into this evidence directory (playwright-report)",
		},
		"observed":             observed,
		"expectedVersusActual": expectedVersusActual,
		"redactions":           "admin passwords and the fixture API key are not written to any evidence file",
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
	return &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// httpPostExpect posts and returns the body; when want != 0 the status
// must match exactly (conflict/validation paths).
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

func httpPost(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	return httpPostExpect(t, client, target, origin, body, 0)
}

func httpPut(t *testing.T, client *http.Client, target, origin, body string) string {
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

// httpPostAsync posts without waiting for the response body; the returned
// function collects the final status and body (simultaneous send races).
func httpPostAsync(target, origin, body string, client *http.Client) func() (int, string) {
	type outcome struct {
		status int
		body   string
	}
	results := make(chan outcome, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		response, err := client.Do(request)
		if err != nil {
			results <- outcome{status: -1, body: err.Error()}
			return
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		results <- outcome{status: response.StatusCode, body: string(raw)}
	}()
	return func() (int, string) {
		result := <-results
		return result.status, result.body
	}
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

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}

// frameRecorder captures the `data:` payloads of one ui-message-stream
// response while the race scenario drives the domain commands.
type frameRecorder struct {
	mu     sync.Mutex
	frames []string
	done   chan struct{}
}

func (recorder *frameRecorder) snapshot() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.frames...)
}

func (recorder *frameRecorder) count() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return len(recorder.frames)
}

// recordStream opens one stream and drains it into the recorder until the
// server closes the response (terminal frame + [DONE] or transport end).
func recordStream(t *testing.T, client *http.Client, target, origin, body string, recorder *frameRecorder) {
	t.Helper()
	go func() {
		defer close(recorder.done)
		request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		request.Header.Set("Accept", "text/event-stream")
		response, err := client.Do(request)
		if err != nil {
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			recorder.mu.Lock()
			recorder.frames = append(recorder.frames, fmt.Sprintf("http-%d:%s", response.StatusCode, raw))
			recorder.mu.Unlock()
			return
		}
		buffer := make([]byte, 4096)
		var pending []byte
		for {
			n, readErr := response.Body.Read(buffer)
			if n > 0 {
				pending = append(pending, buffer[:n]...)
			}
			for {
				idx := bytes.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := strings.TrimSuffix(string(pending[:idx]), "\r")
				pending = pending[idx+1:]
				if strings.HasPrefix(line, "data:") {
					recorder.mu.Lock()
					recorder.frames = append(recorder.frames, strings.TrimPrefix(line, "data: "))
					recorder.mu.Unlock()
				}
			}
			if readErr != nil {
				return
			}
		}
	}()
}

// awaitFrames blocks until the recorder observed at least n frames or the
// timeout expires (the race window opens once the first deltas stream).
func awaitFrames(t *testing.T, recorder *frameRecorder, n int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if recorder.count() >= n {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// awaitCondition polls until the probe returns true or the timeout
// expires; the last probe observation is returned for diagnostics.
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
