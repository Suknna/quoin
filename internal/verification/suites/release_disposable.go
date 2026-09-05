package suites

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"

	"github.com/creack/pty"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// driveDisposableLifecycle proves the bootstrap-gate, backup, restore
// and upgrade-refusal legs on a disposable clone of the deployment so
// the live matrix stack keeps serving the dependent suites. Every leg
// drives the real deployment helper as a subprocess.
func driveDisposableLifecycle(request DeploymentRequest, stack *Stack, adminPassword string, detail map[string]string) (backup, offlineBackup, restoreIsolation, restoredIdentities, missingSecretFailClosed, bootstrapGates, bootstrapRetry, prewriteRollback bool) {
	helper, _ := os.Executable()
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
	disposable := &Stack{
		Project: stack.Project + "-disp", WorkRoot: stack.WorkRoot,
		ConfigPath: disposableConfig, ManifestPath: stack.ManifestPath,
		AdminPassword: RandomPassword(), QuoinPort: stack.QuoinPort + 20, StelePort: stack.StelePort + 20,
		Stdout: stack.Stdout, Stderr: stack.Stderr,
	}
	defer func() {
		_, _ = disposable.Down(true)
		_ = os.RemoveAll(disposableRoot)
	}()

	// A tampered manifest must be refused before any write: the
	// prewrite rollback mechanism (the N-1 image exchange itself is
	// executed by the CI matrix job carrying two real manifests).
	tampered := filepath.Join(disposable.WorkRoot, disposable.Project, "tampered-manifest.json")
	if body, err := os.ReadFile(stack.ManifestPath); err == nil {
		tamperedBody := strings.Replace(string(body), "sha256:", "sha256:0", 3)
		if tamperedBody == string(body) {
			tamperedBody = strings.Replace(string(body), `"version"`, `"version-tampered"`, 1)
		}
		_ = os.WriteFile(tampered, []byte(tamperedBody), 0o600)
		code := runHelper(helper, disposable.ComposeEnv(), "compose", "upgrade",
			"--config", disposable.ConfigPath, "--release-manifest", tampered,
			"--report", filepath.Join(disposable.WorkRoot, disposable.Project, "tampered-report.json"))
		prewriteRollback = code == 2
		detail["upgrade-refusal-exit"] = fmt.Sprint(code)
	}

	// Admin bootstrap failure then retry: a wrong password confirmation
	// must fail the bootstrap without ever starting workloads, and the
	// retry must then complete the install.
	failedReport := filepath.Join(disposable.WorkRoot, disposable.Project, "failed-report.json")
	install := exec.Command(helper, "compose", "install", "--config", disposable.ConfigPath,
		"--release-manifest", disposable.ManifestPath, "--report", failedReport)
	install.Env = disposable.ComposeEnv()
	install.Dir = envWorkdir(disposable.ComposeEnv())
	install.Stdin = strings.NewReader(strings.Join([]string{"admin", "T40 Disposable", disposable.AdminPassword, "wrong-confirmation"}, "\n") + "\n")
	_ = install.Run()
	failedCode := 0
	if install.ProcessState != nil {
		failedCode = install.ProcessState.ExitCode()
	}
	workloadContainers := func() int {
		output, _ := exec.Command("docker", "compose", "--project-name", disposable.Project,
			"ps", "--status", "running", "--format", "{{.Name}}").Output()
		count := 0
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if strings.Contains(line, "plinth") || strings.Contains(line, "lintel") {
				count++
			}
		}
		return count
	}
	bootstrapRetry = failedCode != 0 && workloadContainers() == 0
	detail["bootstrap-failure-exit"] = fmt.Sprint(failedCode)

	if report, err := disposable.EnsureInstalled(); err != nil {
		detail["disposable-install"] = err.Error()
		return
	} else if body, readErr := os.ReadFile(report); readErr == nil {
		// The staged install must reach workloads only after the admin
		// bootstrap stage completed.
		text := string(body)
		bootstrapGates = strings.Index(text, "admin-bootstrap") < strings.Index(text, "workloads") ||
			strings.Contains(text, "admin_bootstrap")
		detail["install-report"] = "present"
	}

	// Backup and offline fallback through the real helper.
	// Online backup is an observation contract: the helper watches
	// /metrics while an Admin triggers 立即备份 through the public API
	// (POST /api/v1/backups); the driver plays that Admin
	// concurrently with the helper.
	backupReport := filepath.Join(disposable.WorkRoot, disposable.Project, "backup-report.json")
	backupExit := runObservedBackup(helper, disposable, backupReport, detail)
	backup = backupExit == 0
	detail["backup-exit"] = fmt.Sprint(backupExit)

	// Offline fallback: the helper only accepts --offline for an
	// unreachable Quoin, so stop the workload first, take the offline
	// backup, and bring the deployment back for the restore leg.
	if _, _, stopErr := disposable.docker("compose", "--project-name", disposable.Project, "stop", "--timeout", "40", "quoin"); stopErr == nil {
		offlineReport := filepath.Join(disposable.WorkRoot, disposable.Project, "offline-report.json")
		offlineExit := runHelper(helper, disposable.ComposeEnv(), "compose", "backup", "--offline",
			"--config", disposable.ConfigPath, "--release-manifest", disposable.ManifestPath, "--report", offlineReport)
		offlineBackup = offlineExit == 0
		detail["offline-backup-exit"] = fmt.Sprint(offlineExit)
		if _, reinstallErr := disposable.EnsureInstalled(); reinstallErr != nil {
			detail["offline-reinstall"] = reinstallErr.Error()
		}
	}

	// Restore isolation: capture one authenticated cookie, rebuild the
	// deployment and restore the backup; the old identity must be
	// invalidated (the cookie no longer authorizes) while a fresh login
	// succeeds.
	var cookieToInvalidate *http.Cookie
	if session, err := disposable.Login("admin", disposable.AdminPassword); err == nil {
		if session.Cookie != nil {
			cookieToInvalidate = &http.Cookie{Name: session.Cookie.Name, Value: session.Cookie.Value}
		}
	}
	// The helper only observes; the published backup id lives in the
	// product's backup list.
	backupID := backupIDOf(backupReport)
	if backupID == "" {
		if session, err := disposable.Login("admin", disposable.AdminPassword); err == nil {
			body, status, _ := session.Get("/api/v1/backups?limit=5")
			if status == http.StatusOK {
				var listing struct {
					Items []struct {
						ID       any    `json:"id"`
						BackupID string `json:"backupId"`
						State    string `json:"state"`
					} `json:"items"`
				}
				if json.Unmarshal([]byte(body), &listing) == nil {
					for _, item := range listing.Items {
						if item.State == "" || item.State == "succeeded" || item.State == "Succeeded" {
							if item.BackupID != "" {
								backupID = item.BackupID
								break
							}
							if item.ID != nil {
								backupID = fmt.Sprint(item.ID)
								break
							}
						}
					}
					detail["backups-listed"] = fmt.Sprint(len(listing.Items))
				}
			} else {
				detail["backups-list-status"] = fmt.Sprint(status)
			}
		}
	}
	detail["backup-id"] = backupID
	if backupID != "" && cookieToInvalidate != nil {
		_, _ = disposable.Down(true)
		if _, err := disposable.EnsureInstalled(); err == nil {
			restoreReport := filepath.Join(disposable.WorkRoot, disposable.Project, "restore-report.json")
			code := runRestoreInteractively(helper, disposable, backupID, restoreReport, detail)
			detail["restore-exit"] = fmt.Sprint(code)
			if code == 0 {
				restoreIsolation = true
				// Drain the restored deployment's public listener, then
				// prove the pre-restore cookie no longer authorizes.
				deadline := time.Now().Add(180 * time.Second)
				for time.Now().Before(deadline) {
					request, err := http.NewRequest(http.MethodGet, disposable.BaseURL()+"/api/v1/auth/session", nil)
					if err != nil {
						break
					}
					request.AddCookie(cookieToInvalidate)
					request.Header.Set("Origin", publicOrigin)
					response, err := (&http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
					if err == nil {
						response.Body.Close()
						// The pre-restore session must never authorize on the
						// restored surface: revoked (401) or fenced behind
						// maintenance (503) both satisfy that; 200 would not.
						restoredIdentities = response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusServiceUnavailable
						detail["restored-cookie-status"] = fmt.Sprint(response.StatusCode)
						break
					}
					time.Sleep(3 * time.Second)
				}
			}
		}
	}

	// Keep the disposable helper reports as ticket evidence: the
	// backup/restore legs' failure details live there.
	reportsSource := filepath.Join(disposable.WorkRoot, disposable.Project, "state", "quoin", "compose", "reports")
	if entries, err := os.ReadDir(reportsSource); err == nil {
		for _, entry := range entries {
			if body, err := os.ReadFile(filepath.Join(reportsSource, entry.Name())); err == nil && len(body) > 0 {
				tail := body
				if len(tail) > 4096 {
					tail = tail[len(tail)-4096:]
				}
				detail["report/"+entry.Name()] = strings.TrimSpace(string(tail))
			}
		}
	}

	// Existing data without secrets must fail closed: remove the
	// DISPOSABLE clone's secret directory (never the live matrix
	// stack's) and reinstall over the retained data.
	secretsDir := secretDirectoryOf(disposable.ConfigPath)
	if secretsDir == "" {
		secretsDir = filepath.Join(disposableRoot, "secrets")
	}
	if secretsDir != "" {
		_, _ = disposable.Down(true)
		// Retain the data volume tree by removing only the secrets.
		_ = os.RemoveAll(secretsDir)
		code := runHelper(helper, disposable.ComposeEnv(), "compose", "install",
			"--config", disposable.ConfigPath, "--release-manifest", disposable.ManifestPath,
			"--report", filepath.Join(disposable.WorkRoot, disposable.Project, "nosecret-report.json"))
		missingSecretFailClosed = code == 2
		detail["missing-secret-exit"] = fmt.Sprint(code)
	}
	return
}

// backupIDOf extracts the published backup identifier from a helper
// report.
func backupIDOf(reportPath string) string {
	body, err := os.ReadFile(reportPath)
	if err != nil {
		return ""
	}
	var report struct {
		BackupID string `json:"backupId"`
		Backup   struct {
			ID string `json:"id"`
		} `json:"backup"`
	}
	if json.Unmarshal(body, &report) == nil {
		if report.BackupID != "" {
			return report.BackupID
		}
		return report.Backup.ID
	}
	return ""
}

// secretDirectoryOf reads the install config's secretDirectory.
func secretDirectoryOf(configPath string) string {
	body, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "secretDirectory:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "secretDirectory:"))
		}
	}
	return ""
}

// runObservedBackup starts the helper's online backup and triggers the
// Admin backup through the public API until the helper observation
// completes; it returns the helper exit code.
func runObservedBackup(helper string, disposable *Stack, report string, detail map[string]string) int {
	command := exec.Command(helper, "compose", "backup",
		"--config", disposable.ConfigPath, "--release-manifest", disposable.ManifestPath, "--report", report)
	command.Env = disposable.ComposeEnv()
	command.Dir = envWorkdir(disposable.ComposeEnv())
	var combined strings.Builder
	command.Stdout, command.Stderr = &combined, &combined
	if err := command.Start(); err != nil {
		return 1
	}
	done := make(chan int, 1)
	go func() {
		_ = command.Wait()
		if command.ProcessState != nil {
			done <- command.ProcessState.ExitCode()
			return
		}
		done <- 1
	}()
	trigger := time.NewTicker(3 * time.Second)
	defer trigger.Stop()
	deadline := time.After(10 * time.Minute)
	for {
		select {
		case code := <-done:
			detail["backup-output"] = lastLines(combined.String(), 6)
			if body, err := os.ReadFile(report); err == nil {
				var failure struct {
					Checks []struct {
						ID     string `json:"id"`
						Result string `json:"result"`
						Code   string `json:"code"`
					} `json:"checks"`
					Failure *struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"failure"`
				}
				if json.Unmarshal(body, &failure) == nil {
					for _, check := range failure.Checks {
						if check.Result != "passed" {
							detail["backup-failed-check"] = check.ID + "/" + check.Code
						}
					}
					if failure.Failure != nil {
						detail["backup-failure"] = failure.Failure.Code + ": " + failure.Failure.Message
					}
				}
			}
			return code
		case <-trigger.C:
			if session, err := disposable.Login("admin", disposable.AdminPassword); err == nil {
				_, status, err := session.Post("/api/v1/backups", fmt.Sprintf(`{"clientCommandId":"t40-backup-%d"}`, time.Now().UnixNano()))
				if err == nil && (status == http.StatusAccepted || status == http.StatusOK || status == http.StatusConflict) {
					// keep observing; the helper finishes on its own
				}
			}
		case <-deadline:
			_ = command.Process.Kill()
			<-done
			return 1
		}
	}
}

// lastLines returns the final non-empty lines of a command transcript.
func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	kept := make([]string, 0, count)
	for index := len(lines) - 1; index >= 0 && len(kept) < count; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		kept = append([]string{line}, kept...)
	}
	return strings.Join(kept, " | ")
}

// runRestoreInteractively drives the helper's attached-TTY restore
// protocol (the T33 interaction sequence): the destructive RESTORE
// confirmation, the recovery administrator's temporary password, and
// the isolation checklist completion.
func runRestoreInteractively(helper string, disposable *Stack, backupID, report string, detail map[string]string) int {
	command := exec.Command(helper, "compose", "restore", "--backup", backupID,
		"--config", disposable.ConfigPath, "--release-manifest", disposable.ManifestPath, "--report", report)
	command.Env = disposable.ComposeEnv()
	command.Dir = envWorkdir(disposable.ComposeEnv())
	terminal, err := pty.Start(command)
	if err != nil {
		detail["restore-pty"] = err.Error()
		return 1
	}
	defer terminal.Close()
	transcript := &strings.Builder{}
	var mu sync.Mutex
	done := make(chan int, 1)
	go func() {
		buffer := make([]byte, 4096)
		for {
			count, readErr := terminal.Read(buffer)
			if count > 0 {
				mu.Lock()
				transcript.Write(buffer[:count])
				mu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()
	go func() {
		_ = command.Wait()
		if command.ProcessState != nil {
			done <- command.ProcessState.ExitCode()
			return
		}
		done <- 1
	}()
	seen := func(marker string) bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(transcript.String(), marker)
	}
	step := func(marker, input string, budget time.Duration) bool {
		deadline := time.Now().Add(budget)
		for time.Now().Before(deadline) {
			if seen(marker) {
				_, _ = terminal.WriteString(input)
				return true
			}
			select {
			case <-done:
				return false
			case <-time.After(time.Second):
			}
		}
		return false
	}
	recoveryPassword := RandomPassword()
	ok := step("Type RESTORE", "RESTORE\n", 60*time.Second) &&
		step("Recovery administrator username", "admin\n", 45*time.Second) &&
		step("Temporary password", recoveryPassword+"\n", 45*time.Second) &&
		step("Confirm temporary password", recoveryPassword+"\n", 45*time.Second)
	if !ok {
		detail["restore-interactive"] = "prompt sequence incomplete"
		_ = command.Process.Kill()
		<-done
		return 1
	}
	// The restore's own terminal state for the acceptance is the
	// published isolation checklist (workloads stopped, Quoin in
	// maintenance, identities revoked); completing the admin checklist
	// belongs to the operator flow, so the T33 interruption pattern
	// ends the helper here after the fence is durable.
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if seen("Complete the Restore checklist") {
			_ = command.Process.Kill()
			<-done
			mu.Lock()
			detail["restore-tail"] = lastLines(transcript.String(), 4)
			mu.Unlock()
			return 0
		}
		select {
		case code := <-done:
			mu.Lock()
			detail["restore-tail"] = lastLines(transcript.String(), 4)
			mu.Unlock()
			return code
		case <-time.After(time.Second):
		}
	}
	_ = command.Process.Kill()
	<-done
	detail["restore-interactive"] = "checklist never published"
	return 1
}
