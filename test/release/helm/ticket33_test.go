package helm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// TestRestoreHelmTicket33 reuses T31's real registry, OCI chart, installation
// and cleanup machinery, then owns the T33 backup/PTY-restore/UI protocol.
func TestRestoreHelmTicket33(t *testing.T) {
	// T33 must not reuse T31 or an interrupted T33 run's retained release/PVC
	// state: helm install treats existing state as authoritative.
	suffix := strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 36)
	registryName = "t33-registry-" + suffix
	// The repository is owned by this run too. Reusing T31's cached namespace
	// would overwrite and later delete images outside this test's lifecycle.
	registryRepository = "t33-" + suffix
	mainRelease, mainNs, retryRelease, retryNs = "t33-"+suffix, "quoin-t33-"+suffix, "t33r-"+suffix, "quoin-t33r-"+suffix
	evidenceRoot := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceRoot == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T33 acceptance evidence run disabled")
	}
	requireTools(t)
	evidenceDir := filepath.Join(evidenceRoot, "helm")
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
	chartDigest, chartSHA := pushChartOCI(t, recorder, workRoot)
	manifestPath := writeReleaseManifest(t, recorder, workRoot, images, chartDigest, chartSHA)
	helper := filepath.Join(workRoot, "quoin-deploy")
	recorder.run("build-helper", nil, nil, 0, "go", "build", "-trimpath", "-o", helper, "./cmd/quoin-deploy")
	configPath := writeInstallConfig(t, workRoot, "t33-install.yaml")
	env := deployEnv(workRoot, mainRelease, mainNs)
	recorder.run("install", env, strings.NewReader(strings.Join([]string{"admin", "Ticket 33 Admin", password, password}, "\n")+"\n"), 0,
		helper, "helm", "install", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "install-report.json"))
	// Persist a real Web Session in the snapshot; restore must revoke it before
	// the restored API is ever exposed.
	initialForward := startHelmPTY(t, evidenceDir, "pre-backup-port-forward", nil, "kubectl", "--namespace", mainNs, "port-forward", "service/"+mainRelease+"-quoin-public", "19180:8080")
	initialForward.waitFor(t, "Forwarding", 45*time.Second)
	preRestoreClient := helmMaintenanceClient(t, "http://127.0.0.1:19180", "https://quoin.example.com", "admin", password)
	formalPassword := password + "-formal"
	helmRequestJSON(t, preRestoreClient, http.MethodPut, "http://127.0.0.1:19180/api/v1/auth/password", "https://quoin.example.com", map[string]string{"currentPassword": password, "newPassword": formalPassword}, http.StatusNoContent)
	preRestoreClient = helmMaintenanceClient(t, "http://127.0.0.1:19180", "https://quoin.example.com", "admin", formalPassword)
	helmRequestJSON(t, preRestoreClient, http.MethodPost, "http://127.0.0.1:19180/api/v1/admin/users", "https://quoin.example.com", map[string]any{"clientCommandId": "t33-create-extra-user", "username": "t33-operator", "displayName": "T33 Operator", "role": "operator", "password": "t33-extra-user-password-2026"}, http.StatusCreated)
	fixtureBinary := filepath.Join(workRoot, "fixture-provider")
	recorder.run("build-provider-fixture", nil, nil, 0, "go", "build", "-trimpath", "-o", fixtureBinary, "./test/fixtures/model-provider")
	fixture := startHelmModelProvider(t, fixtureBinary, evidenceDir)
	defer fixture.Stop()
	nodeIP := strings.TrimSpace(recorder.output("kubectl", "get", "nodes", "-o", "jsonpath={.items[0].status.addresses[?(@.type==\"InternalIP\")].address}"))
	prepareHelmProviderAndRuntime(t, recorder, preRestoreClient, "http://127.0.0.1:19180", "https://quoin.example.com", mainNs, mainRelease, "http://"+nodeIP+":18443")
	preRestoreAlertBearer := support.CreateAlertSourceWithHost(t, preRestoreClient, "http://127.0.0.1:19180", "https://quoin.example.com", "quoin.example.com", "t33-alertmanager", "t33-alert-source")
	steleBefore := startHelmPTY(t, evidenceDir, "pre-backup-stele-port-forward", nil, "kubectl", "--namespace", mainNs, "port-forward", "service/"+mainRelease+"-stele-webhook", "19181:8080")
	steleBefore.waitFor(t, "Forwarding", 45*time.Second)
	sendSteleWebhook(t, "http://127.0.0.1:19181/", preRestoreAlertBearer, http.StatusNoContent)
	_ = steleBefore.cmd.Process.Kill()
	_ = initialForward.cmd.Process.Kill()

	// The helper's own offline path proves the all-workload stop fence, runs the
	// same-Release backup Pod, and restarts the normal release.
	recorder.run("backup-stop-quoin", env, nil, 0, "kubectl", "--namespace", mainNs, "scale", "deployment/"+mainRelease+"-quoin", "--replicas=0")
	recorder.run("backup-wait-quoin-stopped", env, nil, 0, "kubectl", "--namespace", mainNs, "wait", "--for=delete", "pod", "--selector=app.kubernetes.io/name=quoin,app.kubernetes.io/instance="+mainRelease+",app.kubernetes.io/component=quoin", "--timeout=90s")
	backupReport := filepath.Join(workRoot, "backup-report.json")
	recorder.run("backup-offline", env, nil, -1, helper, "helm", "backup", "--offline", "--config", configPath, "--release-manifest", manifestPath, "--report", backupReport)
	if recorder.commands[len(recorder.commands)-1].ExitCode != 0 {
		reportBody, readErr := os.ReadFile(backupReport)
		if readErr != nil {
			t.Fatalf("offline backup failed and report is unavailable: %v", readErr)
		}
		recorder.note("backup-report-failure.json", string(reportBody))
		t.Fatalf("offline backup failed: %s", reportBody)
	}
	postBackupForward := startHelmPTY(t, evidenceDir, "post-backup-port-forward", nil, "kubectl", "--namespace", mainNs, "port-forward", "service/"+mainRelease+"-quoin-public", "19180:8080")
	postBackupForward.waitFor(t, "Forwarding", 45*time.Second)
	backupID := helmNewestBackupID(t, helmMaintenanceClient(t, "http://127.0.0.1:19180", "https://quoin.example.com", "admin", formalPassword))
	_ = postBackupForward.cmd.Process.Kill()
	// Corrupt and foreign copies are created by a real temporary Pod mounted on
	// the retained backup PVC. Quoin is scaled down first so the RWO claim is
	// exclusively owned by this test mutator; restore preflight must reject both
	// copies before its own destructive stop fence.
	recorder.run("backup-mutator-scale-down", env, nil, 0, "kubectl", "--namespace", mainNs, "scale", "deployment/"+mainRelease+"-quoin", "--replicas=0")
	recorder.run("backup-mutator-wait-quoin", env, nil, 0, "kubectl", "--namespace", mainNs, "wait", "--for=delete", "pod", "--selector=app.kubernetes.io/name=quoin,app.kubernetes.io/instance="+mainRelease+",app.kubernetes.io/component=quoin", "--timeout=90s")
	mutatorScript := "cp -a /backups/" + backupID + " /backups/999999998; cp /dev/null /backups/999999998/quoin.db; cp -a /backups/" + backupID + " /backups/999999997; sed -i 's/\\\"release\\\"[[:space:]]*:[[:space:]]*\\\"[^\\\"]*\\\"/\\\"release\\\":\\\"foreign-release-for-t33\\\"/' /backups/999999997/manifest.json"
	overrides, err := json.Marshal(map[string]any{"apiVersion": "v1", "spec": map[string]any{"restartPolicy": "Never", "containers": []any{map[string]any{"name": "mutator", "image": "busybox:1.36", "command": []string{"sh", "-c", mutatorScript}, "volumeMounts": []any{map[string]any{"name": "backups", "mountPath": "/backups"}}}}, "volumes": []any{map[string]any{"name": "backups", "persistentVolumeClaim": map[string]string{"claimName": mainRelease + "-quoin-backups"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	recorder.run("make-invalid-backups", env, nil, 0, "kubectl", "--namespace", mainNs, "run", "t33-backup-mutator", "--image=busybox:1.36", "--restart=Never", "--overrides", string(overrides))
	recorder.run("wait-invalid-backups", env, nil, 0, "kubectl", "--namespace", mainNs, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/t33-backup-mutator", "--timeout=120s")
	recorder.run("delete-backup-mutator", env, nil, 0, "kubectl", "--namespace", mainNs, "delete", "pod/t33-backup-mutator", "--wait=true")
	recorder.run("backup-mutator-scale-up", env, nil, 0, "kubectl", "--namespace", mainNs, "scale", "deployment/"+mainRelease+"-quoin", "--replicas=1")
	recorder.run("backup-mutator-wait-quoin-ready", env, nil, 0, "kubectl", "--namespace", mainNs, "rollout", "status", "deployment/"+mainRelease+"-quoin", "--timeout=120s")
	for _, invalid := range []string{"999999998", "999999997"} {
		reportPath := filepath.Join(workRoot, "restore-invalid-"+invalid+"-report.json")
		before := snapshotHelmWorkloads(t, recorder, env)
		attempt := startHelmPTY(t, evidenceDir, "restore-invalid-"+invalid, env, helper, "helm", "restore", "--backup", invalid, "--config", configPath, "--release-manifest", manifestPath, "--report", reportPath)
		if code := attempt.wait(t, 120*time.Second); code != 2 {
			t.Fatalf("invalid backup %s exit=%d want preflight exit 2", invalid, code)
		}
		if strings.Contains(attempt.output(), "Type RESTORE") {
			t.Fatalf("invalid backup %s reached destructive confirmation:\n%s", invalid, attempt.output())
		}
		assertHelmWorkloadsUnchanged(t, recorder, env, before)
		assertHelmRestorePreflightReport(t, reportPath)
	}
	secretJSON := recorder.output("kubectl", "--namespace", mainNs, "get", "secret/"+mainRelease+"-secrets", "-o", "json")
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(secretJSON), &secret); err != nil {
		t.Fatal(err)
	}
	originalRoot, ok := secret.Data["root-key"]
	if !ok || originalRoot == "" {
		t.Fatal("helm root-key secret is missing")
	}
	wrongRoot := base64.StdEncoding.EncodeToString([]byte("wrong-root-key-for-ticket-33"))
	recorder.run("tamper-root-key", env, nil, 0, "kubectl", "--namespace", mainNs, "patch", "secret/"+mainRelease+"-secrets", "--type=json", "--patch", "[{\"op\":\"replace\",\"path\":\"/data/root-key\",\"value\":\""+wrongRoot+"\"}]")
	rootMismatch := startHelmPTY(t, evidenceDir, "restore-root-key-mismatch", env, helper, "helm", "restore", "--backup", backupID, "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "restore-root-key-mismatch-report.json"))
	rootMismatch.waitFor(t, "Type RESTORE", 45*time.Second)
	rootMismatch.write(t, "RESTORE\n")
	if code := rootMismatch.wait(t, 180*time.Second); code == 0 {
		t.Fatal("root-key mismatch unexpectedly restored")
	}
	restorePatch := filepath.Join(workRoot, "restore-root-key-patch.json")
	if err := os.WriteFile(restorePatch, []byte("[{\"op\":\"replace\",\"path\":\"/data/root-key\",\"value\":\""+originalRoot+"\"}]"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder.run("restore-root-key-secret", env, nil, 0, "kubectl", "--namespace", mainNs, "patch", "secret/"+mainRelease+"-secrets", "--type=json", "--patch-file", restorePatch)
	_ = os.Remove(restorePatch)
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		recorder.run("restart-after-root-key-mismatch-"+component, env, nil, 0, "kubectl", "--namespace", mainNs, "scale", "deployment/"+mainRelease+"-"+component, "--replicas=1")
	}
	assertHelmWorkloadsRunning(t, recorder, env)
	recorder.run("verify-after-root-key-mismatch", env, nil, 0, helper, "helm", "verify", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "root-key-mismatch-verify-report.json"))

	missingBefore := snapshotHelmWorkloads(t, recorder, env)
	missing := startHelmPTY(t, evidenceDir, "restore-missing", env, helper, "helm", "restore", "--backup", "999999999", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "restore-missing-report.json"))
	// The read-only preflight rejects an absent archive before confirmation,
	// scaling, or attached-TTY secret input.
	if code := missing.wait(t, 120*time.Second); code != 2 {
		t.Fatalf("missing backup exit=%d want preflight exit 2", code)
	}
	if strings.Contains(missing.output(), "Type RESTORE") {
		t.Fatalf("missing backup reached destructive confirmation:\n%s", missing.output())
	}
	assertHelmWorkloadsUnchanged(t, recorder, env, missingBefore)
	assertHelmRestorePreflightReport(t, filepath.Join(workRoot, "restore-missing-report.json"))

	temporary := randomPassword(t)
	restore := startHelmPTY(t, evidenceDir, "restore", env, helper, "helm", "restore", "--backup", backupID, "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "restore-report.json"))
	restore.waitFor(t, "Type RESTORE", 45*time.Second)
	restore.write(t, "RESTORE\n")
	restore.waitFor(t, "If you don't see a command prompt", 45*time.Second)
	restore.write(t, "admin\n")
	restore.waitFor(t, "Temporary password", 45*time.Second)
	restore.write(t, temporary+"\n")
	restore.waitFor(t, "Confirm temporary password", 45*time.Second)
	restore.write(t, temporary+"\n")
	restore.waitFor(t, "Complete the Restore checklist", 120*time.Second)
	// Interrupt after publication, then retry through the durable active-Restore
	// plus backup-bound rollback fence. No second TTY recovery credential is
	// permitted on the resumed path.
	_ = restore.cmd.Process.Kill()
	if code := restore.wait(t, 30*time.Second); code == 0 {
		t.Fatal("interrupted restore unexpectedly exited successfully")
	}
	resumed := startHelmPTY(t, evidenceDir, "restore-resume", env, helper, "helm", "restore", "--backup", backupID, "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "restore-resume-report.json"))
	resumed.waitFor(t, "Type RESTORE", 45*time.Second)
	resumed.write(t, "RESTORE\n")
	resumed.waitFor(t, "Complete the Restore checklist", 120*time.Second)
	if strings.Contains(resumed.output(), "Recovery administrator username") {
		t.Fatalf("restore continuation re-ran offline recovery:\n%s", resumed.output())
	}
	for _, component := range []string{"plinth", "lintel", "stele"} {
		replicas := strings.TrimSpace(recorder.run("maintenance-"+component+"-stopped", env, nil, 0, "kubectl", "--namespace", mainNs, "get", "deployment/"+mainRelease+"-"+component, "--output=jsonpath={.spec.replicas}"))
		if replicas != "0" {
			t.Fatalf("%s replicas during Quoin-only maintenance=%q, want 0", component, replicas)
		}
	}

	forward := startHelmPTY(t, evidenceDir, "public-port-forward", nil, "kubectl", "--namespace", mainNs, "port-forward", "service/"+mainRelease+"-quoin-public", "19180:8080")
	forward.waitFor(t, "Forwarding", 45*time.Second)
	base, origin := "http://127.0.0.1:19180", "https://quoin.example.com"
	oldSession, err := http.NewRequest(http.MethodGet, base+"/api/v1/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldSession.Header.Set("Origin", origin)
	oldResponse, err := preRestoreClient.Do(oldSession)
	if err != nil {
		t.Fatal(err)
	}
	_ = oldResponse.Body.Close()
	if oldResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pre-restore Session status=%d, want revoked 401", oldResponse.StatusCode)
	}
	client := helmMaintenanceClient(t, base, origin, "admin", temporary)
	state := helmMaintenanceState(t, client, base, origin)
	if !state.Active || state.Reason != "Restore" {
		t.Fatalf("maintenance state=%+v", state)
	}
	helmRequestJSON(t, client, http.MethodPut, base+"/api/v1/auth/password", origin, map[string]string{"currentPassword": temporary, "newPassword": password}, http.StatusNoContent)
	assertHelmRuntimeRevoked(t, client, base, origin, "plinth")
	assertHelmConnectionRevalidation(t, client, base, origin, "t33-provider")
	helmRequestJSON(t, client, http.MethodPost, base+"/api/v1/maintenance/exit", origin, map[string]any{"clientCommandId": "t33-helm-exit", "expectedReason": "Restore", "expectedRowVersion": state.RowVersion}, http.StatusOK)
	_ = forward.cmd.Process.Kill()
	resumed.write(t, "CONTINUE\n")
	if code := resumed.wait(t, 180*time.Second); code != 0 {
		t.Fatalf("resumed restore exit=%d\n%s", code, resumed.output())
	}
	steleForward := startHelmPTY(t, evidenceDir, "post-restore-stele-port-forward", nil, "kubectl", "--namespace", mainNs, "port-forward", "service/"+mainRelease+"-stele-webhook", "19181:8080")
	steleForward.waitFor(t, "Forwarding", 45*time.Second)
	oldAlert, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:19181/", strings.NewReader(`{"alerts":[]}`))
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
	_ = steleForward.cmd.Process.Kill()

	for _, entry := range []struct {
		name string
		body []byte
	}{{"restore-missing.log", missing.outputBytes()}, {"restore-interrupted.log", restore.outputBytes()}, {"restore-resumed.log", resumed.outputBytes()}} {
		if err := os.WriteFile(filepath.Join(evidenceDir, entry.name), entry.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recorder.run("verify-post-restore", env, nil, 0, helper, "helm", "verify", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "verify-report.json"))
	recorder.observe("restore-observation.json", map[string]any{"backupID": backupID, "maintenance": "Restore", "postRestore": "normal helper verification passed", "missingBackup": "rejected during read-only preflight", "continuation": "published Restore fence resumed without a second offline restore"})
	cleanupTicketResources(t, recorder, workRoot, registryRef, baseline, password)
	assertNoSecretInHelmEvidence(t, evidenceDir, password, formalPassword, temporary, preRestoreAlertBearer)
	cleaned = true
}

type helmMaintenanceView struct {
	Active     bool   `json:"active"`
	Reason     string `json:"reason"`
	RowVersion int64  `json:"rowVersion"`
}

// The real server issues a Secure cookie because its configured public origin
// is HTTPS. The test reaches the isolated ClusterIP through loopback HTTP, so
// explicitly replay that server-issued cookie only to the port-forward host.
func sendSteleWebhook(t *testing.T, endpoint, bearer string, expectedStatus int) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"alerts":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode == expectedStatus {
			return
		}
		// Stele refreshes its credential snapshot asynchronously. Only the
		// expected successful first delivery may wait for that bounded refresh;
		// an expected rejection must remain an immediate security assertion.
		if expectedStatus != http.StatusNoContent || time.Now().After(deadline) {
			t.Fatalf("Stele webhook status=%d want=%d body=%s", response.StatusCode, expectedStatus, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func helmMaintenanceClient(t *testing.T, base, origin, username, password string) *http.Client {
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
	request.Host = strings.TrimPrefix(origin, "https://")
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
			client.Transport = helmSessionCookieTransport{base: http.DefaultTransport, host: request.URL.Host, cookie: cookie}
			return client
		}
	}
	t.Fatal("maintenance login returned no session cookie")
	return nil
}

type helmSessionCookieTransport struct {
	base   http.RoundTripper
	host   string
	cookie *http.Cookie
}

func (transport helmSessionCookieTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host == transport.host {
		request = request.Clone(request.Context())
		request.AddCookie(transport.cookie)
	}
	return transport.base.RoundTrip(request)
}
func helmMaintenanceState(t *testing.T, client *http.Client, base, origin string) helmMaintenanceView {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/maintenance", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = strings.TrimPrefix(origin, "https://")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("maintenance status=%d body=%s", response.StatusCode, body)
	}
	var state helmMaintenanceView
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
func helmRequestJSON(t *testing.T, client *http.Client, method, url, origin string, value any, expected int) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
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
	actual, _ := io.ReadAll(response.Body)
	if response.StatusCode != expected {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, url, response.StatusCode, expected, actual)
	}
}

type helmWorkloadSnapshot struct {
	UID      string
	Phase    string
	Ready    bool
	Restarts int
}

func snapshotHelmWorkloads(t *testing.T, recorder *evidence, env []string) map[string]helmWorkloadSnapshot {
	t.Helper()
	snapshot := map[string]helmWorkloadSnapshot{}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		selector := "app.kubernetes.io/name=quoin,app.kubernetes.io/instance=" + mainRelease + ",app.kubernetes.io/component=" + component
		body := recorder.run("snapshot-"+component+"-pods", env, nil, 0, "kubectl", "--namespace", mainNs, "get", "pods", "--selector="+selector, "--output=json")
		var pods struct {
			Items []struct {
				Metadata struct {
					UID string `json:"uid"`
				} `json:"metadata"`
				Status struct {
					Phase             string `json:"phase"`
					ContainerStatuses []struct {
						Ready        bool `json:"ready"`
						RestartCount int  `json:"restartCount"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &pods); err != nil {
			t.Fatal(err)
		}
		if len(pods.Items) != 1 {
			t.Fatalf("%s does not have exactly one workload pod: %s", component, body)
		}
		restarts, ready := 0, len(pods.Items[0].Status.ContainerStatuses) > 0
		for _, status := range pods.Items[0].Status.ContainerStatuses {
			restarts += status.RestartCount
			ready = ready && status.Ready
		}
		snapshot[component] = helmWorkloadSnapshot{UID: pods.Items[0].Metadata.UID, Phase: pods.Items[0].Status.Phase, Ready: ready, Restarts: restarts}
	}
	return snapshot
}

func assertHelmWorkloadsUnchanged(t *testing.T, recorder *evidence, env []string, before map[string]helmWorkloadSnapshot) {
	t.Helper()
	after := snapshotHelmWorkloads(t, recorder, env)
	for component, value := range before {
		if after[component] != value {
			t.Fatalf("%s changed during rejected restore: before=%+v after=%+v", component, value, after[component])
		}
	}
}

func assertHelmWorkloadsRunning(t *testing.T, recorder *evidence, env []string) {
	t.Helper()
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		replicas := strings.TrimSpace(recorder.run("assert-"+component+"-running", env, nil, 0, "kubectl", "--namespace", mainNs, "get", "deployment/"+mainRelease+"-"+component, "--output", "jsonpath={.spec.replicas}"))
		if replicas != "1" {
			t.Fatalf("%s replicas=%q want 1 after rejected restore", component, replicas)
		}
	}
}

func assertHelmRestorePreflightReport(t *testing.T, path string) {
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

type helmPTY struct {
	cmd  *exec.Cmd
	file *os.File
	log  bytes.Buffer
	mu   sync.Mutex
}

func startHelmPTY(t *testing.T, dir, name string, env []string, argv ...string) *helmPTY {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = repoRoot(t)
	if env != nil {
		cmd.Env = env
	}
	file, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	value := &helmPTY{cmd: cmd, file: file}
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
		value.mu.Lock()
		output := append([]byte(nil), value.log.Bytes()...)
		value.mu.Unlock()
		_ = os.WriteFile(filepath.Join(dir, name+".log"), output, 0o600)
	})
	return value
}
func (command *helmPTY) write(t *testing.T, input string) {
	t.Helper()
	if _, err := command.file.WriteString(input); err != nil {
		t.Fatal(err)
	}
}
func (command *helmPTY) output() string {
	command.mu.Lock()
	defer command.mu.Unlock()
	return command.log.String()
}
func (command *helmPTY) outputBytes() []byte { return []byte(command.output()) }

func assertNoSecretInHelmEvidence(t *testing.T, root string, secrets ...string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(body, []byte(secret)) {
				return fmt.Errorf("evidence contains recovery secret: %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func (command *helmPTY) waitFor(t *testing.T, want string, timeout time.Duration) {
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
func (command *helmPTY) wait(t *testing.T, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- command.cmd.Wait() }()
	select {
	case <-time.After(timeout):
		_ = command.cmd.Process.Kill()
		t.Fatalf("PTY timed out:\n%s", command.output())
		return -1
	case <-done:
		return command.cmd.ProcessState.ExitCode()
	}
}

func helmNewestBackupID(t *testing.T, client *http.Client) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:19180/api/v1/backups", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "quoin.example.com"
	request.Header.Set("Origin", "https://quoin.example.com")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list backups status=%d body=%s", response.StatusCode, body)
	}
	var page struct {
		LatestSuccess *struct {
			ID string `json:"id"`
		} `json:"latestSuccess"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if page.LatestSuccess == nil || page.LatestSuccess.ID == "" {
		t.Fatalf("latest backup missing: %s", body)
	}
	return page.LatestSuccess.ID
}
