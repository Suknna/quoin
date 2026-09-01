package compose

import (
	"fmt"
	"strings"

	deploycompose "github.com/Suknna/quoin/deploy/compose"
	deploybackup "github.com/Suknna/quoin/internal/deploy/backup"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
)

// Backup executes the credential-free deployment backup protocol. Online mode
// reads only the in-network ops metrics and asks the already authenticated
// Admin to create the product command; offline mode stops the stack and uses
// the shipped Quoin image so no host CLI or Web Session is involved.
func Backup(req Request, offline bool) (exitCode int) {
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "backup", err)
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "backup", &InputError{err.Error()})
	}
	helper, err := newRunner(req, loaded, "backup", digest)
	if err != nil {
		return inputFailure(req, "backup", &InputError{err.Error()})
	}
	defer helper.finish(req, loaded)
	if offline {
		return helper.offlineBackup(req, loaded)
	}
	return helper.onlineBackup(req, loaded)
}
func (helper *runner) onlineBackup(req Request, loaded *loadedRequest) int {
	stage := helper.report.BeginStage("online-backup-observation")
	overlay, err := deploycompose.RenderVerifyOverlay(loaded.projection, deploycompose.Options{Images: loaded.images})
	if err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "verifier_overlay_invalid", Message: err.Error(), NextAction: "fix the deployment projection, then retry"})
	}
	arguments := append(append([]string{}, loaded.composeArguments...), "--file", overlay, "run", "--rm", "--no-deps", "quoin-verifier", "http://quoin:9090/metrics")
	read := func(name string) (deploybackup.Observation, error) {
		body, err := helper.run(stage, name, dockerize(arguments)...)
		if err != nil {
			return deploybackup.Observation{}, err
		}
		return deploybackup.ParseObservation(body)
	}
	err = deploybackup.Observe(deploybackup.OnlineOptions{Read: read, OnReady: func() {
		fmt.Fprintln(req.stdout(), "Quoin is available. In the Admin Web UI, select 立即备份; this helper will observe only /metrics and does not receive a Session.")
	}})
	if err != nil {
		if observation, ok := err.(*deploybackup.ObservationError); ok {
			return helper.failStage(req, stage, &PlatformError{Code: observation.Code, Message: observation.Message, NextAction: observation.NextAction})
		}
		return helper.failStage(req, stage, &PlatformError{Code: "online_backup_unavailable", Message: err.Error(), NextAction: "retry after Quoin is ready"})
	}
	helper.report.CompleteStage(stage, "online manual backup observed through ops metrics")
	helper.report.MarkSucceeded()
	return ExitSuccess
}
func (helper *runner) offlineBackup(req Request, loaded *loadedRequest) int {
	stage := helper.report.BeginStage("offline-backup")
	// --offline is a fallback, not permission to take down a healthy service.
	// The in-network verifier command is recorded in the report as the operator's
	// proof that Quoin could not be reached before workloads are stopped.
	overlay, err := deploycompose.RenderVerifyOverlay(loaded.projection, deploycompose.Options{Images: loaded.images})
	if err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "offline_backup_unavailability_unproven", Message: err.Error(), NextAction: "repair the verifier projection and prove Quoin unavailable before retrying"})
	}
	probe := append(append([]string{}, loaded.composeArguments...), "--file", overlay, "run", "--rm", "--no-deps", "quoin-verifier", "http://quoin:9090/metrics")
	if output, probeErr := helper.run(stage, "backup-prove-quoin-unavailable", dockerize(probe)...); probeErr == nil {
		return helper.failStage(req, stage, &PlatformError{Code: "offline_backup_requires_unavailable", Message: "Quoin remains reachable through its ops listener", NextAction: "use online backup while Quoin is reachable; --offline is only for an unavailable Quoin"})
	} else if !deploybackup.UnavailabilityProven(output, probeErr) {
		return helper.failStage(req, stage, &PlatformError{Code: "offline_backup_unavailability_unproven", Message: "the verifier failure was not an explicit network-unavailable result", NextAction: "resolve verifier execution and retry; do not stop workloads without proving Quoin unavailable"})
	}
	if output, err := helper.run(stage, "backup-stop-workloads", dockerize(append(append([]string{}, loaded.composeArguments...), "stop"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "offline_backup_stop_failed", Message: strings.TrimSpace(output), NextAction: "stop all Quoin workloads before retrying offline backup"})
	}
	arguments := append(append([]string{}, loaded.composeArguments...), "run", "--rm", "--no-deps", "quoin", "backup", "--offline", "--config", "/etc/quoin/component.yaml")
	if output, err := helper.run(stage, "backup-offline", dockerize(arguments)...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "offline_backup_failed", Message: strings.TrimSpace(output), NextAction: "keep workloads stopped, inspect the backup report and retry"})
	}
	helper.report.CompleteStage(stage, "workloads stopped and offline Quoin backup completed")
	verifyStage := helper.report.BeginStage("backup-post-verify")
	if output, err := helper.run(verifyStage, "backup-restart-workloads", dockerize(append(append([]string{}, loaded.composeArguments...), "up", "--detach"))...); err != nil {
		return helper.failStage(req, verifyStage, &PlatformError{Code: "offline_backup_restart_failed", Message: strings.TrimSpace(output), NextAction: "start the stopped workloads, then run quoin-deploy compose verify"})
	}
	if err := verifyOperationalSurface(req, loaded, helper, verifyStage); err != nil {
		return helper.failStage(req, verifyStage, err)
	}
	helper.report.CompleteStage(verifyStage, "post-backup operational surface verified")
	helper.report.MarkSucceeded()
	return ExitSuccess
}
