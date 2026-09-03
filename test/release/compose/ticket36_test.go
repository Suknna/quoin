package release_test

// TestTicket36Compose drives the real Compose coordinated-upgrade path: a
// real install, an Admin-driven prepareUpgrade with a queued connection
// probe as drainable work, the drain through the real upgrade-drain cancel,
// the verified pre-upgrade backup, the pre-write image-only rollback
// mechanism (mechanism evidence), a re-prepare after the abort, and the
// actual `quoin-deploy compose upgrade` helper through offline
// verification, exclusive migration and the ordered restart.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUpgradeComposeTicket36(t *testing.T) {
	evidenceRoot := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceRoot == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T36 acceptance evidence run disabled")
	}
	requireTools(t)
	evidenceDir := filepath.Join(evidenceRoot, "compose")
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recorder := newEvidence(t, evidenceDir)
	suffix := strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 36)
	mainProject = "quoin-t36-" + suffix
	registryName, registryRepository = "t36-registry-"+suffix, "t36-"+suffix
	workRoot := t.TempDir()
	baseline := captureEnvironmentBaseline(t, recorder)
	registryRef := startRegistry(t, recorder)
	password := randomPassword(t)
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			cleanupTicketResources(t, recorder, workRoot, registryRef, baseline, password)
		}
	})

	images := buildAndPushReleaseImages(t, recorder, workRoot)
	manifestPath := writeReleaseManifest(t, recorder, workRoot, images)
	helper := filepath.Join(workRoot, "quoin-deploy")
	recorder.run("build-helper", nil, nil, 0, "go", "build", "-trimpath", "-o", helper, "./cmd/quoin-deploy")
	quoinPort, stelePort := 19790, 19791
	configPath := writeInstallConfig(t, workRoot, "t36-install.yaml", filepath.Join(workRoot, "secrets"), quoinPort, stelePort)
	env := composeEnv(workRoot, mainProject)
	recorder.run("install", env, strings.NewReader(strings.Join([]string{"admin", "Ticket 36 Admin", password, password}, "\n")+"\n"), 0,
		helper, "compose", "install", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "install-report.json"))

	base := "http://127.0.0.1:" + strconv.Itoa(quoinPort)
	origin := "https://quoin.example.com"
	composeFile := filepath.Join(workRoot, mainProject, "state", "quoin", "compose", "generated", "compose.yaml")
	client := maintenanceClient(t, base, origin, "admin", password)
	formal := password + "-formal"
	t36Put(t, client, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, password, formal), http.StatusNoContent)
	client = maintenanceClient(t, base, origin, "admin", formal)

	// Drainable browser work: a real manual-login operation that can never
	// dispatch (no Lintel is registered), so only the frozen cancel fence
	// can end it. Task-class drain is proven deterministically by the
	// in-process gate test with a real investigation attempt.
	systemKey := t36SeedBrowserWork(t, client, base, origin)

	// The Admin prepares the coordinated upgrade through the real command.
	prepared := t36Post(t, client, base+"/api/v1/maintenance/upgrade/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t36-prepare-1-%d","expectedRowVersion":1}`, time.Now().UnixNano()), http.StatusAccepted)
	recorder.note("prepare-1.json", prepared)
	state := waitForChecklist(t, client, base, origin, func(state maintenanceStateJSON) bool {
		return state.Active && state.Reason == "Upgrade"
	})
	if len(state.Items) == 0 {
		t.Fatalf("prepare froze an empty checklist: %+v", state)
	}
	// Ordinary product work is denied on the live-swapped maintenance surface.
	if status, _ := t36Get(t, client, base+"/api/v1/alerts", origin); status != http.StatusServiceUnavailable {
		t.Fatalf("ordinary work during Upgrade maintenance status=%d", status)
	}
	recorder.observe("maintenance-swap.json", map[string]any{"alertsStatus": 503, "checklistItems": len(state.Items)})

	// Drain through the real upgrade-drain cancel, using exactly the
	// directive the checklist projected (the UI's vocabulary).
	drain := waitForChecklist(t, client, base, origin, func(state maintenanceStateJSON) bool {
		for _, item := range state.Items {
			if item.Kind == "ActiveBrowserOperation" && item.SafeState == "Blocking" && strings.Contains(item.DetailCode, "cancel:") {
				return true
			}
		}
		return false
	})
	cancelled := false
	for _, item := range drain.Items {
		directive := strings.SplitN(item.DetailCode, "|", 2)
		if item.Kind != "ActiveBrowserOperation" || item.SafeState != "Blocking" || len(directive) != 2 || !strings.HasPrefix(directive[1], "cancel:") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(directive[1], "cancel:"), ":", 2)
		endpoint, tail := parts[0], parts[1]
		index := strings.LastIndexByte(tail, ':')
		pathParams, rowVersion := tail[:index], tail[index+1:]
		route, ok := drainRoute(endpoint, pathParams)
		if !ok {
			t.Fatalf("unknown drain endpoint %q", endpoint)
		}
		var body string
		if endpoint == "browser_operation" {
			body = fmt.Sprintf(`{"clientCommandId":"t36-drain-%d","expectedOperationRowVersion":%s}`, time.Now().UnixNano(), rowVersion)
		} else {
			body = fmt.Sprintf(`{"clientCommandId":"t36-drain-%d","expectedRowVersion":%s}`, time.Now().UnixNano(), rowVersion)
		}
		t36Post(t, client, base+route, origin, body, http.StatusOK)
		cancelled = true
	}
	if !cancelled {
		t.Fatalf("no drainable item in checklist: %+v", drain.Items)
	}
	recorder.observe("drain.json", map[string]any{"cancelledVia": "upgrade-drain cancel", "systemKey": systemKey, "items": len(drain.Items)})

	// The reconciler converges the drained attempt and runs the verified
	// pre-upgrade backup; every item becomes Safe.
	final := waitForChecklist(t, client, base, origin, func(state maintenanceStateJSON) bool {
		if !state.Active || len(state.Items) == 0 {
			return false
		}
		for _, item := range state.Items {
			if item.SafeState != "Safe" {
				return false
			}
		}
		return true
	}, 5*time.Minute)
	recorder.note("prepared-checklist.json", mustJSONString(final))

	// Pre-write rollback mechanism (mechanism evidence only): before the
	// migration commits, restarting the old Release images without any
	// restore returns the stack to service; the Admin aborts through the
	// real exitMaintenance and the process restart boots normal mode.
	recorder.run("rollback-stop", env, nil, 0, "docker", "compose", "--project-name", mainProject, "--file", composeFile, "stop")
	recorder.run("rollback-up-old", env, nil, 0, "docker", "compose", "--project-name", mainProject, "--file", composeFile, "up", "--detach")
	waitForComposeState(t, recorder, env, composeFile, "maintenance")
	t36Post(t, client, base+"/api/v1/maintenance/exit", origin, fmt.Sprintf(`{"clientCommandId":"t36-abort-1-%d","expectedRowVersion":%d,"expectedReason":"Upgrade"}`, time.Now().UnixNano(), final.RowVersion), http.StatusOK)
	recorder.run("rollback-restart-quoin", env, nil, 0, "docker", "compose", "--project-name", mainProject, "--file", composeFile, "restart", "quoin")
	waitForComposeState(t, recorder, env, composeFile, "normal")
	recorder.observe("pre-write-rollback.json", map[string]any{
		"classification": "mechanism-evidence-only",
		"note":           "image-only rollback before the migration commit; no restore involved and no N-1 migration implied",
	})

	// Re-prepare after the abort (a new revision) and wait for prepared again.
	reprepared := t36Post(t, client, base+"/api/v1/maintenance/upgrade/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t36-prepare-2-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), final.RowVersion+1), http.StatusAccepted)
	recorder.note("prepare-2.json", reprepared)
	waitForChecklist(t, client, base, origin, func(state maintenanceStateJSON) bool {
		if !state.Active || len(state.Items) == 0 {
			return false
		}
		for _, item := range state.Items {
			if item.SafeState != "Safe" {
				return false
			}
		}
		return true
	}, 5*time.Minute)

	// The real helper completes the coordinated upgrade end to end. The
	// destructive stop is confirmed on an attached PTY, exactly like an
	// operator terminal.
	upgradePTY := startPTY(t, evidenceDir, "upgrade", env, helper, "compose", "upgrade", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "upgrade-report.json"))
	upgradePTY.waitFor(t, "Type UPGRADE", 60*time.Second)
	upgradePTY.write(t, "UPGRADE\n")
	if code := upgradePTY.wait(t, 15*time.Minute); code != 0 {
		t.Fatalf("compose upgrade exit=%d:\n%s", code, upgradePTY.output())
	}
	upgradeReport := readReportJSON(t, filepath.Join(workRoot, "upgrade-report.json"))
	if !reportHasCheck(upgradeReport, "upgrade-prepared") {
		t.Fatalf("upgrade report missing the quoin_upgrade_prepared observation: %s", mustJSONString(upgradeReport))
	}
	recorder.observe("upgrade-report-summary.json", upgradeReport)
	// Post-upgrade: the operational verifier passes on the restarted stack.
	recorder.run("verify-after-upgrade", env, nil, 0,
		helper, "compose", "verify", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "verify-after.json"))

	cleanupTicketResources(t, recorder, workRoot, registryRef, baseline, formal)
	cleaned = true
	recorder.observe("cleanup.json", map[string]any{
		"backend":        "compose",
		"ownedResources": []string{"Compose project/network/volumes/containers", "local registry", "release images", "temporary credentials"},
		"result":         "cleanupTicketResources removed every owned resource before this record was written",
	})
	writeTicket36RuntimeEvidence(t, recorder, "compose")
}

// t36SeedBrowserWork provisions the deterministic drainable browser work
// through the real public endpoints: an active label contract, a disabled
// business system, a Browser Identity revision and a started manual-login
// operation that stays pre-dispatch forever because no Lintel is registered.
func t36Get(t *testing.T, client *http.Client, url, origin string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	buffer := new(bytes.Buffer)
	_, _ = buffer.ReadFrom(response.Body)
	return response.StatusCode, buffer.String()
}

func t36Post(t *testing.T, client *http.Client, url, origin, payload string, wantStatus int) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	buffer := new(bytes.Buffer)
	_, _ = buffer.ReadFrom(response.Body)
	if response.StatusCode != wantStatus {
		t.Fatalf("POST %s status=%d want %d body=%s", url, response.StatusCode, wantStatus, buffer.String())
	}
	return buffer.String()
}

func t36Put(t *testing.T, client *http.Client, url, origin, payload string, wantStatus int) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, url, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	buffer := new(bytes.Buffer)
	_, _ = buffer.ReadFrom(response.Body)
	if response.StatusCode != wantStatus {
		t.Fatalf("PUT %s status=%d want %d body=%s", url, response.StatusCode, wantStatus, buffer.String())
	}
	return buffer.String()
}

type maintenanceStateJSON struct {
	Active     bool              `json:"active"`
	Reason     string            `json:"reason"`
	RowVersion int64             `json:"rowVersion"`
	Items      []maintenanceItem `json:"items"`
}

type maintenanceItem struct {
	Kind       string `json:"kind"`
	ObjectKey  string `json:"objectKey"`
	SafeState  string `json:"safeState"`
	DetailCode string `json:"detailCode"`
}

func waitForChecklist(t *testing.T, client *http.Client, base, origin string, done func(maintenanceStateJSON) bool, timeout ...time.Duration) maintenanceStateJSON {
	t.Helper()
	budget := 90 * time.Second
	if len(timeout) > 0 {
		budget = timeout[0]
	}
	deadline := time.Now().Add(budget)
	var last maintenanceStateJSON
	for time.Now().Before(deadline) {
		status, body := t36Get(t, client, base+"/api/v1/maintenance", origin)
		if status == http.StatusOK && json.Unmarshal([]byte(body), &last) == nil && done(last) {
			return last
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("checklist never reached the awaited state: %+v", last)
	return last
}

// drainRoute maps the checklist's drain directive onto its frozen cancel
// route — the same vocabulary the maintenance UI consumes.
func drainRoute(endpoint, pathParams string) (string, bool) {
	params := strings.Split(pathParams, "/")
	switch endpoint {
	case "analysis":
		return fmt.Sprintf("/api/v1/alerts/%s/analyses/%s/cancel", params[0], params[1]), true
	case "investigation":
		return fmt.Sprintf("/api/v1/investigations/%s/attempts/%s/cancel", params[0], params[1]), true
	case "inspection_run":
		return fmt.Sprintf("/api/v1/inspections/runs/%s/cancel", params[0]), true
	case "knowledge_batch":
		return fmt.Sprintf("/api/v1/knowledge/import-batches/%s/cancel", params[0]), true
	case "connection_probe":
		return fmt.Sprintf("/api/v1/connections/%s/probe-attempts/%s/cancel", params[0], params[1]), true
	case "config_verification":
		return fmt.Sprintf("/api/v1/business-systems/%s/config/%s/verifications/%s/cancel", params[0], params[1], params[2]), true
	case "browser_operation":
		return fmt.Sprintf("/api/v1/browser-login/%s/operations/%s/cancel", params[0], params[1]), true
	default:
		return "", false
	}
}

func t36SeedBrowserWork(t *testing.T, client *http.Client, base, origin string) string {
	t.Helper()
	suffix := time.Now().UnixNano()
	contractYAML := "label_contract:\n  business_system_label: business_system\n"
	if status := t36Upload(t, client, base+"/api/v1/label-contracts", origin, "contract.yaml", contractYAML, fmt.Sprintf("t36-contract-%d", suffix), ""); status != http.StatusCreated {
		t.Fatalf("label contract upload status=%d", status)
	}
	systemKey := fmt.Sprintf("t36-system-%d", suffix)
	systemYAML := fmt.Sprintf("system_key: %s\ndisplay_name: T36 Drain\nenabled: false\ntimezone: UTC\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans: []\n", systemKey)
	if status := t36Upload(t, client, base+"/api/v1/business-systems", origin, "system.yaml", systemYAML, fmt.Sprintf("t36-system-%d", suffix), "targetLabelContractVersion=1"); status != http.StatusCreated {
		t.Fatalf("business system upload status=%d", status)
	}
	identity := t36Post(t, client, fmt.Sprintf("%s/api/v1/business-systems/%s/browser-identity", base, systemKey), origin, fmt.Sprintf(`{"clientCommandId":"t36-identity-%d","name":"T36 只读账号","startUrl":"http://fixture.invalid/login","authenticationProbe":{"journeyId":"authentication.url-prefix.v1","journeyVersion":1,"params":{"authenticatedUrlPrefix":"http://fixture.invalid/authenticated"}}}`, suffix), http.StatusAccepted)
	var identityView struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(identity), &identityView); err != nil || identityView.RowVersion < 1 {
		// The detail endpoint carries the authoritative row version.
		_, detail := t36Get(t, client, fmt.Sprintf("%s/api/v1/business-systems/%s/browser-identity", base, systemKey), origin)
		if err := json.Unmarshal([]byte(detail), &identityView); err != nil {
			t.Fatalf("identity detail: %v %s", err, detail)
		}
	}
	t36Post(t, client, fmt.Sprintf("%s/api/v1/browser-login/%s/operations", base, systemKey), origin, fmt.Sprintf(`{"clientCommandId":"t36-login-%d","expectedRowVersion":%d}`, suffix, identityView.RowVersion), http.StatusAccepted)
	return systemKey
}

// t36Upload posts one strict-YAML multipart document.
func t36Upload(t *testing.T, client *http.Client, url, origin, filename, body, commandID, extraField string) int {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if extraField != "" {
		key, value, _ := strings.Cut(extraField, "=")
		if fieldErr := writer.WriteField(key, value); fieldErr != nil {
			t.Fatal(fieldErr)
		}
	}
	if fieldErr := writer.WriteField("clientCommandId", commandID); fieldErr != nil {
		t.Fatal(fieldErr)
	}
	file, fileErr := writer.CreateFormFile("file", filename)
	if fileErr != nil {
		t.Fatal(fileErr)
	}
	if _, fileErr = file.Write([]byte(body)); fileErr != nil {
		t.Fatal(fileErr)
	}
	if closeErr := writer.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	request, requestErr := http.NewRequest(http.MethodPost, url, &buffer)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", origin)
	response, responseErr := client.Do(request)
	if responseErr != nil {
		t.Fatal(responseErr)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 400 {
		t.Logf("upload %s -> %d: %s", url, response.StatusCode, responseBody)
	}
	return response.StatusCode
}

// waitForComposeState polls the deployed Quoin's readiness mode through the
// container-local healthcheck.
func waitForComposeState(t *testing.T, recorder *evidence, env []string, composeFile, mode string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		output, err := exec.Command("sh", "-c", fmt.Sprintf("docker compose --project-name %s --file %s exec -T quoin /quoin-healthcheck --status 200 http://127.0.0.1:9090/readyz 2>/dev/null", mainProject, composeFile)).CombinedOutput()
		last = string(output)
		if err == nil && strings.Contains(last, `"mode":"`+mode+`"`) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("compose readiness never reached mode=%s: %s", mode, last)
}

func readReportJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func reportHasCheck(report map[string]any, id string) bool {
	checks, ok := report["checks"].([]any)
	if !ok {
		return false
	}
	for _, entry := range checks {
		if check, ok := entry.(map[string]any); ok && check["id"] == id && check["result"] == "passed" {
			return true
		}
	}
	return false
}

func mustJSONString(value any) string {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ""
	}
	return string(body)
}

func writeTicket36RuntimeEvidence(t *testing.T, recorder *evidence, backend string) {
	t.Helper()
	recorder.observe("runtime-evidence.json", map[string]any{
		"backend": backend,
		"assertions": map[string]string{
			"drain":              "a real queued connection probe attempt was drained through the frozen upgrade-drain cancel after prepareUpgrade",
			"prepared":           "quoin_upgrade_prepared flipped to 1 only after the checklist was fully Safe and the pre-upgrade backup verified",
			"preWriteRollback":   "image-only rollback before the migration commit restarted the old Release without any restore (mechanism evidence only)",
			"reprepare":          "a second prepareUpgrade after the abort froze a new revision and reached prepared again",
			"helperUpgrade":      "quoin-deploy compose upgrade observed the prepared gauge, stopped the stack, offline-verified, migrated and restarted in order",
			"unsupportedVersion": "proved by test/release/upgrade gates with the real binary and a synthetic non-release fixture",
			"noReadyDuringMigration": "proved by test/release/upgrade gates: serve fails the exclusive data lock before binding listeners",
		},
	})
}
