// Package labelcontract hosts the T17 ticket acceptance run: the real
// compose stack (Quoin + Stele) proving the Label Contract activation
// readiness/race/rollback path, the Config Verification Run evidence chain
// and the alert attribution projection with live filter behavior over real
// Stele deliveries. Evidence lands under .artifacts/tickets/T17/.
package labelcontract

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/alerts"
)

const (
	defaultProjectName = "quoin"
	quoinPort          = 18080
	stelePort          = 18081
)

type ticketEvidence struct {
	dir        string
	commands   []map[string]any
	artifacts  []map[string]any
	env        []string
	stateRoot  string
	imageIDs   map[string]string
	dockerPath string
}

type dockerNetworkSnapshot struct {
	Exists      bool
	Attachments []string
}

func parseDockerNetworkSnapshot(exitCode int, output string) (dockerNetworkSnapshot, error) {
	if exitCode != 0 {
		lower := strings.ToLower(output)
		if strings.Contains(lower, "not found") || strings.Contains(lower, "no such network") {
			return dockerNetworkSnapshot{}, nil
		}
		return dockerNetworkSnapshot{}, fmt.Errorf("inspect Docker network: exit=%d output=%s", exitCode, output)
	}
	attachments := strings.Fields(output)
	sort.Strings(attachments)
	return dockerNetworkSnapshot{Exists: true, Attachments: attachments}, nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// dockerProjectShim keeps acceptance stacks isolated without adding a
// production-only deployment option. quoin-deploy intentionally uses the
// stable project name `quoin`; this test-only shim rewrites only that exact
// Compose argument before delegating to the real Docker binary. All other
// It also maps only the four local development tags used by the generated
// Compose projection to this run's namespace, then appends an image override.
// Production deploy behavior and all unrelated Docker calls remain untouched.
func dockerProjectShim(t *testing.T, directory, sourceProject, project, imageNamespace, imageOverride, fixtureRunID string) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	realDocker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "docker")
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
map_image() {
  case "$1" in
    quoin/quoin:v0.1.0-dev|quoin/plinth:v0.1.0-dev|quoin/lintel:v0.1.0-dev|quoin/stele:v0.1.0-dev)
      printf '%%s/%%s' %q "${1#quoin/}"
      ;;
    *) printf '%%s' "$1" ;;
  esac
}
if [[ "$#" -ge 6 && "$1" == "compose" && "$2" == "--project-name" && "$3" == %q && "$4" == "--file" ]]; then
  exec %q compose --project-name %q --file "$5" --file %q "${@:6}"
fi
if [[ "$1" == "build" || "$1" == "rmi" || "$1" == "run" || ( "$1" == "image" && "$#" -ge 3 && "$2" == "inspect" ) ]]; then
  args=("$@")
  for ((index=0; index<${#args[@]}; index++)); do
    if [[ "${args[$index]}" == "-t" || "${args[$index]}" == "--tag" ]]; then
      ((index++))
      args[$index]=$(map_image "${args[$index]}")
    elif [[ "$1" != "build" ]]; then
      args[$index]=$(map_image "${args[$index]}")
    fi
  done
  if [[ "$1" == "build" ]]; then
    context_index=$((${#args[@]} - 1))
    args=("${args[@]:0:$context_index}" --label %q "${args[$context_index]}")
  fi
  exec %q "${args[@]}"
fi
exec %q "$@"
`, imageNamespace, sourceProject, realDocker, project, imageOverride, fixtureRunID, realDocker, realDocker)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return directory
}

func replaceEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func (evidence *ticketEvidence) run(t *testing.T, name string, stdin io.Reader, command string, arguments ...string) string {
	t.Helper()
	started := time.Now()
	executable := command
	if command == "docker" && evidence.dockerPath != "" {
		executable = evidence.dockerPath
	}
	cmd := exec.Command(executable, arguments...)
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
	if err := os.WriteFile(logPath, combined.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	evidence.commands = append(evidence.commands, map[string]any{"name": name, "exitCode": exitCode, "duration": time.Since(started).Round(time.Millisecond).String()})
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": logPath, "sha256": sha256Hex(combined.Bytes()), "bytes": combined.Len()})
	if exitCode != 0 {
		t.Fatalf("%s: exit=%d output:\n%s", name, exitCode, combined.String())
	}
	return combined.String()
}

func (evidence *ticketEvidence) runWithEnv(t *testing.T, name string, stdin io.Reader, additions []string, command string, arguments ...string) string {
	t.Helper()
	original := evidence.env
	evidence.env = append(append([]string{}, original...), additions...)
	defer func() { evidence.env = original }()
	return evidence.run(t, name, stdin, command, arguments...)
}

func (evidence *ticketEvidence) allowFailure(t *testing.T, name string, stdin io.Reader, command string, arguments ...string) (string, int) {
	t.Helper()
	started := time.Now()
	executable := command
	if command == "docker" && evidence.dockerPath != "" {
		executable = evidence.dockerPath
	}
	cmd := exec.Command(executable, arguments...)
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
	if err := os.WriteFile(logPath, combined.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	evidence.commands = append(evidence.commands, map[string]any{"name": name, "exitCode": exitCode, "duration": time.Since(started).Round(time.Millisecond).String()})
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": logPath, "sha256": sha256Hex(combined.Bytes()), "bytes": combined.Len()})
	return combined.String(), exitCode
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(content)), "bytes": len(content)})
}

func scanForSecrets(evidenceDir string, secrets ...string) error {
	return filepath.Walk(evidenceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(data, []byte(secret)) {
				return fmt.Errorf("secret material leaked into %s", path)
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

func httpGet(t *testing.T, client *http.Client, target, origin string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Origin", origin)
	return doRequest(t, client, request)
}

func httpPostExpect(t *testing.T, client *http.Client, target, origin, body string, want int) string {
	t.Helper()
	return httpJSONExpect(t, client, http.MethodPost, target, origin, body, want)
}

// httpPutExpect covers the password-change endpoint, which deliberately uses
// PUT rather than the command-style POST endpoints.
func httpPutExpect(t *testing.T, client *http.Client, target, origin, body string, want int) string {
	t.Helper()
	return httpJSONExpect(t, client, http.MethodPut, target, origin, body, want)
}

func httpJSONExpect(t *testing.T, client *http.Client, method, target, origin, body string, want int) string {
	t.Helper()
	request, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
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
	if response.StatusCode >= http.StatusBadRequest {
		t.Fatalf("%s: status=%d body=%.800s", target, response.StatusCode, raw)
	}
	return string(raw)
}

// postStatus issues a POST without a status assertion and returns the code.
func postStatus(client *http.Client, target, origin, body string) (int, string) {
	status, raw, _ := postResponse(client, target, origin, body)
	return status, raw
}

// postResponse additionally returns response headers for acceptance cases that
// verify the frozen problem+json envelope.
func postResponse(client *http.Client, target, origin, body string) (int, string, http.Header) {
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		return 0, err.Error(), nil
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	return response.StatusCode, string(raw), response.Header.Clone()
}

func doRequest(t *testing.T, client *http.Client, request *http.Request) string {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if response.StatusCode >= 400 {
		t.Fatalf("%s %s: status=%d body=%.600s", request.Method, request.URL, response.StatusCode, raw)
	}
	return string(raw)
}

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

func randomFixtureRunID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
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

// webhookBodyJSON builds one exact Alertmanager webhook body with the
// verified fingerprint (same builder authority as the T04 acceptance).
func webhookBodyJSON(t *testing.T, status string, labels map[string]string, startsAt string) []byte {
	t.Helper()
	sum := alerts.FingerprintOf(labels)
	fingerprint := fmt.Sprintf("%016x", uint64(sum[0])<<56|uint64(sum[1])<<48|uint64(sum[2])<<40|uint64(sum[3])<<32|uint64(sum[4])<<24|uint64(sum[5])<<16|uint64(sum[6])<<8|uint64(sum[7]))
	labelsJSON, _ := json.Marshal(labels)
	return fmt.Appendf(nil, `{"status":%q,"alerts":[{"status":%q,"labels":%s,"startsAt":%q,"endsAt":"0001-01-01T00:00:00Z","fingerprint":"%s"}],"truncatedAlerts":0}`,
		status, status, labelsJSON, startsAt, fingerprint)
}

type sseFrame struct {
	Event string
	ID    string
	Data  string
}

type streamRecorder struct {
	mu     sync.Mutex
	frames []sseFrame
}

func (recorder *streamRecorder) record(frame sseFrame) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.frames = append(recorder.frames, frame)
}

func (recorder *streamRecorder) changeFrames() []sseFrame {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	changes := []sseFrame{}
	for _, frame := range recorder.frames {
		if frame.Event == "change" {
			changes = append(changes, frame)
		}
	}
	return changes
}

// consumeSSE performs one real long-lived SSE GET and records frames. ready
// receives the actual HTTP status after the server accepted the stream, so a
// delivery is never sent before its evidence consumer is attached.
func consumeSSE(ctx context.Context, base, cookieValue, after string, recorder *streamRecorder, ready chan<- int) {
	request, _ := http.NewRequest(http.MethodGet, base+"/api/v1/alerts/events", nil)
	request.Header.Set("Cookie", "__Host-quoin-session="+cookieValue)
	if after != "" {
		request.URL.RawQuery = "after=" + after
	}
	request = request.WithContext(ctx)
	client := &http.Client{Timeout: 0}
	response, err := client.Do(request)
	if err != nil {
		ready <- 0
		return
	}
	defer response.Body.Close()
	ready <- response.StatusCode
	if response.StatusCode != http.StatusOK {
		return
	}
	reader := bufio.NewReader(response.Body)
	var event, id, data strings.Builder
	flush := func() {
		if event.Len() > 0 || id.Len() > 0 || data.Len() > 0 {
			recorder.record(sseFrame{Event: event.String(), ID: id.String(), Data: data.String()})
			event.Reset()
			id.Reset()
			data.Reset()
		}
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			flush()
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			event.WriteString(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "id: "):
			id.WriteString(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

func sessionCookieOf(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range client.Jar.Cookies(parsed) {
		if cookie.Name == "__Host-quoin-session" {
			return cookie.Value
		}
	}
	t.Fatal("session cookie missing from jar")
	return ""
}
