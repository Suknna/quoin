package release_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// proveSIGTERMDrain exercises the frozen drain contract (OPS-HEALTH-006,
// OPS-SHUTDOWN-001): after SIGTERM, /readyz flips to 503 draining while
// /livez keeps answering 200, and the process exits well inside the 60s
// stop grace period. The sampler is one shell child driving engine-level
// `docker exec` probes: per-call `compose exec` consults service state and
// refuses while the container is stopping, which hides the drained window.
func proveSIGTERMDrain(t *testing.T, recorder *evidence, composeFile string) map[string]any {
	t.Helper()
	arguments := composeFileArguments(composeFile)
	container := strings.Fields(strings.TrimSpace(recorder.output(append(arguments, "ps", "-aq", "quoin")...)))[0]
	script := fmt.Sprintf(`for i in $(seq 1 90); do
  R=$(docker exec %[1]s /quoin-healthcheck --status 503 http://127.0.0.1:9090/readyz 2>/dev/null | grep -o '"reason":"[a-z]*"' | head -1)
  L=$(docker exec %[1]s /quoin-healthcheck http://127.0.0.1:9090/livez >/dev/null 2>&1 && echo 200 || echo dead)
  echo "$R $L"
  sleep 0.2
done`, container)
	sampler := exec.Command("bash", "-c", script)
	sampler.Env = os.Environ()
	transcript, err := os.CreateTemp("", "t30-drain-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(transcript.Name())
	sampler.Stdout = transcript
	sampler.Stderr = transcript
	if err := sampler.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	started := time.Now()
	recorder.run("sigterm-stop-quoin", nil, nil, 0, append(arguments, "stop", "quoin")...)
	duration := time.Since(started)
	// Let the sampler observe the post-exit state, then finish it.
	time.Sleep(1500 * time.Millisecond)
	_ = sampler.Process.Kill()
	_ = sampler.Wait()
	data, err := os.ReadFile(transcript.Name())
	if err != nil {
		t.Fatal(err)
	}
	drainingSeen, livezDuringDrain, readyBefore := false, false, false
	var samples []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		samples = append(samples, line)
		draining := strings.Contains(line, `"reason":"draining"`)
		if strings.Contains(line, `"reason":"ready"`) {
			readyBefore = true
		}
		if draining {
			drainingSeen = true
			if strings.HasSuffix(line, " 200") {
				livezDuringDrain = true
			}
		}
	}
	if !readyBefore {
		t.Fatalf("sampler never observed the pre-SIGTERM ready state: %v", samples)
	}
	if !drainingSeen {
		t.Fatalf("SIGTERM drain never observed in %d samples", len(samples))
	}
	if !livezDuringDrain {
		t.Fatalf("livez did not stay 200 while readyz reported draining: %v", samples)
	}
	if duration >= 60*time.Second {
		t.Fatalf("quoin took %s to stop, outside the 60s grace period", duration)
	}
	recorder.note("sigterm-drain-samples.txt", strings.Join(samples, "\n"))
	observations := map[string]any{
		"samples": samples, "stopDurationSeconds": duration.Seconds(),
		"observed": map[string]bool{"readyz_before_sigterm": readyBefore, "readyz_draining": drainingSeen, "livez_200_during_drain": livezDuringDrain},
		"stopExit": "0 (docker compose stop quoin)",
	}
	// Restore the drained component for the remaining proofs.
	recorder.run("quoin-restart", nil, nil, 0, append(arguments, "up", "-d", "quoin")...)
	waitServiceHealthy(t, recorder, composeFile, "quoin", 120*time.Second)
	return observations
}

func waitServiceHealthy(t *testing.T, recorder *evidence, composeFile, service string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, err := exec.Command("docker", append(composeFileArguments(composeFile)[1:], "ps", "--format", "{{.Service}} {{.Health}}", service)...).Output()
		if err == nil && strings.HasPrefix(string(output), service+" healthy") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s did not become healthy again after restart", service)
}

func assertVerifyReport(t *testing.T, recorder *evidence, path string) {
	t.Helper()
	report := loadReport(t, path)
	if report.Command != "verify" || report.ExitCode != 0 {
		t.Fatalf("verify report is not a read-only success: %+v", report)
	}
	for _, check := range report.Checks {
		if check.Result != "passed" {
			t.Fatalf("verify check %s did not pass", check.ID)
		}
	}
	recorder.note("verify-report.json", mustJSON(t, report))
}

// proveRetainedState shows the exact retained-versus-removed semantics:
// `docker compose down` removes containers and networks while the deployment
// state directories and secrets survive, and a reinstall against that state
// confirms (never recreates) the administrator, keeping logins valid.
func proveRetainedState(t *testing.T, recorder *evidence, workRoot, helper, config, manifest, composeFile, password string) map[string]any {
	t.Helper()
	base := fmt.Sprintf("http://127.0.0.1:%d", mainQuoinPort)
	if !loginAsAdmin(t, base, "https://quoin.example.com", "admin", password) {
		t.Fatal("login against the running install failed before retention test")
	}
	stateRoot := filepath.Join(workRoot, mainProject, "state")
	secretDir := filepath.Join(workRoot, "secrets-main")
	recorder.run("retention-down", nil, nil, 0, append(composeFileArguments(composeFile), "down", "--remove-orphans")...)
	listing := recorder.run("retention-ps", nil, nil, 0, append(composeFileArguments(composeFile), "ps", "--all", "--format", "json")...)
	if strings.Contains(listing, `"State":"running"`) {
		t.Fatalf("down left running containers:\n%s", listing)
	}
	retained := map[string]bool{}
	for _, probe := range []string{
		filepath.Join(stateRoot, "quoin", "compose", "data", "quoin.db"),
		filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml"),
		filepath.Join(secretDir, "root-key"),
		filepath.Join(secretDir, "runtime-ca.pem"),
	} {
		if _, err := os.Stat(probe); err != nil {
			t.Fatalf("retained state %s disappeared after down: %v", probe, err)
		}
		retained[probe] = true
	}
	reinstall := strings.NewReader("ignored\nignored\nignored\nignored\n")
	report := filepath.Join(workRoot, "report-reinstall.json")
	recorder.run("retention-reinstall", composeEnv(workRoot, mainProject), reinstall, 0, helper, "compose", "install", "--config", config, "--release-manifest", manifest, "--report", report)
	if !loginAsAdmin(t, base, "https://quoin.example.com", "admin", password) {
		t.Fatal("login with the original administrator password failed after down+reinstall; retained state was lost")
	}
	detail := ""
	for _, stage := range loadReport(t, report).Stages {
		if stage.Name == "admin-bootstrap" {
			detail = stage.Detail
		}
	}
	// The reinstall must confirm (never recreate) the administrator: either
	// the one-shot probe confirms it directly, or the persisted retry state
	// already carries the completed stage. The original login staying valid
	// above is the behavioral proof.
	if !strings.Contains(detail, "confirmed") && !strings.Contains(detail, "already completed") {
		t.Fatalf("reinstall admin stage neither confirmed nor resumed the existing administrator: %q", detail)
	}
	return map[string]any{
		"retainedAfterDown":   retained,
		"removedByDown":       "containers and project networks only",
		"adminAfterReinstall": "original administrator confirmed; original password still valid",
	}
}

// proveInstallRetryState exercises the persisted stage state (OPS-HELPER-002)
// in an isolated project: a deterministic mid-install failure records the
// completed stages, the retry with the same identity resumes past them, and a
// changed config digest against a pending partial install is refused.
func proveInstallRetryState(t *testing.T, recorder *evidence, helper, workRoot, manifest string) map[string]any {
	t.Helper()
	secrets := filepath.Join(workRoot, "secrets-retry")
	config := writeInstallConfig(t, workRoot, "install-retry.yaml", secrets, retryQuoinPort, retryStelePort)
	stateFile := filepath.Join(workRoot, retryProject, "state", "quoin", "compose", "install-state.json")

	// Deterministic failure inside admin-bootstrap: the temporary password
	// violates the frozen password policy, so the one-shot container exits
	// non-zero after the secret stage completed.
	weak := strings.NewReader("admin\nRetry Admin\nshort\nshort\n")
	report := filepath.Join(workRoot, "report-retry-fail.json")
	recorder.run("retry-weak-password", composeEnv(workRoot, retryProject), weak, 1, helper, "compose", "install", "--config", config, "--release-manifest", manifest, "--report", report)
	partial := readInstallState(t, stateFile)
	if !containsStage(partial.StagesDone, "secret-bootstrap") || containsStage(partial.StagesDone, "admin-bootstrap") || partial.FinishedAt != "" {
		t.Fatalf("partial install state wrong after failure: %+v", partial)
	}
	if loadReport(t, report).Failure == nil || loadReport(t, report).Failure.Code != "admin_bootstrap_failed" {
		t.Fatalf("weak-password failure must be admin_bootstrap_failed: %+v", loadReport(t, report).Failure)
	}

	// Same identity retry resumes: the completed secret stage is not re-run.
	password := randomPassword(t)
	answers := strings.NewReader(strings.Join([]string{"admin", "Retry Admin", password, password}, "\n") + "\n")
	resumeReport := filepath.Join(workRoot, "report-retry-resume.json")
	recorder.run("retry-resume", composeEnv(workRoot, retryProject), answers, 0, helper, "compose", "install", "--config", config, "--release-manifest", manifest, "--report", resumeReport)
	resumed := loadReport(t, resumeReport)
	secretDetail := ""
	for _, stage := range resumed.Stages {
		if stage.Name == "secret-bootstrap" {
			secretDetail = stage.Detail
		}
	}
	if !strings.Contains(secretDetail, "already active") {
		t.Fatalf("retry re-ran the completed secret stage instead of resuming: %q", secretDetail)
	}
	finished := readInstallState(t, stateFile)
	if finished.FinishedAt == "" || !containsStage(finished.StagesDone, "verify") {
		t.Fatalf("successful retry did not finish the state: %+v", finished)
	}

	// Changed input against a pending partial install is refused (exit 2)
	// without deployment side effects.
	mismatchSecrets := filepath.Join(workRoot, "secrets-mismatch")
	mismatchConfig := writeInstallConfig(t, workRoot, "install-mismatch.yaml", mismatchSecrets, 19200, 19201)
	weakAgain := strings.NewReader("admin\nMismatch Admin\nshort\nshort\n")
	recorder.run("mismatch-weak", composeEnv(workRoot, mismatchProj), weakAgain, 1, helper, "compose", "install", "--config", mismatchConfig, "--release-manifest", manifest, "--report", filepath.Join(workRoot, "report-mismatch-fail.json"))
	changedConfig := writeInstallConfig(t, workRoot, "install-mismatch2.yaml", mismatchSecrets, 19210, 19211)
	// The partial install legitimately left its completed one-shot state; the
	// refused retry must not add anything to it.
	before := recorder.run("mismatch-containers-before", nil, nil, 0, "docker", "compose", "--project-name", mismatchProj, "ps", "-aq")
	output := recorder.run("mismatch-refused", composeEnv(workRoot, mismatchProj), strings.NewReader(""), 2, helper, "compose", "install", "--config", changedConfig, "--release-manifest", manifest, "--report", filepath.Join(workRoot, "report-mismatch-refused.json"))
	if !strings.Contains(output, "partially completed install is pending") {
		t.Fatalf("changed input against pending state must fail with the stable mismatch error:\n%s", output)
	}
	after := recorder.run("mismatch-containers-after", nil, nil, 0, "docker", "compose", "--project-name", mismatchProj, "ps", "-aq")
	if strings.TrimSpace(before) != strings.TrimSpace(after) || strings.Contains(after, "running") {
		t.Fatalf("refused retry changed the deployment state:\nbefore: %q\nafter: %q", before, after)
	}
	return map[string]any{
		"failureStage":    "admin-bootstrap (password policy)",
		"partialStages":   partial.StagesDone,
		"resumeEvidence":  secretDetail,
		"finishedState":   finished.FinishedAt,
		"mismatchRefusal": "exit 2, stable install_state_mismatch error, no side effects",
	}
}

type installStateFile struct {
	StagesDone []string `json:"stages_completed"`
	FinishedAt string   `json:"finished_at"`
}

func readInstallState(t *testing.T, path string) installStateFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("install state %s missing: %v", path, err)
	}
	var state installStateFile
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func containsStage(stages []string, want string) bool {
	for _, stage := range stages {
		if stage == want {
			return true
		}
	}
	return false
}

// environmentBaseline is the untouched inventory captured before the test
// creates anything.
type environmentBaseline struct {
	containers string
	projects   string
}

func captureEnvironmentBaseline(t *testing.T, recorder *evidence) environmentBaseline {
	t.Helper()
	return environmentBaseline{
		containers: strings.TrimSpace(recorder.output("docker", "ps", "-a", "--format", "{{.ID}}")),
		projects:   recorder.run("baseline-projects", nil, nil, 0, "docker", "compose", "ls", "--all", "--format", "json"),
	}
}

// cleanupTicketResources removes every process, container, network, volume,
// image and fixture this acceptance created and proves pre-existing resources
// are untouched; a cleanup failure fails the ticket even when behavior
// assertions passed.
func cleanupTicketResources(t *testing.T, recorder *evidence, workRoot, registryRef string, baseline environmentBaseline, password string) {
	t.Helper()
	preExistingContainers := baseline.containers
	preExistingProjects := baseline.projects

	dispositions := map[string]string{}
	for _, project := range []string{mainProject, retryProject, mismatchProj} {
		projectCompose := filepath.Join(workRoot, project, "state", "quoin", "compose", "generated", "compose.yaml")
		if _, err := os.Stat(projectCompose); err == nil {
			recorder.run("cleanup-down-"+project, nil, nil, 0, "docker", "compose", "--project-name", project, "--file", projectCompose, "down", "--remove-orphans", "--volumes")
		} else {
			recorder.run("cleanup-down-"+project, nil, nil, 0, "docker", "compose", "--project-name", project, "down", "--remove-orphans", "--volumes")
		}
		dispositions["project:"+project] = "containers, networks and volumes removed by compose down --volumes"
	}
	recorder.run("cleanup-registry", nil, nil, 0, "docker", "rm", "-f", registryName)
	dispositions["fixture:"+registryName] = "registry container removed; pushed test digests removed with it"
	images := []string{}
	for _, component := range []string{"quoin", "stele", "plinth", "lintel"} {
		images = append(images,
			registryHost+"/t30/"+component+":amd64",
			registryHost+"/t30/"+component+":arm64",
		)
	}
	recorder.run("cleanup-images", nil, nil, 0, append([]string{"docker", "rmi", "-f"}, images...)...)
	dispositions["images"] = "locally built test images force-removed; the :index and pushed platform manifests lived only in the disposable registry"

	if remaining := strings.TrimSpace(recorder.output("docker", "ps", "-a", "--format", "{{.ID}} {{.Names}}")); remaining != "" {
		for _, line := range strings.Split(remaining, "\n") {
			if strings.Contains(line, "t30") {
				t.Fatalf("cleanup left a ticket-owned container: %s", line)
			}
		}
	}
	if strings.Contains(recorder.run("cleanup-post-projects", nil, nil, 0, "docker", "compose", "ls", "--all", "--format", "json"), "t30") {
		t.Fatal("cleanup left a ticket-owned compose project")
	}
	if current := strings.TrimSpace(recorder.output("docker", "ps", "-a", "--format", "{{.ID}}")); current != preExistingContainers {
		t.Fatalf("cleanup touched pre-existing containers:\nbefore:\n%s\nafter:\n%s", preExistingContainers, current)
	}
	if after := recorder.run("cleanup-post-projects2", nil, nil, 0, "docker", "compose", "ls", "--all", "--format", "json"); after != preExistingProjects {
		t.Fatalf("compose project inventory changed beyond ticket-owned projects:\nbefore:\n%s\nafter:\n%s", preExistingProjects, after)
	}
	if err := os.RemoveAll(workRoot); err != nil {
		t.Fatalf("remove work root: %v", err)
	}
	dispositions["state-and-reports"] = "temporary state roots, secrets, manifests and reports removed with the work root"
	recorder.observe("cleanup.json", map[string]any{
		"dispositions":         dispositions,
		"preExistingUntouched": true,
		"credentials":          "administrator passwords held only in process memory; never written to evidence",
	})

	finishedAt := time.Now().UTC().Format(time.RFC3339Nano)
	recorder.observe("runtime-evidence.json", map[string]any{
		"gitCommit":        recorder.gitCommit,
		"dirtyStateDigest": recorder.dirtyState,
		"startedAt":        recorder.startedAt.Format(time.RFC3339Nano),
		"finishedAt":       finishedAt,
		"tools":            recorder.toolInfo,
		"commands":         recorder.commands,
		"artifacts":        recorder.artifacts,
		"proofPoints": map[string]string{
			"minimal-schema-input":     "unknown config field and malformed manifest rejected with exit 2 before side effects (invalid-input.log, invalid-manifest.log, compose-ls-after-bad.log)",
			"fixed-ports-volumes":      "verify topology check plus independent container reference assertions (verify-report.json, container-image-references.json)",
			"process-locks":            "second quoin and plinth against live state directories fail with the stable already-owned error (process-locks.json)",
			"sigterm-drain":            "readyz 503 draining with livez 200 during SIGTERM, exit inside the 60s grace (sigterm-drain.json)",
			"metrics-contract":         "install and verify judge the live /metrics output for exact family and closed-label equality with the frozen catalog (verify-report.json)",
			"install-retry-state":      "weak-password failure records completed stages; same-identity retry resumes without re-running them; changed digest against pending state refused (install-retry-state.json)",
			"cleanup-retained-volumes": "down retains state directories and secrets while removing containers/networks; reinstall confirms the original administrator; final cleanup removes exactly the ticket-owned resources (retained-state.json, cleanup.json)",
			"digest-pinned-artifacts":  "four components installed from repository@index digests measured from real dual-platform pushes (release-images.json, container-image-references.json)",
		},
		"disclosures": "the release manifest's non-image sections (helm, compose bundle, helper assets, sigstore names, validation summary) are structural local-test values; the release pipeline owning them is Stage 10 (OPS-RELEASE-001). The lintel image uses the qualified canonical development recipe because the formal locked lintel package set has drifted from Debian 13 (see the ticket findings).",
		"redactions":  "administrator passwords never appear in evidence; host paths under the Go temp root are recorded as-is because the root is removed at teardown",
	})
}

func scanEvidenceForSecrets(t *testing.T, evidenceDir string, secrets ...string) {
	t.Helper()
	err := filepath.Walk(evidenceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, secret := range secrets {
			if secret != "" && strings.Contains(string(data), secret) {
				t.Fatalf("secret material leaked into %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
