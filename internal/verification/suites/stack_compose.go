package suites

// The compose implementation of the suite deployment backend: install
// through the helper's staged compose path, one-shot services through
// docker compose run, loopback-published ports, and project-scoped
// state isolation.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// composeBackend drives one docker compose project per Stack.
type composeBackend struct{}

func (*composeBackend) helperVerb() string { return "compose" }

func (*composeBackend) opsBase(stack *Stack) string {
	return "http://quoin:9090"
}

func (*composeBackend) helperEnv(stack *Stack) []string {
	return append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(stack.WorkRoot, stack.Project, "state"),
		"QUOIN_COMPOSE_PROJECT="+stack.Project,
		"QUOIN_DEPLOY_SCRIPTED=1",
		"DOCKER_CLI_HINTS=false",
	)
}

func (*composeBackend) ensureInstalled(stack *Stack) (string, error) {
	helper, err := os.Executable()
	if err != nil {
		return "", err
	}
	report := filepath.Join(stack.WorkRoot, stack.Project, "install-report.json")
	install := exec.Command(helper, "compose", "install", "--config", stack.ConfigPath,
		"--release-manifest", stack.ManifestPath, "--report", report)
	install.Env = stack.HelperEnv()
	install.Dir = workDirOf(helper)
	install.Stdout, install.Stderr = stack.Stdout, stack.Stderr
	if stack.AdminPassword != "" {
		install.Stdin = strings.NewReader(strings.Join([]string{"admin", "Ticket 40 Admin", stack.AdminPassword, stack.AdminPassword}, "\n") + "\n")
	}
	if err := install.Run(); err != nil {
		return report, fmt.Errorf("compose install: %w", err)
	}
	stack.composeFile = filepath.Join(stack.WorkRoot, stack.Project, "state", "quoin", "compose", "generated", "compose.yaml")
	if err := stack.awaitPublic(300 * time.Second); err != nil {
		return report, err
	}
	return report, nil
}

func (*composeBackend) execCommand(stack *Stack, component string, arguments []string) (string, int, error) {
	full := append([]string{
		"compose", "--project-name", stack.Project, "--file", stack.composeFile,
		"exec", "-T", component,
	}, arguments...)
	return stack.docker(full...)
}

func (*composeBackend) runService(stack *Stack, component string, arguments []string, stdinPayload string) (string, int, error) {
	full := append([]string{
		"compose", "--project-name", stack.Project, "--file", stack.composeFile,
		"run", "--rm", "--no-deps", "-i", "-T", component,
	}, arguments...)
	command := exec.Command("docker", full...)
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

func (*composeBackend) logs(stack *Stack, component string) (string, error) {
	output, _, err := stack.docker("compose", "--project-name", stack.Project, "--file", stack.composeFile,
		"logs", "--no-log-prefix", "--tail", "300", component)
	return output, err
}

// docker runs one docker CLI invocation in the project environment.
func (stack *Stack) docker(arguments ...string) (string, int, error) {
	command := exec.Command("docker", arguments...)
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

func (*composeBackend) refreshTransports(stack *Stack) error {
	// Published container ports follow the containers natively.
	return nil
}

func (*composeBackend) down(stack *Stack, dataRemoval bool) (string, error) {
	arguments := []string{"compose", "--project-name", stack.Project, "down", "--remove-orphans", "--timeout", "45"}
	if dataRemoval {
		arguments = append(arguments, "-v")
	}
	output, _, err := stack.docker(arguments...)
	return output, err
}

func (*composeBackend) stopComponent(stack *Stack, component string, timeout time.Duration) (string, time.Duration, error) {
	started := time.Now()
	if _, _, err := stack.docker("compose", "--project-name", stack.Project, "stop", "--timeout",
		strconv.Itoa(int(timeout.Seconds())), component); err != nil {
		return "", time.Since(started), err
	}
	elapsed := time.Since(started)
	status, _, _ := stack.docker("ps", "-a", "--filter", "name="+stack.Project+"-"+component, "--format", "{{.Status}}")
	code := ""
	if matches := exitStatusPattern.FindStringSubmatch(strings.TrimSpace(status)); matches != nil {
		code = matches[1]
	}
	return code, elapsed, nil
}

func (*composeBackend) startComponent(stack *Stack, component string) error {
	_, _, err := stack.docker("compose", "--project-name", stack.Project, "--file", stack.composeFile,
		"up", "-d", "--no-deps", component)
	return err
}

func (*composeBackend) removeSecretAuthority(stack *Stack) error {
	secretsDir := secretDirectoryOf(stack.ConfigPath)
	if secretsDir == "" {
		return fmt.Errorf("compose install config carries no secretDirectory")
	}
	return os.RemoveAll(secretsDir)
}

func (*composeBackend) stopQuoin(stack *Stack) error {
	_, _, err := stack.docker("compose", "--project-name", stack.Project, "stop", "--timeout", "40", "quoin")
	return err
}

func (*composeBackend) cloneRoot(stack *Stack) string {
	return filepath.Join(stack.WorkRoot, stack.Project+"-disp-root")
}

func (*composeBackend) runningWorkloadCount(stack *Stack) int {
	output, _ := exec.Command("docker", "compose", "--project-name", stack.Project,
		"ps", "--status", "running", "--format", "{{.Name}}").Output()
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.Contains(line, "plinth") || strings.Contains(line, "lintel") {
			count++
		}
	}
	return count
}

func (*composeBackend) deploymentRoot(stack *Stack) string {
	return filepath.Join(stack.WorkRoot, stack.Project)
}

func (*composeBackend) reportsDir(stack *Stack) string {
	return filepath.Join(stack.WorkRoot, stack.Project, "state", "quoin", "compose", "reports")
}

func (backend *composeBackend) clone(stack *Stack) *Stack {
	// The disposable clone deploys its own ports, secret directory and
	// project name so it never collides with the live matrix stack
	// (VERIFY-MATRIX-004: unique project and independent business
	// volumes per invocation-owned deployment).
	disposableRoot := filepath.Join(stack.WorkRoot, stack.Project+"-disp-root")
	_ = os.MkdirAll(filepath.Join(disposableRoot, "secrets"), 0o700)
	disposableConfig := filepath.Join(disposableRoot, "install.yaml")
	configBody, _ := os.ReadFile(stack.ConfigPath)
	replaced := string(configBody)
	replaced = strings.ReplaceAll(replaced, fmt.Sprintf("quoinPublicHostPort: %d", stack.QuoinPort), fmt.Sprintf("quoinPublicHostPort: %d", stack.QuoinPort+20))
	replaced = strings.ReplaceAll(replaced, fmt.Sprintf("steleWebhookHostPort: %d", stack.StelePort), fmt.Sprintf("steleWebhookHostPort: %d", stack.StelePort+20))
	secretDirectory := ""
	for _, line := range strings.Split(replaced, "\n") {
		if strings.HasPrefix(line, "secretDirectory:") {
			secretDirectory = strings.TrimSpace(strings.TrimPrefix(line, "secretDirectory:"))
		}
	}
	replaced = strings.ReplaceAll(replaced, secretDirectory, filepath.Join(disposableRoot, "secrets"))
	_ = os.WriteFile(disposableConfig, []byte(replaced), 0o600)
	return &Stack{
		Backend: stack.Backend,
		Project: stack.Project + "-disp", WorkRoot: stack.WorkRoot,
		ConfigPath: disposableConfig, ManifestPath: stack.ManifestPath,
		AdminPassword: RandomPassword(), QuoinPort: stack.QuoinPort + 20, StelePort: stack.StelePort + 20,
		Stdout: stack.Stdout, Stderr: stack.Stderr,
	}
}
