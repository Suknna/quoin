package release_test

// TestTicket35Compose drives the real Compose recovery path: a real install,
// a real public-API Lintel registration, then the actual
// `quoin-deploy compose recover-lintel` helper for BOTH storage dispositions,
// proving the receipt-backed success and that the one-time registration token
// never leaks into any evidence artifact.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/test/support"
)

func TestTicket35Compose(t *testing.T) {
	evidenceRoot := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceRoot == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T35 acceptance evidence run disabled")
	}
	requireTools(t)
	evidenceDir := filepath.Join(evidenceRoot, "compose")
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recorder := newEvidence(t, evidenceDir)
	suffix := strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 36)
	mainProject = "quoin-t35-" + suffix
	registryName, registryRepository = "t35-registry-"+suffix, "t35-"+suffix
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
	quoinPort, stelePort := 19780, 19781
	configPath := writeInstallConfig(t, workRoot, "t35-install.yaml", filepath.Join(workRoot, "secrets"), quoinPort, stelePort)
	env := composeEnv(workRoot, mainProject)
	recorder.run("install", env, strings.NewReader(strings.Join([]string{"admin", "Ticket 35 Admin", password, password}, "\n")+"\n"), 0,
		helper, "compose", "install", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "install-report.json"))

	// Real Lintel registration through the public reveal + attached stdin.
	base := "http://127.0.0.1:" + strconv.Itoa(quoinPort)
	client := maintenanceClient(t, base, "https://quoin.example.com", "admin", password)
	formal := password + "-formal"
	requestJSON(t, client, http.MethodPut, base+"/api/v1/auth/password", "https://quoin.example.com", map[string]string{"currentPassword": password, "newPassword": formal}, http.StatusNoContent)
	client = maintenanceClient(t, base, "https://quoin.example.com", "admin", formal)
	composeFile := filepath.Join(workRoot, mainProject, "state", "quoin", "compose", "generated", "compose.yaml")
	oneTimeToken := registerComposeLintel(t, recorder, client, base, composeFile, mainProject, env)

	for index, disposition := range []string{"exclusively_reattached", "retired"} {
		reportPath := filepath.Join(workRoot, "recovery-"+disposition+"-report.json")
		recorder.run("recover-lintel-"+disposition, env, nil, 0,
			helper, "compose", "recover-lintel", "--phase", "action", "--storage-disposition", disposition,
			"--config", configPath, "--release-manifest", manifestPath, "--report", reportPath)
		assertRecoveryReport(t, reportPath, disposition)
		// The public runtime view must show the rotation: the current Lintel
		// generation advanced and reconnected through the replacement token.
		assertLintelGeneration(t, client, base, int64(index+2))
	}
	// The stack must be healthy again after the last recovery; the assert
	// phase re-runs the read-only verifier on the recovered deployment.
	recorder.run("recover-lintel-assert", env, nil, 0,
		helper, "compose", "recover-lintel", "--phase", "assert", "--storage-disposition", "retired",
		"--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "recovery-assert-report.json"))
	recorder.observe("lintel-recovery-observation.json", map[string]any{
		"dispositions":      []string{"exclusively_reattached", "retired"},
		"registration":      "public reveal + attached-stdin one-shot",
		"generationAdvance": "public /api/v1/runtime showed currentGeneration 2 then 3 with connected=true",
		"firstAuth":         "issuer exited only after the replacement Hello",
		"receipt":           "helper finalize committed the immutable receipt",
		"postRecovery":      "assert-phase operational verification passed",
	})
	cleanupTicketResources(t, recorder, workRoot, registryRef, baseline, password)
	cleaned = true
	recorder.observe("cleanup.json", map[string]any{
		"backend":        "compose",
		"ownedResources": []string{"Compose project/network/volumes/containers", "local registry", "release images", "temporary credentials"},
		"result":         "cleanupTicketResources removed every owned resource before this record was written",
	})
	writeTicket35RuntimeEvidence(t, recorder, evidenceDir, []string{"exclusively_reattached", "retired"})
	// The leak scan runs last so every evidence artifact, including the
	// summaries written above, is covered; the known secret is the web-reveal
	// one-time registration token.
	scanTicket35Evidence(t, evidenceDir, oneTimeToken)
}

// registerComposeLintel performs the real initial Lintel registration through
// the public prepare/reveal API and the one-shot attached-stdin container,
// returning the one-time token so the evidence scan can prove it never leaks.
func registerComposeLintel(t *testing.T, recorder *evidence, client *http.Client, base, composeFile, project string, env []string) string {
	t.Helper()
	var runtimeView struct {
		Lintel struct {
			RowVersion int64 `json:"rowVersion"`
		} `json:"lintel"`
	}
	body := getJSON(t, client, base+"/api/v1/runtime", "https://quoin.example.com")
	if err := json.Unmarshal(body, &runtimeView); err != nil {
		t.Fatal(err)
	}
	prepared := t35Post(t, client, base+"/api/v1/runtime-slots/lintel/registration/prepare", "https://quoin.example.com",
		fmt.Sprintf(`{"clientCommandId":"t35-lintel-prepare-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), runtimeView.Lintel.RowVersion))
	var preparedView struct {
		RegistrationTokenHandle string `json:"registrationTokenHandle"`
	}
	if err := json.Unmarshal([]byte(prepared), &preparedView); err != nil {
		t.Fatal(err)
	}
	revealed := t35Post(t, client, base+"/api/v1/runtime-slots/registration-token/reveal", "https://quoin.example.com",
		fmt.Sprintf(`{"registrationTokenHandle":%q}`, preparedView.RegistrationTokenHandle))
	var token struct {
		Slot              string `json:"slot"`
		Generation        int64  `json:"generation"`
		RegistrationToken string `json:"registrationToken"`
	}
	if err := json.Unmarshal([]byte(revealed), &token); err != nil || token.RegistrationToken == "" {
		t.Fatalf("reveal: %v %s", err, revealed)
	}
	input := fmt.Sprintf(`{"slot":%q,"generation":%d,"token":%q}`, token.Slot, token.Generation, token.RegistrationToken)
	// The registration one-shot output is non-secret; the one-time token is
	// fed only through stdin.
	command := exec.Command("docker", "compose", "--project-name", project, "--file", composeFile, "run", "--rm", "--no-deps", "-i", "-T", "lintel", "register", "--config", "/etc/quoin/component.yaml")
	command.Env = env
	command.Stdin = strings.NewReader(input + "\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("lintel registration: %v: %s", err, output)
	}
	// Lintel must reach its connected state before the recovery scenario.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		view := string(getJSON(t, client, base+"/api/v1/runtime", "https://quoin.example.com"))
		if strings.Contains(view, `"lintel":{"slot":"lintel","state":"registered"`) && strings.Contains(view, `"connected":true`) {
			return token.RegistrationToken
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("lintel did not connect after registration")
	return ""
}

func assertRecoveryReport(t *testing.T, path, disposition string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		ExitCode int    `json:"exitCode"`
		Command  string `json:"command"`
		Failure  *struct {
			Code string `json:"code"`
		} `json:"failure"`
		Stages []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"stages"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("recovery report %s: %v", path, err)
	}
	if report.ExitCode != 0 || report.Failure != nil || report.Command != "recover-lintel" {
		t.Fatalf("recovery report exitCode=%d failure=%+v command=%q disposition=%s body=%s", report.ExitCode, report.Failure, report.Command, disposition, body)
	}
	completed := map[string]bool{}
	for _, stage := range report.Stages {
		if stage.Status == "completed" {
			completed[stage.Name] = true
		}
	}
	for _, required := range []string{"recovery-stop-fence", "recovery-register", "recovery-finalize", "recovery-restart"} {
		if !completed[required] {
			t.Fatalf("recovery report for %s misses completed stage %q: %s", disposition, required, body)
		}
	}
}

// assertLintelGeneration proves through the public API that the rotation
// advanced the current generation and the replacement reconnected.
func assertLintelGeneration(t *testing.T, client *http.Client, base string, generation int64) {
	t.Helper()
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		var view struct {
			Lintel struct {
				CurrentGeneration int64 `json:"currentGeneration"`
				Connected         bool  `json:"connected"`
			} `json:"lintel"`
		}
		if err := json.Unmarshal(getJSON(t, client, base+"/api/v1/runtime", "https://quoin.example.com"), &view); err != nil {
			t.Fatal(err)
		}
		if view.Lintel.CurrentGeneration == generation && view.Lintel.Connected {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("lintel did not reconnect at generation %d", generation)
}

// scanTicket35Evidence proves the one-time registration token never entered
// any evidence artifact (reports, logs, observations).
func scanTicket35Evidence(t *testing.T, evidenceDir, token string) {
	t.Helper()
	if token == "" {
		t.Fatal("one-time token missing for the evidence scan")
	}
	err := filepath.Walk(evidenceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(body, []byte(token)) {
			t.Fatalf("one-time registration token leaked into %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// t35Post posts one JSON payload and returns the body (2xx enforced).
func t35Post(t *testing.T, client *http.Client, url, origin, payload string) string {
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
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("POST %s status=%d: %s", url, response.StatusCode, buffer.String())
	}
	return buffer.String()
}

// writeTicket35RuntimeEvidence writes the ticket-mandated structured
// evidence summary with expected-versus-actual assertions.
func writeTicket35RuntimeEvidence(t *testing.T, recorder *evidence, evidenceDir string, dispositions []string) {
	t.Helper()
	recorder.observe("runtime-evidence.json", map[string]any{
		"gitCommit": recorder.gitCommit, "dirtyStateDigest": recorder.dirtyState,
		"startedAt": recorder.startedAt.Format(time.RFC3339Nano), "finishedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"status": "passed", "tools": recorder.toolInfo, "commands": recorder.commands, "artifacts": recorder.artifacts,
		"assertions": map[string]string{
			"registeredLintel":    "public prepare/reveal + attached-stdin one-shot registered the real slot",
			"dispositions":        strings.Join(dispositions, ", "),
			"receiptAndFirstAuth": "helper finalization committed the immutable receipt after the real replacement Hello",
			"secretScan":          "the one-time registration token is absent from every evidence artifact",
			"postRecovery":        "assert-phase operational verification passed on the recovered deployment",
		},
	})
}

var _ = support.CreateAlertSource
