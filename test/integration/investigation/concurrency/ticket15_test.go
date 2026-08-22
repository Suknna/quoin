package concurrency

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTicket15 proves the Investigation head-concurrency capability over
// the real compose stack: simultaneous sends serialize on the head fence,
// command replays stay idempotent, Stop/Retry are explicit commands,
// Undo-vs-result and cancel-vs-result resolve by SQLite commit order, the
// withdrawn branch stays read-only, and a detached observer (the
// back-button case) re-attaches to the committed state.
func TestTicket15(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T15 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	evidence.env = append(evidence.env, "QUOIN_FORCE_IMAGE_BUILD=1")
	// Restricted networks need the module mirror passed into the image
	// build (the same authority as the e2e server bootstrap:
	// QUOIN_IMAGE_GOPROXY falling back to `go env GOPROXY`).
	imageProxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if imageProxy == "" {
		imageProxy = strings.TrimSpace(outputOf(t, "go", "env", "GOPROXY"))
	}
	if imageProxy != "" {
		evidence.env = append(evidence.env, "QUOIN_IMAGE_GOPROXY="+imageProxy)
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
	startFixture := func(logName string) *exec.Cmd {
		fixtureLog, err := os.OpenFile(filepath.Join(evidence.dir, logName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(fixtureBinary, "-address", fmt.Sprintf("0.0.0.0:%d", fixturePort))
		cmd.Env = evidence.env
		cmd.Stdout = fixtureLog
		cmd.Stderr = fixtureLog
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ready := false
		for probe := 0; probe < 40; probe++ {
			time.Sleep(200 * time.Millisecond)
			logBody, _ := os.ReadFile(filepath.Join(evidence.dir, logName))
			logText := string(logBody)
			// The fixture logs "listening" BEFORE binding: a bind failure on
			// the next line (or a dead process) must never count as ready.
			if strings.Contains(logText, "address already in use") {
				t.Fatalf("fixture provider could not bind %d (stale listener?): %s", fixturePort, logText)
			}
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("fixture provider exited before becoming ready: %s", logText)
			}
			if strings.Contains(logText, "listening") {
				ready = true
				break
			}
		}
		if !ready {
			t.Fatalf("fixture provider never reported listening (%s)", logName)
		}
		return cmd
	}
	fixtureCmd := startFixture("fixture-provider.log")
	// The restarted outage fixture (and the first one) must never outlive
	// the test: kill whichever instance the variable last held.
	t.Cleanup(func() {
		fixtureCmd.Process.Kill()
		_ = fixtureCmd.Wait()
	})
	stopFixture := func(cmd *exec.Cmd) {
		cmd.Process.Kill()
		_ = cmd.Wait()
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
	runtimeRow := httpGet(t, client, base+"/api/v1/runtime", origin)
	var runtimeView struct {
		Plinth struct {
			RowVersion int64 `json:"rowVersion"`
		} `json:"plinth"`
	}
	if err := json.Unmarshal([]byte(runtimeRow), &runtimeView); err != nil {
		t.Fatalf("runtime view parse: %v\n%s", err, runtimeRow)
	}
	prepare := httpPost(t, client, base+"/api/v1/runtime-slots/plinth/registration/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t15-prepare-%s","expectedRowVersion":%d}`, randomSecret(t, 8), runtimeView.Plinth.RowVersion))
	var prepareObj struct {
		RegistrationTokenHandle string `json:"registrationTokenHandle"`
	}
	if err := json.Unmarshal([]byte(prepare), &prepareObj); err != nil {
		t.Fatalf("prepare parse: %v\n%s", err, prepareObj)
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
	registerCmd.Dir = repoRoot(t)
	evidence.logCommand(t, "plinth-register", registerCmd)
	plinthConnected := awaitCondition(60*time.Second, func() bool {
		body := httpGet(t, client, base+"/api/v1/runtime", origin)
		var status struct {
			Plinth struct {
				Connected bool `json:"connected"`
			} `json:"plinth"`
		}
		return json.Unmarshal([]byte(body), &status) == nil && status.Plinth.Connected
	})
	if !plinthConnected {
		t.Fatal("plinth control stream never attached after registration")
	}

	// --- Enabled qualified model provider (real probe first) ------------
	httpPost(t, client, base+"/api/v1/connections", origin, fmt.Sprintf(`{"clientCommandId":"t15-create-conn-1","name":"t15-provider","connection":{"type":"model_provider","baseUrl":"http://%s:%d","chatModelId":"fixture-chat-1","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}`, gatewayIP, fixturePort))
	httpPost(t, client, base+"/api/v1/connections/t15-provider/probe", origin, `{"clientCommandId":"t15-probe-1"}`)
	var probeResultID string
	if !awaitCondition(120*time.Second, func() bool {
		listBody := httpGet(t, client, base+"/api/v1/connections/t15-provider/probe-results", origin)
		var listObj struct {
			Items []struct {
				ID      string `json:"id"`
				Outcome string `json:"outcome"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(listBody), &listObj); err != nil {
			return false
		}
		for _, item := range listObj.Items {
			if item.Outcome == "passed" {
				probeResultID = item.ID
			}
		}
		return probeResultID != ""
	}) {
		t.Fatal("t15-provider never qualified (probe did not pass)")
	}
	connDetail := httpGet(t, client, base+"/api/v1/connections/t15-provider", origin)
	var connObj struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(connDetail), &connObj); err != nil {
		t.Fatalf("connection detail parse: %v\n%s", err, connDetail)
	}
	httpPost(t, client, base+"/api/v1/connections/t15-provider/enable", origin, fmt.Sprintf(`{"clientCommandId":"t15-enable-1","expectedRowVersion":%d,"qualifiedProbeResultId":"%s"}`, connObj.RowVersion, probeResultID))

	// Shared projections -------------------------------------------------
	type attemptItem struct {
		ID                string  `json:"id"`
		State             string  `json:"state"`
		RowVersion        int64   `json:"rowVersion"`
		TerminationReason *string `json:"terminationReason"`
	}
	attemptsOf := func(investigationID string) []attemptItem {
		body := httpGet(t, client, base+"/api/v1/investigations/"+investigationID+"/attempts", origin)
		var listObj struct {
			Items []attemptItem `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &listObj); err != nil {
			t.Fatalf("attempts parse (%s): %v\n%s", investigationID, err, body)
		}
		return listObj.Items
	}
	detailOf := func(investigationID string) (head string, active string, messages int64, attempts int64) {
		body := httpGet(t, client, base+"/api/v1/investigations/"+investigationID, origin)
		var detail struct {
			HeadMessageID   string `json:"headMessageId"`
			ActiveAttemptID string `json:"activeAttemptId"`
			MessageCount    int64  `json:"messageCount"`
			AttemptCount    int64  `json:"attemptCount"`
		}
		if err := json.Unmarshal([]byte(body), &detail); err != nil {
			t.Fatalf("detail parse (%s): %v\n%s", investigationID, err, body)
		}
		return detail.HeadMessageID, detail.ActiveAttemptID, detail.MessageCount, detail.AttemptCount
	}
	awaitIdle := func(investigationID string) {
		if !awaitCondition(120*time.Second, func() bool {
			_, active, _, _ := detailOf(investigationID)
			return active == ""
		}) {
			_, active, _, _ := detailOf(investigationID)
			t.Fatalf("investigation %s never settled (activeAttempt=%q)", investigationID, active)
		}
	}
	createInvestigation := func(commandID, content string) string {
		body := httpPost(t, client, base+"/api/v1/investigations", origin, fmt.Sprintf(`{"clientCommandId":"%s","content":"%s"}`, commandID, content))
		var created struct {
			ID            string `json:"id"`
			HeadMessageID string `json:"headMessageId"`
		}
		if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == "" {
			t.Fatalf("create (%s) parse: %v\n%s", commandID, err, body)
		}
		return created.ID
	}
	streamURL := func(investigationID, messageID string) string {
		return base + "/api/v1/investigations/" + investigationID + "/messages/" + messageID + "/stream"
	}

	// ===================================================================
	// A. Simultaneous sends serialize on the head fence (DATA-INVEST-001).
	// ===================================================================
	invA := createInvestigation("t15-inv-a", "T15 并发第一轮：请给出结论")
	awaitIdle(invA)
	headA, _, _, _ := detailOf(invA)
	first := httpPostAsync(base+"/api/v1/investigations/"+invA+"/messages", origin, fmt.Sprintf(`{"clientCommandId":"t15-race-send-a","content":"并发消息甲","expectedHeadMessageId":"%s"}`, headA), client)
	second := httpPostAsync(base+"/api/v1/investigations/"+invA+"/messages", origin, fmt.Sprintf(`{"clientCommandId":"t15-race-send-b","content":"并发消息乙","expectedHeadMessageId":"%s"}`, headA), client)
	statusA, bodyA := first()
	statusB, bodyB := second()
	if !((statusA == 201 && statusB == 409) || (statusA == 409 && statusB == 201)) {
		t.Fatalf("simultaneous sends must yield one 201 and one 409, got %d/%d bodies=%.300s %.300s", statusA, statusB, bodyA, bodyB)
	}
	if !strings.Contains(bodyA+bodyB, `"code":"head_conflict"`) && !strings.Contains(bodyA+bodyB, `"code":"active_conflict"`) {
		t.Fatalf("loser conflict envelope missing: %.400s %.400s", bodyA, bodyB)
	}
	winnerCommand, winnerContent := "t15-race-send-a", "并发消息甲"
	if statusB == 201 {
		winnerCommand, winnerContent = "t15-race-send-b", "并发消息乙"
	}
	awaitIdle(invA)
	_, _, messageCount, _ := detailOf(invA)
	if messageCount != 4 {
		t.Fatalf("invA messages=%d want 4 (first turn + the single winning send with its reply)", messageCount)
	}
	evidence.note(t, "simultaneous-send.json", mustJSON(t, map[string]any{"statuses": []int{statusA, statusB}, "bodies": []string{bodyA, bodyB}}))

	// ===================================================================
	// B. Command replay: same id + digest replays; a different request
	//    with the same id conflicts (HTTP-COMMAND-003/007).
	// ===================================================================
	replayedSend := httpPost(t, client, base+"/api/v1/investigations/"+invA+"/messages", origin, fmt.Sprintf(`{"clientCommandId":"%s","content":"%s","expectedHeadMessageId":"%s"}`, winnerCommand, winnerContent, headA))
	if !strings.Contains(replayedSend, `"role":"user"`) || !strings.Contains(replayedSend, winnerContent) {
		t.Fatalf("send replay diverged: %.400s", replayedSend)
	}
	_, _, messageCount, _ = detailOf(invA)
	if messageCount != 4 {
		t.Fatalf("replay must not append (messages=%d)", messageCount)
	}
	reused := httpPostExpect(t, client, base+"/api/v1/investigations/"+invA+"/messages", origin, fmt.Sprintf(`{"clientCommandId":"%s","content":"不同的正文","expectedHeadMessageId":"%s"}`, winnerCommand, headA), http.StatusConflict)
	if !strings.Contains(reused, `"code":"command_id_reused"`) {
		t.Fatalf("reused command envelope missing: %.400s", reused)
	}

	// ===================================================================
	// C. Head conflict: a stale fence is a deterministic 409.
	// ===================================================================
	headNow, _, _, _ := detailOf(invA)
	staleHead := httpPostExpect(t, client, base+"/api/v1/investigations/"+invA+"/messages", origin, fmt.Sprintf(`{"clientCommandId":"t15-stale-head","content":"过期围栏","expectedHeadMessageId":"%s"}`, staleHeadID(t, invA, client, base, origin)), http.StatusConflict)
	if !strings.Contains(staleHead, `"code":"head_conflict"`) || !strings.Contains(staleHead, `"headMessageId":"`+headNow+`"`) {
		t.Fatalf("stale head conflict envelope wrong: %.400s", staleHead)
	}

	// ===================================================================
	// D. Stop (cancellation fence) races: cancel-first fences a running
	//    turn to Cancelled with the stream closing on the cancelled
	//    terminal frame; success-first answers the completed object.
	// ===================================================================
	invD := createInvestigation("t15-inv-stop", "T15 停止验证：请围绕容量、依赖与恢复顺序给出完整结论，并补充现场处置建议。")
	_, activeD, _, _ := detailOf(invD)
	if activeD == "" {
		t.Fatal("stop scenario attempt never became active")
	}
	stopRecorder := &frameRecorder{done: make(chan struct{})}
	recordStream(t, client, streamURL(invD, headOf(t, client, base, origin, invD)), origin, `{"clientCommandId":"t15-stream-stop","protocol":"ui-message-stream"}`, stopRecorder)
	if !awaitFrames(t, stopRecorder, 2, 30*time.Second) {
		t.Fatalf("stop stream never produced frames: %v", stopRecorder.snapshot())
	}
	var stopRowVersion int64
	for _, attempt := range attemptsOf(invD) {
		if attempt.ID == activeD {
			stopRowVersion = attempt.RowVersion
		}
	}
	if stopRowVersion == 0 {
		t.Fatalf("active attempt %s not found in attempts list", activeD)
	}
	stopResponse := httpPost(t, client, base+"/api/v1/investigations/"+invD+"/attempts/"+activeD+"/cancel", origin, fmt.Sprintf(`{"clientCommandId":"t15-stop-1","expectedRowVersion":%d}`, stopRowVersion))
	if !strings.Contains(stopResponse, `"state":"Cancelling"`) {
		t.Fatalf("stop fence must answer Cancelling: %.400s", stopResponse)
	}
	select {
	case <-stopRecorder.done:
	case <-time.After(60 * time.Second):
		t.Fatalf("stop stream never closed: %v", stopRecorder.snapshot())
	}
	stopFrames := stopRecorder.snapshot()
	if len(stopFrames) == 0 || stopFrames[len(stopFrames)-1] != "[DONE]" {
		t.Fatalf("stop stream must terminate with [DONE]: %v", stopFrames)
	}
	if !containsFrame(stopFrames, `"type":"error"`) || !containsFrameText(stopFrames, "回复已停止") {
		t.Fatalf("stop stream must close with the cancelled terminal frame: %v", stopFrames)
	}
	if !awaitCondition(60*time.Second, func() bool {
		for _, attempt := range attemptsOf(invD) {
			if attempt.ID == activeD {
				return attempt.State == "Cancelled"
			}
		}
		return false
	}) {
		t.Fatalf("stopped attempt never reached Cancelled: %+v", attemptsOf(invD))
	}
	// Cancelling never sticks: the final state is terminal.
	_, activeAfter, messageCountD, _ := detailOf(invD)
	if activeAfter != "" || messageCountD != 1 {
		t.Fatalf("stopped turn must leave no assistant message (active=%q messages=%d)", activeAfter, messageCountD)
	}
	// Terminal answers completed: stopping the already-cancelled attempt
	// returns the object (200), never a conflict.
	cancelAgain := httpPost(t, client, base+"/api/v1/investigations/"+invD+"/attempts/"+activeD+"/cancel", origin, `{"clientCommandId":"t15-stop-2","expectedRowVersion":999}`)
	if !strings.Contains(cancelAgain, `"state":"Cancelled"`) {
		t.Fatalf("terminal stop must answer the completed object: %.400s", cancelAgain)
	}
	// Success-first: stopping the sealed first turn of invA answers
	// Succeeded (HTTP-COMMAND-005) even with a stale expected version.
	sealed := attemptsOf(invA)[0]
	stopSealed := httpPost(t, client, base+"/api/v1/investigations/"+invA+"/attempts/"+sealed.ID+"/cancel", origin, `{"clientCommandId":"t15-stop-sealed","expectedRowVersion":1}`)
	if !strings.Contains(stopSealed, `"state":"Succeeded"`) {
		t.Fatalf("stop after success must answer the completed object: %.400s", stopSealed)
	}
	evidence.note(t, "stop-race.json", mustJSON(t, map[string]any{"cancelFirst": stopResponse, "streamFrames": stopFrames, "terminalStop": cancelAgain, "successFirst": stopSealed}))

	// ===================================================================
	// E. Undo versus a running result (DATA-INVEST-002): the withdrawal
	//    fence wins the commit order, the late result stays audit-only.
	// ===================================================================
	invE := createInvestigation("t15-inv-undo-race", "T15 撤回竞态：请围绕容量、依赖与恢复顺序给出完整结论。")
	headE := headOf(t, client, base, origin, invE)
	undoRecorder := &frameRecorder{done: make(chan struct{})}
	recordStream(t, client, streamURL(invE, headE), origin, `{"clientCommandId":"t15-stream-undo","protocol":"ui-message-stream"}`, undoRecorder)
	if !awaitFrames(t, undoRecorder, 2, 30*time.Second) {
		t.Fatalf("undo stream never produced frames: %v", undoRecorder.snapshot())
	}
	undoResponse := httpPost(t, client, base+"/api/v1/investigations/"+invE+"/undo", origin, fmt.Sprintf(`{"clientCommandId":"t15-undo-1","expectedHeadMessageId":"%s"}`, headE))
	var undoDetail struct {
		HeadMessageID string `json:"headMessageId"`
	}
	if err := json.Unmarshal([]byte(undoResponse), &undoDetail); err != nil {
		t.Fatalf("undo response parse: %v\n%s", err, undoResponse)
	}
	if undoDetail.HeadMessageID != "" {
		t.Fatalf("undo of the only turn must clear the head, got %q", undoDetail.HeadMessageID)
	}
	select {
	case <-undoRecorder.done:
	case <-time.After(60 * time.Second):
		t.Fatalf("undo stream never closed: %v", undoRecorder.snapshot())
	}
	undoFrames := undoRecorder.snapshot()
	if len(undoFrames) == 0 || undoFrames[len(undoFrames)-1] != "[DONE]" {
		t.Fatalf("undo stream must close deterministically: %v", undoFrames)
	}
	if !awaitCondition(60*time.Second, func() bool {
		_, active, _, _ := detailOf(invE)
		return active == ""
	}) {
		t.Fatal("undone attempt never settled (cancel ack missing)")
	}
	messagesE := httpGet(t, client, base+"/api/v1/investigations/"+invE+"/messages", origin)
	if !strings.Contains(messagesE, `"status":"withdrawn"`) || strings.Contains(messagesE, `"role":"assistant"`) {
		t.Fatalf("withdrawn turn must stay assistant-free and marked withdrawn: %.600s", messagesE)
	}
	// Undo replay is idempotent state reporting.
	httpPost(t, client, base+"/api/v1/investigations/"+invE+"/undo", origin, fmt.Sprintf(`{"clientCommandId":"t15-undo-1","expectedHeadMessageId":"%s"}`, headE))
	// The branch continues from the empty head with an explicit null fence.
	resend := httpPost(t, client, base+"/api/v1/investigations/"+invE+"/messages", origin, `{"clientCommandId":"t15-undo-resend","content":"T15 撤回后重发：请给出结论","expectedHeadMessageId":null}`)
	if !strings.Contains(resend, `"role":"user"`) {
		t.Fatalf("resend after full withdrawal failed: %.400s", resend)
	}
	awaitIdle(invE)
	headE2, _, messagesE2, _ := detailOf(invE)
	// 3 rows: the withdrawn turn stays as audit + the resumed user/assistant pair.
	if headE2 == "" || headE2 == headE || messagesE2 != 3 {
		t.Fatalf("resumed branch wrong: head=%q messages=%d", headE2, messagesE2)
	}
	evidence.note(t, "undo-race.json", mustJSON(t, map[string]any{"undoResponse": undoResponse, "streamFrames": undoFrames, "resend": resend}))

	// ===================================================================
	// F. Undo after success withdraws the whole turn; a stale head
	//    conflicts instead of withdrawing a sealed reply.
	// ===================================================================
	headF, _, _, _ := detailOf(invA) // head = assistant of the winning send
	undoF := httpPost(t, client, base+"/api/v1/investigations/"+invA+"/undo", origin, fmt.Sprintf(`{"clientCommandId":"t15-undo-f","expectedHeadMessageId":"%s"}`, headF))
	if !strings.Contains(undoF, `"messageCount":4`) {
		t.Fatalf("undo after success response wrong: %.400s", undoF)
	}
	headF2, _, _, _ := detailOf(invA)
	if headF2 == "" || headF2 == headF {
		t.Fatalf("head must fall back to the remaining active turn: %q", headF2)
	}
	staleUndo := httpPostExpect(t, client, base+"/api/v1/investigations/"+invA+"/undo", origin, fmt.Sprintf(`{"clientCommandId":"t15-undo-stale","expectedHeadMessageId":"%s"}`, headF), http.StatusConflict)
	if !strings.Contains(staleUndo, `"code":"head_conflict"`) {
		t.Fatalf("stale undo envelope missing: %.400s", staleUndo)
	}
	evidence.note(t, "undo-after-success.json", undoF)

	// ===================================================================
	// G. Retry across a real provider outage: the failed turn re-answers
	//    through a new attempt once the provider is back.
	// ===================================================================
	stopFixture(fixtureCmd)
	invG := createInvestigation("t15-inv-retry", "T15 重试恢复路径：请给出结论")
	if !awaitCondition(120*time.Second, func() bool {
		for _, attempt := range attemptsOf(invG) {
			if attempt.State == "Failed" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("outage attempt never failed: %+v", attemptsOf(invG))
	}
	var failedAttempt attemptItem
	for _, attempt := range attemptsOf(invG) {
		if attempt.State == "Failed" {
			failedAttempt = attempt
		}
	}
	headG := headOf(t, client, base, origin, invG)
	fixtureCmd = startFixture("fixture-provider-restart.log")
	retryResponse := httpPostExpect(t, client, base+"/api/v1/investigations/"+invG+"/attempts/"+failedAttempt.ID+"/retry", origin, `{"clientCommandId":"t15-retry-1"}`, http.StatusAccepted)
	var retried struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(retryResponse), &retried); err != nil || retried.ID == "" || retried.ID == failedAttempt.ID {
		t.Fatalf("retry response wrong: %v %s", err, retryResponse)
	}
	// The retry attempt runs the real path and streams the recovered answer.
	retryRecorder := &frameRecorder{done: make(chan struct{})}
	recordStream(t, client, streamURL(invG, headG), origin, fmt.Sprintf(`{"clientCommandId":"t15-stream-retry","protocol":"ui-message-stream"}`), retryRecorder)
	select {
	case <-retryRecorder.done:
	case <-time.After(90 * time.Second):
		t.Fatalf("retry stream never closed: %v", retryRecorder.snapshot())
	}
	retryFrames := retryRecorder.snapshot()
	if !containsFrameText(retryFrames, "fixture-proof-t13") || retryFrames[len(retryFrames)-1] != "[DONE]" {
		t.Fatalf("retry stream must deliver the recovered answer: %v", retryFrames)
	}
	awaitIdle(invG)
	headG2, _, messagesG, attemptsG := detailOf(invG)
	if headG2 == headG || messagesG != 2 || attemptsG != 2 {
		t.Fatalf("retry outcome wrong: head=%s messages=%d attempts=%d", headG2, messagesG, attemptsG)
	}
	// Guards: retrying the now-Succeeded attempt conflicts.
	httpPostExpect(t, client, base+"/api/v1/investigations/"+invG+"/attempts/"+retried.ID+"/retry", origin, `{"clientCommandId":"t15-retry-done"}`, http.StatusConflict)
	// A withdrawn turn never re-enters the model context.
	invG2 := createInvestigation("t15-inv-retry-undo", "T13Broken 请触发失败路径")
	if !awaitCondition(120*time.Second, func() bool {
		for _, attempt := range attemptsOf(invG2) {
			if attempt.State == "Failed" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("broken attempt never failed: %+v", attemptsOf(invG2))
	}
	httpPost(t, client, base+"/api/v1/investigations/"+invG2+"/undo", origin, fmt.Sprintf(`{"clientCommandId":"t15-retry-undo-cmd","expectedHeadMessageId":"%s"}`, headOf(t, client, base, origin, invG2)))
	withdrawnRetry := httpPostExpect(t, client, base+"/api/v1/investigations/"+invG2+"/attempts/"+attemptsOf(invG2)[0].ID+"/retry", origin, `{"clientCommandId":"t15-retry-withdrawn"}`, http.StatusConflict)
	if !strings.Contains(withdrawnRetry, "撤回") {
		t.Fatalf("withdrawn retry envelope must say so: %.400s", withdrawnRetry)
	}
	// An active attempt blocks retrying an older failed turn: invG's
	// original attempt stays Failed while a new slow turn runs.
	sendG2 := httpPost(t, client, base+"/api/v1/investigations/"+invG+"/messages", origin, fmt.Sprintf(`{"clientCommandId":"t15-retry-active-send","content":"T15 活动冲突：请围绕容量、依赖与恢复顺序给出完整结论。","expectedHeadMessageId":"%s"}`, headG2))
	if !strings.Contains(sendG2, `"role":"user"`) {
		t.Fatalf("invG second send failed: %.400s", sendG2)
	}
	retryActive := httpPostExpect(t, client, base+"/api/v1/investigations/"+invG+"/attempts/"+failedAttempt.ID+"/retry", origin, `{"clientCommandId":"t15-retry-active"}`, http.StatusConflict)
	if !strings.Contains(retryActive, "正在生成") {
		t.Fatalf("active retry guard envelope wrong: %.400s", retryActive)
	}
	awaitIdle(invG)
	evidence.note(t, "retry-path.json", mustJSON(t, map[string]any{"failedAttempt": failedAttempt, "retryResponse": retryResponse, "streamFrames": retryFrames, "withdrawnRetry": withdrawnRetry, "activeRetry": retryActive}))

	// ===================================================================
	// H. Reconnect / back-button state: detaching the observer never
	//    cancels the turn; a returning observer replays the committed
	//    state exactly once (HTTP-STREAM-003/004/006).
	// ===================================================================
	invH := createInvestigation("t15-inv-back", "T15 返回重连：请围绕容量、依赖与恢复顺序给出完整结论，并补充建议。")
	headH := headOf(t, client, base, origin, invH)
	// The back-button observer leaves mid-stream: the transport detaches
	// while the domain turn keeps running (HTTP-STREAM-006).
	detachStream(t, client, streamURL(invH, headH), origin, `{"clientCommandId":"t15-stream-detach","protocol":"ui-message-stream"}`)
	awaitIdle(invH)
	headH2, _, messagesH, _ := detailOf(invH)
	if headH2 == headH || messagesH != 2 {
		t.Fatalf("detached turn must still commit: head=%s messages=%d", headH2, messagesH)
	}
	// The returning observer gets the committed assistant message.
	returnRecorder := &frameRecorder{done: make(chan struct{})}
	recordStream(t, client, streamURL(invH, headH), origin, `{"clientCommandId":"t15-stream-return","protocol":"ui-message-stream"}`, returnRecorder)
	select {
	case <-returnRecorder.done:
	case <-time.After(60 * time.Second):
		t.Fatalf("return stream never closed: %v", returnRecorder.snapshot())
	}
	returnFrames := returnRecorder.snapshot()
	proofs := 0
	for _, frame := range returnFrames {
		if strings.Contains(frame, "fixture-proof-t13") {
			proofs++
		}
	}
	if proofs != 1 || returnFrames[len(returnFrames)-1] != "[DONE]" {
		t.Fatalf("returning observer must replay the committed answer exactly once: %v", returnFrames)
	}
	evidence.note(t, "reconnect.json", mustJSON(t, map[string]any{"finalHead": headH2, "returnFrames": returnFrames}))

	// --- SQLite authority evidence --------------------------------------
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Fatalf("host sqlite3 is required for the evidence path: %v", err)
	}
	dbPath := filepath.Join(stateRoot, "quoin", "compose", "data", "quoin.db")
	queryRow := func(query string, args ...string) string {
		t.Helper()
		for _, arg := range args {
			query = strings.Replace(query, "?", arg, 1)
		}
		out, err := exec.Command("sqlite3", dbPath, query).CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite query %q: %v\n%s", query, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	headNullCheck := queryRow(`SELECT COALESCE(current_head_message_id,'NULL') FROM investigations WHERE id=?`, invG2)
	withdrawnCheck := queryRow(`SELECT COUNT(*) FROM investigation_messages WHERE status='withdrawn'`)
	undoAuditCheck := queryRow(`SELECT COUNT(*) FROM audit_events WHERE action='investigation.message.undo' AND outcome='success'`)
	cancelledCheck := queryRow(`SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='investigation' AND state='Cancelled' AND termination_reason='cancelled'`)
	retryCheck := queryRow(`SELECT COUNT(*) FROM investigations i WHERE (SELECT COUNT(*) FROM execution_attempts a WHERE a.scope_type='investigation' AND a.scope_id=i.id)=2 AND (SELECT COUNT(*) FROM investigation_messages m WHERE m.investigation_id=i.id AND m.role='assistant')=1`)
	noUnwithdrawCheck := queryRow(`SELECT COUNT(*) FROM investigation_messages m WHERE m.status='withdrawn' AND EXISTS (SELECT 1 FROM attempt_input_items t WHERE t.investigation_message_id=m.id AND t.snapshot_id IN (SELECT s.id FROM attempt_input_snapshots s JOIN execution_attempts a ON a.id=s.attempt_id WHERE a.created_at > m.created_at))`)
	if withdrawnCheck == "0" || undoAuditCheck == "0" || cancelledCheck == "0" || retryCheck == "0" || noUnwithdrawCheck != "0" {
		t.Fatalf("sqlite evidence wrong: headNull=%s withdrawn=%s undoAudit=%s cancelled=%s retry=%s unwithdraw=%s", headNullCheck, withdrawnCheck, undoAuditCheck, cancelledCheck, retryCheck, noUnwithdrawCheck)
	}
	evidence.note(t, "sqlite-evidence.json", mustJSON(t, map[string]any{
		"withdrawnHead":        headNullCheck,
		"withdrawnMessages":    withdrawnCheck,
		"undoAuditEvents":      undoAuditCheck,
		"cancelledAttempts":    cancelledCheck,
		"retryInvestigations":  retryCheck,
		"withdrawnInSnapshots": noUnwithdrawCheck,
	}))

	// --- No credential / environment leakage -----------------------------
	scanForSecrets(t, evidenceDir, newPassword, tempPassword, "fixture-api-key-2026")

	// --- Teardown ----------------------------------------------------------
	evidence.run(t, "teardown-stack", nil, "docker", "compose", "--project-name", projectName, "--file", composeFile, "down", "--remove-orphans")
	builtImages := []string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if !preExisting[image] {
			builtImages = append(builtImages, image)
		}
	}
	if len(builtImages) > 0 {
		evidence.run(t, "teardown-images", nil, "docker", append([]string{"rmi"}, builtImages...)...)
	} else {
		evidence.note(t, "teardown-images.json", mustJSON(t, map[string]any{"conclusion": "all four images pre-existed; none removed"}))
	}
	commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	dirtyDigest := sha256Hex([]byte(outputOf(t, "git", "status", "--porcelain")))
	evidence.writeRuntimeEvidence(t, commit, dirtyDigest, map[string]any{
		"realPath": "HTTP create/send/undo/stop/retry over the compose stack -> Plinth supervisor -> sandboxed worker -> fixture provider -> ui-message-stream SSE -> SQLite committed head/messages/attempts",
		"races": map[string]any{
			"simultaneousSend": map[string]any{"statuses": []int{statusA, statusB}, "deterministicOutcome": "one 201 + one 409 (head_conflict/active_conflict)"},
			"stopVsResult":     map[string]any{"cancelFirst": "Cancelling -> CancelAck -> Cancelled with the stream closing on the cancelled terminal frame", "successFirst": "200 Succeeded (completed object, never 409)"},
			"undoVsResult":     map[string]any{"undoFirst": "withdrawn turn + cancelled attempt, no assistant message, head NULL, late result audit-only", "resultFirst": "409 head_conflict on the stale undo fence"},
		},
		"retry":     map[string]any{"outage": "fixture provider killed; the turn sealed Failed", "recovery": "fixture restarted; retry created a new attempt that streamed the recovered answer and committed the assistant message"},
		"reconnect": map[string]any{"detach": "closing the observer mid-stream never cancelled the turn", "return": "a returning observer replayed the committed answer exactly once"},
	}, map[string]string{
		"simultaneous send / client-command replay / head conflict": "actual: two parallel sends fenced on the same head serialized to exactly one 201 and one 409; the same command id + same digest replayed the original message without appending; the same id with a different body returned 409 command_id_reused; a stale expectedHeadMessageId returned 409 head_conflict with the current head in the envelope",
		"Undo-vs-result / cancel races":                             "actual: undo committed first -> the running attempt fenced to Cancelling, the stream closed with the cancelled terminal frame + [DONE], the attempt converged to Cancelled and no assistant message was committed (late result audit-only); the result committed first -> the undo fence returned 409 head_conflict; cancel-first fenced Cancelling->Cancelled; cancel-after-success answered the completed Succeeded/Cancelled object with 200",
		"withdrawn UI rules (protocol/store level)":                 "actual: withdrawn messages keep status=withdrawn with no assistant on the branch, the head falls back to the remaining active turn or NULL, an explicit null expectedHeadMessageId resumes the branch, and no withdrawn message re-enters any newer attempt input snapshot (SQLite zero-count proof); the browser-side withdrawn presentation is exercised by the @ticket-15 Chromium run writing into this evidence directory",
		"reconnect and back-button state":                           "actual: detaching the stream observer mid-run never cancelled the attempt (the turn still committed head=assistant, messages=2); a returning observer received the committed assistant content exactly once terminated by [DONE]",
	})
	os.RemoveAll(workRoot)
}

// headOf reads the current head locator of one investigation.
func headOf(t *testing.T, client *http.Client, base, origin, investigationID string) string {
	t.Helper()
	body := httpGet(t, client, base+"/api/v1/investigations/"+investigationID, origin)
	var detail struct {
		HeadMessageID string `json:"headMessageId"`
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil || detail.HeadMessageID == "" {
		t.Fatalf("head read (%s): %v %s", investigationID, err, body)
	}
	return detail.HeadMessageID
}

// staleHeadID returns a head locator that is guaranteed stale (the first
// user message of the branch, never the current head after a reply).
func staleHeadID(t *testing.T, investigationID string, client *http.Client, base, origin string) string {
	t.Helper()
	body := httpGet(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin)
	var listObj struct {
		Items []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &listObj); err != nil {
		return "1"
	}
	for _, item := range listObj.Items {
		if item.Role == "user" {
			return item.ID
		}
	}
	return "1"
}

// detachStream opens one ui-message-stream, reads the first frames and
// closes the connection mid-flight (transport detach; HTTP-STREAM-006).
func detachStream(t *testing.T, client *http.Client, target, origin, body string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("detach stream %s: %v", target, err)
	}
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
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

func containsFrame(frames []string, fragment string) bool {
	return containsFrameText(frames, fragment)
}

func containsFrameText(frames []string, fragment string) bool {
	for _, frame := range frames {
		if strings.Contains(frame, fragment) {
			return true
		}
	}
	return false
}
