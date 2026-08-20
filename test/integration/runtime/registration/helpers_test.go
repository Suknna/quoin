// Package registration hosts the T06 ticket acceptance run: the real
// compose stack with Plinth and Lintel registered through the attached-stdin
// one-time command over real gRPC, proving token single consumption, boot /
// epoch reconnect behavior, catalog digest mismatch rejection, replacement
// races and readiness transitions. Evidence lands under
// .artifacts/tickets/T06/.
package registration

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
	"net/url"
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
)

type ticketEvidence struct {
	dir       string
	commands  []map[string]any
	artifacts []map[string]any
	env       []string
	stateRoot string
	imageIDs  map[string]string
}

type statusResponse struct {
	Status  int
	Body    string
	Headers http.Header
}

func (evidence *ticketEvidence) run(t *testing.T, name string, command string, arguments ...string) string {
	return evidence.runStdin(t, name, nil, command, arguments...)
}

func (evidence *ticketEvidence) runStdin(t *testing.T, name string, stdin io.Reader, command string, arguments ...string) string {
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

// runExpect runs a command whose NON-ZERO exit is expected; returns output.
func (evidence *ticketEvidence) runExpect(t *testing.T, name string, wantNonZero bool, command string, arguments ...string) (string, int) {
	t.Helper()
	started := time.Now()
	cmd := exec.Command(command, arguments...)
	cmd.Env = evidence.env
	cmd.Dir = repoRoot(t)
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
	if wantNonZero && exitCode == 0 {
		t.Fatalf("%s: expected non-zero exit, got 0:\n%s", name, combined.String())
	}
	if !wantNonZero && exitCode != 0 {
		t.Fatalf("%s: exit=%d output:\n%s", name, exitCode, combined.String())
	}
	return combined.String(), exitCode
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	os.WriteFile(path, []byte(content), 0o644)
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(content)), "bytes": len(content)})
}

func cookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func doRequest(t *testing.T, client *http.Client, method, target, origin, body string) statusResponse {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	return statusResponse{Status: response.StatusCode, Body: string(raw), Headers: response.Header}
}

func loginAndGetCookie(t *testing.T, base, origin, username, password string) (*http.Client, string) {
	t.Helper()
	client := cookieClient(t)
	response := doRequest(t, client, http.MethodPost, base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":%q,"password":%q}`, username, password))
	if response.Status != http.StatusOK {
		t.Fatalf("login %s: status=%d body=%s", username, response.Status, response.Body)
	}
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range client.Jar.Cookies(parsed) {
		if cookie.Name == "__Host-quoin-session" {
			return client, cookie.Value
		}
	}
	t.Fatal("session cookie missing")
	return client, ""
}

// slotView fetches the runtime status projection for one slot.
func slotView(t *testing.T, client *http.Client, base, origin, slot string) map[string]any {
	t.Helper()
	response := doRequest(t, client, http.MethodGet, base+"/api/v1/runtime", origin, "")
	if response.Status != http.StatusOK {
		t.Fatalf("runtime status: %d %s", response.Status, response.Body)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(response.Body), &document); err != nil {
		t.Fatal(err)
	}
	view, _ := document[slot].(map[string]any)
	if view == nil {
		t.Fatalf("slot %s missing from runtime status: %s", slot, response.Body)
	}
	return view
}

func waitForSlot(t *testing.T, client *http.Client, base, origin, slot string, predicate func(map[string]any) bool, what string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = slotView(t, client, base, origin, slot)
		if predicate(last) {
			return last
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("%s never became true; last=%v", what, last)
	return nil
}

func number(t *testing.T, view map[string]any, key string) float64 {
	t.Helper()
	value, ok := view[key].(float64)
	if !ok {
		t.Fatalf("field %s missing or not numeric: %v", key, view)
	}
	return value
}

// registrationToken captures the revealed one-time token over HTTP.
type registrationToken struct {
	Slot        string
	Generation  int64
	Token       string
	Handle      string
}

func prepareAndReveal(t *testing.T, client *http.Client, base, origin, slot string, expectedRow float64) registrationToken {
	t.Helper()
	prepare := doRequest(t, client, http.MethodPost, base+"/api/v1/runtime-slots/"+slot+"/registration/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t06-%s-%d","expectedRowVersion":%d}`, slot, time.Now().UnixNano(), int64(expectedRow)))
	if prepare.Status != http.StatusOK {
		t.Fatalf("prepare %s: %d %s", slot, prepare.Status, prepare.Body)
	}
	var preparation struct {
		RegistrationTokenAvailable bool   `json:"registrationTokenAvailable"`
		RegistrationTokenHandle    string `json:"registrationTokenHandle"`
		RowVersion                 int64  `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(prepare.Body), &preparation); err != nil {
		t.Fatal(err)
	}
	if !preparation.RegistrationTokenAvailable {
		t.Fatalf("prepare must expose a reveal handle: %s", prepare.Body)
	}
	reveal := doRequest(t, client, http.MethodPost, base+"/api/v1/runtime-slots/registration-token/reveal", origin, fmt.Sprintf(`{"registrationTokenHandle":%q}`, preparation.RegistrationTokenHandle))
	if reveal.Status != http.StatusOK {
		t.Fatalf("reveal %s: %d %s", slot, reveal.Status, reveal.Body)
	}
	var result struct {
		Slot              string `json:"slot"`
		Generation        int64  `json:"generation"`
		RegistrationToken string `json:"registrationToken"`
	}
	if err := json.Unmarshal([]byte(reveal.Body), &result); err != nil {
		t.Fatal(err)
	}
	return registrationToken{Slot: result.Slot, Generation: result.Generation, Token: result.RegistrationToken, Handle: preparation.RegistrationTokenHandle}
}

// registerInContainer runs the component's register subcommand under a pty,
// typing the registration JSON only after the prompt is observed (the token
// never enters argv or logs).
func registerInContainer(t *testing.T, evidence *ticketEvidence, composeFile, service, binary string, token registrationToken) string {
	t.Helper()
	command := exec.Command("docker", "compose", "--project-name", projectName, "--file", composeFile, "run", "--rm", "--no-deps", "-i", "-T", service, "register", "--config", "/etc/quoin/component.yaml")
	command.Dir = repoRoot(t)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"slot": token.Slot, "generation": token.Generation, "token": token.Token})
	go func() {
		_, _ = stdin.Write(append(payload, '\n'))
		_ = stdin.Close()
	}()
	var combined bytes.Buffer
	var waiter sync.WaitGroup
	waiter.Add(2)
	go func() { defer waiter.Done(); _, _ = io.Copy(&combined, io.LimitReader(stdout, 1<<20)) }()
	go func() { defer waiter.Done(); _, _ = io.Copy(&combined, io.LimitReader(stderr, 1<<20)) }()
	waiter.Wait()
	exitErr := command.Wait()
	output := combined.String()
	logPath := filepath.Join(evidence.dir, "register-"+service+"-"+fmt.Sprint(token.Generation)+".log")
	// The payload contains the one-time token: it must never land in evidence.
	safe := strings.ReplaceAll(output, token.Token, "[REDACTED]")
	os.WriteFile(logPath, []byte(safe), 0o644)
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": logPath, "sha256": sha256Hex([]byte(safe)), "bytes": len(safe)})
	if exitErr != nil {
		return output
	}
	return output
}

func randomSecret(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
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
	os.WriteFile(path, []byte(content), 0o600)
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
