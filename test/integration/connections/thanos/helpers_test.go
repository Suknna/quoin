// Package thanos hosts the T07 thanos slice of the acceptance run: the real
// compose stack with a registered Plinth supervisor, a real Thanos Query
// container, a deterministic wrong-shape fixture and an unroutable negative
// target, driven end to end through HTTP create/probe/enable and the live
// control stream. Evidence lands under .artifacts/tickets/T07/.
package thanos

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
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
	"sync"
	"testing"
	"time"
)

const (
	projectName = "quoin"
	quoinPort   = 18080
)

type ticketEvidence struct {
	dir      string
	commands []map[string]any
	env      []string
}

type statusResponse struct {
	Status int
	Body   string
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func randomSecret(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

func outputOf(t *testing.T, command string, arguments ...string) string {
	t.Helper()
	out, err := exec.Command(command, arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", command, arguments, err, out)
	}
	return strings.TrimSpace(string(out))
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
	os.WriteFile(filepath.Join(evidence.dir, name+".log"), combined.Bytes(), 0o644)
	evidence.commands = append(evidence.commands, map[string]any{"name": name, "exitCode": exitCode, "duration": time.Since(started).Round(time.Millisecond).String()})
	if exitCode != 0 {
		t.Fatalf("%s: exit=%d output:\n%s", name, exitCode, combined.String())
	}
	return combined.String()
}

// runAllowFail records a command without failing the test on its exit code.
func (evidence *ticketEvidence) runAllowFail(t *testing.T, name string, command string, arguments ...string) (string, int) {
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
	os.WriteFile(filepath.Join(evidence.dir, name+".log"), combined.Bytes(), 0o644)
	evidence.commands = append(evidence.commands, map[string]any{"name": name, "exitCode": exitCode, "duration": time.Since(started).Round(time.Millisecond).String()})
	return combined.String(), exitCode
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	os.WriteFile(filepath.Join(evidence.dir, name), []byte(content), 0o644)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 20 * time.Second}
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
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return statusResponse{Status: response.StatusCode, Body: string(raw)}
}

func loginAndGetCookie(t *testing.T, base, origin, username, password string) *http.Client {
	t.Helper()
	client := cookieClient(t)
	response := doRequest(t, client, http.MethodPost, base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":%q,"password":%q}`, username, password))
	if response.Status != http.StatusOK {
		t.Fatalf("login %s: status=%d body=%s", username, response.Status, response.Body)
	}
	return client
}

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

func waitFor(t *testing.T, what string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("%s never became true", what)
}

type registrationToken struct {
	Slot       string
	Generation int64
	Token      string
	Handle     string
}

func prepareAndReveal(t *testing.T, client *http.Client, base, origin, slot string, expectedRow float64) registrationToken {
	t.Helper()
	prepare := doRequest(t, client, http.MethodPost, base+"/api/v1/runtime-slots/"+slot+"/registration/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t07-%s-%d","expectedRowVersion":%d}`, slot, time.Now().UnixNano(), int64(expectedRow)))
	if prepare.Status != http.StatusOK {
		t.Fatalf("prepare %s: %d %s", slot, prepare.Status, prepare.Body)
	}
	var preparation struct {
		RegistrationTokenAvailable bool   `json:"registrationTokenAvailable"`
		RegistrationTokenHandle    string `json:"registrationTokenHandle"`
	}
	if err := json.Unmarshal([]byte(prepare.Body), &preparation); err != nil {
		t.Fatal(err)
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

// registerPlinth runs plinth's register subcommand with the token on
// attached stdin (never argv).
func registerPlinth(t *testing.T, evidence *ticketEvidence, composeFile string, token registrationToken) string {
	t.Helper()
	command := exec.Command("docker", "compose", "--project-name", projectName, "--file", composeFile, "run", "--rm", "--no-deps", "-i", "-T", "plinth", "register", "--config", "/etc/quoin/component.yaml")
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
	_ = command.Wait()
	output := combined.String()
	os.WriteFile(filepath.Join(evidence.dir, "register-plinth.log"), []byte(output), 0o644)
	return output
}

// quoinNetwork discovers the compose network the quoin containers share.
func quoinNetwork(t *testing.T) string {
	t.Helper()
	container := outputOf(t, "docker", "compose", "--project-name", projectName, "ps", "-q", "quoin")
	if container == "" {
		t.Fatal("quoin container not found")
	}
	raw := outputOf(t, "docker", "inspect", container, "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}")
	for _, name := range strings.Fields(raw) {
		if strings.HasSuffix(name, "quoin_internal") {
			return name
		}
	}
	t.Fatalf("quoin_internal network not found (raw=%q)", raw)
	return ""
}

// startFixture runs one owned fixture container attached to the stack
// network; its lifecycle is recorded for cleanup.
func startFixture(t *testing.T, evidence *ticketEvidence, network, name string, arguments ...string) {
	t.Helper()
	full := append([]string{"run", "-d", "--name", name, "--network", network}, arguments...)
	output, code := evidence.runAllowFail(t, "fixture-"+name, "docker", full...)
	if code != 0 {
		t.Fatalf("fixture %s failed to start: %s", name, output)
	}
}

// waitForHTTP polls a URL until it answers any status.
func waitForHTTP(t *testing.T, target, what string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(target)
		if err == nil {
			response.Body.Close()
			if response.StatusCode > 0 {
				return
			}
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("%s never answered", what)
}

// connectionSummary extracts id/rowVersion from a connection response.
func connectionLocator(t *testing.T, body string) (string, int64) {
	t.Helper()
	var parsed struct {
		ID         string `json:"id"`
		RowVersion int64  `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.ID == "" {
		// ConnectionSummary carries no id in some projections; fall back to
		// rowVersion only.
		var partial struct {
			RowVersion int64 `json:"rowVersion"`
		}
		if err := json.Unmarshal([]byte(body), &partial); err != nil {
			t.Fatalf("connection response unparseable: %s", body)
		}
		return "", partial.RowVersion
	}
	return parsed.ID, parsed.RowVersion
}

// createConnection posts the one-time-secret create command.
func createConnection(t *testing.T, client *http.Client, base, origin, commandID, name string, connection map[string]any) statusResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"clientCommandId": commandID, "name": name, "connection": connection})
	if err != nil {
		t.Fatal(err)
	}
	return doRequest(t, client, http.MethodPost, base+"/api/v1/connections", origin, string(body))
}

// probeState polls the attempt until terminal and returns its final view.
func probeState(t *testing.T, client *http.Client, base, origin, name, attemptID string) map[string]any {
	t.Helper()
	var last map[string]any
	waitFor(t, "probe "+attemptID+" terminal", func() bool {
		response := doRequest(t, client, http.MethodGet, base+"/api/v1/connections/"+name+"/probe-attempts/"+attemptID, origin, "")
		if response.Status != http.StatusOK {
			t.Fatalf("probe attempt read: %d %s", response.Status, response.Body)
		}
		var document map[string]any
		if err := json.Unmarshal([]byte(response.Body), &document); err != nil {
			t.Fatal(err)
		}
		last = document
		state, _ := document["state"].(string)
		switch state {
		case "Succeeded", "Failed", "Cancelled", "Interrupted":
			return true
		}
		return false
	})
	return last
}

// probeResults lists the immutable result history.
func probeResults(t *testing.T, client *http.Client, base, origin, name string) []map[string]any {
	t.Helper()
	response := doRequest(t, client, http.MethodGet, base+"/api/v1/connections/"+name+"/probe-results?limit=20", origin, "")
	if response.Status != http.StatusOK {
		t.Fatalf("probe results: %d %s", response.Status, response.Body)
	}
	var document struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(response.Body), &document); err != nil {
		t.Fatal(err)
	}
	return document.Items
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
