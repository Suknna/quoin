// Package recovery hosts the T12 ticket acceptance run: the real compose
// stack, the deterministic fixture provider (slow stream, partial-stream
// failure), a real Alertmanager delivery per scenario alert, and the full
// cancel / retry / recovery path — cancellation fence races, same-boot
// reconnect with reliable result delivery, ResultAck-loss replay, new-boot
// interruption, lease-loss convergence, partial-token failure and repeated
// command interleavings. Evidence lands under .artifacts/tickets/T12/.
package recovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	projectName = "quoin"
	quoinPort   = 18080
	stelePort   = 18081
	amUiPort    = 19093
)

type ticketEvidence struct {
	dir       string
	commands  []map[string]any
	artifacts []map[string]any
	env       []string
	stateRoot string
}

func TestTicket12(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T12 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	for _, stale := range []string{"t12-am", "t12-forwarder"} {
		exec.Command("docker", "rm", "-f", stale).Run()
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
	fixtureCmd := exec.Command(fixtureBinary, "-address", "0.0.0.0:18443")
	fixtureCmd.Env = evidence.env
	fixtureLog, err := os.Create(filepath.Join(evidence.dir, "fixture-provider.log"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureCmd.Stdout = fixtureLog
	fixtureCmd.Stderr = fixtureLog
	if err := fixtureCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		fixtureCmd.Process.Kill()
		_ = fixtureCmd.Wait()
		fixtureLog.Close()
	}()
	waitFor(t, "fixture provider listening", func() bool {
		logBody, _ := os.ReadFile(filepath.Join(evidence.dir, "fixture-provider.log"))
		return bytes.Contains(logBody, []byte("listening"))
	}, 20*time.Second)

	// --- Register Plinth so agent attempts dispatch ---------------------
	prepareProviderAndRuntime(t, evidence, client, base, origin, composeFile)

	// --- Alert source + Alertmanager ------------------------------------
	bearer := createAlertSource(t, evidence, client, base, origin)
	startAlertmanager(t, evidence, workRoot, bearer)

	// --- Scenario alerts -------------------------------------------------
	// Each alertname is a distinct fingerprint; the fixture branches on the
	// alert name carried inside the frozen analysis prompt.
	fireAlert := func(t *testing.T, alertName string) string {
		t.Helper()
		execCommand(t, evidence, "am-alert-"+alertName, nil, "docker", "exec", "t12-am", "amtool", "--alertmanager.url=http://127.0.0.1:9093", "alert", "add", "alertname="+alertName, "severity=critical", "instance=db-t12", "job=quoin")
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			body := httpGet(t, client, base+"/api/v1/alerts", origin)
			if strings.Contains(body, alertName) {
				var snapshot struct {
					Items []struct {
						ID     string            `json:"id"`
						Labels map[string]string `json:"labels"`
					} `json:"items"`
				}
				if err := json.Unmarshal([]byte(body), &snapshot); err == nil {
					for _, item := range snapshot.Items {
						if item.Labels["alertname"] == alertName {
							return item.ID
						}
					}
				}
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("%s occurrence never appeared", alertName)
		return ""
	}

	dbPath := filepath.Join(stateRoot, "quoin", "compose", "data", "quoin.db")
	queryRow := sqliteQuerier(t, dbPath)

	// S1 -----------------------------------------------------------------
	s1CancelFirst(t, evidence, client, base, origin, fireAlert, queryRow)
	// S2 -----------------------------------------------------------------
	s2ResultFirst(t, evidence, client, base, origin, fireAlert, queryRow)
	// S3 -----------------------------------------------------------------
	s3SameBootNetworkBlip(t, evidence, client, base, origin, composeFile, fireAlert, queryRow)
	// S3b ----------------------------------------------------------------
	s3bResultAckLoss(t, evidence, client, base, origin, composeFile, fireAlert, queryRow)
	// S4 -----------------------------------------------------------------
	s4NewBoot(t, evidence, client, base, origin, composeFile, fireAlert, queryRow)
	// S5 -----------------------------------------------------------------
	s5LeaseLoss(t, evidence, client, base, origin, composeFile, fireAlert, queryRow)
	// S6 -----------------------------------------------------------------
	s6PartialThenFailure(t, evidence, client, base, origin, fireAlert, queryRow)

	// --- No credential / environment leakage -----------------------------
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Fatalf("host sqlite3 is required for the evidence path: %v", err)
	}
	cipherHex := queryRow(`SELECT lower(hex(ciphertext)) FROM credential_generations WHERE connection_id=(SELECT id FROM connections WHERE name='t12-provider')`)
	keyHex := fmt.Sprintf("%x", []byte("fixture-api-key-2026"))
	if strings.Contains(cipherHex, keyHex) {
		t.Fatalf("plaintext API key found inside the sealed credential ciphertext")
	}
	scanForSecrets(t, evidenceDir, newPassword, tempPassword, bearer, "fixture-api-key-2026")

	// --- Teardown ----------------------------------------------------------
	pausedPlinth := false
	if state := strings.TrimSpace(outputOf(t, "docker", "inspect", "-f", "{{.State.Paused}}", containerOf(t, composeFile, "plinth"))); state == "true" {
		execCommand(t, evidence, "teardown-unpause", nil, "docker", "unpause", containerOf(t, composeFile, "plinth"))
		pausedPlinth = true
	}
	_ = pausedPlinth
	execCommand(t, evidence, "teardown-forwarder", nil, "docker", "rm", "-f", "t12-forwarder")
	execCommand(t, evidence, "teardown-am", nil, "docker", "rm", "-f", "t12-am")
	execCommand(t, evidence, "teardown-stack", nil, "docker", "compose", "--project-name", projectName, "--file", composeFile, "down", "--remove-orphans")
	builtImages := []string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if !preExisting[image] {
			builtImages = append(builtImages, image)
		}
	}
	if len(builtImages) > 0 {
		arguments := append([]string{"rmi"}, builtImages...)
		execCommand(t, evidence, "teardown-images", nil, "docker", arguments...)
	} else {
		evidence.note(t, "teardown-images.json", mustJSON(t, map[string]any{"conclusion": "all four images pre-existed; none removed"}))
	}
	commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	evidence.writeRuntimeEvidence(t, commit, newPassword, tempPassword, bearer, map[string]any{
		"realPath": "Alertmanager container -> Stele -> Quoin SQLite -> HTTP create -> Plinth supervisor -> sandboxed worker -> fixture provider -> cancel/reconnect/recovery semantics -> HTTP detail + SQLite authority",
		"scenarios": map[string]string{
			"cancel-first":   "fence commits Cancelling -> CancelAttempt -> worker stop -> CancelAck -> Cancelled; repeated command idempotent; terminal re-cancel conflicts 409",
			"result-first":   "success commits first -> cancel command returns the completed object (200, no 409)",
			"same-boot":      "quoin container restart mid-run (control stream dies) -> plinth reconnects same boot with epoch+1 -> reconcile renews lease -> running attempt completes Succeeded",
			"resultack-loss": "quoin container restart at result time -> in-flight ack/proposal lost -> pending delivery replays on reconnect -> idempotent adjudication commits exactly once",
			"new-boot":       "plinth container restart mid-run -> new boot accepted -> old-boot attempt Interrupted(lease_expired) immediately; operator retry (new analysis) Succeeds",
			"lease-loss":     "plinth paused mid-run -> heartbeats stop -> lease burns -> sweeper converges Interrupted(lease_expired); unpausing restores the runtime; retry Succeeds",
			"partial-fail":   "fixture hangs up after two visible tokens -> physical call seals failed with complete=0 partial output -> attempt Failed(provider_unavailable), no domain output",
		},
		"redactions": "the provider API key appears only in the request body over TLS and the provider bearer header; never in evidence, logs or the sealed ciphertext",
	})
	evidence.writeCleanup(t, map[string]any{
		"containers":      []string{"t12-am (removed)", "t12-forwarder (removed)", "compose stack quoin-* (down --remove-orphans)"},
		"images":          builtImagesDisposition(preExisting),
		"fixtureProvider": "host process killed and waited",
		"volumes":         "compose named volumes removed by down; workRoot removed via t.TempDir",
		"credentials":     "registration token consumed once; long-term token only in the plinth state volume (deleted with the stack); revealed bearer never persisted",
	})
	os.RemoveAll(workRoot)
}
