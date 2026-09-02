package compose

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

// Restore stops every long-lived workload before invoking the same release's
// attached-TTY restore command. It intentionally leaves Quoin alone in its
// maintenance-safe mode; an operator exits the SQL checklist before using the
// ordinary install/verify path to restart other components.
func Restore(req Request, backupID string) int {
	if backupID == "" {
		return inputFailure(req, "restore", &InputError{"restore requires --backup <published-backup-id>"})
	}
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "restore", err)
	}
	if loaded.manifest == nil {
		return inputFailure(req, "restore", &InputError{"restore requires --release-manifest with the exact digest-pinned Quoin image"})
	}
	if !isTerminal(req.Stdin) {
		return inputFailure(req, "restore", &InputError{"restore requires an attached TTY; the temporary recovery password is never accepted from stdin"})
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "restore", &InputError{err.Error()})
	}
	helper, err := newRunner(req, loaded, "restore", digest)
	if err != nil {
		return inputFailure(req, "restore", &InputError{err.Error()})
	}
	defer helper.finish(req, loaded)
	stage := helper.report.BeginStage("restore-preflight")
	if output, err := helper.run(stage, "restore-compose-config", dockerize(append(append([]string{}, loaded.composeArguments...), "config", "--quiet"))...); err != nil {
		return helper.failPreflight(req, stage, "restore_projection_invalid", strings.TrimSpace(output))
	}
	// Preflight is a same-Release one-shot container, not `compose exec`: the
	// whole point of offline recovery is that Quoin may be stopped or unable to
	// start while its backup volume remains readable.
	preflightArgs := append(append([]string{}, loaded.composeArguments...), "run", "--rm", "--no-deps", "quoin", "restore", "preflight", "--backup", backupID, "--config", "/etc/quoin/component.yaml")
	manifestSummary, err := helper.run(stage, "restore-backup-preflight", dockerize(preflightArgs)...)
	if err != nil {
		return helper.failPreflight(req, stage, "restore_backup_invalid", strings.TrimSpace(manifestSummary))
	}
	fmt.Fprintf(req.stdout(), "Restore release %s from backup %s using Quoin image %s. Verified target manifest: %s. This stops quoin, plinth, lintel, and stele. Type RESTORE to continue: ", loaded.release(), backupID, loaded.images["quoin"], strings.TrimSpace(manifestSummary))
	var confirmation string
	if _, err := fmt.Fscan(req.Stdin, &confirmation); err != nil || confirmation != "RESTORE" {
		return helper.failPreflight(req, stage, "restore_not_confirmed", "restore was not confirmed; no workloads were stopped")
	}
	helper.report.CompleteStage(stage, "Compose projection valid; destructive stop was explicitly confirmed and recovery credential will be read only by Quoin attached TTY")
	stage = helper.report.BeginStage("restore-offline")
	if output, err := helper.run(stage, "restore-stop-workloads", dockerize(append(append([]string{}, loaded.composeArguments...), "stop"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "restore_stop_failed", Message: strings.TrimSpace(output), NextAction: "ensure every Quoin workload is stopped, then retry"})
	}
	// A stopped release may already have published this exact restore. Query the
	// data-lock-protected fence before opening a second offline restore TTY. Only
	// an explicit absent marker permits a first restore; a damaged active fence
	// is a hard failure and keeps workloads stopped.
	continuationArgs := append(append([]string{}, loaded.composeArguments...), "run", "--rm", "--no-deps", "quoin", "restore", "continue", "--backup", backupID, "--config", "/etc/quoin/component.yaml")
	continuation, continuationErr := helper.run(stage, "restore-continuation", dockerize(continuationArgs)...)
	resumed := continuationErr == nil
	if continuationErr != nil && !strings.Contains(continuation, "restore_continuation=absent") {
		return helper.failStage(req, stage, &PlatformError{Code: "restore_continuation_invalid", Message: strings.TrimSpace(continuation), NextAction: "keep workloads stopped and repair the published Restore maintenance fence before retrying"})
	}
	if !resumed {
		// Force a container-side pseudo-terminal: the helper deliberately captures
		// child output for its report, so Compose cannot infer the caller's TTY from
		// stdout. The recovery command itself rejects non-terminal password input.
		arguments := append(append([]string{}, loaded.composeArguments...), "run", "--rm", "--no-TTY=false", "--no-deps", "quoin", "restore", "--backup", backupID, "--config", "/etc/quoin/component.yaml")
		if output, err := helper.runInteractive(req, stage, "restore-offline", dockerize(arguments)); err != nil {
			return helper.failStage(req, stage, &PlatformError{Code: "restore_failed", Message: strings.TrimSpace(output), NextAction: "keep workloads stopped; inspect the restore report and retry with the same backup"})
		}
		helper.report.CompleteStage(stage, "verified backup restored and trust isolation committed before publication")
	} else {
		helper.report.CompleteStage(stage, "resumed published Restore maintenance using durable rollback fence")
	}
	stage = helper.report.BeginStage("restore-maintenance")
	if output, err := helper.run(stage, "restore-start-quoin-maintenance", dockerize(append(append([]string{}, loaded.composeArguments...), "up", "--detach", "--no-deps", "quoin"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "restore_maintenance_start_failed", Message: strings.TrimSpace(output), NextAction: "start Quoin alone from the restored data volume and inspect /readyz"})
	}
	if err := waitForComposeMaintenance(helper, loaded, stage); err != nil {
		return helper.failStage(req, stage, err)
	}
	helper.report.CompleteStage(stage, fmt.Sprintf("Quoin alone reached restore isolation from backup %s", backupID))

	fmt.Fprint(req.stdout(), "Complete the Restore checklist and exit maintenance in Quoin, then type CONTINUE to restart Quoin in normal mode: ")
	var continueRestore string
	if _, err := fmt.Fscan(req.Stdin, &continueRestore); err != nil || continueRestore != "CONTINUE" {
		return helper.failStage(req, stage, &PlatformError{Code: "restore_not_completed", Message: "maintenance checklist was not confirmed complete", NextAction: "complete the checklist and rerun restore"})
	}
	stage = helper.report.BeginStage("restore-post-verify")
	if output, err := helper.run(stage, "restore-restart-quoin", dockerize(append(append([]string{}, loaded.composeArguments...), "restart", "quoin"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "restore_normal_restart_failed", Message: strings.TrimSpace(output), NextAction: "restart Quoin after exiting maintenance"})
	}
	if err := waitForComposeNormal(helper, loaded, stage); err != nil {
		return helper.failStage(req, stage, err)
	}
	if output, err := helper.run(stage, "restore-start-workloads", dockerize(append(append([]string{}, loaded.composeArguments...), "up", "--detach", "plinth", "lintel", "stele"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "restore_workloads_start_failed", Message: strings.TrimSpace(output), NextAction: "start same-Release workloads only after Quoin reports normal readiness"})
	}
	if err := verifyOperationalSurface(req, loaded, helper, stage); err != nil {
		return helper.failStage(req, stage, errAsPlatform(err))
	}
	rollback := ".restore-rollback-" + backupID
	if output, err := helper.run(stage, "restore-finalize-rollback", dockerize(append(append([]string{}, loaded.composeArguments...), "exec", "-T", "quoin", "/quoin", "restore", "finalize", "--rollback", rollback, "--config", "/etc/quoin/component.yaml"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "restore_rollback_finalize_failed", Message: strings.TrimSpace(output), NextAction: "keep the rollback point and inspect post-restore cleanup"})
	}
	helper.report.CompleteStage(stage, "post-restore operational verification completed and rollback point finalized")
	helper.report.MarkSucceeded()
	return ExitSuccess
}

func waitForComposeMaintenance(helper *runner, loaded *loadedRequest, stage int) *PlatformError {
	arguments := dockerize(append(append([]string{}, loaded.composeArguments...), "exec", "-T", "quoin", "/quoin-healthcheck", "--status", "200", "http://127.0.0.1:9090/readyz"))
	var last string
	for attempt := 0; attempt < 60; attempt++ {
		output, err := helper.run(stage, "restore-maintenance-readyz", arguments...)
		last = output
		var readiness sharedops.Readiness
		if err == nil && json.Unmarshal([]byte(strings.TrimSpace(output)), &readiness) == nil && readiness.Mode == "maintenance" && !readiness.AcceptingWork && readiness.Reason == sharedops.Maintenance {
			helper.report.RecordCheck(restoreMaintenanceCheck())
			return nil
		}
		time.Sleep(time.Second)
	}
	return &PlatformError{Code: "restore_maintenance_not_ready", Message: strings.TrimSpace(last), NextAction: "keep other workloads stopped and inspect Quoin maintenance readiness"}
}

func waitForComposeNormal(helper *runner, loaded *loadedRequest, stage int) *PlatformError {
	arguments := dockerize(append(append([]string{}, loaded.composeArguments...), "exec", "-T", "quoin", "/quoin-healthcheck", "--status", "200", "http://127.0.0.1:9090/readyz"))
	var last string
	for attempt := 0; attempt < 60; attempt++ {
		output, err := helper.run(stage, "restore-normal-readyz", arguments...)
		last = output
		var readiness sharedops.Readiness
		if err == nil && json.Unmarshal([]byte(strings.TrimSpace(output)), &readiness) == nil && readiness.Mode == "normal" && readiness.AcceptingWork && readiness.Reason == sharedops.Ready {
			helper.report.RecordCheck(report.Check{ID: "restore-normal-ready", Result: "passed", Expected: "mode=normal acceptingWork=true reason=ready", Actual: "Quoin normal readiness after maintenance exit", Code: "restore_normal_ready"})
			return nil
		}
		time.Sleep(time.Second)
	}
	return &PlatformError{Code: "restore_normal_not_ready", Message: strings.TrimSpace(last), NextAction: "do not start other workloads until Quoin exits maintenance and reports normal readiness"}
}

func restoreMaintenanceCheck() report.Check {
	return report.Check{ID: "restore-maintenance-ready", Result: "passed", Expected: "mode=maintenance acceptingWork=false reason=maintenance", Actual: "Quoin maintenance-safe readiness", Code: "restore_maintenance_ready"}
}
