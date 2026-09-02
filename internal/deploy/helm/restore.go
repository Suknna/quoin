package helm

import (
	"fmt"
	"strings"
	"time"

	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

// Restore runs a same-release, attached-TTY restore Pod against retained data
// and backup PVCs. All deployments remain at zero except Quoin, which is then
// started alone to expose only its maintenance-safe HTTP and ops surfaces.
func Restore(req Request, backupID string) int {
	if backupID == "" || strings.Trim(backupID, "0123456789") != "" {
		return inputFailure(req, "restore", fmt.Errorf("restore requires numeric --backup <published-backup-id>"))
	}
	if !isTerminal(req.Stdin) {
		return inputFailure(req, "restore", fmt.Errorf("restore requires an attached TTY; the temporary recovery password is never accepted from stdin"))
	}
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "restore", err)
	}
	if loaded.manifest == nil {
		return inputFailure(req, "restore", fmt.Errorf("the Helm backend requires --release-manifest for same-Release restore"))
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "restore", err)
	}
	if err := stateDirectory(loaded.stateDir); err != nil {
		return inputFailure(req, "restore", err)
	}
	rep := report.New("helm", buildPlatform(), "restore", req.ConfigPath, digest)
	rep.Release = loaded.release()
	r := newRunner(rep)
	defer func() { _ = rep.Finish(reportTarget(req, loaded.stateDir)) }()
	namespace, release := helmNamespace(), helmReleaseName()
	stage := rep.BeginStage("restore-preflight")
	image, err := r.run(stage, "restore-image", kubectl(namespace, "get", "deployment/"+release+"-quoin", "--output", "jsonpath={.spec.template.spec.containers[0].image}")...)
	if err != nil {
		return restoreFailure(rep, stage, "restore_image_unreadable", strings.TrimSpace(image), "inspect the Quoin deployment image and retry")
	}
	if strings.TrimSpace(image) != loaded.images["quoin"] {
		return restorePreflightFailure(rep, stage, "restore_image_release_mismatch", strings.TrimSpace(image), "restore only with the exact digest-pinned Quoin image from the supplied Release Manifest")
	}
	// Preflight uses a same-Release one-shot Pod rather than `kubectl exec`: a
	// restore must remain possible when the long-lived Quoin Pod cannot start.
	preflightName := release + "-restore-preflight"
	_, _ = r.run(stage, "restore-preflight-cleanup-old", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", preflightName)...)
	if output, err := r.runInput(stage, "restore-backup-preflight-create", offlinePreflightPod(preflightName, strings.TrimSpace(image), release, backupID), kubectl(namespace, "apply", "-f", "-")...); err != nil {
		return restoreFailure(rep, stage, "restore_backup_preflight_create_failed", strings.TrimSpace(output), "create the same-Release backup preflight Pod before stopping workloads")
	}
	phase, preflightErr := waitVerifierTerminal(r, stage, namespace, preflightName, "restore-backup-preflight")
	manifestSummary, logsErr := r.run(stage, "restore-backup-preflight-logs", kubectl(namespace, "logs", "pod/"+preflightName)...)
	_, _ = r.run(stage, "restore-backup-preflight-cleanup", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", preflightName)...)
	if preflightErr != nil {
		return restoreFailure(rep, stage, "restore_backup_preflight_unavailable", strings.TrimSpace(manifestSummary), "repair Kubernetes preflight execution before stopping workloads")
	}
	if logsErr != nil || strings.TrimSpace(manifestSummary) == "" {
		return restoreFailure(rep, stage, "restore_backup_preflight_logs_unreadable", strings.TrimSpace(manifestSummary), "read the verified backup manifest summary before stopping workloads")
	}
	if phase != "Succeeded" {
		return restorePreflightFailure(rep, stage, "restore_backup_invalid", strings.TrimSpace(manifestSummary), "choose a verified backup from this exact Release before stopping workloads")
	}
	fmt.Fprintf(req.Stdout, "Restore release %s from backup %s using Quoin image %s. Verified target manifest: %s. This scales quoin, plinth, lintel, and stele to zero. Type RESTORE to continue: ", loaded.release(), backupID, loaded.images["quoin"], strings.TrimSpace(manifestSummary))
	var confirmation string
	if _, err := fmt.Fscan(req.Stdin, &confirmation); err != nil || confirmation != "RESTORE" {
		return restoreFailure(rep, stage, "restore_not_confirmed", "restore was not confirmed; no workloads were stopped", "rerun restore and type RESTORE after reviewing the target")
	}
	rep.CompleteStage(stage, "same Release Quoin image selected; destructive stop explicitly confirmed")
	stage = rep.BeginStage("restore-offline")
	for _, component := range deployconfig.Components {
		if output, err := r.run(stage, "restore-stop-"+component, kubectl(namespace, "scale", "deployment/"+release+"-"+component, "--replicas=0")...); err != nil {
			return restoreFailure(rep, stage, "restore_stop_failed", strings.TrimSpace(output), "stop every release workload before retrying")
		}
	}
	for _, component := range deployconfig.Components {
		selector := componentSelector(release, component)
		if output, err := r.run(stage, "restore-wait-"+component, kubectl(namespace, "wait", "--for=delete", "pod", "--selector="+selector, "--timeout=90s")...); err != nil {
			return restoreFailure(rep, stage, "restore_workload_still_running", strings.TrimSpace(output), "wait for every release Pod to terminate and release its data lock")
		}
	}
	// The one-shot continuation Pod can succeed only when the stopped data
	// volume contains both active Restore maintenance and this backup-ID-bound
	// rollback point. An absent marker is the only route to a first restore.
	continuationName := release + "-restore-continuation"
	_, _ = r.run(stage, "restore-continuation-cleanup-old", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", continuationName)...)
	if output, err := r.runInput(stage, "restore-continuation-create", offlineContinuePod(continuationName, strings.TrimSpace(image), release, backupID), kubectl(namespace, "apply", "-f", "-")...); err != nil {
		return restoreFailure(rep, stage, "restore_continuation_create_failed", strings.TrimSpace(output), "keep workloads stopped and inspect the Restore continuation fence")
	}
	continuationPhase, continuationErr := waitVerifierTerminal(r, stage, namespace, continuationName, "restore-continuation")
	continuationLogs, _ := r.run(stage, "restore-continuation-logs", kubectl(namespace, "logs", "pod/"+continuationName)...)
	if summary := strings.TrimSpace(continuationLogs); summary != "" {
		// The one-shot Pod's non-secret fence receipt belongs in the operator
		// transcript and evidence just like Compose's direct container output.
		fmt.Fprintln(req.Stdout, summary)
	}
	_, _ = r.run(stage, "restore-continuation-cleanup", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", continuationName)...)
	resumed := continuationErr == nil && continuationPhase == "Succeeded"
	if !resumed && !strings.Contains(continuationLogs, "restore_continuation=absent") {
		return restoreFailure(rep, stage, "restore_continuation_invalid", strings.TrimSpace(continuationLogs), "keep workloads stopped and repair the published Restore maintenance fence before retrying")
	}
	if !resumed {
		name := release + "-restore-offline"
		_, _ = r.run(stage, "restore-cleanup-old", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", name)...)
		if output, err := r.runInput(stage, "restore-create", offlineRestorePod(name, strings.TrimSpace(image), release, backupID), kubectl(namespace, "apply", "-f", "-")...); err != nil {
			return restoreFailure(rep, stage, "restore_create_failed", strings.TrimSpace(output), "keep workloads stopped and retry the restore Pod")
		}
		if output, err := r.run(stage, "restore-wait-pod-ready", kubectl(namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=60s")...); err != nil {
			return restoreFailure(rep, stage, "restore_pod_not_ready", strings.TrimSpace(output), "keep workloads stopped and inspect the restore Pod before retrying")
		}
		if output, err := r.runInteractive(stage, "restore-attach", req.Stdin, req.Stdout, req.Stderr, kubectl(namespace, "attach", "--stdin", "--tty", "--container=quoin-restore", "pod/"+name)...); err != nil {
			_, _ = r.run(stage, "restore-logs", kubectl(namespace, "logs", "pod/"+name)...)
			return restoreFailure(rep, stage, "restore_failed", strings.TrimSpace(output), "keep workloads stopped; inspect restore logs and retry")
		}
		phase, err = waitVerifierTerminal(r, stage, namespace, name, "restore-offline")
		logs, _ := r.run(stage, "restore-logs", kubectl(namespace, "logs", "pod/"+name)...)
		_, _ = r.run(stage, "restore-cleanup", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", name)...)
		if err != nil || phase != "Succeeded" {
			return restoreFailure(rep, stage, "restore_failed", strings.TrimSpace(logs), "keep workloads stopped and retry")
		}
		rep.CompleteStage(stage, "verified restore and isolation transaction completed before publication")
	} else {
		rep.CompleteStage(stage, "resumed published Restore maintenance using durable rollback fence")
	}
	stage = rep.BeginStage("restore-maintenance")
	if output, err := r.run(stage, "restore-start-quoin-maintenance", kubectl(namespace, "scale", "deployment/"+release+"-quoin", "--replicas=1")...); err != nil {
		return restoreFailure(rep, stage, "restore_maintenance_start_failed", strings.TrimSpace(output), "start Quoin alone and inspect its maintenance /readyz response")
	}
	if failure := waitForHelmMaintenance(r, rep, loaded, namespace, release, stage); failure != nil {
		return restoreFailure(rep, stage, failure.code, failure.message, "keep other workloads stopped and inspect Quoin maintenance readiness")
	}
	rep.CompleteStage(stage, "Quoin alone reached restore isolation")

	fmt.Fprint(req.Stdout, "Complete the Restore checklist and exit maintenance in Quoin, then type CONTINUE to restart Quoin in normal mode: ")
	var continueRestore string
	if _, err := fmt.Fscan(req.Stdin, &continueRestore); err != nil || continueRestore != "CONTINUE" {
		return restoreFailure(rep, stage, "restore_not_completed", "maintenance checklist was not confirmed complete", "complete the checklist and rerun restore")
	}
	stage = rep.BeginStage("restore-post-verify")
	quoinSelector := componentSelector(release, "quoin")
	if output, err := r.run(stage, "restore-restart-quoin", kubectl(namespace, "delete", "pod", "--selector="+quoinSelector)...); err != nil {
		return restoreFailure(rep, stage, "restore_normal_restart_failed", strings.TrimSpace(output), "restart Quoin after exiting maintenance")
	}
	if failure := waitForHelmNormal(r, rep, loaded, namespace, release, stage); failure != nil {
		return restoreFailure(rep, stage, failure.code, failure.message, "do not start other workloads until Quoin exits maintenance and reports normal readiness")
	}
	for _, component := range []string{"plinth", "lintel", "stele"} {
		if output, err := r.run(stage, "restore-start-"+component, kubectl(namespace, "scale", "deployment/"+release+"-"+component, "--replicas=1")...); err != nil {
			return restoreFailure(rep, stage, "restore_workloads_start_failed", strings.TrimSpace(output), "start same-Release workloads only after Quoin reports normal readiness")
		}
	}
	if failure := verifyOperationalSurface(req, r, rep, loaded, namespace, release, stage); failure != nil {
		return restoreFailure(rep, stage, failure.code, failure.message, "inspect post-restore verification and retry")
	}
	rollback := ".restore-rollback-" + backupID
	if output, err := r.run(stage, "restore-finalize-rollback", kubectl(namespace, "exec", "deployment/"+release+"-quoin", "--", "/quoin", "restore", "finalize", "--rollback", rollback, "--config", "/etc/quoin/component.yaml")...); err != nil {
		return restoreFailure(rep, stage, "restore_rollback_finalize_failed", strings.TrimSpace(output), "keep the rollback point and inspect post-restore cleanup")
	}
	rep.CompleteStage(stage, "post-restore operational verification completed and rollback point finalized")
	rep.MarkSucceeded()
	return 0
}

func waitForHelmMaintenance(r *runner, rep *report.Report, loaded *loadedRequest, namespace, release string, stage int) *verifyError {
	var last string
	for attempt := 0; attempt < 60; attempt++ {
		body, err := verifyProbe(r, stage, namespace, release, "quoin", "restore-maintenance-readyz", loaded.images["quoin"], "200", fmt.Sprintf("http://%s-quoin-ops:9090/readyz", release))
		last = body
		var readiness sharedops.Readiness
		if err == nil && firstJSONField(body, &readiness) == nil && readiness.Mode == "maintenance" && !readiness.AcceptingWork && readiness.Reason == sharedops.Maintenance {
			rep.RecordCheck(report.Check{ID: "restore-maintenance-ready", Result: "passed", Expected: "mode=maintenance acceptingWork=false reason=maintenance", Actual: "Quoin maintenance-safe readiness", Code: "restore_maintenance_ready"})
			return nil
		}
		time.Sleep(time.Second)
	}
	return verifyFail("restore_maintenance_not_ready", "%s", strings.TrimSpace(last))
}

func waitForHelmNormal(r *runner, rep *report.Report, loaded *loadedRequest, namespace, release string, stage int) *verifyError {
	var last string
	for attempt := 0; attempt < 60; attempt++ {
		body, err := verifyProbe(r, stage, namespace, release, "quoin", "restore-normal-readyz", loaded.images["quoin"], "200", fmt.Sprintf("http://%s-quoin-ops:9090/readyz", release))
		last = body
		var readiness sharedops.Readiness
		if err == nil && firstJSONField(body, &readiness) == nil && readiness.Mode == "normal" && readiness.AcceptingWork && readiness.Reason == sharedops.Ready {
			rep.RecordCheck(report.Check{ID: "restore-normal-ready", Result: "passed", Expected: "mode=normal acceptingWork=true reason=ready", Actual: "Quoin normal readiness after maintenance exit", Code: "restore_normal_ready"})
			return nil
		}
		time.Sleep(time.Second)
	}
	return verifyFail("restore_normal_not_ready", "%s", strings.TrimSpace(last))
}

func offlinePreflightPod(name, image, release, backupID string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s}
spec:
  restartPolicy: Never
  containers:
    - name: quoin-preflight
      image: %s
      command: ["/quoin"]
      args: ["restore", "preflight", "--backup", "%s", "--config", "/etc/quoin/component.yaml"]
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: backups, mountPath: /var/lib/quoin/backups, readOnly: true}
  volumes:
    - {name: config, configMap: {name: %s-component-quoin}}
    - {name: backups, persistentVolumeClaim: {claimName: %s-quoin-backups}}
`, name, image, backupID, release, release)
}

func offlineContinuePod(name, image, release, backupID string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s}
spec:
  restartPolicy: Never
  containers:
    - name: quoin-continuation
      image: %s
      command: ["/quoin"]
      args: ["restore", "continue", "--backup", "%s", "--config", "/etc/quoin/component.yaml"]
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: secrets, mountPath: /run/quoin-secrets, readOnly: true}
  volumes:
    - {name: config, configMap: {name: %s-component-quoin}}
    - {name: data, persistentVolumeClaim: {claimName: %s-quoin-data}}
    - {name: secrets, secret: {secretName: %s-secrets}}
`, name, image, backupID, release, release, release)
}

func offlineRestorePod(name, image, release, backupID string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s}
spec:
  restartPolicy: Never
  containers:
    - name: quoin-restore
      image: %s
      stdin: true
      tty: true
      command: ["/quoin"]
      args: ["restore", "--backup", "%s", "--config", "/etc/quoin/component.yaml"]
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: backups, mountPath: /var/lib/quoin/backups}
        - {name: secrets, mountPath: /run/quoin-secrets, readOnly: true}
  volumes:
    - {name: config, configMap: {name: %s-component-quoin}}
    - {name: data, persistentVolumeClaim: {claimName: %s-quoin-data}}
    - {name: backups, persistentVolumeClaim: {claimName: %s-quoin-backups}}
    - {name: secrets, secret: {secretName: %s-secrets}}
`, name, image, backupID, release, release, release, release)
}

func restorePreflightFailure(rep *report.Report, stage int, code, message, next string) int {
	rep.FailStage(stage, message)
	rep.MarkFailed(code, message, next)
	rep.ExitCode = 2
	return 2
}

func restoreFailure(rep *report.Report, stage int, code, message, next string) int {
	rep.FailStage(stage, message)
	rep.MarkFailed(code, message, next)
	return 1
}
