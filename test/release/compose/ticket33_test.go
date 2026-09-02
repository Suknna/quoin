package release_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/test/support"
	"github.com/creack/pty"
)

// TestTicket33Compose executes the actual helper and Compose restoration path.
// It deliberately reuses T30's release-image, manifest, install and cleanup
// harness: T33 owns only the backup/restore protocol observations.
func TestRestoreComposeTicket33(t *testing.T) {
	// T33 must never reuse T30 or an interrupted T33 run's persistent project.
	suffix := strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 36)
	mainProject, retryProject, mismatchProj = "quoin-t33main-"+suffix, "quoin-t33retry-"+suffix, "quoin-t33mismatch-"+suffix
	registryName, registryRepository = "t33-registry-"+suffix, "t33-"+suffix
	evidenceRoot := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceRoot == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T33 acceptance evidence run disabled")
	}
	requireTools(t)
	evidenceDir := filepath.Join(evidenceRoot, "compose")
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recorder := newEvidence(t, evidenceDir)
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
	configPath := writeInstallConfig(t, workRoot, "t33-install.yaml", filepath.Join(workRoot, "secrets"), mainQuoinPort, mainStelePort)
	env := composeEnv(workRoot, mainProject)
	installReport := filepath.Join(workRoot, "install-report.json")
	recorder.run("install", env, strings.NewReader(strings.Join([]string{"admin", "Ticket 33 Admin", password, password}, "\n")+"\n"), 0,
		helper, "compose", "install", "--config", configPath, "--release-manifest", manifestPath, "--report", installReport)
	// Persist a real Web Session in the snapshot; restore must revoke it before
	// the restored API is ever exposed.
	baseBeforeRestore := "http://127.0.0.1:" + strconv.Itoa(mainQuoinPort)
	preRestoreClient := maintenanceClient(t, baseBeforeRestore, "https://quoin.example.com", "admin", password)
	formalPassword := password + "-formal"
	requestJSON(t, preRestoreClient, http.MethodPut, baseBeforeRestore+"/api/v1/auth/password", "https://quoin.example.com", map[string]string{"currentPassword": password, "newPassword": formalPassword}, http.StatusNoContent)
	preRestoreClient = maintenanceClient(t, baseBeforeRestore, "https://quoin.example.com", "admin", formalPassword)
	requestJSON(t, preRestoreClient, http.MethodPost, baseBeforeRestore+"/api/v1/admin/users", "https://quoin.example.com", map[string]any{"clientCommandId": "t33-create-extra-user", "username": "t33-operator", "displayName": "T33 Operator", "role": "operator", "password": "t33-extra-user-password-2026"}, http.StatusCreated)
	oldOperatorClient := maintenanceClient(t, baseBeforeRestore, "https://quoin.example.com", "t33-operator", "t33-extra-user-password-2026")
	preRestoreAlertBearer := support.CreateAlertSource(t, preRestoreClient, baseBeforeRestore, "https://quoin.example.com", "t33-alertmanager", "t33-alert-source")
	if preRestoreAlertBearer == "" {
		t.Fatal("fixture did not create a live alert bearer")
	}
	// Force the helper's offline backup branch. It must itself prove the stopped
	// Quoin state, stop the whole release, create the snapshot and restart it.
	composeFile := filepath.Join(workRoot, mainProject, "state", "quoin", "compose", "generated", "compose.yaml")
	fixtureBinary := filepath.Join(workRoot, "fixture-provider")
	recorder.run("build-provider-fixture", nil, nil, 0, "go", "build", "-trimpath", "-o", fixtureBinary, "./test/fixtures/model-provider")
	fixture := startModelProvider(t, fixtureBinary)
	defer fixture.Stop()
	providerURL := "http://" + composeNetworkGateway(t, mainProject) + ":18443"
	support.PrepareProviderAndRuntime(t, preRestoreClient, baseBeforeRestore, "https://quoin.example.com", composeFile, mainProject, providerURL, "t33")
	recorder.run("backup-stop-quoin", env, nil, 0, "docker", "compose", "--project-name", mainProject, "--file", composeFile, "stop", "quoin")
	backupReport := filepath.Join(workRoot, "backup-report.json")
	recorder.run("backup-offline", env, nil, 0, helper, "compose", "backup", "--offline", "--config", configPath, "--release-manifest", manifestPath, "--report", backupReport)
	backupDirectory := filepath.Join(workRoot, mainProject, "state", "quoin", "compose", "backups")
	backupID := newestBackupID(t, backupDirectory)
	// Exercise the real helper's archive gates against filesystem copies. These
	// are test-owned backup artifacts, not product-table fixtures.
	corruptID, foreignID := "999999998", "999999997"
	copyDirectory(t, filepath.Join(backupDirectory, backupID), filepath.Join(backupDirectory, corruptID))
	copyDirectory(t, filepath.Join(backupDirectory, backupID), filepath.Join(backupDirectory, foreignID))
	corruptDB := filepath.Join(backupDirectory, corruptID, "quoin.db")
	corruptBody, err := os.ReadFile(corruptDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptDB, append(corruptBody, byte(0)), 0o600); err != nil {
		t.Fatal(err)
	}
	foreignManifest := filepath.Join(backupDirectory, foreignID, "manifest.json")
	manifestBody, err := os.ReadFile(foreignManifest)
	if err != nil {
		t.Fatal(err)
	}
	var foreign map[string]any
	if err := json.Unmarshal(manifestBody, &foreign); err != nil {
		t.Fatal(err)
	}
	foreign["release"] = "foreign-release-for-t33"
	manifestBody, err = json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignManifest, append(manifestBody, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct{ name, id string }{{"restore-corrupt", corruptID}, {"restore-foreign", foreignID}} {
		reportPath := filepath.Join(workRoot, invalid.name+"-report.json")
		before := snapshotComposeWorkloads(t, recorder, env, composeFile, mainProject)
		attempt := startPTY(t, evidenceDir, invalid.name, env, helper, "compose", "restore", "--backup", invalid.id, "--config", configPath, "--release-manifest", manifestPath, "--report", reportPath)
		if code := attempt.wait(t, 90*time.Second); code != 2 {
			t.Fatalf("%s exit=%d want preflight exit 2", invalid.name, code)
		}
		if strings.Contains(attempt.output(), "Type RESTORE") {
			t.Fatalf("%s reached destructive confirmation:\n%s", invalid.name, attempt.output())
		}
		assertComposeWorkloadsUnchanged(t, recorder, env, composeFile, mainProject, before)
		assertRestorePreflightReport(t, reportPath)
	}
	wrongSecrets := filepath.Join(workRoot, "wrong-secrets")
	copyDirectory(t, filepath.Join(workRoot, "secrets"), wrongSecrets)
	wrongRoot := []byte("wrong-root-key-for-ticket-33-0001")
	if err := os.WriteFile(filepath.Join(wrongSecrets, "root-key"), wrongRoot, 0o600); err != nil {
		t.Fatal(err)
	}
	wrongConfig := writeInstallConfig(t, workRoot, "t33-wrong-root.yaml", wrongSecrets, mainQuoinPort, mainStelePort)
	rootMismatch := startPTY(t, evidenceDir, "restore-root-key-mismatch", env, helper, "compose", "restore", "--backup", backupID, "--config", wrongConfig, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "restore-root-key-mismatch-report.json"))
	rootMismatch.waitFor(t, "Type RESTORE", 45*time.Second)
	rootMismatch.write(t, "RESTORE\n")
	if code := rootMismatch.wait(t, 120*time.Second); code == 0 {
		t.Fatal("root-key mismatch unexpectedly restored")
	}
	recorder.run("restart-after-root-key-mismatch", env, nil, 0, "docker", "compose", "--project-name", mainProject, "--file", composeFile, "start")
	assertComposeWorkloadsRunning(t, recorder, env, composeFile, mainProject)
	recorder.run("verify-after-root-key-mismatch", env, nil, 0, helper, "compose", "verify", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "root-key-mismatch-verify-report.json"))

	// An absent archive must fail through the real attached-TTY helper before it
	// can publish or start any workload. The valid restore follows below.
	missingBefore := snapshotComposeWorkloads(t, recorder, env, composeFile, mainProject)
	missing := startPTY(t, evidenceDir, "restore-missing", env, helper, "compose", "restore", "--backup", "999999999", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "restore-missing-report.json"))
	// The helper must reject a missing archive during its read-only preflight,
	// before it displays the destructive confirmation or stops any workload.
	if code := missing.wait(t, 90*time.Second); code != 2 {
		t.Fatalf("missing backup exit=%d want preflight exit 2", code)
	}
	if strings.Contains(missing.output(), "Type RESTORE") {
		t.Fatalf("missing backup reached destructive confirmation:\n%s", missing.output())
	}
	assertComposeWorkloadsUnchanged(t, recorder, env, composeFile, mainProject, missingBefore)
	assertRestorePreflightReport(t, filepath.Join(workRoot, "restore-missing-report.json"))

	temporary := randomPassword(t)
	restore := startPTY(t, evidenceDir, "restore", env, helper, "compose", "restore", "--backup", backupID, "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "restore-report.json"))
	restore.waitFor(t, "Type RESTORE", 45*time.Second)
	restore.write(t, "RESTORE\n")
	restore.waitFor(t, "Recovery administrator username", 45*time.Second)
	restore.write(t, "admin\n")
	restore.waitFor(t, "Temporary password", 45*time.Second)
	restore.write(t, temporary+"\n")
	restore.waitFor(t, "Confirm temporary password", 45*time.Second)
	restore.write(t, temporary+"\n")
	restore.waitFor(t, "Complete the Restore checklist", 120*time.Second)
	// Simulate helper interruption after the isolation transaction has been
	// published. The retry must verify the durable fence and resume without
	// another offline restore or another temporary-password prompt.
	_ = restore.cmd.Process.Kill()
	if code := restore.wait(t, 30*time.Second); code == 0 {
		t.Fatal("interrupted restore unexpectedly exited successfully")
	}
	resumed := startPTY(t, evidenceDir, "restore-resume", env, helper, "compose", "restore", "--backup", backupID, "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "restore-resume-report.json"))
	resumed.waitFor(t, "Type RESTORE", 45*time.Second)
	resumed.write(t, "RESTORE\n")
	resumed.waitFor(t, "Complete the Restore checklist", 120*time.Second)
	if strings.Contains(resumed.output(), "Recovery administrator username") {
		t.Fatalf("restore continuation re-ran offline recovery:\n%s", resumed.output())
	}
	running := recorder.run("maintenance-workloads-stopped", env, nil, 0, "docker", "compose", "--project-name", mainProject, "--file", composeFile, "ps", "--status", "running", "--services")
	for _, component := range []string{"plinth", "lintel", "stele"} {
		if strings.Contains(running, component) {
			t.Fatalf("%s is running during Quoin-only maintenance: %q", component, running)
		}
	}

	base := "http://127.0.0.1:" + strconv.Itoa(mainQuoinPort)
	oldSession, err := http.NewRequest(http.MethodGet, base+"/api/v1/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldSession.Header.Set("Origin", "https://quoin.example.com")
	oldResponse, err := preRestoreClient.Do(oldSession)
	if err != nil {
		t.Fatal(err)
	}
	_ = oldResponse.Body.Close()
	if oldResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pre-restore Session status=%d, want revoked 401", oldResponse.StatusCode)
	}
	oldOperatorSession, err := http.NewRequest(http.MethodGet, base+"/api/v1/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldOperatorSession.Header.Set("Origin", "https://quoin.example.com")
	oldOperatorResponse, err := oldOperatorClient.Do(oldOperatorSession)
	if err != nil {
		t.Fatal(err)
	}
	_ = oldOperatorResponse.Body.Close()
	if oldOperatorResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pre-restore operator Session status=%d, want revoked 401", oldOperatorResponse.StatusCode)
	}
	client := maintenanceClient(t, base, "https://quoin.example.com", "admin", temporary)
	state := maintenanceState(t, client, base, "https://quoin.example.com")
	if !state.Active || state.Reason != "Restore" {
		t.Fatalf("maintenance state=%+v, want active Restore", state)
	}
	requestJSON(t, client, http.MethodPut, base+"/api/v1/auth/password", "https://quoin.example.com", map[string]string{"currentPassword": temporary, "newPassword": password}, http.StatusNoContent)
	assertRuntimeRevoked(t, client, base, "https://quoin.example.com", "plinth")
	assertConnectionRevalidation(t, client, base, "https://quoin.example.com", "t33-provider")
	assertUserDisabledPublic(t, client, base, "https://quoin.example.com", "t33-operator")
	requestJSON(t, client, http.MethodPost, base+"/api/v1/maintenance/exit", "https://quoin.example.com", map[string]any{"clientCommandId": "t33-compose-exit", "expectedReason": "Restore", "expectedRowVersion": state.RowVersion}, http.StatusOK)
	resumed.write(t, "CONTINUE\n")
	if code := resumed.wait(t, 180*time.Second); code != 0 {
		t.Fatalf("resumed restore exit=%d\n%s", code, resumed.output())
	}
	oldAlert, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(mainStelePort)+"/", strings.NewReader(`{"alerts":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	oldAlert.Header.Set("Content-Type", "application/json")
	oldAlert.Header.Set("Authorization", "Bearer "+preRestoreAlertBearer)
	oldAlertResponse, err := http.DefaultClient.Do(oldAlert)
	if err != nil {
		t.Fatal(err)
	}
	_ = oldAlertResponse.Body.Close()
	if oldAlertResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("retired alert bearer status=%d, want 401 after normal restart", oldAlertResponse.StatusCode)
	}

	for _, entry := range []struct {
		name string
		body []byte
	}{{"restore-missing.log", missing.outputBytes()}, {"restore-interrupted.log", restore.outputBytes()}, {"restore-resumed.log", resumed.outputBytes()}} {
		if err := os.WriteFile(filepath.Join(evidenceDir, entry.name), entry.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	verifyReport := filepath.Join(workRoot, "verify-report.json")
	recorder.run("verify-post-restore", env, nil, 0, helper, "compose", "verify", "--config", configPath, "--release-manifest", manifestPath, "--report", verifyReport)
	recorder.observe("restore-observation.json", map[string]any{"backupID": backupID, "maintenance": "Restore", "postRestore": "normal helper verification passed", "missingBackup": "rejected during read-only preflight", "continuation": "published Restore fence resumed without a second offline restore"})
	cleanupTicketResources(t, recorder, workRoot, registryRef, baseline, password)
	scanEvidenceForSecrets(t, evidenceDir, password, formalPassword, temporary, preRestoreAlertBearer)
	cleaned = true
}

type modelProviderProcess struct{ cmd *exec.Cmd }

func startModelProvider(t *testing.T, binary string) *modelProviderProcess {
	t.Helper()
	cmd := exec.Command(binary, "-address", "0.0.0.0:18443")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://127.0.0.1:18443/v1/models")
		if err == nil {
			_ = response.Body.Close()
			return &modelProviderProcess{cmd: cmd}
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	t.Fatal("model provider fixture did not become ready")
	return nil
}

func (process *modelProviderProcess) Stop() {
	if process != nil && process.cmd != nil && process.cmd.Process != nil {
		_ = process.cmd.Process.Kill()
		_ = process.cmd.Wait()
	}
}

func composeNetworkGateway(t *testing.T, project string) string {
	t.Helper()
	containerOutput, err := exec.Command("docker", "compose", "--project-name", project, "ps", "-q", "quoin").CombinedOutput()
	if err != nil {
		t.Fatalf("locate Compose Quoin container: %v: %s", err, containerOutput)
	}
	containerID := strings.TrimSpace(string(containerOutput))
	if containerID == "" {
		t.Fatal("Compose Quoin container not found")
	}
	networksOutput, err := exec.Command("docker", "inspect", "--format", "{{range $name, $network := .NetworkSettings.Networks}}{{$name}} {{end}}", containerID).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect Compose Quoin networks: %v: %s", err, networksOutput)
	}
	for _, network := range strings.Fields(string(networksOutput)) {
		if !strings.HasSuffix(network, "_internal") {
			continue
		}
		gatewayOutput, err := exec.Command("docker", "network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", network).CombinedOutput()
		if err != nil {
			t.Fatalf("inspect Compose internal network: %v: %s", err, gatewayOutput)
		}
		gateway := strings.TrimSpace(string(gatewayOutput))
		if gateway != "" {
			return gateway
		}
	}
	t.Fatalf("Compose internal network not found: %q", networksOutput)
	return ""
}

func assertUserDisabledPublic(t *testing.T, client *http.Client, base, origin, username string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/admin/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list restored users status=%d body=%s", response.StatusCode, body)
	}
	var payload struct {
		Items []struct {
			Username string `json:"username"`
			Enabled  bool   `json:"enabled"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, item := range payload.Items {
		if item.Username == username {
			if item.Enabled {
				t.Fatalf("restored user %q remains enabled", username)
			}
			return
		}
	}
	t.Fatalf("restored user %q missing from public admin users projection", username)
}

type maintenanceView struct {
	Active     bool   `json:"active"`
	Reason     string `json:"reason"`
	RowVersion int64  `json:"rowVersion"`
}

// maintenanceClient uses the session cookie issued by the real login response.
// The formal public origin is HTTPS, while the isolated Compose test reaches
// its loopback HTTP publishing port; CookieJar correctly refuses to replay a
// Secure cookie over HTTP, so the test transport explicitly attaches the
// server-issued cookie only to that loopback host.
func maintenanceClient(t *testing.T, base, origin, username, password string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/login", bytes.NewReader(body))
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
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("maintenance login status=%d body=%s", response.StatusCode, responseBody)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "__Host-quoin-session" {
			client.Transport = sessionCookieTransport{base: http.DefaultTransport, host: request.URL.Host, cookie: cookie}
			return client
		}
	}
	t.Fatal("maintenance login returned no session cookie")
	return nil
}

type sessionCookieTransport struct {
	base   http.RoundTripper
	host   string
	cookie *http.Cookie
}

func (transport sessionCookieTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host == transport.host {
		request = request.Clone(request.Context())
		request.AddCookie(transport.cookie)
	}
	return transport.base.RoundTrip(request)
}

func maintenanceState(t *testing.T, client *http.Client, base, origin string) maintenanceView {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/maintenance", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("maintenance state status=%d body=%s", response.StatusCode, body)
	}
	var state maintenanceView
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func assertRuntimeRevoked(t *testing.T, client *http.Client, base, origin, slot string) {
	t.Helper()
	body := getJSON(t, client, base+"/api/v1/runtime", origin)
	var payload map[string]map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	view, ok := payload[slot]
	if !ok {
		t.Fatalf("runtime slot %q missing from %s", slot, body)
	}
	if view["state"] != "revoked" {
		t.Fatalf("runtime slot %q state=%v, want revoked", slot, view["state"])
	}
}

func assertConnectionRevalidation(t *testing.T, client *http.Client, base, origin, name string) {
	t.Helper()
	body := getJSON(t, client, base+"/api/v1/connections/"+name, origin)
	var view struct {
		Enabled              bool `json:"enabled"`
		RevalidationRequired bool `json:"revalidationRequired"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Enabled || !view.RevalidationRequired {
		t.Fatalf("connection %q isolation=%s, want enabled=false/revalidationRequired=true", name, body)
	}
}

func getJSON(t *testing.T, client *http.Client, url, origin string) []byte {
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
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", url, response.StatusCode, body)
	}
	return body
}

func requestJSON(t *testing.T, client *http.Client, method, url, origin string, value any, expected int) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
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
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode != expected {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, url, response.StatusCode, expected, responseBody)
	}
}

func snapshotComposeWorkloads(t *testing.T, recorder *evidence, env []string, composeFile, project string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		containerID := strings.TrimSpace(recorder.run("snapshot-"+component, env, nil, 0, "docker", "compose", "--project-name", project, "--file", composeFile, "ps", "--quiet", component))
		if containerID == "" {
			t.Fatalf("%s has no running container before rejected restore", component)
		}
		snapshot[component] = strings.TrimSpace(recorder.run("snapshot-"+component+"-inspect", env, nil, 0, "docker", "inspect", "--format", "{{.Id}} {{.State.StartedAt}} {{.RestartCount}}", containerID))
	}
	return snapshot
}

func assertComposeWorkloadsUnchanged(t *testing.T, recorder *evidence, env []string, composeFile, project string, before map[string]string) {
	t.Helper()
	assertComposeWorkloadsRunning(t, recorder, env, composeFile, project)
	after := snapshotComposeWorkloads(t, recorder, env, composeFile, project)
	for component, value := range before {
		if after[component] != value {
			t.Fatalf("%s changed during rejected restore: before=%q after=%q", component, value, after[component])
		}
	}
}

func assertComposeWorkloadsRunning(t *testing.T, recorder *evidence, env []string, composeFile, project string) {
	t.Helper()
	running := recorder.run("assert-workloads-running", env, nil, 0, "docker", "compose", "--project-name", project, "--file", composeFile, "ps", "--status", "running", "--services")
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		if !strings.Contains(running, component) {
			t.Fatalf("%s is not running after rejected restore: %q", component, running)
		}
	}
}

func assertRestorePreflightReport(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		ExitCode int `json:"exitCode"`
		Failure  struct {
			Code string `json:"code"`
		} `json:"failure"`
		Stages []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"stages"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if report.ExitCode != 2 || report.Failure.Code != "restore_backup_invalid" || len(report.Stages) != 1 || report.Stages[0].Name != "restore-preflight" || report.Stages[0].Status != "failed" {
		t.Fatalf("restore preflight report=%s", body)
	}
}

func newestBackupID(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	latest := ""
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() > latest {
			latest = entry.Name()
		}
	}
	if latest == "" {
		t.Fatal("offline backup created no archive")
	}
	return latest
}

type ptyCommand struct {
	cmd  *exec.Cmd
	file *os.File
	log  bytes.Buffer
	mu   sync.Mutex
}

func startPTY(t *testing.T, evidenceDir, name string, env []string, argv ...string) *ptyCommand {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = repoRoot(t)
	cmd.Env = env
	file, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	value := &ptyCommand{cmd: cmd, file: file}
	go func() {
		buffer := make([]byte, 4096)
		for {
			count, readErr := file.Read(buffer)
			if count > 0 {
				value.mu.Lock()
				value.log.Write(buffer[:count])
				value.mu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = file.Close()
		_ = cmd.Process.Kill()
		_ = os.WriteFile(filepath.Join(evidenceDir, name+".log"), value.outputBytes(), 0o600)
	})
	return value
}

func (command *ptyCommand) write(t *testing.T, input string) {
	t.Helper()
	if _, err := command.file.WriteString(input); err != nil {
		t.Fatal(err)
	}
}
func (command *ptyCommand) output() string {
	command.mu.Lock()
	defer command.mu.Unlock()
	return command.log.String()
}
func (command *ptyCommand) outputBytes() []byte { return []byte(command.output()) }
func (command *ptyCommand) waitFor(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(command.output(), want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("PTY did not print %q:\n%s", want, command.output())
}
func (command *ptyCommand) wait(t *testing.T, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- command.cmd.Wait() }()
	select {
	case <-time.After(timeout):
		_ = command.cmd.Process.Kill()
		t.Fatalf("PTY command timed out:\n%s", command.output())
		return -1
	case <-done:
		return command.cmd.ProcessState.ExitCode()
	}
}

func copyDirectory(t *testing.T, source, destination string) {
	t.Helper()
	if err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, info.Mode().Perm())
	}); err != nil {
		t.Fatal(err)
	}
}
