package compose

import (
	"fmt"
	"strings"

	deployupgrade "github.com/Suknna/quoin/cmd/quoin-deploy/upgrade"
	deploycompose "github.com/Suknna/quoin/deploy/compose"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
)

// Upgrade executes the coordinated Compose upgrade (OPS-UPGRADE-002/004):
// observe quoin_upgrade_prepared through the in-network verifier, stop every
// workload behind an explicit confirmation, offline-verify the maintenance
// fence with the deployed (old) image, re-project the new Release images,
// run the exclusive migration with the new image, then bring Quoin and the
// same-Release runtimes back in order and verify the operational surface.
func Upgrade(req Request, flags deployupgrade.Flags) int {
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "upgrade", err)
	}
	if loaded.manifest == nil {
		return inputFailure(req, "upgrade", &InputError{"upgrade requires --release-manifest with the exact digest-pinned images"})
	}
	if !isTerminal(req.Stdin) {
		return inputFailure(req, "upgrade", &InputError{"upgrade requires an attached TTY; the destructive stop is explicitly confirmed"})
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "upgrade", &InputError{err.Error()})
	}
	helper, err := newRunner(req, loaded, "upgrade", digest)
	if err != nil {
		return inputFailure(req, "upgrade", &InputError{err.Error()})
	}
	defer helper.finish(req, loaded)

	stage := helper.report.BeginStage("upgrade-preflight")
	if output, err := helper.run(stage, "upgrade-compose-config", dockerize(append(append([]string{}, loaded.composeArguments...), "config", "--quiet"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "upgrade_projection_invalid", Message: strings.TrimSpace(output), NextAction: "fix the Compose projection, then retry"})
	}
	helper.report.CompleteStage(stage, "Compose projection and Release manifest validated")

	stage = helper.report.BeginStage("upgrade-observe-prepared")
	overlay, err := deploycompose.RenderVerifyOverlay(loaded.projection, deploycompose.Options{Images: loaded.images})
	if err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "verifier_overlay_invalid", Message: err.Error(), NextAction: "fix the deployment projection, then retry"})
	}
	arguments := append(append([]string{}, loaded.composeArguments...), "--file", overlay, "run", "--rm", "--no-TTY", "--no-deps", "quoin-verifier", "--status", "200", "http://quoin:9090/metrics")
	read := func(name string) (deployupgrade.PreparedObservation, error) {
		body, err := helper.run(stage, name, dockerize(arguments)...)
		if err != nil {
			return deployupgrade.PreparedObservation{}, fmt.Errorf("verifier run: %w: %s", err, body)
		}
		return deployupgrade.ParsePrepared(body)
	}
	err = deployupgrade.ObservePrepared(deployupgrade.PreparedOptions{Read: read, WaitFor: flags.PreparedWait, OnEnter: func() {
		fmt.Fprintln(req.stdout(), "In the Admin Web UI, open 管理 → 维护与升级 and execute 准备升级维护; cancel remaining active work and wait for the verified pre-upgrade backup. This helper only observes /metrics and receives no Session.")
	}})
	if err != nil {
		if prepared, ok := err.(*deployupgrade.PreparedError); ok {
			return helper.failStage(req, stage, &PlatformError{Code: prepared.Code, Message: prepared.Message, NextAction: prepared.NextAction})
		}
		return helper.failStage(req, stage, &PlatformError{Code: "upgrade_prepared_unavailable", Message: err.Error(), NextAction: "ensure the verifier can reach the Quoin ops listener"})
	}
	helper.report.RecordCheck(preparedCheck())
	helper.report.CompleteStage(stage, "quoin_upgrade_prepared observed at 1 through the in-network verifier")

	fmt.Fprintf(req.stdout(), "Release %s will stop stele, plinth, lintel and quoin, verify the Upgrade fence offline, migrate with image %s and restart every component. Type UPGRADE to continue: ", loaded.release(), loaded.images["quoin"])
	var confirmation string
	if _, err := fmt.Fscan(req.Stdin, &confirmation); err != nil || confirmation != "UPGRADE" {
		return helper.failStage(req, stage, &PlatformError{Code: "upgrade_not_confirmed", Message: "upgrade was not confirmed; no workloads were stopped", NextAction: "rerun the upgrade command to resume from the prepared observation"})
	}

	stage = helper.report.BeginStage("upgrade-stop-workloads")
	if output, err := helper.run(stage, "upgrade-stop-workloads", dockerize(append(append([]string{}, loaded.composeArguments...), "stop", "stele", "plinth", "lintel", "quoin"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "upgrade_stop_failed", Message: strings.TrimSpace(output), NextAction: "ensure every Quoin workload is stopped, then rerun the upgrade command"})
	}
	helper.report.CompleteStage(stage, "stele, plinth, lintel and quoin stopped after quoin_upgrade_prepared=1")

	stage = helper.report.BeginStage("upgrade-offline-verify")
	preflight := append(append([]string{}, loaded.composeArguments...), "run", "--rm", "--no-deps", "quoin", "migrate", "preflight", "--config", "/etc/quoin/component.yaml")
	if output, err := helper.run(stage, "upgrade-migrate-preflight", dockerize(preflight)...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "upgrade_preflight_failed", Message: strings.TrimSpace(output), NextAction: "keep workloads stopped; the same-Release offline verification of the Upgrade fence failed — inspect the maintenance checklist or restore the pre-upgrade backup"})
	}
	helper.report.CompleteStage(stage, "deployed-image offline verification passed: Upgrade maintenance, checklist and pre-upgrade backup digest")

	stage = helper.report.BeginStage("upgrade-migrate")
	if _, err := deploycompose.RenderWithOptions(loaded.input, loaded.stateDir, deploycompose.Options{Images: loaded.images, DeploymentBinding: loaded.binding}); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "upgrade_reprojection_failed", Message: err.Error(), NextAction: "workloads are stopped; fix the projection and rerun the upgrade command"})
	}
	// The re-projection rewrites the same canonical generated compose.yaml, so
	// the captured arguments now address the new Release images.
	if output, err := helper.run(stage, "upgrade-migrate", dockerize(append(append([]string{}, loaded.composeArguments...), "run", "--rm", "--no-deps", "quoin", "migrate", "--config", "/etc/quoin/component.yaml"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "upgrade_migrate_failed", Message: strings.TrimSpace(output), NextAction: "workloads are stopped; the new version has not accepted writes — either fix the migration and rerun, or restart the old workloads (image-only rollback is still allowed)"})
	}
	helper.report.CompleteStage(stage, "fresh-v1 schema gate passed and the fully-verified Upgrade maintenance exited")

	stage = helper.report.BeginStage("upgrade-restart")
	if output, err := helper.run(stage, "upgrade-start-quoin", dockerize(append(append([]string{}, loaded.composeArguments...), "up", "--detach", "--no-deps", "quoin"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "upgrade_quoin_start_failed", Message: strings.TrimSpace(output), NextAction: "the new version accepted writes; fix Quoin or explicitly restore the pre-upgrade backup"})
	}
	if err := waitForComposeNormal(helper, loaded, stage); err != nil {
		return helper.failStage(req, stage, err)
	}
	if output, err := helper.run(stage, "upgrade-start-workloads", dockerize(append(append([]string{}, loaded.composeArguments...), "up", "--detach", "plinth", "lintel", "stele"))...); err != nil {
		return helper.failStage(req, stage, &PlatformError{Code: "upgrade_workloads_start_failed", Message: strings.TrimSpace(output), NextAction: "start same-Release workloads after Quoin reports normal readiness"})
	}
	if err := verifyOperationalSurface(req, loaded, helper, stage); err != nil {
		return helper.failStage(req, stage, errAsPlatform(err))
	}
	helper.report.CompleteStage(stage, "new Release Quoin Ready first, then same-Release plinth, lintel, stele; operational surface verified")
	helper.report.MarkSucceeded()
	return ExitSuccess
}

func preparedCheck() report.Check {
	return report.Check{ID: "upgrade-prepared", Result: "passed", Expected: "quoin_maintenance{reason=upgrade}=1, accepting_work=0, quoin_upgrade_prepared=1", Actual: "in-network verifier observed the prepared gauge", Code: "upgrade_prepared"}
}
