package helm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	deploycompose "github.com/Suknna/quoin/deploy/compose"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

const (
	stagePreflight       = "preflight"
	stageRetainedVolumes = "retained-volumes"
	stageSecretBootstrap = "secret-bootstrap"
	stageAdminBootstrap  = "admin-bootstrap"
	stageWorkloads       = "workloads"
	stageVerify          = "verify"

	exitSuccess = 0
	exitInput   = 2
)

// helmChartRef reads the digest-pinned OCI chart reference from the release
// manifest (the only chart authority; mutable tags are never accepted).
func helmChartRef(manifest *deployconfig.ReleaseManifest) (string, error) {
	var chart struct {
		OCIRepository string `json:"oci_repository"`
		OCIDigest     string `json:"oci_digest"`
	}
	if err := json.Unmarshal(manifest.Helm, &chart); err != nil {
		return "", fmt.Errorf("release manifest helm section: %w", err)
	}
	if chart.OCIRepository == "" || !strings.HasPrefix(chart.OCIDigest, "sha256:") {
		return "", fmt.Errorf("release manifest helm section needs oci_repository and a sha256 oci_digest")
	}
	return "oci://" + chart.OCIRepository + "@" + chart.OCIDigest, nil
}

// chartValues projects the validated input and the digest-pinned images into
// chart values. Secret material never enters this file.
func chartValues(input installInput, images map[string]string) ([]byte, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var projected map[string]any
	if err := json.Unmarshal(encoded, &projected); err != nil {
		return nil, err
	}
	delete(projected, "document")
	return yaml.Marshal(map[string]any{"input": projected, "images": images})
}

// stackRunning reports whether the long-lived Quoin workload already exists.
func stackRunning(r *runner, stage int, namespace, release string) bool {
	_, err := r.run(stage, "stack-probe", kubectl(namespace, "get", "deployment", release+"-quoin")...)
	return err == nil
}

// Install runs the staged Helm installation. Each completed stage is persisted
// (OPS-HELPER-002): a retry with the same identity resumes from the last
// completed stage; a changed identity with a pending partial install refuses.
func Install(req Request) (exitCode int) {
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "install", err)
	}
	if loaded.manifest == nil {
		return inputFailure(req, "install", fmt.Errorf("the Helm backend requires --release-manifest (images and the OCI chart digest are release-owned)"))
	}
	chartRef, err := helmChartRef(loaded.manifest)
	if err != nil {
		return inputFailure(req, "install", err)
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "install", err)
	}
	if err := stateDirectory(loaded.stateDir); err != nil {
		return inputFailure(req, "install", err)
	}
	rep := report.New("helm", buildPlatform(), "install", req.ConfigPath, digest)
	rep.Release = loaded.release()
	r := newRunner(rep)
	preflightStage := rep.BeginStage(stagePreflight)
	namespace := helmNamespace()
	release := helmReleaseName()
	// Every exit path writes the atomic report (OPS-HELPER-003).
	defer func() { _ = rep.Finish(reportTarget(req, loaded.stateDir)) }()

	target, targetErr := helmTargetIdentity(r, preflightStage, namespace, release)
	if targetErr != nil {
		rep.MarkFailed("kubernetes_target_unreadable", targetErr.Error(), "check the active Kubernetes context and namespace, then rerun")
		return 1
	}
	key := deployconfig.InstallStateKey{ReleaseVersion: loaded.release(), Backend: "helm", ConfigDigest: digest, Command: "install", TargetIdentity: target}
	state, stateErr := deployconfig.LoadInstallState(loaded.stateDir)
	if stateErr != nil {
		rep.MarkFailed("install_state_unreadable", stateErr.Error(), "fix or remove the install state under XDG_STATE_HOME, then rerun")
		return 1
	}
	resume := map[string]bool{}
	if state != nil {
		if state.Key != key {
			if state.FinishedAt == "" {
				rep.MarkFailed("install_state_mismatch",
					fmt.Sprintf("a partially completed install is pending for different inputs (release %s, config digest %s); finish it with those inputs, or remove %s after cleaning up that deployment",
						state.Key.ReleaseVersion, shortDigest(state.Key.ConfigDigest), filepath.Join(loaded.stateDir, "install-state.json")),
					"rerun with the pending inputs or clean up the partial deployment first")
				rep.ExitCode = exitInput
				return exitInput
			}
			state = nil
		} else if state.FinishedAt != "" {
			// A completed historical install is not a partial retry. Re-run the
			// idempotent stages so a Helm-uninstalled release rechecks the
			// retained Secret and confirms the existing administrator before it
			// recreates workloads.
			state = &deployconfig.InstallState{Key: key}
		} else {
			for _, stage := range state.StagesDone {
				resume[stage] = true
			}
		}
	}
	if state == nil {
		state = &deployconfig.InstallState{Key: key}
	}
	recordStage := func(stage string) bool {
		state.StagesDone = append(state.StagesDone, stage)
		if writeErr := state.WriteInstallState(loaded.stateDir); writeErr != nil {
			fmt.Fprintf(req.Stderr, "quoin-deploy: persist install state: %v\n", writeErr)
			return false
		}
		return true
	}
	failStage := func(stage int, code, message, next string) int {
		rep.FailStage(stage, message)
		rep.MarkFailed(code, message, next)
		fmt.Fprintf(req.Stderr, "quoin-deploy: %s: %s\nnext action: %s\n", code, message, next)
		return 1
	}

	// Scripted bootstrap answers are read before any child drains stdin.
	forceScripted := os.Getenv("QUOIN_DEPLOY_SCRIPTED") == "1"
	var scripted *deploycompose.AdminAnswers
	if forceScripted || !isTerminal(req.Stdin) {
		answers, answersErr := adminAnswersFromStdin(req.Stdin)
		if answersErr != nil {
			return inputFailure(req, "install", answersErr)
		}
		scripted = &answers
	}

	stage := preflightStage
	if output, runErr := r.run(stage, "helm-version", "helm", "version", "--short"); runErr != nil {
		return failStage(stage, "helm_unavailable", strings.TrimSpace(output), "install Helm and retry")
	}
	if output, runErr := r.run(stage, "kubectl-version", "kubectl", "version", "--client=false", "--output=yaml"); runErr != nil {
		return failStage(stage, "cluster_unreachable", strings.TrimSpace(output), "provide a reachable Kubernetes context and retry")
	}
	if output, runErr := r.runInput(stage, "namespace-ensure",
		"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+namespace+"\n  labels: {app.kubernetes.io/part-of: quoin}\n",
		kubectl(namespace, "apply", "--filename", "-")...); runErr != nil {
		return failStage(stage, "namespace_unwritable", strings.TrimSpace(output), "fix cluster access, then rerun the same command")
	}
	running := stackRunning(r, stage, namespace, release)
	rep.CompleteStage(stage, fmt.Sprintf("Helm and Kubernetes reachable; existing release running=%t", running))
	if !recordStage(stagePreflight) {
		return failStage(stage, "install_state_unwritable", "the install retry state could not be persisted", "fix state directory permissions under XDG_STATE_HOME, then rerun")
	}

	// Retained PVCs: helper-owned; `helm uninstall` never removes them.
	// Binding is intentionally not awaited here: the default local-path
	// provisioner binds WaitForFirstConsumer, so the claims surface once the
	// bootstrap Job and workloads mount them; the verify stage proves all
	// four Bound.
	stage = rep.BeginStage(stageRetainedVolumes)
	if output, runErr := r.runInput(stage, "pvc-apply", renderRetainedPVCs(release, loaded.input), kubectl(namespace, "apply", "--filename", "-")...); runErr != nil {
		return failStage(stage, "retained_volumes_failed", strings.TrimSpace(output), "inspect PVC provisioning and storage class, then rerun the same command")
	}
	rep.CompleteStage(stage, "retained PVCs applied: data, backups, plinth state, lintel state (binding follows first consumer)")
	if !recordStage(stageRetainedVolumes) {
		return failStage(stage, "install_state_unwritable", "the install retry state could not be persisted", "fix state directory permissions under XDG_STATE_HOME, then rerun")
	}

	// Deployment secrets: one-shot in-cluster bootstrap into the retained
	// Kubernetes Secret (OPS-SECRET-003).
	stage = rep.BeginStage(stageSecretBootstrap)
	secretPresent, _ := secretExists(r, stage, namespace, release)
	// The retained Kubernetes Secret is the authority: present and complete
	// means the deployment secret set is active, regardless of local retry
	// state (a fresh helper state against an existing deployment must resume
	// from the cluster, not regenerate secrets over an existing database).
	if running || secretPresent {
		rep.CompleteStage(stage, "existing deployment secret set already active")
	} else {
		if err := runSecretBootstrap(r, stage, namespace, release, loaded.input, loaded.images); err != nil {
			return failStage(stage, "secret_bootstrap_failed", err.Error(), "fix the reported bootstrap error; persistent state was preserved; rerun the same command")
		}
		if cleanupErr := cleanupBootstrap(r, stage, namespace, release); cleanupErr != nil {
			return failStage(stage, "bootstrap_cleanup_failed", cleanupErr.Error(), "remove the reported disposable bootstrap resource, then rerun")
		}
		rep.CompleteStage(stage, "deployment secret set created in the retained Kubernetes Secret")
	}
	if !recordStage(stageSecretBootstrap) {
		return failStage(stage, "install_state_unwritable", "the install retry state could not be persisted", "fix state directory permissions under XDG_STATE_HOME, then rerun")
	}

	// First administrator through the attached-TTY pod (OPS-PACKAGE-003).
	stage = rep.BeginStage(stageAdminBootstrap)
	if running {
		rep.CompleteStage(stage, "existing administrator and persistent data preserved")
	} else if resume[stageAdminBootstrap] {
		rep.CompleteStage(stage, "stage already completed in the persisted retry state")
	} else {
		pod := renderAdminBootstrap(release, loaded.images["quoin"], loaded.images["plinth"], loaded.input.PublicOrigin)
		if _, err := r.run(stage, "admin-pod-delete", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", release+"-admin-bootstrap", "configmap", release+"-admin-bootstrap-config")...); err != nil {
			return failStage(stage, "admin_bootstrap_failed", err.Error(), "inspect cluster state, then rerun the same command")
		}
		if output, runErr := r.runInput(stage, "admin-pod-apply", pod, kubectl(namespace, "apply", "--filename", "-")...); runErr != nil {
			return failStage(stage, "admin_bootstrap_failed", strings.TrimSpace(output), "inspect cluster state, then rerun the same command")
		}
		attached, waitErr := awaitAdminReadyOrSuccess(r, stage, namespace, release, 120*time.Second)
		if waitErr != nil {
			return failStage(stage, "admin_bootstrap_failed", waitErr.Error(), "inspect the admin pod events and logs; rerun the same command")
		}
		if attached {
			var adminErr error
			if scripted == nil {
				adminErr = runAdminBootstrapInteractive(req, r, stage, namespace, release)
			} else {
				adminErr = runAdminBootstrapScripted(req, r, stage, namespace, release, *scripted)
			}
			if adminErr != nil {
				return failStage(stage, "admin_bootstrap_failed", adminErr.Error(), "fix the reported administrator bootstrap error; persistent state was preserved; rerun the same command")
			}
		}
		if cleanupErr := cleanupBootstrap(r, stage, namespace, release); cleanupErr != nil {
			return failStage(stage, "bootstrap_cleanup_failed", cleanupErr.Error(), "remove the reported disposable bootstrap resource, then rerun")
		}
		if markerErr := writeBootstrapComplete(r, stage, namespace, release); markerErr != nil {
			return failStage(stage, "bootstrap_marker_failed", markerErr.Error(), "inspect the bootstrap ConfigMap and rerun")
		}
		if attached {
			rep.CompleteStage(stage, "first administrator created or confirmed")
		} else {
			rep.CompleteStage(stage, "existing administrator confirmed")
		}
	}
	if running || resume[stageAdminBootstrap] {
		if markerErr := requireBootstrapComplete(r, stage, namespace, release); markerErr != nil {
			return failStage(stage, "bootstrap_marker_missing", markerErr.Error(), "do not bypass bootstrap; restore the matching helper-owned marker or rerun a clean install")
		}
	}
	if !recordStage(stageAdminBootstrap) {
		return failStage(stage, "install_state_unwritable", "the install retry state could not be persisted", "fix state directory permissions under XDG_STATE_HOME, then rerun")
	}

	// Long-lived workloads from the immutable OCI chart digest.
	stage = rep.BeginStage(stageWorkloads)
	values, valuesErr := chartValues(loaded.input, loaded.images)
	if valuesErr != nil {
		return failStage(stage, "values_render_failed", valuesErr.Error(), "fix deployment input")
	}
	valuesPath := filepath.Join(loaded.stateDir, "values.yaml")
	if writeErr := os.WriteFile(valuesPath, values, 0o600); writeErr != nil {
		return failStage(stage, "values_write_failed", writeErr.Error(), "fix state directory permissions, then rerun")
	}
	// Do not use Helm --wait: Plinth and Lintel correctly remain
	// Kubernetes-not-ready until the operator performs their separate Runtime
	// registration with a revealed one-time token (OPS-RUNTIME-REG-001).
	// awaitHealthy below waits for the closed install-ready state instead.
	if output, runErr := r.run(stage, "helm-upgrade-install",
		"helm", "upgrade", "--install", release, chartRef,
		"--namespace", namespace, "--create-namespace",
		"--values", valuesPath); runErr != nil {
		return failStage(stage, "workloads_not_started", strings.TrimSpace(output), "inspect the Helm release (`helm status`, `kubectl describe`), then rerun the same command")
	}
	if waitErr := awaitHealthy(r, stage, namespace, release, healthyTimeout); waitErr != nil {
		return failStage(stage, "workloads_not_healthy", waitErr.Error(), "inspect component logs; rerun the same command to resume")
	}
	rep.CompleteStage(stage, "quoin and stele available; plinth and lintel Running pending separate Runtime registration")
	if !recordStage(stageWorkloads) {
		return failStage(stage, "install_state_unwritable", "the install retry state could not be persisted", "fix state directory permissions under XDG_STATE_HOME, then rerun")
	}

	// Post-install verification reuses the standalone verifier
	// (OPS-HELPER-004).
	stage = rep.BeginStage(stageVerify)
	if verifyErr := verifyOperationalSurface(req, r, rep, loaded, namespace, release, stage); verifyErr != nil {
		return failStage(stage, verifyErr.code, verifyErr.Error(), "inspect the reported check, then rerun the same command")
	}
	rep.CompleteStage(stage, "operational surface verified: readiness, metrics, logs, topology, image digests")
	if !recordStage(stageVerify) {
		return failStage(stage, "install_state_unwritable", "the install retry state could not be persisted", "fix state directory permissions under XDG_STATE_HOME, then rerun")
	}
	state.FinishedAt = nowRFC3339()
	if writeErr := state.WriteInstallState(loaded.stateDir); writeErr != nil {
		fmt.Fprintf(req.Stderr, "quoin-deploy: persist install state: %v\n", writeErr)
	}
	rep.MarkSucceeded()
	fmt.Fprintf(req.Stdout, "Quoin is running on Kubernetes. Public Origin: %s\n", loaded.input.PublicOrigin)
	fmt.Fprintf(req.Stdout, "Helm release: %s (namespace %s)\n", release, namespace)
	return exitSuccess
}

// Verify runs the read-only operational-surface verifier (OPS-HELPER-004).
func Verify(req Request) (exitCode int) {
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "verify", err)
	}
	if loaded.manifest == nil {
		return inputFailure(req, "verify", fmt.Errorf("the Helm backend requires --release-manifest"))
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "verify", err)
	}
	if err := stateDirectory(loaded.stateDir); err != nil {
		return inputFailure(req, "verify", err)
	}
	rep := report.New("helm", buildPlatform(), "verify", req.ConfigPath, digest)
	rep.Release = loaded.release()
	r := newRunner(rep)
	defer func() { _ = rep.Finish(reportTarget(req, loaded.stateDir)) }()
	namespace := helmNamespace()
	release := helmReleaseName()
	stage := rep.BeginStage(stageVerify)
	if verifyErr := verifyOperationalSurface(req, r, rep, loaded, namespace, release, stage); verifyErr != nil {
		rep.FailStage(stage, verifyErr.Error())
		rep.MarkFailed(verifyErr.code, verifyErr.Error(), "inspect the reported check, then rerun verify")
		return 1
	}
	rep.CompleteStage(stage, "operational surface verified: readiness, metrics, logs, topology, image digests")
	rep.MarkSucceeded()
	return exitSuccess
}

func inputFailure(req Request, command string, err error) int {
	rep := report.New("helm", buildPlatform(), command, req.ConfigPath, "")
	rep.MarkFailed("invalid_input", err.Error(), "fix the deployment input; no deployment side effect has occurred")
	rep.ExitCode = exitInput
	_ = rep.Finish(reportTarget(req, ""))
	fmt.Fprintf(req.Stderr, "quoin-deploy: %v\n", err)
	return exitInput
}

func reportTarget(req Request, stateDir string) string {
	if req.ReportPath != "" {
		return req.ReportPath
	}
	if stateDir != "" {
		return defaultReportPath(stateDir)
	}
	return "verification-report.json"
}

func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

const healthyTimeout = 5 * time.Minute

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// helmNamespace selects the deployment namespace (stable override surface).
func helmNamespace() string {
	if namespace := os.Getenv("QUOIN_HELM_NAMESPACE"); namespace != "" {
		return namespace
	}
	return "quoin"
}

// helmReleaseName selects the Helm release name.
func helmReleaseName() string {
	if release := os.Getenv("QUOIN_HELM_RELEASE"); release != "" {
		return release
	}
	return "quoin"
}

// isTerminal reports whether the reader is an interactive terminal, so the
// admin bootstrap can choose between interactive attach and scripted answers.
func isTerminal(reader io.Reader) bool {
	if file, ok := reader.(*os.File); ok {
		return term.IsTerminal(int(file.Fd()))
	}
	return false
}

// helmTargetIdentity binds resumable install state to the actual Kubernetes
// target, not merely to process-local config. A prior partial install can only
// resume against the same API server/cluster UID, namespace and release.
func helmTargetIdentity(r *runner, stage int, namespace, release string) (string, error) {
	server, err := r.run(stage, "target-server", "kubectl", "config", "view", "--minify", "--raw", "--output", "jsonpath={.clusters[0].cluster.server}")
	if err != nil || strings.TrimSpace(server) == "" {
		return "", fmt.Errorf("read Kubernetes API server: %s", strings.TrimSpace(server))
	}
	clusterUID, err := r.run(stage, "target-cluster-uid", "kubectl", "get", "namespace", "kube-system", "--output", "jsonpath={.metadata.uid}")
	if err != nil || strings.TrimSpace(clusterUID) == "" {
		return "", fmt.Errorf("read Kubernetes cluster identity: %s", strings.TrimSpace(clusterUID))
	}
	return strings.Join([]string{strings.TrimSpace(server), strings.TrimSpace(clusterUID), namespace, release}, "|"), nil
}

// awaitAdminReadyOrSuccess distinguishes a prompt-ready bootstrap pod from
// the deliberate no-prompt success path used when retained state already has
// an administrator. kubectl wait alone cannot make that distinction and would
// unnecessarily spend its full timeout on every reinstall.
func awaitAdminReadyOrSuccess(r *runner, stage int, namespace, release string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := r.run(stage, "admin-pod-status", kubectl(namespace, "get", "pod", release+"-admin-bootstrap", "--output", "jsonpath={.status.phase} {.status.containerStatuses[0].ready}")...)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		fields := strings.Fields(status)
		if len(fields) > 0 && fields[0] == "Succeeded" {
			return false, nil
		}
		if len(fields) > 0 && fields[0] == "Failed" {
			return false, judgeAdminOutcome(Request{Stdout: io.Discard}, r, stage, namespace, release, "", "")
		}
		if len(fields) > 1 && fields[1] == "true" {
			return true, nil
		}
		time.Sleep(time.Second)
	}
	return false, fmt.Errorf("admin bootstrap pod did not become ready or complete within %s", timeout)
}
