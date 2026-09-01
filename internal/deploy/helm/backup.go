package helm

import (
	"fmt"
	"strings"

	deploybackup "github.com/Suknna/quoin/internal/deploy/backup"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
)

// Backup is the Helm counterpart of the Compose credential-free backup
// protocol. Online observation reads only the cluster-internal ops Service;
// offline fallback uses the running release image with its retained PVCs.
func Backup(req Request, offline bool) int {
	loaded, err := load(req)
	if err != nil {
		return inputFailure(req, "backup", err)
	}
	if loaded.manifest == nil {
		return inputFailure(req, "backup", fmt.Errorf("the Helm backend requires --release-manifest for same-Release backup execution"))
	}
	digest, err := deployconfig.DigestFile(req.ConfigPath)
	if err != nil {
		return inputFailure(req, "backup", err)
	}
	if err = stateDirectory(loaded.stateDir); err != nil {
		return inputFailure(req, "backup", err)
	}
	rep := report.New("helm", buildPlatform(), "backup", req.ConfigPath, digest)
	rep.Release = loaded.release()
	r := newRunner(rep)
	defer func() { _ = rep.Finish(reportTarget(req, loaded.stateDir)) }()
	namespace, release := helmNamespace(), helmReleaseName()
	stage := rep.BeginStage("backup-observation")
	if offline {
		return offlineBackup(req, r, rep, stage, namespace, release, loaded)
	}
	return onlineBackup(r, rep, stage, namespace, release, loaded)
}
func onlineBackup(r *runner, rep *report.Report, stage int, namespace, release string, loaded *loadedRequest) int {
	service := fmt.Sprintf("%s-quoin-ops", release)
	read := func(label string) (deploybackup.Observation, error) {
		body, err := verifyProbe(r, stage, namespace, release, "quoin", label, loaded.images["quoin"], "", fmt.Sprintf("http://%s:9090/metrics", service))
		if err != nil {
			return deploybackup.Observation{}, err
		}
		return deploybackup.ParseObservation(body)
	}
	err := deploybackup.Observe(deploybackup.OnlineOptions{Read: read, OnReady: func() {
		fmt.Println("Quoin is available. Ask the logged-in Admin to select 立即备份; this helper observes only ops metrics and has no Web credentials.")
	}})
	if err != nil {
		if observation, ok := err.(*deploybackup.ObservationError); ok {
			return backupFailure(rep, stage, observation.Code, observation.Message, observation.NextAction)
		}
		return backupFailure(rep, stage, "online_backup_unavailable", err.Error(), "retry after Quoin is ready")
	}
	rep.CompleteStage(stage, "online manual backup observed through metrics")
	rep.MarkSucceeded()
	return 0
}
func offlineBackup(req Request, r *runner, rep *report.Report, stage int, namespace, release string, loaded *loadedRequest) int {
	// An offline fallback may begin only after the isolated verifier could not
	// reach Quoin's ops listener; a successful probe proves the helper must use
	// the online protocol instead of disrupting a healthy release.
	service := fmt.Sprintf("%s-quoin-ops", release)
	if output, probeErr := verifyProbe(r, stage, namespace, release, "quoin", "backup-prove-quoin-unavailable", loaded.images["quoin"], "", fmt.Sprintf("http://%s:9090/metrics", service)); probeErr == nil {
		return backupFailure(rep, stage, "offline_backup_requires_unavailable", "Quoin remains reachable through its ops listener", "use online backup while Quoin is reachable; --offline is only for an unavailable Quoin")
	} else if !deploybackup.UnavailabilityProven(output, probeErr) {
		return backupFailure(rep, stage, "offline_backup_unavailability_unproven", "the verifier failure was not an explicit network-unavailable result", "resolve verifier execution and retry; do not stop workloads without proving Quoin unavailable")
	}
	for _, component := range deployconfig.Components {
		if output, err := r.run(stage, "backup-stop-"+component, kubectl(namespace, "scale", "deployment/"+release+"-"+component, "--replicas=0")...); err != nil {
			return backupFailure(rep, stage, "offline_backup_stop_failed", strings.TrimSpace(output), "stop all workloads before retrying offline backup")
		}
	}
	// Every workload sharing the release must have reached zero Pods before the
	// offline helper opens Quoin's data PVC. Waiting only for Quoin leaves a
	// still-running Runtime or browser component outside the proven stop fence.
	for _, component := range deployconfig.Components {
		selector := "app.kubernetes.io/name=" + component + ",app.kubernetes.io/instance=" + release + ",app.kubernetes.io/component=" + component
		if output, err := r.run(stage, "backup-wait-"+component+"-terminated", kubectl(namespace, "wait", "--for=delete", "pod", "--selector="+selector, "--timeout=90s")...); err != nil {
			return backupFailure(rep, stage, "offline_backup_workload_still_running", strings.TrimSpace(output), "wait until every release workload is gone and its data lock is released before retrying offline backup")
		}
	}
	imageOutput, err := r.run(stage, "backup-image", kubectl(namespace, "get", "deployment/"+release+"-quoin", "--output", "jsonpath={.spec.template.spec.containers[0].image}")...)
	if err != nil {
		return backupFailure(rep, stage, "offline_backup_image_unreadable", strings.TrimSpace(imageOutput), "inspect the Quoin deployment image and retry")
	}
	name := release + "-backup-offline"
	_, _ = r.run(stage, "backup-cleanup-old", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", name)...)
	manifest := offlineBackupPod(name, strings.TrimSpace(imageOutput), release)
	if output, err := r.runInput(stage, "backup-create", manifest, kubectl(namespace, "apply", "-f", "-")...); err != nil {
		return backupFailure(rep, stage, "offline_backup_create_failed", strings.TrimSpace(output), "keep workloads stopped and retry the offline backup Pod")
	}
	phase, err := waitVerifierTerminal(r, stage, namespace, name, "backup-offline")
	logs, _ := r.run(stage, "backup-logs", kubectl(namespace, "logs", "pod/"+name)...)
	_, _ = r.run(stage, "backup-cleanup", kubectl(namespace, "delete", "--ignore-not-found=true", "pod", name)...)
	if err != nil || phase != "Succeeded" {
		return backupFailure(rep, stage, "offline_backup_failed", strings.TrimSpace(logs), "keep workloads stopped, inspect the offline backup error and retry")
	}
	rep.CompleteStage(stage, "workloads stopped and offline backup Pod succeeded")
	verifyStage := rep.BeginStage("backup-post-verify")
	for _, component := range deployconfig.Components {
		if output, err := r.run(verifyStage, "backup-restart-"+component, kubectl(namespace, "scale", "deployment/"+release+"-"+component, "--replicas=1")...); err != nil {
			return backupFailure(rep, verifyStage, "offline_backup_restart_failed", strings.TrimSpace(output), "start stopped workloads then run quoin-deploy helm verify")
		}
	}
	if failure := verifyOperationalSurface(req, r, rep, loaded, namespace, release, verifyStage); failure != nil {
		return backupFailure(rep, verifyStage, failure.code, failure.message, "inspect the post-backup verifier and retry")
	}
	rep.CompleteStage(verifyStage, "post-backup operational surface verified")
	rep.MarkSucceeded()
	return 0
}
func offlineBackupPod(name, image, release string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s}
spec:
  restartPolicy: Never
  containers:
    - name: quoin-backup
      image: %s
      command: ["/quoin"]
      args: ["backup", "--offline", "--config", "/etc/quoin/component.yaml"]
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: backups, mountPath: /var/lib/quoin/backups}
        - {name: secrets, mountPath: /run/quoin-secrets, readOnly: true}
        - {name: tmp, mountPath: /tmp}
  volumes:
    - {name: config, configMap: {name: %s-component-quoin}}
    - {name: data, persistentVolumeClaim: {claimName: %s-quoin-data}}
    - {name: backups, persistentVolumeClaim: {claimName: %s-quoin-backups}}
    - {name: secrets, secret: {secretName: %s-secrets}}
    - {name: tmp, emptyDir: {}}
`, name, image, release, release, release, release)
}
func backupFailure(rep *report.Report, stage int, code, message, next string) int {
	rep.FailStage(stage, message)
	rep.MarkFailed(code, message, next)
	return 1
}
