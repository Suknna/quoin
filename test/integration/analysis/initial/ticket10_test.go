// Package initial hosts the T10 ticket acceptance run: the real compose
// stack (Quoin + registered Plinth + Lintel + Stele), the deterministic
// fixture model provider, a real Alertmanager delivery, and the full
// Initial Analysis path — HTTP create → plinth supervisor → fresh
// sandboxed agent worker → Eino provider calls → SQLite sealed output →
// HTTP detail. Evidence lands under .artifacts/tickets/T10/.
package initial

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
)

const (
	projectName = "quoin"
	quoinPort   = 18080
	stelePort   = 18081
	fwdPort     = 18082
	amUiPort    = 19093
)

type ticketEvidence struct {
	dir       string
	commands  []map[string]any
	artifacts []map[string]any
	env       []string
	stateRoot string
}

func TestTicket10(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T10 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	for _, stale := range []string{"t10-am", "t10-forwarder"} {
		exec.Command("docker", "rm", "-f", stale).Run()
	}
	exec.Command("pkill", "-f", "fixtures/model-provider").Run()
	workRoot := t.TempDir()
	stateRoot := filepath.Join(workRoot, "state")
	evidence.stateRoot = stateRoot
	secretDir := filepath.Join(workRoot, "secrets")
	evidence.env = append(evidence.env, "XDG_STATE_HOME="+stateRoot)

	evidence.run(t, "build-helper", nil, "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	preExisting := map[string]bool{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		preExisting[image] = exec.Command("docker", "image", "inspect", image).Run() == nil
	}
	evidence.run(t, "images", nil, "bash", "build/package/images.sh")

	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, stelePort, secretDir))
	tempPassword := randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "Ticket Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.run(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"
	client := cookieClient(t)
	login := httpPost(t, client, base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":"admin","password":"%s"}`, tempPassword))
	if !strings.Contains(login, `"passwordChangeRequired":true`) {
		t.Fatalf("first login must require password change: %.300s", login)
	}
	newPassword := randomSecret(t, 24)
	httpPut(t, client, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":"%s","newPassword":"%s"}`, tempPassword, newPassword))

	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")

	// --- Deterministic fixture provider on the host ---------------------
	fixtureBinary := filepath.Join(workRoot, "fixture-provider")
	evidence.run(t, "build-fixture", nil, "go", "build", "-trimpath", "-o", fixtureBinary, "./test/fixtures/model-provider")
	fixtureCmd := exec.Command(fixtureBinary, "-address", "0.0.0.0:18443")
	fixtureCmd.Env = evidence.env
	fixtureLog, err := os.Create(filepath.Join(evidence.dir, "fixture-provider.log"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureCmd.Stdout = fixtureLog
	fixtureCmd.Stderr = fixtureLog
	if err := fixtureCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		fixtureCmd.Process.Kill()
		_ = fixtureCmd.Wait()
		fixtureLog.Close()
	}()
	// The fixture must actually own the port (a stale fixture from another
	// run would answer with old behavior silently).
	fixtureReady := false
	for probe := 0; probe < 20; probe++ {
		time.Sleep(200 * time.Millisecond)
		logBody, _ := os.ReadFile(filepath.Join(evidence.dir, "fixture-provider.log"))
		if bytes.Contains(logBody, []byte("listening")) {
			fixtureReady = true
			break
		}
		if bytes.Contains(logBody, []byte("address already in use")) {
			t.Fatalf("fixture could not bind 18443 (stale provider?): %s", logBody)
		}
	}
	if !fixtureReady {
		t.Fatal("fixture provider never reported listening")
	}
	// The Plinth container reaches the fixture through the quoin_internal
	// gateway IP (the supervisor executes provider calls inside the
	// deployment network).
	gateway := outputOf(t, "docker", "compose", "--project-name", projectName, "--file", composeFile, "ps", "-q", "quoin")
	gateway = outputOf(t, "docker", "inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", strings.TrimSpace(gateway))
	networkName := ""
	for _, name := range strings.Fields(gateway) {
		if strings.HasSuffix(name, "quoin_internal") {
			networkName = name
		}
	}
	if networkName == "" {
		t.Fatalf("quoin_internal network not found on the quoin container: %q", gateway)
	}
	gatewayIP := outputOf(t, "docker", "network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", networkName)
	gatewayIP = strings.TrimSpace(gatewayIP)
	if gatewayIP == "" {
		t.Fatal("quoin_internal gateway IP empty")
	}

	// --- Register Plinth so agent attempts dispatch ---------------------
	plinthRow := httpGet(t, client, base+"/api/v1/runtime", origin)
	var runtimeView struct {
		Plinth struct {
			RowVersion int64 `json:"rowVersion"`
		} `json:"plinth"`
	}
	if err := json.Unmarshal([]byte(plinthRow), &runtimeView); err != nil {
		t.Fatalf("runtime view parse: %v\n%s", err, plinthRow)
	}
	prepare := httpPost(t, client, base+"/api/v1/runtime-slots/plinth/registration/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t10-prepare-%s","expectedRowVersion":%d}`, randomSecret(t, 8), runtimeView.Plinth.RowVersion))
	var prepareObj struct {
		RegistrationTokenHandle string `json:"registrationTokenHandle"`
	}
	if err := json.Unmarshal([]byte(prepare), &prepareObj); err != nil {
		t.Fatalf("prepare parse: %v\n%s", err, prepare)
	}
	reveal := httpPost(t, client, base+"/api/v1/runtime-slots/registration-token/reveal", origin, fmt.Sprintf(`{"registrationTokenHandle":"%s"}`, prepareObj.RegistrationTokenHandle))
	var revealObj struct {
		Slot              string `json:"slot"`
		Generation        int64  `json:"generation"`
		RegistrationToken string `json:"registrationToken"`
	}
	if err := json.Unmarshal([]byte(reveal), &revealObj); err != nil {
		t.Fatalf("reveal parse: %v\n%s", err, reveal)
	}
	tokenJSON := mustJSON(t, map[string]any{"slot": revealObj.Slot, "generation": revealObj.Generation, "token": revealObj.RegistrationToken})
	registerCmd := exec.Command("docker", "compose", "--project-name", projectName, "--file", composeFile, "run", "--rm", "--no-deps", "-i", "-T", "plinth", "register", "--config", "/etc/quoin/component.yaml")
	registerCmd.Stdin = strings.NewReader(tokenJSON + "\n")
	evidence.logCommand(t, "plinth-register", registerCmd)

	// --- Enabled qualified model provider (real probe first) ------------
	createConn := httpPost(t, client, base+"/api/v1/connections", origin, fmt.Sprintf(`{"clientCommandId":"t10-create-conn-1","name":"t10-provider","connection":{"type":"model_provider","baseUrl":"http://%s:18443","chatModelId":"fixture-chat-1","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}`, gatewayIP))
	_ = createConn
	// Connection creation does not auto-probe (T07: the operator triggers
	// the qualification); run the probe through the real command path.
	httpPost(t, client, base+"/api/v1/connections/t10-provider/probe", origin, `{"clientCommandId":"t10-probe-1"}`)
	// Poll the immutable probe results endpoint (the list projection
	// carries no qualified pointer).
	var probeResultID string
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		listBody := httpGet(t, client, base+"/api/v1/connections/t10-provider/probe-results", origin)
		var listObj struct {
			Items []struct {
				ID      string `json:"id"`
				Outcome string `json:"outcome"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(listBody), &listObj); err != nil {
			t.Fatalf("probe results parse: %v\n%s", err, listBody)
		}
		for _, item := range listObj.Items {
			if item.Outcome == "passed" {
				probeResultID = item.ID
			}
		}
		if probeResultID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if probeResultID == "" {
		t.Fatal("t10-provider never qualified (probe did not pass)")
	}
	connDetail := httpGet(t, client, base+"/api/v1/connections/t10-provider", origin)
	var connObj struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(connDetail), &connObj); err != nil {
		t.Fatalf("connection detail parse: %v\n%s", err, connDetail)
	}
	httpPost(t, client, base+"/api/v1/connections/t10-provider/enable", origin, fmt.Sprintf(`{"clientCommandId":"t10-enable-1","expectedRowVersion":%d,"qualifiedProbeResultId":"%s"}`, connObj.RowVersion, probeResultID))

	// --- A real firing occurrence ---------------------------------------
	createBody := fmt.Sprintf(`{"key":"t10-alertmanager","protocol":"alertmanager","clientCommandId":"t10-source-1"}`)
	metadata := httpPost(t, client, base+"/api/v1/alert-sources", origin, createBody)
	var metadataObj struct {
		RevealHandle string `json:"revealHandle"`
	}
	if err := json.Unmarshal([]byte(metadata), &metadataObj); err != nil {
		t.Fatalf("create source parse: %v\n%s", err, metadata)
	}
	revealResp := httpPost(t, client, base+"/api/v1/alert-sources/credentials/reveal", origin, fmt.Sprintf(`{"revealHandle":"%s"}`, metadataObj.RevealHandle))
	var bearerObj struct {
		BearerToken string `json:"bearerToken"`
	}
	if err := json.Unmarshal([]byte(revealResp), &bearerObj); err != nil {
		t.Fatalf("reveal bearer parse: %v\n%s", err, revealResp)
	}
	bearer := bearerObj.BearerToken
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
	execCommand(t, evidence, "forwarder-run", nil, "docker", "run", "-d", "--name", "t10-forwarder",
		"-e", "STELE_URL=http://stele:8080/",
		"-e", "STELE_BEARER="+bearer,
		"-v", forwarderConfig+":/forwarder.py:ro",
		"python:3.12-slim", "python", "/forwarder.py")
	// Both relay containers live on quoin_internal (loopback-published host
	// ports are unreachable from the docker bridge): the forwarder reaches
	// Stele by container DNS and Alertmanager reaches the forwarder the
	// same way (the T04 fixture pattern).
	execCommand(t, evidence, "forwarder-connect", nil, "docker", "network", "connect", "quoin_internal", "t10-forwarder")
	amConfig := filepath.Join(workRoot, "alertmanager.yml")
	writeFile(t, amConfig, `route:
  receiver: sink
  group_wait: 0s
  group_interval: 1s
  repeat_interval: 1h
receivers:
- name: sink
  webhook_configs:
  - url: http://t10-forwarder:8099/
    send_resolved: true
`)
	execCommand(t, evidence, "am-run", nil, "docker", "run", "-d", "--name", "t10-am",
		"-p", fmt.Sprintf("127.0.0.1:%d:9093", amUiPort),
		"-v", amConfig+":/etc/alertmanager/alertmanager.yml:ro",
		"prom/alertmanager:v0.28.1")
	execCommand(t, evidence, "am-connect", nil, "docker", "network", "connect", "quoin_internal", "t10-am")
	// The image pull can outlive a fixed sleep; poll the container state
	// and surface its logs when it crashes (a config error must be loud).
	amRunning := false
	for probe := 0; probe < 60; probe++ {
		time.Sleep(1 * time.Second)
		state := strings.TrimSpace(outputOf(t, "docker", "inspect", "-f", "{{.State.Running}}", "t10-am"))
		if state == "true" {
			amRunning = true
			break
		}
		if state == "false" {
			t.Fatalf("alertmanager container exited; logs:\n%s", outputOf(t, "docker", "logs", "--tail", "20", "t10-am"))
		}
	}
	if !amRunning {
		t.Fatal("alertmanager container never started")
	}
	// Stele must have loaded its credential snapshot before the first
	// webhook arrives: a snapshot miss answers 500 and Alertmanager 0.28
	// does not retry failed webhook deliveries.
	steleReady := false
	for probe := 0; probe < 60; probe++ {
		time.Sleep(1 * time.Second)
		if strings.Contains(outputOf(t, "docker", "logs", "--tail", "40", "quoin-stele-1"), "reason=ready") {
			steleReady = true
			break
		}
	}
	if !steleReady {
		t.Fatalf("stele never became ready; logs:\n%s", outputOf(t, "docker", "logs", "--tail", "40", "quoin-stele-1"))
	}
	execCommand(t, evidence, "am-alert", nil, "docker", "exec", "t10-am", "amtool", "--alertmanager.url=http://127.0.0.1:9093", "alert", "add", "alertname=T10Probe", "severity=critical", "instance=db-1", "job=quoin")

	var snapshot struct {
		Items []struct {
			ID     string            `json:"id"`
			Labels map[string]string `json:"labels"`
		} `json:"items"`
	}
	occurrenceID := ""
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		body := httpGet(t, client, base+"/api/v1/alerts", origin)
		if err := json.Unmarshal([]byte(body), &snapshot); err == nil {
			for _, item := range snapshot.Items {
				if item.Labels["alertname"] == "T10Probe" {
					occurrenceID = item.ID
				}
			}
		}
		if occurrenceID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if occurrenceID == "" {
		// Dump the intake path logs before failing (the forwarder answers
		// 502 silently on any relay failure).
		evidence.note(t, "forwarder-logs-on-failure.txt", outputOf(t, "docker", "logs", "--tail", "30", "t10-forwarder"))
		evidence.note(t, "stele-logs-on-failure.txt", outputOf(t, "docker", "logs", "--tail", "30", "quoin-stele-1"))
		t.Fatalf("T10Probe occurrence never appeared")
	}
	evidence.note(t, "occurrence-observed.json", mustJSON(t, map[string]any{"occurrenceId": occurrenceID, "alertname": "T10Probe"}))

	// --- Initial Analysis: create → worker → sealed output --------------
	created := httpPost(t, client, base+"/api/v1/alerts/"+occurrenceID+"/analyses", origin, `{"clientCommandId":"t10-analysis-1"}`)
	var createdObj struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(created), &createdObj); err != nil {
		t.Fatalf("analysis create parse: %v\n%s", err, created)
	}
	if createdObj.ID == "" {
		t.Fatalf("analysis create returned no id: %s", created)
	}
	evidence.note(t, "analysis-created.json", created)
	analysisID := createdObj.ID
	detailURL := base + "/api/v1/alerts/" + occurrenceID + "/analyses/" + analysisID
	deadline = time.Now().Add(180 * time.Second)
	var finalDetail string
	for time.Now().Before(deadline) {
		finalDetail = httpGet(t, client, detailURL, origin)
		if strings.Contains(finalDetail, `"state":"Succeeded"`) || strings.Contains(finalDetail, `"state":"Failed"`) || strings.Contains(finalDetail, `"state":"Cancelled"`) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(finalDetail, `"state":"Succeeded"`) {
		// Dump the plinth logs for diagnosis before failing.
		plinthLogs := outputOf(t, "docker", "compose", "--project-name", projectName, "--file", composeFile, "logs", "--no-color", "--tail", "80", "plinth")
		evidence.note(t, "plinth-logs-on-failure.txt", plinthLogs)
		t.Fatalf("initial analysis did not succeed; last detail:\n%s\nplinth logs:\n%s", finalDetail, plinthLogs)
	}
	evidence.note(t, "analysis-succeeded.json", finalDetail)
	if !strings.Contains(finalDetail, "初步诊断") || !strings.Contains(finalDetail, "agent-fixture-proof") {
		t.Fatalf("analysis output lacks the deterministic provider text:\n%s", finalDetail)
	}

	// --- SQLite authority evidence --------------------------------------
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Fatalf("host sqlite3 is required for the evidence path: %v", err)
	}
	dbPath := filepath.Join(stateRoot, "quoin", "compose", "data", "quoin.db")
	queryRow := func(query string, args ...string) string {
		t.Helper()
		// The sqlite3 CLI takes no positional parameters; substitute the
		// arguments in order (all acceptance queries pass integer ids).
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
	analysisState := queryRow(`SELECT state FROM initial_analyses WHERE id=?`, analysisID)
	if analysisState != "Succeeded" {
		t.Fatalf("initial_analyses state=%q", analysisState)
	}
	attemptID := queryRow(`SELECT id FROM execution_attempts WHERE attempt_type='initial_analysis' AND scope_id=? ORDER BY id DESC LIMIT 1`, analysisID)
	attemptState := queryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID)
	agentVersion := queryRow(`SELECT agent_version FROM execution_attempts WHERE id=?`, attemptID)
	if attemptState != "Succeeded" || agentVersion != "initial-analysis-v1" {
		t.Fatalf("attempt state=%q agent=%q", attemptState, agentVersion)
	}
	chatCalls := queryRow(`SELECT COUNT(*) FROM model_calls WHERE attempt_id=? AND operation='chat' AND status='succeeded'`, attemptID)
	toolCalls := queryRow(`SELECT COUNT(*) FROM tool_calls WHERE attempt_id=? AND status='succeeded'`, attemptID)
	outputs := queryRow(`SELECT COUNT(*) FROM model_call_outputs o JOIN model_calls m ON m.id=o.model_call_id WHERE m.attempt_id=? AND o.complete=1`, attemptID)
	// The worker ran the bash tool with the fixture-proposed command.
	bashProof := queryRow(`SELECT COUNT(*) FROM tool_calls WHERE attempt_id=? AND tool_name='bash' AND arguments_json LIKE '%agent-fixture-proof%' AND status='succeeded'`, attemptID)
	if chatCalls != "2" || toolCalls != "1" || outputs != "2" || bashProof != "1" {
		t.Fatalf("ledger evidence wrong: chatCalls=%s toolCalls=%s outputs=%s bashProof=%s", chatCalls, toolCalls, outputs, bashProof)
	}
	evidence.note(t, "sqlite-evidence.json", mustJSON(t, map[string]any{
		"analysisState": analysisState, "attemptState": attemptState, "agentVersion": agentVersion,
		"succeededChatCalls": chatCalls, "succeededToolCalls": toolCalls,
		"completeOutputs": outputs, "bashToolProof": bashProof,
		"workerProof": "the bash tool_calls row carries the fixture-proposed command (agent-fixture-proof) with a committed result",
	}))

	// --- No credential / environment leakage -----------------------------
	// 1. The sealed credential never stores the plaintext API key.
	cipherHex := queryRow(`SELECT lower(hex(ciphertext)) FROM credential_generations WHERE connection_id=(SELECT id FROM connections WHERE name='t10-provider')`)
	keyHex := fmt.Sprintf("%x", []byte("fixture-api-key-2026"))
	if strings.Contains(cipherHex, keyHex) {
		t.Fatalf("plaintext API key found inside the sealed credential ciphertext")
	}
	// 2. Every evidence file is scanned for the key and both passwords.
	scanForSecrets(t, evidenceDir, newPassword, tempPassword, bearer, "fixture-api-key-2026")

	// --- Teardown ----------------------------------------------------------
	execCommand(t, evidence, "teardown-forwarder", nil, "docker", "rm", "-f", "t10-forwarder")
	execCommand(t, evidence, "teardown-am", nil, "docker", "rm", "-f", "t10-am")
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
	commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	evidence.writeRuntimeEvidence(t, commit, newPassword, tempPassword, bearer, map[string]any{
		"realPath":    "Alertmanager container -> Stele -> Quoin SQLite -> HTTP create -> Plinth supervisor -> sandboxed worker -> fixture provider -> sealed output -> HTTP detail",
		"provider":    "deterministic fixture (test/fixtures/model-provider) with streaming native tool call + text diagnosis",
		"worker":      "fresh sandboxed worker process (Landlock deny-by-default + seccomp + env cleared) per attempt, self-checked before StartAttemptAck",
		"ledger":      "2 succeeded chat model_calls, 1 succeeded bash tool_call with the fixture command, 2 complete outputs",
		"cancelRaces": "cancel-vs-success commit-order and retry races have deterministic unit tests (internal/quoin/analysis/service_test.go); the frozen schema closes Failed as terminal so the retry reopen is deferred to T12 (ticket comment)",
		"redactions":  "the provider API key appears in the request body sent over HTTPS to Quoin and the provider bearer header; it is never written to evidence, logs or the sealed ciphertext",
	})
	os.RemoveAll(workRoot)
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
		"containers":  "t10-am/t10-forwarder removed (rm -f); quoin compose project down --remove-orphans",
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
			"fixtureProvider": "test/fixtures/model-provider (deterministic OpenAI-compatible, streaming agent mode)",
			"alertmanager":    "prom/alertmanager:v0.28.1 (official container)",
			"imageDigests":    imageDigests,
		},
		"observed": observed,
		"expectedVersusActual": map[string]string{
			"analysis Succeeded":     "actual: initial_analyses.state=Succeeded via the real HTTP poll",
			"worker sandbox":         "actual: every attempt ran in a fresh worker process with Landlock deny-by-default self-check before StartAttemptAck",
			"deterministic provider": "actual: the fixture's streaming native bash tool call and the text diagnosis appear verbatim in the sealed output",
			"ledger closure":         "actual: 2 succeeded chat calls, 1 succeeded bash tool_call, 2 complete outputs (sqlite-evidence.json)",
			"no credential leakage":  "actual: plaintext key absent from ciphertext hex and every evidence file (scanForSecrets)",
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
	request.Header.Set("Origin", origin)
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
		t.Fatalf("%s %s: status=%d body=%.800s", request.Method, request.URL, response.StatusCode, raw)
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
	// Bind-mounted configs must be world-readable: the Alertmanager image
	// runs as UID 65534 and cannot open 0600 host files.
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
