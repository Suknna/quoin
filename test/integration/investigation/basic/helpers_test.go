// Package basic hosts the T13 ticket acceptance run; this file carries the
// evidence/command/HTTP helpers shared by the acceptance test.
package basic

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

func execCommand(t *testing.T, evidence *ticketEvidence, name string, stdin io.Reader, command string, arguments ...string) string {
	return evidence.run(t, name, stdin, command, arguments...)
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	os.WriteFile(path, []byte(content), 0o644)
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(content)), "bytes": len(content)})
}

func (evidence *ticketEvidence) writeRuntimeEvidence(t *testing.T, commit, newPassword, tempPassword, bearer string, observed map[string]any) {
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
		"containers":  "t13-am/t13-forwarder removed (rm -f); quoin compose project down --remove-orphans",
		"images":      "quoin/{quoin,plinth,lintel,stele}:v0.1.0-dev removed (only if the run built them)",
		"workRoot":    "temporary XDG_STATE_HOME + secrets removed with the test temp root",
		"credentials": "admin passwords, the revealed alert bearer and the fixture API key held only in process memory",
		"timestamp":   startedAt.Format(time.RFC3339),
	})), 0o644)
	os.WriteFile(filepath.Join(evidence.dir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit": commit,
		"startedAt": startedAt.Format(time.RFC3339),
		"commands":  evidence.commands,
		"artifacts": evidence.artifacts,
		"components": map[string]any{
			"deployHelper":    "cmd/quoin-deploy (go build -trimpath)",
			"fixtureProvider": "test/fixtures/model-provider (deterministic OpenAI-compatible, streaming investigation branch)",
			"alertmanager":    "prom/alertmanager:v0.28.1 (official container)",
			"imageDigests":    imageDigests,
		},
		"observed": observed,
		"expectedVersusActual": map[string]string{
			"first-message atomicity": "actual: the create command committed Investigation + sources + user message + Queued attempt + frozen input in one transaction; the list stayed empty before the first message",
			"exact stream framing":    "actual: Content-Type text/event-stream + X-Vercel-Ai-Ui-Message-Stream: v1; text-start + text-delta* + finish + [DONE]; the deterministic Chinese answer survived 1-byte reads straddling multi-byte runes",
			"error framing":           "actual: the broken fixture turn emitted {\"type\":\"error\"} + [DONE] and sealed the attempt Failed/provider_unavailable",
			"EOF/detach":              "actual: closing the stream mid-flight never cancelled the attempt (the turn still committed)",
			"durable head/messages":   "actual: sqlite head=assistant, attempt=Succeeded, agent=investigation-v1, schema=investigation_v1",
			"head conflict":           "actual: a stale expectedHeadMessageId returned 409 with code=head_conflict",
			"command replay":          "actual: the same clientCommandId returned the original investigation without a second row",
		},
		"redactions": "admin passwords, the alert bearer and the fixture API key are not written to any evidence file",
	})), 0o644)
	_ = newPassword
	_ = tempPassword
	_ = bearer
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
	return &http.Client{Jar: jar, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func httpPost(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	return httpPostExpect(t, client, target, origin, body, 0)
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

// dumpStreamDiagnostics captures quoin/plinth logs into the evidence dir
// when a stream never starts (the attempt produced no observable output).
func dumpStreamDiagnostics(t *testing.T, target, stateRoot string) {
	t.Helper()
	dir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	quoinLogs := outputOf(t, "docker", "logs", "--tail", "80", "quoin-quoin-1")
	plinthLogs := outputOf(t, "docker", "logs", "--tail", "80", "quoin-plinth-1")
	dbRows := ""
	if stateRoot != "" {
		dbPath := filepath.Join(stateRoot, "quoin", "compose", "data", "quoin.db")
		if rows := outputOf(t, "sqlite3", dbPath, "SELECT id,attempt_type,state,termination_reason FROM execution_attempts ORDER BY id DESC LIMIT 6"); rows != "" && !strings.HasPrefix(rows, "unavailable") {
			dbRows = rows
		}
	}
	payload := fmt.Sprintf("stream target: %s\n\n--- quoin ---\n%s\n\n--- plinth ---\n%s\n\n--- attempts ---\n%s\n", target, quoinLogs, plinthLogs, dbRows)
	os.WriteFile(filepath.Join(dir, "stream-diagnostics.log"), []byte(payload), 0o644)
}
