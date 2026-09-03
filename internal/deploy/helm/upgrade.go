package helm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	deployupgrade "github.com/Suknna/quoin/cmd/quoin-deploy/upgrade"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
)

// Upgrade executes the coordinated Helm upgrade (OPS-UPGRADE-002/004):
// observe quoin_upgrade_prepared through the same-Release verifier Pod, stop
// every workload behind an explicit confirmation, offline-verify the
// maintenance fence with the deployed (old) image, roll the Release forward
// to the manifest images, run the exclusive migration with the new image,
// then bring Quoin and the same-Release runtimes back in order and verify
// the operational surface.
func Upgrade(req Request, flags deployupgrade.Flags) int {
	if !isTerminal(req.Stdin) {
		return inputFailure(req, "upgrade", fmt.Errorf("upgrade requires an attached TTY; the destructive stop is explicitly confirmed"))
	}
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "upgrade", err)
	}
	if loaded.manifest == nil {
		return inputFailure(req, "upgrade", fmt.Errorf("the Helm backend requires --release-manifest for the coordinated upgrade"))
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "upgrade", err)
	}
	if err := stateDirectory(loaded.stateDir); err != nil {
		return inputFailure(req, "upgrade", err)
	}
	rep := report.New("helm", buildPlatform(), "upgrade", req.ConfigPath, digest)
	rep.Release = loaded.release()
	r := newRunner(rep)
	defer func() { _ = rep.Finish(reportTarget(req, loaded.stateDir)) }()
	namespace, release := helmNamespace(), helmReleaseName()

	stage := rep.BeginStage("upgrade-observe-prepared")
	read := func(label string) (deployupgrade.PreparedObservation, error) {
		body, err := verifyProbe(r, stage, namespace, release, "quoin", label, loaded.images["quoin"], "200", fmt.Sprintf("http://%s-quoin-ops:9090/metrics", release))
		if err != nil {
			return deployupgrade.PreparedObservation{}, err
		}
		return deployupgrade.ParsePrepared(body)
	}
	err = deployupgrade.ObservePrepared(deployupgrade.PreparedOptions{Read: read, WaitFor: flags.PreparedWait, OnEnter: func() {
		fmt.Fprintln(req.Stdout, "In the Admin Web UI, open 管理 → 维护与升级 and execute 准备升级维护; cancel remaining active work and wait for the verified pre-upgrade backup. This helper only observes /metrics and receives no Session.")
	}})
	if err != nil {
		if prepared, ok := err.(*deployupgrade.PreparedError); ok {
			return upgradeFailure(req, rep, stage, prepared.Code, prepared.Message, prepared.NextAction)
		}
		return upgradeFailure(req, rep, stage, "upgrade_prepared_unavailable", err.Error(), "ensure the verifier can reach the Quoin ops listener")
	}
	rep.RecordCheck(report.Check{ID: "upgrade-prepared", Result: "passed", Expected: "quoin_maintenance{reason=upgrade}=1, accepting_work=0, quoin_upgrade_prepared=1", Actual: "verifier Pod observed the prepared gauge", Code: "upgrade_prepared"})
	rep.CompleteStage(stage, "quoin_upgrade_prepared observed at 1 through the verifier Pod")

	stage = rep.BeginStage("upgrade-stop-workloads")
	fmt.Fprintf(req.Stdout, "Release %s will stop stele, plinth, lintel and quoin, verify the Upgrade fence offline, migrate with image %s and restart every component. Type UPGRADE to continue: ", loaded.release(), loaded.images["quoin"])
	var confirmation string
	if _, err := fmt.Fscan(req.Stdin, &confirmation); err != nil || confirmation != "UPGRADE" {
		return upgradeFailure(req, rep, stage, "upgrade_not_confirmed", "upgrade was not confirmed; no workloads were stopped", "rerun the upgrade command to resume from the prepared observation")
	}
	// OPS-UPGRADE-002 stops Stele, Plinth, Lintel and only then the old
	// Quoin — the drained-and-backed-up authority closes last.
	for _, component := range []string{"stele", "plinth", "lintel", "quoin"} {
		if output, err := r.run(stage, "upgrade-stop-"+component, kubectl(namespace, "scale", "deployment/"+release+"-"+component, "--replicas=0")...); err != nil {
			return upgradeFailure(req, rep, stage, "upgrade_stop_failed", strings.TrimSpace(output), "stop every release workload before retrying")
		}
	}
	for _, component := range []string{"stele", "plinth", "lintel", "quoin"} {
		selector := componentSelector(release, component)
		if output, err := r.run(stage, "upgrade-wait-"+component, kubectl(namespace, "wait", "--for=delete", "pod", "--selector="+selector, "--timeout=90s")...); err != nil {
			return upgradeFailure(req, rep, stage, "upgrade_workload_still_running", strings.TrimSpace(output), "wait for every release Pod to terminate and release its data lock")
		}
	}
	rep.CompleteStage(stage, "stele, plinth, lintel and quoin stopped after quoin_upgrade_prepared=1")

	stage = rep.BeginStage("upgrade-offline-verify")
	deployed, err := r.run(stage, "upgrade-deployed-image", kubectl(namespace, "get", "deployment/"+release+"-quoin", "--output", "jsonpath={.spec.template.spec.containers[0].image}")...)
	if err != nil {
		return upgradeFailure(req, rep, stage, "upgrade_image_unreadable", strings.TrimSpace(deployed), "inspect the Quoin deployment image and retry")
	}
	preflightName := release + "-upgrade-preflight"
	_, _ = r.run(stage, "upgrade-preflight-cleanup-old", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", preflightName)...)
	if output, err := r.runInput(stage, "upgrade-preflight-create", migratePod(preflightName, strings.TrimSpace(deployed), release, true), kubectl(namespace, "apply", "-f", "-")...); err != nil {
		return upgradeFailure(req, rep, stage, "upgrade_preflight_create_failed", strings.TrimSpace(output), "create the same-Release offline verification Pod before continuing")
	}
	phase, preflightErr := waitVerifierTerminal(r, stage, namespace, preflightName, "upgrade-preflight")
	logs, _ := r.run(stage, "upgrade-preflight-logs", kubectl(namespace, "logs", "pod/"+preflightName)...)
	_, _ = r.run(stage, "upgrade-preflight-cleanup", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", preflightName)...)
	if preflightErr != nil || phase != "Succeeded" {
		return upgradeFailure(req, rep, stage, "upgrade_preflight_failed", strings.TrimSpace(logs), "keep workloads stopped; the same-Release offline verification of the Upgrade fence failed — inspect the maintenance checklist or restore the pre-upgrade backup")
	}
	rep.CompleteStage(stage, "deployed-image offline verification passed: Upgrade maintenance, checklist and pre-upgrade backup digest")

	stage = rep.BeginStage("upgrade-migrate")
	if failure := rollReleaseImages(req, r, rep, loaded, namespace, release, stage); failure != nil {
		return upgradeFailure(req, rep, stage, failure.code, failure.message, "workloads are stopped; fix the release roll and rerun the upgrade command")
	}
	migrateName := release + "-upgrade-migrate"
	_, _ = r.run(stage, "upgrade-migrate-cleanup-old", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", migrateName)...)
	if output, err := r.runInput(stage, "upgrade-migrate-create", migratePod(migrateName, loaded.images["quoin"], release, false), kubectl(namespace, "apply", "-f", "-")...); err != nil {
		return upgradeFailure(req, rep, stage, "upgrade_migrate_create_failed", strings.TrimSpace(output), "workloads are stopped; retry the migration Pod with the new Release image")
	}
	phase, migrateErr := waitVerifierTerminal(r, stage, namespace, migrateName, "upgrade-migrate")
	logs, _ = r.run(stage, "upgrade-migrate-logs", kubectl(namespace, "logs", "pod/"+migrateName)...)
	_, _ = r.run(stage, "upgrade-migrate-cleanup", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", migrateName)...)
	if migrateErr != nil || phase != "Succeeded" {
		return upgradeFailure(req, rep, stage, "upgrade_migrate_failed", strings.TrimSpace(logs), "workloads are stopped; the new version has not accepted writes — either fix the migration and rerun, or roll back and explicitly restore the pre-upgrade backup")
	}
	rep.CompleteStage(stage, "fresh-v1 schema gate passed and the fully-verified Upgrade maintenance exited")

	stage = rep.BeginStage("upgrade-restart")
	if output, err := r.run(stage, "upgrade-start-quoin", kubectl(namespace, "scale", "deployment/"+release+"-quoin", "--replicas=1")...); err != nil {
		return upgradeFailure(req, rep, stage, "upgrade_quoin_start_failed", strings.TrimSpace(output), "the new version accepted writes; fix Quoin or explicitly restore the pre-upgrade backup")
	}
	if failure := waitForHelmNormal(r, rep, loaded, namespace, release, stage); failure != nil {
		return upgradeFailure(req, rep, stage, failure.code, failure.message, "do not start other workloads until Quoin reports normal readiness")
	}
	for _, component := range []string{"plinth", "lintel", "stele"} {
		if output, err := r.run(stage, "upgrade-start-"+component, kubectl(namespace, "scale", "deployment/"+release+"-"+component, "--replicas=1")...); err != nil {
			return upgradeFailure(req, rep, stage, "upgrade_workloads_start_failed", strings.TrimSpace(output), "start same-Release workloads only after Quoin reports normal readiness")
		}
	}
	if failure := verifyOperationalSurface(req, r, rep, loaded, namespace, release, stage); failure != nil {
		return upgradeFailure(req, rep, stage, failure.code, failure.message, "inspect post-upgrade verification and retry")
	}
	rep.CompleteStage(stage, "new Release Quoin Ready first, then same-Release plinth, lintel, stele; operational surface verified")
	rep.MarkSucceeded()
	return 0
}

// migratePod is the one-shot migration Pod: preflight runs the deployed
// image's read-only verification command; the exclusive step runs the new
// image's migration. Workloads are scaled to zero, so the data PVC is free.
func migratePod(name, image, release string, preflight bool) string {
	// preflight runs `migrate preflight`; the exclusive step is plain
	// `migrate` (no positional subcommand).
	step := ""
	if preflight {
		step = "preflight"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s}
spec:
  restartPolicy: Never
  containers:
    - name: quoin-migrate
      image: %s
      command: ["/quoin"]
      args: %s
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: secrets, mountPath: /run/quoin-secrets, readOnly: true}
  volumes:
    - {name: config, configMap: {name: %s-component-quoin}}
    - {name: data, persistentVolumeClaim: {claimName: %s-quoin-data}}
    - {name: secrets, secret: {secretName: %s-secrets}}
`, name, image, argsLine(step), release, release, release)
}

func argsLine(step string) string {
	if step == "" {
		return `["migrate", "--config", "/etc/quoin/component.yaml"]`
	}
	return `["migrate", "` + step + `", "--config", "/etc/quoin/component.yaml"]`
}

// rollReleaseImages rolls the Helm release to the manifest's digest-pinned
// images while every replica is still zero. It reuses the install path's
// chart reference and values projection so no second image table can exist
// (OPS-UPGRADE-001/002).
func rollReleaseImages(req Request, r *runner, rep *report.Report, loaded *loadedRequest, namespace, release string, stage int) *verifyError {
	chartRef, err := helmChartRef(loaded.manifest)
	if err != nil {
		return verifyFail("upgrade_chart_ref_invalid", "%s", err.Error())
	}
	values, valuesErr := chartValues(loaded.input, loaded.images)
	if valuesErr != nil {
		return verifyFail("upgrade_values_render_failed", "%s", valuesErr.Error())
	}
	valuesPath := filepath.Join(loaded.stateDir, "values.yaml")
	if writeErr := os.WriteFile(valuesPath, values, 0o600); writeErr != nil {
		return verifyFail("upgrade_values_write_failed", "%s", writeErr.Error())
	}
	// The workloads stay scaled to zero; the roll only re-points the
	// immutable chart digest and the digest-pinned images.
	if output, runErr := r.run(stage, "upgrade-helm-roll",
		"helm", "upgrade", release, chartRef,
		"--namespace", namespace,
		"--values", valuesPath); runErr != nil {
		return verifyFail("upgrade_helm_roll_failed", "%s", strings.TrimSpace(output))
	}
	return nil
}

func upgradeFailure(req Request, rep *report.Report, stage int, code, message, next string) int {
	rep.FailStage(stage, message)
	rep.MarkFailed(code, message, next)
	// The operator transcript carries the same stable code as the report.
	fmt.Fprintf(req.Stderr, "quoin-deploy: upgrade failed (%s): %s\nnext action: %s\n", code, message, next)
	return 1
}
