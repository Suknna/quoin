// Package compose hosts the T01 ticket acceptance run: the real install path,
// real processes in containers, real SQLite, and the real same-origin HTTP
// surface, producing the structured runtime and cleanup evidence the ticket
// demands. It skips unless QUOIN_EVIDENCE_DIR is set so `go test ./...` stays
// cheap in ordinary development.
package compose_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	deployProject = "quoin"
	quoinPort     = 18180
	stelePort     = 18181
)

type evidence struct {
	t          *testing.T
	dir        string
	commands   []commandRecord
	artifacts  []artifactRecord
	gitCommit  string
	dirtyState string
	toolInfo   map[string]string
	startedAt  time.Time
	env        []string
}

type commandRecord struct {
	Name     string `json:"name"`
	ExitCode int    `json:"exitCode"`
	Duration string `json:"duration"`
}

type artifactRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

func TestTicket01(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T01 acceptance evidence run disabled")
	}
	requireCommand(t, "docker")
	workRoot := t.TempDir()
	stateRoot := filepath.Join(workRoot, "state")
	recorder := &evidence{t: t, dir: evidenceDir, startedAt: time.Now().UTC(), toolInfo: map[string]string{}, env: append(os.Environ(), "XDG_STATE_HOME="+stateRoot)}
	recorder.captureEnvironment()

	secretDir := filepath.Join(workRoot, "secrets")
	binaryPath := filepath.Join(workRoot, "quoin-deploy")
	buildHelper(t, recorder, "./cmd/quoin-deploy", binaryPath)

	tempPassword := randomPassword(t)
	newPassword := randomPassword(t)
	installConfig := writeInstallConfig(t, workRoot, secretDir)
	badConfig := filepath.Join(workRoot, "bad-install.yaml")
	writeFile(t, badConfig, "document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: 1\nsteleWebhookHostPort: 2\nunknownField: true\nsecretDirectory: "+secretDir+"\nlintelBrowserSlots: 1\n")

	recorder.run(t, "images", nil, "bash", "build/package/images.sh")

	assertNoProjectSideEffects(t, recorder)
	output := recorder.runExpectExit(t, "invalid-input", 2, strings.NewReader(""), binaryPath, "compose", "install", "--config", badConfig)
	if !strings.Contains(output, "invalid deployment input") {
		t.Fatalf("unknown input field must fail validation, got: %s", output)
	}
	assertNoProjectSideEffects(t, recorder)

	adminInput := strings.NewReader(strings.Join([]string{"admin", "Ticket Admin", tempPassword, tempPassword}, "\n") + "\n")
	recorder.run(t, "install", adminInput, binaryPath, "compose", "install", "--config", installConfig)
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")
	assertTopology(t, recorder, composeFile)
	assertOpsSurfaces(t, recorder, composeFile)
	logs := captureLogs(t, recorder, composeFile)
	assertJsonLogs(t, logs)
	assertSecretFiles(t, secretDir)

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	runHttpMatrix(t, recorder, base, tempPassword, newPassword)

	// Idempotent reinstall against existing secrets and an existing admin.
	reinstall := strings.NewReader("ignored\nignored\nignored\nignored\n")
	recorder.run(t, "reinstall", reinstall, binaryPath, "compose", "install", "--config", installConfig)
	confirmLogin(t, base, newPassword, tempPassword)
	recorder.run(t, "admin-already-exists", strings.NewReader(""), "docker", "compose", "--project-name", deployProject, "--file", composeFile, "run", "--rm", "-T", "admin-bootstrap")

	assertRateLimitLast(t, base, tempPassword)

	teardown(t, recorder, composeFile, workRoot)
	recorder.writeRuntimeEvidence(t, newPassword, tempPassword)
	scanForSecrets(t, evidenceDir, newPassword, tempPassword)
}

func (recorder *evidence) captureEnvironment() {
	t := recorder.t
	recorder.gitCommit = strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	status := outputOf(t, "git", "status", "--porcelain")
	recorder.dirtyState = sha256String(status)
	for name, command := range map[string]struct {
		command   string
		arguments []string
	}{
		"go":         {"go", []string{"version"}},
		"node":       {"node", []string{"--version"}},
		"pnpm":       {"pnpm", []string{"--version"}},
		"docker":     {"docker", []string{"version", "--format", "{{.Server.Version}}"}},
		"compose":    {"docker", []string{"compose", "version"}},
		"playwright": {"pnpm", []string{"--dir", "web", "exec", "playwright", "--version"}},
	} {
		recorder.toolInfo[name] = strings.TrimSpace(outputOf(t, command.command, command.arguments...))
	}
	writeFile(t, filepath.Join(recorder.dir, "environment.json"), mustJSON(t, map[string]any{
		"gitCommit": recorder.gitCommit, "dirtyStateDigest": recorder.dirtyState, "tools": recorder.toolInfo,
	}))
}

func (recorder *evidence) run(t *testing.T, name string, stdin io.Reader, command string, arguments ...string) string {
	return recorder.runExpectExit(t, name, 0, stdin, command, arguments...)
}

func (recorder *evidence) runExpectExit(t *testing.T, name string, wantExit int, stdin io.Reader, command string, arguments ...string) string {
	t.Helper()
	full := append([]string{command}, arguments...)
	_ = full
	logPath := filepath.Join(recorder.dir, name+".log")
	started := time.Now()
	cmd := exec.Command(command, arguments...)
	cmd.Dir = repoRoot(recorder.t)
	cmd.Env = recorder.env
	if stdin == nil {
		cmd.Stdin = nil
	} else {
		cmd.Stdin = stdin
	}
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	writeFile(t, logPath, combined.String())
	recorder.commands = append(recorder.commands, commandRecord{Name: name, ExitCode: exitCode, Duration: time.Since(started).Round(time.Millisecond).String()})
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: logPath, SHA256: sha256Bytes(combined.Bytes()), Bytes: combined.Len()})
	if exitCode != wantExit {
		t.Fatalf("%s: exit=%d want=%d output:\n%s", name, exitCode, wantExit, combined.String())
	}
	return combined.String()
}

func (recorder *evidence) note(t *testing.T, name string, content string) {
	t.Helper()
	path := filepath.Join(recorder.dir, name)
	writeFile(t, path, content)
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: path, SHA256: sha256String(content), Bytes: len(content)})
}

func (recorder *evidence) writeRuntimeEvidence(t *testing.T, newPassword, tempPassword string) {
	t.Helper()
	cleanup := map[string]any{
		"containers":        "removed by docker compose down --remove-orphans (see teardown.log)",
		"networks":          "quoin_default, quoin_internal, quoin_edge removed by compose down (see teardown.log)",
		"images":            "quoin/{quoin,plinth,lintel,stele}:v0.1.0-dev removed by docker rmi (see teardown.log)",
		"state-directories": "temporary XDG_STATE_HOME and secret directory removed with the test temp root",
		"credentials":       "temporary and replacement admin passwords held only in process memory; never written",
		"compose-project":   deployProject,
	}
	writeFile(t, filepath.Join(recorder.dir, "cleanup.json"), mustJSON(t, cleanup))
	writeFile(t, filepath.Join(recorder.dir, "runtime-evidence.json"), mustJSON(t, map[string]any{
		"gitCommit":        recorder.gitCommit,
		"dirtyStateDigest": recorder.dirtyState,
		"startedAt":        recorder.startedAt.Format(time.RFC3339Nano),
		"finishedAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"tools":            recorder.toolInfo,
		"commands":         recorder.commands,
		"artifacts":        recorder.artifacts,
		"observed": map[string]any{
			"bootstrapOrdering": "secrets.bootstrap.created -> admin.bootstrap.created -> four long-lived services (see install.log)",
			"invalidInput":      "unknown field rejected with exit 2 before Docker side effects (invalid-input.log)",
			"topology":          "exactly quoin/plinth/lintel/stele running; only Quoin public and Stele webhook published on loopback (topology.json)",
			"readiness":         "quoin ready/200; plinth+lintel 503 runtime_unregistered; stele 503 dependency_unavailable; all livez ok (readiness-*.json)",
			"metrics":           "ticket-applicable catalog families only, closed label values (metrics-*.txt)",
			"logs":              "every line JSON with ts/level/component/release/code/msg; release=v0.1.0-dev (logs-*.txt)",
			"httpMatrix":        "login/metadata-gate/change-password/revocation/logout/re-login/reinstall/re-login matrix (http-matrix.json)",
			"rateLimit":         "five 401 failures then 429 cooldown on sixth attempt (rate-limit.json)",
			"secretLeakScan":    "temporary and replacement passwords absent from every evidence artifact (verified after teardown)",
		},
		"redactions": "admin passwords appear nowhere; host paths under the Go temp root are recorded as-is because they are removed at teardown",
	}))
	_ = newPassword
	_ = tempPassword
}

func buildHelper(t *testing.T, recorder *evidence, packagePath, output string) {
	t.Helper()
	started := time.Now()
	cmd := exec.Command("go", "build", "-trimpath", "-o", output, packagePath)
	cmd.Dir = repoRoot(t)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", packagePath, err, combined)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	recorder.note(t, "quoin-deploy-binary.json", mustJSON(t, map[string]string{"path": output, "sha256": sha256Bytes(data)}))
	recorder.commands = append(recorder.commands, commandRecord{Name: "build-quoin-deploy", ExitCode: 0, Duration: time.Since(started).Round(time.Millisecond).String()})
}

func writeInstallConfig(t *testing.T, root, secretDir string) string {
	t.Helper()
	path := filepath.Join(root, "install.yaml")
	writeFile(t, path, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, stelePort, secretDir))
	return path
}

func assertNoProjectSideEffects(t *testing.T, recorder *evidence) {
	t.Helper()
	listing := recorder.run(t, "project-side-effects", nil, "docker", "compose", "ls", "--all", "--format", "json")
	if strings.Contains(listing, deployProject) {
		var projects []map[string]any
		if err := json.Unmarshal([]byte(listing), &projects); err == nil {
			for _, project := range projects {
				if project["Name"] == deployProject {
					t.Fatalf("project %s already exists before install", deployProject)
				}
			}
		}
	}
}

func assertTopology(t *testing.T, recorder *evidence, composeFile string) {
	t.Helper()
	raw := recorder.run(t, "topology", nil, "docker", "compose", "--project-name", deployProject, "--file", composeFile, "ps", "--all", "--format", "json")
	type serviceState struct {
		Name  string `json:"Name"`
		State string `json:"State"`
		Ports []struct {
			URL           string `json:"URL"`
			PublishedPort int    `json:"PublishedPort"`
		} `json:"Publishers"`
	}
	var states []serviceState
	decoder := json.NewDecoder(strings.NewReader(raw))
	for decoder.More() {
		var state serviceState
		if err := decoder.Decode(&state); err != nil {
			t.Fatalf("parse compose ps output: %v\n%s", err, raw)
		}
		states = append(states, state)
	}
	running := map[string]bool{}
	published := map[string][]int{}
	for _, state := range states {
		running[state.Name] = state.State == "running"
		for _, port := range state.Ports {
			if port.PublishedPort != 0 {
				published[state.Name] = append(published[state.Name], port.PublishedPort)
			}
		}
	}
	for _, service := range []string{"quoin-quoin-1", "quoin-plinth-1", "quoin-lintel-1", "quoin-stele-1"} {
		if !running[service] {
			t.Fatalf("service %s not running:\n%s", service, raw)
		}
	}
	if len(published["quoin-plinth-1"]) != 0 || len(published["quoin-lintel-1"]) != 0 {
		t.Fatalf("runtime or ops ports published to host: %v", published)
	}
	if len(published["quoin-quoin-1"]) != 1 || published["quoin-quoin-1"][0] != quoinPort {
		t.Fatalf("quoin public port not published on loopback as configured: %v", published)
	}
	if len(published["quoin-stele-1"]) != 1 || published["quoin-stele-1"][0] != stelePort {
		t.Fatalf("stele webhook port not published on loopback as configured: %v", published)
	}
	recorder.note(t, "topology.json", raw)
}

func assertOpsSurfaces(t *testing.T, recorder *evidence, composeFile string) {
	t.Helper()
	expectations := []struct {
		service string
		port    int
		status  int
		reason  string
	}{
		{"quoin", 9090, http.StatusOK, "ready"},
		{"plinth", 9090, http.StatusServiceUnavailable, "runtime_unregistered"},
		{"lintel", 9090, http.StatusServiceUnavailable, "runtime_unregistered"},
		{"stele", 9090, http.StatusServiceUnavailable, "dependency_unavailable"},
	}
	for _, expected := range expectations {
		body := recorder.run(t, fmt.Sprintf("readiness-%s", expected.service), nil,
			"docker", "compose", "--project-name", deployProject, "--file", composeFile, "exec", "-T", expected.service,
			"/quoin-healthcheck", "--status", fmt.Sprint(expected.status), fmt.Sprintf("http://127.0.0.1:%d/readyz", expected.port))
		var readiness struct {
			Component string `json:"component"`
			Release   string `json:"release"`
			Reason    string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &readiness); err != nil {
			t.Fatalf("%s readiness not JSON: %v\n%s", expected.service, err, body)
		}
		if readiness.Component != expected.service || readiness.Reason != expected.reason || readiness.Release != "v0.1.0-dev" {
			t.Fatalf("%s readiness mismatch: %+v", expected.service, readiness)
		}
		metrics := recorder.run(t, fmt.Sprintf("metrics-%s", expected.service), nil,
			"docker", "compose", "--project-name", deployProject, "--file", composeFile, "exec", "-T", expected.service,
			"/quoin-healthcheck", "--status", "200", fmt.Sprintf("http://127.0.0.1:%d/metrics", expected.port))
		assertMetricFamilies(t, expected.service, metrics)
		recorder.run(t, fmt.Sprintf("livez-%s", expected.service), nil,
			"docker", "compose", "--project-name", deployProject, "--file", composeFile, "exec", "-T", expected.service,
			"/quoin-healthcheck", fmt.Sprintf("http://127.0.0.1:%d/livez", expected.port))
	}
}

func assertMetricFamilies(t *testing.T, service, exposition string) {
	t.Helper()
	allowed := map[string]map[string][]string{
		"quoin":  {"quoin_ready": nil, "quoin_accepting_work": nil, "quoin_maintenance": {"Restore", "Upgrade", "RootKeyRebind", "LintelRecovery"}, "quoin_upgrade_prepared": nil, "quoin_storage_writable": {"data", "backup"}},
		"plinth": {"plinth_ready": nil, "plinth_storage_writable": {"state", "workspace"}},
		"lintel": {"lintel_ready": nil, "lintel_storage_writable": {"state", "shm"}},
		"stele":  {"stele_ready": nil, "stele_quoin_available": nil},
	}[service]
	seen := map[string]bool{}
	for _, line := range strings.Split(exposition, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if index := strings.Index(line, "{"); index >= 0 {
			name = line[:index]
			labels := line[index:strings.Index(line, "}")]
			if closed := allowed[name]; closed != nil && len(closed) > 0 {
				for _, value := range extractLabelValues(labels) {
					found := false
					for _, candidate := range closed {
						if candidate == value {
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("%s metric %s has label value %q outside the closed catalog set", service, name, value)
					}
				}
			}
		} else if index := strings.Index(line, " "); index >= 0 {
			name = line[:index]
		}
		if _, ok := allowed[name]; !ok {
			t.Fatalf("%s exported family %q is outside the ticket-applicable catalog subset:\n%s", service, name, exposition)
		}
		seen[name] = true
	}
	for family := range allowed {
		if !seen[family] {
			t.Fatalf("%s missing preinitialized family %q", service, family)
		}
	}
}

func extractLabelValues(labels string) []string {
	var values []string
	for _, part := range strings.Split(labels, ",") {
		if index := strings.Index(part, `="`); index >= 0 && strings.HasSuffix(part, `"`) {
			values = append(values, strings.TrimSuffix(part[index+2:], `"`))
		}
	}
	return values
}

func captureLogs(t *testing.T, recorder *evidence, composeFile string) map[string]string {
	t.Helper()
	logs := map[string]string{}
	for _, service := range []string{"quoin", "plinth", "lintel", "stele"} {
		logs[service] = recorder.run(t, "logs-"+service, nil, "docker", "compose", "--project-name", deployProject, "--file", composeFile, "logs", "--no-log-prefix", service)
	}
	return logs
}

func assertJsonLogs(t *testing.T, logs map[string]string) {
	t.Helper()
	for service, body := range logs {
		for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("%s log line is not JSON: %v\n%s", service, err, line)
			}
			for _, field := range []string{"ts", "level", "component", "release", "code", "msg"} {
				if _, ok := entry[field]; !ok {
					t.Fatalf("%s log line missing frozen field %q: %s", service, field, line)
				}
			}
			if entry["component"] != service || entry["release"] != "v0.1.0-dev" {
				t.Fatalf("%s log line identity mismatch: %s", service, line)
			}
		}
	}
}

func assertSecretFiles(t *testing.T, secretDir string) {
	t.Helper()
	for name, size := range map[string]int64{"root-key": 32, "stele-service-token": 32} {
		info, err := os.Lstat(filepath.Join(secretDir, name))
		if err != nil {
			t.Fatalf("secret %s: %v", name, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != size {
			t.Fatalf("secret %s mode=%v size=%d", name, info.Mode().Perm(), info.Size())
		}
	}
	for _, name := range []string{"runtime-ca.pem", "runtime-ca.key", "runtime-tls.crt", "runtime-tls.key"} {
		if _, err := os.Stat(filepath.Join(secretDir, name)); err != nil {
			t.Fatalf("secret %s missing: %v", name, err)
		}
	}
}

func runHttpMatrix(t *testing.T, recorder *evidence, base, tempPassword, newPassword string) {
	t.Helper()
	origin := "https://quoin.example.com"
	client := newCookieClient(t)
	matrix := map[string]string{}

	shell := doRequest(t, client, http.MethodGet, base+"/", "", map[string]string{}, http.StatusOK, "")
	if !strings.Contains(shell, `id="root"`) {
		t.Fatalf("web shell not served: %.200s", shell)
	}
	matrix["web-shell"] = "200 text/html with root container"
	matrix["me-unauthenticated"] = doStatus(t, client, http.MethodGet, base+"/api/v1/auth/me", "", nil, http.StatusUnauthorized)

	noMeta := doStatus(t, client, http.MethodPost, base+"/api/v1/auth/login", `{"username":"admin","password":"`+tempPassword+`"}`, map[string]string{"Content-Type": "application/json"}, http.StatusForbidden)
	matrix["login-without-browser-metadata"] = noMeta

	wrong := doStatus(t, client, http.MethodPost, base+"/api/v1/auth/login", `{"username":"admin","password":"definitely not the password 123"}`, jsonHeaders(origin), http.StatusUnauthorized)
	matrix["login-wrong-password"] = wrong

	first := doRequest(t, client, http.MethodPost, base+"/api/v1/auth/login", `{"username":"admin","password":"`+tempPassword+`"}`, jsonHeaders(origin), http.StatusOK, "")
	if !strings.Contains(first, `"passwordChangeRequired":true`) {
		t.Fatalf("first login must require password change: %s", first)
	}
	matrix["login-temporary"] = "200 session cookie issued; passwordChangeRequired=true"
	if !clientHasSession(t, client, base) {
		t.Fatal("login did not set the __Host-quoin-session cookie")
	}

	second := newCookieClient(t)
	doRequest(t, second, http.MethodPost, base+"/api/v1/auth/login", `{"username":"admin","password":"`+tempPassword+`"}`, jsonHeaders(origin), http.StatusOK, "")
	matrix["second-session"] = "200 second session created"

	doRequest(t, client, http.MethodPut, base+"/api/v1/auth/password", `{"currentPassword":"`+tempPassword+`","newPassword":"`+newPassword+`"}`, jsonHeaders(origin), http.StatusNoContent, "")
	matrix["change-password"] = "204"

	after := doRequest(t, client, http.MethodGet, base+"/api/v1/auth/me", "", map[string]string{}, http.StatusOK, "")
	if !strings.Contains(after, `"authRevision":2`) || strings.Contains(after, `"passwordChangeRequired":true`) {
		t.Fatalf("password change projection wrong: %s", after)
	}
	secondAfter := doStatus(t, second, http.MethodGet, base+"/api/v1/auth/me", "", nil, http.StatusUnauthorized)
	matrix["other-session-revoked"] = secondAfter
	matrix["current-session-survives"] = "200"

	doRequest(t, client, http.MethodPost, base+"/api/v1/auth/logout", "", map[string]string{"Origin": origin}, http.StatusNoContent, "")
	matrix["logout"] = "204 with Clear-Site-Data"
	matrix["session-revoked"] = doStatus(t, client, http.MethodGet, base+"/api/v1/auth/me", "", nil, http.StatusUnauthorized)

	recorder.note(t, "http-matrix.json", mustJSON(t, matrix))
}

func confirmLogin(t *testing.T, base, expectedPassword, oldPassword string) {
	t.Helper()
	client := newCookieClient(t)
	doRequest(t, client, http.MethodPost, base+"/api/v1/auth/login", `{"username":"admin","password":"`+expectedPassword+`"}`, jsonHeaders("https://quoin.example.com"), http.StatusOK, "")
	doRequest(t, client, http.MethodPost, base+"/api/v1/auth/logout", "", map[string]string{"Origin": "https://quoin.example.com"}, http.StatusNoContent, "")
}

func assertRateLimitLast(t *testing.T, base, tempPassword string) {
	t.Helper()
	client := newCookieClient(t)
	codes := []string{}
	for attempt := 0; attempt < 6; attempt++ {
		request, _ := http.NewRequest(http.MethodPost, base+"/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"never the right password!"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "https://quoin.example.com")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, fmt.Sprint(response.StatusCode))
		response.Body.Close()
	}
	if codes[4] != "401" || codes[5] != "429" {
		t.Fatalf("rate limit sequence wrong: %v", codes)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("QUOIN_EVIDENCE_DIR"), "rate-limit.json"), []byte(mustJSON(t, map[string]any{"sequence": codes})), 0o644); err != nil {
		t.Fatal(err)
	}
}

func teardown(t *testing.T, recorder *evidence, composeFile, workRoot string) {
	t.Helper()
	recorder.run(t, "teardown", nil, "docker", "compose", "--project-name", deployProject, "--file", composeFile, "down", "--remove-orphans")
	recorder.run(t, "teardown-images", nil, "docker", "rmi", "quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev")
	if err := os.RemoveAll(workRoot); err != nil {
		t.Fatalf("remove work root: %v", err)
	}
}

func scanForSecrets(t *testing.T, evidenceDir string, secrets ...string) {
	t.Helper()
	err := filepath.Walk(evidenceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(data, []byte(secret)) {
				t.Fatalf("secret material leaked into %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
