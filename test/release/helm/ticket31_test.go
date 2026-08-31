package helm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTicket31 proves the hardened Helm/Kubernetes install end to end through
// the real quoin-deploy / helm / kubectl / cluster path: digest-pinned
// four-component images plus an OCI chart digest, retained PVCs outside Helm,
// an in-cluster secret bootstrap Job with retry, an attached-TTY first
// administrator, fixed probes/security context, the frozen readiness/metrics/
// logs contracts, and exact cleanup and retained-state semantics — all with
// structured evidence under QUOIN_EVIDENCE_DIR.
func TestTicket31(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T31 acceptance evidence run disabled")
	}
	requireTools(t)
	recorder := newEvidence(t, evidenceDir)
	workRoot := t.TempDir()
	baseline := captureEnvironmentBaseline(t, recorder)
	registryRef := startRegistry(t, recorder)
	// Cleanup is registered as soon as external resources can be created; every
	// later Fatalf still tears down test-owned namespaces, registry and files.
	var tempPassword, password string
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			cleanupTicketResources(t, recorder, workRoot, registryRef, baseline, tempPassword, password)
		}
	})

	images := buildAndPushReleaseImages(t, recorder, workRoot)
	chartDigest, chartSHA := pushChartOCI(t, recorder, workRoot)
	manifestPath := writeReleaseManifest(t, recorder, workRoot, images, chartDigest, chartSHA)

	helper := filepath.Join(workRoot, "quoin-deploy")
	started := time.Now()
	recorder.run("build-helper", nil, nil, 0, "go", "build", "-trimpath", "-o", helper, "./cmd/quoin-deploy")
	recorder.observe("helper-binary.json", map[string]any{"path": helper, "buildSeconds": time.Since(started).Seconds()})

	mainConfig := writeInstallConfig(t, workRoot, "install-main.yaml")

	// Minimal Schema input proof: unknown fields and malformed manifests are
	// rejected with exit 2 and no cluster side effect.
	badConfig := filepath.Join(workRoot, "bad-install.yaml")
	writeFileT(t, badConfig, strings.Replace(readFileT(t, mainConfig), "lintelBrowserSlots: 1", "lintelBrowserSlots: 1\nunknownField: true", 1))
	beforeNamespaces := recorder.run("namespaces-before", nil, nil, 0, "kubectl", "get", "namespaces", "--no-headers", "-o", "custom-columns=NAME:.metadata.name")
	recorder.run("invalid-input", deployEnv(workRoot, mainRelease, mainNs), strings.NewReader(""), 2, helper, "helm", "install", "--config", badConfig, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "report-invalid.json"))
	tampered := filepath.Join(workRoot, "release-manifest-bad.json")
	tamperManifest(t, manifestPath, tampered)
	recorder.run("invalid-manifest", deployEnv(workRoot, mainRelease, mainNs), strings.NewReader(""), 2, helper, "helm", "install", "--config", mainConfig, "--release-manifest", tampered, "--report", filepath.Join(workRoot, "report-badmanifest.json"))
	afterNamespaces := recorder.run("namespaces-after-bad", nil, nil, 0, "kubectl", "get", "namespaces", "--no-headers", "-o", "custom-columns=NAME:.metadata.name")
	if strings.TrimSpace(beforeNamespaces) != strings.TrimSpace(afterNamespaces) {
		t.Fatalf("invalid input produced cluster side effects:\nbefore:\n%s\nafter:\n%s", beforeNamespaces, afterNamespaces)
	}

	// Deterministic Job bootstrap retry proof: the quoin image digest points
	// nowhere, so the in-cluster secret bootstrap Job can never pull; the
	// staged install records the completed retained-volume stage and fails at
	// secret-bootstrap.
	broken := brokenImageManifest(t, manifestPath, filepath.Join(workRoot, "release-manifest-broken.json"))
	recorder.run("job-retry-broken-image", deployEnv(workRoot, retryRelease, retryNs), strings.NewReader("admin\nRetry Admin\nplaceholder-password\nplaceholder-password\n"), 1, helper, "helm", "install", "--config", writeInstallConfig(t, workRoot, "install-retry.yaml"), "--release-manifest", broken, "--report", filepath.Join(workRoot, "report-broken.json"))
	brokenState := readInstallState(t, filepath.Join(workRoot, retryRelease, "state", "quoin", "helm", "install-state.json"))
	if !containsStage(brokenState.StagesDone, "preflight") || !containsStage(brokenState.StagesDone, "retained-volumes") || containsStage(brokenState.StagesDone, "secret-bootstrap") || brokenState.FinishedAt != "" {
		t.Fatalf("broken-image install state wrong: %+v", brokenState)
	}
	brokenReport := loadReport(t, filepath.Join(workRoot, "report-broken.json"))
	if brokenReport.Failure == nil || brokenReport.Failure.Code != "secret_bootstrap_failed" {
		t.Fatalf("broken-image failure must be secret_bootstrap_failed: %+v", brokenReport.Failure)
	}
	// Failed bootstrap must remove every short-lived resource before a retry;
	// only the fixed deployment Secret and retained PVCs may survive.
	for _, resource := range [][]string{{"job", retryRelease + "-secret-bootstrap"}, {"rolebinding", retryRelease + "-secret-bootstrap"}, {"role", retryRelease + "-secret-bootstrap"}, {"serviceaccount", retryRelease + "-secret-bootstrap"}, {"configmap", retryRelease + "-bootstrap-config"}, {"pvc", retryRelease + "-bootstrap-staging"}} {
		arguments := append([]string{"--namespace", retryNs, "get"}, resource...)
		recorder.run("failed-bootstrap-cleanup-"+resource[0], nil, nil, 1, append([]string{"kubectl"}, arguments...)...)
	}

	// A partial state must fail closed before any new-target side effect.
	// Keep the exact XDG_STATE_HOME/release but select a different namespace.
	changedTarget := deployEnv(workRoot, retryRelease, retryNs+"-other")
	recorder.run("job-retry-changed-target", changedTarget, strings.NewReader(""), 2, helper, "helm", "install", "--config", writeInstallConfig(t, workRoot, "install-retry.yaml"), "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "report-target-mismatch.json"))
	recorder.run("job-retry-changed-target-no-side-effects", nil, nil, 1, "kubectl", "get", "namespace", retryNs+"-other")
	mismatchReport := loadReport(t, filepath.Join(workRoot, "report-target-mismatch.json"))
	if mismatchReport.Failure == nil || mismatchReport.Failure.Code != "install_state_mismatch" {
		t.Fatalf("changed target must fail closed with install_state_mismatch: %+v", mismatchReport.Failure)
	}

	// Same-identity retry resumes: weak administrator password fails inside
	// the admin stage after the bootstrap Job completed and the deployment
	// Secret exists.
	weak := strings.NewReader("admin\nRetry Admin\nshort\nshort\n")
	recorder.run("job-retry-weak-password", deployEnv(workRoot, retryRelease, retryNs), weak, 1, helper, "helm", "install", "--config", writeInstallConfig(t, workRoot, "install-retry.yaml"), "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "report-weak.json"))
	weakState := readInstallState(t, filepath.Join(workRoot, retryRelease, "state", "quoin", "helm", "install-state.json"))
	if !containsStage(weakState.StagesDone, "secret-bootstrap") || containsStage(weakState.StagesDone, "admin-bootstrap") || weakState.FinishedAt != "" {
		t.Fatalf("weak-password install state wrong: %+v", weakState)
	}

	password = randomPassword(t)
	answers := strings.NewReader(strings.Join([]string{"admin", "Retry Admin", password, password}, "\n") + "\n")
	resumeReport := filepath.Join(workRoot, "report-resume.json")
	recorder.run("job-retry-resume", deployEnv(workRoot, retryRelease, retryNs), answers, 0, helper, "helm", "install", "--config", writeInstallConfig(t, workRoot, "install-retry.yaml"), "--release-manifest", manifestPath, "--report", resumeReport)
	finished := readInstallState(t, filepath.Join(workRoot, retryRelease, "state", "quoin", "helm", "install-state.json"))
	if finished.FinishedAt == "" || !containsStage(finished.StagesDone, "verify") {
		t.Fatalf("successful retry did not finish the state: %+v", finished)
	}
	secretDetail := stageDetail(loadReport(t, resumeReport), "secret-bootstrap")
	if !strings.Contains(secretDetail, "already active") {
		t.Fatalf("retry re-ran the completed secret stage instead of resuming: %q", secretDetail)
	}

	// Formal digest-pinned install through the real helper path on the main
	// release (fresh namespace; PVCs and Secret are created by the helper).
	tempPassword = randomPassword(t)
	mainAnswers := strings.NewReader(strings.Join([]string{"admin", "Ticket Admin", tempPassword, tempPassword}, "\n") + "\n")
	installReport := filepath.Join(workRoot, "report-install.json")
	recorder.run("install", deployEnv(workRoot, mainRelease, mainNs), mainAnswers, 0, helper, "helm", "install", "--config", mainConfig, "--release-manifest", manifestPath, "--report", installReport)
	assertInstallReport(t, recorder, installReport)

	// helm lint/template proof with the helper-generated values: the same
	// authority the release actually installed.
	valuesPath := filepath.Join(workRoot, mainRelease, "state", "quoin", "helm", "values.yaml")
	values := readFileT(t, valuesPath)
	recorder.note("helper-values.yaml", values)
	recorder.run("helm-lint", nil, nil, 0, "helm", "lint", "deploy/helm/quoin", "--values", valuesPath)
	for name, invalid := range map[string]string{
		"empty-resources":           "resources: {}\n",
		"empty-component-resources": "resources: {quoin: {}}\n",
		"empty-resource-requests":   "resources: {quoin: {requests: {}}}\n",
		"empty-resource-limits":     "resources: {quoin: {limits: {}}}\n",
	} {
		invalidValues := filepath.Join(workRoot, "invalid-"+name+".yaml")
		writeFileT(t, invalidValues, values+"\ninput:\n"+invalid)
		// values are a helper projection whose input object is already present;
		// use yq-free textual replacement to keep the invalid fixture exact.
		writeFileT(t, invalidValues, strings.Replace(values, "prometheusRule: false", "prometheusRule: false\n  "+strings.ReplaceAll(strings.TrimSpace(invalid), "\n", "\n  "), 1))
		recorder.run("helm-lint-"+name, nil, nil, 1, "helm", "lint", "deploy/helm/quoin", "--values", invalidValues)
	}
	recorder.run("helm-template", nil, nil, 0, "helm", "template", mainRelease, "deploy/helm/quoin", "--values", valuesPath, "--namespace", mainNs, "--dry-run=server")
	monitorValues := filepath.Join(workRoot, "values-monitoring.yaml")
	writeFileT(t, monitorValues, strings.Replace(values, "prometheusRule: false", "prometheusRule: true", 1))
	monitoring := recorder.run("helm-template-monitoring", nil, nil, 0, "helm", "template", mainRelease, "deploy/helm/quoin", "--values", monitorValues, "--namespace", mainNs, "--dry-run=server")
	for alert, expression := range map[string]string{
		"QuoinQuoinNotReady":         "quoin_ready == 0",
		"QuoinPlinthNotReady":        "plinth_ready == 0",
		"QuoinLintelNotReady":        "lintel_ready == 0",
		"QuoinSteleNotReady":         "stele_ready == 0",
		"QuoinNotAcceptingWork":      "quoin_accepting_work == 0",
		"QuoinBackupScheduleOverdue": "quoin_backup_schedule_overdue == 1",
		"QuoinBackupActiveTooLong":   "quoin_backup_active == 1 and quoin_backup_oldest_active_age_seconds > 7200",
		"QuoinBackupFailures":        "increase(quoin_backup_failures_total[15m]) > 0",
		"QuoinRuntimeOffline":        "min(quoin_runtime_connected) == 0",
		"QuoinStreamQueueOverflow":   "increase(quoin_stream_queue_overflows_total[5m]) > 0",
		"QuoinStorageNotWritable":    "min(quoin_storage_writable) == 0",
		"QuoinArtifactGCOverdue":     "time() - quoin_artifact_gc_last_success_timestamp_seconds > 86400",
		"QuoinSteleUnavailable":      "stele_ready == 0",
	} {
		if !strings.Contains(monitoring, "alert: "+alert) || !strings.Contains(monitoring, "expr: "+expression) {
			t.Fatalf("enabled PrometheusRule missed %s (%s)", alert, expression)
		}
	}

	assertClusterProjection(t, recorder, mainNs, mainRelease, images)

	verifyReport := filepath.Join(workRoot, "report-verify.json")
	recorder.run("verify", deployEnv(workRoot, mainRelease, mainNs), nil, 0, helper, "helm", "verify", "--config", mainConfig, "--release-manifest", manifestPath, "--report", verifyReport)
	assertVerifyReport(t, recorder, verifyReport)

	retention := proveRetainedState(t, recorder, workRoot, helper, mainConfig, manifestPath, mainNs, mainRelease)
	recorder.observe("retained-state.json", retention)

	cleanupTicketResources(t, recorder, workRoot, registryRef, baseline, tempPassword, password)
	cleaned = true

	scanEvidenceForSecrets(t, evidenceDir, tempPassword, password)
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
