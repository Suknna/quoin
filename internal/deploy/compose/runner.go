package compose

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	deploycompose "github.com/Suknna/quoin/deploy/compose"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
)

type runner struct {
	stdout, stderr io.Writer
	env            []string
	report         *report.Report
	logsDirectory  string
	project        string
}

func newRunner(req Request, loaded *loadedRequest, command, configDigest string) (*runner, error) {
	reports := filepath.Join(loaded.stateDir, "reports")
	if err := os.MkdirAll(reports, 0o700); err != nil {
		return nil, err
	}
	deploymentReport := report.New("compose", buildPlatform(), command, req.ConfigPath, configDigest)
	deploymentReport.Release = loaded.release()
	if loaded.manifest != nil {
		deploymentReport.ImageMode = "release-manifest-digest-pinned"
	} else {
		deploymentReport.ImageMode = "local-development"
	}
	helper := &runner{
		stdout: req.stdout(), stderr: req.stderr(),
		env:           os.Environ(),
		report:        deploymentReport,
		logsDirectory: reports,
		project:       loaded.project,
	}
	if loaded.manifest != nil && req.ReleaseManifestPath != "" {
		digest, err := deployconfig.DigestFile(req.ReleaseManifestPath)
		if err != nil {
			return nil, err
		}
		deploymentReport.ReleaseManifestDigest = digest
	}
	return helper, nil
}

func buildPlatform() string {
	return fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
}

// dockerize prefixes the docker binary; composeArguments carries only the
// compose subcommand chain.
func dockerize(arguments []string) []string {
	return append([]string{"docker"}, arguments...)
}

// run executes an external command, tees combined output to the operator and
// a log file, and records the raw exit code in the running report stage.
// Children never inherit the helper's stdin: piped bootstrap answers must not
// be drained by Docker clients (interactive TTY runs use runInteractive).
func (helper *runner) run(stage int, name string, argv ...string) (string, error) {
	return helper.runWithStdin(stage, name, nil, argv...)
}

func (helper *runner) runWithStdin(stage int, name string, stdin io.Reader, argv ...string) (string, error) {
	command := exec.Command(argv[0], argv[1:]...)
	command.Stdin = stdin
	command.Env = helper.env
	return helper.execute(stage, name, command)
}

func (helper *runner) runInteractive(req Request, stage int, name string, argv []string) (string, error) {
	command := exec.Command(argv[0], argv[1:]...)
	command.Stdin = req.Stdin
	command.Env = helper.env
	return helper.execute(stage, name, command)
}

func (helper *runner) execute(stage int, name string, command *exec.Cmd) (string, error) {
	var combined bytes.Buffer
	command.Stdout = io.MultiWriter(&combined, helper.stdout)
	command.Stderr = io.MultiWriter(&combined, helper.stderr)
	started := time.Now()
	runErr := command.Run()
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	logPath := filepath.Join(helper.logsDirectory, name+".log")
	_ = os.WriteFile(logPath, combined.Bytes(), 0o600)
	helper.report.RecordCommand(stage, report.Command{Argv: command.Args, ExitCode: exitCode, Duration: time.Since(started).Round(time.Millisecond).String(), LogPath: logPath})
	return combined.String(), runErr
}

// Install runs the staged Compose installation. Stage completion is persisted
// after each successful external effect, so a retry with the same
// release/backend/config/command identity resumes from the last completed
// stage (OPS-HELPER-002); a changed identity with a pending partial install
// is refused instead of silently reusing the old state.
func Install(req Request) (exitCode int) {
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "install", err)
	}
	configDigest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "install", &InputError{err.Error()})
	}
	helper, err := newRunner(req, loaded, "install", configDigest)
	if err != nil {
		return inputFailure(req, "install", &InputError{err.Error()})
	}

	key := deployconfig.InstallStateKey{ReleaseVersion: loaded.release(), Backend: "compose", ConfigDigest: configDigest, Command: "install"}
	state, stateErr := deployconfig.LoadInstallState(loaded.stateDir)
	if stateErr != nil {
		return inputFailure(req, "install", &InputError{stateErr.Error()})
	}
	resume := map[string]bool{}
	if state != nil {
		if state.Key != key {
			if state.FinishedAt == "" {
				return inputFailure(req, "install", &InputError{
					fmt.Sprintf("a partially completed install is pending for different inputs (release %s, config digest %s); finish it with those inputs, or remove %s after cleaning up that deployment",
						state.Key.ReleaseVersion, shortDigest(state.Key.ConfigDigest), filepath.Join(loaded.stateDir, "install-state.json"))})
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
	// recordStage persists the last completed stage; a state that cannot be
	// persisted must fail the run, because the frozen retry contract depends
	// on it (OPS-HELPER-002).
	recordStage := func(stage string) bool {
		state.StagesDone = append(state.StagesDone, stage)
		if writeErr := state.WriteInstallState(loaded.stateDir); writeErr != nil {
			fmt.Fprintf(req.stderr(), "quoin-deploy: persist install state: %v\n", writeErr)
			return false
		}
		return true
	}

	// Read scripted bootstrap answers before any Docker child can inherit and
	// drain piped stdin; interactive runs keep passthrough stdin instead.
	forceScripted := os.Getenv("QUOIN_DEPLOY_SCRIPTED") == "1"
	var scripted *deploycompose.AdminAnswers
	if forceScripted || !isTerminal(req.Stdin) {
		answers, answersErr := deploycompose.ReadAdminAnswers()
		if answersErr != nil {
			return inputFailure(req, "install", &InputError{answersErr.Error()})
		}
		scripted = &answers
	}

	defer helper.finish(req, loaded)
	stage := helper.report.BeginStage(stagePreflight)
	// OPS-HELPER-003: preflight errors exit 2 without deployment side
	// effects; platform and behavioral failures later exit 1.
	if output, runErr := helper.run(stage, "compose-version", dockerize(append(append([]string{}, loaded.composeArguments...), "version"))...); runErr != nil {
		return helper.failPreflight(req, stage, "docker_compose_unavailable", strings.TrimSpace(output))
	}
	if output, runErr := helper.run(stage, "compose-config", dockerize(append(append([]string{}, loaded.composeArguments...), "config", "--quiet"))...); runErr != nil {
		return helper.failPreflight(req, stage, "compose_projection_invalid", strings.TrimSpace(output))
	}
	stackRunning := helper.stackRunning(loaded)
	helper.report.CompleteStage(stage, fmt.Sprintf("Docker Compose reachable; projection valid; existing stack running=%t", stackRunning))
	if !recordStage(stagePreflight) {
		return helper.failStage(req, stage, &PlatformError{Code: "install_state_unwritable", Message: "the install retry state could not be persisted", NextAction: "fix the state directory permissions under XDG_STATE_HOME, then rerun the same command"})
	}

	// Stage: deployment secrets (OPS-SECRET-003). The one-shot service is
	// idempotent; retries skip it only when the stage is recorded complete
	// and the secret set is still present.
	stage = helper.report.BeginStage(stageSecretBootstrap)
	if stackRunning || (resume[stageSecretBootstrap] && secretsPresent(loaded.input.SecretDirectory)) {
		helper.report.CompleteStage(stage, "existing deployment secret set already active")
	} else {
		// `compose run` is the only path that honours the service's TTY
		// allocation while the helper keeps stdin ownership.
		if output, runErr := helper.run(stage, "secret-bootstrap", dockerize(append(append([]string{}, loaded.composeArguments...), "run", "--rm", "-T", "secret-bootstrap"))...); runErr != nil {
			return helper.failStage(req, stage, &PlatformError{Code: "secret_bootstrap_failed", Message: strings.TrimSpace(output), NextAction: "fix the reported secret bootstrap error; persistent state was preserved; rerun the same command"})
		}
		helper.report.CompleteStage(stage, "deployment secret set created or verified")
	}
	if !recordStage(stageSecretBootstrap) {
		return helper.failStage(req, stage, &PlatformError{Code: "install_state_unwritable", Message: "the install retry state could not be persisted", NextAction: "fix the state directory permissions under XDG_STATE_HOME, then rerun the same command"})
	}

	// Stage: first administrator (OPS-PACKAGE-003).
	stage = helper.report.BeginStage(stageAdminBootstrap)
	if stackRunning {
		helper.report.CompleteStage(stage, "existing administrator and persistent data preserved")
	} else if resume[stageAdminBootstrap] {
		// A prior attempt already created the administrator; retries must
		// continue from completed stages instead of re-driving the one-shot
		// bootstrap (OPS-HELPER-002).
		helper.report.CompleteStage(stage, "stage already completed in the persisted retry state")
	} else if scripted == nil {
		if output, runErr := helper.runInteractive(req, stage, "admin-bootstrap", dockerize(append(append([]string{}, loaded.composeArguments...), "run", "--rm", "admin-bootstrap"))); runErr != nil {
			return helper.failStage(req, stage, &PlatformError{Code: "admin_bootstrap_failed", Message: strings.TrimSpace(output), NextAction: "fix the reported administrator bootstrap error; persistent state was preserved; rerun the same command"})
		}
		helper.report.CompleteStage(stage, "first administrator created or confirmed")
	} else {
		transcript, scriptErr := deploycompose.RunAdminBootstrapScripted(loaded.projection.ComposeFile, *scripted)
		fmt.Fprint(req.stdout(), transcript)
		if scriptErr != nil {
			return helper.failStage(req, stage, &PlatformError{Code: "admin_bootstrap_failed", Message: scriptErr.Error(), NextAction: "fix the reported administrator bootstrap error; persistent state was preserved; rerun the same command"})
		}
		helper.report.CompleteStage(stage, "first administrator created or confirmed")
	}
	if !recordStage(stageAdminBootstrap) {
		return helper.failStage(req, stage, &PlatformError{Code: "install_state_unwritable", Message: "the install retry state could not be persisted", NextAction: "fix the state directory permissions under XDG_STATE_HOME, then rerun the same command"})
	}

	// Stage: long-lived workloads. No `--wait`: Compose re-creates exited
	// one-shot dependencies while waiting, which races the running Quoin for
	// the data-directory flock.
	stage = helper.report.BeginStage(stageWorkloads)
	if output, runErr := helper.run(stage, "compose-up", dockerize(append(append([]string{}, loaded.composeArguments...), "up", "--detach", "--remove-orphans", "quoin", "plinth", "lintel", "stele"))...); runErr != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "workloads_not_started", Message: strings.TrimSpace(output), NextAction: "inspect the reported service failure; rerun the same command to resume"})
	}
	if waitErr := helper.awaitHealthy(loaded, 300*time.Second, stage); waitErr != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "workloads_not_healthy", Message: waitErr.Error(), NextAction: "inspect component logs; rerun the same command to resume"})
	}
	helper.report.CompleteStage(stage, "quoin, plinth, lintel, and stele healthy")
	if !recordStage(stageWorkloads) {
		return helper.failStage(req, stage, &PlatformError{Code: "install_state_unwritable", Message: "the install retry state could not be persisted", NextAction: "fix the state directory permissions under XDG_STATE_HOME, then rerun the same command"})
	}

	// Stage: post-install verification with the install expectation profile
	// (OPS-HELPER-004: install reuses the same verifier and judges the
	// action's closed expected post-state).
	stage = helper.report.BeginStage(stageVerify)
	if verifyErr := verifyOperationalSurface(req, loaded, helper, stage); verifyErr != nil {
		return helper.failStage(req, stage, verifyErr)
	}
	helper.report.CompleteStage(stage, "operational surface verified: readiness, metrics, logs, topology, image digests")
	if !recordStage(stageVerify) {
		return helper.failStage(req, stage, &PlatformError{Code: "install_state_unwritable", Message: "the install retry state could not be persisted", NextAction: "fix the state directory permissions under XDG_STATE_HOME, then rerun the same command"})
	}
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if writeErr := state.WriteInstallState(loaded.stateDir); writeErr != nil {
		fmt.Fprintf(req.stderr(), "quoin-deploy: persist install state: %v\n", writeErr)
	}
	helper.report.MarkSucceeded()
	fmt.Fprintf(req.stdout(), "Quoin is running. Public Origin: %s\n", loaded.input.PublicOrigin)
	fmt.Fprintf(req.stdout(), "Generated deployment projection: %s\n", loaded.projection.ComposeFile)
	return ExitSuccess
}

// Verify runs the read-only, repeatable operational-surface verifier
// (OPS-HELPER-004). Apart from the disposable in-network verifier service it
// creates no product domain state.
func Verify(req Request) (exitCode int) {
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "verify", err)
	}
	configDigest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "verify", &InputError{err.Error()})
	}
	helper, err := newRunner(req, loaded, "verify", configDigest)
	if err != nil {
		return inputFailure(req, "verify", &InputError{err.Error()})
	}
	defer helper.finish(req, loaded)
	stage := helper.report.BeginStage(stageVerify)
	if verifyErr := verifyOperationalSurface(req, loaded, helper, stage); verifyErr != nil {
		return helper.failStage(req, stage, verifyErr)
	}
	helper.report.CompleteStage(stage, "operational surface verified: readiness, metrics, logs, topology, image digests")
	helper.report.MarkSucceeded()
	return ExitSuccess
}

func inputFailure(req Request, command string, err error) int {
	brief := report.New("compose", buildPlatform(), command, req.ConfigPath, "")
	brief.MarkFailed("invalid_input", err.Error(), "fix the deployment input; no deployment side effect has occurred")
	brief.ExitCode = ExitInput
	path := req.ReportPath
	if path == "" {
		if stateDir, stateErr := deployconfig.StateDirectory(); stateErr == nil {
			path = filepath.Join(stateDir, "verification-report.json")
		} else {
			path = "verification-report.json"
		}
	}
	_ = brief.Finish(path)
	fmt.Fprintf(req.stderr(), "quoin-deploy: invalid deployment input: %v\n", err)
	return ExitInput
}

// failPreflight closes a preflight-stage failure with exit 2: the frozen
// helper contract classifies preflight errors as input-side failures that
// must not have produced deployment side effects.
func (helper *runner) failPreflight(req Request, stage int, code, message string) int {
	helper.report.FailStage(stage, code+": "+message)
	helper.report.MarkFailed(code, message, "fix the reported preflight error; no deployment side effect has occurred")
	helper.report.ExitCode = ExitInput
	fmt.Fprintf(req.stderr(), "quoin-deploy: preflight failed (%s): %s\nnext action: fix the reported preflight error; no deployment side effect has occurred\n", code, message)
	return ExitInput
}

func (helper *runner) failStage(req Request, stage int, failure error) int {
	if platform, ok := failure.(*PlatformError); ok {
		helper.report.FailStage(stage, platform.Error())
		helper.report.MarkFailed(platform.Code, platform.Message, platform.NextAction)
		fmt.Fprintf(req.stderr(), "quoin-deploy: %s failed (%s): %s\nnext action: %s\n", helper.report.Stages[stage].Name, platform.Code, platform.Message, platform.NextAction)
		return ExitPlatform
	}
	message := failure.Error()
	helper.report.FailStage(stage, message)
	helper.report.MarkFailed("verification_failed", message, "inspect the failed checks in the report, fix the cause, then rerun")
	fmt.Fprintf(req.stderr(), "quoin-deploy: verification failed: %s\n", message)
	return ExitPlatform
}

func (helper *runner) finish(req Request, loaded *loadedRequest) {
	path := req.ReportPath
	if path == "" {
		path = filepath.Join(loaded.stateDir, "verification-report.json")
	}
	if err := helper.report.Finish(path); err != nil {
		fmt.Fprintf(req.stderr(), "quoin-deploy: write verification report: %v\n", err)
	}
	fmt.Fprintf(req.stdout(), "Verification report: %s\n", path)
}

// stackRunning reports whether the exact projected compose file already has a
// live Quoin. Containers from a different config file must not count: they
// are cleaned up before a fresh install.
func (helper *runner) stackRunning(loaded *loadedRequest) bool {
	output, err := helper.capture(dockerize(append(append([]string{}, loaded.composeArguments...), "ps", "--status", "running", "--quiet", "quoin"))...)
	if err != nil || len(output) == 0 {
		return false
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return false
	}
	container := strings.TrimSpace(lines[0])
	configFile, err := helper.capture("docker", "inspect", container, "--format", "{{index .Config.Labels \"com.docker.compose.project.config_files\"}}")
	if err != nil {
		return false
	}
	return strings.TrimSpace(configFile) == loaded.projection.ComposeFile
}

func (helper *runner) capture(argv ...string) (string, error) {
	command := exec.Command(argv[0], argv[1:]...)
	command.Env = helper.env
	var combined bytes.Buffer
	command.Stdout = &combined
	command.Stderr = &combined
	err := command.Run()
	return combined.String(), err
}
