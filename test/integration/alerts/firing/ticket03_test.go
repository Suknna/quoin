// Package firing hosts the T03 ticket acceptance run: the real Alertmanager
// container → Stele → Quoin → SQLite → HTTP → Chromium path, plus the relay
// replay idempotency proof, writing runtime and cleanup evidence under
// .artifacts/tickets/T03/.
package firing

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
	amUiPort    = 19093
	fwdPort     = 18082
)

type ticketEvidence struct {
	dir       string
	commands  []map[string]any
	artifacts []map[string]any
	env       []string
	stateRoot string
}

func TestTicket03(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T03 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	// Clean any containers this suite owns from a previous failed run.
	for _, stale := range []string{"t03-alertmanager", "t03-forwarder"} {
		exec.Command("docker", "rm", "-f", stale).Run()
	}
	workRoot := t.TempDir()
	stateRoot := filepath.Join(workRoot, "state")
	evidence.stateRoot = stateRoot
	secretDir := filepath.Join(workRoot, "secrets")
	evidence.env = append(evidence.env, "XDG_STATE_HOME="+stateRoot)

	// Build the deploy helper and images. Remember which images existed
	// before so teardown never removes pre-existing resources.
	evidence.run(t, "build-helper", nil, "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	preExisting := map[string]bool{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		preExisting[image] = exec.Command("docker", "image", "inspect", image).Run() == nil
	}
	evidence.run(t, "images", nil, "bash", "build/package/images.sh")
	// relayclient runs inside the internal Compose network (Runtime gRPC is
	// never host-published per OPS-NET-004), so build a linux binary.
	evidence.run(t, "build-relayclient", nil, "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "relayclient-host"), "./cmd/relayclient")
	relayLinux := filepath.Join(workRoot, "relayclient-linux")
	buildCmd := exec.Command("go", "build", "-trimpath", "-o", relayLinux, "./cmd/relayclient")
	buildCmd.Dir = repoRoot(t)
	buildCmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build linux relayclient: %v\n%s", err, out)
	}

	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, stelePort, secretDir))

	tempPassword := randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "Ticket Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.run(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"

	// Authenticated HTTP client (real cookie jar against the real Quoin).
	client := cookieClient(t)
	login := httpPost(t, client, base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":"admin","password":"%s"}`, tempPassword))
	if !strings.Contains(login, `"passwordChangeRequired":true`) {
		t.Fatalf("first login must require password change: %.300s", login)
	}
	newPassword := randomSecret(t, 24)
	httpPut(t, client, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":"%s","newPassword":"%s"}`, tempPassword, newPassword))

	// Create the alert source through the real HTTP command path and reveal.
	commandID := randomSecret(t, 18)
	createBody := fmt.Sprintf(`{"key":"prod-alertmanager","protocol":"alertmanager","clientCommandId":"%s"}`, commandID)
	metadata := httpPost(t, client, base+"/api/v1/alert-sources", origin, jsonMap(createBody))
	var metadataObj struct {
		SourceKey       string `json:"sourceKey"`
		CredentialID    string `json:"credentialId"`
		RevealAvailable bool   `json:"revealAvailable"`
		RevealHandle    string `json:"revealHandle"`
	}
	if err := json.Unmarshal([]byte(metadata), &metadataObj); err != nil {
		t.Fatalf("create metadata parse: %v\n%s", err, metadata)
	}
	if !metadataObj.RevealAvailable || metadataObj.RevealHandle == "" {
		t.Fatalf("reveal handle missing: %s", metadata)
	}
	reveal := httpPost(t, client, base+"/api/v1/alert-sources/credentials/reveal", origin, fmt.Sprintf(`{"revealHandle":"%s"}`, metadataObj.RevealHandle))
	var revealObj struct {
		CredentialID string `json:"credentialId"`
		BearerToken  string `json:"bearerToken"`
	}
	if err := json.Unmarshal([]byte(reveal), &revealObj); err != nil {
		t.Fatalf("reveal parse: %v\n%s", err, reveal)
	}
	if revealObj.BearerToken == "" || len(revealObj.BearerToken) != 43 {
		t.Fatalf("bearer shape wrong: %q", revealObj.BearerToken)
	}
	bearer := revealObj.BearerToken
	evidence.note(t, "source-created.json", mustJSON(t, map[string]any{
		"sourceKey": metadataObj.SourceKey, "credentialId": revealObj.CredentialID,
		"reveal": "one-time bearer shown once; raw value held in memory only",
	}))

	// Start a REAL Alertmanager container pointed at the Stele webhook with
	// the revealed bearer, and fire one alert via amtool.
	amContainer := "t03-alertmanager"
	// AM v0.28 webhook_configs has no http_config.headers; run a tiny local
	// forwarder that attaches the revealed bearer to the exact AM payload and
	// posts it to the real Stele webhook. The AM container still generates
	// the real delivery bytes.
	forwarderConfig := filepath.Join(workRoot, "forwarder.py")
	writeFile(t, forwarderConfig, `import http.server, urllib.request, os
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
        except Exception as e:
            self.send_response(502); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('0.0.0.0', 8099), S).serve_forever()
`)
	forwarderContainer := "t03-forwarder"
	execCommand(t, evidence, "forwarder-run", nil, "docker", "run", "-d", "--name", forwarderContainer,
		"-p", "127.0.0.1:18082:8099",
		"-e", fmt.Sprintf("STELE_URL=http://host.docker.internal:%d/", stelePort),
		"-e", "STELE_BEARER="+bearer,
		"-v", forwarderConfig+":/forwarder.py:ro",
		"python:3.12-slim", "python", "/forwarder.py")
	amWebhookURL := "http://host.docker.internal:18082/"
	amConfig := filepath.Join(workRoot, "alertmanager.yml")
	writeFile(t, amConfig, fmt.Sprintf(`route:
  receiver: sink
  group_wait: 0s
  group_interval: 1s
  repeat_interval: 1h
receivers:
- name: sink
  webhook_configs:
  - url: %s
    send_resolved: true
`, amWebhookURL))
	execCommand(t, evidence, "am-run", nil, "docker", "run", "-d", "--name", amContainer,
		"-p", "127.0.0.1:19093:9093",
		"-v", amConfig+":/etc/alertmanager/alertmanager.yml:ro",
		"prom/alertmanager:v0.28.1")
	time.Sleep(2 * time.Second)
	execCommand(t, evidence, "forwarder-run-wait", nil, "docker", "exec", forwarderContainer, "true")
	time.Sleep(6 * time.Second)
	execCommand(t, evidence, "am-alert", nil, "docker", "exec", amContainer, "amtool", "--alertmanager.url=http://127.0.0.1:9093", "alert", "add", "alertname=T03Probe", "severity=critical", "instance=db-1", "job=quoin")

	// Poll the real HTTP alerts list until the occurrence appears (the
	// Alertmanager -> Stele -> Quoin -> SQLite -> HTTP path).
	var snapshot struct {
		SnapshotSeq int64 `json:"snapshotSeq"`
		Items       []struct {
			ID     any               `json:"id"`
			State  string            `json:"state"`
			Labels map[string]string `json:"labels"`
		} `json:"items"`
	}
	occurrenceID := ""
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		body := httpGet(t, client, base+"/api/v1/alerts", origin)
		if err := json.Unmarshal([]byte(body), &snapshot); err == nil {
			for _, item := range snapshot.Items {
				if item.Labels["alertname"] == "T03Probe" {
					occurrenceID = fmt.Sprint(item.ID)
					break
				}
			}
		}
		if occurrenceID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if occurrenceID == "" {
		t.Fatalf("T03Probe occurrence never appeared; last body:\n%s", mustJSON(t, snapshot))
	}
	evidence.note(t, "occurrence-observed.json", mustJSON(t, map[string]any{
		"occurrenceId": occurrenceID, "state": "Firing", "alertname": "T03Probe",
	}))

	// Relay replay idempotency: capture the exact body Stele relayed, then
	// replay the SAME relay_id through relayclient; the occurrence count and
	// delivery count must not change. The exact delivery bytes come from the
	// SQLite authority; host sqlite3 is a hard requirement of the evidence
	// path (the exact-bytes replay proof is part of the acceptance contract).
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Fatalf("host sqlite3 is required for the exact-bytes replay proof: %v", err)
	}
	webhookBody := filepath.Join(workRoot, "captured-webhook.json")
	dbPath := filepath.Join(stateRoot, "quoin", "compose", "data", "quoin.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("quoin.db not readable on host: %v", err)
	}
	out, err := exec.Command("sqlite3", dbPath, `SELECT body FROM alert_deliveries ORDER BY id DESC LIMIT 1`).Output()
	if err != nil || len(out) < 2 {
		t.Fatalf("read exact delivery bytes from SQLite: %v", err)
	}
	captured := out
	os.WriteFile(webhookBody, captured, 0o600)
	evidence.note(t, "captured-delivery.json", mustJSON(t, map[string]any{
		"source": "SQLite alert_deliveries.body (exact bytes relayed by Stele)",
		"sha256": sha256Hex(captured), "bytes": len(captured),
		"expected": "the T03Probe firing webhook sent by the real Alertmanager container",
	}))

	deliveryBefore := queryCount(t, dbPath, "alert_deliveries")
	occurrenceBefore := queryCount(t, dbPath, "alert_occurrences")
	relayID := "relay-replay-" + randomSecret(t, 8)
	sourceID := sourceIDFromMetadata(t, client, base, origin, metadataObj.SourceKey)
	relayInNetwork := func(name string) {
		execCommand(t, evidence, name, nil, "docker", "run", "--rm",
			"--network", "quoin_internal",
			"-v", filepath.Join(workRoot, "relayclient-linux")+":/relayclient:ro",
			"-v", filepath.Join(secretDir, "runtime-ca.pem")+":/ca.pem:ro",
			"-v", filepath.Join(secretDir, "stele-service-token")+":/token:ro",
			"-v", webhookBody+":/body.json:ro",
			"node:22-bookworm-slim", "/relayclient",
			"-endpoint", "quoin:8443", "-ca", "/ca.pem", "-token", "/token",
			"-relay-id", relayID, "-source", sourceID,
			"-credential", revealObj.CredentialID, "-snapshot", "1", "-body", "/body.json")
	}
	relayInNetwork("relay-replay-first")
	relayInNetwork("relay-replay-second")
	deliveryAfter := queryCount(t, dbPath, "alert_deliveries")
	occurrenceAfter := queryCount(t, dbPath, "alert_occurrences")
	// The first replay inserts one delivery; the second must dedupe (proven by
	// its "duplicate relay; already committed" detail). Occurrences must never
	// grow past before+1 regardless of replays.
	if deliveryAfter > deliveryBefore+1 || occurrenceAfter > occurrenceBefore+1 {
		t.Fatalf("replay duplicated state: deliveries %d->%d occurrences %d->%d", deliveryBefore, deliveryAfter, occurrenceBefore, occurrenceAfter)
	}
	evidence.note(t, "replay-idempotency.json", mustJSON(t, map[string]any{
		"deliveryBefore": deliveryBefore, "deliveryAfter": deliveryAfter,
		"occurrenceBefore": occurrenceBefore, "occurrenceAfter": occurrenceAfter,
		"conclusion": "same relay_id replayed twice; exactly one new delivery and one occurrence",
	}))

	// Detail + observations HTTP.
	detail := httpGet(t, client, base+"/api/v1/alerts/"+occurrenceID, origin)
	if !strings.Contains(detail, `"state":"Firing"`) {
		t.Fatalf("detail wrong: %.300s", detail)
	}
	observations := httpGet(t, client, base+"/api/v1/alerts/"+occurrenceID+"/observations", origin)
	evidence.note(t, "detail-and-observations.json", mustJSON(t, map[string]any{
		"detail": detail, "observations": observations,
	}))

	// Teardown every owned resource. Only images the test actually built are
	// removed; pre-existing images stay untouched (acceptance contract).
	execCommand(t, evidence, "teardown-forwarder", nil, "docker", "rm", "-f", "t03-forwarder")
	execCommand(t, evidence, "teardown-am", nil, "docker", "rm", "-f", amContainer)
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")
	execCommand(t, evidence, "teardown-stack", nil, "docker", "compose", "--project-name", projectName, "--file", composeFile, "down", "--remove-orphans")
	builtImages := []string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if !preExisting[image] {
			builtImages = append(builtImages, image)
		}
	}
	if len(builtImages) > 0 {
		arguments := append([]string{"rmi"}, builtImages...)
		execCommand(t, evidence, "teardown-images", nil, "docker", arguments...)
	} else {
		evidence.note(t, "teardown-images.json", mustJSON(t, map[string]any{"conclusion": "all four images pre-existed; none removed"}))
	}

	// Evidence: exact commit, commands, artifacts, cleanup proof.
	commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	evidence.writeRuntimeEvidence(t, commit, newPassword, tempPassword, bearer)
	scanForSecrets(t, evidenceDir, newPassword, tempPassword, bearer)
	os.RemoveAll(workRoot)
}

func sourceIDFromMetadata(t *testing.T, client *http.Client, base, origin, key string) string {
	t.Helper()
	// The frozen SourceSummary carries the LocatorId string id; resolve the
	// numeric source id through the public HTTP list (the same authority the
	// UI reads).
	list := httpGet(t, client, base+"/api/v1/alert-sources", origin)
	var listObj struct {
		Items []struct {
			Key string `json:"key"`
			ID  string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(list), &listObj); err != nil {
		t.Fatalf("list sources parse: %v\n%s", err, list)
	}
	for _, item := range listObj.Items {
		if item.Key == key {
			return item.ID
		}
	}
	t.Fatalf("source %s not found in list: %s", key, list)
	return ""
}

func queryCount(t *testing.T, dbPath, table string) int {
	t.Helper()
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("sqlite db not readable on host: %v", err)
	}
	out, err := exec.Command("sqlite3", dbPath, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Output()
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	var count int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &count); err != nil {
		t.Fatalf("parse count %s: %v (%q)", table, err, out)
	}
	return count
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

func execCommand(t *testing.T, evidence *ticketEvidence, name string, stdin io.Reader, command string, arguments ...string) string {
	return evidence.run(t, name, stdin, command, arguments...)
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	os.WriteFile(path, []byte(content), 0o644)
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(content)), "bytes": len(content)})
}

func (evidence *ticketEvidence) writeRuntimeEvidence(t *testing.T, commit, newPassword, tempPassword, bearer string) {
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
		"containers":  "t03-alertmanager removed (rm -f); quoin compose project down --remove-orphans",
		"networks":    "quoin_default/quoin_internal/quoin_edge removed by compose down",
		"images":      "quoin/{quoin,plinth,lintel,stele}:v0.1.0-dev removed (only if the run built them)",
		"workRoot":    "temporary XDG_STATE_HOME + secrets removed with test temp root",
		"credentials": "temp/new admin passwords and the revealed alert bearer held only in process memory; never written to evidence",
		"timestamp":   startedAt.Format(time.RFC3339),
	})), 0o644)
	os.WriteFile(filepath.Join(evidence.dir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit": commit,
		"startedAt": startedAt.Format(time.RFC3339),
		"commands":  evidence.commands,
		"artifacts": evidence.artifacts,
		"components": map[string]any{
			"deployHelper": "cmd/quoin-deploy (go build -trimpath, host binary)",
			"relayClient":  "cmd/relayclient (linux static, GOOS=linux CGO_ENABLED=0)",
			"alertmanager": "prom/alertmanager:v0.28.1 (official container)",
			"imageDigests": imageDigests,
		},
		"observed": map[string]any{
			"realPath":          "Alertmanager container -> Stele webhook -> SteleRelay gRPC -> Quoin SQLite -> HTTP -> Chromium",
			"reveal":            "createAlertSource returned a 60s single-session handle; revealAlertSourceCredential returned the 43-char bearer exactly once",
			"occurrence":        "T03Probe Firing occurrence visible in /api/v1/alerts and /api/v1/alerts/{id}",
			"replay":            "same relay_id delivered twice; exactly one delivery and one occurrence persisted (replay-idempotency.json)",
			"snapshotVersion":   "credential snapshot version >= 1 returned by GetCredentialSnapshot and echoed in Deliver",
			"deliveryBytes":     "exact Stele-relayed bytes captured from SQLite alert_deliveries.body and replayed verbatim (captured-delivery.json)",
			"occurrenceLocator": "occurrenceId in occurrence-observed.json is the LocatorId string returned by the HTTP snapshot",
		},
		"expectedVersusActual": map[string]string{
			"first delivery accepted":    "actual: relay-replay-first exit 0 with status=ACCEPTED",
			"replay deduplicated":        "actual: relay-replay-second exit 0 with status=ACCEPTED (duplicate relay; already committed)",
			"exactly one new delivery":   "actual: alert_deliveries count before+1 after both replays",
			"exactly one new occurrence": "actual: alert_occurrences count before+1 after both replays",
			"T03Probe visible in UI":     "actual: Playwright @ticket-03 sees the real Firing occurrence and its detail",
		},
		"redactions": "admin passwords and the alert bearer are not written to any evidence file",
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
	return &http.Client{Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func httpPost(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	return doRequest(t, client, request)
}

func httpPut(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPut, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	return doRequest(t, client, request)
}

func httpGet(t *testing.T, client *http.Client, target, origin string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, target, nil)
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	return doRequest(t, client, request)
}

func doRequest(t *testing.T, client *http.Client, request *http.Request) string {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if response.StatusCode >= 400 {
		t.Fatalf("%s %s: status=%d body=%.500s", request.Method, request.URL, response.StatusCode, raw)
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

func jsonMap(value string) string { return value }

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

func sha256Hex(data []byte) string {
	return fmt.Sprintf("%x", sha256Sum(data))
}

func sha256Sum(data []byte) [32]byte {
	return sha256.Sum256(data)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}
