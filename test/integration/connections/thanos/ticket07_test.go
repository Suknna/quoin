package thanos

// TestTicket07 drives the thanos slice of T07 over the real path: compose
// install, attached-stdin plinth registration, HTTP connection create with a
// one-time secret, probe dispatch over the live control stream into the
// Plinth supervisor, a real Thanos Query container, deterministic negative
// fixtures, cancellation commit order, secret-input command replay and
// enable fences. Evidence lands under .artifacts/tickets/T07/.

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

func TestTicket07(t *testing.T) {
	if os.Getenv("QUOIN_EVIDENCE_DIR") == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T07 acceptance run disabled")
	}
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	evidence := &ticketEvidence{dir: evidenceDir, env: append([]string{}, os.Environ()...)}
	// Fail-path cleanup: owned fixtures and the stack must go even when an
	// assertion aborts mid-run (the happy-path cleanup section still
	// records the authoritative dispositions).
	t.Cleanup(func() {
		for _, name := range []string{"quoin-t07-thanos", "quoin-t07-wrongshape", "quoin-t07-hang"} {
			_ = exec.Command("docker", "rm", "-f", "-v", name).Run()
		}
		_ = exec.Command("docker", "compose", "--project-name", projectName, "down", "--remove-orphans").Run()
	})
	workRoot := t.TempDir()
	secretDir := filepath.Join(workRoot, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(workRoot, "state")
	if err := os.MkdirAll(stateRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// quoin-deploy resolves its state directory from XDG_STATE_HOME.
	evidence.env = append(evidence.env, "XDG_STATE_HOME="+stateRoot)
	var secrets []string
	startedAt := time.Now()

	// --- stack bring-up (canonical helper, images, scripted install) ------
	evidence.run(t, "build-helper", "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	imageProxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if imageProxy == "" {
		imageProxy = strings.TrimSpace(outputOf(t, "go", "env", "GOPROXY"))
	}
	evidence.run(t, "build-images", "bash", "-c", fmt.Sprintf("QUOIN_IMAGE_GOPROXY=%q QUOIN_FORCE_IMAGE_BUILD=1 bash build/package/images.sh", imageProxy))
	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: 18081\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, secretDir))
	tempPassword := randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "T07 Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.runStdin(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")

	admin := loginAndGetCookie(t, base, origin, "admin", tempPassword)
	newPassword := "T07 admin replacement passphrase 2026!"
	if response := doRequest(t, admin, http.MethodPut, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, tempPassword, newPassword)); response.Status != 204 {
		t.Fatalf("password change: %d %s", response.Status, response.Body)
	}
	admin = loginAndGetCookie(t, base, origin, "admin", newPassword)
	secrets = append(secrets, tempPassword, newPassword)

	// --- register plinth so the supervisor is connected -------------------
	plinthView := slotView(t, admin, base, origin, "plinth")
	plinthRowValue, _ := plinthView["rowVersion"].(float64)
	plinthToken := prepareAndReveal(t, admin, base, origin, "plinth", plinthRowValue)
	secrets = append(secrets, plinthToken.Token)
	registerOutput := registerPlinth(t, evidence, composeFile, plinthToken)
	evidence.note(t, "register-plinth-1.log", registerOutput)
	waitFor(t, "plinth connected", func() bool {
		view := slotView(t, admin, base, origin, "plinth")
		connected, _ := view["connected"].(bool)
		return connected
	})

	// --- fixtures on the stack network ------------------------------------
	// Fixture images pull explicitly first: `docker run -d` pulls
	// synchronously and a slow registry would otherwise eat the whole
	// test budget before any fixture starts.
	evidence.run(t, "pull-thanos", "docker", "pull", "thanosio/thanos:v0.36.0")
	evidence.run(t, "pull-nginx", "docker", "pull", "nginx:1.27-alpine")
	evidence.run(t, "pull-socat", "docker", "pull", "alpine/socat:latest")
	network := quoinNetwork(t)
	startFixture(t, evidence, network, "quoin-t07-thanos", "thanosio/thanos:v0.36.0", "query", "--http-address=0.0.0.0:9090", "--log.level=warn")
	fixtureConf := filepath.Join(workRoot, "wrongshape.conf")
	writeFile(t, fixtureConf, `server {
  listen 80;
  location = /api/v1/query {
    default_type application/json;
    return 200 '{"status":"success","data":{"resultType":"matrix","result":[]}}';
  }
}
`)
	startFixture(t, evidence, network, "quoin-t07-wrongshape", "-v", fixtureConf+":/etc/nginx/conf.d/default.conf:ro", "nginx:1.27-alpine")
	// The hang fixture accepts TCP and never answers (socat + sleep): the
	// probe deterministically stays in Running until its 15s action
	// timeout, giving the 409 conflict and the cancellation fence a stable
	// window.
	startFixture(t, evidence, network, "quoin-t07-hang", "alpine/socat:latest", "TCP-LISTEN:80,fork,reuseaddr", "EXEC:/bin/sleep 120")
	// Readiness from inside the stack network (fixtures publish no host
	// ports): busybox wget against each target.
	waitFor(t, "fixtures reachable in-network", func() bool {
		for _, target := range []string{"quoin-t07-thanos:9090", "quoin-t07-wrongshape:80"} {
			probe := exec.Command("docker", "run", "--rm", "--network", network, "busybox", "wget", "-qO-", "http://"+target+"/api/v1/query?query=vector%281%29")
			if err := probe.Run(); err != nil {
				return false
			}
		}
		return true
	})

	// --- 1. real Thanos target passes the frozen action set ---------------
	created := createConnection(t, admin, base, origin, "t07-create-main", "main-thanos", map[string]any{
		"type": "thanos", "baseUrl": "http://quoin-t07-thanos:9090", "password": "t07-thanos-password",
	})
	secrets = append(secrets, "t07-thanos-password")
	if created.Status != 201 {
		t.Fatalf("create main-thanos: %d %s", created.Status, created.Body)
	}
	_, firstRow := connectionLocator(t, created.Body)
	if firstRow == 0 {
		t.Fatalf("create must return the connection projection: %s", created.Body)
	}
	probe := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/main-thanos/probe", origin, fmt.Sprintf(`{"clientCommandId":"t07-probe-main-%d"}`, time.Now().UnixNano()))
	if probe.Status != 202 {
		t.Fatalf("probe main-thanos: %d %s", probe.Status, probe.Body)
	}
	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(probe.Body), &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("probe accepted body: %s", probe.Body)
	}
	terminal := probeState(t, admin, base, origin, "main-thanos", accepted.ID)
	if state, _ := terminal["state"].(string); state != "Succeeded" {
		t.Fatalf("real thanos probe must succeed, got %v", terminal)
	}
	results := probeResults(t, admin, base, origin, "main-thanos")
	if len(results) != 1 {
		t.Fatalf("expected one result, got %d", len(results))
	}
	if outcome, _ := results[0]["outcome"].(string); outcome != "passed" {
		t.Fatalf("expected passed, got %v", results[0])
	}
	if actionSet, _ := results[0]["actionSetId"].(string); actionSet != "thanos-query-v1" {
		t.Fatalf("expected frozen action set, got %v", results[0])
	}
	details, _ := results[0]["details"].(map[string]any)
	if details["query"] != "vector(1)" || details["responseType"] != "vector" {
		t.Fatalf("typed detail wrong: %v", details)
	}
	if fmt.Sprint(details["sampleValue"]) != "1" {
		t.Fatalf("sample value must be 1, got %v", details["sampleValue"])
	}
	evidence.note(t, "thanos-passed-result.json", mustJSON(t, results[0]))

	// --- 2. secret-input command replay returns the original -------------
	replayed := createConnection(t, admin, base, origin, "t07-create-main", "main-thanos", map[string]any{
		"type": "thanos", "baseUrl": "http://quoin-t07-thanos:9090", "password": "t07-thanos-retry-secret",
	})
	secrets = append(secrets, "t07-thanos-retry-secret")
	if replayed.Status != 201 {
		t.Fatalf("replay create: %d %s", replayed.Status, replayed.Body)
	}
	_, replayRow := connectionLocator(t, replayed.Body)
	if replayRow != firstRow {
		t.Fatalf("replay must return the original connection row: %d vs %d", replayRow, firstRow)
	}
	secrets = append(secrets, "a-different-secret")

	// --- 3. deterministic wrong-shape fixture fails with typed detail -----
	wrong := createConnection(t, admin, base, origin, "t07-create-wrongshape", "wrongshape-thanos", map[string]any{
		"type": "thanos", "baseUrl": "http://quoin-t07-wrongshape",
	})
	if wrong.Status != 201 {
		t.Fatalf("create wrongshape: %d %s", wrong.Status, wrong.Body)
	}
	probeWrong := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/wrongshape-thanos/probe", origin, fmt.Sprintf(`{"clientCommandId":"t07-probe-wrongshape-%d"}`, time.Now().UnixNano()))
	if probeWrong.Status != 202 {
		t.Fatalf("probe wrongshape: %d %s", probeWrong.Status, probeWrong.Body)
	}
	var wrongAccepted struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(probeWrong.Body), &wrongAccepted)
	wrongTerminal := probeState(t, admin, base, origin, "wrongshape-thanos", wrongAccepted.ID)
	if state, _ := wrongTerminal["state"].(string); state != "Failed" {
		t.Fatalf("wrong-shape fixture must fail, got %v", wrongTerminal)
	}
	wrongResults := probeResults(t, admin, base, origin, "wrongshape-thanos")
	if len(wrongResults) != 1 || wrongResults[0]["outcome"] != "failed" {
		t.Fatalf("wrong-shape result must be failed: %v", wrongResults)
	}
	wrongDetails, _ := wrongResults[0]["details"].(map[string]any)
	if wrongDetails["responseType"] != "matrix" {
		t.Fatalf("wrong-shape detail must carry the observed shape: %v", wrongDetails)
	}
	evidence.note(t, "thanos-wrongshape-result.json", mustJSON(t, wrongResults[0]))

	// --- 4. unroutable target: active-conflict, cancel commit order -------
	unroutable := createConnection(t, admin, base, origin, "t07-create-hang", "unroutable-thanos", map[string]any{
		"type": "thanos", "baseUrl": "http://quoin-t07-hang",
	})
	if unroutable.Status != 201 {
		t.Fatalf("create unroutable: %d %s", unroutable.Status, unroutable.Body)
	}
	probeSlow := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/unroutable-thanos/probe", origin, fmt.Sprintf(`{"clientCommandId":"t07-probe-unroutable-%d"}`, time.Now().UnixNano()))
	if probeSlow.Status != 202 {
		t.Fatalf("probe unroutable: %d %s", probeSlow.Status, probeSlow.Body)
	}
	var slowAccepted struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(probeSlow.Body), &slowAccepted)
	// One active probe per connection while it runs.
	conflict := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/unroutable-thanos/probe", origin, fmt.Sprintf(`{"clientCommandId":"t07-probe-conflict-%d"}`, time.Now().UnixNano()))
	if conflict.Status != 409 {
		t.Fatalf("second active probe must conflict, got %d %s", conflict.Status, conflict.Body)
	}
	// Wait until the attempt is Running, then commit the cancellation fence.
	t.Cleanup(func() {
		if t.Failed() {
			if logs, err := exec.Command("docker", "logs", "quoin-plinth-1").CombinedOutput(); err == nil {
				os.WriteFile(filepath.Join(evidenceDir, "plinth-logs-on-failure.log"), logs, 0o644)
			}
		}
	})
	slowView := map[string]any{}
	waitFor(t, "unroutable probe Running", func() bool {
		response := doRequest(t, admin, http.MethodGet, base+"/api/v1/connections/unroutable-thanos/probe-attempts/"+slowAccepted.ID, origin, "")
		if response.Status != http.StatusOK {
			return false
		}
		json.Unmarshal([]byte(response.Body), &slowView)
		state, _ := slowView["state"].(string)
		return state == "Running"
	})
	// The row-version fence may reject a stale expected version (the
	// attempt's row advanced after the read); re-read and retry once like
	// the product guidance tells the operator to.
	cancel := statusResponse{}
	for attempt := 0; attempt < 3; attempt++ {
		refresh := doRequest(t, admin, http.MethodGet, base+"/api/v1/connections/unroutable-thanos/probe-attempts/"+slowAccepted.ID, origin, "")
		if refresh.Status != http.StatusOK {
			t.Fatalf("cancel refresh read: %d %s", refresh.Status, refresh.Body)
		}
		var fresh struct {
			State      string  `json:"state"`
			RowVersion float64 `json:"rowVersion"`
		}
		if err := json.Unmarshal([]byte(refresh.Body), &fresh); err != nil {
			t.Fatal(err)
		}
		if fresh.State != "Running" {
			t.Fatalf("attempt left Running before cancel: %s", refresh.Body)
		}
		cancel = doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/unroutable-thanos/probe-attempts/"+slowAccepted.ID+"/cancel", origin, fmt.Sprintf(`{"clientCommandId":"t07-cancel-%d-%d","expectedRowVersion":%d}`, attempt, time.Now().UnixNano(), int64(fresh.RowVersion)))
		if cancel.Status == http.StatusOK {
			break
		}
	}
	if cancel.Status != 200 {
		t.Fatalf("cancel: %d %s", cancel.Status, cancel.Body)
	}
	cancelled := probeState(t, admin, base, origin, "unroutable-thanos", slowAccepted.ID)
	if state, _ := cancelled["state"].(string); state != "Cancelled" {
		t.Fatalf("cancelled probe must reach Cancelled, got %v", cancelled)
	}
	cancelResults := probeResults(t, admin, base, origin, "unroutable-thanos")
	if len(cancelResults) != 1 || cancelResults[0]["outcome"] != "cancelled" {
		t.Fatalf("cancel closure must write the cancelled result: %v", cancelResults)
	}
	evidence.note(t, "thanos-cancelled-result.json", mustJSON(t, cancelResults[0]))

	// --- 5. enable fences -------------------------------------------------
	enabled := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/main-thanos/enable", origin, fmt.Sprintf(`{"clientCommandId":"t07-enable-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), firstRow))
	if enabled.Status != 200 {
		t.Fatalf("enable main-thanos: %d %s", enabled.Status, enabled.Body)
	}
	staleEnable := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/main-thanos/disable", origin, fmt.Sprintf(`{"clientCommandId":"t07-disable-stale-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), firstRow))
	if staleEnable.Status != 409 {
		t.Fatalf("stale row version must conflict, got %d %s", staleEnable.Status, staleEnable.Body)
	}
	// Single-enabled thanos: a second enabled thanos conflicts.
	backup := createConnection(t, admin, base, origin, "t07-create-backup", "backup-thanos", map[string]any{
		"type": "thanos", "baseUrl": "http://quoin-t07-thanos:9090",
	})
	if backup.Status != 201 {
		t.Fatalf("create backup: %d %s", backup.Status, backup.Body)
	}
	_, backupRow := connectionLocator(t, backup.Body)
	secondEnable := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/backup-thanos/enable", origin, fmt.Sprintf(`{"clientCommandId":"t07-enable-backup-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), backupRow))
	if secondEnable.Status != 409 {
		t.Fatalf("second enabled thanos must conflict, got %d %s", secondEnable.Status, secondEnable.Body)
	}

	// --- evidence + cleanup ------------------------------------------------
	commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	dirtyDigest := sha256Hex([]byte(outputOf(t, "git", "status", "--porcelain")))
	os.WriteFile(filepath.Join(evidenceDir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit":        commit,
		"dirtyStateDigest": dirtyDigest,
		"startedAt":        startedAt.Format(time.RFC3339),
		"commands":         evidence.commands,
		"components": map[string]any{
			"realTargets":  "thanosio/thanos:v0.36.0 (query) answering vector(1) over the stack network",
			"negatives":    "nginx fixed-shape fixture (resultType=matrix) and RFC5737 unroutable address",
			"executedPath": "HTTP create(probe) → DispatchAttempt over live Connect stream → Plinth supervisor FetchCredentialGrant → typed ResultProposal → SQLite closure",
		},
		"observed": map[string]any{
			"transitions": []string{
				"plinth registered and connected before any probe",
				"real thanos probe passed with query=vector(1), sampleValue=1",
				"wrong-shape fixture failed with the observed resultType in the typed detail",
				"second active probe rejected with 409 while the first runs",
				"cancellation fence committed first; attempt reached Cancelled with the cancelled typed result",
				"secret-input create replay returned the original connection without secret comparison",
				"stale row version enable/disable conflicts rejected; second enabled thanos conflicts",
			},
		},
		"expectedVersusActual": map[string]string{
			"real Thanos target":     "actual: probe Succeeded, result passed, action set thanos-query-v1 v1",
			"deterministic negative": "actual: wrong-shape failed with typed detail; unroutable cancelled with typed result",
			"closure ordering":       "actual: cancel fence precedes CancelAttempt; late runtime results rejected (unit race tests)",
			"reveal replay/tamper":   "actual: create replay by clientCommandId returns original; grant fences covered by unit tests (boot/epoch/attempt/terminal)",
		},
		"redactions": "admin passwords, registration token and secret values redacted; evidence scanned",
	})), 0o644)

	// Owned fixtures removed; pre-existing stack resources untouched.
	cleanup := []string{}
	for _, name := range []string{"quoin-t07-thanos", "quoin-t07-wrongshape", "quoin-t07-hang"} {
		output, code := evidence.runAllowFail(t, "cleanup-"+name, "docker", "rm", "-f", "-v", name)
		_ = output
		cleanup = append(cleanup, fmt.Sprintf("%s: removed (exit %d)", name, code))
	}
	downOutput, downCode := evidence.runAllowFail(t, "teardown-stack", "docker", "compose", "--project-name", projectName, "down", "--remove-orphans")
	_ = downOutput
	cleanup = append(cleanup, fmt.Sprintf("compose stack %s: down (exit %d)", projectName, downCode))
	os.WriteFile(filepath.Join(evidenceDir, "cleanup.json"), []byte(mustJSON(t, map[string]any{
		"owned":       cleanup,
		"preExisting": "images and unrelated containers untouched; workdir is a test TempDir",
		"secrets":     "fixture secrets existed only in request bodies and container memory; stack removed",
		"timestamp":   time.Now().Format(time.RFC3339),
	})), 0o644)
	scanForSecrets(t, evidenceDir, secrets...)
}

// scanForSecrets fails the run if any protected value leaked into evidence.
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
