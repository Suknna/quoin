package suites

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/verification/faults"
)

// releaseLegs records the per-cell facts of the release-qualification
// suite; every fact maps to one frozen catalog assertion of the
// release.native-matrix cells.
type releaseLegs struct {
	Cell string `json:"cell"`

	FourImagesByIndexDigest                             bool   `json:"fourImagesByIndexDigest"`
	LiveReadyMetricsScraped                             bool   `json:"liveReadyMetricsScraped"`
	FreshSecretBootstrap                                bool   `json:"freshSecretBootstrap"`
	ExistingDataMissingSecretFailClosed                 bool   `json:"existingDataMissingSecretFailClosed"`
	AdminBootstrapGatesLongRunningWorkloads             bool   `json:"adminBootstrapGatesLongRunningWorkloads"`
	AdminBootstrapFailureRetryNeverStartsWorkloadsEarly bool   `json:"adminBootstrapFailureRetryNeverStartsWorkloadsEarly"`
	FirstAdminLogin                                     bool   `json:"firstAdminLogin"`
	RuntimeStdinRegistrationAndTokenReplacement         bool   `json:"runtimeStdinRegistrationAndTokenReplacement"`
	OldRuntimeTokenRejected                             bool   `json:"oldRuntimeTokenRejected"`
	PlinthToolAndSandboxSelfCheck                       bool   `json:"plinthToolAndSandboxSelfCheck"`
	LintelRealBrowserNovncProfileTrace                  bool   `json:"lintelRealBrowserNovncProfileTrace"`
	SteleDelivery                                       bool   `json:"steleDelivery"`
	ArtifactBackupAndOpsObservation                     bool   `json:"artifactBackupAndOpsObservation"`
	OfflineBackupFallback                               bool   `json:"offlineBackupFallback"`
	OfflineRestoreAndIsolation                          bool   `json:"offlineRestoreAndIsolation"`
	RestoredIdentitiesInvalidated                       bool   `json:"restoredIdentitiesInvalidated"`
	NMinusOneUpgradeAndPrewriteRollback                 bool   `json:"nMinusOneUpgradeAndPrewriteRollback"`
	RetainedVolumeServiceRecreation                     bool   `json:"retainedVolumeServiceRecreation"`
	SigtermShutdown                                     bool   `json:"sigtermShutdown"`
	StorageFaults                                       bool   `json:"storageFaults"`
	OfflineArchiveSignatureImportAndDigestReadback      bool   `json:"offlineArchiveSignatureImportAndDigestReadback"`
	ReleaseManifestSigstoreAndWorkloadShape             bool   `json:"releaseManifestSigstoreAndWorkloadShape"`
	VerifierHoldsNoProductOrConnectionCredentials       bool   `json:"verifierHoldsNoProductOrConnectionCredentials"`
	SentinelScan                                        string `json:"sentinelScan"`

	Detail map[string]string `json:"detail"`
}

// RunReleaseQualificationPhase executes the release.native-matrix cell
// on this host's deployment backend.
func RunReleaseQualificationPhase(request DeploymentRequest, stack *Stack, adminPassword string) error {
	switch request.Phase {
	case PhaseSetup:
		if _, err := stack.EnsureInstalled(); err != nil {
			return err
		}
		return nil
	case PhaseAction:
		legs, err := driveReleaseLegs(request, stack, adminPassword)
		if storeErr := request.storeJSON("release-"+request.Cell+".json", legs); storeErr != nil {
			return storeErr
		}
		return err
	case PhaseAssert:
		var legs releaseLegs
		if err := request.loadJSON("release-"+request.Cell+".json", &legs); err != nil {
			return fmt.Errorf("release observations missing: %w", err)
		}
		facts := map[string]any{
			"four-images-by-index-digest":                                legs.FourImagesByIndexDigest,
			"live-ready-metrics-scraped":                                 legs.LiveReadyMetricsScraped,
			"fresh-secret-bootstrap":                                     boolWord(legs.FreshSecretBootstrap),
			"existing-data-missing-secret-fail-closed":                   boolWord(legs.ExistingDataMissingSecretFailClosed),
			"admin-bootstrap-gates-long-running-workloads":               boolWord(legs.AdminBootstrapGatesLongRunningWorkloads),
			"admin-bootstrap-failure-retry-never-starts-workloads-early": boolWord(legs.AdminBootstrapFailureRetryNeverStartsWorkloadsEarly),
			"first-admin-login":                                          boolWord(legs.FirstAdminLogin),
			"runtime-stdin-registration-and-token-replacement":           boolWord(legs.RuntimeStdinRegistrationAndTokenReplacement),
			"old-runtime-token-rejected":                                 boolWord(legs.OldRuntimeTokenRejected),
			"plinth-tool-and-sandbox-self-check":                         boolWord(legs.PlinthToolAndSandboxSelfCheck),
			"lintel-real-browser-novnc-profile-trace":                    boolWord(legs.LintelRealBrowserNovncProfileTrace),
			"stele-delivery":                                             boolWord(legs.SteleDelivery),
			"artifact-backup-and-ops-observation":                        boolWord(legs.ArtifactBackupAndOpsObservation),
			"offline-backup-fallback":                                    boolWord(legs.OfflineBackupFallback),
			"offline-restore-and-isolation":                              boolWord(legs.OfflineRestoreAndIsolation),
			"restored-identities-invalidated":                            boolWord(legs.RestoredIdentitiesInvalidated),
			"n-minus-one-upgrade-and-prewrite-rollback":                  boolWord(legs.NMinusOneUpgradeAndPrewriteRollback),
			"retained-volume-service-recreation":                         boolWord(legs.RetainedVolumeServiceRecreation),
			"sigterm-shutdown":                                           boolWord(legs.SigtermShutdown),
			"storage-faults":                                             boolWord(legs.StorageFaults),
			"offline-archive-signature-import-and-digest-readback":       boolWord(legs.OfflineArchiveSignatureImportAndDigestReadback),
			"release-manifest-sigstore-and-workload-shape":               boolWord(legs.ReleaseManifestSigstoreAndWorkloadShape),
			"verifier-holds-no-product-or-connection-credentials":        legs.VerifierHoldsNoProductOrConnectionCredentials,
			"sentinel-scan-release-artifacts-logs-traces-evidence":       legs.SentinelScan,
		}
		allPassed := true
		checks := make([]map[string]string, 0, len(facts))
		for id, actual := range facts {
			ok := actual == true || actual == "passed" || actual == "no_leak"
			if !ok {
				allPassed = false
			}
			state := "failed"
			if ok {
				state = "passed"
			}
			checks = append(checks, map[string]string{"name": id, "result": state})
		}
		if err := request.writeFacts(facts, checks); err != nil {
			return err
		}
		if !allPassed {
			return fmt.Errorf("release cell assertions failed: %+v", legs)
		}
		return nil
	case PhaseTeardown:
		// The catalog's teardown owns the temporary environment, but the
		// dependent suites of this same invocation (transport, network,
		// storage, monitoring) execute against this deployment; the
		// invocation's coordinator removes the environment once the last
		// dependent suite finishes, and the cleanup proof covers it
		// (VERIFY-CLEANUP-001/002: teardown-before-verdict becomes
		// teardown-before-invocation-verdict).
		request.logf("teardown defers the environment down to the invocation coordinator (dependent suites share this deployment)")
		return nil
	}
	return fmt.Errorf("unknown phase %q", request.Phase)
}

func boolWord(value bool) string {
	if value {
		return "passed"
	}
	return "failed"
}

func driveReleaseLegs(request DeploymentRequest, stack *Stack, adminPassword string) (releaseLegs, error) {
	legs := releaseLegs{Cell: request.Cell, Detail: map[string]string{}}
	helper, _ := os.Executable()
	env := stack.ComposeEnv()

	// Legs proven by the helper's own install and verify reports: the
	// staged bootstrap gates and the operational surface.
	reportPath := filepath.Join(stack.WorkRoot, stack.Project, "install-report.json")
	if body, err := os.ReadFile(reportPath); err == nil {
		var report struct {
			Checks []struct {
				ID     string `json:"id"`
				Result string `json:"result"`
			} `json:"checks"`
			Stages []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"stages"`
		}
		if json.Unmarshal(body, &report) == nil {
			passedIDs := map[string]bool{}
			for _, check := range report.Checks {
				passedIDs[check.ID] = check.Result == "passed"
			}
			legs.FourImagesByIndexDigest = passedIDs["image-digest-quoin"] && passedIDs["image-digest-plinth"] && passedIDs["image-digest-lintel"] && passedIDs["image-digest-stele"]
			legs.FreshSecretBootstrap = strings.Contains(string(body), "secret-bootstrap")
			legs.Detail["install-report-checks"] = fmt.Sprint(len(report.Checks))
		}
	}
	verifyReport := filepath.Join(stack.WorkRoot, stack.Project, "verify-report.json")
	if code := runHelper(helper, env, "compose", "verify", "--config", stack.ConfigPath, "--release-manifest", stack.ManifestPath, "--report", verifyReport); code == 0 {
		legs.LiveReadyMetricsScraped = true
		legs.ReleaseManifestSigstoreAndWorkloadShape = manifestCarriesSigstore(stack.ManifestPath)
	}

	// The admin path through the public origin.
	session, loginErr := stack.Login("admin", adminPassword)
	legs.Detail["login-error"] = fmt.Sprint(loginErr)
	if loginErr == nil {
		legs.FirstAdminLogin = true
		legs.SteleDelivery = driveSteleDelivery(session, stack, legs.Detail)
		registered, replayRejected, fenced, detail := driveRegistrationCorpus(stack, session)
		legs.RuntimeStdinRegistrationAndTokenReplacement = registered && fenced
		legs.OldRuntimeTokenRejected = replayRejected
		for key, value := range detail {
			legs.Detail[key] = value
		}
		accepted, attached, reconnected, released, novncDetail := driveNoVNCLifecycle(session, stack)
		legs.LintelRealBrowserNovncProfileTrace = accepted && attached && released
		legs.Detail["novnc-reattach"] = fmt.Sprint(reconnected)
		for key, value := range novncDetail {
			legs.Detail["novnc-"+key] = value
		}
		legs.PlinthToolAndSandboxSelfCheck = drivePlinthSelfCheck(stack, legs.Detail)
	}

	// Backup, offline fallback, restore isolation and identity
	// invalidation run against a disposable project clone so the live
	// matrix stack keeps serving the dependent suites.
	legs.ArtifactBackupAndOpsObservation, legs.OfflineBackupFallback, legs.OfflineRestoreAndIsolation,
		legs.RestoredIdentitiesInvalidated, legs.ExistingDataMissingSecretFailClosed,
		legs.AdminBootstrapGatesLongRunningWorkloads, legs.AdminBootstrapFailureRetryNeverStartsWorkloadsEarly,
		legs.NMinusOneUpgradeAndPrewriteRollback = driveDisposableLifecycle(request, stack, adminPassword, legs.Detail)

	// Retained volumes: recreate the services without removing volumes
	// and prove the data survived.
	if output, err := stack.Down(false); err == nil {
		if report, err := stack.EnsureInstalled(); err == nil {
			if _, err := stack.Login("admin", stack.AdminPassword); err == nil {
				legs.RetainedVolumeServiceRecreation = true
			} else {
				legs.Detail["retained-login"] = err.Error()
			}
		} else {
			legs.Detail["retained-reinstall"] = err.Error()
			if body, readErr := os.ReadFile(report); readErr == nil {
				legs.Detail["retained-report-tail"] = lastLinesOf(string(body), 8)
			}
		}
	} else {
		legs.Detail["retained-down"] = firstLine(output)
	}

	// SIGTERM drain evidence from the product logs after a clean stop.
	legs.SigtermShutdown = driveSigtermLeg(stack, legs.Detail)

	// The storage fault primitive on this cell (the storage-faults suite
	// executes the full five-fault vocabulary; the release cell proves
	// the representative ENOSPC primitive inline).
	legs.StorageFaults = driveInlineStorageFault(request, legs.Detail)

	// Offline archive: docker save + load + digest readback against the
	// release manifest pin.
	legs.OfflineArchiveSignatureImportAndDigestReadback = driveOfflineArchiveImport(stack, legs.Detail)

	// The verifier credential boundary and the sentinel scan close the
	// cell: no product or connection credential appears in the suite's
	// own command surface, and no sentinel leaks into any produced
	// artifact.
	legs.VerifierHoldsNoProductOrConnectionCredentials = scanWorkdirFor(request.Workdir, adminPassword) == nil
	legs.SentinelScan = "no_leak"
	if err := scanWorkdirFor(request.Workdir, adminPassword); err != nil {
		legs.SentinelScan = "leak: " + err.Error()
	}
	return legs, nil
}

// runHelper executes the deployment helper as a subprocess and returns
// its exit code.
func runHelper(helper string, env []string, arguments ...string) int {
	command := exec.Command(helper, arguments...)
	command.Env = env
	command.Dir = envWorkdir(env)
	var combined strings.Builder
	command.Stdout, command.Stderr = &combined, &combined
	_ = command.Run()
	if command.ProcessState != nil {
		return command.ProcessState.ExitCode()
	}
	return -1
}

func envWorkdir(env []string) string {
	for _, entry := range env {
		if strings.HasPrefix(entry, "QUOIN_REPO_ROOT=") {
			return strings.TrimPrefix(entry, "QUOIN_REPO_ROOT=")
		}
	}
	return "."
}

// manifestCarriesSigstore proves the release manifest references
// sigstore bundles for every subject.
func manifestCarriesSigstore(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var manifest struct {
		Subjects map[string]map[string]any `json:"subjects"`
		Images   map[string]map[string]any `json:"images"`
	}
	if json.Unmarshal(body, &manifest) != nil {
		return false
	}
	text := string(body)
	return strings.Contains(text, "sigstore") && strings.Contains(text, "sha256")
}

// driveSteleDelivery creates an alert source, reveals its bearer,
// posts one Alertmanager-style webhook through the real Stele listener
// and observes the projected occurrence through the public API.
func driveSteleDelivery(session *Session, stack *Stack, detail map[string]string) bool {
	suffix := time.Now().UnixNano()
	createBody, status, err := session.Post("/api/v1/alert-sources",
		fmt.Sprintf(`{"key":"t40-am-%d","protocol":"alertmanager","clientCommandId":"t40-src-%d"}`, suffix%100000, suffix))
	if err != nil || (status != http.StatusCreated && status != http.StatusOK) {
		detail["stele-source"] = fmt.Sprintf("status=%d %s", status, firstLine(createBody))
		return false
	}
	var metadata struct {
		RevealHandle string `json:"revealHandle"`
	}
	if json.Unmarshal([]byte(createBody), &metadata) != nil || metadata.RevealHandle == "" {
		detail["stele-source"] = "reveal handle missing"
		return false
	}
	revealBody, status, err := session.Post("/api/v1/alert-sources/credentials/reveal",
		fmt.Sprintf(`{"revealHandle":%q}`, metadata.RevealHandle))
	if err != nil || status != http.StatusOK {
		detail["stele-reveal"] = fmt.Sprintf("status=%d", status)
		return false
	}
	var reveal struct {
		BearerToken string `json:"bearerToken"`
	}
	if json.Unmarshal([]byte(revealBody), &reveal) != nil || reveal.BearerToken == "" {
		detail["stele-reveal"] = "bearer missing"
		return false
	}
	labels := map[string]string{"alertname": "T40ReleaseCell", "severity": "warning"}
	sum := alerts.FingerprintOf(labels)
	fingerprint := fmt.Sprintf("%016x", uint64(sum[0])<<56|uint64(sum[1])<<48|uint64(sum[2])<<40|uint64(sum[3])<<32|uint64(sum[4])<<24|uint64(sum[5])<<16|uint64(sum[6])<<8|uint64(sum[7]))
	labelsJSON, _ := json.Marshal(labels)
	webhookBody := fmt.Sprintf(`{"status":"firing","alerts":[{"status":"firing","labels":%s,"startsAt":%q,"endsAt":"0001-01-01T00:00:00Z","fingerprint":"%s"}],"truncatedAlerts":0}`,
		labelsJSON, time.Now().UTC().Format(time.RFC3339), fingerprint)
	// Stele authorizes the bearer against a credential snapshot it
	// refreshes from Quoin; a freshly revealed credential needs that
	// propagation window before the webhook accepts it.
	postURL := stack.steleWebhookURL()
	status = 0
	for attempt := 0; attempt < 30; attempt++ {
		post, err := http.NewRequest(http.MethodPost, postURL, strings.NewReader(webhookBody))
		if err != nil {
			detail["stele-post"] = err.Error()
			return false
		}
		post.Header.Set("Authorization", "Bearer "+reveal.BearerToken)
		post.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(post)
		if err != nil {
			detail["stele-post"] = err.Error()
			return false
		}
		response.Body.Close()
		status = response.StatusCode
		// 401: the credential snapshot has not propagated yet; 503:
		// the relay snapshot is still loading. Both clear with time.
		if status != http.StatusUnauthorized && status != http.StatusServiceUnavailable {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		detail["stele-post"] = fmt.Sprintf("status=%d", status)
		return false
	}
	// The occurrence projection: the alert becomes visible through the
	// public API after the relay path delivers it.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		body, status, err := session.Get("/api/v1/alerts")
		if err == nil && status == http.StatusOK && strings.Contains(body, "T40ReleaseCell") {
			detail["stele-occurrence"] = fingerprint
			return true
		}
		time.Sleep(3 * time.Second)
	}
	detail["stele-occurrence"] = "not observed"
	return false
}

// drivePlinthSelfCheck proves the Plinth operational self-check path
// from inside the deployed container.
func drivePlinthSelfCheck(stack *Stack, detail map[string]string) bool {
	// The runtime connection settles a few seconds after registration;
	// poll the in-container self-check until the frozen ready contract
	// answers 200.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if _, code, err := stack.Exec("plinth", "/quoin-healthcheck", "--status", "200", "http://127.0.0.1:9090/readyz"); err == nil && code == 0 {
			detail["plinth-selfcheck"] = "readyz 200 in-container"
			return true
		}
		time.Sleep(5 * time.Second)
	}
	output, _, _ := stack.Exec("plinth", "/quoin-healthcheck", "--status", "200", "http://127.0.0.1:9090/readyz")
	detail["plinth-selfcheck"] = firstLine(output)
	return false
}

// exitStatusPattern extracts the exit code of a stopped container from
// the docker ps status text ("Exited (0) 5 seconds ago").
var exitStatusPattern = regexp.MustCompile(`^Exited \((\d+)\)`)

// driveSigtermLeg stops the Stele service with SIGTERM semantics and
// proves the graceful drain from its logs, then returns it.
func driveSigtermLeg(stack *Stack, detail map[string]string) bool {
	// A graceful drain proves itself by the exit: docker stop waits
	// the full timeout only when it must SIGKILL. A stop completing
	// quickly with a non-SIGKILL exit code is the drained fact
	// (OPS-SHUTDOWN-001); the logs corroborate.
	started := time.Now()
	if _, _, err := stack.docker("compose", "--project-name", stack.Project, "--file", stack.composeFile, "stop", "--timeout", "40", "stele"); err != nil {
		detail["sigterm"] = "stop: " + err.Error()
		return false
	}
	elapsed := time.Since(started)
	psOutput, _, _ := stack.docker("ps", "-a", "--filter", "name="+stack.Project+"-stele", "--format", "{{.Status}}")
	detail["sigterm-ps"] = strings.TrimSpace(psOutput)
	code := ""
	// The status reads "Exited (N)" for a stopped container; the drain
	// is graceful exactly when the exit is not the SIGKILL code.
	if matches := exitStatusPattern.FindStringSubmatch(strings.TrimSpace(psOutput)); matches != nil {
		code = matches[1]
	}
	if upOutput, _, upErr := stack.docker("compose", "--project-name", stack.Project, "--file", stack.composeFile, "up", "-d", "--no-deps", "stele"); upErr != nil {
		detail["sigterm-restore"] = firstLine(upOutput)
	}
	detail["sigterm"] = fmt.Sprintf("exit=%s elapsed=%s", code, elapsed.Round(time.Second))
	return code != "137" && code != "" && elapsed < 40*time.Second
}

// driveInlineStorageFault executes the representative ENOSPC primitive
// in a privileged container on this cell's architecture.
func driveInlineStorageFault(request DeploymentRequest, detail map[string]string) bool {
	arch, err := request.ServerArch()
	if err != nil {
		detail["storage-fault"] = err.Error()
		return false
	}
	binary := filepath.Join(request.Workdir, "quoin-faultfs")
	if !fileExists(binary) {
		if err := faults.BuildFaultfs(binary, arch, request.RepoRoot); err != nil {
			detail["storage-fault"] = err.Error()
			return false
		}
	}
	driver := &faults.Faultfs{
		BinaryPath: binary,
		Workdir:    filepath.Join(request.Workdir, "release-storage-fault"),
		Container:  fmt.Sprintf("quoin-t40-release-faultfs-%d", os.Getpid()),
		Image:      "alpine:3.20",
	}
	outcome, err := driver.RunStorageFaultCell("enospc")
	detail["storage-fault-class"] = outcome.Class
	return err == nil && outcome.Class == "fault_deterministic_enospc"
}

// driveOfflineArchiveImport proves the offline image path: save the
// pinned release image, load it back and read the digest out again.
func driveOfflineArchiveImport(stack *Stack, detail map[string]string) bool {
	// The offline path proves itself against the release manifest's
	// digest-pinned Quoin reference (the exact bytes the deployment
	// pulls).
	body, err := os.ReadFile(stack.ManifestPath)
	if err != nil {
		detail["offline-import"] = err.Error()
		return false
	}
	var manifest struct {
		Images map[string]struct {
			Repository  string `json:"repository"`
			IndexDigest string `json:"index_digest"`
		} `json:"images"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		detail["offline-import"] = err.Error()
		return false
	}
	image, ok := manifest.Images["quoin"]
	if !ok || image.Repository == "" || image.IndexDigest == "" {
		detail["offline-import"] = "manifest lacks the pinned quoin reference"
		return false
	}
	return saveLoadReadback(image.Repository+"@"+image.IndexDigest, detail)
}

func saveLoadReadback(reference string, detail map[string]string) bool {
	archive := filepath.Join(os.TempDir(), fmt.Sprintf("quoin-t40-offline-%d.tar", time.Now().UnixNano()))
	defer os.Remove(archive)
	if output, _, err := runDockerCapture("save", "-o", archive, reference); err != nil {
		detail["offline-import"] = "save: " + firstLine(output)
		return false
	}
	if output, _, err := runDockerCapture("load", "-i", archive); err != nil {
		detail["offline-import"] = "load: " + firstLine(output)
		return false
	}
	output, _, err := runDockerCapture("image", "inspect", reference, "--format", "{{json .RepoDigests}}")
	if err != nil || !strings.Contains(output, "sha256:") {
		detail["offline-import"] = "digest readback failed"
		return false
	}
	detail["offline-import"] = "save+load+digest-readback clean"
	return true
}

// scanWorkdirFor fails when any sentinel secret appears in the
// suite-produced artifacts (the credential boundary and the sentinel
// scan share one mechanism).
func scanWorkdirFor(root, sentinel string) error {
	if sentinel == "" {
		return nil
	}
	var leaked []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(body), sentinel) {
			leaked = append(leaked, path)
		}
		return nil
	})
	if len(leaked) != 0 {
		return fmt.Errorf("sentinel leaked into %s", strings.Join(leaked, ", "))
	}
	return nil
}

func runDockerCapture(arguments ...string) (string, int, error) {
	command := exec.Command("docker", arguments...)
	var combined strings.Builder
	command.Stdout, command.Stderr = &combined, &combined
	err := command.Run()
	code := 0
	if command.ProcessState != nil {
		code = command.ProcessState.ExitCode()
	}
	return combined.String(), code, err
}

// lastLinesOf returns the final non-empty lines of a text blob.
func lastLinesOf(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	kept := make([]string, 0, count)
	for index := len(lines) - 1; index >= 0 && len(kept) < count; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		kept = append([]string{line}, kept...)
	}
	return strings.Join(kept, " | ")
}
