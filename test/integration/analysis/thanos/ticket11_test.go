// Package thanos hosts the T11 ticket acceptance run: the real compose
// stack (Quoin + registered Plinth + Lintel + Stele), the deterministic
// fixture model provider, a deterministic Prometheus-compatible Thanos
// target with a hit counter, and the full Initial Analysis Thanos Tool
// path — tool-before-authorization rejection, grant resolution, the
// supervisor-typed read-only query, long-output spill into the complete
// Artifact, deterministic Evidence, the success transaction and the GC
// fence facts. Evidence lands under .artifacts/tickets/T11/.
package thanos

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	projectName  = "quoin"
	quoinPort    = 18080
	stelePort    = 18081
	amUiPort     = 19093
	providerPort = 18443
	thanosPort   = 18444
)

type ticketEvidence struct {
	dir       string
	commands  []map[string]any
	artifacts []map[string]any
	env       []string
	stateRoot string
}

func TestTicket11(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T11 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	for _, stale := range []string{"t11-am", "t11-forwarder"} {
		exec.Command("docker", "rm", "-f", stale).Run()
	}
	exec.Command("pkill", "-f", "fixtures/model-provider").Run()
	exec.Command("pkill", "-f", "fixtures/thanos-query").Run()
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

	// --- Deterministic fixtures on the host ------------------------------
	// A stale fixture from an earlier run must not hold the fixture ports:
	// the bind-first fixture refuses to start otherwise and the listening
	// probe below would read a stale counter.
	_ = exec.Command("pkill", "-f", "fixture-thanos").Run()
	_ = exec.Command("pkill", "-f", "fixture-provider").Run()
	fixtureBinary := filepath.Join(workRoot, "fixture-provider")
	evidence.run(t, "build-fixture-provider", nil, "go", "build", "-trimpath", "-o", fixtureBinary, "./test/fixtures/model-provider")
	fixtureCmd := exec.Command(fixtureBinary, "-address", fmt.Sprintf("0.0.0.0:%d", providerPort))
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
	thanosBinary := filepath.Join(workRoot, "fixture-thanos")
	evidence.run(t, "build-fixture-thanos", nil, "go", "build", "-trimpath", "-o", thanosBinary, "./test/fixtures/thanos-query")
	thanosCmd := exec.Command(thanosBinary, "-address", fmt.Sprintf("0.0.0.0:%d", thanosPort))
	thanosCmd.Env = evidence.env
	thanosLog, err := os.Create(filepath.Join(evidence.dir, "fixture-thanos.log"))
	if err != nil {
		t.Fatal(err)
	}
	thanosCmd.Stdout = thanosLog
	thanosCmd.Stderr = thanosLog
	if err := thanosCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		thanosCmd.Process.Kill()
		_ = thanosCmd.Wait()
		thanosLog.Close()
	}()
	fixtureReady := false
	for probe := 0; probe < 20; probe++ {
		time.Sleep(200 * time.Millisecond)
		logBody, _ := os.ReadFile(filepath.Join(evidence.dir, "fixture-provider.log"))
		thanosBody, _ := os.ReadFile(filepath.Join(evidence.dir, "fixture-thanos.log"))
		if bytes.Contains(logBody, []byte("listening")) && bytes.Contains(thanosBody, []byte("listening")) {
			fixtureReady = true
			break
		}
	}
	if !fixtureReady {
		t.Fatalf("fixtures never reported listening:\n%s\n%s", logTail(filepath.Join(evidence.dir, "fixture-provider.log")), logTail(filepath.Join(evidence.dir, "fixture-thanos.log")))
	}
	// Plinth reaches both host fixtures through the quoin_internal gateway
	// IP (the supervisor executes provider and Thanos calls inside the
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
	gatewayIP := strings.TrimSpace(outputOf(t, "docker", "network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", networkName))
	if gatewayIP == "" {
		t.Fatal("quoin_internal gateway IP empty")
	}
	evidence.note(t, "fixtures.json", mustJSON(t, map[string]any{
		"provider": fmt.Sprintf("test/fixtures/model-provider on host port %d (fixture-chat-thanos loop)", providerPort),
		"thanos":   fmt.Sprintf("test/fixtures/thanos-query on host port %d (big matrix > 50 KiB / 2000 lines + hit counter)", thanosPort),
		"gateway":  gatewayIP,
	}))

	// --- Register Plinth so agent attempts dispatch ----------------------
	plinthRow := httpGet(t, client, base+"/api/v1/runtime", origin)
	var runtimeView struct {
		Plinth struct {
			RowVersion int64 `json:"rowVersion"`
		} `json:"plinth"`
	}
	if err := json.Unmarshal([]byte(plinthRow), &runtimeView); err != nil {
		t.Fatalf("runtime view parse: %v\n%s", err, plinthRow)
	}
	prepare := httpPost(t, client, base+"/api/v1/runtime-slots/plinth/registration/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t11-prepare-%s","expectedRowVersion":%d}`, randomSecret(t, 8), runtimeView.Plinth.RowVersion))
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

	// --- Enabled qualified model provider (fixture-chat-thanos) ----------
	httpPost(t, client, base+"/api/v1/connections", origin, fmt.Sprintf(`{"clientCommandId":"t11-create-provider","name":"t11-provider","connection":{"type":"model_provider","baseUrl":"http://%s:%d","chatModelId":"fixture-chat-thanos","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}`, gatewayIP, providerPort))
	httpPost(t, client, base+"/api/v1/connections/t11-provider/probe", origin, `{"clientCommandId":"t11-probe-provider"}`)
	probeResultID := ""
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		listBody := httpGet(t, client, base+"/api/v1/connections/t11-provider/probe-results", origin)
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
		t.Fatal("t11-provider never qualified (probe did not pass)")
	}
	connDetail := httpGet(t, client, base+"/api/v1/connections/t11-provider", origin)
	var connObj struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(connDetail), &connObj); err != nil {
		t.Fatalf("provider detail parse: %v\n%s", err, connDetail)
	}
	httpPost(t, client, base+"/api/v1/connections/t11-provider/enable", origin, fmt.Sprintf(`{"clientCommandId":"t11-enable-provider","expectedRowVersion":%d,"qualifiedProbeResultId":"%s"}`, connObj.RowVersion, probeResultID))

	// --- The disabled Thanos connection (authorization target) -----------
	httpPost(t, client, base+"/api/v1/connections", origin, fmt.Sprintf(`{"clientCommandId":"t11-create-thanos","name":"t11-thanos","connection":{"type":"thanos","baseUrl":"http://%s:%d"}}`, gatewayIP, thanosPort))

	// --- A real firing occurrence -----------------------------------------
	createBody := fmt.Sprintf(`{"key":"t11-alertmanager","protocol":"alertmanager","clientCommandId":"t11-source-1"}`)
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
	execCommand(t, evidence, "forwarder-run", nil, "docker", "run", "-d", "--name", "t11-forwarder",
		"-e", "STELE_URL=http://stele:8080/",
		"-e", "STELE_BEARER="+bearer,
		"-v", forwarderConfig+":/forwarder.py:ro",
		"python:3.12-slim", "python", "/forwarder.py")
	execCommand(t, evidence, "forwarder-connect", nil, "docker", "network", "connect", "quoin_internal", "t11-forwarder")
	amConfig := filepath.Join(workRoot, "alertmanager.yml")
	writeFile(t, amConfig, `route:
  receiver: sink
  group_wait: 0s
  group_interval: 1s
  repeat_interval: 1h
receivers:
- name: sink
  webhook_configs:
  - url: http://t11-forwarder:8099/
    send_resolved: true
`)
	execCommand(t, evidence, "am-run", nil, "docker", "run", "-d", "--name", "t11-am",
		"-p", fmt.Sprintf("127.0.0.1:%d:9093", amUiPort),
		"-v", amConfig+":/etc/alertmanager/alertmanager.yml:ro",
		"prom/alertmanager:v0.28.1")
	execCommand(t, evidence, "am-connect", nil, "docker", "network", "connect", "quoin_internal", "t11-am")
	amRunning := false
	for probe := 0; probe < 60; probe++ {
		time.Sleep(1 * time.Second)
		state := strings.TrimSpace(outputOf(t, "docker", "inspect", "-f", "{{.State.Running}}", "t11-am"))
		if state == "true" {
			amRunning = true
			break
		}
		if state == "false" {
			t.Fatalf("alertmanager container exited; logs:\n%s", outputOf(t, "docker", "logs", "--tail", "20", "t11-am"))
		}
	}
	if !amRunning {
		t.Fatal("alertmanager container never started")
	}
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
	execCommand(t, evidence, "am-alert-a", nil, "docker", "exec", "t11-am", "amtool", "--alertmanager.url=http://127.0.0.1:9093", "alert", "add", "alertname=T11Reject", "severity=critical", "instance=db-1", "job=quoin")
	occurrenceA := waitForOccurrence(t, client, base, origin, "T11Reject")

	// =====================================================================
	// Phase A — tool-before-authorization rejection: the model proposes
	// thanos_query while no Thanos connection is enabled; Quoin refuses
	// the whole model call, no tool call row exists and the Thanos target
	// records zero hits (execution never precedes authorization).
	// =====================================================================
	createdA := httpPost(t, client, base+"/api/v1/alerts/"+occurrenceA+"/analyses", origin, `{"clientCommandId":"t11-analysis-reject"}`)
	var createdObjA struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(createdA), &createdObjA); err != nil {
		t.Fatalf("analysis A create parse: %v\n%s", err, createdA)
	}
	evidence.note(t, "analysis-reject-created.json", createdA)
	finalA := waitForTerminalAnalysis(t, client, base+"/api/v1/alerts/"+occurrenceA+"/analyses/"+createdObjA.ID, origin)
	if !strings.Contains(finalA, `"state":"Failed"`) {
		evidence.note(t, "analysis-reject-final.json", finalA)
		t.Fatalf("analysis A must fail without an enabled Thanos connection:\n%s", finalA)
	}
	evidence.note(t, "analysis-reject-final.json", finalA)
	// The Thanos fixture answers /hits; big must still be zero.
	hitsA := thanosHits(t)
	if hitsA["big"] != 0 {
		t.Fatalf("thanos fixture executed %v unauthorized queries", hitsA)
	}
	evidence.note(t, "thanos-hits-after-rejection.json", mustJSON(t, hitsA))

	// =====================================================================
	// Phase B — the enabled connection: grant resolution, the real
	// supervisor-typed query, long-output spill, deterministic Evidence,
	// artifact_read on the spilled Artifact and the sealed output.
	// =====================================================================
	thanosDetail := httpGet(t, client, base+"/api/v1/connections/t11-thanos", origin)
	var thanosObj struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(thanosDetail), &thanosObj); err != nil {
		t.Fatalf("thanos detail parse: %v\n%s", err, thanosDetail)
	}
	httpPost(t, client, base+"/api/v1/connections/t11-thanos/enable", origin, fmt.Sprintf(`{"clientCommandId":"t11-enable-thanos","expectedRowVersion":%d}`, thanosObj.RowVersion))

	execCommand(t, evidence, "am-alert-b", nil, "docker", "exec", "t11-am", "amtool", "--alertmanager.url=http://127.0.0.1:9093", "alert", "add", "alertname=T11Thanosa", "severity=warning", "instance=db-2", "job=quoin")
	occurrenceB := waitForOccurrence(t, client, base, origin, "T11Thanosa")
	createdB := httpPost(t, client, base+"/api/v1/alerts/"+occurrenceB+"/analyses", origin, `{"clientCommandId":"t11-analysis-happy"}`)
	var createdObjB struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(createdB), &createdObjB); err != nil {
		t.Fatalf("analysis B create parse: %v\n%s", err, createdB)
	}
	evidence.note(t, "analysis-happy-created.json", createdB)
	detailURLB := base + "/api/v1/alerts/" + occurrenceB + "/analyses/" + createdObjB.ID
	finalB := waitForTerminalAnalysis(t, client, detailURLB, origin)
	if !strings.Contains(finalB, `"state":"Succeeded"`) {
		plinthLogs := outputOf(t, "docker", "compose", "--project-name", projectName, "--file", composeFile, "logs", "--no-color", "--tail", "80", "plinth")
		evidence.note(t, "plinth-logs-on-failure.txt", plinthLogs)
		t.Fatalf("analysis B did not succeed; last detail:\n%s\nplinth logs:\n%s", finalB, plinthLogs)
	}
	evidence.note(t, "analysis-happy-final.json", finalB)
	if !strings.Contains(finalB, "thanos-proof") {
		t.Fatalf("analysis output lacks the deterministic provider text:\n%s", finalB)
	}

	// --- SQLite authority evidence ---------------------------------------
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Fatalf("host sqlite3 is required for the evidence path: %v", err)
	}
	dbPath := filepath.Join(stateRoot, "quoin", "compose", "data", "quoin.db")
	queryRow := func(query string, args ...string) string {
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
	// Phase A ledger: failed model call, zero tool calls, zero evidence.
	attemptA := queryRow(`SELECT id FROM execution_attempts WHERE attempt_type='initial_analysis' AND scope_id=?`, createdObjA.ID)
	modelFailA := queryRow(`SELECT termination_reason FROM model_calls WHERE attempt_id=? ORDER BY id DESC LIMIT 1`, attemptA)
	toolCallsA := queryRow(`SELECT COUNT(*) FROM tool_calls WHERE attempt_id=?`, attemptA)
	if modelFailA != "invalid_response" || toolCallsA != "0" {
		t.Fatalf("rejection ledger wrong: reason=%q toolCalls=%s", modelFailA, toolCallsA)
	}
	// Phase B ledger: thanos_query + artifact_read succeeded, three chat
	// calls, evidence bound to the tool call and the Artifact.
	attemptB := queryRow(`SELECT id FROM execution_attempts WHERE attempt_type='initial_analysis' AND scope_id=?`, createdObjB.ID)
	chatCallsB := queryRow(`SELECT COUNT(*) FROM model_calls WHERE attempt_id=? AND operation='chat' AND status='succeeded'`, attemptB)
	thanosToolID := queryRow(`SELECT id FROM tool_calls WHERE attempt_id=? AND tool_name='thanos_query' AND status='succeeded' ORDER BY id`, attemptB)
	readToolID := queryRow(`SELECT id FROM tool_calls WHERE attempt_id=? AND tool_name='artifact_read' AND status='succeeded' ORDER BY id`, attemptB)
	spilledArtifact := queryRow(`SELECT result_artifact_id FROM tool_calls WHERE id=?`, thanosToolID)
	if chatCallsB != "3" || thanosToolID == "" || readToolID == "" || spilledArtifact == "" || spilledArtifact == "0" {
		t.Fatalf("ledger wrong: chatCalls=%s thanosTool=%s readTool=%s artifact=%s", chatCallsB, thanosToolID, readToolID, spilledArtifact)
	}
	evidenceCount := queryRow(`SELECT COUNT(*) FROM evidence WHERE attempt_id=? AND tool_call_id=?`, attemptB, thanosToolID)
	evidenceBodyArtifact := queryRow(`SELECT COALESCE(artifact_id,0) FROM evidence WHERE tool_call_id=?`, thanosToolID)
	if evidenceCount != "1" || evidenceBodyArtifact != spilledArtifact {
		t.Fatalf("evidence closure wrong: count=%s bodyArtifact=%s", evidenceCount, evidenceBodyArtifact)
	}
	outputEvidence := queryRow(`SELECT COUNT(*) FROM initial_analysis_output_evidence o JOIN initial_analysis_outputs x ON x.id=o.output_id WHERE x.analysis_id=?`, createdObjB.ID)
	if outputEvidence != "1" {
		t.Fatalf("output evidence refs=%s", outputEvidence)
	}
	grantRow := queryRow(`SELECT COUNT(*) FROM tool_call_connection_grants tcg JOIN attempt_connection_grants g ON g.id=tcg.connection_grant_id WHERE tcg.tool_call_id=? AND g.purpose='thanos_query'`, thanosToolID)
	readGrants := queryRow(`SELECT COUNT(*) FROM attempt_artifact_grants WHERE attempt_id=? AND artifact_id=?`, attemptB, spilledArtifact)
	shaHex := queryRow(`SELECT b.sha256 FROM artifacts a JOIN artifact_blobs b ON b.id=a.blob_id WHERE a.id=?`, spilledArtifact)
	expiresAt := queryRow(`SELECT expires_at FROM artifacts WHERE id=?`, spilledArtifact)
	bodyExpired := queryRow(`SELECT body_expired FROM artifacts WHERE id=?`, spilledArtifact)
	if grantRow != "1" || readGrants != "1" || len(shaHex) != 64 || expiresAt == "" || bodyExpired != "0" {
		t.Fatalf("binding facts wrong: grant=%s readGrants=%s sha=%s expires=%s expired=%s", grantRow, readGrants, shaHex, expiresAt, bodyExpired)
	}
	// The complete Artifact body lives on the artifact volume with the
	// exact content hash (long-output truncation proof: the preview was
	// bounded, the blob is complete).
	blobPath := filepath.Join(stateRoot, "quoin", "compose", "data", "artifacts", "blobs", shaHex+".blob")
	blobBytes, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("blob file missing: %v", err)
	}
	blobHash := sha256.Sum256(blobBytes)
	if hex.EncodeToString(blobHash[:]) != shaHex {
		t.Fatalf("blob hash mismatch: file=%s row=%s", hex.EncodeToString(blobHash[:]), shaHex)
	}
	if !bytes.Contains(blobBytes, []byte("fixture_series")) || len(blobBytes) <= 50*1024 {
		t.Fatalf("blob is not the complete spilled body: %d bytes", len(blobBytes))
	}
	hitsB := thanosHits(t)
	if hitsB["big"] != 1 {
		t.Fatalf("happy path must execute exactly one big query: %v", hitsB)
	}
	evidence.note(t, "sqlite-evidence.json", mustJSON(t, map[string]any{
		"attemptA":             attemptA,
		"rejectionModelReason": modelFailA,
		"rejectionToolCalls":   toolCallsA,
		"attemptB":             attemptB,
		"chatCallsB":           chatCallsB,
		"thanosToolCall":       thanosToolID,
		"readToolCall":         readToolID,
		"spilledArtifact":      spilledArtifact,
		"evidenceBoundTool":    evidenceCount,
		"evidenceBodyArtifact": evidenceBodyArtifact,
		"outputEvidenceRefs":   outputEvidence,
		"grantBindings":        grantRow,
		"attemptReadGrants":    readGrants,
		"artifactSha256":       shaHex,
		"artifactBytes":        len(blobBytes),
		"expiresAt":            expiresAt,
		"bodyExpired":          bodyExpired,
		"thanosHitsAfterHappy": hitsB,
	}))

	// --- HTTP evidence surface --------------------------------------------
	evidenceIDs, err := evidenceIDsOf(t, client, base, origin, finalB)
	if err != nil || len(evidenceIDs) != 1 {
		t.Fatalf("analysis output evidence ids=%v err=%v", evidenceIDs, err)
	}
	evidenceID := evidenceIDs[0]
	evidenceDetail := httpGet(t, client, base+"/api/v1/evidence/"+evidenceID, origin)
	var evidenceObj struct {
		ID         string `json:"id"`
		ObservedAt string `json:"observedAt"`
		Integrity  string `json:"integrity"`
		Producer   struct {
			Kind        string `json:"kind"`
			ToolCallID  string `json:"toolCallId"`
			ToolName    string `json:"toolName"`
			ToolVersion string `json:"toolVersion"`
		} `json:"producer"`
		Connections []struct {
			Key  string `json:"key"`
			Type string `json:"type"`
		} `json:"connections"`
		Body struct {
			Kind     string          `json:"kind"`
			Artifact json.RawMessage `json:"artifact"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(evidenceDetail), &evidenceObj); err != nil {
		t.Fatalf("evidence detail parse: %v\n%s", err, evidenceDetail)
	}
	if evidenceObj.Producer.Kind != "plinth_tool" || evidenceObj.Producer.ToolName != "thanos_query" ||
		evidenceObj.Integrity != "complete" || evidenceObj.ObservedAt == "" ||
		len(evidenceObj.Connections) != 1 || evidenceObj.Connections[0].Type != "thanos" ||
		evidenceObj.Body.Kind != "artifact" {
		t.Fatalf("evidence detail wrong: %s", evidenceDetail)
	}
	evidence.note(t, "evidence-http.json", evidenceDetail)
	metadataHTTP := httpGet(t, client, base+"/api/v1/artifacts/"+spilledArtifact, origin)
	var artifactObj struct {
		ID            string `json:"id"`
		Kind          string `json:"kind"`
		BodyExpired   bool   `json:"bodyExpired"`
		RetentionKind string `json:"retentionKind"`
		SHA256        string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(metadataHTTP), &artifactObj); err != nil {
		t.Fatalf("artifact metadata parse: %v\n%s", err, metadataHTTP)
	}
	if artifactObj.Kind != "tool_result" || artifactObj.BodyExpired || artifactObj.RetentionKind != "generated" || artifactObj.SHA256 != shaHex {
		t.Fatalf("artifact metadata wrong: %s", metadataHTTP)
	}
	download := downloadArtifact(t, client, base+"/api/v1/artifacts/"+spilledArtifact+"/content", origin)
	if download.status != 200 {
		t.Fatalf("download status=%d body=%.200s", download.status, download.body)
	}
	if !strings.Contains(download.headers.Get("Content-Disposition"), "attachment") ||
		download.headers.Get("Cache-Control") != "no-store" ||
		download.headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download headers wrong: %v", download.headers)
	}
	downloadHash := sha256.Sum256([]byte(download.body))
	if hex.EncodeToString(downloadHash[:]) != shaHex {
		t.Fatalf("download body hash mismatch")
	}
	evidence.note(t, "artifact-download-proof.json", mustJSON(t, map[string]any{
		"status": download.status,
		"bytes":  len(download.body),
		"sha256": hex.EncodeToString(downloadHash[:]),
		"headers": map[string]string{
			"Content-Disposition":    download.headers.Get("Content-Disposition"),
			"Cache-Control":          download.headers.Get("Cache-Control"),
			"X-Content-Type-Options": download.headers.Get("X-Content-Type-Options"),
		},
	}))

	// --- GC fence evidence -------------------------------------------------
	// The retention facts a future GC consults are set once and monotonic;
	// expiring the body (simulated here by the same one-way UPDATE the
	// retention runner will issue) keeps metadata + blob durable and turns
	// every read/download path into the structured expired answer
	// (DATA-ARTIFACT-003/004).
	if _, err := exec.Command("sqlite3", dbPath, fmt.Sprintf("UPDATE artifacts SET body_expired=1 WHERE id=%s", spilledArtifact)).CombinedOutput(); err != nil {
		t.Fatalf("expire simulation: %v", err)
	}
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("blob vanished at expiry (GC fence broken): %v", err)
	}
	expiredMetadata := httpGet(t, client, base+"/api/v1/artifacts/"+spilledArtifact, origin)
	if !strings.Contains(expiredMetadata, `"bodyExpired":true`) {
		t.Fatalf("expired metadata must keep the bodyExpired fact: %s", expiredMetadata)
	}
	expiredDownload := downloadArtifact(t, client, base+"/api/v1/artifacts/"+spilledArtifact+"/content", origin)
	if expiredDownload.status != 410 {
		t.Fatalf("expired download status=%d want 410", expiredDownload.status)
	}
	evidence.note(t, "gc-fence-evidence.json", mustJSON(t, map[string]any{
		"expiresAtSet":        expiresAt,
		"bodyExpiredOneWay":   true,
		"blobStillOnVolume":   true,
		"metadataAfterExpiry": expiredMetadata,
		"expiredDownload":     expiredDownload.status,
		"readFence":           "unit tests pin ErrBodyExpired on ReadText/GrepText/OpenBody (store_interruption_test.go)",
	}))

	// --- No credential / environment leakage ------------------------------
	scanForSecrets(t, evidenceDir, newPassword, tempPassword, bearer, "fixture-api-key-2026")

	// --- Teardown ----------------------------------------------------------
	execCommand(t, evidence, "teardown-forwarder", nil, "docker", "rm", "-f", "t11-forwarder")
	execCommand(t, evidence, "teardown-am", nil, "docker", "rm", "-f", "t11-am")
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
	dirtyDigest := sha256Hex([]byte(outputOf(t, "git", "status", "--porcelain")))
	evidence.writeRuntimeEvidence(t, commit, dirtyDigest, map[string]any{
		"rejection":    "fixture-chat-thanos proposed thanos_query with zero enabled Thanos connections; Quoin refused the model call (invalid_response), zero tool_calls rows, zero fixture hits",
		"happyPath":    "enabled t11-thanos -> grant froze in the pending-row transaction -> supervisor fetched the credential grant -> real query against the deterministic target -> 96 KiB matrix spilled into the tool_result Artifact -> deterministic Evidence bound to the tool call -> artifact_read on the spilled Artifact -> sealed output",
		"interruption": "upload interruption/retry/conflict, expired-body read/grep/open fences and out-of-grant denial are deterministic unit tests (internal/quoin/artifact/store_interruption_test.go, internal/plinth/tools/thanosquery_test.go)",
		"gcFence":      "expires_at set at creation, body_expired flips one-way, metadata and blob stay durable, expired downloads answer 410",
		"redactions":   "the provider API key appears only in the create-connection request body and the provider bearer header; never in evidence, logs or the sealed ciphertext",
	})
	os.RemoveAll(workRoot)
}

// thanosHits reads the fixture hit counter.
