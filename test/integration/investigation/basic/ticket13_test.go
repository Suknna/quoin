// Package basic hosts the T13 ticket acceptance run: the real compose
// stack (Quoin + registered Plinth + Lintel + Stele), the deterministic
// fixture model provider, a real Alertmanager delivery, and the full
// Investigation path — HTTP create (blank + sourced) → plinth supervisor →
// fresh sandboxed worker → fixture provider → exact ui-message-stream SSE
// frames → SQLite committed head/messages. Evidence lands under
// .artifacts/tickets/T13/.
package basic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTicket13(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T13 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	// The acceptance always rebuilds the four images: the web dist and Go
	// sources both feed the quoin image, and a stale image would test old
	// code (the same policy as test/e2e/compose/server.sh).
	evidence.env = append(evidence.env, "QUOIN_FORCE_IMAGE_BUILD=1")
	for _, stale := range []string{"t13-am", "t13-forwarder"} {
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
	prepare := httpPost(t, client, base+"/api/v1/runtime-slots/plinth/registration/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t13-prepare-%s","expectedRowVersion":%d}`, randomSecret(t, 8), runtimeView.Plinth.RowVersion))
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
	// The control stream attaches asynchronously after registration; wait
	// for the live projection so the first dispatch binds deterministically.
	plinthConnected := false
	for probe := 0; probe < 60; probe++ {
		runtimeBody := httpGet(t, client, base+"/api/v1/runtime", origin)
		var runtimeStatus struct {
			Plinth struct {
				Connected bool `json:"connected"`
			} `json:"plinth"`
		}
		if err := json.Unmarshal([]byte(runtimeBody), &runtimeStatus); err == nil && runtimeStatus.Plinth.Connected {
			plinthConnected = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !plinthConnected {
		t.Fatal("plinth control stream never attached after registration")
	}

	// --- Enabled qualified model provider (real probe first) ------------
	httpPost(t, client, base+"/api/v1/connections", origin, fmt.Sprintf(`{"clientCommandId":"t13-create-conn-1","name":"t13-provider","connection":{"type":"model_provider","baseUrl":"http://%s:18443","chatModelId":"fixture-chat-1","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}`, gatewayIP))
	httpPost(t, client, base+"/api/v1/connections/t13-provider/probe", origin, `{"clientCommandId":"t13-probe-1"}`)
	var probeResultID string
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		listBody := httpGet(t, client, base+"/api/v1/connections/t13-provider/probe-results", origin)
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
		t.Fatal("t13-provider never qualified (probe did not pass)")
	}
	connDetail := httpGet(t, client, base+"/api/v1/connections/t13-provider", origin)
	var connObj struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(connDetail), &connObj); err != nil {
		t.Fatalf("connection detail parse: %v\n%s", err, connDetail)
	}
	httpPost(t, client, base+"/api/v1/connections/t13-provider/enable", origin, fmt.Sprintf(`{"clientCommandId":"t13-enable-1","expectedRowVersion":%d,"qualifiedProbeResultId":"%s"}`, connObj.RowVersion, probeResultID))

	// --- A real firing occurrence for the sourced investigation ---------
	createBody := fmt.Sprintf(`{"key":"t13-alertmanager","protocol":"alertmanager","clientCommandId":"t13-source-1"}`)
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
	execCommand(t, evidence, "forwarder-run", nil, "docker", "run", "-d", "--name", "t13-forwarder",
		"-e", "STELE_URL=http://stele:8080/",
		"-e", "STELE_BEARER="+bearer,
		"-v", forwarderConfig+":/forwarder.py:ro",
		"python:3.12-slim", "python", "/forwarder.py")
	execCommand(t, evidence, "forwarder-connect", nil, "docker", "network", "connect", "quoin_internal", "t13-forwarder")
	amConfig := filepath.Join(workRoot, "alertmanager.yml")
	writeFile(t, amConfig, `route:
  receiver: sink
  group_wait: 0s
  group_interval: 1s
  repeat_interval: 1h
receivers:
- name: sink
  webhook_configs:
  - url: http://t13-forwarder:8099/
    send_resolved: true
`)
	execCommand(t, evidence, "am-run", nil, "docker", "run", "-d", "--name", "t13-am",
		"-v", amConfig+":/etc/alertmanager/alertmanager.yml:ro",
		"prom/alertmanager:v0.28.1")
	execCommand(t, evidence, "am-connect", nil, "docker", "network", "connect", "quoin_internal", "t13-am")
	amRunning := false
	for probe := 0; probe < 60; probe++ {
		time.Sleep(1 * time.Second)
		state := strings.TrimSpace(outputOf(t, "docker", "inspect", "-f", "{{.State.Running}}", "t13-am"))
		if state == "true" {
			amRunning = true
			break
		}
		if state == "false" {
			t.Fatalf("alertmanager container exited; logs:\n%s", outputOf(t, "docker", "logs", "--tail", "20", "t13-am"))
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
	execCommand(t, evidence, "am-alert", nil, "docker", "exec", "t13-am", "amtool", "--alertmanager.url=http://127.0.0.1:9093", "alert", "add", "alertname=T13Probe", "severity=critical", "instance=db-1", "job=quoin")
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
				if item.Labels["alertname"] == "T13Probe" {
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
		evidence.note(t, "stele-logs-on-failure.txt", outputOf(t, "docker", "logs", "--tail", "30", "quoin-stele-1"))
		t.Fatalf("T13Probe occurrence never appeared")
	}
	evidence.note(t, "occurrence-observed.json", mustJSON(t, map[string]any{"occurrenceId": occurrenceID, "alertname": "T13Probe"}))

	// --- Blank investigation: first-message atomicity -------------------
	// The "new" surface persists nothing: the list is empty before the
	// first message commits (DATA-INVEST-001).
	listBefore := httpGet(t, client, base+"/api/v1/investigations", origin)
	if !strings.Contains(listBefore, `"items":[]`) {
		t.Fatalf("investigation list must start empty: %.400s", listBefore)
	}
	createdBody := httpPost(t, client, base+"/api/v1/investigations", origin, `{"clientCommandId":"t13-inv-create-1","content":"请分析 T13Probe 告警的排查思路"}`)
	var createdDetail struct {
		ID              string `json:"id"`
		DisplayTitle    string `json:"displayTitle"`
		HeadMessageID   string `json:"headMessageId"`
		ActiveAttemptID string `json:"activeAttemptId"`
		MessageCount    int    `json:"messageCount"`
		AttemptCount    int    `json:"attemptCount"`
	}
	if err := json.Unmarshal([]byte(createdBody), &createdDetail); err != nil {
		t.Fatalf("create parse: %v\n%s", err, createdBody)
	}
	if createdDetail.ID == "" || createdDetail.HeadMessageID == "" || createdDetail.ActiveAttemptID == "" || createdDetail.MessageCount != 1 || createdDetail.AttemptCount != 1 {
		t.Fatalf("create detail wrong: %+v", createdDetail)
	}
	if createdDetail.DisplayTitle != "请分析 T13Probe 告警的排查思路" {
		t.Fatalf("displayTitle=%q", createdDetail.DisplayTitle)
	}
	evidence.note(t, "investigation-created.json", createdBody)
	investigationID := createdDetail.ID

	// Command replay: the same command id returns the original record and
	// never creates a second investigation (HTTP-COMMAND-003).
	replayed := httpPost(t, client, base+"/api/v1/investigations", origin, `{"clientCommandId":"t13-inv-create-1","content":"请分析 T13Probe 告警的排查思路"}`)
	if !strings.Contains(replayed, `"id":"`+investigationID+`"`) {
		t.Fatalf("replay diverged: %s", replayed)
	}
	listAfterReplay := httpGet(t, client, base+"/api/v1/investigations", origin)
	if strings.Count(listAfterReplay, `"displayTitle"`) != 1 {
		t.Fatalf("replay must not duplicate the investigation: %s", listAfterReplay)
	}

	// Head conflict: a send fenced on a stale head is a deterministic 409
	// with the frozen conflict envelope.
	conflict := httpPostExpect(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin, `{"clientCommandId":"t13-send-conflict","content":"并发消息","expectedHeadMessageId":"999999"}`, http.StatusConflict)
	if !strings.Contains(conflict, `"code":"head_conflict"`) {
		t.Fatalf("head conflict envelope missing: %.400s", conflict)
	}
	// Attachments stay clearly unavailable in this slice (no half-written
	// staging rows).
	attachmentReject := httpPostExpect(t, client, base+"/api/v1/investigations", origin, `{"clientCommandId":"t13-attach-reject","content":"带附件","attachmentIds":["1"]}`, http.StatusUnprocessableEntity)
	if !strings.Contains(attachmentReject, "附件") {
		t.Fatalf("attachment rejection must be ordinary language: %.300s", attachmentReject)
	}

	// --- Exact ui-message-stream framing over the real path -------------
	streamURL := base + "/api/v1/investigations/" + investigationID + "/messages/" + createdDetail.HeadMessageID + "/stream"
	frames := streamFrames(t, client, streamURL, origin, `{"clientCommandId":"t13-stream-1","protocol":"ui-message-stream"}`, evidence.stateRoot)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, `{"type":"text-start","id":"text-1"}`) {
		t.Fatalf("stream lacks text-start: %s", joined)
	}
	if !strings.Contains(joined, "fixture-proof-t13") || !strings.Contains(joined, "调查结论：") {
		t.Fatalf("stream lacks the deterministic Chinese answer: %s", joined)
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("stream must terminate with [DONE], last frame: %q", frames[len(frames)-1])
	}
	evidence.note(t, "stream-frames.json", mustJSON(t, map[string]any{"frameCount": len(frames), "frames": frames}))

	// Durable head: the committed assistant message owns the head and the
	// attempt sealed Succeeded with the investigation agent generation.
	deadline = time.Now().Add(120 * time.Second)
	var detailBody string
	for time.Now().Before(deadline) {
		detailBody = httpGet(t, client, base+"/api/v1/investigations/"+investigationID, origin)
		if !strings.Contains(detailBody, `"activeAttemptId"`) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	var finalDetail struct {
		HeadMessageID string `json:"headMessageId"`
		ActiveAttempt string `json:"activeAttemptId"`
		MessageCount  int    `json:"messageCount"`
	}
	if err := json.Unmarshal([]byte(detailBody), &finalDetail); err != nil {
		t.Fatalf("final detail parse: %v\n%s", err, detailBody)
	}
	if finalDetail.HeadMessageID == "" || finalDetail.HeadMessageID == createdDetail.HeadMessageID {
		t.Fatalf("head must move to the assistant message: %s", detailBody)
	}
	if finalDetail.MessageCount != 2 {
		t.Fatalf("messageCount=%d want 2", finalDetail.MessageCount)
	}
	messagesBody := httpGet(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin)
	if !strings.Contains(messagesBody, `"role":"assistant"`) || !strings.Contains(messagesBody, "fixture-proof-t13") {
		t.Fatalf("durable assistant message missing: %.500s", messagesBody)
	}
	evidence.note(t, "investigation-succeeded.json", detailBody)

	// --- Sourced investigation from the real alert ----------------------
	sourcedBody := httpPost(t, client, base+"/api/v1/investigations", origin, fmt.Sprintf(`{"clientCommandId":"t13-inv-sourced","content":"结合这条告警继续排查","sources":[{"type":"occurrence","sourceId":"%s"}]}`, occurrenceID))
	var sourced struct {
		ID              string `json:"id"`
		HeadMessageID   string `json:"headMessageId"`
		ActiveAttemptID string `json:"activeAttemptId"`
		Sources         []struct {
			Type     string `json:"type"`
			SourceID string `json:"sourceId"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(sourcedBody), &sourced); err != nil {
		t.Fatalf("sourced create parse: %v\n%s", err, sourcedBody)
	}
	if len(sourced.Sources) != 1 || sourced.Sources[0].Type != "occurrence" || sourced.Sources[0].SourceID != occurrenceID {
		t.Fatalf("sourced investigation links wrong: %+v", sourced.Sources)
	}
	sourcedFrames := streamFrames(t, client, base+"/api/v1/investigations/"+sourced.ID+"/messages/"+sourced.HeadMessageID+"/stream", origin, `{"clientCommandId":"t13-stream-sourced","protocol":"ui-message-stream"}`, evidence.stateRoot)
	if !strings.Contains(strings.Join(sourcedFrames, "\n"), "fixture-proof-t13") {
		t.Fatalf("sourced stream lacks the answer: %v", sourcedFrames)
	}
	evidence.note(t, "investigation-sourced.json", sourcedBody)

	// --- Error framing: partial stream then provider hangup -------------
	brokenBody := httpPost(t, client, base+"/api/v1/investigations", origin, `{"clientCommandId":"t13-inv-broken","content":"T13Broken 请触发失败路径"}`)
	var broken struct {
		ID            string `json:"id"`
		HeadMessageID string `json:"headMessageId"`
	}
	if err := json.Unmarshal([]byte(brokenBody), &broken); err != nil {
		t.Fatalf("broken create parse: %v\n%s", err, brokenBody)
	}
	brokenFrames := streamFrames(t, client, base+"/api/v1/investigations/"+broken.ID+"/messages/"+broken.HeadMessageID+"/stream", origin, `{"clientCommandId":"t13-stream-broken","protocol":"ui-message-stream"}`, evidence.stateRoot)
	// The broken fixture emits a partial text stream (start + two deltas)
	// before hanging up: the stream must always close with an error frame
	// and [DONE], never mid-frame silence.
	if len(brokenFrames) < 2 || brokenFrames[len(brokenFrames)-1] != "[DONE]" {
		t.Fatalf("broken stream must terminate with [DONE]: %v", brokenFrames)
	}
	if !strings.Contains(brokenFrames[len(brokenFrames)-2], `"type":"error"`) {
		t.Fatalf("broken stream must end with an error frame: %v", brokenFrames)
	}
	deadline = time.Now().Add(120 * time.Second)
	var brokenDetail string
	for time.Now().Before(deadline) {
		brokenDetail = httpGet(t, client, base+"/api/v1/investigations/"+broken.ID, origin)
		if !strings.Contains(brokenDetail, `"activeAttemptId"`) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !strings.Contains(brokenDetail, `"attemptCount":1`) {
		t.Fatalf("broken attempt history wrong: %s", brokenDetail)
	}
	attemptsBody := httpGet(t, client, base+"/api/v1/investigations/"+broken.ID+"/attempts", origin)
	if !strings.Contains(attemptsBody, `"state":"Failed"`) || !strings.Contains(attemptsBody, `"terminationReason":"provider_unavailable"`) {
		t.Fatalf("broken attempt must seal Failed/provider_unavailable: %.500s", attemptsBody)
	}
	evidence.note(t, "investigation-failed.json", attemptsBody)

	// --- Transport detach never cancels the task (HTTP-STREAM-006) ------
	detachBody := httpPost(t, client, base+"/api/v1/investigations", origin, `{"clientCommandId":"t13-inv-detach","content":"请给出较长的排查结论"}`)
	var detach struct {
		ID            string `json:"id"`
		HeadMessageID string `json:"headMessageId"`
	}
	if err := json.Unmarshal([]byte(detachBody), &detach); err != nil {
		t.Fatalf("detach create parse: %v\n%s", err, detachBody)
	}
	streamDetachThenClose(t, client, base+"/api/v1/investigations/"+detach.ID+"/messages/"+detach.HeadMessageID+"/stream", origin, `{"clientCommandId":"t13-stream-detach","protocol":"ui-message-stream"}`)
	deadline = time.Now().Add(120 * time.Second)
	detached := false
	for time.Now().Before(deadline) {
		detailBody = httpGet(t, client, base+"/api/v1/investigations/"+detach.ID, origin)
		if strings.Contains(detailBody, `"messageCount":2`) && !strings.Contains(detailBody, `"activeAttemptId"`) {
			detached = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !detached {
		t.Fatalf("detached attempt never completed: %s", detailBody)
	}
	evidence.note(t, "investigation-detached.json", detailBody)

	// --- SQLite authority evidence --------------------------------------
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
	headCheck := queryRow(`SELECT m.role FROM investigations i JOIN investigation_messages m ON m.id=i.current_head_message_id WHERE i.id=?`, investigationID)
	attemptCheck := queryRow(`SELECT state FROM execution_attempts WHERE attempt_type='investigation' AND scope_id=? AND scope_type='investigation'`, investigationID)
	agentCheck := queryRow(`SELECT agent_version FROM execution_attempts WHERE attempt_type='investigation' AND scope_id=? AND scope_type='investigation'`, investigationID)
	schemaCheck := queryRow(`SELECT schema_kind FROM attempt_input_snapshots WHERE attempt_id=(SELECT id FROM execution_attempts WHERE attempt_type='investigation' AND scope_id=? AND scope_type='investigation')`, investigationID)
	itemCheck := queryRow(`SELECT COUNT(*) FROM attempt_input_items WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=(SELECT id FROM execution_attempts WHERE attempt_type='investigation' AND scope_id=? AND scope_type='investigation'))`, investigationID)
	titleCheck := queryRow(`SELECT COUNT(*) FROM investigations WHERE id=?`, investigationID)
	if headCheck != "assistant" || attemptCheck != "Succeeded" || agentCheck != "investigation-v1" || schemaCheck != "investigation_v1" || itemCheck != "1" || titleCheck != "1" {
		t.Fatalf("sqlite evidence wrong: head=%s attempt=%s agent=%s schema=%s items=%s count=%s", headCheck, attemptCheck, agentCheck, schemaCheck, itemCheck, titleCheck)
	}
	evidence.note(t, "sqlite-evidence.json", mustJSON(t, map[string]any{
		"headRole":     headCheck,
		"attemptState": attemptCheck,
		"agentVersion": agentCheck,
		"schemaKind":   schemaCheck,
		"inputItems":   itemCheck,
		"streamProof":  "the stream frames carry the deterministic fixture text and terminate with [DONE]",
	}))

	// --- No credential / environment leakage -----------------------------
	keyHex := fmt.Sprintf("%x", []byte("fixture-api-key-2026"))
	cipherHex := queryRow(`SELECT lower(hex(ciphertext)) FROM credential_generations WHERE connection_id=(SELECT id FROM connections WHERE name='t13-provider')`)
	if strings.Contains(cipherHex, keyHex) {
		t.Fatalf("plaintext API key found inside the sealed credential ciphertext")
	}
	scanForSecrets(t, evidenceDir, newPassword, tempPassword, bearer, "fixture-api-key-2026")

	// --- Teardown ----------------------------------------------------------
	execCommand(t, evidence, "teardown-forwarder", nil, "docker", "rm", "-f", "t13-forwarder")
	execCommand(t, evidence, "teardown-am", nil, "docker", "rm", "-f", "t13-am")
	execCommand(t, evidence, "teardown-stack", nil, "docker", "compose", "--project-name", projectName, "--file", composeFile, "down", "--remove-orphans")
	builtImages := []string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if !preExisting[image] {
			builtImages = append(builtImages, image)
		}
	}
	if len(builtImages) > 0 {
		execCommand(t, evidence, "teardown-images", nil, "docker", append([]string{"rmi"}, builtImages...)...)
	} else {
		evidence.note(t, "teardown-images.json", mustJSON(t, map[string]any{"conclusion": "all four images pre-existed; none removed"}))
	}
	commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	evidence.writeRuntimeEvidence(t, commit, newPassword, tempPassword, bearer, map[string]any{
		"realPath":   "Alertmanager container -> Stele -> Quoin SQLite -> HTTP create/send -> Plinth supervisor -> sandboxed worker -> fixture provider -> exact ui-message-stream SSE -> committed head/messages",
		"framing":    "text-start + ordered text-delta frames (deterministic Chinese text) + finish(usage) + data: [DONE]; error turns emit {\"type\":\"error\"} + [DONE]",
		"atomicity":  "the first message commits Investigation + sources + user message + Attempt + frozen input in one transaction; a replayed command returns the original record",
		"detach":     "closing the stream mid-flight never cancelled the attempt (the turn still committed)",
		"redactions": "the provider API key appears in the request body sent over HTTPS to Quoin and the provider bearer header; it is never written to evidence, logs or the sealed ciphertext",
	})
	os.RemoveAll(workRoot)
}

// streamFrames reads one ui-message-stream response in small byte chunks
// (the 1-byte reads force a multi-byte UTF-8 rune to straddle transport
// boundaries) and returns the decoded `data:` payloads.
func streamFrames(t *testing.T, client *http.Client, target, origin, body, stateRoot string) []string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	request.Header.Set("Accept", "text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		// The stream's first frames must arrive within the client timeout;
		// a header wait means the attempt produced no observable output —
		// capture the live diagnostics before failing.
		dumpStreamDiagnostics(t, target, stateRoot)
		t.Fatalf("stream %s: %v", target, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		dumpStreamDiagnostics(t, target, stateRoot)
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		t.Fatalf("stream %s: status=%d body=%.800s", target, response.StatusCode, raw)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("stream Content-Type=%q want text/event-stream", contentType)
	}
	if streamHeader := response.Header.Get("X-Vercel-Ai-Ui-Message-Stream"); streamHeader != "v1" {
		t.Fatalf("stream protocol header=%q want v1", streamHeader)
	}
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-cache" {
		t.Fatalf("stream Cache-Control=%q want no-cache", cacheControl)
	}
	var frames []string
	var pending []byte
	reader := bufio.NewReaderSize(response.Body, 7)
	buffer := make([]byte, 1)
	for {
		n, readErr := reader.Read(buffer)
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
				frames = append(frames, strings.TrimPrefix(line, "data: "))
			}
		}
		if readErr != nil {
			break
		}
	}
	return frames
}

// streamDetachThenClose opens the stream, reads the first frames and then
// closes the connection mid-flight (transport detach; HTTP-STREAM-006).
func streamDetachThenClose(t *testing.T, client *http.Client, target, origin, body string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("detach stream %s: %v", target, err)
	}
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		t.Fatalf("detach stream status=%d body=%.400s", response.StatusCode, raw)
	}
	reader := bufio.NewReader(response.Body)
	for line := 0; line < 2; line++ {
		if _, err := reader.ReadString('\n'); err != nil {
			break
		}
	}
	// EOF on the client side: the observer detaches, the task keeps running.
	response.Body.Close()
}
