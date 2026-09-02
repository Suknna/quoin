package helm

// TestTicket35Helm drives the real Kubernetes recovery path: a real Helm
// install, a real public-API Lintel registration through a one-shot attached
// stdin Pod, then the actual `quoin-deploy helm recover-lintel` helper for
// BOTH storage dispositions, proving receipt-backed success and that the
// one-time registration token never leaks into evidence artifacts.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTicket35Helm(t *testing.T) {
	root := os.Getenv("QUOIN_EVIDENCE_DIR")
	if root == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T35 acceptance evidence run disabled")
	}
	requireTools(t)
	evidenceDir := filepath.Join(root, "helm")
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recorder := newEvidence(t, evidenceDir)
	suffix := strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 36)
	registryName = "t35-registry-" + suffix
	registryRepository = "t35-" + suffix
	mainRelease, mainNs = "t35-"+suffix, "quoin-t35-"+suffix
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
	configPath := writeInstallConfig(t, workRoot, "t35-install.yaml")
	env := deployEnv(workRoot, mainRelease, mainNs)
	recorder.run("install", env, strings.NewReader(strings.Join([]string{"admin", "Ticket 35 Admin", password, password}, "\n")+"\n"), 0,
		helper, "helm", "install", "--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "install-report.json"))

	forward := startHelmPTY(t, evidenceDir, "t35-port-forward", nil, "kubectl", "--namespace", mainNs, "port-forward", "service/"+mainRelease+"-quoin-public", "19880:8080")
	forward.waitFor(t, "Forwarding", 45*time.Second)
	base, origin := "http://127.0.0.1:19880", "https://quoin.example.com"
	client := helmMaintenanceClient(t, base, origin, "admin", password)
	formal := password + "-formal"
	helmRequestJSON(t, client, http.MethodPut, base+"/api/v1/auth/password", origin, map[string]string{"currentPassword": password, "newPassword": formal}, http.StatusNoContent)
	client = helmMaintenanceClient(t, base, origin, "admin", formal)
	oneTimeToken := registerHelmLintel(t, recorder, client, base, origin, mainNs, mainRelease, images["lintel"].Repository+"@"+images["lintel"].Index)
	_ = forward.cmd.Process.Kill()

	for index, disposition := range []string{"exclusively_reattached", "retired"} {
		reportPath := filepath.Join(workRoot, "recovery-"+disposition+"-report.json")
		recorder.run("recover-lintel-"+disposition, env, nil, 0,
			helper, "helm", "recover-lintel", "--phase", "action", "--storage-disposition", disposition,
			"--config", configPath, "--release-manifest", manifestPath, "--report", reportPath)
		assertHelmRecoveryReport(t, reportPath, disposition)
		// The public runtime view must show the rotation through a fresh
		// port-forward after each recovery restarted the release.
		verifyForward := startHelmPTY(t, evidenceDir, "t35-verify-forward-"+disposition, nil, "kubectl", "--namespace", mainNs, "port-forward", "service/"+mainRelease+"-quoin-public", "19880:8080")
		verifyForward.waitFor(t, "Forwarding", 45*time.Second)
		assertHelmLintelGeneration(t, client, base, origin, int64(index+2))
		_ = verifyForward.cmd.Process.Kill()
	}
	recorder.run("recover-lintel-assert", env, nil, 0,
		helper, "helm", "recover-lintel", "--phase", "assert",
		"--config", configPath, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "recovery-assert-report.json"))
	recorder.observe("lintel-recovery-observation.json", map[string]any{
		"dispositions": []string{"exclusively_reattached", "retired"},
		"registration": "public reveal + attached-stdin one-shot Pod",
		"firstAuth":    "issuer exec exited only after the replacement Hello",
		"receipt":      "finalize Pod committed the immutable receipt",
		"postRecovery": "assert-phase operational verification passed",
	})
	recorder.observe("lintel-recovery-observation.json", map[string]any{
		"dispositions":      []string{"exclusively_reattached", "retired"},
		"registration":      "public reveal + attached-stdin one-shot Pod",
		"generationAdvance": "public /api/v1/runtime showed currentGeneration 2 then 3 with connected=true",
		"firstAuth":         "issuer exec exited only after the replacement Hello",
		"receipt":           "finalize Pod committed the immutable receipt",
		"postRecovery":      "assert-phase operational verification passed",
	})
	cleanupTicketResources(t, recorder, workRoot, registryRef, baseline, password)
	cleaned = true
	recorder.observe("cleanup.json", map[string]any{
		"backend":        "helm",
		"ownedResources": []string{"Helm release/namespace/PVCs/pods", "local registry", "release images and OCI chart", "temporary credentials"},
		"result":         "cleanupTicketResources removed every owned resource before this record was written",
	})
	recorder.observe("runtime-evidence.json", map[string]any{
		"gitCommit": recorder.gitCommit, "dirtyStateDigest": recorder.dirtyState,
		"startedAt": recorder.startedAt.Format(time.RFC3339Nano), "finishedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"status": "passed", "tools": recorder.toolInfo, "commands": recorder.commands, "artifacts": recorder.artifacts,
		"assertions": map[string]string{
			"registeredLintel":    "public prepare/reveal + attached-stdin one-shot Pod registered the real slot",
			"dispositions":        "exclusively_reattached, retired",
			"receiptAndFirstAuth": "finalize Pod committed the immutable receipt after the real replacement Hello",
			"secretScan":          "the known web-reveal one-time registration token is absent from every evidence artifact",
			"postRecovery":        "assert-phase operational verification passed on the recovered release",
		},
	})
	// The leak scan runs last so every evidence artifact is covered; the
	// known secret is the web-reveal one-time registration token.
	scanHelmT35Evidence(t, evidenceDir, oneTimeToken)
}

// registerHelmLintel performs the real initial registration: public
// prepare/reveal through the port-forwarded API, then a one-shot Pod with the
// Lintel state PVC mounted consuming the envelope on attached stdin.
func registerHelmLintel(t *testing.T, recorder *evidence, client *http.Client, base, origin, namespace, release, lintelImage string) string {
	t.Helper()
	var runtimeView struct {
		Lintel struct {
			RowVersion int64 `json:"rowVersion"`
		} `json:"lintel"`
	}
	if err := json.Unmarshal([]byte(helmGet(t, client, base+"/api/v1/runtime", origin)), &runtimeView); err != nil {
		t.Fatal(err)
	}
	prepared := helmT35Post(t, client, base+"/api/v1/runtime-slots/lintel/registration/prepare", origin,
		fmt.Sprintf(`{"clientCommandId":"t35-hlintel-prepare-%d","expectedRowVersion":%d}`, time.Now().UnixNano(), runtimeView.Lintel.RowVersion))
	var preparedView struct {
		RegistrationTokenHandle string `json:"registrationTokenHandle"`
	}
	if err := json.Unmarshal([]byte(prepared), &preparedView); err != nil {
		t.Fatal(err)
	}
	revealed := helmT35Post(t, client, base+"/api/v1/runtime-slots/registration-token/reveal", origin,
		fmt.Sprintf(`{"registrationTokenHandle":%q}`, preparedView.RegistrationTokenHandle))
	var token struct {
		Slot              string `json:"slot"`
		Generation        int64  `json:"generation"`
		RegistrationToken string `json:"registrationToken"`
	}
	if err := json.Unmarshal([]byte(revealed), &token); err != nil || token.RegistrationToken == "" {
		t.Fatalf("reveal: %v %s", err, revealed)
	}
	// Holder pod + exec: `kubectl run -i` does not reliably forward stdin
	// with --overrides, so the registration runs through `kubectl exec -i`
	// on a shell-holding one-shot pod (Debian-based Lintel image).
	registerPod := release + "-t35-register"
	_ = exec.Command("kubectl", "--namespace", namespace, "delete", "--ignore-not-found=true", "pod", registerPod).Run()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[2]s, quoin.io/role: lintel-recovery-register}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532, fsGroupChangePolicy: OnRootMismatch, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: register
      image: %[3]s
      command: ["sh", "-c", "trap '' TERM; while :; do sleep 30; done"]
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: state, mountPath: /var/lib/lintel}
        - {name: runtime-ca, mountPath: /run/quoin-secrets/runtime-ca.pem, subPath: runtime-ca.pem, readOnly: true}
  volumes:
    - name: config
      configMap: {name: %[2]s-component-lintel}
    - name: state
      persistentVolumeClaim: {claimName: %[2]s-lintel-state}
    - name: runtime-ca
      secret: {secretName: %[2]s-secrets}
`, registerPod, release, lintelImage)
	apply := exec.Command("kubectl", "--namespace", namespace, "apply", "--filename", "-")
	apply.Stdin = strings.NewReader(manifest)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("register pod apply: %v: %s", err, out)
	}
	defer func() {
		_ = exec.Command("kubectl", "--namespace", namespace, "delete", "--ignore-not-found=true", "pod", registerPod).Run()
	}()
	wait, waitErr := exec.Command("kubectl", "--namespace", namespace, "wait", "--for=condition=Ready", "pod/"+registerPod, "--timeout=180s").CombinedOutput()
	if waitErr != nil {
		describe, _ := exec.Command("kubectl", "--namespace", namespace, "describe", "pod", registerPod).CombinedOutput()
		events, _ := exec.Command("kubectl", "--namespace", namespace, "get", "events", "--sort-by=.lastTimestamp").CombinedOutput()
		yaml, _ := exec.Command("kubectl", "--namespace", namespace, "get", "pod", registerPod, "-o", "yaml").CombinedOutput()
		t.Fatalf("register pod ready: %v: %s\nDESCRIBE:\n%s\nEVENTS:\n%s\nYAML:\n%s", waitErr, wait, describe, events, yaml)
	}
	command := exec.Command("kubectl", "--namespace", namespace, "exec", "-i", registerPod, "--", "/lintel", "register", "--config", "/etc/quoin/component.yaml")
	command.Stdin = strings.NewReader(fmt.Sprintf(`{"slot":%q,"generation":%d,"token":%q}`, token.Slot, token.Generation, token.RegistrationToken) + "\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("lintel registration: %v: %s", err, output)
	}
	return token.RegistrationToken
}

func assertHelmRecoveryReport(t *testing.T, path, disposition string) {
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

// assertHelmLintelGeneration proves through the public API that the
// rotation advanced the current generation and the replacement reconnected.
func assertHelmLintelGeneration(t *testing.T, client *http.Client, base, origin string, generation int64) {
	t.Helper()
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		var view struct {
			Lintel struct {
				CurrentGeneration int64 `json:"currentGeneration"`
				Connected         bool  `json:"connected"`
			} `json:"lintel"`
		}
		if err := json.Unmarshal([]byte(helmGet(t, client, base+"/api/v1/runtime", origin)), &view); err != nil {
			t.Fatal(err)
		}
		if view.Lintel.CurrentGeneration == generation && view.Lintel.Connected {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("lintel did not reconnect at generation %d", generation)
}

func scanHelmT35Evidence(t *testing.T, evidenceDir, token string) {
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

func helmGet(t *testing.T, client *http.Client, url, origin string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = strings.TrimPrefix(origin, "https://")
	request.Header.Set("Origin", origin)
	return helmT35Do(t, client, request)
}

func helmT35Post(t *testing.T, client *http.Client, url, origin, payload string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Host = strings.TrimPrefix(origin, "https://")
	request.Header.Set("Origin", origin)
	return helmT35Do(t, client, request)
}

func helmT35Do(t *testing.T, client *http.Client, request *http.Request) string {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := new(bytes.Buffer)
	if _, err := io.ReadAll(io.TeeReader(response.Body, body)); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("%s %s status=%d: %s", request.Method, request.URL, response.StatusCode, body.String())
	}
	return body.String()
}
