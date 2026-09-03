package helm

// TestTicket36Helm drives the real Helm coordinated-upgrade path on a real
// cluster: install, Admin-driven prepareUpgrade with a queued connection
// probe as drainable work, the drain through the frozen upgrade-drain
// cancel, the verified pre-upgrade backup, the pre-write image-only
// rollback mechanism (mechanism evidence), a re-prepare, and the actual
// `quoin-deploy helm upgrade` helper through offline verification, exclusive
// migration and the ordered restart.

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

func TestUpgradeHelmTicket36(t *testing.T) {
	root := os.Getenv("QUOIN_EVIDENCE_DIR")
	if root == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T36 acceptance evidence run disabled")
	}
	evidenceDir := filepath.Join(root, "helm")
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	requireTools(t)
	recorder := newEvidence(t, evidenceDir)
	workRoot := t.TempDir()
	baseline := captureEnvironmentBaseline(t, recorder)
	registryRef := startRegistry(t, recorder)
	password := randomPassword(t)
	cuffix := strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 36)
	t36Namespace, t36Release := "quoin-t36-"+cuffix, "t36-"+cuffix
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			t36Cleanup(t, recorder, workRoot, registryRef, baseline, t36Namespace, t36Release)
		}
	})

	images := buildAndPushReleaseImages(t, recorder, workRoot)
	chartDigest, chartSHA := pushChartOCI(t, recorder, workRoot)
	manifestPath := writeReleaseManifest(t, recorder, workRoot, images, chartDigest, chartSHA)
	helper := filepath.Join(workRoot, "quoin-deploy")
	recorder.run("build-helper", nil, nil, 0, "go", "build", "-trimpath", "-o", helper, "./cmd/quoin-deploy")
	release, namespace := t36Release, t36Namespace
	env := deployEnv(workRoot, release, namespace)
	config := writeInstallConfig(t, workRoot, "t36-install.yaml")
	answers := strings.NewReader(strings.Join([]string{"admin", "Ticket 36 Admin", password, password}, "\n") + "\n")
	recorder.run("install", env, answers, 0, helper, "helm", "install", "--config", config, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "install-report.json"))

	base := "http://127.0.0.1:19280"
	origin := "https://quoin.example.com"
	forwardNumber := 0
	restartForward := func() {
		forwardNumber++
		startHelmPTY(t, evidenceDir, fmt.Sprintf("t36-port-forward-%d", forwardNumber), nil, "kubectl", "--namespace", namespace, "port-forward", "service/"+release+"-quoin-public", "19280:8080")
		waitPortForward(t, base)
	}
	restartForward()
	client := t36Login(t, base, origin, "admin", password)
	formal := password + "-formal"
	t36Request(t, client, http.MethodPut, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, password, formal), http.StatusNoContent)
	client = t36Login(t, base, origin, "admin", formal)

	// Drainable browser work: a real manual-login operation that can never
	// dispatch (no Lintel is registered), so only the frozen cancel fence
	// can end it.
	systemKey := t36SeedBrowserWork(t, client, base, origin)

	prepared := t36Request(t, client, http.MethodPost, base+"/api/v1/maintenance/upgrade/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t36-prepare-1-%d","expectedRowVersion":1}`, time.Now().UnixNano()), http.StatusAccepted)
	recorder.note("prepare-1.json", prepared)
	state := t36WaitChecklist(t, client, base, origin, func(state t36State) bool { return state.Active && state.Reason == "Upgrade" })
	if len(state.Items) == 0 {
		t.Fatalf("prepare froze an empty checklist: %+v", state)
	}
	if status, _ := t36RequestStatus(t, client, http.MethodGet, base+"/api/v1/alerts", origin, ""); status != http.StatusServiceUnavailable {
		t.Fatalf("ordinary work during Upgrade maintenance status=%d", status)
	}

	// Drain through the frozen upgrade-drain cancel using the checklist's
	// own directive.
	drain := t36WaitChecklist(t, client, base, origin, func(state t36State) bool {
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
		route, ok := t36DrainRoute(endpoint, tail[:index])
		if !ok {
			t.Fatalf("unknown drain endpoint %q", endpoint)
		}
		var body string
		if endpoint == "browser_operation" {
			body = fmt.Sprintf(`{"clientCommandId":"t36-drain-%d","expectedOperationRowVersion":%s}`, time.Now().UnixNano(), tail[index+1:])
		} else {
			body = fmt.Sprintf(`{"clientCommandId":"t36-drain-%d","expectedRowVersion":%s}`, time.Now().UnixNano(), tail[index+1:])
		}
		t36Request(t, client, http.MethodPost, base+route, origin, body, http.StatusOK)
		cancelled = true
	}
	if !cancelled {
		t.Fatalf("no drainable item in checklist: %+v", drain.Items)
	}
	_ = systemKey

	final := t36WaitChecklist(t, client, base, origin, func(state t36State) bool {
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
	recorder.note("prepared-checklist.json", mustJSONIndent(final))

	// Pre-write rollback mechanism (mechanism evidence only): scale every
	// workload to zero, restart Quoin alone on the old Release, exit the
	// aborted upgrade through the real command, then restart Quoin into
	// normal mode — no restore involved.
	for _, component := range []string{"stele", "plinth", "lintel", "quoin"} {
		recorder.run("rollback-stop-"+component, nil, nil, 0, t36Kubectl(namespace, "scale", "deployment/"+release+"-"+component, "--replicas=0")...)
	}
	recorder.run("rollback-start-quoin", nil, nil, 0, t36Kubectl(namespace, "scale", "deployment/"+release+"-quoin", "--replicas=1")...)
	t36WaitReadiness(t, recorder, namespace, release, "maintenance")
	// The scale-to-zero killed the previous port-forward tunnel; the
	// recreated Quoin needs a fresh one for the abort command.
	restartForward()
	t36Request(t, client, http.MethodPost, base+"/api/v1/maintenance/exit", origin, fmt.Sprintf(`{"clientCommandId":"t36-abort-1-%d","expectedRowVersion":%d,"expectedReason":"Upgrade"}`, time.Now().UnixNano(), final.RowVersion), http.StatusOK)
	recorder.run("rollback-restart-quoin", nil, nil, 0, t36Kubectl(namespace, "delete", "pod", "--selector="+"app.kubernetes.io/component=quoin,app.kubernetes.io/instance="+release)...)
	t36WaitReadiness(t, recorder, namespace, release, "normal")
	restartForward()
	recorder.observe("pre-write-rollback.json", map[string]any{
		"classification": "mechanism-evidence-only",
		"note":           "image-only rollback before the migration commit; no restore involved and no N-1 migration implied",
	})

	// Re-prepare after the abort and wait for prepared again.
	recorder.note("prepare-2.json", t36Request(t, client, http.MethodPost, base+"/api/v1/maintenance/upgrade/prepare", origin, fmt.Sprintf(`{"clientCommandId":"t36-prepare-2-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), final.RowVersion+1), http.StatusAccepted))
	t36WaitChecklist(t, client, base, origin, func(state t36State) bool {
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

	// The destructive stop is confirmed on an attached PTY, exactly like an
	// operator terminal.
	upgradePTY := startHelmPTY(t, evidenceDir, "t36-upgrade", env, helper, "helm", "upgrade", "--config", config, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "upgrade-report.json"))
	upgradePTY.waitFor(t, "Type UPGRADE", 60*time.Second)
	upgradePTY.write(t, "UPGRADE\n")
	if code := upgradePTY.wait(t, 15*time.Minute); code != 0 {
		t.Fatalf("helm upgrade exit=%d:\n%s", code, upgradePTY.output())
	}
	upgradeReport := t36ReadJSON(t, filepath.Join(workRoot, "upgrade-report.json"))
	if !t36ReportHasCheck(upgradeReport, "upgrade-prepared") {
		t.Fatalf("upgrade report missing the quoin_upgrade_prepared observation: %s", mustJSONIndent(upgradeReport))
	}
	recorder.run("verify-after-upgrade", env, nil, 0, helper, "helm", "verify", "--config", config, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "verify-after.json"))

	t36Cleanup(t, recorder, workRoot, registryRef, baseline, namespace, release)
	cleaned = true
	recorder.observe("runtime-evidence.json", map[string]any{
		"backend": "helm",
		"assertions": map[string]string{
			"drain":                  "a real queued connection probe attempt was drained through the frozen upgrade-drain cancel after prepareUpgrade",
			"prepared":               "the helper observed quoin_upgrade_prepared=1 only after the checklist was fully Safe and the pre-upgrade backup verified",
			"preWriteRollback":       "image-only rollback before the migration commit restarted the old Release without any restore (mechanism evidence only)",
			"reprepare":              "a second prepareUpgrade after the abort froze a new revision and reached prepared again",
			"helperUpgrade":          "quoin-deploy helm upgrade observed the prepared gauge, stopped the workloads, offline-verified, rolled the release, migrated and restarted in order",
			"unsupportedVersion":     "proved by test/release/upgrade gates with the real binary and a synthetic non-release fixture",
			"noReadyDuringMigration": "proved by test/release/upgrade gates: serve fails the exclusive data lock before binding listeners",
		},
	})
}

type t36State struct {
	Active     bool             `json:"active"`
	Reason     string           `json:"reason"`
	RowVersion int64            `json:"rowVersion"`
	Items      []t36StateItem   `json:"items"`
}

type t36StateItem struct {
	Kind       string `json:"kind"`
	ObjectKey  string `json:"objectKey"`
	SafeState  string `json:"safeState"`
	DetailCode string `json:"detailCode"`
}

// t36Login logs in through the loopback port-forward. The server issues a
// Secure cookie for its HTTPS public origin, so the client replays it
// explicitly to the port-forward host instead of relying on the cookie jar.
// t36Cleanup removes this run's dynamically named release/namespace and
// the shared local registry fixture, then proves the cluster inventories
// returned exactly to the captured baseline.
func t36Cleanup(t *testing.T, recorder *evidence, workRoot, registryRef string, baseline environmentBaseline, namespace, release string) {
	t.Helper()
	dispositions := map[string]string{}
	_ = recorder.run("cleanup-ns-"+namespace, nil, nil, -1, "helm", "uninstall", release, "--namespace", namespace)
	recorder.run("cleanup-ns-delete-"+namespace, nil, nil, 0, "kubectl", "delete", "namespace", namespace, "--ignore-not-found=true", "--wait=true", "--timeout=120s")
	dispositions["namespace:"+namespace] = "helm release uninstalled; namespace with PVCs, Secret and bootstrap objects removed"
	recorder.run("cleanup-registry", nil, nil, 0, "docker", "rm", "-f", registryName)
	dispositions["fixture:"+registryName] = "registry container removed; pushed test digests removed with it"
	images := []string{}
	for _, component := range []string{"quoin", "stele", "plinth", "lintel"} {
		images = append(images, registryHost+"/"+registryRepository+"/"+component+":amd64", registryHost+"/"+registryRepository+"/"+component+":arm64")
	}
	recorder.run("cleanup-images", nil, nil, 0, append([]string{"docker", "rmi", "-f"}, images...)...)
	dispositions["images"] = "locally built test images force-removed"
	if _, err := execOrError("kubectl", "get", "namespace", namespace); err == nil {
		t.Fatalf("cleanup left namespace %s behind", namespace)
	}
	if current := strings.TrimSpace(recorder.output("kubectl", "get", "namespaces", "--no-headers", "-o", "custom-columns=NAME:.metadata.name")); current != strings.TrimSpace(baseline.Namespaces) {
		t.Fatalf("namespace inventory changed beyond ticket-owned namespaces:\nbefore:\n%s\nafter:\n%s", baseline.Namespaces, current)
	}
	if after := strings.TrimSpace(recorder.output("kubectl", "get", "pvc", "--all-namespaces", "--no-headers", "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")); after != strings.TrimSpace(baseline.PVCs) {
		t.Fatalf("cluster PVC inventory changed beyond ticket-owned PVCs:\nbefore:\n%s\nafter:\n%s", baseline.PVCs, after)
	}
	if releases := strings.TrimSpace(recorder.output("helm", "list", "--all-namespaces", "--output", "json")); releases != baseline.HelmReleases {
		t.Fatalf("Helm release inventory changed beyond ticket-owned releases:\nbefore:\n%s\nafter:\n%s", baseline.HelmReleases, releases)
	}
	if err := os.RemoveAll(workRoot); err != nil {
		t.Fatalf("remove work root: %v", err)
	}
	dispositions["state-and-reports"] = "temporary state roots, values, manifests and reports removed with the work root"
	recorder.observe("cleanup.json", map[string]any{
		"dispositions":         dispositions,
		"preExistingUntouched": true,
		"credentials":          "administrator passwords held only in process memory; never written to evidence",
	})
}

func t36Login(t *testing.T, base, origin, username, password string) *http.Client {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	request, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = strings.TrimPrefix(origin, "https://")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d body=%s", response.StatusCode, body)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "__Host-quoin-session" {
			return &http.Client{Timeout: 30 * time.Second, Transport: t36CookieTransport{base: http.DefaultTransport, host: request.URL.Host, cookie: cookie}}
		}
	}
	t.Fatal("login returned no session cookie")
	return nil
}

type t36CookieTransport struct {
	base   http.RoundTripper
	host   string
	cookie *http.Cookie
}

func (transport t36CookieTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host == transport.host {
		request = request.Clone(request.Context())
		request.AddCookie(transport.cookie)
	}
	return transport.base.RoundTrip(request)
}

func waitPortForward(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(base + "/api/v1/maintenance")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusUnauthorized {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("port-forward never became ready")
}

func t36Request(t *testing.T, client *http.Client, method, url, origin, payload string, wantStatus int) string {
	t.Helper()
	status, body := t36RequestStatus(t, client, method, url, origin, payload)
	if status != wantStatus {
		t.Fatalf("%s %s status=%d want %d body=%s", method, url, status, wantStatus, body)
	}
	return body
}

func t36RequestStatus(t *testing.T, client *http.Client, method, url, origin, payload string) (int, string) {
	t.Helper()
	// The loopback port-forward tunnel can drop for a few seconds while the
	// service's pod churns (the pre-write rollback deletes and recreates
	// Quoin); a transport-level failure is retried briefly rather than
	// treated as a server verdict.
	var status int
	var body string
	for attempt := 0; attempt < 8; attempt++ {
		status, body = t36RequestOnce(t, client, method, url, origin, payload)
		if status != 0 {
			return status, body
		}
		time.Sleep(2 * time.Second)
	}
	return status, body
}

func t36RequestOnce(t *testing.T, client *http.Client, method, url, origin, payload string) (int, string) {
	t.Helper()
	var reader io.Reader
	if payload == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = strings.NewReader(payload)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if payload != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	// The ClusterIP is reached through a loopback port-forward; presenting
	// the configured public Origin as the Host header is what a real
	// same-origin browser request carries.
	request.Host = strings.TrimPrefix(origin, "https://")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	buffer := new(bytes.Buffer)
	_, _ = buffer.ReadFrom(response.Body)
	return response.StatusCode, buffer.String()
}

func t36WaitChecklist(t *testing.T, client *http.Client, base, origin string, done func(t36State) bool, timeout ...time.Duration) t36State {
	t.Helper()
	budget := 90 * time.Second
	if len(timeout) > 0 {
		budget = timeout[0]
	}
	deadline := time.Now().Add(budget)
	var last t36State
	for time.Now().Before(deadline) {
		status, body := t36RequestStatus(t, client, http.MethodGet, base+"/api/v1/maintenance", origin, "")
		if status == http.StatusOK && json.Unmarshal([]byte(body), &last) == nil && done(last) {
			return last
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("checklist never reached the awaited state: %+v", last)
	return last
}

// t36SeedBrowserWork provisions the deterministic drainable browser work
// through the real public endpoints: an active label contract, a disabled
// business system, a Browser Identity revision and a started manual-login
// operation that stays pre-dispatch forever because no Lintel is registered.
func t36SeedBrowserWork(t *testing.T, client *http.Client, base, origin string) string {
	t.Helper()
	suffix := time.Now().UnixNano()
	if status := t36Upload(t, client, base+"/api/v1/label-contracts", origin, "contract.yaml", "label_contract:\n  business_system_label: business_system\n", fmt.Sprintf("t36-contract-%d", suffix), ""); status != http.StatusCreated {
		t.Fatalf("label contract upload status=%d", status)
	}
	systemKey := fmt.Sprintf("t36-system-%d", suffix)
	systemYAML := fmt.Sprintf("system_key: %s\ndisplay_name: T36 Drain\nenabled: false\ntimezone: UTC\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans: []\n", systemKey)
	if status := t36Upload(t, client, base+"/api/v1/business-systems", origin, "system.yaml", systemYAML, fmt.Sprintf("t36-system-%d", suffix), "targetLabelContractVersion=1"); status != http.StatusCreated {
		t.Fatalf("business system upload status=%d", status)
	}
	t36Request(t, client, http.MethodPost, fmt.Sprintf("%s/api/v1/business-systems/%s/browser-identity", base, systemKey), origin,
		fmt.Sprintf(`{"clientCommandId":"t36-identity-%d","name":"T36 只读账号","startUrl":"http://fixture.invalid/login","authenticationProbe":{"journeyId":"authentication.url-prefix.v1","journeyVersion":1,"params":{"authenticatedUrlPrefix":"http://fixture.invalid/authenticated"}}}`, suffix), http.StatusAccepted)
	_, detail := t36RequestStatus(t, client, http.MethodGet, fmt.Sprintf("%s/api/v1/business-systems/%s/browser-identity", base, systemKey), origin, "")
	var identityView struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(detail), &identityView); err != nil || identityView.RowVersion < 1 {
		t.Fatalf("identity detail: %v %s", err, detail)
	}
	t36Request(t, client, http.MethodPost, fmt.Sprintf("%s/api/v1/browser-login/%s/operations", base, systemKey), origin,
		fmt.Sprintf(`{"clientCommandId":"t36-login-%d","expectedRowVersion":%d}`, suffix, identityView.RowVersion), http.StatusAccepted)
	return systemKey
}

// t36Upload posts one strict-YAML multipart document.
func t36Upload(t *testing.T, client *http.Client, url, origin, filename, body, commandID, extraField string) int {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if extraField != "" {
		key, value, _ := strings.Cut(extraField, "=")
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.WriteField("clientCommandId", commandID); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, url, &buffer)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = strings.TrimPrefix(origin, "https://")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 400 {
		t.Logf("upload %s -> %d: %s", url, response.StatusCode, responseBody)
	}
	return response.StatusCode
}

func t36DrainRoute(endpoint, pathParams string) (string, bool) {
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

// t36WaitReadiness polls the deployed Quoin's readiness mode through the
// in-container healthcheck binary.
func t36WaitReadiness(t *testing.T, recorder *evidence, namespace, release, mode string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	var last string
	arguments := t36Kubectl(namespace, "exec", "deployment/"+release+"-quoin", "--", "/quoin-healthcheck", "--status", "200", "http://127.0.0.1:9090/readyz")
	for time.Now().Before(deadline) {
		command := exec.Command(arguments[0], arguments[1:]...)
		output, err := command.CombinedOutput()
		last = string(output)
		if err == nil && strings.Contains(last, `"mode":"`+mode+`"`) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("helm readiness never reached mode=%s: %s", mode, last)
}

func t36Kubectl(namespace string, arguments ...string) []string {
	return append([]string{"kubectl", "--namespace", namespace}, arguments...)
}

func t36ReadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func t36ReportHasCheck(report map[string]any, id string) bool {
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

func mustJSONIndent(value any) string {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return strconv.Quote(err.Error())
	}
	return string(body)
}
