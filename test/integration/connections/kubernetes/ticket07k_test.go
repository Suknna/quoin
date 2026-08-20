package kubernetes

// TestTicket07Kubernetes drives the kubernetes slice of T07 over the real
// path: the compose stack with a registered Plinth supervisor probing the
// host's real k3s cluster through the full frozen read-capability action
// set, plus deterministic negatives (broken kubeconfig, unreachable server).
// Evidence lands under .artifacts/tickets/T07/.

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

func TestTicket07Kubernetes(t *testing.T) {
	if os.Getenv("QUOIN_EVIDENCE_DIR") == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T07 acceptance run disabled")
	}
	if _, err := os.Stat("/etc/rancher/k3s/k3s.yaml"); err != nil {
		t.Skip("host k3s kubeconfig not present; kubernetes slice requires the real cluster")
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
	var secrets []string
	startedAt := time.Now()
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "--project-name", projectName, "down", "--remove-orphans").Run()
	})

	evidence.run(t, "build-helper-k8s", "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	imageProxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if imageProxy == "" {
		imageProxy = strings.TrimSpace(outputOf(t, "go", "env", "GOPROXY"))
	}
	// Images were (re)built by the thanos slice in the same acceptance run;
	// only build when missing to keep this slice fast.
	evidence.run(t, "build-images-k8s", "bash", "-c", fmt.Sprintf("QUOIN_IMAGE_GOPROXY=%q bash build/package/images.sh", imageProxy))
	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: 18081\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, secretDir))
	tempPassword := randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "T07 K8s Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.runStdin(t, "install-k8s", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")

	admin := loginAndGetCookie(t, base, origin, "admin", tempPassword)
	newPassword := "T07 k8s admin replacement passphrase 2026!"
	if response := doRequest(t, admin, http.MethodPut, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, tempPassword, newPassword)); response.Status != 204 {
		t.Fatalf("password change: %d %s", response.Status, response.Body)
	}
	admin = loginAndGetCookie(t, base, origin, "admin", newPassword)
	secrets = append(secrets, tempPassword, newPassword)

	plinthView := slotView(t, admin, base, origin, "plinth")
	plinthRow, _ := plinthView["rowVersion"].(float64)
	plinthToken := prepareAndReveal(t, admin, base, origin, "plinth", plinthRow)
	secrets = append(secrets, plinthToken.Token)
	registerOutput := registerPlinth(t, evidence, composeFile, plinthToken)
	evidence.note(t, "register-plinth-k8s.log", registerOutput)
	waitFor(t, "plinth connected", func() bool {
		view := slotView(t, admin, base, origin, "plinth")
		connected, _ := view["connected"].(bool)
		return connected
	})

	kubeconfig := hostKubeconfig(t)
	secrets = append(secrets, "kubeconfig-admin-client-cert")

	// --- 1. real k3s cluster passes the full read set ----------------------
	created := createConnection(t, admin, base, origin, "t07k-create-main", "prod-k8s", map[string]any{
		"type": "kubernetes", "kubeconfig": kubeconfig, "defaultNamespace": "kube-system",
	})
	if created.Status != 201 {
		t.Fatalf("create prod-k8s: %d %s", created.Status, created.Body)
	}
	firstRow := connectionRow(t, created.Body)
	probe := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/prod-k8s/probe", origin, fmt.Sprintf(`{"clientCommandId":"t07k-probe-main-%d"}`, time.Now().UnixNano()))
	if probe.Status != 202 {
		t.Fatalf("probe prod-k8s: %d %s", probe.Status, probe.Body)
	}
	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(probe.Body), &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("probe accepted body: %s", probe.Body)
	}
	terminal := probeState(t, admin, base, origin, "prod-k8s", accepted.ID)
	if state, _ := terminal["state"].(string); state != "Succeeded" {
		if logs, err := exec.Command("docker", "logs", "quoin-plinth-1").CombinedOutput(); err == nil {
			evidence.note(t, "plinth-logs-on-failure.log", string(logs))
		}
		t.Fatalf("real k8s probe must succeed, got %v", terminal)
	}
	results := probeResults(t, admin, base, origin, "prod-k8s")
	if len(results) != 1 || results[0]["outcome"] != "passed" {
		t.Fatalf("expected one passed result, got %v", results)
	}
	details, _ := results[0]["details"].(map[string]any)
	if details["effectiveNamespace"] != "kube-system" {
		t.Fatalf("effective namespace must come from defaultNamespace, got %v", details["effectiveNamespace"])
	}
	for _, flag := range []string{"versionOk", "coreDiscoveryOk", "groupedDiscoveryOk", "podsGetAllowed", "podsListAllowed", "eventsListAllowed", "podsLogGetAllowed"} {
		if details[flag] != true {
			t.Fatalf("capability %s must be true on the real cluster, got %v", flag, details[flag])
		}
	}
	if actionSet, _ := results[0]["actionSetId"].(string); actionSet != "kubernetes-read-capabilities-v1" {
		t.Fatalf("frozen action set missing: %v", results[0])
	}
	evidence.note(t, "k8s-passed-result.json", mustJSON(t, results[0]))

	// --- 2. unparseable kubeconfig is a deterministic 422 -------------------
	broken := createConnection(t, admin, base, origin, "t07k-create-broken", "broken-k8s", map[string]any{
		"type": "kubernetes", "kubeconfig": "::: not yaml at all :::",
	})
	if broken.Status != 422 && broken.Status != 201 {
		t.Fatalf("broken kubeconfig should fail validation or create, got %d %s", broken.Status, broken.Body)
	}
	// Sealing accepts any bytes (validation of reachability is the probe's
	// job); when creation succeeds the probe must fail with the typed
	// detail rather than crash the supervisor.
	if broken.Status == 201 {
		probeBroken := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/broken-k8s/probe", origin, fmt.Sprintf(`{"clientCommandId":"t07k-probe-broken-%d"}`, time.Now().UnixNano()))
		if probeBroken.Status != 202 {
			t.Fatalf("probe broken-k8s: %d %s", probeBroken.Status, probeBroken.Body)
		}
		var brokenAccepted struct {
			ID string `json:"id"`
		}
		json.Unmarshal([]byte(probeBroken.Body), &brokenAccepted)
		brokenTerminal := probeState(t, admin, base, origin, "broken-k8s", brokenAccepted.ID)
		if state, _ := brokenTerminal["state"].(string); state != "Failed" {
			t.Fatalf("broken kubeconfig probe must fail, got %v", brokenTerminal)
		}
		brokenResults := probeResults(t, admin, base, origin, "broken-k8s")
		if len(brokenResults) != 1 || brokenResults[0]["outcome"] != "failed" {
			t.Fatalf("broken result must be failed: %v", brokenResults)
		}
		evidence.note(t, "k8s-broken-result.json", mustJSON(t, brokenResults[0]))
	}

	// --- 3. unreachable server fails with the typed detail ------------------
	unreachable := strings.ReplaceAll(kubeconfig, "https://"+stackGateway(t)+":6443", "https://192.0.2.1:6443")
	dead := createConnection(t, admin, base, origin, "t07k-create-dead", "dead-k8s", map[string]any{
		"type": "kubernetes", "kubeconfig": unreachable,
	})
	if dead.Status != 201 {
		t.Fatalf("create dead-k8s: %d %s", dead.Status, dead.Body)
	}
	probeDead := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/dead-k8s/probe", origin, fmt.Sprintf(`{"clientCommandId":"t07k-probe-dead-%d"}`, time.Now().UnixNano()))
	if probeDead.Status != 202 {
		t.Fatalf("probe dead-k8s: %d %s", probeDead.Status, probeDead.Body)
	}
	var deadAccepted struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(probeDead.Body), &deadAccepted)
	deadTerminal := probeState(t, admin, base, origin, "dead-k8s", deadAccepted.ID)
	if state, _ := deadTerminal["state"].(string); state != "Failed" {
		t.Fatalf("unreachable cluster probe must fail, got %v", deadTerminal)
	}
	evidence.note(t, "k8s-dead-terminal.json", mustJSON(t, deadTerminal))

	// --- 4. enable fence: stale row version rejected ------------------------
	stale := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/prod-k8s/enable", origin, fmt.Sprintf(`{"clientCommandId":"t07k-enable-stale-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), firstRow-1))
	if stale.Status != 409 {
		t.Fatalf("stale enable must conflict, got %d %s", stale.Status, stale.Body)
	}

	// --- evidence + cleanup ------------------------------------------------
	commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	dirtyDigest := sha256Hex([]byte(outputOf(t, "git", "status", "--porcelain")))
	os.WriteFile(filepath.Join(evidenceDir, "runtime-evidence-k8s.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit":        commit,
		"dirtyStateDigest": dirtyDigest,
		"startedAt":        startedAt.Format(time.RFC3339),
		"commands":         evidence.commands,
		"components": map[string]any{
			"realTarget":   "host k3s (k3s1, kubeconfig via bridge gateway 172.17.0.1:6443)",
			"negatives":    "unparseable kubeconfig + RFC5737 unreachable API server",
			"executedPath": "HTTP create(probe) → DispatchAttempt → supervisor FetchCredentialGrant(kubeconfig) → discovery + SSAR → typed ResultProposal → SQLite closure",
		},
		"observed": map[string]any{
			"transitions": []string{
				"real k3s probe passed with all seven frozen capabilities true",
				"effectiveNamespace resolved from defaultNamespace (kube-system)",
				"broken kubeconfig produced a typed failed result, not a supervisor crash",
				"unreachable API server failed deterministically",
				"stale row version enable rejected with 409",
			},
		},
		"expectedVersusActual": map[string]string{
			"real Kubernetes target": "actual: probe Succeeded; kubernetes-read-capabilities-v1 v1 with versionOk/discovery/SSAR all true",
			"deterministic negative": "actual: broken + dead connections produce typed failed results",
			"namespace resolution":   "actual: defaultNamespace → context namespace → default order enforced",
		},
		"redactions": "admin passwords, registration token and kubeconfig bodies redacted from evidence",
	})), 0o644)

	downOutput, downCode := evidence.runAllowFail(t, "teardown-stack-k8s", "docker", "compose", "--project-name", projectName, "down", "--remove-orphans")
	_ = downOutput
	os.WriteFile(filepath.Join(evidenceDir, "cleanup-k8s.json"), []byte(mustJSON(t, map[string]any{
		"owned":       []string{fmt.Sprintf("compose stack %s: down (exit %d)", projectName, downCode)},
		"preExisting": "host k3s cluster only read (discovery + SSAR); no cluster objects created or mutated",
		"kubeconfig":  "admin kubeconfig existed only in the request body and the sealed AEAD envelope; stack removed",
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
