package compose

// Recovery orchestration keeps the registration envelope off every captured
// stream and out of the container log driver: the issuer container runs the
// signal-blocking `--phase hold` as its main command, and the serve phase
// runs through `docker exec -i`, whose stream never enters the container
// logs. The helper gates the register one-shot on the issuer's first
// stderr event, so a resumed recovery (replacement already current) never
// starts a register child that would wait for an envelope that is never
// coming. Digest inputs are stable facts only: retries of the same frozen
// recovery revision derive byte-identical digests.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// recoveryEvidence holds the canonical, digest-bearing records. Every field
// is a stable deployment fact: retries of the same recovery must derive
// byte-identical digests or the frozen fence would conflict.
type recoveryEvidence struct {
	fenceReport       map[string]any
	disposition       map[string]any
	recoveryReport    map[string]any
	postVerify        map[string]any
	fenceDigest       string
	dispositionDigest string
	recoveryDigest    string
	postVerifyDigest  string
}

func canonicalSHA(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// writeEvidenceRecord persists the canonical record next to its digest so the
// receipt's digests stay reviewable against durable bytes.
func writeEvidenceRecord(stateDir, name string, record map[string]any) error {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "reports"), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, "reports", name), append(raw, '\n'), 0o600)
}

const (
	recoveryStageFence    = "recovery-stop-fence"
	recoveryStageRegister = "recovery-register"
	recoveryStageFinalize = "recovery-finalize"
)

// recoveryHolderContainer is the fixed name of the detached issuer holder.
const recoveryHolderContainer = "quoin-lintel-recovery"

// RecoverLintel runs the stopped Compose recovery sequence. It never reads or
// logs the registration envelope; the issuer serve stream is a private
// `docker exec -i` pipe endpoint only.
func RecoverLintel(req Request, flags deployrecovery.Flags) int {
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "recover-lintel", err)
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "recover-lintel", &InputError{err.Error()})
	}
	helper, err := newRunner(req, loaded, "recover-lintel", digest)
	if err != nil {
		return inputFailure(req, "recover-lintel", &InputError{err.Error()})
	}
	defer helper.finish(req, loaded)
	if flags.Phase == deployrecovery.PhaseSetup {
		stage := helper.report.BeginStage("recovery-preflight")
		if out, e := helper.run(stage, "compose-version", dockerize(append(append([]string{}, loaded.composeArguments...), "version"))...); e != nil {
			return helper.failPreflight(req, stage, "docker_compose_unavailable", strings.TrimSpace(out))
		}
		if out, e := helper.run(stage, "compose-config", dockerize(append(append([]string{}, loaded.composeArguments...), "config", "--quiet"))...); e != nil {
			return helper.failPreflight(req, stage, "compose_projection_invalid", strings.TrimSpace(out))
		}
		helper.report.CompleteStage(stage, "projection and recovery inputs validated")
		helper.report.MarkSucceeded()
		return ExitSuccess
	}
	if flags.Phase == deployrecovery.PhaseAssert {
		stage := helper.report.BeginStage("recovery-assert")
		if e := verifyOperationalSurface(req, loaded, helper, stage); e != nil {
			return helper.failStage(req, stage, e)
		}
		helper.report.CompleteStage(stage, "operational surface verified after recovery")
		helper.report.MarkSucceeded()
		return ExitSuccess
	}

	// The disposition is part of the retry identity: a completed recovery for
	// one disposition must never be resumed by another (OPS-HELPER-002).
	key := deployconfig.InstallStateKey{ReleaseVersion: loaded.release(), Backend: "compose", ConfigDigest: digest, Command: "recover-lintel/" + flags.StorageDisposition}
	state, stateErr := deployconfig.LoadInstallState(loaded.stateDir)
	if stateErr != nil {
		return inputFailure(req, "recover-lintel", &InputError{stateErr.Error()})
	}
	resume := map[string]bool{}
	if state != nil {
		if state.Key != key {
			if state.FinishedAt == "" {
				return inputFailure(req, "recover-lintel", &InputError{
					"a partially completed recovery is pending for different inputs; finish it with those inputs, or remove its state after cleanup"})
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
			fmt.Fprintf(req.stderr(), "quoin-deploy: persist recovery state: %v\n", writeErr)
			return false
		}
		return true
	}

	lintelState := filepath.Join(loaded.stateDir, "lintel") // render.go fixes this bind path.
	evidence := &recoveryEvidence{
		fenceReport: map[string]any{
			"backend": "compose", "release": loaded.release(), "fenced": true,
			"components":        []string{"quoin", "plinth", "lintel", "stele"},
			"lintel_state_path": lintelState,
		},
		disposition: map[string]any{
			"backend": "compose", "release": loaded.release(),
			"disposition": flags.StorageDisposition, "lintel_state_path": lintelState,
		},
	}
	evidence.fenceDigest = canonicalSHA(evidence.fenceReport)
	evidence.dispositionDigest = canonicalSHA(evidence.disposition)
	evidence.recoveryReport = map[string]any{
		"backend": "compose", "release": loaded.release(),
		"disposition": flags.StorageDisposition, "fence_report_digest": evidence.fenceDigest,
		"registration": "attached-stdin one-time exchange through private exec stream", "replacement_first_hello": "issuer generation-bound wait",
	}
	evidence.recoveryDigest = canonicalSHA(evidence.recoveryReport)
	evidence.postVerify = map[string]any{
		"backend": "compose", "release": loaded.release(),
		"workload_fence": evidence.fenceDigest, "storage_authority": lintelState,
		"issuer_first_auth_observed": "serve exit 0 after the replacement generation first_authenticated_at",
		"finalizer_prechecks":        "exclusive app lock, frozen fence digests, current+retiring shape, first-auth fact",
	}
	evidence.postVerifyDigest = canonicalSHA(evidence.postVerify)
	for name, record := range map[string]map[string]any{
		"recovery-fence-report.json": evidence.fenceReport,
		"recovery-disposition.json":  evidence.disposition,
		"recovery-report.json":       evidence.recoveryReport,
		"recovery-post-verify.json":  evidence.postVerify,
	} {
		if writeErr := writeEvidenceRecord(loaded.stateDir, name, record); writeErr != nil {
			return inputFailure(req, "recover-lintel", &InputError{fmt.Sprintf("persist recovery evidence %s: %v", name, writeErr)})
		}
	}

	stage := helper.report.BeginStage(recoveryStageFence)
	if out, e := helper.run(stage, "recovery-stop", dockerize(append(append([]string{}, loaded.composeArguments...), "stop", "quoin", "plinth", "lintel", "stele"))...); e != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "recovery_stop_failed", Message: strings.TrimSpace(out), NextAction: "keep all workloads stopped and retry"})
	}
	running, e := helper.run(stage, "recovery-fence", dockerize(append(append([]string{}, loaded.composeArguments...), "ps", "--status", "running", "--quiet"))...)
	if e != nil || strings.TrimSpace(running) != "" {
		return helper.failStage(req, stage, &PlatformError{Code: "recovery_fence_failed", Message: strings.TrimSpace(running), NextAction: "prove all four workloads are stopped before recovery"})
	}
	helper.report.CompleteStage(stage, "all workloads stopped; Lintel bind path fenced")
	if !recordStage(recoveryStageFence) {
		return helper.failStage(req, stage, &PlatformError{Code: "install_state_unwritable", Message: "the recovery retry state could not be persisted", NextAction: "fix state directory permissions, then rerun the same command"})
	}

	if !resume[recoveryStageRegister] {
		stage = helper.report.BeginStage(recoveryStageRegister)
		if e := helper.composeRegistrationExchange(req, stage, loaded, flags, evidence); e != nil {
			return helper.failStage(req, stage, e)
		}
		helper.report.CompleteStage(stage, "replacement registered; first authenticated Hello observed by the issuer")
		if !recordStage(recoveryStageRegister) {
			return helper.failStage(req, stage, &PlatformError{Code: "install_state_unwritable", Message: "the recovery retry state could not be persisted", NextAction: "fix state directory permissions, then rerun the same command"})
		}
	} else {
		stage = helper.report.BeginStage(recoveryStageRegister)
		helper.report.CompleteStage(stage, "registration already completed in the persisted retry state; replay skipped")
	}

	if !resume[recoveryStageFinalize] {
		stage = helper.report.BeginStage(recoveryStageFinalize)
		final := append(append([]string{}, loaded.composeArguments...), "run", "--rm", "-T", "quoin", "maintenance", "recover-lintel", "--phase", "finalize", "--config", "/etc/quoin/component.yaml", "--storage-disposition", flags.StorageDisposition, "--disposition-digest", evidence.dispositionDigest, "--fence-report-digest", evidence.fenceDigest, "--recovery-report-digest", evidence.recoveryDigest, "--post-verify-digest", evidence.postVerifyDigest)
		if out, e := helper.run(stage, "recovery-finalize", dockerize(final)...); e != nil {
			return helper.failStage(req, stage, &PlatformError{Code: "recovery_finalize_failed", Message: strings.TrimSpace(out), NextAction: "keep workloads stopped and retry finalization with the same evidence"})
		}
		helper.report.CompleteStage(stage, "immutable recovery receipt committed after first authenticated Hello")
		if !recordStage(recoveryStageFinalize) {
			return helper.failStage(req, stage, &PlatformError{Code: "install_state_unwritable", Message: "the recovery retry state could not be persisted", NextAction: "fix state directory permissions, then rerun the same command"})
		}
	} else {
		stage = helper.report.BeginStage(recoveryStageFinalize)
		helper.report.CompleteStage(stage, "finalization already completed in the persisted retry state; replay skipped")
	}

	stage = helper.report.BeginStage("recovery-restart")
	if out, e := helper.run(stage, "recovery-restart", dockerize(append(append([]string{}, loaded.composeArguments...), "up", "--detach", "quoin", "plinth", "lintel", "stele"))...); e != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "recovery_restart_failed", Message: strings.TrimSpace(out), NextAction: "restart remaining workloads then run recover-lintel --phase assert"})
	}
	if e := helper.awaitHealthy(loaded, 300*time.Second, stage); e != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "recovery_health_failed", Message: e.Error(), NextAction: "inspect recovery logs and retry assertion"})
	}
	if e := verifyOperationalSurface(req, loaded, helper, stage); e != nil {
		return helper.failStage(req, stage, e)
	}
	helper.report.CompleteStage(stage, "operational surface verified; recovery receipt and replacement first-auth closed the action")
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if writeErr := state.WriteInstallState(loaded.stateDir); writeErr != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "install_state_unwritable", Message: "the final recovery state could not be persisted", NextAction: "fix state directory permissions; the recovery itself already completed"})
	}
	helper.report.MarkSucceeded()
	return ExitSuccess
}

// composeRegistrationExchange drives the token exchange with session
// lifecycles that cannot deadlock: the short `issue` exec session mints the
// token, streams the single envelope on stdout and exits as soon as the
// Register RPC consumes it — so the register one-shot's stdin reaches EOF
// independently of the first-auth wait. The long `await` exec session then
// serves the ordinary Connect handshake; the long-lived Lintel starts in
// between and its real Hello completes the generation-bound wait. Every
// stream is an os.Pipe descriptor handed directly to the child.
func (helper *runner) composeRegistrationExchange(req Request, stage int, loaded *loadedRequest, flags deployrecovery.Flags, evidence *recoveryEvidence) error {
	holder := append(append([]string{}, loaded.composeArguments...), "run", "--rm", "-d", "--name", recoveryHolderContainer, "--use-aliases", "quoin", "maintenance", "recover-lintel", "--phase", "hold", "--config", "/etc/quoin/component.yaml")
	if out, e := helper.run(stage, "recovery-holder-start", dockerize(holder)...); e != nil {
		return &PlatformError{Code: "recovery_issuer_start_failed", Message: strings.TrimSpace(out), NextAction: "inspect the Compose recovery issuer"}
	}
	defer func() {
		_, _ = helper.run(stage, "recovery-holder-cleanup", dockerize([]string{"rm", "-f", recoveryHolderContainer})...)
	}()

	baseArgs := []string{"maintenance", "recover-lintel", "--config", "/etc/quoin/component.yaml", "--backend", "compose", "--storage-disposition", flags.StorageDisposition, "--disposition-digest", evidence.dispositionDigest, "--fence-report-digest", evidence.fenceDigest}
	issueCommand := append([]string{"exec", "-i", recoveryHolderContainer, "/quoin"}, baseArgs...)
	issueCommand = append(issueCommand, "--phase", "issue", "--first-auth-timeout", flags.FirstAuthTimeout.String())
	issueArgs := dockerize(issueCommand)

	envelopeReader, envelopeWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		return &PlatformError{Code: "recovery_issuer_start_failed", Message: pipeErr.Error(), NextAction: "retry while workloads remain fenced"}
	}
	issueErrReader, issueErrWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		_ = envelopeReader.Close()
		_ = envelopeWriter.Close()
		return &PlatformError{Code: "recovery_issuer_start_failed", Message: pipeErr.Error(), NextAction: "retry while workloads remain fenced"}
	}
	issue := exec.Command(issueArgs[0], issueArgs[1:]...)
	issue.Env = helper.env
	issue.Stdout = envelopeWriter
	issue.Stderr = issueErrWriter
	issueStarted := time.Now()
	if err := issue.Start(); err != nil {
		_ = envelopeWriter.Close()
		_ = envelopeReader.Close()
		_ = issueErrWriter.Close()
		_ = issueErrReader.Close()
		return &PlatformError{Code: "recovery_issuer_start_failed", Message: err.Error(), NextAction: "inspect the Compose recovery issuer"}
	}
	// The parent copies go now; the children hold the real descriptors.
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
			return &PlatformError{Code: "recovery_issuer_start_failed", Message: fmt.Sprintf("issuer did not report its serving state: %v", err), NextAction: "keep workloads fenced and rerun"}
		}
	case <-time.After(90 * time.Second):
		_ = issue.Process.Kill()
		_ = issue.Wait()
		_ = envelopeReader.Close()
		_ = issueErrReader.Close()
		return &PlatformError{Code: "recovery_issuer_start_failed", Message: "issuer did not report its serving state within 90s", NextAction: "keep workloads fenced and rerun"}
	}
	var serving struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(first), &serving); err != nil || serving.Code != "lintel_recovery.serving" {
		issueRun := issue.Wait()
		_ = envelopeReader.Close()
		rest, _ := io.ReadAll(events)
		_ = issueErrReader.Close()
		return &PlatformError{Code: "recovery_issuer_start_failed", Message: fmt.Sprintf("issue session failed (%v): %s", issueRun, strings.TrimSpace(first+string(rest))), NextAction: "keep workloads fenced and rerun"}
	}
	needsRegistration := strings.Contains(first, `"needs_registration":true`)

	if needsRegistration {
		regArgs := dockerize(append(append([]string{}, loaded.composeArguments...), "run", "--rm", "--no-deps", "-i", "-T", "lintel", "register", "--config", "/etc/quoin/component.yaml"))
		register := exec.Command(regArgs[0], regArgs[1:]...)
		register.Env = helper.env
		register.Stdin = envelopeReader
		var regErr bytes.Buffer
		register.Stderr = &regErr
		regStarted := time.Now()
		if err := register.Start(); err != nil {
			_ = envelopeReader.Close()
			_ = issue.Process.Kill()
			_ = issue.Wait()
			_ = issueErrReader.Close()
			return &PlatformError{Code: "recovery_register_start_failed", Message: err.Error(), NextAction: "inspect the Lintel registration one-shot"}
		}
		// The issue session exits as soon as the token is consumed; its exit
		// closes the envelope stream, so the register one-shot's stdin
		// reaches EOF and both processes finish independently of the later
		// first-auth wait.
		issueRun := issue.Wait()
		regRun := register.Wait()
		_ = envelopeReader.Close()
		helper.report.RecordCommand(stage, report.Command{Argv: issueArgs, ExitCode: commandExitCode(issue), Duration: time.Since(issueStarted).Round(time.Millisecond).String()})
		helper.report.RecordCommand(stage, report.Command{Argv: regArgs, ExitCode: commandExitCode(register), Duration: time.Since(regStarted).Round(time.Millisecond).String()})
		rest, _ := io.ReadAll(events)
		_ = issueErrReader.Close()
		if issueRun != nil {
			return &PlatformError{Code: "recovery_registration_failed", Message: fmt.Sprintf("issue session failed (%v): %s", issueRun, strings.TrimSpace(first+string(rest))), NextAction: "retry while workloads remain fenced; the one-time token was never recorded"}
		}
		if regRun != nil {
			return &PlatformError{Code: "recovery_registration_failed", Message: strings.TrimSpace(regErr.String()), NextAction: "retry while workloads remain fenced; the one-time token was never recorded"}
		}
	} else {
		issueRun := issue.Wait()
		_ = envelopeReader.Close()
		helper.report.RecordCommand(stage, report.Command{Argv: issueArgs, ExitCode: commandExitCode(issue), Duration: time.Since(issueStarted).Round(time.Millisecond).String()})
		rest, _ := io.ReadAll(events)
		_ = issueErrReader.Close()
		if issueRun != nil {
			return &PlatformError{Code: "recovery_issuer_start_failed", Message: fmt.Sprintf("issue session failed (%v): %s", issueRun, strings.TrimSpace(first+string(rest))), NextAction: "keep workloads fenced and rerun"}
		}
	}

	// Await session: serve the ordinary Connect handshake until the exact
	// replacement generation records its first authenticated Hello.
	awaitErrReader, awaitErrWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		return &PlatformError{Code: "recovery_issuer_start_failed", Message: pipeErr.Error(), NextAction: "retry while workloads remain fenced"}
	}
	awaitCommand := append([]string{"exec", "-i", recoveryHolderContainer, "/quoin"}, baseArgs...)
	awaitCommand = append(awaitCommand, "--phase", "await", "--first-auth-timeout", flags.FirstAuthTimeout.String())
	awaitArgs := dockerize(awaitCommand)
	await := exec.Command(awaitArgs[0], awaitArgs[1:]...)
	await.Env = helper.env
	await.Stdout = io.Discard
	await.Stderr = awaitErrWriter
	awaitStarted := time.Now()
	if err := await.Start(); err != nil {
		_ = awaitErrWriter.Close()
		_ = awaitErrReader.Close()
		return &PlatformError{Code: "recovery_issuer_start_failed", Message: err.Error(), NextAction: "inspect the Compose recovery issuer"}
	}
	_ = awaitErrWriter.Close()
	if out, lintelErr := helper.run(stage, "recovery-start-lintel", dockerize(append(append([]string{}, loaded.composeArguments...), "up", "--detach", "lintel"))...); lintelErr != nil {
		_ = await.Process.Kill()
		_ = await.Wait()
		_ = awaitErrReader.Close()
		return &PlatformError{Code: "recovery_lintel_start_failed", Message: strings.TrimSpace(out), NextAction: "keep the recovery issuer running and retry"}
	}
	awaitRun := await.Wait()
	awaitEvents, _ := io.ReadAll(awaitErrReader)
	_ = awaitErrReader.Close()
	helper.report.RecordCommand(stage, report.Command{Argv: awaitArgs, ExitCode: commandExitCode(await), Duration: time.Since(awaitStarted).Round(time.Millisecond).String()})
	if awaitRun != nil {
		return &PlatformError{Code: "recovery_first_hello_missing", Message: strings.TrimSpace(string(awaitEvents)), NextAction: "keep workloads fenced and rerun; the issuer resumes without a new token"}
	}
	if !strings.Contains(string(awaitEvents), "lintel_recovery.first_authenticated") {
		return &PlatformError{Code: "recovery_first_hello_missing", Message: "await session exited without recording the first authenticated Hello", NextAction: "keep workloads fenced and rerun"}
	}
	return nil
}

func commandExitCode(command *exec.Cmd) int {
	if command == nil || command.ProcessState == nil {
		return -1
	}
	return command.ProcessState.ExitCode()
}
