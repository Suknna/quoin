package thanos

// Package thanos holds the T11 acceptance harness (TestTicket11 plus its
// evidence/helper surface): this file owns the read-only helpers, the
// evidence runner and the cleanup/disposition accounting, split out of
// ticket11_test.go by domain responsibility.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

func thanosHits(t *testing.T) map[string]int {
	t.Helper()
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/hits", thanosPort))
	if err != nil {
		t.Fatalf("hits request: %v", err)
	}
	defer response.Body.Close()
	var body struct {
		Queries map[string]int `json:"queries"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("hits parse: %v", err)
	}
	return body.Queries
}

func waitForOccurrence(t *testing.T, client *http.Client, base, origin, alertname string) string {
	t.Helper()
	var snapshot struct {
		Items []struct {
			ID     string            `json:"id"`
			Labels map[string]string `json:"labels"`
		} `json:"items"`
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		body := httpGet(t, client, base+"/api/v1/alerts", origin)
		if err := json.Unmarshal([]byte(body), &snapshot); err == nil {
			for _, item := range snapshot.Items {
				if item.Labels["alertname"] == alertname {
					return item.ID
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s occurrence never appeared", alertname)
	return ""
}

func waitForTerminalAnalysis(t *testing.T, client *http.Client, url, origin string) string {
	t.Helper()
	deadline := time.Now().Add(180 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		last = httpGet(t, client, url, origin)
		if strings.Contains(last, `"state":"Succeeded"`) || strings.Contains(last, `"state":"Failed"`) || strings.Contains(last, `"state":"Cancelled"`) {
			return last
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("analysis never reached a terminal state; last:\n%s", last)
	return ""
}

// evidenceIDsOf extracts the sealed output's evidence ids from the
// analysis detail projection.
func evidenceIDsOf(t *testing.T, client *http.Client, base, origin, detailBody string) ([]string, error) {
	t.Helper()
	var detail struct {
		Output *struct {
			EvidenceIDs []string `json:"evidenceIds"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(detailBody), &detail); err != nil {
		return nil, err
	}
	if detail.Output == nil {
		return nil, fmt.Errorf("analysis has no output")
	}
	return detail.Output.EvidenceIDs, nil
}

type downloadResult struct {
	status  int
	headers http.Header
	body    string
}

func downloadArtifact(t *testing.T, client *http.Client, url, origin string) downloadResult {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return downloadResult{status: response.StatusCode, headers: response.Header, body: string(body)}
}

func logTail(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(body)
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

func (evidence *ticketEvidence) note(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(body)), "bytes": len(body)})
}

func (evidence *ticketEvidence) writeRuntimeEvidence(t *testing.T, commit, dirtyDigest string, observed map[string]any) {
	t.Helper()
	body := mustJSON(t, map[string]any{
		"gitCommit":        commit,
		"dirtyStateDigest": dirtyDigest,
		"startedAt":        time.Now().Format(time.RFC3339),
		"commands":         evidence.commands,
		"artifacts":        evidence.artifacts,
		"components": map[string]any{
			"stack":    "real compose install (quoin/plinth/lintel/stele) + prom/alertmanager:v0.28.1 + python forwarder",
			"provider": "deterministic fixture (test/fixtures/model-provider, fixture-chat-thanos loop: thanos_query -> artifact_read -> final text)",
			"thanos":   "deterministic Prometheus-compatible target (test/fixtures/thanos-query) with a per-query hit counter",
		},
		"expectedVersusActual": observed,
		"redactions":           "admin passwords, the revealed bearer and the provider API key are scanned out of the evidence directory",
	})
	path := filepath.Join(evidence.dir, "runtime-evidence.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func execCommand(t *testing.T, evidence *ticketEvidence, name string, stdin io.Reader, command string, arguments ...string) {
	t.Helper()
	evidence.run(t, name, stdin, command, arguments...)
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; T11 acceptance run disabled")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func outputOf(t *testing.T, command string, arguments ...string) string {
	t.Helper()
	cmd := exec.Command(command, arguments...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", command, arguments, err, out)
	}
	return string(out)
}

func cookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func httpGet(t *testing.T, client *http.Client, url, origin string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode >= 300 {
		t.Fatalf("GET %s: %d %s", url, response.StatusCode, string(body))
	}
	return string(body)
}

func httpPost(t *testing.T, client *http.Client, url, origin, body string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode >= 300 {
		t.Fatalf("POST %s: %d %s", url, response.StatusCode, string(responseBody))
	}
	return string(responseBody)
}

func httpPut(t *testing.T, client *http.Client, url, origin, body string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode >= 300 {
		t.Fatalf("PUT %s: %d %s", url, response.StatusCode, string(responseBody))
	}
	return string(responseBody)
}

func randomSecret(t *testing.T, length int) string {
	t.Helper()
	body := make([]byte, length)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scanForSecrets(t *testing.T, dir string, secrets ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		for _, secret := range secrets {
			if secret != "" && strings.Contains(string(body), secret) {
				t.Fatalf("secret leaked into %s", entry.Name())
			}
		}
	}
}
