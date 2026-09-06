package suites

// The Kubernetes implementation of the suite deployment backend: the
// staged install runs through the helper's helm path, the cluster is
// reached exclusively through invocation-owned kubectl port-forwards
// (the compose loopback-parity rule: nothing is published persistently
// to the cluster), one-shot registration execs inside the running
// workload pod so it carries exactly the workload's image, environment
// and mounts, and the disposable clone is a second helm release of the
// same subject in the same namespace with its own release-scoped
// resources and data.

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// kubernetesBackend drives one helm release per Stack.
type kubernetesBackend struct{}

func (*kubernetesBackend) helperVerb() string { return "helm" }

func (*kubernetesBackend) opsBase(stack *Stack) string {
	return "http://" + stack.releaseName() + "-quoin-ops:9090"
}

func (*kubernetesBackend) helperEnv(stack *Stack) []string {
	return append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(stack.WorkRoot, stack.releaseSlug(), "state"),
		"QUOIN_HELM_NAMESPACE="+stack.namespace(),
		"QUOIN_HELM_RELEASE="+stack.releaseName(),
		"QUOIN_DEPLOY_SCRIPTED=1",
	)
}

// namespace and releaseName mirror the helm helper's own override
// surface with the same defaults, so the adapter and every helper
// subprocess agree on one deployment identity.
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

// releaseSlug is the release's on-disk identity (the compose project's
// counterpart): per-release state, reports and credentials key on it.
func (stack *Stack) releaseSlug() string {
	return stack.namespace() + "-" + stack.releaseName()
}

func (*kubernetesBackend) ensureInstalled(stack *Stack) (string, error) {
	helper, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := stack.ensureNamespace(); err != nil {
		return "", err
	}
	// A reinstall right after Down races the previous pods' asynchronous
	// teardown (volume detach holds Terminating pods past the uninstall
	// wait); a bounded quiet-window prevents the new pods from scheduling
	// onto half-torn volumes.
	if err := stack.awaitTerminationSettled(90 * time.Second); err != nil {
		return "", err
	}
	report := filepath.Join(stack.WorkRoot, stack.releaseSlug(), "install-report.json")
	install := exec.Command(helper, "helm", "install", "--config", stack.ConfigPath,
		"--release-manifest", stack.ManifestPath, "--report", report)
	install.Env = stack.HelperEnv()
	install.Dir = workDirOf(helper)
	install.Stdout, install.Stderr = stack.Stdout, stack.Stderr
	if stack.AdminPassword != "" {
		install.Stdin = strings.NewReader(strings.Join([]string{"admin", "Ticket 43 Admin", stack.AdminPassword, stack.AdminPassword}, "\n") + "\n")
	}
	// One idempotent resume when the workloads merely outran the
	// install-ready wait (large image extraction under load): the
	// helper's own contract says "rerun the same command to resume"
	// (OPS-HELPER-002). A second failure is a real failure.
	if err := install.Run(); err != nil {
		retry := exec.Command(helper, "helm", "install", "--config", stack.ConfigPath,
			"--release-manifest", stack.ManifestPath, "--report", report)
		retry.Env = install.Env
		retry.Dir = install.Dir
		retry.Stdout, retry.Stderr = stack.Stdout, stack.Stderr
		if stack.AdminPassword != "" {
			retry.Stdin = strings.NewReader(strings.Join([]string{"admin", "Ticket 43 Admin", stack.AdminPassword, stack.AdminPassword}, "\n") + "\n")
		}
		if retryErr := retry.Run(); retryErr != nil {
			return report, fmt.Errorf("helm install: %w (resume attempt: %v)", err, retryErr)
		}
	}
	// The cluster exposes nothing persistently (compose loopback
	// parity): bring the public and webhook services to this process's
	// loopback through invocation-owned port-forwards.
	if err := stack.startForwards(); err != nil {
		return report, err
	}
	if err := stack.awaitPublic(300 * time.Second); err != nil {
		return report, err
	}
	return report, nil
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
	if err := forward("service/"+stack.releaseName()+"-quoin-public", stack.QuoinPort); err != nil {
		rollback()
		return err
	}
	if err := forward("service/"+stack.releaseName()+"-stele-webhook", stack.StelePort); err != nil {
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
		"deployment/" + stack.releaseName() + "-" + component, "--",
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
		"deployment/" + stack.releaseName() + "-" + component, "--", "/" + component,
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
		"deployment/"+stack.releaseName()+"-"+component, "--tail=300")
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
	if err := stack.startForwardsRequired(stack.releaseName() + "-quoin-public"); err != nil {
		return err
	}
	stack.startForwardsBestEffort(stack.releaseName() + "-stele-webhook")
	return nil
}

func (*kubernetesBackend) down(stack *Stack, _ bool) (string, error) {
	var combined bytes.Buffer
	uninstall := exec.Command("helm", "uninstall", stack.releaseName(), "--namespace", stack.namespace(), "--wait", "--timeout", "120s")
	uninstall.Stdout, uninstall.Stderr = &combined, &combined
	uninstallErr := uninstall.Run()
	if uninstallErr != nil && strings.Contains(uninstallErr.Error(), "release: not found") {
		return combined.String(), nil
	}
	// PVCs are NEVER deleted here: the compose counterpart's data lives
	// on host bind mounts that down -v cannot touch either — the
	// disposable legs depend on data surviving Down, and data destruction
	// belongs to the namespace teardown (the invocation's cleanup).
	return combined.String(), uninstallErr
}

func (*kubernetesBackend) stopComponent(stack *Stack, component string, timeout time.Duration) (string, time.Duration, error) {
	// The graceful-drain fact: delete the component's current pod and
	// read its terminated exit code while the pod object lingers —
	// SIGKILL drains surface as 137 exactly like compose. The
	// deployment is scaled to zero AFTER the drain is observed so the
	// component is genuinely stopped (no replacement pod) until
	// startComponent returns it to service.
	started := time.Now()
	selector := "app.kubernetes.io/instance=" + stack.releaseName() + ",app.kubernetes.io/component=" + component
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
		"deployment/"+stack.releaseName()+"-"+component, "--replicas=0")
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
	deployment := "deployment/" + stack.releaseName() + "-" + component
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
		"deployment/"+stack.releaseName()+"-quoin", "--replicas=0"); err != nil {
		return err
	}
	_, _, err := stack.kubectl("--namespace", stack.namespace(), "wait",
		"--for=delete", "pod", "-l", "app.kubernetes.io/instance="+stack.releaseName()+",app.kubernetes.io/component=quoin",
		"--timeout=90s")
	return err
}

func (*kubernetesBackend) cloneRoot(stack *Stack) string {
	return filepath.Join(stack.WorkRoot, stack.releaseSlug()+"-disp-root")
}

func (*kubernetesBackend) runningWorkloadCount(stack *Stack) int {
	output, _, _ := stack.kubectl("--namespace", stack.namespace(), "get", "pods",
		"-l", "app.kubernetes.io/instance="+stack.releaseName(),
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
	// The disposable lifecycle deployment is a second helm release of
	// the same subject in its own namespace: release-scoped resources,
	// PVCs, secrets and state, with its loopback forwards offset like
	// the compose clone. The separate namespace is required — the chart
	// owns a fixed-name "quoin" Service (the runtime TLS SAN alias) that
	// a same-namespace second release cannot adopt.
	disposableRoot := filepath.Join(stack.WorkRoot, stack.releaseSlug()+"-disp-root")
	_ = os.MkdirAll(disposableRoot, 0o700)
	return &Stack{
		Backend:     stack.Backend,
		Namespace:   stack.namespace() + "-disp",
		ReleaseName: stack.releaseName() + "-disp",
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
			"-l", "app.kubernetes.io/instance="+stack.releaseName(),
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
