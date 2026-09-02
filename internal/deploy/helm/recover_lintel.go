package helm

// Lintel recovery orchestration on Kubernetes (T35, OPS-HELPER-005):
// the issuer is a distroless-safe Pod whose main command is the signal
// blocking `--phase hold`; the actual serve phase runs through `kubectl
// exec -i`, so the one-time registration envelope only ever streams to the
// helper's private pipe and never enters pod logs, argv, Secrets or reports.
// The registration one-shot runs through `kubectl run -i --rm` with the
// Lintel state PVC mounted, consuming the envelope on attached stdin.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	deployrecovery "github.com/Suknna/quoin/cmd/quoin-deploy/recover_lintel"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
)

const (
	helmRecoveryStageFence    = "recovery-stop-fence"
	helmRecoveryStageRegister = "recovery-register"
	helmRecoveryStageFinalize = "recovery-finalize"
	helmRecoveryStageRestart  = "recovery-restart"
)

type helmRecoveryEvidence struct {
	fenceReport       map[string]any
	disposition       map[string]any
	recoveryReport    map[string]any
	postVerify        map[string]any
	fenceDigest       string
	dispositionDigest string
	recoveryDigest    string
	postVerifyDigest  string
}

func helmCanonicalSHA(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func writeHelmEvidence(stateDir, name string, record map[string]any) {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Join(stateDir, "reports"), 0o700)
	_ = os.WriteFile(filepath.Join(stateDir, "reports", name), append(raw, '\n'), 0o600)
}

// renderRecoveryIssuerPod renders the temporary Quoin recovery Pod. It
// carries the exact release quoin service labels so the frozen Lintel
// runtime endpoint (`https://quoin:8443`) resolves to it while the normal
// Deployment is scaled to zero; the main command only blocks on the pod
// termination signal (distroless image: no shell, no sleep).
func renderRecoveryIssuerPod(release, quoinImage string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s-lintel-recovery
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s, app.kubernetes.io/component: quoin, quoin.io/role: lintel-recovery}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 30
  securityContext: {runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532, fsGroupChangePolicy: OnRootMismatch, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: recovery
      image: %[2]s
      imagePullPolicy: IfNotPresent
      command: ["/quoin"]
      args: ["maintenance", "recover-lintel", "--phase", "hold", "--config", "/etc/quoin/component.yaml"]
      ports:
        - {name: runtime, containerPort: 8443}
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: secrets, mountPath: /run/quoin-secrets, readOnly: true}
        - {name: tmp, mountPath: /tmp}
  volumes:
    - name: config
      configMap: {name: %[1]s-component-quoin}
    - name: data
      persistentVolumeClaim: {claimName: %[1]s-quoin-data}
    - name: secrets
      secret: {secretName: %[1]s-secrets}
    - {name: tmp, emptyDir: {}}
`, release, quoinImage)
}

// renderRecoveryRegisterHolderPod renders the one-shot Lintel registration
// holder: the Debian-based Lintel image provides the shell its own frozen
// extract-pod precedent uses, so the container simply stays alive while the
// actual `lintel register` runs through `kubectl exec -i` — the envelope
// only ever streams through the private exec channel, never pod logs or
// argv.
func renderRecoveryRegisterHolderPod(release, lintelImage string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s-lintel-register
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s, quoin.io/role: lintel-recovery-register}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 30
  securityContext: {runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532, fsGroupChangePolicy: OnRootMismatch, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: register
      image: %[2]s
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "trap '' TERM; while :; do sleep 30; done"]
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: state, mountPath: /var/lib/lintel}
        - {name: runtime-ca, mountPath: /run/quoin-secrets/runtime-ca.pem, subPath: runtime-ca.pem, readOnly: true}
  volumes:
    - name: config
      configMap: {name: %[1]s-component-lintel}
    - name: state
      persistentVolumeClaim: {claimName: %[1]s-lintel-state}
    - name: runtime-ca
      secret: {secretName: %[1]s-secrets}
`, release, lintelImage)
}

// RecoverLintel runs the stopped Kubernetes recovery sequence.
func RecoverLintel(req Request, flags deployrecovery.Flags) int {
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "recover-lintel", err)
	}
	if loaded.manifest == nil {
		return inputFailure(req, "recover-lintel", fmt.Errorf("the Helm backend requires --release-manifest"))
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "recover-lintel", err)
	}
	if err := stateDirectory(loaded.stateDir); err != nil {
		return inputFailure(req, "recover-lintel", err)
	}
	rep := report.New("helm", buildPlatform(), "recover-lintel", req.ConfigPath, digest)
	rep.Release = loaded.release()
	r := newRunner(rep)
	namespace := helmNamespace()
	release := helmReleaseName()
	defer func() { _ = rep.Finish(reportTarget(req, loaded.stateDir)) }()

	if flags.Phase == deployrecovery.PhaseSetup {
		stage := rep.BeginStage("recovery-preflight")
		if output, runErr := r.run(stage, "helm-version", "helm", "version", "--short"); runErr != nil {
			rep.FailStage(stage, strings.TrimSpace(output))
			rep.MarkFailed("helm_unavailable", strings.TrimSpace(output), "install Helm and retry")
			return 1
		}
		if output, runErr := r.run(stage, "kubectl-version", "kubectl", "version", "--client=false", "--output=yaml"); runErr != nil {
			rep.FailStage(stage, strings.TrimSpace(output))
			rep.MarkFailed("cluster_unreachable", strings.TrimSpace(output), "provide a reachable Kubernetes context and retry")
			return 1
		}
		rep.CompleteStage(stage, "cluster reachable; recovery inputs validated")
		rep.MarkSucceeded()
		return 0
	}
	if flags.Phase == deployrecovery.PhaseAssert {
		stage := rep.BeginStage("recovery-assert")
		if verifyErr := verifyOperationalSurface(req, r, rep, loaded, namespace, release, stage); verifyErr != nil {
			rep.FailStage(stage, verifyErr.Error())
			rep.MarkFailed(verifyErr.code, verifyErr.Error(), "inspect the reported check, then rerun recover-lintel --phase assert")
			return 1
		}
		rep.CompleteStage(stage, "operational surface verified after recovery")
		rep.MarkSucceeded()
		return 0
	}

	preflightStage := rep.BeginStage(helmRecoveryStageFence)
	target, targetErr := helmTargetIdentity(r, preflightStage, namespace, release)
	if targetErr != nil {
		rep.MarkFailed("kubernetes_target_unreadable", targetErr.Error(), "check the active Kubernetes context and namespace, then rerun")
		return 1
	}
	key := deployconfig.InstallStateKey{ReleaseVersion: loaded.release(), Backend: "helm", ConfigDigest: digest, Command: "recover-lintel/" + flags.StorageDisposition, TargetIdentity: target}
	state, stateErr := deployconfig.LoadInstallState(loaded.stateDir)
	if stateErr != nil {
		rep.MarkFailed("install_state_unreadable", stateErr.Error(), "fix or remove the helper state under XDG_STATE_HOME, then rerun")
		return 1
	}
	resume := map[string]bool{}
	if state != nil {
		if state.Key != key {
			if state.FinishedAt == "" {
				rep.MarkFailed("install_state_mismatch", "a partially completed recovery is pending for different inputs", "finish it with those inputs, or remove its state after cleanup")
				return 2
			}
			state = nil
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
			fmt.Fprintf(req.Stderr, "quoin-deploy: persist recovery state: %v\n", writeErr)
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

	// Stop fence: scale every release Deployment to zero and prove the pods
	// are gone before any PVC is touched.
	stage := preflightStage
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		if output, runErr := r.run(stage, "recovery-scale-"+component, kubectl(namespace, "scale", "deployment/"+release+"-"+component, "--replicas=0")...); runErr != nil {
			return failStage(stage, "recovery_stop_failed", strings.TrimSpace(output), "keep all workloads stopped and retry")
		}
		if output, runErr := r.run(stage, "recovery-wait-"+component, kubectl(namespace, "wait", "--for=delete", "pod", "-l", componentSelector(release, component), "--timeout=180s")...); runErr != nil {
			return failStage(stage, "recovery_fence_failed", strings.TrimSpace(output), "prove all four workloads are stopped before recovery")
		}
	}
	pvc := release + "-lintel-state"
	pvcJSON, pvcErr := r.run(stage, "recovery-storage-authority", kubectl(namespace, "get", "pvc", pvc, "--output=json")...)
	if pvcErr != nil {
		return failStage(stage, "recovery_storage_unreadable", strings.TrimSpace(pvcJSON), "inspect the retained Lintel state PVC, then rerun")
	}
	var pvcView struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			VolumeName string `json:"volumeName"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(pvcJSON), &pvcView); err != nil {
		return failStage(stage, "recovery_storage_unreadable", err.Error(), "inspect the retained Lintel state PVC, then rerun")
	}

	evidence := &helmRecoveryEvidence{
		fenceReport: map[string]any{
			"backend": "helm", "release": loaded.release(), "fenced": true,
			"components": []string{"quoin", "plinth", "lintel", "stele"},
			"namespace":  namespace,
		},
		disposition: map[string]any{
			"backend": "helm", "release": loaded.release(),
			"disposition": flags.StorageDisposition,
			"pvc_name":    pvcView.Metadata.Name, "pvc_volume": pvcView.Spec.VolumeName,
			"pvc_phase": pvcView.Status.Phase,
		},
	}
	evidence.fenceDigest = helmCanonicalSHA(evidence.fenceReport)
	evidence.dispositionDigest = helmCanonicalSHA(evidence.disposition)
	evidence.recoveryReport = map[string]any{
		"backend": "helm", "release": loaded.release(),
		"disposition": flags.StorageDisposition, "fence_report_digest": evidence.fenceDigest,
		"registration": "attached-stdin one-time exchange", "replacement_first_hello": "required",
	}
	evidence.recoveryDigest = helmCanonicalSHA(evidence.recoveryReport)
	evidence.postVerify = map[string]any{
		"backend": "helm", "release": loaded.release(),
		"workload_fence": evidence.fenceDigest, "storage_authority": pvcView.Metadata.Name,
		"first_auth_required_before_finalize": true, "app_lock": "exclusive offline finalizer",
	}
	evidence.postVerifyDigest = helmCanonicalSHA(evidence.postVerify)
	writeHelmEvidence(loaded.stateDir, "recovery-fence-report.json", evidence.fenceReport)
	writeHelmEvidence(loaded.stateDir, "recovery-disposition.json", evidence.disposition)
	writeHelmEvidence(loaded.stateDir, "recovery-report.json", evidence.recoveryReport)
	writeHelmEvidence(loaded.stateDir, "recovery-post-verify.json", evidence.postVerify)
	rep.CompleteStage(stage, "all workloads stopped; Lintel state PVC authority recorded")
	if !recordStage(helmRecoveryStageFence) {
		return failStage(stage, "install_state_unwritable", "the recovery retry state could not be persisted", "fix state directory permissions, then rerun the same command")
	}

	issuerPod := release + "-lintel-recovery"
	if !resume[helmRecoveryStageRegister] {
		stage = rep.BeginStage(helmRecoveryStageRegister)
		if _, delErr := r.run(stage, "recovery-issuer-delete", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", issuerPod)...); delErr != nil {
			return failStage(stage, "recovery_issuer_failed", delErr.Error(), "inspect the recovery Pod and rerun")
		}
		if output, runErr := r.runInput(stage, "recovery-issuer-apply", renderRecoveryIssuerPod(release, loaded.images["quoin"]), kubectl(namespace, "apply", "--filename", "-")...); runErr != nil {
			return failStage(stage, "recovery_issuer_failed", strings.TrimSpace(output), "inspect the recovery Pod events and rerun")
		}
		if output, runErr := r.run(stage, "recovery-issuer-ready", kubectl(namespace, "wait", "--for=condition=Ready", "pod/"+issuerPod, "--timeout=180s")...); runErr != nil {
			return failStage(stage, "recovery_issuer_failed", strings.TrimSpace(output), "inspect the recovery Pod events and rerun")
		}
		if err := helmRegistrationPipe(req, r, stage, namespace, release, loaded, flags, evidence); err != nil {
			return failStage(stage, platformCode(err), err.Error(), "keep workloads fenced and rerun; the issuer resumes without a new token")
		}
		rep.CompleteStage(stage, "replacement registered; first authenticated Hello observed by the issuer")
		if _, delErr := r.run(stage, "recovery-issuer-cleanup", kubectl(namespace, "delete", "--wait=true", "pod", issuerPod)...); delErr != nil {
			return failStage(stage, "recovery_cleanup_failed", delErr.Error(), "remove the recovery Pod, then rerun the same command")
		}
		if !recordStage(helmRecoveryStageRegister) {
			return failStage(stage, "install_state_unwritable", "the recovery retry state could not be persisted", "fix state directory permissions, then rerun the same command")
		}
	} else {
		stage = rep.BeginStage(helmRecoveryStageRegister)
		rep.CompleteStage(stage, "registration already completed in the persisted retry state; replay skipped")
	}
	if !resume[helmRecoveryStageFinalize] {
		stage = rep.BeginStage(helmRecoveryStageFinalize)
		finalizePod := release + "-lintel-finalize"
		overrides, overridesErr := renderRecoveryFinalizeOverrides(release, loaded.images["quoin"], flags, evidence)
		if overridesErr != nil {
			return failStage(stage, "recovery_finalize_failed", overridesErr.Error(), "rerun finalization with the same evidence")
		}
		if output, runErr := r.run(stage, "recovery-finalize-run", kubectl(namespace, "run", finalizePod, "--image="+loaded.images["quoin"], "--restart=Never", "--overrides", overrides)...); runErr != nil {
			return failStage(stage, "recovery_finalize_failed", strings.TrimSpace(output), "keep workloads stopped and retry finalization with the same evidence")
		}
		if output, runErr := r.run(stage, "recovery-finalize-wait", kubectl(namespace, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/"+finalizePod, "--timeout=180s")...); runErr != nil {
			return failStage(stage, "recovery_finalize_failed", strings.TrimSpace(output), "keep workloads stopped and retry finalization with the same evidence")
		}
		if output, runErr := r.run(stage, "recovery-finalize-logs", kubectl(namespace, "logs", finalizePod)...); runErr != nil {
			return failStage(stage, "recovery_finalize_failed", strings.TrimSpace(output), "keep workloads stopped and retry finalization with the same evidence")
		}
		if _, delErr := r.run(stage, "recovery-finalize-cleanup", kubectl(namespace, "delete", "--wait=true", "pod", finalizePod)...); delErr != nil {
			return failStage(stage, "recovery_cleanup_failed", delErr.Error(), "remove the finalize Pod, then rerun the same command")
		}
		rep.CompleteStage(stage, "immutable recovery receipt committed after first authenticated Hello")
		if !recordStage(helmRecoveryStageFinalize) {
			return failStage(stage, "install_state_unwritable", "the recovery retry state could not be persisted", "fix state directory permissions, then rerun the same command")
		}
	} else {
		stage = rep.BeginStage(helmRecoveryStageFinalize)
		rep.CompleteStage(stage, "finalization already completed in the persisted retry state; replay skipped")
	}

	stage = rep.BeginStage(helmRecoveryStageRestart)
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		if output, runErr := r.run(stage, "recovery-restart-"+component, kubectl(namespace, "scale", "deployment/"+release+"-"+component, "--replicas=1")...); runErr != nil {
			return failStage(stage, "recovery_restart_failed", strings.TrimSpace(output), "restart remaining workloads then run recover-lintel --phase assert")
		}
	}
	if waitErr := awaitHealthy(r, stage, namespace, release, healthyTimeout); waitErr != nil {
		return failStage(stage, "recovery_health_failed", waitErr.Error(), "inspect recovery logs and rerun the assert phase")
	}
	if verifyErr := verifyOperationalSurface(req, r, rep, loaded, namespace, release, stage); verifyErr != nil {
		return failStage(stage, verifyErr.code, verifyErr.Error(), "inspect the reported check, then rerun the assert phase")
	}
	rep.CompleteStage(stage, "operational surface verified; recovery receipt and replacement first-auth closed the action")
	state.FinishedAt = nowRFC3339()
	if writeErr := state.WriteInstallState(loaded.stateDir); writeErr != nil {
		return failStage(stage, "install_state_unwritable", "the final recovery state could not be persisted: "+writeErr.Error(), "fix state directory permissions; the recovery itself already completed")
	}
	rep.MarkSucceeded()
	return 0
}

// helmRegistrationPipe drives the Kubernetes token exchange with the same
// deadlock-free session split as the Compose backend: the short `issue`
// exec session mints the token, streams the single envelope and exits once
// the Register RPC consumes it (stdin EOF reaches the register one-shot
// independently); the long `await` exec session serves the ordinary Connect
// handshake while the scaled-up Lintel deployment performs its real Hello.
func helmRegistrationPipe(req Request, r *runner, stage int, namespace, release string, loaded *loadedRequest, flags deployrecovery.Flags, evidence *helmRecoveryEvidence) error {
	issuerPod := release + "-lintel-recovery"
	baseArgs := []string{"maintenance", "recover-lintel", "--config", "/etc/quoin/component.yaml", "--backend", "helm", "--storage-disposition", flags.StorageDisposition, "--disposition-digest", evidence.dispositionDigest, "--fence-report-digest", evidence.fenceDigest}
	issueCommand := append([]string{"exec", "-i", issuerPod, "--", "/quoin"}, baseArgs...)
	issueCommand = append(issueCommand, "--phase", "issue", "--first-auth-timeout", flags.FirstAuthTimeout.String())
	issueArgs := kubectl(namespace, issueCommand...)

	envelopeReader, envelopeWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		return &recoveryError{code: "recovery_issuer_start_failed", message: pipeErr.Error()}
	}
	issueErrReader, issueErrWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		_ = envelopeReader.Close()
		_ = envelopeWriter.Close()
		return &recoveryError{code: "recovery_issuer_start_failed", message: pipeErr.Error()}
	}
	issue := exec.Command(issueArgs[0], issueArgs[1:]...)
	issue.Stdout = envelopeWriter
	issue.Stderr = issueErrWriter
	issueStarted := time.Now()
	if err := issue.Start(); err != nil {
		_ = envelopeWriter.Close()
		_ = envelopeReader.Close()
		_ = issueErrWriter.Close()
		_ = issueErrReader.Close()
		return &recoveryError{code: "recovery_issuer_start_failed", message: err.Error()}
	}
	_ = envelopeWriter.Close()
	_ = issueErrWriter.Close()

	events := bufio.NewReader(issueErrReader)
	firstLine := make(chan string, 1)
	firstErr := make(chan error, 1)
	go func() {
		line, readErr := events.ReadString('\n')
		firstLine <- line
		firstErr <- readErr
	}()
	var first string
	select {
	case first = <-firstLine:
		if err := <-firstErr; err != nil {
			_ = issue.Process.Kill()
			_ = issue.Wait()
			_ = envelopeReader.Close()
			_ = issueErrReader.Close()
			return &recoveryError{code: "recovery_issuer_start_failed", message: fmt.Sprintf("issuer did not report its serving state: %v", err)}
		}
	case <-time.After(90 * time.Second):
		_ = issue.Process.Kill()
		_ = issue.Wait()
		_ = envelopeReader.Close()
		_ = issueErrReader.Close()
		return &recoveryError{code: "recovery_issuer_start_failed", message: "issuer did not report its serving state within 90s"}
	}
	var serving struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(first), &serving); err != nil || serving.Code != "lintel_recovery.serving" {
		issueRun := issue.Wait()
		_ = envelopeReader.Close()
		rest, _ := io.ReadAll(events)
		_ = issueErrReader.Close()
		return &recoveryError{code: "recovery_issuer_start_failed", message: fmt.Sprintf("issue session failed (%v): %s", issueRun, strings.TrimSpace(first+string(rest)))}
	}
	needsRegistration := strings.Contains(first, `"needs_registration":true`)

	if needsRegistration {
		registerPod := release + "-lintel-register"
		if _, delErr := r.run(stage, "recovery-register-delete", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", registerPod)...); delErr != nil {
			_ = issue.Process.Kill()
			_ = issue.Wait()
			_ = envelopeReader.Close()
			_ = issueErrReader.Close()
			return &recoveryError{code: "recovery_register_failed", message: delErr.Error()}
		}
		if output, runErr := r.runInput(stage, "recovery-register-apply", renderRecoveryRegisterHolderPod(release, loaded.images["lintel"]), kubectl(namespace, "apply", "--filename", "-")...); runErr != nil {
			_ = issue.Process.Kill()
			_ = issue.Wait()
			_ = envelopeReader.Close()
			_ = issueErrReader.Close()
			return &recoveryError{code: "recovery_register_failed", message: strings.TrimSpace(output)}
		}
		if output, runErr := r.run(stage, "recovery-register-ready", kubectl(namespace, "wait", "--for=condition=Ready", "pod/"+registerPod, "--timeout=180s")...); runErr != nil {
			_ = issue.Process.Kill()
			_ = issue.Wait()
			_ = envelopeReader.Close()
			_ = issueErrReader.Close()
			_, _ = r.run(stage, "recovery-register-cleanup", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", registerPod)...)
			return &recoveryError{code: "recovery_register_failed", message: strings.TrimSpace(output)}
		}
		registerArgs := kubectl(namespace, "exec", "-i", registerPod, "--", "/lintel", "register", "--config", "/etc/quoin/component.yaml")
		var regErr bytes.Buffer
		register := exec.Command(registerArgs[0], registerArgs[1:]...)
		register.Stdin = envelopeReader
		register.Stderr = &regErr
		register.Stdout = io.Discard
		registerStarted := time.Now()
		if err := register.Start(); err != nil {
			_ = envelopeReader.Close()
			_ = issue.Process.Kill()
			_ = issue.Wait()
			_ = issueErrReader.Close()
			_, _ = r.run(stage, "recovery-register-cleanup", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", registerPod)...)
			return &recoveryError{code: "recovery_register_start_failed", message: err.Error()}
		}
		issueRun := issue.Wait()
		registerRun := register.Wait()
		_ = envelopeReader.Close()
		r.report.RecordCommand(stage, report.Command{Argv: issueArgs, ExitCode: commandExitCode(issue), Duration: time.Since(issueStarted).Round(time.Millisecond).String()})
		r.report.RecordCommand(stage, report.Command{Argv: registerArgs, ExitCode: commandExitCode(register), Duration: time.Since(registerStarted).Round(time.Millisecond).String()})
		rest, _ := io.ReadAll(events)
		_ = issueErrReader.Close()
		if _, delErr := r.run(stage, "recovery-register-cleanup", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", release+"-lintel-register")...); delErr != nil {
			return &recoveryError{code: "recovery_cleanup_failed", message: delErr.Error()}
		}
		if issueRun != nil || registerRun != nil {
			return &recoveryError{code: "recovery_registration_failed", message: fmt.Sprintf("issue=%v register=%v; issue events: %s; register stderr: %s", issueRun, registerRun, strings.TrimSpace(first+string(rest)), strings.TrimSpace(regErr.String()))}
		}
	} else {
		issueRun := issue.Wait()
		_ = envelopeReader.Close()
		r.report.RecordCommand(stage, report.Command{Argv: issueArgs, ExitCode: commandExitCode(issue), Duration: time.Since(issueStarted).Round(time.Millisecond).String()})
		rest, _ := io.ReadAll(events)
		_ = issueErrReader.Close()
		if issueRun != nil {
			return &recoveryError{code: "recovery_issuer_start_failed", message: fmt.Sprintf("issue session failed (%v): %s", issueRun, strings.TrimSpace(first+string(rest)))}
		}
	}

	awaitErrReader, awaitErrWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		return &recoveryError{code: "recovery_issuer_start_failed", message: pipeErr.Error()}
	}
	awaitCommand := append([]string{"exec", "-i", issuerPod, "--", "/quoin"}, baseArgs...)
	awaitCommand = append(awaitCommand, "--phase", "await", "--first-auth-timeout", flags.FirstAuthTimeout.String())
	awaitArgs := kubectl(namespace, awaitCommand...)
	await := exec.Command(awaitArgs[0], awaitArgs[1:]...)
	await.Stdout = io.Discard
	await.Stderr = awaitErrWriter
	awaitStarted := time.Now()
	if err := await.Start(); err != nil {
		_ = awaitErrWriter.Close()
		_ = awaitErrReader.Close()
		return &recoveryError{code: "recovery_issuer_start_failed", message: err.Error()}
	}
	_ = awaitErrWriter.Close()
	if output, runErr := r.run(stage, "recovery-start-lintel", kubectl(namespace, "scale", "deployment/"+release+"-lintel", "--replicas=1")...); runErr != nil {
		_ = await.Process.Kill()
		_ = await.Wait()
		_ = awaitErrReader.Close()
		return &recoveryError{code: "recovery_lintel_start_failed", message: strings.TrimSpace(output)}
	}
	awaitRun := await.Wait()
	awaitEvents, _ := io.ReadAll(awaitErrReader)
	_ = awaitErrReader.Close()
	r.report.RecordCommand(stage, report.Command{Argv: awaitArgs, ExitCode: commandExitCode(await), Duration: time.Since(awaitStarted).Round(time.Millisecond).String()})
	if awaitRun != nil {
		return &recoveryError{code: "recovery_first_hello_missing", message: strings.TrimSpace(string(awaitEvents))}
	}
	if !strings.Contains(string(awaitEvents), "lintel_recovery.first_authenticated") {
		return &recoveryError{code: "recovery_first_hello_missing", message: "await session exited without recording the first authenticated Hello"}
	}
	return nil
}

// renderRecoveryFinalizeOverrides builds the finalize one-shot Pod: same
// volume identity as the recovery issuer, main command is the offline
// finalizer (its output is the non-secret summary line).
func renderRecoveryFinalizeOverrides(release, quoinImage string, flags deployrecovery.Flags, evidence *helmRecoveryEvidence) (string, error) {
	overrides := map[string]any{
		"apiVersion": "v1",
		"spec": map[string]any{
			"restartPolicy":                 "Never",
			"automountServiceAccountToken":  false,
			"terminationGracePeriodSeconds": 30,
			"securityContext": map[string]any{
				"runAsNonRoot": true, "runAsUser": 65532, "fsGroup": 65532,
				"fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []any{map[string]any{
				"name":  "finalize",
				"image": quoinImage,
				"command": []string{"/quoin", "maintenance", "recover-lintel", "--phase", "finalize", "--config", "/etc/quoin/component.yaml",
					"--storage-disposition", flags.StorageDisposition,
					"--disposition-digest", evidence.dispositionDigest,
					"--fence-report-digest", evidence.fenceDigest,
					"--recovery-report-digest", evidence.recoveryDigest,
					"--post-verify-digest", evidence.postVerifyDigest},
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
					"capabilities": map[string]any{"drop": []string{"ALL"}},
				},
				"volumeMounts": []any{
					map[string]any{"name": "config", "mountPath": "/etc/quoin/component.yaml", "subPath": "component.yaml", "readOnly": true},
					map[string]any{"name": "data", "mountPath": "/var/lib/quoin/data"},
					map[string]any{"name": "secrets", "mountPath": "/run/quoin-secrets", "readOnly": true},
					map[string]any{"name": "tmp", "mountPath": "/tmp"},
				},
			}},
			"volumes": []any{
				map[string]any{"name": "config", "configMap": map[string]any{"name": release + "-component-quoin"}},
				map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": release + "-quoin-data"}},
				map[string]any{"name": "secrets", "secret": map[string]any{"secretName": release + "-secrets"}},
				map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
			},
		},
	}
	encoded, err := json.Marshal(overrides)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

type recoveryError struct {
	code    string
	message string
}

func (e *recoveryError) Error() string { return e.message }

func platformCode(err error) string {
	var recovery *recoveryError
	if errors.As(err, &recovery) {
		return recovery.code
	}
	return "recovery_failed"
}

func commandExitCode(command *exec.Cmd) int {
	if command == nil || command.ProcessState == nil {
		return -1
	}
	return command.ProcessState.ExitCode()
}
