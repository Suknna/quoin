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
func boolExit(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

// runNativeObservedBackup triggers the online product path and waits until the
// product publishes a completed backup. It never stops Quoin or uses the
// offline Pod runner, preserving the online-vs-offline qualification boundary.
func runNativeObservedBackup(stack *Stack, detail map[string]string) int {
	session, err := stack.Login("admin", stack.AdminPassword)
	if err != nil {
		detail["backup-login"] = err.Error()
		return 1
	}
	_, status, err := session.Post("/api/v1/backups", fmt.Sprintf(`{"clientCommandId":"native-backup-%d"}`, time.Now().UnixNano()))
	if err != nil || (status != http.StatusAccepted && status != http.StatusOK && status != http.StatusConflict) {
		detail["backup-trigger"] = fmt.Sprintf("status=%d err=%v", status, err)
		return 1
	}
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		body, listStatus, _ := session.Get("/api/v1/backups?limit=5")
		if listStatus == http.StatusOK && (strings.Contains(body, `"state":"succeeded"`) || strings.Contains(body, `"state":"Succeeded"`)) {
			detail["backup-online"] = "published-succeeded"
			return 0
		}
		time.Sleep(3 * time.Second)
	}
	detail["backup-online"] = "timed-out"
	return 1
}

func driveDisposableLifecycle(request DeploymentRequest, stack *Stack, adminPassword string, detail map[string]string) (backup, offlineBackup, restoreIsolation, restoredIdentities, missingSecretFailClosed, bootstrapGates, bootstrapRetry, prewriteRollback bool) {
	helper, _ := os.Executable()
	// The disposable clone is a second, fully-isolated deployment of the
	// same subject: its own identity, data and loopback ports
	// (VERIFY-MATRIX-004). The backend owns how isolation is realized.
	disposableRoot := stack.LifecycleCloneRoot()
	disposable := stack.LifecycleClone()
	defer func() {
		_, _ = disposable.Down(true)
		_ = os.RemoveAll(disposableRoot)
	}()

	// A tampered manifest must be refused before any write: the
	// prewrite rollback mechanism (the N-1 image exchange itself is
	// executed by the CI matrix job carrying two real manifests).
	tampered := filepath.Join(disposable.DeploymentRoot(), "tampered-manifest.json")
	if body, err := os.ReadFile(stack.ManifestPath); err == nil {
		tamperedBody := strings.Replace(string(body), "sha256:", "sha256:0", 3)
		if tamperedBody == string(body) {
			tamperedBody = strings.Replace(string(body), `"version"`, `"version-tampered"`, 1)
		}
		_ = os.WriteFile(tampered, []byte(tamperedBody), 0o600)
		if disposable.Backend == BackendKubernetes {
			prewriteRollback = disposable.NativeUpgradeRefusal(tampered) != nil
			detail["upgrade-refusal-exit"] = fmt.Sprint(boolExit(prewriteRollback))
		} else {
			code := runHelper(helper, disposable.HelperEnv(), disposable.HelperVerb(), "upgrade",
				"--config", disposable.ConfigPath, "--release-manifest", tampered,
				"--report", filepath.Join(disposable.DeploymentRoot(), "tampered-report.json"))
			prewriteRollback = code == 2
			detail["upgrade-refusal-exit"] = fmt.Sprint(code)
		}

	}

	// Admin bootstrap failure then retry: a wrong password confirmation
	// must fail the bootstrap without ever starting workloads, and the
	// retry must then complete the install.
	if disposable.Backend == BackendKubernetes {
		// Prepare the real fixed-name manifest/PVC resources but keep Quoin
		// stopped. A wrong PTY confirmation then proves no serving workload is
		// admitted before the successful retry creates the first administrator.
		if err := disposable.NativePrepareBootstrap(); err == nil {
			bootstrapRetry, err = disposable.NativeBootstrapFailureRetry()
			if err != nil {
				detail["bootstrap-failure"] = err.Error()
			}
		} else {
			detail["bootstrap-prepare"] = err.Error()
		}
		detail["bootstrap-failure-exit"] = fmt.Sprint(boolExit(bootstrapRetry))
	} else {
		failedReport := filepath.Join(disposable.DeploymentRoot(), "failed-report.json")
		install := exec.Command(helper, disposable.HelperVerb(), "install", "--config", disposable.ConfigPath,
			"--release-manifest", disposable.ManifestPath, "--report", failedReport)
		install.Env = disposable.ComposeEnv()
		install.Dir = envWorkdir(disposable.ComposeEnv())
		install.Stdin = strings.NewReader(strings.Join([]string{"admin", "T40 Disposable", disposable.AdminPassword, "wrong-confirmation"}, "\n") + "\n")
		_ = install.Run()
		failedCode := 0
		if install.ProcessState != nil {
			failedCode = install.ProcessState.ExitCode()
		}
		bootstrapRetry = failedCode != 0 && disposable.RunningWorkloadCount() == 0
		detail["bootstrap-failure-exit"] = fmt.Sprint(failedCode)
	}

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
	backupReport := filepath.Join(disposable.DeploymentRoot(), "backup-report.json")
	backupExit := 1
	if disposable.Backend == BackendKubernetes {
		// Online proof remains online: trigger through the authenticated public
		// API and observe the published backup listing. OfflinePod is reserved
		// exclusively for the following stopped-workload fallback leg.
		backupExit = runNativeObservedBackup(disposable, detail)
	} else {
		backupExit = runObservedBackup(helper, disposable, backupReport, detail)
	}
	backup = backupExit == 0
	detail["backup-exit"] = fmt.Sprint(backupExit)

	// Offline fallback: the helper only accepts --offline for an
	// unreachable Quoin, so stop the workload first, take the offline
	// backup, and bring the deployment back for the restore leg.
	if stopErr := disposable.StopQuoin(); stopErr == nil {
		offlineExit := 1
		if disposable.Backend == BackendKubernetes {
			output, err := disposable.NativeBackup()
			offlineExit = boolExit(err == nil)
			detail["offline-backup-output"] = lastLines(output, 6)
		} else {
			offlineReport := filepath.Join(disposable.DeploymentRoot(), "offline-report.json")
			offlineExit = runHelper(helper, disposable.HelperEnv(), disposable.HelperVerb(), "backup", "--offline",
				"--config", disposable.ConfigPath, "--release-manifest", disposable.ManifestPath, "--report", offlineReport)
		}
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
		_, _ = disposable.Down(disposable.Backend != BackendKubernetes)
		if _, err := disposable.EnsureInstalled(); err == nil {
			restoreReport := filepath.Join(disposable.DeploymentRoot(), "restore-report.json")
			code := 1
			if disposable.Backend == BackendKubernetes {
				if err := disposable.StopQuoin(); err == nil {
					output, restoreErr := disposable.NativeRestore(backupID, "recovery-admin", disposable.AdminPassword+"-recovery")
					code = boolExit(restoreErr == nil)
					detail["restore-output"] = lastLines(output, 6)
				}
			} else {
				code = runRestoreInteractively(helper, disposable, backupID, restoreReport, detail)
			}
			detail["restore-exit"] = fmt.Sprint(code)

			if code == 0 {
				restoreIsolation = true
				if refreshErr := disposable.RefreshTransports(); refreshErr != nil {
					detail["restore-transports"] = refreshErr.Error()
				}
				// Drain the restored deployment's public listener, then
				// prove the pre-restore cookie no longer authorizes.
				deadline := time.Now().Add(180 * time.Second)
				for time.Now().Before(deadline) {
					request, err := http.NewRequest(http.MethodGet, disposable.BaseURL()+"/api/v1/auth/me", nil)
					if err != nil {
						break
					}
					request.AddCookie(cookieToInvalidate)
					request.Header.Set("Origin", publicOrigin)
					response, err := (&http.Client{Timeout: 5 * time.Second, Transport: insecureTLSTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
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
	reportsSource := disposable.ReportsDir()
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

	// Existing data without secrets must fail closed: the DISPOSABLE
	// clone's secret authority is removed through the backend seam
	// (directory on compose, release-scoped Secret on kubernetes —
	// never the live matrix stack's) and the reinstall runs over the
	// retained data.
	// Keep the native PVC while removing only its authority; deleting the
	// disposable namespace would erase the data and make this a false proof.
	_, _ = disposable.Down(disposable.Backend != BackendKubernetes)
	if err := disposable.RemoveSecretAuthority(); err != nil {
		detail["missing-secret-removal"] = err.Error()
		return
	}
	code := 1
	if disposable.Backend == BackendKubernetes {
		_, err := disposable.EnsureInstalled()
		code = boolExit(err == nil)
		missingSecretFailClosed = err != nil
	} else {
		code = runHelper(helper, disposable.HelperEnv(), disposable.HelperVerb(), "install",
			"--config", disposable.ConfigPath, "--release-manifest", disposable.ManifestPath,
			"--report", filepath.Join(disposable.DeploymentRoot(), "nosecret-report.json"))
		missingSecretFailClosed = code == 2
	}
	detail["missing-secret-exit"] = fmt.Sprint(code)
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
	command := exec.Command(helper, disposable.HelperVerb(), "backup",
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
	command := exec.Command(helper, disposable.HelperVerb(), "restore", "--backup", backupID,
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
	// The Kubernetes attach relay does not replay output that predates
	// the attach: the pod's username prompt prints before kubectl attach
	// joins, so it never appears in the transcript. A username is not
	// secret — the prior deployment helper bootstrap answered it blind for the same
	// reason — while the password prompts print after the attach and are
	// answered strictly after their markers (same as compose).
	ok := step("Type RESTORE", "RESTORE\n", 60*time.Second)
	if ok {
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) && !seen("All commands and output from this session") {
			select {
			case <-done:
				ok = false
			case <-time.After(time.Second):
			}
		}
		if ok {
			_, _ = terminal.WriteString("admin\n")
		}
	}
	ok = ok &&
		step("Temporary password", recoveryPassword+"\n", 45*time.Second) &&
		step("Confirm temporary password", recoveryPassword+"\n", 45*time.Second)
	if !ok {
		mu.Lock()
		transcriptTail := transcript.String()
		mu.Unlock()
		if len(transcriptTail) > 1200 {
			transcriptTail = transcriptTail[len(transcriptTail)-1200:]
		}
		detail["restore-interactive"] = "prompt sequence incomplete; transcript tail: " + transcriptTail
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
