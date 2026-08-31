package helm

import (
	"fmt"
	"strings"
	"time"
)

// secretFileKeys is the frozen deployment secret set (OPS-SECRET-003). The
// same key set is verified on the Kubernetes Secret, the staging volume and
// the mounted container paths.
var secretFileKeys = []string{
	"root-key", "runtime-ca.pem", "runtime-ca.key",
	"runtime-tls.crt", "runtime-tls.key", "stele-service-token",
}

// secretExists reports whether the retained Kubernetes Secret already carries
// the complete frozen key set.
func secretExists(r *runner, stage int, namespace, release string) (bool, error) {
	output, err := r.run(stage, "secret-get", kubectl(namespace, "get", "secret", release+"-secrets", "--output", "jsonpath={.data}")...)
	if err != nil {
		return false, nil
	}
	for _, key := range secretFileKeys {
		if !strings.Contains(output, key+"\":") {
			return false, nil
		}
	}
	return true, nil
}

// runSecretBootstrap drives the disposable in-cluster bootstrap. The helper
// creates only an empty fixed-name Secret; the same Quoin image generates and
// patches the secret bytes through its short-lived ServiceAccount, so the
// helper never materializes secret data.
func runSecretBootstrap(r *runner, stage int, namespace, release string, input installInput, images map[string]string) error {
	failed := func(cause error) error {
		if cleanupErr := cleanupBootstrap(r, stage, namespace, release); cleanupErr != nil {
			return fmt.Errorf("%w; failed bootstrap cleanup: %v", cause, cleanupErr)
		}
		return cause
	}
	if err := ensureEmptySecret(r, stage, namespace, release); err != nil {
		return err
	}
	manifest := renderSecretBootstrap(release, input, images["quoin"], images["plinth"])
	// Jobs are immutable; a previous attempt (complete or failed) must be
	// removed so the retry creates a fresh one.
	if _, err := r.run(stage, "bootstrap-job-delete", kubectl(namespace, "delete", "--ignore-not-found=true", "job", release+"-secret-bootstrap")...); err != nil {
		return failed(fmt.Errorf("remove previous bootstrap job: %w", err))
	}
	if output, err := r.runInput(stage, "bootstrap-apply", manifest, kubectl(namespace, "apply", "--filename", "-")...); err != nil {
		return failed(fmt.Errorf("apply bootstrap resources: %s", strings.TrimSpace(output)))
	}
	if output, err := r.run(stage, "bootstrap-job-wait", kubectl(namespace, "wait", "--for=condition=complete", "job/"+release+"-secret-bootstrap", "--timeout=180s")...); err != nil {
		logs, _ := r.run(stage, "bootstrap-job-logs", kubectl(namespace, "logs", "job/"+release+"-secret-bootstrap", "--tail=50")...)
		return failed(fmt.Errorf("secret bootstrap job did not complete: %s\nbootstrap logs: %s", strings.TrimSpace(output), strings.TrimSpace(logs)))
	}
	if complete, err := secretExists(r, stage, namespace, release); err != nil || !complete {
		return failed(fmt.Errorf("bootstrap Job completed without the complete deployment Secret"))
	}
	return nil
}

// cleanupBootstrap removes the disposable bootstrap objects; the retained PVCs
// and the deployment Secret stay.
func cleanupBootstrap(r *runner, stage int, namespace, release string) error {
	for _, arguments := range [][]string{
		kubectl(namespace, "delete", "--ignore-not-found=true", "--wait=true", "job", release+"-secret-bootstrap"),
		kubectl(namespace, "delete", "--ignore-not-found=true", "--wait=true", "rolebinding", release+"-secret-bootstrap"),
		kubectl(namespace, "delete", "--ignore-not-found=true", "--wait=true", "role", release+"-secret-bootstrap"),
		kubectl(namespace, "delete", "--ignore-not-found=true", "--wait=true", "serviceaccount", release+"-secret-bootstrap"),
		kubectl(namespace, "delete", "--ignore-not-found=true", "--wait=true", "configmap", release+"-bootstrap-config", release+"-admin-bootstrap-config"),
		kubectl(namespace, "delete", "--ignore-not-found=true", "--wait=true", "pod", release+"-secret-extract"),
		kubectl(namespace, "delete", "--ignore-not-found=true", "--wait=true", "pod", release+"-admin-bootstrap"),
		kubectl(namespace, "delete", "--ignore-not-found=true", "--wait=true", "pvc", release+"-"+stagingSuffix),
	} {
		if output, err := r.run(stage, "cleanup", arguments...); err != nil {
			return fmt.Errorf("remove disposable bootstrap resource: %s", strings.TrimSpace(output))
		}
	}
	return nil
}

// awaitHealthy waits for the install-ready state. Quoin and Stele must be
// Deployment-available; Plinth and Lintel must be Running but intentionally
// remain Kubernetes-not-ready until the operator performs the separate,
// attached-stdin Runtime registration (OPS-RUNTIME-REG-001). The following
// verifier observes each component's complete /readyz contract.
func awaitHealthy(r *runner, stage int, namespace, release string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastOutput string
	for time.Now().Before(deadline) {
		pending := false
		for _, component := range []string{"quoin", "stele"} {
			output, err := r.run(stage, "rollout-"+component, kubectl(namespace, "rollout", "status", "deployment/"+release+"-"+component, "--timeout=5s")...)
			lastOutput = output
			if err != nil {
				pending = true
			}
		}
		for _, component := range []string{"plinth", "lintel"} {
			output, err := r.run(stage, "runtime-running-"+component, kubectl(namespace, "get", "pods", "-l", "app.kubernetes.io/instance="+release+",app.kubernetes.io/component="+component, "--field-selector=status.phase=Running", "--no-headers")...)
			lastOutput = output
			if err != nil || strings.TrimSpace(output) == "" {
				pending = true
			}
		}
		if !pending {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("timed out waiting for install-ready workloads: %s", strings.TrimSpace(lastOutput))
}

// ensureEmptySecret pre-creates the fixed retained Secret without any value.
// The bootstrap Job has only get/update permission and fills it in-cluster.
func ensureEmptySecret(r *runner, stage int, namespace, release string) error {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s-secrets
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %s}
type: Opaque
`, release, release)
	output, err := r.runInput(stage, "secret-placeholder-apply", manifest, kubectl(namespace, "apply", "--filename", "-")...)
	if err != nil {
		return fmt.Errorf("create empty deployment Secret: %s", strings.TrimSpace(output))
	}
	return nil
}
