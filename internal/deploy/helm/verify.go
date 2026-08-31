package helm

import (
	"fmt"
	"strings"
	"time"

	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

// verifyError carries the stable failure code of one operational check.
type verifyError struct {
	code    string
	message string
}

func (err *verifyError) Error() string { return err.message }

func verifyFail(code, format string, arguments ...any) *verifyError {
	return &verifyError{code: code, message: fmt.Sprintf(format, arguments...)}
}

// verifyOperationalSurface is the single verifier used by install's final
// stage and by the standalone verify command: image digest pinning, liveness,
// the frozen readiness contract, the frozen metrics catalog, JSON logs, the
// fixed volume topology and the fixed service topology — all observed through
// one-shot in-network verifier pods so ops listeners are never published.
func verifyOperationalSurface(req Request, r *runner, rep *report.Report, loaded *loadedRequest, namespace, release string, stage int) *verifyError {
	for _, component := range deployconfig.Components {
		if failure := judgeDeploymentImage(r, stage, namespace, release, component, loaded.images[component]); failure != nil {
			return failure
		}
		rep.RecordCheck(report.Check{ID: "image-digest-" + component, Result: "passed", Expected: loaded.images[component], Actual: loaded.images[component], Code: "image_digest_pinned"})

		service := fmt.Sprintf("%s-%s-ops", release, component)
		livez, livezErr := verifyEventually(r, stage, namespace, release, component, "livez", loaded.images["quoin"], "", fmt.Sprintf("http://%s:9090/livez", service))
		if livezErr != nil {
			return verifyFail("livez_failed", "%s: %s", component, strings.TrimSpace(livez))
		}
		body, probeErr := verifyProbe(r, stage, namespace, release, component, "readyz", loaded.images["quoin"], "503", fmt.Sprintf("http://%s:9090/readyz", service))
		var readiness sharedops.Readiness
		parseErr := firstJSONField(body, &readiness)
		// A ready or maintenance component answers 200, which intentionally
		// fails the 503 probe; confirm that positive path explicitly, as the
		// Compose verifier does.
		if parseErr != nil || readiness.Reason == sharedops.Ready || readiness.Mode == "maintenance" {
			confirm, confirmErr := verifyProbe(r, stage, namespace, release, component, "readyz-ok", loaded.images["quoin"], "200", fmt.Sprintf("http://%s:9090/readyz", service))
			if confirmErr != nil {
				if parseErr != nil {
					return verifyFail("readiness_unreadable", "%s: %s | 200-probe: %s", component, strings.TrimSpace(body), strings.TrimSpace(confirm))
				}
				return verifyFail("readiness_status_incoherent", "%s reports %q but does not answer 200", component, readiness.Reason)
			}
			body = confirm
			if parseErr := firstJSONField(body, &readiness); parseErr != nil {
				return verifyFail("readiness_unreadable", "%s: %s", component, strings.TrimSpace(confirm))
			}
		} else if probeErr != nil {
			return verifyFail("readiness_failed", "%s: %s", component, strings.TrimSpace(body))
		}
		if failure := judgeReadiness(component, loaded.release(), readiness); failure != nil {
			rep.RecordCheck(report.Check{ID: "readiness-" + component, Result: "failed", Expected: "frozen readiness contract", Actual: fmt.Sprintf("%+v", readiness), Code: failure.code, Recovery: "inspect the component logs; the frozen readiness contract is mandatory"})
			return failure
		}
		rep.RecordCheck(report.Check{ID: "readiness-" + component, Result: "passed", Expected: "frozen readiness contract", Actual: fmt.Sprintf("reason=%s mode=%s", readiness.Reason, readiness.Mode), Code: "readiness_contract"})

		metrics, metricsErr := verifyProbe(r, stage, namespace, release, component, "metrics", loaded.images["quoin"], "200", fmt.Sprintf("http://%s:9090/metrics", service))
		if metricsErr != nil {
			return verifyFail("metrics_unreadable", "%s: %s", component, strings.TrimSpace(metrics))
		}
		if failure := judgeMetrics(component, metrics); failure != nil {
			rep.RecordCheck(report.Check{ID: "metrics-" + component, Result: "failed", Expected: "frozen metrics catalog", Actual: failure.message, Code: failure.code, Recovery: "redeploy the component from the release manifest"})
			return failure
		}
		rep.RecordCheck(report.Check{ID: "metrics-" + component, Result: "passed", Expected: "frozen metrics catalog", Actual: "exact family and closed-label equality", Code: "metrics_catalog"})

		logs, logsErr := r.run(stage, "verify-logs-"+component, kubectl(namespace, "logs", "deployment/"+release+"-"+component, "--container", component, "--tail=200")...)
		if logsErr != nil {
			return verifyFail("logs_unreadable", "%s: %v", component, logsErr)
		}
		if failure := judgeLogs(component, logs, loaded.release()); failure != nil {
			rep.RecordCheck(report.Check{ID: "logs-" + component, Result: "failed", Expected: "JSON Lines with frozen fields", Actual: failure.message, Code: failure.code, Recovery: "the frozen OPS-LOG-001 field set is mandatory"})
			return failure
		}
		rep.RecordCheck(report.Check{ID: "logs-" + component, Result: "passed", Expected: "JSON Lines with frozen fields", Actual: "every line JSON with ts/level/component/release/code/msg", Code: "logs_json"})
	}
	if failure := judgeVolumeTopology(r, stage, namespace, release); failure != nil {
		return failure
	}
	for _, claim := range retainedPVCNames(release) {
		output, err := r.run(stage, "verify-pvc-bound-"+claim, kubectl(namespace, "get", "pvc", claim, "--output", "jsonpath={.status.phase}")...)
		if err != nil {
			return verifyFail("volume_topology_unreadable", "%s: %s", claim, strings.TrimSpace(output))
		}
		if strings.TrimSpace(output) != "Bound" {
			return verifyFail("pvc_not_bound", "retained PVC %s is %q, want Bound", claim, strings.TrimSpace(output))
		}
	}
	return judgeServiceTopology(r, stage, namespace, release)
}

// verifyProbe runs one one-shot verifier pod executing the frozen
// quoin-healthcheck binary in-network and returns its log output plus its
// container exit status.
func verifyProbe(r *runner, stage int, namespace, release, component, probe, image, statusFlag, url string) (string, error) {
	name := fmt.Sprintf("%s-verify-%s-%s", release, probe, component)
	arguments := []string{"--", "/quoin-healthcheck"}
	if statusFlag != "" {
		arguments = append(arguments, "--status", statusFlag)
	}
	arguments = append(arguments, url)
	// A failed previous verifier may leave this fixed-name disposable pod;
	// wait for deletion so kubectl run never races an existing object.
	if output, err := r.run(stage, "verify-delete-"+probe+"-"+component, kubectl(namespace, "delete", "--ignore-not-found=true", "--wait=true", "pod", name)...); err != nil {
		return output, err
	}
	if output, err := r.run(stage, "verify-run-"+probe+"-"+component, append(kubectl(namespace, "run", name, "--image="+image, "--restart=Never", "--command"), arguments...)...); err != nil {
		return output, err
	}
	// A negative --status probe intentionally exits non-zero, so waiting for
	// phase=Succeeded would turn an observed HTTP status into a timeout. Wait
	// for either terminal phase, then return the actual process exit status to
	// the readiness judge.
	phase, waitErr := waitVerifierTerminal(r, stage, namespace, name, probe+"-"+component)
	if waitErr != nil {
		return phase, waitErr
	}
	exit, exitErr := r.run(stage, "verify-exit-"+probe+"-"+component, kubectl(namespace, "get", "pod", name, "--output", "jsonpath={.status.containerStatuses[0].state.terminated.exitCode}")...)
	// Collect the response body even for a non-zero healthcheck. In
	// particular, --status intentionally exits non-zero for HTTP 503 but its
	// JSON body is the frozen not-ready evidence the caller must judge.
	logs, logsErr := r.run(stage, "verify-logs-"+probe+"-"+component, kubectl(namespace, "logs", "pod/"+name)...)
	_, _ = r.run(stage, "verify-cleanup-"+probe+"-"+component, kubectl(namespace, "delete", "--ignore-not-found=true", "pod", name)...)
	if logsErr != nil {
		return logs, logsErr
	}
	if exitErr != nil || strings.TrimSpace(exit) != "0" {
		return logs, fmt.Errorf("verifier pod phase=%s exit status %q (%v)", phase, strings.TrimSpace(exit), exitErr)
	}
	return logs, nil
}

// waitVerifierTerminal waits for a verifier pod to Succeed or Fail. Probe
// failures are useful observations, not launcher failures, so the caller must
// receive the terminal phase and process exit code rather than a wait timeout.
func waitVerifierTerminal(r *runner, stage int, namespace, name, label string) (string, error) {
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		output, err := r.run(stage, "verify-phase-"+label, kubectl(namespace, "get", "pod", name, "--output", "jsonpath={.status.phase}")...)
		last = strings.TrimSpace(output)
		if err == nil && (last == "Succeeded" || last == "Failed") {
			return last, nil
		}
		time.Sleep(time.Second)
	}
	return last, fmt.Errorf("timed out waiting for verifier pod %s to become terminal (last phase %q)", name, last)
}

// judgeDeploymentImage proves the deployed pod template pins exactly the
// release manifest digest reference (OPS-VERIFY-003).
func judgeDeploymentImage(r *runner, stage int, namespace, release, component, pinned string) *verifyError {
	output, err := r.run(stage, "verify-image-"+component, kubectl(namespace, "get", "deployment", release+"-"+component, "--output", "jsonpath={.spec.template.spec.containers[0].image}")...)
	if err != nil {
		return verifyFail("topology_service_not_running", "%s deployment unreadable: %s", component, strings.TrimSpace(output))
	}
	actual := strings.TrimSpace(output)
	if actual != pinned {
		return verifyFail("image_digest_mismatch", "%s runs %q but the release manifest pins %q", component, actual, pinned)
	}
	return nil
}

// helmFrozenMounts is the fixed volume topology per component on Kubernetes:
// parity with the Compose binds at directory level (plinth workspaces live
// inside the single plinth state volume; lintel adds the shared-memory mount).
var helmFrozenMounts = map[string][]string{
	"quoin":  {"/etc/quoin/component.yaml", "/var/lib/quoin/backups", "/var/lib/quoin/data", "/run/quoin-secrets"},
	"plinth": {"/etc/quoin/component.yaml", "/run/quoin-secrets/runtime-ca.pem", "/var/lib/plinth"},
	"lintel": {"/etc/quoin/component.yaml", "/run/quoin-secrets/runtime-ca.pem", "/var/lib/lintel", "/dev/shm"},
	"stele":  {"/etc/quoin/component.yaml", "/run/quoin-secrets/runtime-ca.pem", "/run/quoin-secrets/stele-service-token"},
}

func judgeVolumeTopology(r *runner, stage int, namespace, release string) *verifyError {
	for _, component := range deployconfig.Components {
		output, err := r.run(stage, "verify-volumes-"+component, kubectl(namespace, "get", "deployment", release+"-"+component, "--output", "jsonpath={.spec.template.spec.containers[0].volumeMounts[*].mountPath}")...)
		if err != nil {
			return verifyFail("volume_topology_unreadable", "%s: %s", component, strings.TrimSpace(output))
		}
		observed := map[string]bool{}
		for _, path := range strings.Fields(strings.TrimSpace(output)) {
			observed[path] = true
		}
		for _, path := range helmFrozenMounts[component] {
			if !observed[path] {
				return verifyFail("volume_topology_violation", "%s does not mount %s (observed %v)", component, path, observed)
			}
		}
	}
	return nil
}

func judgeServiceTopology(r *runner, stage int, namespace, release string) *verifyError {
	output, err := r.run(stage, "verify-services", kubectl(namespace, "get", "services", "--output", "jsonpath={range .items[*]}{.metadata.name} {.spec.type} {.spec.ports[*].nodePort}{\"\\n\"}{end}")...)
	if err != nil {
		return verifyFail("topology_unreadable", "%s", strings.TrimSpace(output))
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(fields[0], release+"-") && fields[1] != "ClusterIP" {
			return verifyFail("topology_exposure_violation", "service %s is %s; only ClusterIP is allowed (ingress owns public exposure)", fields[0], fields[1])
		}
		if len(fields) > 2 {
			for _, port := range fields[2:] {
				if port != "<none>" && port != "0" {
					return verifyFail("topology_exposure_violation", "service %s publishes node port %s", fields[0], port)
				}
			}
		}
	}
	for _, component := range deployconfig.Components {
		hostNetwork, err := r.run(stage, "verify-hostnetwork-"+component, kubectl(namespace, "get", "deployment", release+"-"+component, "--output", "jsonpath={.spec.template.spec.hostNetwork}")...)
		if err != nil {
			return verifyFail("topology_unreadable", "%s: %s", component, strings.TrimSpace(hostNetwork))
		}
		if strings.TrimSpace(hostNetwork) == "true" {
			return verifyFail("topology_exposure_violation", "%s enables host networking", component)
		}
	}
	rep0 := r.report
	rep0.RecordCheck(report.Check{ID: "topology", Result: "passed", Expected: "four Deployments ready; ClusterIP services only; ingress owns public exposure", Actual: strings.TrimSpace(output), Code: "topology_fixed"})
	return nil
}

// verifyEventually absorbs the bounded service/endpoints propagation window
// after an otherwise healthy pod starts. It does not relax the probe contract:
// the last raw probe output is returned unchanged after the fixed deadline.
func verifyEventually(r *runner, stage int, namespace, release, component, probe, image, statusFlag, url string) (string, error) {
	deadline := time.Now().Add(60 * time.Second)
	var output string
	var err error
	for {
		output, err = verifyProbe(r, stage, namespace, release, component, probe, image, statusFlag, url)
		if err == nil || time.Now().After(deadline) {
			return output, err
		}
		time.Sleep(2 * time.Second)
	}
}
