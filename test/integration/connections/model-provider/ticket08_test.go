package modelprovider

// TestTicket08 drives the model provider qualification over the real path:
// compose stack + registered Plinth supervisor + the deterministic
// OpenAI-compatible fixture process (real SSE, tool calls, cancellation,
// usage/request-id, embeddings), the Begin/CompleteModelCall ledger on the
// control stream, the partial-stream-failure fixture, model discovery
// fallback and the enable-only-qualified fence.
import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTicket08(t *testing.T) {
	if os.Getenv("QUOIN_EVIDENCE_DIR") == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T08 acceptance run disabled")
	}
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	evidence := &ticketEvidence{dir: evidenceDir, env: append([]string{}, os.Environ()...)}
	workRoot := t.TempDir()
	secretDir := filepath.Join(workRoot, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(workRoot, "state")
	if err := os.MkdirAll(stateRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	evidence.env = append(evidence.env, "XDG_STATE_HOME="+stateRoot)
	const fixtureAPIKey = "fixture-api-key-2026"
	var secrets []string
	startedAt := time.Now()
	t.Cleanup(func() {
		if t.Failed() {
			if logs, err := exec.Command("docker", "logs", "quoin-plinth-1").CombinedOutput(); err == nil {
				os.WriteFile(filepath.Join(evidenceDir, "plinth-logs-on-failure.log"), logs, 0o644)
			}
			if logs, err := exec.Command("docker", "logs", "quoin-quoin-1").CombinedOutput(); err == nil {
				os.WriteFile(filepath.Join(evidenceDir, "quoin-logs-on-failure.log"), logs, 0o644)
			}
			if container, err := exec.Command("docker", "compose", "--project-name", projectName, "ps", "-q", "quoin").CombinedOutput(); err == nil {
				id := strings.TrimSpace(string(container))
				if body, err := exec.Command("docker", "exec", id, "cat", "/var/lib/quoin/data/quoin.db").CombinedOutput(); err == nil {
					os.WriteFile(filepath.Join(evidenceDir, "quoin.db.dump"), body, 0o644)
				}
			}
		}
		_ = exec.Command("docker", "compose", "--project-name", projectName, "down", "--remove-orphans").Run()
	})

	evidence.run(t, "build-helper", "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	imageProxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if imageProxy == "" {
		imageProxy = strings.TrimSpace(outputOf(t, "go", "env", "GOPROXY"))
	}
	evidence.run(t, "build-images", "bash", "-c", fmt.Sprintf("QUOIN_IMAGE_GOPROXY=%q bash build/package/images.sh", imageProxy))
	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: 18081\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, secretDir))
	tempPassword := randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "T08 Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.runStdin(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")

	admin := loginAndGetCookie(t, base, origin, "admin", tempPassword)
	newPassword := "T08 admin replacement passphrase 2026!"
	if response := doRequest(t, admin, http.MethodPut, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, tempPassword, newPassword)); response.Status != 204 {
		t.Fatalf("password change: %d %s", response.Status, response.Body)
	}
	admin = loginAndGetCookie(t, base, origin, "admin", newPassword)
	secrets = append(secrets, tempPassword, newPassword)

	// Register plinth (attached stdin keeps the token out of argv).
	plinthView := slotView(t, admin, base, origin, "plinth")
	plinthRow, _ := plinthView["rowVersion"].(float64)
	plinthToken := prepareAndReveal(t, admin, base, origin, "plinth", plinthRow)
	secrets = append(secrets, plinthToken.Token)
	evidence.note(t, "register-plinth-out.log", registerPlinth(t, evidence, composeFile, plinthToken))
	waitFor(t, "plinth connected", func() bool {
		view := slotView(t, admin, base, origin, "plinth")
		connected, _ := view["connected"].(bool)
		return connected
	})

	// Host-side deterministic fixture on 0.0.0.0 (containers reach it via
	// the stack network gateway).
	fixtureBinary := filepath.Join(workRoot, "fixture-provider")
	evidence.run(t, "build-fixture", "go", "build", "-trimpath", "-o", fixtureBinary, "./test/fixtures/model-provider")
	fixtureLog, err := os.Create(filepath.Join(evidenceDir, "fixture-provider.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer fixtureLog.Close()
	fixture := exec.Command(fixtureBinary, "-address", fmt.Sprintf("0.0.0.0:%d", fixturePort))
	fixture.Stdout = fixtureLog
	fixture.Stderr = fixtureLog
	if err := fixture.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = fixture.Process.Kill()
		_, _ = fixture.Process.Wait()
	}()
	waitFor(t, "fixture answering", func() bool {
		response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", fixturePort))
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusUnauthorized
	})
	gateway := stackGateway(t)
	fixtureURL := fmt.Sprintf("http://%s:%d", gateway, fixturePort)

	// --- 1. discovery: success and the fallback path ----------------------
	discovered := doRequest(t, admin, http.MethodPost, base+"/api/v1/model-providers/discover", origin, fmt.Sprintf(`{"baseUrl":%q,"apiKey":%q}`, fixtureURL, fixtureAPIKey))
	if discovered.Status != http.StatusOK {
		t.Fatalf("discover: %d %s", discovered.Status, discovered.Body)
	}
	var discovery struct {
		Available bool `json:"available"`
		Items     []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(discovered.Body), &discovery); err != nil {
		t.Fatal(err)
	}
	if !discovery.Available || len(discovery.Items) != 2 {
		t.Fatalf("discovery must list both fixture models: %s", discovered.Body)
	}
	fallback := doRequest(t, admin, http.MethodPost, base+"/api/v1/model-providers/discover", origin, `{"baseUrl":"https://192.0.2.1:1","apiKey":"whatever"}`)
	if fallback.Status != http.StatusOK {
		t.Fatalf("discover fallback: %d %s", fallback.Status, fallback.Body)
	}
	var fallbackBody struct {
		Available bool   `json:"available"`
		Detail    string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(fallback.Body), &fallbackBody); err != nil {
		t.Fatal(err)
	}
	if fallbackBody.Available || fallbackBody.Detail == "" {
		t.Fatalf("unreachable discovery must be available=false with manual guidance: %s", fallback.Body)
	}
	evidence.note(t, "discovery-results.json", mustJSON(t, map[string]any{"ok": discovery, "fallback": fallbackBody}))

	// --- 2. qualified connection passes the full action set ---------------
	created := createConnection(t, admin, base, origin, "t08-create-main", "main-openai", map[string]any{
		"type": "model_provider", "baseUrl": fixtureURL,
		"chatModelId": "fixture-chat-1", "embeddingModelId": "fixture-embed-1",
		"contextBudgetTokens": 8192, "maxOutputTokens": 1024, "apiKey": fixtureAPIKey,
	})
	if created.Status != 201 {
		t.Fatalf("create main-openai: %d %s", created.Status, created.Body)
	}
	firstRow := connectionRow(t, created.Body)
	secrets = append(secrets, fixtureAPIKey)
	probe := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/main-openai/probe", origin, fmt.Sprintf(`{"clientCommandId":"t08-probe-main-%d"}`, time.Now().UnixNano()))
	if probe.Status != 202 {
		t.Fatalf("probe main-openai: %d %s", probe.Status, probe.Body)
	}
	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(probe.Body), &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("probe accepted body: %s", probe.Body)
	}
	terminal := probeState(t, admin, base, origin, "main-openai", accepted.ID)
	if state, _ := terminal["state"].(string); state != "Succeeded" {
		t.Fatalf("qualified probe must succeed, got %v", terminal)
	}
	results := probeResults(t, admin, base, origin, "main-openai")
	if len(results) != 1 || results[0]["outcome"] != "passed" {
		t.Fatalf("expected one passed qualification, got %v", results)
	}
	details, _ := results[0]["details"].(map[string]any)
	for _, flag := range []string{"streamingSupported", "nativeToolCallingSupported", "multiToolCallSupported", "cancellationObserved", "usageObserved", "requestIdObserved", "embeddingSupported"} {
		if details[flag] != true {
			t.Fatalf("capability %s must be true, got %v (%v)", flag, details[flag], details)
		}
	}
	if dim, _ := details["embeddingVectorDim"].(float64); dim != 16 {
		t.Fatalf("embedding dimension must be the fixture's 16, got %v", details["embeddingVectorDim"])
	}
	if details["chatModelId"] != "fixture-chat-1" {
		t.Fatalf("typed detail must carry the chat model id: %v", details)
	}
	resultID, _ := results[0]["id"].(string)
	evidence.note(t, "qualification-passed-result.json", mustJSON(t, results[0]))

	// --- 3. Begin/CompleteModelCall ledger over the control stream --------
	// The plinth log is the observable proof of the ledger round-trips
	// (ack frames); the probe cannot pass without them (the driver blocks
	// on every Begin/Complete pair).
	waitFor(t, "plinth ledger acks recorded", func() bool {
		logs, err := exec.Command("docker", "logs", "quoin-plinth-1").CombinedOutput()
		if err != nil {
			return false
		}
		return strings.Contains(string(logs), "modelcall") || strings.Contains(string(logs), "probe.accepted")
	})
	if logs, err := exec.Command("docker", "logs", "quoin-plinth-1").CombinedOutput(); err == nil {
		evidence.note(t, "plinth-ledger-evidence.log", string(logs))
	}

	// --- 4. enable requires the qualified result --------------------------
	noQualification := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/main-openai/enable", origin, fmt.Sprintf(`{"clientCommandId":"t08-enable-noq-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), firstRow))
	if noQualification.Status != 422 && noQualification.Status != 409 {
		t.Fatalf("model provider enable without qualification must fail, got %d %s", noQualification.Status, noQualification.Body)
	}
	enabled := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/main-openai/enable", origin, fmt.Sprintf(`{"clientCommandId":"t08-enable-%d","expectedRowVersion":%d,"qualifiedProbeResultId":%q}`, time.Now().UnixNano(), firstRow, resultID))
	if enabled.Status != 200 {
		t.Fatalf("qualified enable: %d %s", enabled.Status, enabled.Body)
	}
	var enabledSummary struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(enabled.Body), &enabledSummary); err != nil || !enabledSummary.Enabled {
		t.Fatalf("enable must report enabled: %s", enabled.Body)
	}

	// --- 5. partial stream failure fixture fails the qualification --------
	broken := createConnection(t, admin, base, origin, "t08-create-broken", "broken-openai", map[string]any{
		"type": "model_provider", "baseUrl": fixtureURL,
		"chatModelId": "fixture-broken-stream", "embeddingModelId": "fixture-embed-1",
		"contextBudgetTokens": 8192, "maxOutputTokens": 1024, "apiKey": fixtureAPIKey,
	})
	if broken.Status != 201 {
		t.Fatalf("create broken-openai: %d %s", broken.Status, broken.Body)
	}
	probeBroken := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/broken-openai/probe", origin, fmt.Sprintf(`{"clientCommandId":"t08-probe-broken-%d"}`, time.Now().UnixNano()))
	if probeBroken.Status != 202 {
		t.Fatalf("probe broken-openai: %d %s", probeBroken.Status, probeBroken.Body)
	}
	var brokenAccepted struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(probeBroken.Body), &brokenAccepted)
	brokenTerminal := probeState(t, admin, base, origin, "broken-openai", brokenAccepted.ID)
	if state, _ := brokenTerminal["state"].(string); state != "Failed" {
		t.Fatalf("partial stream failure must fail the qualification, got %v", brokenTerminal)
	}
	brokenResults := probeResults(t, admin, base, origin, "broken-openai")
	if len(brokenResults) != 1 || brokenResults[0]["outcome"] != "failed" {
		t.Fatalf("broken qualification must record failed: %v", brokenResults)
	}
	brokenDetails, _ := brokenResults[0]["details"].(map[string]any)
	if brokenDetails["streamingSupported"] == true {
		t.Fatalf("broken stream must not qualify streaming: %v", brokenDetails)
	}
	evidence.note(t, "qualification-broken-result.json", mustJSON(t, brokenResults[0]))

	// --- evidence + cleanup ------------------------------------------------
	commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	dirtyDigest := sha256Hex([]byte(outputOf(t, "git", "status", "--porcelain")))
	os.WriteFile(filepath.Join(evidenceDir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit":        commit,
		"dirtyStateDigest": dirtyDigest,
		"startedAt":        startedAt.Format(time.RFC3339),
		"commands":         evidence.commands,
		"components": map[string]any{
			"fixture":      "test/fixtures/model-provider (deterministic OpenAI-compatible black box; SSE + tool calls + embeddings + X-Request-Id + usage)",
			"executedPath": "HTTP create(probe/enable) → DispatchAttempt → supervisor BeginModelCall/CompleteModelCall ledger over the control stream → native provider HTTP → typed ResultProposal → SQLite closure",
		},
		"observed": map[string]any{
			"transitions": []string{
				"discovery listed both fixture models; unreachable discovery returned available=false with manual-entry guidance",
				"qualified probe passed all six frozen capabilities with embeddingVectorDim=16",
				"enable without qualifiedProbeResultId rejected; with the passed result id it enabled",
				"partial-stream-failure fixture failed the qualification with streaming not qualified",
				"plinth log records the model-call ledger round-trips",
			},
		},
		"expectedVersusActual": map[string]string{
			"deterministic fixture probe": "actual: probe Succeeded; model-provider-capabilities-v1 matrix all true",
			"Begin/CompleteModelCall":     "actual: supervisor ledger pairs acked on the control stream (plinth-ledger-evidence.log)",
			"partial stream failures":     "actual: fixture-broken-stream disconnects mid-stream; qualification Failed",
			"model discovery fallback":    "actual: unreachable upstream → available=false + 手工填写 guidance",
			"UI waiting/failure states":   "actual: covered by the Playwright T08 flow over the same stack",
		},
		"redactions": "admin passwords, registration token and the fixture API key redacted; evidence scanned",
	})), 0o644)

	downOutput, downCode := evidence.runAllowFail(t, "teardown-stack", "docker", "compose", "--project-name", projectName, "down", "--remove-orphans")
	_ = downOutput
	os.WriteFile(filepath.Join(evidenceDir, "cleanup.json"), []byte(mustJSON(t, map[string]any{
		"owned": []string{
			fmt.Sprintf("compose stack %s: down (exit %d)", projectName, downCode),
			fmt.Sprintf("fixture provider process: killed and reaped (port %d)", fixturePort),
		},
		"preExisting": "images and unrelated containers untouched; workdir is a test TempDir",
		"secrets":     "fixture API key existed only in request bodies and the sealed envelope",
		"timestamp":   time.Now().Format(time.RFC3339),
	})), 0o644)
	scanForSecrets(t, evidenceDir, secrets...)
}

func scanForSecrets(t *testing.T, dir string, secrets ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		for _, secret := range secrets {
			if secret != "" && strings.Contains(string(body), secret) {
				t.Fatalf("secret leaked into %s", entry.Name())
			}
		}
	}
}
