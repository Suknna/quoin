package suites

// The Kubernetes implementation of the suite deployment backend applies the
// ordinary deployment manifest with kubectl. The cluster is reached exclusively
// through invocation-owned port-forwards (the compose loopback-parity rule:
// nothing is published persistently to the cluster), and one-shot registration
// execs run inside the workload pod so they carry the workload's exact image,
// environment and mounts. A disposable clone uses a separate namespace.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/creack/pty"
)

// kubernetesBackend drives one plain-manifest deployment per Stack.
type kubernetesBackend struct{}

// helperVerb identifies the native dispatcher. Only catalog-driven verify
// phases are supported; lifecycle work is executed directly by this backend.
func (*kubernetesBackend) helperVerb() string { return "kubernetes" }

func (*kubernetesBackend) opsBase(*Stack) string { return "http://quoin-ops:9090" }

func (*kubernetesBackend) helperEnv(stack *Stack) []string {
	return append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(stack.WorkRoot, stack.releaseSlug(), "state"),
		"QUOIN_DEPLOY_SCRIPTED=1",
	)
}

// namespace isolates a Kubernetes deployment. Fixed manifest resource names
// remain collision-free because a disposable clone receives its own namespace.
func (stack *Stack) namespace() string {
	if stack.Namespace != "" {
		return stack.Namespace
	}
	return "quoin"
}

func (stack *Stack) releaseName() string {
	if stack.ReleaseName != "" {
		return stack.ReleaseName
	}
	return "quoin"
}

func (stack *Stack) kubernetesManifest() string {
	// Qualification config is an invocation-local marker, while the ordinary
	// manifest remains repository-owned. Resolve it from the suite's repo root
	// rather than assuming the work directory mirrors repository layout.
	return filepath.Join(repoRootOf(stack.ConfigPath), "deploy", "kubernetes", "quoin.yaml")
}

func (stack *Stack) kubernetesManifests() []string {
	root := filepath.Join(repoRootOf(stack.ConfigPath), "deploy", "kubernetes")
	return []string{filepath.Join(root, "quoin.yaml"), filepath.Join(root, "ops-services.yaml")}
}

func repoRootOf(configPath string) string {
	if root := os.Getenv("QUOIN_REPO_ROOT"); root != "" {
		return root
	}
	return workDirOf(configPath)
}

// releaseSlug is the release's on-disk identity (the compose project's
// counterpart): per-release state, reports and credentials key on it.
func (stack *Stack) releaseSlug() string {
	return stack.namespace() + "-" + stack.releaseName()
}

// prepareBootstrap creates and pins the native workload resources, but leaves
// Quoin stopped so the one-off first-administrator Pod owns the database lock.
func (backend *kubernetesBackend) prepareBootstrap(stack *Stack) error {
	if err := stack.ensureNamespace(); err != nil {
		return err
	}
	if err := stack.bootstrapNativeSecrets(); err != nil {
		return err
	}
	for _, manifest := range stack.kubernetesManifests() {
		if _, _, err := stack.kubectl("--namespace", stack.namespace(), "apply", "--filename", manifest); err != nil {
			return fmt.Errorf("apply Kubernetes manifest %s: %w", filepath.Base(manifest), err)
		}
	}
	if err := stack.pinManifestImages(); err != nil {
		return err
	}
	_, _, err := stack.kubectl("--namespace", stack.namespace(), "scale", "deployment/quoin", "--replicas=0")
	return err
}

func (*kubernetesBackend) ensureInstalled(stack *Stack) (string, error) {
	if err := stack.ensureNamespace(); err != nil {
		return "", err
	}
	// A reinstall right after Down races the previous pods' asynchronous
	// teardown. Wait for the old namespace objects to settle before apply.
	if err := stack.awaitTerminationSettled(90 * time.Second); err != nil {
		return "", err
	}
	if err := stack.bootstrapNativeSecrets(); err != nil {
		return "", err
	}
	for _, manifest := range stack.kubernetesManifests() {
		if _, _, err := stack.kubectl("--namespace", stack.namespace(), "apply", "--filename", manifest); err != nil {
			return "", fmt.Errorf("apply Kubernetes manifest %s: %w", filepath.Base(manifest), err)
		}
	}
	if err := stack.pinManifestImages(); err != nil {
		return "", err
	}
	// The first administrator must be created while no Quoin server owns the
	// database lock. A one-off Pod mounts the real PVCs and config before the
	// long-running deployment is allowed to roll out.
	if _, _, err := stack.kubectl("--namespace", stack.namespace(), "scale", "deployment/quoin", "--replicas=0"); err != nil {
		return "", fmt.Errorf("stop Quoin for administrator bootstrap: %w", err)
	}
	if _, _, err := stack.kubectl("--namespace", stack.namespace(), "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/component=quoin", "--timeout=120s"); err != nil {
		return "", fmt.Errorf("wait for Quoin bootstrap lock release: %w", err)
	}
	if err := stack.bootstrapNativeAdministrator(); err != nil {
		return "", err
	}
	if _, _, err := stack.kubectl("--namespace", stack.namespace(), "scale", "deployment/quoin", "--replicas=1"); err != nil {
		return "", fmt.Errorf("start Quoin after administrator bootstrap: %w", err)
	}
	// Plinth and Lintel are deliberately runtime-unregistered until suite
	// registration. Waiting for their readiness here would deadlock bootstrap.
	for _, deployment := range []string{"gateway", "frontend", "stele"} {
		if _, _, err := stack.kubectl("--namespace", stack.namespace(), "rollout", "status", "deployment/"+deployment, "--timeout=300s"); err != nil {
			return "", fmt.Errorf("wait for deployment %s: %w", deployment, err)
		}
	}
	// The cluster exposes nothing persistently: bring public and webhook
	// services to this process through invocation-owned port-forwards.
	if err := stack.startForwards(); err != nil {
		return "", err
	}
	if err := stack.awaitPublic(300 * time.Second); err != nil {
		return "", err
	}
	return "", nil
}

// bootstrapNativeSecrets uses the core bootstrap implementation to generate
// the real secret set locally, then sends it directly to the isolated cluster
// namespace. Secret bytes never enter reports or command-line arguments.
func (stack *Stack) bootstrapNativeSecrets() error {
	// A retained PVC without its matching authority is a fail-closed state.
	// Never mint fresh keys into an existing namespace: that would make stored
	// data unrecoverable and could silently rotate a live Runtime identity.
	secret, _, secretErr := stack.kubectl("--namespace", stack.namespace(), "get", "secret/quoin-secrets", "--output=json")
	if secretErr == nil {
		var current struct {
			Data map[string]string `json:"data"`
		}
		if json.Unmarshal([]byte(secret), &current) != nil {
			return fmt.Errorf("read existing Kubernetes secret: invalid JSON")
		}
		for _, key := range []string{"root-key", "runtime-ca.pem", "runtime-tls.crt", "runtime-tls.key", "stele-service-token"} {
			if current.Data[key] == "" {
				return fmt.Errorf("existing quoin-secrets is incomplete (missing %s); restore the original complete secret set", key)
			}
		}
		return nil
	}
	if _, _, pvcErr := stack.kubectl("--namespace", stack.namespace(), "get", "pvc/quoin-data"); pvcErr == nil {
		return fmt.Errorf("quoin data PVC exists without quoin-secrets; restore the original complete secret set")
	}
	root := filepath.Join(stack.DeploymentRoot(), "bootstrap")
	secrets := filepath.Join(root, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		return fmt.Errorf("create native bootstrap directory: %w", err)
	}
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: publicOrigin,
		DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		SteleServiceTokenFile:     filepath.Join(secrets, "stele-service-token"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		return fmt.Errorf("generate Kubernetes bootstrap secrets: %w", err)
	}
	// Create-or-replace avoids retaining an incomplete secret after a failed
	// first attempt while preserving the exact core-generated bytes.
	arguments := []string{"--namespace", stack.namespace(), "create", "secret", "generic", "quoin-secrets",
		"--from-file=root-key=" + config.RootKeyFile,
		"--from-file=runtime-ca.pem=" + filepath.Join(secrets, "runtime-ca.pem"),
		"--from-file=runtime-tls.crt=" + config.RuntimeTLSCertificateFile,
		"--from-file=runtime-tls.key=" + config.RuntimeTLSPrivateKeyFile,
		"--from-file=stele-service-token=" + config.SteleServiceTokenFile,
		"--dry-run=client", "--output=yaml"}
	command := exec.Command("kubectl", arguments...)
	command.Env = stack.HelperEnv()
	body, err := command.Output()
	if err != nil {
		return fmt.Errorf("render native secret: %w", err)
	}
	if _, _, err := stack.kubectlInput(string(body), "apply", "--filename", "-"); err != nil {
		return fmt.Errorf("apply native secret: %w", err)
	}
	gateway := exec.Command("kubectl", "--namespace", stack.namespace(), "create", "secret", "tls", "gateway-tls",
		"--cert="+config.RuntimeTLSCertificateFile, "--key="+config.RuntimeTLSPrivateKeyFile, "--dry-run=client", "--output=yaml")
	gateway.Env = stack.HelperEnv()
	body, err = gateway.Output()
	if err != nil {
		return fmt.Errorf("render gateway TLS secret: %w", err)
	}
	if _, _, err := stack.kubectlInput(string(body), "apply", "--filename", "-"); err != nil {
		return fmt.Errorf("apply gateway TLS secret: %w", err)
	}
	return nil
}

// bootstrapNativeAdministrator starts a one-off Pod over the retained Quoin
// PVCs and attaches a pseudo-terminal only after the command prompts. It runs
// while the deployment is scaled down, so it exclusively owns the database
// lock rather than racing a serving Quoin process.
func (stack *Stack) bootstrapNativeAdministrator() error {
	return stack.bootstrapNativeAdministratorWithConfirmation(stack.AdminPassword)
}

// bootstrapNativeAdministratorWithConfirmation exists to prove the bootstrap
// gate with a real failed confirmation before a successful retry. The supplied
// confirmation is never persisted or emitted in diagnostics.
func (stack *Stack) bootstrapNativeAdministratorWithConfirmation(confirmation string) error {
	if stack.AdminPassword == "" {
		return nil
	}
	image, err := stack.nativeImage("quoin")
	if err != nil {
		return err
	}
	pod := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: quoin-admin-bootstrap
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: admin
      image: %s
      stdin: true
      tty: true
      command: ["/quoin", "admin", "create", "--config", "/etc/quoin/component.yaml"]
      securityContext:
        allowPrivilegeEscalation: false
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: backups, mountPath: /var/lib/quoin/backups}
        - {name: secrets, mountPath: /run/quoin-secrets, readOnly: true}
  volumes:
    - {name: config, configMap: {name: quoin-config}}
    - {name: data, persistentVolumeClaim: {claimName: quoin-data}}
    - {name: backups, persistentVolumeClaim: {claimName: quoin-backups}}
    - {name: secrets, secret: {secretName: quoin-secrets}}
`, image)
	if _, _, err := stack.kubectlInput(pod, "--namespace", stack.namespace(), "apply", "--filename", "-"); err != nil {
		return fmt.Errorf("create administrator bootstrap pod: %w", err)
	}
	defer func() {
		_, _, _ = stack.kubectl("--namespace", stack.namespace(), "delete", "pod/quoin-admin-bootstrap", "--ignore-not-found=true", "--wait=true")
	}()
	command := exec.Command("kubectl", "--namespace", stack.namespace(), "attach", "--stdin", "--tty", "pod/quoin-admin-bootstrap", "--container", "admin")
	command.Env = stack.HelperEnv()
	terminal, err := pty.Start(command)
	if err != nil {
		return fmt.Errorf("attach administrator bootstrap terminal: %w", err)
	}
	defer terminal.Close()
	output := make(chan string, 16)
	go func() {
		buffer := make([]byte, 4096)
		for {
			count, readErr := terminal.Read(buffer)
			if count > 0 {
				output <- string(buffer[:count])
			}
			if readErr != nil {
				close(output)
				return
			}
		}
	}()
	transcript := ""
	answerPrompt := func(prompt, answer string) error {
		deadline := time.NewTimer(90 * time.Second)
		defer deadline.Stop()
		for !strings.Contains(transcript, prompt) {
			select {
			case chunk, open := <-output:
				if !open {
					return fmt.Errorf("administrator bootstrap ended before %q", prompt)
				}
				transcript += chunk
			case <-deadline.C:
				return fmt.Errorf("administrator bootstrap prompt %q timed out", prompt)
			}
		}
		// term.ReadPassword disables echo before the password prompt is emitted.
		// Prompt synchronization keeps secret input out of captured terminal text.
		if _, err := terminal.Write([]byte(answer + "\n")); err != nil {
			return fmt.Errorf("answer administrator bootstrap prompt: %w", err)
		}
		return nil
	}
	for _, item := range []struct{ prompt, answer string }{
		{"Username:", "admin"},
		{"Display name:", "Qualification Administrator"},
		{"Temporary password:", stack.AdminPassword},
		{"Confirm temporary password:", confirmation},
	} {
		if err := answerPrompt(item.prompt, item.answer); err != nil {
			return err
		}
	}
	if err := command.Wait(); err != nil {
		return fmt.Errorf("create first administrator: %w", err)
	}
	return nil
}

// offlinePod runs a core Quoin command in a short-lived Pod with the actual
// retained PVCs and secret authority. It replaces the retired Helm helper's
// offline lifecycle transport without introducing another deployment DSL.
func (backend *kubernetesBackend) offlinePod(stack *Stack, operation string, arguments ...string) (string, error) {
	image, err := stack.nativeImage("quoin")
	if err != nil {
		return "", err
	}
	name := "quoin-native-" + operation
	commandJSON, _ := json.Marshal(append([]string{"/quoin"}, arguments...))
	pod := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: operation
      image: %s
      command: %s
      securityContext: {allowPrivilegeEscalation: false}
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: backups, mountPath: /var/lib/quoin/backups}
        - {name: secrets, mountPath: /run/quoin-secrets, readOnly: true}
  volumes:
    - {name: config, configMap: {name: quoin-config}}
    - {name: data, persistentVolumeClaim: {claimName: quoin-data}}
    - {name: backups, persistentVolumeClaim: {claimName: quoin-backups}}
    - {name: secrets, secret: {secretName: quoin-secrets}}
`, name, image, commandJSON)
	if _, _, err := stack.kubectlInput(pod, "--namespace", stack.namespace(), "apply", "--filename", "-"); err != nil {
		return "", err
	}
	defer func() {
		_, _, _ = stack.kubectl("--namespace", stack.namespace(), "delete", "pod/"+name, "--ignore-not-found=true", "--wait=true")
	}()
	if output, _, err := stack.kubectl("--namespace", stack.namespace(), "wait", "--for=condition=Ready=false", "pod/"+name, "--timeout=10s"); err != nil && output != "" {
		// A fast-completing offline Pod need not become Ready.
	}
	_, _, _ = stack.kubectl("--namespace", stack.namespace(), "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/"+name, "--timeout=300s")
	logs, _, logErr := stack.kubectl("--namespace", stack.namespace(), "logs", "pod/"+name)
	phase, _, phaseErr := stack.kubectl("--namespace", stack.namespace(), "get", "pod/"+name, "-o", "jsonpath={.status.phase}")
	if logErr != nil {
		return logs, logErr
	}
	if phaseErr != nil || strings.TrimSpace(phase) != "Succeeded" {
		return logs, fmt.Errorf("native %s Pod phase=%s", operation, strings.TrimSpace(phase))
	}
	return logs, nil
}

func (backend *kubernetesBackend) restorePod(stack *Stack, backupID, username, password string) (string, error) {
	image, err := stack.nativeImage("quoin")
	if err != nil {
		return "", err
	}
	const name = "quoin-native-restore"
	pod := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: operation
      image: %s
      stdin: true
      tty: true
      command: ["/quoin", "restore", "--backup", %q, "--config", "/etc/quoin/component.yaml"]
      securityContext: {allowPrivilegeEscalation: false}
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: backups, mountPath: /var/lib/quoin/backups}
        - {name: secrets, mountPath: /run/quoin-secrets, readOnly: true}
  volumes:
    - {name: config, configMap: {name: quoin-config}}
    - {name: data, persistentVolumeClaim: {claimName: quoin-data}}
    - {name: backups, persistentVolumeClaim: {claimName: quoin-backups}}
    - {name: secrets, secret: {secretName: quoin-secrets}}
`, name, image, backupID)
	if _, _, err := stack.kubectlInput(pod, "--namespace", stack.namespace(), "apply", "--filename", "-"); err != nil {
		return "", err
	}
	defer func() {
		_, _, _ = stack.kubectl("--namespace", stack.namespace(), "delete", "pod/"+name, "--ignore-not-found=true", "--wait=true")
	}()
	command := exec.Command("kubectl", "--namespace", stack.namespace(), "attach", "--stdin", "--tty", "pod/"+name, "--container", "operation")
	command.Env = stack.HelperEnv()
	terminal, err := pty.Start(command)
	if err != nil {
		return "", err
	}
	defer terminal.Close()
	chunks := make(chan string, 16)
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, e := terminal.Read(buffer)
			if n > 0 {
				chunks <- string(buffer[:n])
			}
			if e != nil {
				close(chunks)
				return
			}
		}
	}()
	transcript := ""
	for _, item := range []struct{ prompt, answer string }{{"Recovery administrator username:", username}, {"Temporary password:", password}, {"Confirm temporary password:", password}} {
		deadline := time.NewTimer(90 * time.Second)
		for !strings.Contains(transcript, item.prompt) {
			select {
			case chunk, ok := <-chunks:
				if !ok {
					deadline.Stop()
					return transcript, fmt.Errorf("restore ended before %q", item.prompt)
				}
				transcript += chunk
			case <-deadline.C:
				return transcript, fmt.Errorf("restore prompt %q timed out", item.prompt)
			}
		}
		deadline.Stop()
		if _, err := terminal.Write([]byte(item.answer + "\n")); err != nil {
			return transcript, err
		}
	}
	if err := command.Wait(); err != nil {
		return transcript, err
	}
	return transcript, nil
}

func (stack *Stack) nativeImage(component string) (string, error) {
	body, err := os.ReadFile(stack.ManifestPath)
	if err != nil {
		return "", fmt.Errorf("read release manifest: %w", err)
	}
	var manifest struct {
		Images map[string]struct {
			Repository string `json:"repository"`
			Index      string `json:"index_digest"`
		} `json:"images"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", fmt.Errorf("parse release manifest: %w", err)
	}
	image, ok := manifest.Images[component]
	if !ok || image.Repository == "" || image.Index == "" {
		return "", fmt.Errorf("release manifest has no digest-pinned %s image", component)
	}
	return image.Repository + "@" + image.Index, nil
}

// pinManifestImages applies the qualification subject's immutable image
// references after the stock manifest is applied. This is test harness glue,
// not a user-facing deployment language.
func (stack *Stack) pinManifestImages() error {
	body, err := os.ReadFile(stack.ManifestPath)
	if err != nil {
		return fmt.Errorf("read release manifest: %w", err)
	}
	var manifest struct {
		Images map[string]struct {
			Repository string `json:"repository"`
			Index      string `json:"index_digest"`
		} `json:"images"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return fmt.Errorf("parse release manifest: %w", err)
	}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele", "frontend"} {
		image, ok := manifest.Images[component]
		if !ok || image.Repository == "" || image.Index == "" {
			return fmt.Errorf("release manifest has no digest-pinned %s image", component)
		}
		if _, _, err := stack.kubectl("--namespace", stack.namespace(), "set", "image", "deployment/"+component,
			component+"="+image.Repository+"@"+image.Index); err != nil {
			return fmt.Errorf("pin %s image: %w", component, err)
		}
	}
	return nil
}

// ensureNamespace creates the deployment namespace when absent.
func (stack *Stack) ensureNamespace() error {
	command := exec.Command("kubectl", "get", "namespace", stack.namespace())
	if err := command.Run(); err == nil {
		return nil
	}
	create := exec.Command("kubectl", "create", "namespace", stack.namespace())
	var combined bytes.Buffer
	create.Stdout, create.Stderr = &combined, &combined
	if err := create.Run(); err != nil {
		return fmt.Errorf("create namespace %s: %v: %s", stack.namespace(), err, combined.String())
	}
	return nil
}

// portForward is one invocation-owned kubectl port-forward. It dies
// with its process group and is always removed through CloseTransports.
type portForward struct {
	command *exec.Cmd
	local   int
	service string
}

// startForwards brings the Quoin public and Stele webhook services to
// this process's loopback on the Stack's declared ports (so BaseURL and
// steleWebhookURL address them exactly like the compose publication).
//
// The forwards must outlive the phase subprocess that installed the
// deployment (setup, action and assert are separate processes sharing
// one Stack identity), so each kubectl runs detached in its own session
// with a pidfile under the deployment root; every later process of the
// same Stack adopts the recorded pids, and CloseTransports kills them
// by record. Startup is transactional: a failure or timeout kills
// everything this call started, and a pidfile is only adopted after the
// recorded process is verified to still be the recorded kubectl
// port-forward (PID reuse can never steer a kill at an unrelated
// process). Nothing is published to the cluster itself.
func (stack *Stack) startForwards() error {
	type pending struct {
		service string
		local   int
		pid     int
		logFile *os.File
		pidfile string
	}
	started := []*pending{}
	commit := func() {
		for _, entry := range started {
			_ = os.WriteFile(entry.pidfile, []byte(strconv.Itoa(entry.pid)), 0o600)
			entry.logFile.Close()
		}
	}
	rollback := func() {
		for _, entry := range started {
			if entry.pid != 0 {
				_ = syscall.Kill(entry.pid, syscall.SIGKILL)
			}
			_ = os.Remove(entry.pidfile)
			entry.logFile.Close()
		}
	}
	forward := func(service string, local int) error {
		if _, ok := stack.adoptForward(service, local); ok {
			return nil
		}
		if err := os.MkdirAll(filepath.Join(stack.DeploymentRoot(), "forwards"), 0o700); err != nil {
			return fmt.Errorf("port-forward %s state: %w", service, err)
		}
		logFile, err := os.OpenFile(stack.forwardLog(service), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("port-forward %s log: %w", service, err)
		}
		command := exec.Command("kubectl", "--namespace", stack.namespace(),
			"port-forward", service, "--address", "127.0.0.1", fmt.Sprintf("%d:8080", local))
		command.Stdout, command.Stderr = logFile, logFile
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := command.Start(); err != nil {
			logFile.Close()
			return fmt.Errorf("port-forward %s: %w", service, err)
		}
		entry := &pending{service: service, local: local, pid: command.Process.Pid, logFile: logFile, pidfile: stack.forwardPidfile(service)}
		_ = command.Process.Release()
		started = append(started, entry)
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if loopbackAccepts(local) {
				return nil
			}
			if syscall.Kill(entry.pid, 0) != nil {
				body, _ := os.ReadFile(stack.forwardLog(service))
				return fmt.Errorf("port-forward %s exited: %s", service, body)
			}
			time.Sleep(500 * time.Millisecond)
		}
		return fmt.Errorf("port-forward %s never accepted connections", service)
	}
	if err := forward("service/gateway", stack.QuoinPort); err != nil {
		rollback()
		return err
	}
	if err := forward("service/stele", stack.StelePort); err != nil {
		rollback()
		return err
	}
	commit()
	return nil
}

// startForwardsRequired brings ONE required service forward up,
// transactionally (failure cleans what it started).
func (stack *Stack) startForwardsRequired(service string) error {
	return stack.startOne(service, stack.portOfService(service), true)
}

// startForwardsBestEffort brings one service forward up when its
// endpoints allow it; a service with no endpoints (an intentionally
// scaled-down component) is a legitimate state, not a failure.
func (stack *Stack) startForwardsBestEffort(service string) {
	_ = stack.startOne(service, stack.portOfService(service), false)
}

func (stack *Stack) portOfService(service string) int {
	if strings.Contains(service, "stele-webhook") {
		return stack.StelePort
	}
	return stack.QuoinPort
}

func (stack *Stack) startOne(service string, local int, required bool) error {
	type pending struct {
		pid     int
		logFile *os.File
		pidfile string
	}
	var started []*pending
	rollback := func() {
		for _, entry := range started {
			if entry.pid != 0 {
				_ = syscall.Kill(entry.pid, syscall.SIGKILL)
			}
			_ = os.Remove(entry.pidfile)
			entry.logFile.Close()
		}
	}
	if _, ok := stack.adoptForward(service, local); ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(stack.DeploymentRoot(), "forwards"), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(stack.forwardLog(service), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	command := exec.Command("kubectl", "--namespace", stack.namespace(),
		"port-forward", "service/"+service, "--address", "127.0.0.1", fmt.Sprintf("%d:8080", local))
	command.Stdout, command.Stderr = logFile, logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		logFile.Close()
		return err
	}
	entry := &pending{pid: command.Process.Pid, logFile: logFile, pidfile: stack.forwardPidfile("service/" + service)}
	_ = command.Process.Release()
	started = append(started, entry)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if loopbackAccepts(local) {
			_ = os.WriteFile(entry.pidfile, []byte(strconv.Itoa(entry.pid)), 0o600)
			logFile.Close()
			return nil
		}
		if syscall.Kill(entry.pid, 0) != nil {
			body, _ := os.ReadFile(stack.forwardLog(service))
			rollback()
			if !required {
				return nil
			}
			return fmt.Errorf("port-forward %s exited: %s", service, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	rollback()
	if !required {
		return nil
	}
	return fmt.Errorf("port-forward %s never accepted connections", service)
}

// adoptForward reuses a recorded, identity-verified forward of a prior
// phase process. A pidfile alone proves nothing: the PID is adopted
// only while /proc still shows it as this stack's kubectl port-forward
// for the same namespace, service and local port.
func (stack *Stack) adoptForward(service string, local int) (int, bool) {
	pidfile := stack.forwardPidfile(service)
	body, err := os.ReadFile(pidfile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 1 || syscall.Kill(pid, 0) != nil {
		_ = os.Remove(pidfile)
		return 0, false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return 0, false
	}
	fields := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	joined := strings.Join(fields, " ")
	if !strings.Contains(joined, "kubectl") || !strings.Contains(joined, "port-forward") ||
		!strings.Contains(joined, "namespace") || !strings.Contains(joined, stack.namespace()) ||
		!strings.Contains(joined, service) || !strings.Contains(joined, fmt.Sprintf("127.0.0.1:%d", local)) {
		_ = os.Remove(pidfile)
		return 0, false
	}
	return pid, true
}

// loopbackAccepts proves the forwarded tunnel answers TCP/HTTP on the
// local port (any HTTP status proves the tunnel; the product's 404s are
// as good as 200s here).
func loopbackAccepts(local int) bool {
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", local), nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	return true
}

func (stack *Stack) forwardPidfile(service string) string {
	safe := strings.ReplaceAll(strings.TrimPrefix(service, "service/"), "/", "-")
	return filepath.Join(stack.DeploymentRoot(), "forwards", safe+".pid")
}

func (stack *Stack) forwardLog(service string) string {
	safe := strings.ReplaceAll(strings.TrimPrefix(service, "service/"), "/", "-")
	return filepath.Join(stack.DeploymentRoot(), "forwards", safe+".log")
}

func readForwardPid(path string) (int, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(body)))
}

// CloseTransports kills every recorded port-forward of this deployment
// (the current process's records and the pidfile records of earlier
// phase processes). Safe to call repeatedly.
func (stack *Stack) CloseTransports() {
	killed := map[int]bool{}
	for _, forward := range stack.forwards {
		if forward.command.Process != nil {
			_ = syscall.Kill(-forward.command.Process.Pid, syscall.SIGKILL)
			_, _ = forward.command.Process.Wait()
			killed[forward.command.Process.Pid] = true
		}
	}
	stack.forwards = nil
	matches, _ := filepath.Glob(filepath.Join(stack.DeploymentRoot(), "forwards", "*.pid"))
	for _, pidfile := range matches {
		if pid, err := readForwardPid(pidfile); err == nil && !killed[pid] {
			if forwardStillOurs(pid, stack) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = os.Remove(pidfile)
	}
	// Final identity-checked sweep: a forward whose pidfile was lost
	// (a failed refresh rolled it back after Start, a killed phase
	// never committed) is still found by its own /proc identity —
	// any kubectl port-forward still targeting this stack's namespace
	// dies here, so PID reuse can never steer the kill.
	for _, pid := range namespaceForwardPIDs(stack) {
		if !killed[pid] {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// namespaceForwardPIDs scans /proc for kubectl port-forward processes
// still bound to this stack's namespace.
func namespaceForwardPIDs(stack *Stack) []int {
	var pids []int
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if forwardStillOurs(pid, stack) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func (*kubernetesBackend) execCommand(stack *Stack, component string, arguments []string) (string, int, error) {
	full := append([]string{
		"--namespace", stack.namespace(), "exec",
		"deployment/" + component, "--",
	}, arguments...)
	return stack.kubectl(full...)
}

func (*kubernetesBackend) runService(stack *Stack, component string, arguments []string, stdinPayload string) (string, int, error) {
	// The registration vehicle is the running workload pod itself: exec
	// carries exactly the workload's image, environment and mounts — the
	// kubectl counterpart of compose run on the service definition.
	// kubectl exec bypasses the image entrypoint, so the component's
	// own binary is invoked explicitly (the frozen image layout:
	// /quoin, /plinth, /lintel, /stele).
	full := append([]string{
		"--namespace", stack.namespace(), "exec", "-i",
		"deployment/" + component, "--", "/" + component,
	}, arguments...)
	command := exec.Command("kubectl", full...)
	command.Dir = workDirOf(stack.ConfigPath)
	command.Env = stack.HelperEnv()
	command.Stdin = strings.NewReader(stdinPayload + "\n")
	var combined bytes.Buffer
	command.Stdout, command.Stderr = &combined, &combined
	err := command.Run()
	code := 0
	if command.ProcessState != nil {
		code = command.ProcessState.ExitCode()
	}
	return combined.String(), code, err
}

func (*kubernetesBackend) logs(stack *Stack, component string) (string, error) {
	output, _, err := stack.kubectl("--namespace", stack.namespace(), "logs",
		"deployment/"+component, "--tail=300")
	return output, err
}

func (*kubernetesBackend) refreshTransports(stack *Stack) error {
	// kubectl port-forward pins the pod resolved at start; a restore
	// that rebuilt the workloads left the pinned pods dead. Restart the
	// forwards against the live pods. Post-restore, stele is INTENTIONALLY
	// scaled to zero (the restore checklist's isolation state), so its
	// service has no endpoints; only the public forward is required —
	// the stele forward is re-established when the workloads return
	// (the next EnsureInstalled cycle) and pidfile records keep the
	// best-effort forward cleanup-safe.
	stack.CloseTransports()
	if err := stack.startForwardsRequired("gateway"); err != nil {
		return err
	}
	stack.startForwardsBestEffort("stele")
	return nil
}

func (*kubernetesBackend) down(stack *Stack, dataRemoval bool) (string, error) {
	// Delete ordinary workload objects by manifest. Persistent data survives a
	// normal down exactly like Compose bind-mounted data; the disposable
	// namespace is deleted only when its caller requests data removal.
	var output string
	var err error
	for _, manifest := range stack.kubernetesManifests() {
		output, _, err = stack.kubectl("--namespace", stack.namespace(), "delete", "--ignore-not-found=true", "--wait=true", "--filename", manifest)
		if err != nil {
			return output, err
		}
	}
	if dataRemoval && strings.HasSuffix(stack.namespace(), "-disp") {
		output, _, err = stack.kubectl("delete", "namespace", stack.namespace(), "--ignore-not-found=true", "--wait=true")
	}
	return output, err
}

func (*kubernetesBackend) stopComponent(stack *Stack, component string, timeout time.Duration) (string, time.Duration, error) {
	// The graceful-drain fact: delete the component's current pod and
	// read its terminated exit code while the pod object lingers —
	// SIGKILL drains surface as 137 exactly like compose. The
	// deployment is scaled to zero AFTER the drain is observed so the
	// component is genuinely stopped (no replacement pod) until
	// startComponent returns it to service.
	started := time.Now()
	selector := "app.kubernetes.io/component=" + component
	podList, _, _ := stack.kubectl("--namespace", stack.namespace(), "get", "pods", "-l", selector, "-o", "jsonpath={.items[0].metadata.name}")
	pod := strings.TrimSpace(podList)
	if pod == "" {
		return "", time.Since(started), fmt.Errorf("no pod for %s", component)
	}
	if _, _, err := stack.kubectl("--namespace", stack.namespace(), "delete", "pod", pod,
		"--wait=false"); err != nil {
		return "", time.Since(started), err
	}
	exitCode, drainErr := waitForPodExitCode(stack, pod, timeout)
	_, _, _ = stack.kubectl("--namespace", stack.namespace(), "scale",
		"deployment/"+component, "--replicas=0")
	if drainErr != nil {
		return exitCode, time.Since(started), drainErr
	}
	return exitCode, time.Since(started), nil
}

// waitForPodExitCode polls a deleting pod's terminated container exit
// code while the pod object still exists.
func waitForPodExitCode(stack *Stack, pod string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, _, _ := stack.kubectl("--namespace", stack.namespace(), "get", "pod", pod,
			"-o", "jsonpath={.status.containerStatuses[0].state.terminated.exitCode}")
		if code := strings.TrimSpace(output); code != "" {
			return code, nil
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("pod %s never exposed a terminated exit code", pod)
}

func (*kubernetesBackend) startComponent(stack *Stack, component string) error {
	// stopComponent scaled the deployment to zero; return it to service.
	deployment := "deployment/" + component
	if _, _, err := stack.kubectl("--namespace", stack.namespace(), "scale",
		deployment, "--replicas=1"); err != nil {
		return err
	}
	_, _, err := stack.kubectl("--namespace", stack.namespace(), "rollout", "status",
		deployment, "--timeout=180s")
	return err
}

func (*kubernetesBackend) removeSecretAuthority(stack *Stack) error {
	_, _, err := stack.kubectl("--namespace", stack.namespace(), "delete", "secret",
		stack.releaseName()+"-secrets", "--ignore-not-found=true", "--wait=true")
	return err
}

func (*kubernetesBackend) stopQuoin(stack *Stack) error {
	// Scale to zero AND wait for the pod to actually terminate (the
	// compose counterpart's docker stop waits too): the offline backup
	// probes unavailability immediately after, and a still-terminating
	// pod would answer with a live body instead of proving the network
	// path dead.
	if _, _, err := stack.kubectl("--namespace", stack.namespace(), "scale",
		"deployment/quoin", "--replicas=0"); err != nil {
		return err
	}
	_, _, err := stack.kubectl("--namespace", stack.namespace(), "wait",
		"--for=delete", "pod", "-l", "app.kubernetes.io/component=quoin",
		"--timeout=90s")
	return err
}

func (*kubernetesBackend) cloneRoot(stack *Stack) string {
	return filepath.Join(stack.WorkRoot, stack.releaseSlug()+"-disp-root")
}

func (*kubernetesBackend) runningWorkloadCount(stack *Stack) int {
	output, _, _ := stack.kubectl("--namespace", stack.namespace(), "get", "pods",
		"-l", "app.kubernetes.io/part-of=quoin",
		"-o", "jsonpath={.items[*].metadata.name}")
	count := 0
	for _, name := range strings.Fields(output) {
		if strings.Contains(name, "plinth") || strings.Contains(name, "lintel") {
			count++
		}
	}
	return count
}

func (*kubernetesBackend) deploymentRoot(stack *Stack) string {
	return filepath.Join(stack.WorkRoot, stack.releaseSlug())
}

func (*kubernetesBackend) reportsDir(stack *Stack) string {
	// The legs' explicit --report files land in the deployment root.
	return stack.DeploymentRoot()
}

func (*kubernetesBackend) clone(stack *Stack) *Stack {
	// The disposable lifecycle deployment applies the same plain manifest in
	// its own namespace. Fixed resource names are thus fully isolated while
	// its loopback forwards retain the compose clone's offset ports.
	disposableRoot := filepath.Join(stack.WorkRoot, stack.releaseSlug()+"-disp-root")
	_ = os.MkdirAll(disposableRoot, 0o700)
	return &Stack{
		Backend:     stack.Backend,
		Namespace:   stack.namespace() + "-disp",
		ReleaseName: "quoin",
		WorkRoot:    stack.WorkRoot,
		ConfigPath:  stack.ConfigPath, ManifestPath: stack.ManifestPath,
		AdminPassword: RandomPassword(), QuoinPort: stack.QuoinPort + 20, StelePort: stack.StelePort + 20,
		Stdout: stack.Stdout, Stderr: stack.Stderr,
	}
}

// kubectl runs one kubectl invocation and returns combined output.
func (stack *Stack) kubectl(arguments ...string) (string, int, error) {
	command := exec.Command("kubectl", arguments...)
	command.Env = stack.HelperEnv()
	var combined bytes.Buffer
	command.Stdout, command.Stderr = &combined, &combined
	err := command.Run()
	code := 0
	if command.ProcessState != nil {
		code = command.ProcessState.ExitCode()
	}
	return combined.String(), code, err
}

// kubectlInput runs one kubectl invocation with stdin.
func (stack *Stack) kubectlInput(stdin string, arguments ...string) (string, int, error) {
	command := exec.Command("kubectl", arguments...)
	command.Env = stack.HelperEnv()
	command.Stdin = strings.NewReader(stdin)
	var combined bytes.Buffer
	command.Stdout, command.Stderr = &combined, &combined
	err := command.Run()
	code := 0
	if command.ProcessState != nil {
		code = command.ProcessState.ExitCode()
	}
	return combined.String(), code, err
}

// Kubectl runs one kubectl invocation in this stack's environment.
func (stack *Stack) Kubectl(arguments ...string) (string, int, error) {
	return stack.kubectl(arguments...)
}

// awaitTerminationSettled waits until no release-scoped pod carries a
// deletion timestamp (any phase) and none of the previous release's
// pods remain; a timeout is a diagnostic error naming what is stuck.
func (stack *Stack) awaitTerminationSettled(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var stuck string
	for time.Now().Before(deadline) {
		output, _, err := stack.kubectl("--namespace", stack.namespace(), "get", "pods",
			"-l", "app.kubernetes.io/part-of=quoin",

			"-o", "jsonpath={range .items[*]}{.metadata.name}{\" \"}{.metadata.deletionTimestamp}{\";\"}{end}")
		if err == nil {
			stuck = ""
			for _, record := range strings.Split(strings.TrimSpace(output), ";") {
				// "name <timestamp>" = terminating; "name " alone is a
				// live pod of the current release (or the fresh install
				// has no pods at all) — neither blocks a reinstall.
				if fields := strings.Fields(record); len(fields) == 2 {
					stuck = record
					break
				}
			}
			if stuck == "" {
				return nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	if stuck == "" {
		stuck = "release pods still present"
	}
	return fmt.Errorf("previous release pods never settled: %s", stuck)
}

// forwardStillOurs proves a recorded pid is still a kubectl
// port-forward of this stack before it is killed (PID reuse must never
// steer a SIGKILL at an unrelated process).
func forwardStillOurs(pid int, stack *Stack) bool {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	joined := strings.Join(strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"), " ")
	return strings.Contains(joined, "kubectl") && strings.Contains(joined, "port-forward") &&
		strings.Contains(joined, "namespace") && strings.Contains(joined, stack.namespace())
}
