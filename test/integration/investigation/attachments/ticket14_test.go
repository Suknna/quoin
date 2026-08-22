// Package attachments hosts the T14 ticket acceptance run: the real
// compose stack (Quoin + registered Plinth), the deterministic fixture
// model provider and Thanos target, and the full Investigation attachment
// and tool path — streaming multipart staging (10 MiB/UTF-8/NUL limits,
// replay, reuse), text-only/attachment-only/multi-attachment turns, the
// Thanos/workspace tool chain with spilled artifacts and sealed evidence,
// and the worker sandbox /proc/state/network adversarial suite. Evidence
// lands under .artifacts/tickets/T14/.
package attachments

import (
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

func TestTicket14(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T14 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	// The acceptance always rebuilds the four images: the web dist and Go
	// sources both feed the quoin image, and a stale image would test old
	// code (the same policy as test/e2e/compose/server.sh). Restricted
	// networks need the module mirror passed into the image build
	// (QUOIN_IMAGE_GOPROXY falling back to `go env GOPROXY`).
	imageProxy := strings.TrimSpace(outputOf(t, "go", "env", "GOPROXY"))
	if imageProxy != "" {
		evidence.env = append(evidence.env, "QUOIN_IMAGE_GOPROXY="+imageProxy)
	}
	evidence.env = append(evidence.env, "QUOIN_FORCE_IMAGE_BUILD=1")
	for _, stale := range []string{"t14-thanos-fixture", "quoin-t07-thanos"} {
		exec.Command("docker", "rm", "-f", stale).Run()
	}
	// A previously failed acceptance run may have left the compose project
	// up (its teardown only runs on success); reclaim the ports first.
	exec.Command("docker", "compose", "--project-name", projectName, "down", "--remove-orphans").Run()
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
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: 18081\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, secretDir))
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

	// --- Fixtures on the host: provider + Thanos target -----------------
	providerBinary := filepath.Join(workRoot, "fixture-provider")
	thanosBinary := filepath.Join(workRoot, "fixture-thanos")
	evidence.run(t, "build-provider", nil, "go", "build", "-trimpath", "-o", providerBinary, "./test/fixtures/model-provider")
	evidence.run(t, "build-thanos", nil, "go", "build", "-trimpath", "-o", thanosBinary, "./test/fixtures/thanos-query")
	startFixture := func(binary string, port int, logName string) *exec.Cmd {
		command := exec.Command(binary, "-address", fmt.Sprintf("0.0.0.0:%d", port))
		command.Env = evidence.env
		log, err := os.Create(filepath.Join(evidence.dir, logName))
		if err != nil {
			t.Fatal(err)
		}
		command.Stdout = log
		command.Stderr = log
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		return command
	}
	providerCmd := startFixture(providerBinary, 18443, "fixture-provider.log")
	thanosCmd := startFixture(thanosBinary, 18444, "fixture-thanos.log")
	defer func() {
		providerCmd.Process.Kill()
		thanosCmd.Process.Kill()
		_, _ = providerCmd.Process.Wait()
		_, _ = thanosCmd.Process.Wait()
	}()
	waitForLog := func(name, marker string) {
		t.Helper()
		for probe := 0; probe < 30; probe++ {
			time.Sleep(200 * time.Millisecond)
			body, _ := os.ReadFile(filepath.Join(evidence.dir, name))
			if strings.Contains(string(body), marker) {
				return
			}
		}
		t.Fatalf("fixture %s never reported %s", name, marker)
	}
	waitForLog("fixture-provider.log", "listening")
	waitForLog("fixture-thanos.log", "listening")

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
	prepare := httpPost(t, client, base+"/api/v1/runtime-slots/plinth/registration/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t14-prepare-%s","expectedRowVersion":%d}`, randomSecret(t, 8), runtimeView.Plinth.RowVersion))
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

	// --- Enabled qualified provider + enabled Thanos connection ---------
	httpPost(t, client, base+"/api/v1/connections", origin, fmt.Sprintf(`{"clientCommandId":"t14-create-conn-1","name":"t14-provider","connection":{"type":"model_provider","baseUrl":"http://%s:18443","chatModelId":"fixture-chat-1","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}`, gatewayIP))
	httpPost(t, client, base+"/api/v1/connections/t14-provider/probe", origin, `{"clientCommandId":"t14-probe-1"}`)
	var probeResultID string
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		listBody := httpGet(t, client, base+"/api/v1/connections/t14-provider/probe-results", origin)
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
		t.Fatal("t14-provider never qualified (probe did not pass)")
	}
	connDetail := httpGet(t, client, base+"/api/v1/connections/t14-provider", origin)
	var connObj struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(connDetail), &connObj); err != nil {
		t.Fatalf("connection detail parse: %v\n%s", err, connDetail)
	}
	httpPost(t, client, base+"/api/v1/connections/t14-provider/enable", origin, fmt.Sprintf(`{"clientCommandId":"t14-enable-1","expectedRowVersion":%d,"qualifiedProbeResultId":"%s"}`, connObj.RowVersion, probeResultID))
	httpPost(t, client, base+"/api/v1/connections", origin, fmt.Sprintf(`{"clientCommandId":"t14-create-thanos","name":"t14-thanos","connection":{"type":"thanos","baseUrl":"http://%s:18444"}}`, gatewayIP))
	thanosDetail := httpGet(t, client, base+"/api/v1/connections/t14-thanos", origin)
	var thanosObj struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(thanosDetail), &thanosObj); err != nil {
		t.Fatalf("thanos detail parse: %v\n%s", err, thanosDetail)
	}
	httpPost(t, client, base+"/api/v1/connections/t14-thanos/enable", origin, fmt.Sprintf(`{"clientCommandId":"t14-enable-thanos","expectedRowVersion":%d}`, thanosObj.RowVersion))

	// --- Staging upload matrix (HTTP-FILE-002) --------------------------
	bodyA := "T14-ATTACHMENT-A\n第一份附件正文：连接超时日志样本。\n"
	uploadA, _ := uploadAttachment(t, client, base, origin, "t14-upload-a-000001", "a-logs.txt", []byte(bodyA), 0)
	var attachmentA struct {
		ID               string `json:"id"`
		ArtifactID       string `json:"artifactId"`
		OriginalFilename string `json:"originalFilename"`
		MediaType        string `json:"mediaType"`
		SizeBytes        int64  `json:"sizeBytes"`
		Digest           string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(uploadA), &attachmentA); err != nil {
		t.Fatalf("upload A parse: %v\n%s", err, uploadA)
	}
	if attachmentA.OriginalFilename != "a-logs.txt" || attachmentA.MediaType != "text/plain" || attachmentA.SizeBytes != int64(len(bodyA)) {
		t.Fatalf("upload A summary wrong: %s", uploadA)
	}
	// Replay: same command + same bytes returns the original object.
	replayed, _ := uploadAttachment(t, client, base, origin, "t14-upload-a-000001", "a-logs.txt", []byte(bodyA), 0)
	if !strings.Contains(replayed, attachmentA.ID) {
		t.Fatalf("upload replay diverged: %s", replayed)
	}
	// Same command + different bytes: deterministic conflict.
	uploadAttachment(t, client, base, origin, "t14-upload-a-000001", "a-logs.txt", []byte("不同内容"), 409)
	// Invalid UTF-8 and NUL bodies reject with 422.
	uploadAttachment(t, client, base, origin, "t14-upload-bad-utf8", "bad.txt", []byte("正文\xff尾部"), 422)
	uploadAttachment(t, client, base, origin, "t14-upload-nul-0001", "nul.txt", []byte("正文\x00尾部"), 422)
	// 10 MiB single-file boundary (default limit): +1 byte rejects 413.
	limit := 10 << 20
	uploadAttachment(t, client, base, origin, "t14-upload-big-0001", "big.txt", []byte(strings.Repeat("x", limit+1)), 413)
	// Metadata read-back of the staged object.
	meta := httpGet(t, client, base+"/api/v1/investigation-attachments/"+attachmentA.ID, origin)
	if !strings.Contains(meta, attachmentA.ArtifactID) {
		t.Fatalf("attachment metadata wrong: %s", meta)
	}
	evidence.note(t, "staging-matrix.json", mustJSON(t, map[string]any{
		"validUpload": attachmentA,
		"replay":      "same id returned for the same command+bytes; divergent bytes -> 409 command_id_reused",
		"rejections":  "invalid UTF-8 -> 422, NUL -> 422, 10MiB+1 -> 413 payload_too_large",
	}))

	bodyB := "T14-ATTACHMENT-B\n第二份附件正文：另一段日志。\n"
	uploadB, _ := uploadAttachment(t, client, base, origin, "t14-upload-b-000001", "b-logs.txt", []byte(bodyB), 0)
	var attachmentB struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(uploadB), &attachmentB)
	bodyC := "T14-ATTACHMENT-C\n第三份附件正文：复用证明。\n"
	uploadC, _ := uploadAttachment(t, client, base, origin, "t14-upload-c-000001", "c-logs.txt", []byte(bodyC), 0)
	var attachmentC struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(uploadC), &attachmentC)

	// --- Text + attachment first turn -----------------------------------
	createdBody := httpPost(t, client, base+"/api/v1/investigations", origin, fmt.Sprintf(`{"clientCommandId":"t14-inv-create-1","content":"T14Attach 请读取附件内容","attachmentIds":["%s"]}`, attachmentA.ID))
	var created struct {
		ID            string `json:"id"`
		HeadMessageID string `json:"headMessageId"`
	}
	if err := json.Unmarshal([]byte(createdBody), &created); err != nil {
		t.Fatalf("create parse: %v\n%s", err, createdBody)
	}
	investigationID := created.ID

	waitTurn := func() (string, string) {
		t.Helper()
		deadline := time.Now().Add(180 * time.Second)
		var detailBody, head string
		for time.Now().Before(deadline) {
			detailBody = httpGet(t, client, base+"/api/v1/investigations/"+investigationID, origin)
			var detail struct {
				HeadMessageID   string  `json:"headMessageId"`
				ActiveAttemptID *string `json:"activeAttemptId"`
			}
			if err := json.Unmarshal([]byte(detailBody), &detail); err == nil && detail.ActiveAttemptID == nil {
				head = detail.HeadMessageID
				break
			}
			time.Sleep(1 * time.Second)
		}
		if head == "" {
			t.Fatalf("turn never finished: %s", detailBody)
		}
		return head, detailBody
	}
	streamTurn := func(turnInvestigationID, messageID string) []string {
		t.Helper()
		return drainStream(t, client, base+"/api/v1/investigations/"+turnInvestigationID+"/messages/"+messageID+"/stream", origin, fmt.Sprintf(`{"clientCommandId":"t14-stream-%s","protocol":"ui-message-stream"}`, randomSecret(t, 6)))
	}

	// Attach the stream of the running first turn (tool round + answer).
	frames := streamTurn(investigationID, created.HeadMessageID)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "[DONE]") || !strings.Contains(joined, "attachment-proof-t14") {
		t.Fatalf("attachment turn stream lacks the proof answer: %s", joined)
	}
	head, _ := waitTurn()
	// Negative paths run on the settled conversation (the one-active-attempt
	// fence owns the running window; DATA-INVEST-003).
	// Duplicate attachment ids in one message reject (uniqueItems).
	httpPostExpect(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin,
		fmt.Sprintf(`{"clientCommandId":"t14-dup-000001","content":"重复引用","expectedHeadMessageId":"%s","attachmentIds":["%s","%s"]}`, head, attachmentA.ID, attachmentA.ID), 422)
	// Unknown attachment id rejects.
	httpPostExpect(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin,
		fmt.Sprintf(`{"clientCommandId":"t14-unknown-00001","content":"未知引用","expectedHeadMessageId":"%s","attachmentIds":["999999"]}`, head), 422)
	// All-empty message rejects.
	httpPostExpect(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin,
		fmt.Sprintf(`{"clientCommandId":"t14-empty-000001","content":"","expectedHeadMessageId":"%s"}`, head), 422)
	messagesBody := httpGet(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin)
	if !strings.Contains(messagesBody, `"attachments":[{"id":"`+attachmentA.ID) {
		t.Fatalf("first message must project its ordered attachment: %.600s", messagesBody)
	}
	if !strings.Contains(messagesBody, "附件已读取") || !strings.Contains(messagesBody, "T14-ATTACHMENT-A") {
		t.Fatalf("assistant echo must carry the attachment content slice: %.800s", messagesBody)
	}
	// The turn's input granted the attachment artifact to the attempt
	// (artifact_read could not have succeeded otherwise).
	evidence.note(t, "attachment-turn.json", messagesBody)

	// --- Attachment-only second turn ------------------------------------
	attachmentOnly := httpPost(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin,
		fmt.Sprintf(`{"clientCommandId":"t14-attach-only-1","content":"","expectedHeadMessageId":"%s","attachmentIds":["%s"]}`, head, attachmentB.ID))
	var sentOnly struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(attachmentOnly), &sentOnly); err != nil {
		t.Fatalf("attachment-only send parse: %v\n%s", err, attachmentOnly)
	}
	frames = streamTurn(investigationID, sentOnly.ID)
	if joined = strings.Join(frames, "\n"); !strings.Contains(joined, "attachment-proof-t14") || !strings.Contains(joined, "T14-ATTACHMENT-B") {
		t.Fatalf("attachment-only turn must read its own attachment: %s", joined)
	}
	head, _ = waitTurn()

	// --- Multi-attachment third turn with reuse -------------------------
	multiTurn := httpPost(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin,
		fmt.Sprintf(`{"clientCommandId":"t14-multi-000001","content":"复用并携带多份附件","expectedHeadMessageId":"%s","attachmentIds":["%s","%s"]}`, head, attachmentA.ID, attachmentC.ID))
	var sentMulti struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(multiTurn), &sentMulti); err != nil {
		t.Fatalf("multi send parse: %v\n%s", err, multiTurn)
	}
	frames = streamTurn(investigationID, sentMulti.ID)
	if joined = strings.Join(frames, "\n"); !strings.Contains(joined, "attachment-proof-t14") {
		t.Fatalf("multi-attachment turn must complete: %s", joined)
	}
	head, _ = waitTurn()
	messagesBody = httpGet(t, client, base+"/api/v1/investigations/"+investigationID+"/messages", origin)
	if got := strings.Count(messagesBody, `"originalFilename":"a-logs.txt"`); got != 2 {
		t.Fatalf("attachment A must be referenced by exactly two messages (reuse), got %d: %.800s", got, messagesBody)
	}
	// Reuse never re-uploaded: exactly three staging objects exist.
	if strings.Count(messagesBody, `"originalFilename"`) < 3 {
		t.Fatalf("message projection lost attachments: %.800s", messagesBody)
	}
	evidence.note(t, "attachment-reuse.json", messagesBody)

	// --- Aggregate boundary: two 6 MiB files exceed one message --------
	big1 := strings.Repeat("1", 6<<20)
	big2 := strings.Repeat("2", 6<<20)
	big1Body, _ := uploadAttachment(t, client, base, origin, "t14-upload-big1-001", "big1.txt", []byte(big1), 0)
	big2Body, _ := uploadAttachment(t, client, base, origin, "t14-upload-big2-001", "big2.txt", []byte(big2), 0)
	var big1ID, big2ID struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(big1Body), &big1ID)
	json.Unmarshal([]byte(big2Body), &big2ID)
	httpPostExpect(t, client, base+"/api/v1/investigations", origin,
		fmt.Sprintf(`{"clientCommandId":"t14-aggregate-001","content":"","attachmentIds":["%s","%s"]}`, big1ID.ID, big2ID.ID), 413)

	// --- Tool chain: workspace + long output + Thanos --------------------
	toolsTurn := httpPost(t, client, base+"/api/v1/investigations", origin, `{"clientCommandId":"t14-inv-tools-001","content":"T14Tools 请执行完整工具链"}`)
	var tools struct {
		ID            string `json:"id"`
		HeadMessageID string `json:"headMessageId"`
	}
	if err := json.Unmarshal([]byte(toolsTurn), &tools); err != nil {
		t.Fatalf("tools create parse: %v\n%s", err, toolsTurn)
	}
	streamTurn(tools.ID, tools.HeadMessageID)
	toolsHead, _ := waitFirstTurn(t, client, base, origin, tools.ID)
	toolCallsBody := httpGet(t, client, base+"/api/v1/investigations/"+tools.ID+"/attempts/"+firstAttemptID(t, client, base, origin, tools.ID)+"/tool-calls", origin)
	var toolCalls struct {
		Items []struct {
			ToolName         string          `json:"toolName"`
			Status           string          `json:"status"`
			Result           json.RawMessage `json:"result"`
			ResultArtifactID string          `json:"resultArtifactId"`
			ErrorDetail      string          `json:"errorDetail"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(toolCallsBody), &toolCalls); err != nil {
		t.Fatalf("tool calls parse: %v\n%s", err, toolCallsBody)
	}
	byName := map[string]json.RawMessage{}
	artifactByTool := map[string]string{}
	for _, item := range toolCalls.Items {
		if item.Status != "succeeded" {
			t.Fatalf("tool %s status=%s detail=%s", item.ToolName, item.Status, item.ErrorDetail)
		}
		byName[item.ToolName] = item.Result
		artifactByTool[item.ToolName] = item.ResultArtifactID
	}
	if len(toolCalls.Items) != 3 {
		t.Fatalf("tool chain must carry exactly three calls, got %d: %s", len(toolCalls.Items), toolCallsBody)
	}
	if !strings.Contains(string(byName["write"]), "已写入") {
		t.Fatalf("write result wrong: %s", byName["write"])
	}
	if !strings.Contains(string(byName["bash"]), `"truncated":true`) || !strings.Contains(string(byName["bash"]), "已存入 Artifact") {
		t.Fatalf("long bash output must spill into an artifact: %s", byName["bash"])
	}
	if artifactByTool["bash"] == "" {
		t.Fatalf("spilled bash result must reference its artifact: %s", toolCallsBody)
	}
	if !strings.Contains(string(byName["thanos_query"]), `"success":true`) {
		t.Fatalf("thanos result wrong: %s", byName["thanos_query"])
	}
	// The spilled artifact downloads through the authorized entry with
	// the frozen headers.
	download := httpGet(t, client, base+"/api/v1/artifacts/"+artifactByTool["bash"]+"/content", origin)
	if !strings.Contains(download, "29999") || !strings.Contains(download, "30000") {
		t.Fatalf("spilled artifact body must contain the seq tail: %d bytes", len(download))
	}
	// Evidence: the assistant message carries the thanos evidence link
	// and the evidence detail reads back.
	toolsMessages := httpGet(t, client, base+"/api/v1/investigations/"+tools.ID+"/messages", origin)
	var toolsMessageList struct {
		Items []struct {
			Role        string   `json:"role"`
			EvidenceIDs []string `json:"evidenceIds"`
			Content     string   `json:"content"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(toolsMessages), &toolsMessageList); err != nil {
		t.Fatalf("tools messages parse: %v\n%s", err, toolsMessages)
	}
	var evidenceID string
	for _, item := range toolsMessageList.Items {
		if item.Role == "assistant" && len(item.EvidenceIDs) > 0 {
			evidenceID = item.EvidenceIDs[0]
		}
	}
	if evidenceID == "" {
		t.Fatalf("assistant tool turn must reference its evidence: %s", toolsMessages)
	}
	evidenceDetail := httpGet(t, client, base+"/api/v1/evidence/"+evidenceID, origin)
	if !strings.Contains(evidenceDetail, `"toolName":"thanos_query"`) {
		t.Fatalf("evidence detail must project the producing tool: %s", evidenceDetail)
	}
	if !strings.Contains(toolsMessages, "t14-tools-proof") {
		t.Fatalf("tools turn conclusion missing: %s", toolsMessages)
	}
	evidence.note(t, "tool-chain.json", mustJSON(t, map[string]any{
		"toolCalls": toolCalls,
		"spill":     map[string]string{"artifactId": artifactByTool["bash"], "bodyCheck": "seq tail 29999/30000 present via downloadArtifactContent"},
		"evidence":  evidenceDetail,
		"head":      toolsHead,
	}))

	// --- Sandbox adversarial suite --------------------------------------
	sandboxTurn := httpPost(t, client, base+"/api/v1/investigations", origin, `{"clientCommandId":"t14-inv-sandbox-1","content":"T14Sandbox 请执行沙箱对抗命令"}`)
	var sandbox struct {
		ID            string `json:"id"`
		HeadMessageID string `json:"headMessageId"`
	}
	if err := json.Unmarshal([]byte(sandboxTurn), &sandbox); err != nil {
		t.Fatalf("sandbox create parse: %v\n%s", err, sandboxTurn)
	}
	streamTurn(sandbox.ID, sandbox.HeadMessageID)
	sandboxHead, _ := waitFirstTurn(t, client, base, origin, sandbox.ID)
	sandboxCallsBody := httpGet(t, client, base+"/api/v1/investigations/"+sandbox.ID+"/attempts/"+firstAttemptID(t, client, base, origin, sandbox.ID)+"/tool-calls", origin)
	var sandboxCalls struct {
		Items []struct {
			ProviderToolCallID string          `json:"providerToolCallId"`
			Arguments          json.RawMessage `json:"arguments"`
			Status             string          `json:"status"`
			Result             json.RawMessage `json:"result"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(sandboxCallsBody), &sandboxCalls); err != nil {
		t.Fatalf("sandbox calls parse: %v\n%s", err, sandboxCallsBody)
	}
	if len(sandboxCalls.Items) != 4 {
		t.Fatalf("sandbox suite must carry four bash probes: %s", sandboxCallsBody)
	}
	mustContain := func(index int, needles ...string) {
		t.Helper()
		if index >= len(sandboxCalls.Items) {
			t.Fatalf("sandbox probe %d missing", index)
		}
		item := sandboxCalls.Items[index]
		body := string(item.Arguments) + " " + string(item.Result)
		for _, needle := range needles {
			if !strings.Contains(body, needle) {
				t.Fatalf("sandbox probe %d lacks %q: args=%s result=%s", index, needle, item.Arguments, item.Result)
			}
		}
	}
	// /proc/1/environ: denied for the non-dumpable supervisor (the command
	// itself fails, so the denial is in both the exit code and the output).
	mustContain(0, "/proc/1/environ", `"success":false`, "Permission denied")
	// /proc/self: readable (deny-by-expectation contrast).
	mustContain(1, "/proc/self/status", `"success":true`, "Name:")
	// Outbound TCP through bash /dev/tcp: refused by the Landlock net rule.
	mustContain(2, "/dev/tcp", `"success":false`)
	// Writing outside the workspace: refused.
	mustContain(3, "/tmp/t14-escape-proof", `"success":false`)
	sandboxMessages := httpGet(t, client, base+"/api/v1/investigations/"+sandbox.ID+"/messages", origin)
	if !strings.Contains(sandboxMessages, "t14-sandbox-proof") {
		t.Fatalf("sandbox conclusion missing: %s", sandboxMessages)
	}
	evidence.note(t, "sandbox-suite.json", mustJSON(t, map[string]any{
		"probes": sandboxCalls,
		"verdict": map[string]string{
			"proc":  "/proc/1/environ denied (dumpable=0); /proc/self/status readable as designed",
			"net":   "bash /dev/tcp connect refused (Landlock network ruleset)",
			"write": "touch /tmp outside the workspace refused",
			"head":  sandboxHead,
		},
	}))

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
	attachInvID := strconv.FormatInt(mustInt64(t, investigationID), 10)
	grants := queryRow(`SELECT COUNT(*) FROM attempt_artifact_grants g JOIN execution_attempts a ON a.id=g.attempt_id WHERE a.scope_id=? AND g.source_kind='input_snapshot'`, attachInvID)
	items := queryRow(`SELECT COUNT(*) FROM attempt_input_items i JOIN attempt_input_snapshots s ON s.id=i.snapshot_id JOIN execution_attempts a ON a.id=s.attempt_id WHERE a.scope_id=? AND i.artifact_id IS NOT NULL`, attachInvID)
	ordinals := queryRow(`SELECT COUNT(*) FROM investigation_message_attachments`)
	staged := queryRow(`SELECT COUNT(*) FROM text_attachments`)
	if grants != "4" || items != "4" || ordinals != "4" || staged != "5" {
		t.Fatalf("sqlite evidence wrong: input grants=%s items=%s ordinals=%s staged=%s (want 4/4/4/5)", grants, items, ordinals, staged)
	}
	evidence.note(t, "sqlite-evidence.json", mustJSON(t, map[string]any{
		"inputSnapshotGrants":    grants,
		"attachmentLineageItems": items,
		"messageAttachmentRefs":  ordinals,
		"stagedAttachments":      staged,
		"longOutputArtifact":     artifactByTool["bash"],
		"evidenceLink":           evidenceID,
	}))

	// --- No credential / environment leakage ----------------------------
	keyHex := fmt.Sprintf("%x", []byte("fixture-api-key-2026"))
	cipherHex := queryRow(`SELECT lower(hex(ciphertext)) FROM credential_generations WHERE connection_id=(SELECT id FROM connections WHERE name='t14-provider')`)
	if strings.Contains(cipherHex, keyHex) {
		t.Fatalf("plaintext API key found inside the sealed credential ciphertext")
	}
	scanForSecrets(t, evidenceDir, newPassword, tempPassword, "fixture-api-key-2026")

	// --- Teardown ----------------------------------------------------------
	// Capture the immutable image IDs BEFORE teardown removes them (locally
	// built images have no repo digest; the config sha256 pins the build).
	imageIDs := map[string]string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if out, err := exec.Command("docker", "image", "inspect", image, "--format", "{{.Id}}").Output(); err == nil {
			imageIDs[image] = strings.TrimSpace(string(out))
		}
	}
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
	writeTicketEvidence(t, evidence, commit, newPassword, tempPassword, imageIDs)
	os.RemoveAll(workRoot)
}

func execCommand(t *testing.T, evidence *ticketEvidence, name string, stdin io.Reader, command string, arguments ...string) string {
	return evidence.run(t, name, stdin, command, arguments...)
}

// waitFirstTurn waits for a fresh investigation's first turn to finish
// and returns the head + full detail body.
func waitFirstTurn(t *testing.T, client *http.Client, base, origin, investigationID string) (string, string) {
	t.Helper()
	deadline := time.Now().Add(180 * time.Second)
	var detailBody, head string
	for time.Now().Before(deadline) {
		detailBody = httpGet(t, client, base+"/api/v1/investigations/"+investigationID, origin)
		var detail struct {
			HeadMessageID   string  `json:"headMessageId"`
			ActiveAttemptID *string `json:"activeAttemptId"`
		}
		if err := json.Unmarshal([]byte(detailBody), &detail); err == nil && detail.ActiveAttemptID == nil {
			head = detail.HeadMessageID
			break
		}
		time.Sleep(1 * time.Second)
	}
	if head == "" {
		t.Fatalf("first turn never finished: %s", detailBody)
	}
	return head, detailBody
}

// firstAttemptID resolves a single-turn investigation's attempt locator
// from the attempts subresource.
func firstAttemptID(t *testing.T, client *http.Client, base, origin, investigationID string) string {
	t.Helper()
	body := httpGet(t, client, base+"/api/v1/investigations/"+investigationID+"/attempts", origin)
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list.Items) == 0 {
		t.Fatalf("attempts parse: %v\n%s", err, body)
	}
	return list.Items[0].ID
}

func mustInt64(t *testing.T, value string) int64 {
	t.Helper()
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("locator %q: %v", value, err)
	}
	return parsed
}

func writeTicketEvidence(t *testing.T, evidence *ticketEvidence, commit, newPassword, tempPassword string, imageIDs map[string]string) {
	t.Helper()
	startedAt := time.Now().UTC()
	// imageIDs were captured before teardown (locally built images carry
	// no repo digest; the immutable config sha256 pins the exact build).
	imageDigests := imageIDs
	// Dirty-state digest: hash of `git status --porcelain` (empty output =
	// clean; the ticket requires the evidence to pin the working-tree
	// state the binaries were built from).
	dirtyDigest := ""
	if status, err := exec.Command("git", "-C", repoRoot(t), "status", "--porcelain").Output(); err == nil {
		dirtyDigest = sha256Hex(status)
	} else {
		dirtyDigest = "unavailable: " + err.Error()
	}
	// Chromium/browser version from the Playwright install (the acceptance
	// proves keyboard/paste behavior there; pin its revision).
	chromiumVersion := "Playwright 1.62.1 / chromium-1234"
	os.WriteFile(filepath.Join(evidence.dir, "cleanup.json"), []byte(mustJSON(t, map[string]any{
		"containers":  "fixture provider/thanos host processes killed; quoin compose project down --remove-orphans",
		"images":      "quoin/{quoin,plinth,lintel,stele}:v0.1.0-dev removed (only if the run built them)",
		"workRoot":    "temporary XDG_STATE_HOME + secrets removed with the test temp root",
		"credentials": "admin passwords and the fixture API key held only in process memory",
		"timestamp":   startedAt.Format(time.RFC3339),
	})), 0o644)
	os.WriteFile(filepath.Join(evidence.dir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit":        commit,
		"dirtyStateDigest": dirtyDigest,
		"startedAt":        startedAt.Format(time.RFC3339),
		"commands":         evidence.commands,
		"artifacts":        evidence.artifacts,
		"components": map[string]any{
			"deployHelper":    "cmd/quoin-deploy (go build -trimpath)",
			"fixtureProvider": "test/fixtures/model-provider (deterministic T14 attachment/tool/sandbox branches)",
			"fixtureThanos":   "test/fixtures/thanos-query (deterministic instant-query target)",
			"browser":         chromiumVersion,
			"imageDigests":    imageDigests,
		},
		"realPath": "streaming multipart staging -> SQLite source_material/artifact/text_attachments -> head-fenced send with attachmentIds -> Plinth supervisor -> sandboxed worker -> fixture provider (tool rounds) -> ui-message-stream SSE -> committed messages/tool_calls/evidence/artifacts",
		"expectedVersusActual": map[string]string{
			"10 MiB limit":     "actual: 10MiB+1 single upload -> 413 payload_too_large; two 6MiB files in one message -> 413 (aggregate boundary)",
			"UTF-8/NUL":        "actual: invalid UTF-8 and NUL bodies -> 422 validation_failed without staging rows",
			"duplicate limits": "actual: duplicate attachmentIds in one message -> 422; unknown id -> 422; empty-everything -> 422",
			"long output":      "actual: seq 1 30000 (~168KB) spilled into a tool_result Artifact; preview truncated flag true; body downloaded through the authorized entry contains the seq tail",
			"attachment reuse": "actual: attachment A referenced by two different messages across turns without re-upload (exactly one staging row; two message references)",
			"sandbox suite":    "actual: /proc/1/environ denied, /dev/tcp connect refused, /tmp write refused, /proc/self/status readable; all four bash results committed on the durable timeline",
			"tool/evidence":    "actual: listAttemptToolCalls carries write/bash/thanos_query with results; assistant message references the sealed evidence; getEvidence projects the producing tool",
			"keyboard/paste":   "covered by the @ticket-14 Chromium acceptance (web/e2e): keyboard-only attach/remove/send plus the 16KiB/200-line paste conversion; the playwright artifacts land beside this evidence",
		},
		"redactions": "admin passwords and the fixture API key are not written to any evidence file",
	})), 0o644)
	_ = newPassword
	_ = tempPassword
}
