package refresh

// TestTicket18 proves the T18 capability over the real compose stack:
// a registered Plinth supervisor executes the PromQL Config Verification
// checks against a deterministic Prometheus-compatible fixture through the
// frozen config_thanos_query grant, the run closes Passed with per-check
// Evidence; the manual Resource Refresh freezes the published discoveries,
// executes them through the same supervisor path and projects the current
// Observed Resources with identity labels and freshness.

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

const t18SystemYAML = `system_key: payments
display_name: 支付系统
enabled: true
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries:
  - key: web-pods
    display_name: Web Pods
    selector: 'up{business_system="payments", job="web"}'
    identity_labels: [job, instance]
inspection_plans:
  - key: daily-check
    display_name: Daily Check
    cron: "30 8 * * *"
    checks:
      - key: up-instant
        display_name: Up Instant
        analysis_question: 当前可用吗？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="payments"}'
`

func TestTicket18(t *testing.T) {
	if os.Getenv("QUOIN_EVIDENCE_DIR") == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T18 acceptance run disabled")
	}
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: append([]string{}, os.Environ()...)}
	evidence.env = append(evidence.env, "QUOIN_FORCE_IMAGE_BUILD=1")
	imageProxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if imageProxy == "" {
		imageProxy = strings.TrimSpace(outputOf(t, "go", "env", "GOPROXY"))
	}
	if imageProxy != "" {
		evidence.env = append(evidence.env, "QUOIN_IMAGE_GOPROXY="+imageProxy)
	}
	t.Cleanup(func() {
		if t.Failed() {
			if logs, err := exec.Command("docker", "logs", "quoin-quoin-1").CombinedOutput(); err == nil {
				os.WriteFile(filepath.Join(evidenceDir, "quoin-logs-on-failure.log"), logs, 0o644)
			}
			if logs, err := exec.Command("docker", "logs", "quoin-plinth-1").CombinedOutput(); err == nil {
				os.WriteFile(filepath.Join(evidenceDir, "plinth-logs-on-failure.log"), logs, 0o644)
			}
		}
		_ = exec.Command("docker", "rm", "-f", "-v", "quoin-t18-fixture").Run()
		_ = exec.Command("docker", "compose", "--project-name", projectName, "down", "--remove-orphans").Run()
	})
	workRoot := t.TempDir()
	stateRoot := filepath.Join(workRoot, "state")
	secretDir := filepath.Join(workRoot, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	evidence.env = append(evidence.env, "XDG_STATE_HOME="+stateRoot)
	startedAt := time.Now()

	// --- stack bring-up (canonical helper, images, scripted install) ------
	evidence.run(t, "build-helper", "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	evidence.run(t, "build-images", "bash", "-c", fmt.Sprintf("QUOIN_IMAGE_GOPROXY=%q QUOIN_FORCE_IMAGE_BUILD=1 bash build/package/images.sh", imageProxy))
	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, stelePort, secretDir))
	tempPassword := randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "T18 Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.runStdin(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"

	admin := loginAndGetCookie(t, base, origin, "admin", tempPassword)
	newPassword := "T18 admin replacement passphrase 2026!"
	if response := doRequest(t, admin, http.MethodPut, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, tempPassword, newPassword)); response.Status != 204 {
		t.Fatalf("password change: %d %s", response.Status, response.Body)
	}
	admin = loginAndGetCookie(t, base, origin, "admin", newPassword)

	// --- register plinth so the supervisor is connected -------------------
	plinthView := slotView(t, admin, base, origin, "plinth")
	plinthRowValue, _ := plinthView["rowVersion"].(float64)
	plinthToken := prepareAndReveal(t, admin, base, origin, "plinth", plinthRowValue)
	registerOutput := registerPlinth(t, evidence, composeFile, plinthToken)
	evidence.note(t, "register-plinth-1.log", registerOutput)
	waitFor(t, "plinth connected", func() bool {
		view := slotView(t, admin, base, origin, "plinth")
		connected, _ := view["connected"].(bool)
		return connected
	})

	// --- deterministic Prometheus-compatible fixture on the stack network --
	// Any query answers one series carrying the identity labels the published
	// discovery projects; verification needs a successful typed response.
	evidence.run(t, "pull-nginx", "docker", "pull", "nginx:1.27-alpine")
	network := quoinNetwork(t)
	fixtureConf := filepath.Join(workRoot, "fixture.conf")
	writeFile(t, fixtureConf, `server {
  listen 80;
  location = /api/v1/query {
    default_type application/json;
    return 200 '{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up","business_system":"payments","job":"web","instance":"10.0.0.1:9090"},"value":[1700000000,"1"]}]}}';
  }
}
`)
	startFixture(t, evidence, network, "quoin-t18-fixture", "-v", fixtureConf+":/etc/nginx/conf.d/default.conf:ro", "nginx:1.27-alpine")
	waitFor(t, "fixture reachable in-network", func() bool {
		probe := exec.Command("docker", "run", "--rm", "--network", network, "busybox", "wget", "-qO-", "http://quoin-t18-fixture/api/v1/query?query=vector%281%29")
		return probe.Run() == nil
	})

	// --- the one enabled deployment Thanos connection ---------------------
	created := createConnection(t, admin, base, origin, "t18-create-main", "main-thanos", map[string]any{
		"type": "thanos", "baseUrl": "http://quoin-t18-fixture", "password": "t18-thanos-password",
	})
	if created.Status != 201 {
		t.Fatalf("create main-thanos: %d %s", created.Status, created.Body)
	}
	probe := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/main-thanos/probe", origin, fmt.Sprintf(`{"clientCommandId":"t18-probe-main-%d"}`, time.Now().UnixNano()))
	if probe.Status != 202 {
		t.Fatalf("probe main-thanos: %d %s", probe.Status, probe.Body)
	}
	var probeAccepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(probe.Body), &probeAccepted); err != nil || probeAccepted.ID == "" {
		t.Fatalf("probe accepted body: %s", probe.Body)
	}
	terminal := probeState(t, admin, base, origin, "main-thanos", probeAccepted.ID)
	if state, _ := terminal["state"].(string); state != "Succeeded" {
		t.Fatalf("fixture probe must succeed before config execution: %v", terminal)
	}
	// The passed probe advanced the connection's rowVersion; re-read the
	// current row like the product guidance tells the operator to.
	var enableRow int64
	waitFor(t, "connection row readable", func() bool {
		list := doRequest(t, admin, http.MethodGet, base+"/api/v1/connections?limit=100", origin, "")
		if list.Status != http.StatusOK {
			return false
		}
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(list.Body), &page); err != nil {
			return false
		}
		for _, item := range page.Items {
			if item["name"] == "main-thanos" {
				row, _ := item["rowVersion"].(float64)
				enableRow = int64(row)
				return row > 0
			}
		}
		return false
	})
	enabled := doRequest(t, admin, http.MethodPost, base+"/api/v1/connections/main-thanos/enable", origin, fmt.Sprintf(`{"clientCommandId":"t18-enable-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), enableRow))
	if enabled.Status != 200 {
		t.Fatalf("enable main-thanos: %d %s", enabled.Status, enabled.Body)
	}

	// --- Label Contract prerequisite --------------------------------------
	contractYAML := "label_contract:\n  business_system_label: business_system\n"
	status, body := uploadMultipart(t, admin, base+"/api/v1/label-contracts", origin,
		map[string]string{"clientCommandId": "t18-contract-create-1"}, "file", "contract.yaml", []byte(contractYAML))
	if status != 201 {
		t.Fatalf("contract draft create: status=%d body=%.500s", status, body)
	}
	activation := doRequest(t, admin, http.MethodPost, base+"/api/v1/label-contracts/1/activate", origin,
		`{"clientCommandId":"t18-contract-activate-1","expectedStateRowVersion":1,"expectedCurrentContractVersionId":null,"expectedTargetRowVersion":1,"compatibleVersions":[]}`)
	if activation.Status != 200 {
		t.Fatalf("contract activate: %d %s", activation.Status, activation.Body)
	}

	// --- config upload + publish -------------------------------------------
	uploadStatus, uploadBody := uploadMultipart(t, admin, base+"/api/v1/business-systems", origin,
		map[string]string{"clientCommandId": "t18-upload-1", "targetLabelContractVersion": "1"}, "file", "config.yaml", []byte(t18SystemYAML))
	if uploadStatus != 201 {
		t.Fatalf("config upload: status=%d body=%.800s", uploadStatus, uploadBody)
	}
	var version struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(uploadBody), &version); err != nil || version.ID == "" {
		t.Fatalf("upload must return the immutable version id: %s", uploadBody)
	}

	// --- Config Verification PromQL execution (on the draft) ---------------
	run := doRequest(t, admin, http.MethodPost, base+"/api/v1/business-systems/payments/config/"+version.ID+"/verifications", origin,
		`{"clientCommandId":"t18-run-1","purpose":"prepublish"}`)
	if run.Status != 202 {
		t.Fatalf("run verification: %d %s", run.Status, run.Body)
	}
	var runAccepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(run.Body), &runAccepted); err != nil || runAccepted.ID == "" {
		t.Fatalf("verification accepted body: %s", run.Body)
	}
	var finalRun map[string]any
	waitFor(t, "config verification terminal", func() bool {
		response := doRequest(t, admin, http.MethodGet, base+"/api/v1/business-systems/payments/config/"+version.ID+"/verifications/"+runAccepted.ID, origin, "")
		if response.Status != http.StatusOK {
			t.Fatalf("verification read: %d %s", response.Status, response.Body)
		}
		if err := json.Unmarshal([]byte(response.Body), &finalRun); err != nil {
			t.Fatal(err)
		}
		state, _ := finalRun["state"].(string)
		evidence.note(t, "verification-run-progress.json", response.Body)
		switch state {
		case "Passed", "Failed", "Cancelled", "Interrupted":
			return true
		}
		return false
	})
	if state, _ := finalRun["state"].(string); state != "Passed" {
		t.Fatalf("config verification must pass over the supervisor PromQL path: %v", finalRun)
	}
	checks, _ := finalRun["checkResults"].([]any)
	if len(checks) != 1 {
		t.Fatalf("one check result expected, got %v", finalRun["checkResults"])
	}
	check := checks[0].(map[string]any)
	if check["planKey"] != "daily-check" || check["checkKey"] != "up-instant" || check["status"] != "ok" || check["evidenceId"] == nil {
		t.Fatalf("check result must be ok with evidence: %v", check)
	}
	evidence.note(t, "verification-run-passed.json", mustJSON(t, finalRun))

	// --- publish the verified draft -----------------------------------------
	published := doRequest(t, admin, http.MethodPost, base+"/api/v1/business-systems/payments/config/"+version.ID+"/publish", origin,
		`{"clientCommandId":"t18-publish-1","expectedCurrentPublishedVersionId":null}`)
	if published.Status != 200 || !strings.Contains(published.Body, `"currentConfigVersionId":"`+version.ID+`"`) {
		t.Fatalf("publish: %d %s", published.Status, published.Body)
	}

	// --- manual Resource Refresh -------------------------------------------
	refresh := doRequest(t, admin, http.MethodPost, base+"/api/v1/business-systems/payments/resources:refresh", origin,
		`{"clientCommandId":"t18-refresh-1"}`)
	if refresh.Status != 202 {
		t.Fatalf("start refresh: %d %s", refresh.Status, refresh.Body)
	}
	var refreshAccepted struct {
		ID          string `json:"id"`
		TriggerKind string `json:"triggerKind"`
	}
	if err := json.Unmarshal([]byte(refresh.Body), &refreshAccepted); err != nil || refreshAccepted.ID == "" {
		t.Fatalf("refresh accepted body: %s", refresh.Body)
	}
	if refreshAccepted.TriggerKind != "manual" {
		t.Fatalf("refresh trigger kind: %q", refreshAccepted.TriggerKind)
	}
	var finalRefresh map[string]any
	waitFor(t, "resource refresh terminal", func() bool {
		response := doRequest(t, admin, http.MethodGet, base+"/api/v1/business-systems/payments/resource-refresh-runs/"+refreshAccepted.ID, origin, "")
		if response.Status != http.StatusOK {
			t.Fatalf("refresh read: %d %s", response.Status, response.Body)
		}
		if err := json.Unmarshal([]byte(response.Body), &finalRefresh); err != nil {
			t.Fatal(err)
		}
		state, _ := finalRefresh["state"].(string)
		switch state {
		case "Completed", "CompletedWithWarnings", "Failed", "Cancelled", "Interrupted":
			return true
		}
		return false
	})
	if state, _ := finalRefresh["state"].(string); state != "Completed" {
		t.Fatalf("resource refresh must complete over the supervisor path: %v", finalRefresh)
	}
	evidence.note(t, "refresh-run-completed.json", mustJSON(t, finalRefresh))

	// --- current Observed Resources projection ------------------------------
	resources := doRequest(t, admin, http.MethodGet, base+"/api/v1/business-systems/payments/resources?current=true", origin, "")
	if resources.Status != http.StatusOK {
		t.Fatalf("resources list: %d %s", resources.Status, resources.Body)
	}
	var resourcePage struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(resources.Body), &resourcePage); err != nil {
		t.Fatal(err)
	}
	if len(resourcePage.Items) != 1 {
		t.Fatalf("one current resource expected: %s", resources.Body)
	}
	resource := resourcePage.Items[0]
	labels, _ := resource["identityLabels"].(map[string]any)
	if resource["discoveryKey"] != "web-pods" || labels["job"] != "web" || labels["instance"] != "10.0.0.1:9090" {
		t.Fatalf("resource identity labels wrong: %v", resource)
	}
	if current, _ := resource["current"].(bool); !current {
		t.Fatalf("resource must be current: %v", resource)
	}
	if stale, _ := resource["stale"].(bool); stale {
		t.Fatalf("freshly refreshed resource must not be stale: %v", resource)
	}
	if observedAt, _ := resource["observedAt"].(string); observedAt == "" {
		t.Fatalf("resource must carry its observation time: %v", resource)
	}
	evidence.note(t, "observed-resources.json", resources.Body)

	// --- acceptance evidence summary ----------------------------------------
	outcome := map[string]any{
		"startedAt": startedAt.UTC().Format(time.RFC3339),
		"outcome":   "complete",
		"components": map[string]any{
			"deployHelper":   "cmd/quoin-deploy (go build -trimpath, host binary)",
			"composeProject": projectName,
			"fixture":        "nginx:1.27-alpine deterministic Prometheus-compatible vector on the quoin_internal network",
			"supervisor":     "registered Plinth executing config_thanos_query grants",
		},
		"verification": map[string]any{"runId": runAccepted.ID, "state": finalRun["state"]},
		"refresh":      map[string]any{"runId": refreshAccepted.ID, "state": finalRefresh["state"]},
		"resources":    resourcePage.Items,
		"redactions":   "admin passwords are not written to evidence",
	}
	evidence.note(t, "runtime-evidence.json", mustJSON(t, outcome))
	evidence.note(t, "commands.json", mustJSON(t, evidence.commands))
}
