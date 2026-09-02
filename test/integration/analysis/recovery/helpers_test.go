package recovery

// T12 scenario helpers and shared harness utilities. Every scenario drives
// the real path (HTTP command -> SQLite -> Plinth dispatch -> sandboxed
// worker -> fixture provider) and asserts both the HTTP projection and the
// SQLite authority.

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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/test/support"
)

// fireAlertFunc delivers one real Alertmanager alert and returns the
// occurrence id.
type fireAlertFunc func(t *testing.T, alertName string) string

// queryRowFunc runs one sqlite3 query returning a scalar string.
type queryRowFunc func(query string, args ...string) string

// createAnalysis posts the real create command and returns the analysis id.
func createAnalysis(t *testing.T, client *http.Client, base, origin, occurrenceID, commandID string) (string, string) {
	t.Helper()
	body := httpPost(t, client, fmt.Sprintf("%s/api/v1/alerts/%s/analyses", base, occurrenceID), origin, fmt.Sprintf(`{"clientCommandId":"%s"}`, commandID))
	var parsed struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.ID == "" {
		t.Fatalf("analysis create failed: %v\n%s", err, body)
	}
	return parsed.ID, parsed.State
}

// analysisDetail reads the authoritative detail projection.
func analysisDetail(t *testing.T, client *http.Client, base, origin, occurrenceID, analysisID string) (state string, rowVersion int64, body string) {
	t.Helper()
	body = httpGet(t, client, fmt.Sprintf("%s/api/v1/alerts/%s/analyses/%s", base, occurrenceID, analysisID), origin)
	var parsed struct {
		State      string `json:"state"`
		RowVersion int64  `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("analysis detail parse: %v\n%s", err, body)
	}
	return parsed.State, parsed.RowVersion, body
}

// waitAnalysisState polls the authoritative detail until the analysis
// reaches one of the terminal states; the observed transition evidence is
// the caller's to record.
func waitAnalysisState(t *testing.T, client *http.Client, base, origin, occurrenceID, analysisID string, terminals ...string) (string, string) {
	t.Helper()
	deadline := time.Now().Add(240 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		state, _, body := analysisDetail(t, client, base, origin, occurrenceID, analysisID)
		last = body
		for _, terminal := range terminals {
			if state == terminal {
				return state, body
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("analysis never reached %v; last detail:\n%s", terminals, last)
	return "", ""
}

// cancelAnalysis posts the cancellation fence command.
func cancelAnalysis(t *testing.T, client *http.Client, base, origin, occurrenceID, analysisID string, rowVersion int64, commandID string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"clientCommandId":"%s","expectedRowVersion":%d}`, commandID, rowVersion)
	target := fmt.Sprintf("%s/api/v1/alerts/%s/analyses/%s/cancel", base, occurrenceID, analysisID)
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(payload)
}

// attemptAuthority reads attempt state/reason from the SQLite authority.
func attemptAuthority(t *testing.T, queryRow queryRowFunc, analysisID string) (state, reason string) {
	t.Helper()
	attemptID := queryRow(`SELECT id FROM execution_attempts WHERE attempt_type='initial_analysis' AND scope_id=? ORDER BY id DESC LIMIT 1`, analysisID)
	state = queryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID)
	reason = queryRow(`SELECT COALESCE(termination_reason,'') FROM execution_attempts WHERE id=?`, attemptID)
	return state, reason
}

// containerOf resolves the concrete container name of one compose service.
func containerOf(t *testing.T, composeFile, service string) string {
	t.Helper()
	return strings.TrimSpace(outputOf(t, "docker", "compose", "--project-name", projectName, "--file", composeFile, "ps", "-q", service))
}

// waitRuntimeReconnect waits until the plinth slot reports connected on the
// given boot with an epoch at least minEpoch.
func waitRuntimeReconnect(t *testing.T, client *http.Client, base string, origin string, minEpoch int) (bootID string, epoch int) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		body := httpGet(t, client, base+"/api/v1/runtime", origin)
		var view struct {
			Plinth struct {
				Connected       bool   `json:"connected"`
				BootID          string `json:"bootId"`
				ConnectionEpoch *int   `json:"connectionEpoch"`
			} `json:"plinth"`
		}
		if err := json.Unmarshal([]byte(body), &view); err == nil && view.Plinth.Connected && view.Plinth.ConnectionEpoch != nil && *view.Plinth.ConnectionEpoch >= minEpoch {
			return view.Plinth.BootID, *view.Plinth.ConnectionEpoch
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("plinth never reconnected with epoch >= %d", minEpoch)
	return "", 0
}

// waitQuoinHTTP waits until the Quoin public endpoint answers login-free
// routes (any status except connection errors; the login route is enough).
func waitQuoinHTTP(t *testing.T, base, origin string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequest(http.MethodPost, base+"/api/v1/auth/login", strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		if response, err := http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("quoin HTTP never came back")
}

// prepareProviderAndRuntime registers Plinth and enables the qualified
// fixture model provider through the real command paths.
func prepareProviderAndRuntime(t *testing.T, evidence *ticketEvidence, client *http.Client, base, origin, composeFile string) {
	t.Helper()
	gateway := gatewayIP(t, composeFile)
	support.PrepareProviderAndRuntime(t, client, base, origin, composeFile, projectName, "http://"+gateway+":18443", "t12")
	evidence.note(t, "plinth-register-output.txt", "registered through test/support.PrepareProviderAndRuntime")
}

// createAlertSource creates the Stele-authorized source and reveals its
// bearer (the bearer travels only in request bodies / env vars).
func createAlertSource(t *testing.T, evidence *ticketEvidence, client *http.Client, base, origin string) string {
	t.Helper()
	bearer := support.CreateAlertSource(t, client, base, origin, "t12-alertmanager", "t12-source-1")
	evidence.note(t, "alert-source-create.txt", "created through test/support.CreateAlertSource")
	return bearer
}

// startAlertmanager boots the forwarder + Alertmanager pair on the
// deployment network.
func startAlertmanager(t *testing.T, evidence *ticketEvidence, workRoot, bearer string) {
	t.Helper()
	forwarderConfig := filepath.Join(workRoot, "forwarder.py")
	writeFile(t, forwarderConfig, `import http.server, urllib.request, urllib.error, os
class S(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(n)
        req = urllib.request.Request(os.environ['STELE_URL'], data=body, method='POST', headers={'Content-Type': self.headers.get('Content-Type', 'application/json'), 'Authorization': 'Bearer ' + os.environ['STELE_BEARER']})
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                self.send_response(resp.status); self.end_headers()
        except urllib.error.HTTPError as e:
            self.send_response(e.code); self.end_headers()
        except Exception:
            self.send_response(502); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('0.0.0.0', 8099), S).serve_forever()
`)
	execCommand(t, evidence, "forwarder-run", nil, "docker", "run", "-d", "--name", "t12-forwarder",
		"-e", "STELE_URL=http://stele:8080/",
		"-e", "STELE_BEARER="+bearer,
		"-v", forwarderConfig+":/forwarder.py:ro",
		"python:3.12-slim", "python", "/forwarder.py")
	execCommand(t, evidence, "forwarder-connect", nil, "docker", "network", "connect", "quoin_internal", "t12-forwarder")
	amConfig := filepath.Join(workRoot, "alertmanager.yml")
	writeFile(t, amConfig, `route:
  receiver: sink
  group_wait: 0s
  group_interval: 1s
  repeat_interval: 1h
receivers:
- name: sink
  webhook_configs:
  - url: http://t12-forwarder:8099/
    send_resolved: true
`)
	execCommand(t, evidence, "am-run", nil, "docker", "run", "-d", "--name", "t12-am",
		"-p", fmt.Sprintf("127.0.0.1:%d:9093", amUiPort),
		"-v", amConfig+":/etc/alertmanager/alertmanager.yml:ro",
		"prom/alertmanager:v0.28.1")
	execCommand(t, evidence, "am-connect", nil, "docker", "network", "connect", "quoin_internal", "t12-am")
	waitFor(t, "alertmanager running", func() bool {
		return strings.TrimSpace(outputOf(t, "docker", "inspect", "-f", "{{.State.Running}}", "t12-am")) == "true"
	}, 90*time.Second)
	// Stele must hold its credential snapshot before the first webhook.
	waitFor(t, "stele ready", func() bool {
		return strings.Contains(outputOf(t, "docker", "logs", "--tail", "40", "quoin-stele-1"), "reason=ready")
	}, 60*time.Second)
}

// gatewayIP resolves the quoin_internal gateway the containers use to
// reach host fixtures.
func gatewayIP(t *testing.T, composeFile string) string {
	t.Helper()
	raw := outputOf(t, "docker", "compose", "--project-name", projectName, "--file", composeFile, "ps", "-q", "quoin")
	networks := outputOf(t, "docker", "inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", strings.TrimSpace(raw))
	for _, name := range strings.Fields(networks) {
		if strings.HasSuffix(name, "quoin_internal") {
			gateway := strings.TrimSpace(outputOf(t, "docker", "network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", name))
			if gateway != "" {
				return gateway
			}
		}
	}
	t.Fatalf("quoin_internal gateway not found: %q", networks)
	return ""
}

func sqliteQuerier(t *testing.T, dbPath string) queryRowFunc {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Fatalf("host sqlite3 is required: %v", err)
	}
	return func(query string, args ...string) string {
		t.Helper()
		for _, arg := range args {
			if _, err := strconv.ParseInt(arg, 10, 64); err != nil {
				t.Fatalf("evidence query args must be integers, got %q", arg)
			}
			query = strings.Replace(query, "?", arg, 1)
		}
		out, err := exec.Command("sqlite3", dbPath, query).CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite query %q: %v\n%s", query, err, out)
		}
		return strings.TrimSpace(string(out))
	}
}

func waitFor(t *testing.T, what string, check func() bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("%s never became true within %s", what, within)
}

func builtImagesDisposition(preExisting map[string]bool) []string {
	disposition := []string{}
	for image, existed := range preExisting {
		if existed {
			disposition = append(disposition, image+" (pre-existed; kept)")
		} else {
			disposition = append(disposition, image+" (built by this run; removed)")
		}
	}
	return disposition
}

// --- shared command/http helpers (T10 harness shape) --------------------

func (evidence *ticketEvidence) run(t *testing.T, name string, stdin io.Reader, command string, arguments ...string) string {
	t.Helper()
	cmd := exec.Command(command, arguments...)
	if stdin != nil {
		cmd.Stdin = stdin
	} else {
		cmd.Stdin = bytes.NewReader(nil)
	}
	cmd.Env = evidence.env
	cmd.Dir = repoRoot(t)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	evidence.logCommand(t, name, cmd)
	evidence.commands = append(evidence.commands, map[string]any{
		"name": name, "command": command, "arguments": arguments, "exitCode": exitCodeOf(runErr),
	})
	if runErr != nil {
		t.Fatalf("%s failed (%v):\n%s", name, runErr, output.String())
	}
	return output.String()
}

func (evidence *ticketEvidence) logCommand(t *testing.T, name string, cmd *exec.Cmd) {
	t.Helper()
	digest := sha256.Sum256([]byte(strings.Join(cmd.Args, " ")))
	entry := map[string]any{
		"name": name, "command": strings.Join(cmd.Args, " "),
		"argvDigest": fmt.Sprintf("%x", digest[:]),
	}
	evidence.commands = append(evidence.commands, entry)
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	evidence.artifacts = append(evidence.artifacts, map[string]any{
		"name": name, "sha256": fmt.Sprintf("%x", digest[:]), "bytes": len(content),
	})
}

func (evidence *ticketEvidence) writeRuntimeEvidence(t *testing.T, commit, newPassword, tempPassword, bearer string, observed map[string]any) {
	t.Helper()
	document := map[string]any{
		"ticket":      "T12",
		"gitCommit":   commit,
		"dirtyState":  outputOf(t, "git", "status", "--porcelain"),
		"generatedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"componentVersions": map[string]any{
			"quoin":        buildinfoVersion(t),
			"images":       []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"},
			"alertmanager": "prom/alertmanager:v0.28.1",
			"fixture":      "test/fixtures/model-provider (deterministic OpenAI-compatible, T12 slow/partial branches)",
		},
		"observed":   observed,
		"commands":   evidence.commands,
		"artifacts":  evidence.artifacts,
		"redactions": []string{"adminNewPassword", "adminTempPassword", "stBearer", "providerApiKey", "registrationToken"},
	}
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence.dir, "runtime-evidence.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = newPassword
	_ = tempPassword
	_ = bearer
}

func (evidence *ticketEvidence) writeCleanup(t *testing.T, disposition map[string]any) {
	t.Helper()
	body, err := json.MarshalIndent(disposition, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence.dir, "cleanup.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildinfoVersion(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(outputOf(t, "git", "rev-parse", "--short", "HEAD"))
}

func exitCodeOf(runErr error) int {
	if runErr == nil {
		return 0
	}
	if exitError, ok := runErr.(*exec.ExitError); ok {
		return exitError.ExitCode()
	}
	return -1
}

func execCommand(t *testing.T, evidence *ticketEvidence, name string, stdin io.Reader, command string, arguments ...string) string {
	t.Helper()
	cmd := exec.Command(command, arguments...)
	if stdin != nil {
		cmd.Stdin = stdin
	} else {
		cmd.Stdin = bytes.NewReader(nil)
	}
	cmd.Dir = repoRoot(t)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	evidence.logCommand(t, name, cmd)
	evidence.commands = append(evidence.commands, map[string]any{
		"name": name, "command": command, "arguments": arguments, "exitCode": exitCodeOf(runErr),
	})
	if runErr != nil {
		t.Fatalf("%s failed (%v):\n%s", name, runErr, output.String())
	}
	return output.String()
}

func scanForSecrets(t *testing.T, evidenceDir string, secrets ...string) {
	t.Helper()
	entries, err := os.ReadDir(evidenceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(evidenceDir, entry.Name())
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(body, []byte(secret)) {
				t.Fatalf("secret leaked into %s", path)
			}
		}
	}
}

func cookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}
}

func httpPost(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	return doRequest(t, client, request)
}

func httpPut(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	return doRequest(t, client, request)
}

func httpGet(t *testing.T, client *http.Client, target, origin string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	return doRequest(t, client, request)
}

func doRequest(t *testing.T, client *http.Client, request *http.Request) string {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func randomSecret(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)[:length]
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker is required for the T12 acceptance path: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	// Config fixtures mounted read-only into containers need world-read
	// (0600 root-owned files are unreadable by container users).
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	raw, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

func outputOf(t *testing.T, command string, arguments ...string) string {
	t.Helper()
	out, err := exec.Command(command, arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed (%v):\n%s", command, arguments, err, out)
	}
	return string(out)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
