package suites

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Stack drives one real Compose deployment of the release subject
// through the deployment helper itself: install runs as a subprocess of
// the running helper binary (os.Executable), runtime registration uses
// the one-time attached-stdin command over real gRPC, and product
// observations go through the public HTTP surface. The Kubernetes
// backend implements the same contract through kubectl; nothing in the
// suite legs may depend on the backend kind.
type Stack struct {
	Project        string
	WorkRoot       string
	ConfigPath     string
	ManifestPath   string
	AdminPassword  string
	QuoinPort      int
	StelePort      int
	Stdout, Stderr io.Writer

	// SharesInvocationCredential marks the invocation's primary
	// deployment: its first-login password change publishes the formal
	// credential for the dependent suite processes. Disposable clones
	// keep their own bootstrap credential.
	SharesInvocationCredential bool

	composeFile string
}

// ComposeEnv is the isolated per-invocation environment
// (VERIFY-MATRIX-004: unique project and state per invocation).
func (stack *Stack) ComposeEnv() []string {
	return append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(stack.WorkRoot, stack.Project, "state"),
		"QUOIN_COMPOSE_PROJECT="+stack.Project,
		"QUOIN_DEPLOY_SCRIPTED=1",
		"DOCKER_CLI_HINTS=false",
	)
}

// BaseURL is the public Quoin origin base. A qualification cell
// running containerized reaches the host-published loopback through
// QUOIN_LOOPBACK_HOST.
func (stack *Stack) BaseURL() string {
	host := "127.0.0.1"
	if fromEnv := os.Getenv("QUOIN_LOOPBACK_HOST"); fromEnv != "" {
		host = fromEnv
	}
	return "http://" + host + ":" + strconv.Itoa(stack.QuoinPort)
}

// EnsureInstalled runs the helper's staged install (idempotent resume)
// with the admin bootstrap answers on stdin, then waits for the public
// listener. It returns the install report path.
func (stack *Stack) EnsureInstalled() (string, error) {
	helper, err := os.Executable()
	if err != nil {
		return "", err
	}
	report := filepath.Join(stack.WorkRoot, stack.Project, "install-report.json")
	install := exec.Command(helper, "compose", "install", "--config", stack.ConfigPath,
		"--release-manifest", stack.ManifestPath, "--report", report)
	install.Env = stack.ComposeEnv()
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

// awaitPublic polls the public listener until the product answers.
func (stack *Stack) awaitPublic(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodGet, stack.BaseURL()+"/api/v1/auth/session", nil)
		if err == nil {
			request.Header.Set("Origin", "https://quoin.example.com")
			if response, err := client.Do(request); err == nil {
				response.Body.Close()
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("public listener never answered on %s", stack.BaseURL())
}

// Exec runs a one-shot command inside one component container.
func (stack *Stack) Exec(component string, arguments ...string) (string, int, error) {
	full := append([]string{"compose", "--project-name", stack.Project, "--file", stack.composeFile,
		"exec", "-T", component}, arguments...)
	return stack.docker(full...)
}

// RunService runs a one-shot service container (the registration
// command's vehicle) with a stdin payload and returns its exit code.
func (stack *Stack) RunService(component string, arguments []string, stdinPayload string) (string, int, error) {
	full := append([]string{"compose", "--project-name", stack.Project, "--file", stack.composeFile,
		"run", "--rm", "--no-deps", "-i", "-T", component}, arguments...)
	command := exec.Command("docker", full...)
	command.Dir = workDirOf(stack.ConfigPath)
	command.Env = stack.ComposeEnv()
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

// Logs drains one component's recent logs.
func (stack *Stack) Logs(component string) (string, error) {
	output, _, err := stack.docker("compose", "--project-name", stack.Project, "--file", stack.composeFile,
		"logs", "--no-log-prefix", "--tail", "300", component)
	return output, err
}

func (stack *Stack) docker(arguments ...string) (string, int, error) {
	command := exec.Command("docker", arguments...)
	command.Env = stack.ComposeEnv()
	var combined bytes.Buffer
	command.Stdout, command.Stderr = &combined, &combined
	err := command.Run()
	code := 0
	if command.ProcessState != nil {
		code = command.ProcessState.ExitCode()
	}
	return combined.String(), code, err
}

// Down removes the deployment (volumes included when dataRemoval is
// set) and returns the combined output.
func (stack *Stack) Down(dataRemoval bool) (string, error) {
	arguments := []string{"compose", "--project-name", stack.Project, "down", "--remove-orphans", "--timeout", "45"}
	if dataRemoval {
		arguments = append(arguments, "-v")
	}
	output, _, err := stack.docker(arguments...)
	return output, err
}

// Session is one authenticated admin HTTP session against the public
// origin (the same-origin login round trip). The __Host- session
// cookie is Secure, so it is pinned here and attached per request
// instead of travelling through a jar over the plain-HTTP loopback.
type Session struct {
	Client *http.Client
	Base   string
	Origin string
	Cookie *http.Cookie
}

const publicOrigin = "https://quoin.example.com"

// Login performs the real admin login over the public port. The first
// login of a fresh deployment answers passwordChangeRequired; the
// change is completed here and the stack's password is advanced so
// later logins (and reinstalls that resume rather than re-bootstrap)
// use the formal credential.
func (stack *Stack) Login(username, password string) (*Session, error) {
	session, body, err := stack.loginOnce(username, password)
	if err != nil {
		return nil, err
	}
	if strings.Contains(body, "\"passwordChangeRequired\":true") {
		formal := password + "-formal"
		changeBody, status, _, err := session.Do(http.MethodPut, "/api/v1/auth/password",
			fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, password, formal))
		if err != nil {
			return nil, err
		}
		if status != http.StatusNoContent && status != http.StatusOK {
			return nil, fmt.Errorf("password change status=%d body=%.300s", status, changeBody)
		}
		stack.AdminPassword = formal
		// The formal credential outlives this helper process: the
		// dependent suites of the same invocation read it from the
		// shared work root (the environment still carries the
		// bootstrap password that created the deployment).
		if stack.SharesInvocationCredential {
			_ = os.WriteFile(stack.sharedCredentialPath(), []byte(formal), 0o600)
		}
		session, _, err = stack.loginOnce(username, formal)
		if err != nil {
			return nil, err
		}
	}
	return session, nil
}

// sharedCredentialPath is the invocation-shared formal credential
// pointer. It lives OUTSIDE the evidence tree (the sentinel scan must
// never see plaintext credentials) and is keyed by the deployment
// project so parallel invocations cannot cross-read.
func (stack *Stack) sharedCredentialPath() string {
	return filepath.Join(os.TempDir(), "quoin-suite-"+stack.Project+"-admin-password")
}

// sharedAdminPassword reads the invocation-shared formal credential
// when a prior suite completed the first-login password change.
func (stack *Stack) sharedAdminPassword() (string, bool) {
	if !stack.SharesInvocationCredential {
		return "", false
	}
	body, err := os.ReadFile(stack.sharedCredentialPath())
	if err != nil || len(body) == 0 {
		return "", false
	}
	return string(body), true
}

func (stack *Stack) loginOnce(username, password string) (*Session, string, error) {
	if shared, ok := stack.sharedAdminPassword(); ok {
		password = shared
	}
	session := &Session{Client: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Base: stack.BaseURL(), Origin: publicOrigin}
	request, err := http.NewRequest(http.MethodPost, session.Base+"/api/v1/auth/login",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", session.Origin)
	response, err := session.Client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return nil, string(payload), fmt.Errorf("login status=%d body=%s", response.StatusCode, payload)
	}
	// The frozen session cookie is __Host- prefixed and Secure, so a
	// jar would refuse to send it over the plain-HTTP loopback; pin it
	// through the transport instead (the T33 client's proven approach).
	for _, cookie := range response.Cookies() {
		if cookie.Name == "__Host-quoin-session" {
			session.Cookie = cookie
			// Verify the pinned cookie authenticates before returning.
			probe, status, _, probeErr := session.Do(http.MethodGet, "/api/v1/auth/me", "")
			if probeErr != nil || status != http.StatusOK {
				return nil, string(payload), fmt.Errorf("session probe status=%d err=%v body=%.200s", status, probeErr, probe)
			}
			return session, string(payload), nil
		}
	}
	return nil, "", fmt.Errorf("login returned no session cookie")
}

// Do issues one request with the session cookie and the frozen origin.
func (session *Session) Do(method, path, body string) (string, int, http.Header, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, session.Base+path, reader)
	if err != nil {
		return "", 0, nil, err
	}
	request.Header.Set("Origin", session.Origin)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if session.Cookie != nil {
		request.AddCookie(session.Cookie)
	}
	response, err := session.Client.Do(request)
	if err != nil {
		return "", 0, nil, err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	return string(payload), response.StatusCode, response.Header, nil
}

// Get is a convenience GET.
func (session *Session) Get(path string) (string, int, error) {
	body, status, _, err := session.Do(http.MethodGet, path, "")
	return body, status, err
}

// Post is a convenience POST expecting no particular status.
func (session *Session) Post(path, body string) (string, int, error) {
	payload, status, _, err := session.Do(http.MethodPost, path, body)
	return payload, status, err
}

// SSE opens the task event stream and returns a reader plus the
// response; the caller drives framing and cursor resume.
func (session *Session) SSE(path string, lastEventID string) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodGet, session.Base+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Origin", session.Origin)
	request.Header.Set("Accept", "text/event-stream")
	if session.Cookie != nil {
		request.AddCookie(session.Cookie)
	}
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	return session.Client.Do(request)
}

// RandomPassword generates a short-lived test credential
// (VERIFY-EXTERNAL-003: synthetic data and unique sentinels only).
func RandomPassword() string {
	buffer := make([]byte, 24)
	_, _ = rand.Read(buffer)
	return "t40-" + base64.RawURLEncoding.EncodeToString(buffer)
}

// RegistrationToken is the revealed one-time registration credential.
type RegistrationToken struct {
	Slot       string `json:"slot"`
	Generation int64  `json:"generation"`
	Token      string `json:"registrationToken"`
}

// PrepareAndReveal walks the public prepare/reveal endpoints for one
// runtime slot; expectedRowVersion is the slot view's current row.
func (session *Session) PrepareAndReveal(slot string, expectedRowVersion int64) (RegistrationToken, error) {
	prepareBody, status, err := session.Post("/api/v1/runtime-slots/"+slot+"/registration/prepare",
		fmt.Sprintf(`{"clientCommandId":"t40-%s-%d","expectedRowVersion":%d}`, slot, time.Now().UnixNano(), expectedRowVersion))
	if err != nil {
		return RegistrationToken{}, err
	}
	if status != http.StatusOK {
		return RegistrationToken{}, fmt.Errorf("prepare %s: %d %s", slot, status, prepareBody)
	}
	var preparation struct {
		RegistrationTokenAvailable bool   `json:"registrationTokenAvailable"`
		RegistrationTokenHandle    string `json:"registrationTokenHandle"`
	}
	if err := json.Unmarshal([]byte(prepareBody), &preparation); err != nil {
		return RegistrationToken{}, err
	}
	if !preparation.RegistrationTokenAvailable {
		return RegistrationToken{}, fmt.Errorf("prepare did not expose a reveal handle: %s", prepareBody)
	}
	revealBody, status, err := session.Post("/api/v1/runtime-slots/registration-token/reveal",
		fmt.Sprintf(`{"registrationTokenHandle":%q}`, preparation.RegistrationTokenHandle))
	if err != nil {
		return RegistrationToken{}, err
	}
	if status != http.StatusOK {
		return RegistrationToken{}, fmt.Errorf("reveal %s: %d %s", slot, status, revealBody)
	}
	var token RegistrationToken
	if err := json.Unmarshal([]byte(revealBody), &token); err != nil {
		return RegistrationToken{}, err
	}
	return token, nil
}

// SlotView reads the runtime status view of one slot.
func (session *Session) SlotView(slot string) (map[string]any, error) {
	body, status, err := session.Get("/api/v1/runtime")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("runtime status: %d %s", status, body)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		return nil, err
	}
	view, _ := document[slot].(map[string]any)
	if view == nil {
		return nil, fmt.Errorf("slot %s missing from runtime status: %s", slot, body)
	}
	return view, nil
}

// WaitForSlot polls until the predicate holds on the slot view.
func (session *Session) WaitForSlot(slot string, predicate func(map[string]any) bool, what string, timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		view, err := session.SlotView(slot)
		if err == nil {
			last = view
			if predicate(view) {
				return view, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("%s never became true (last=%v)", what, last)
}

// RegisterRuntime performs the one-time attached-stdin registration
// command inside the component container: the real gRPC transport path.
func (stack *Stack) RegisterRuntime(component string, token RegistrationToken) (string, int, error) {
	payload, _ := json.Marshal(map[string]any{"slot": token.Slot, "generation": token.Generation, "token": token.Token})
	return stack.RunService(component, []string{"register", "--config", "/etc/quoin/component.yaml"}, string(payload))
}

// workDirOf resolves the repository root for helper subprocesses: the
// suite always runs from the repository (the catalog phase contract
// executes with cwd = repo root).
func workDirOf(path string) string {
	if root := os.Getenv("QUOIN_REPO_ROOT"); root != "" {
		return root
	}
	return filepath.Dir(path)
}

// steleWebhookURL is the Stele webhook listener's public address.
func (stack *Stack) steleWebhookURL() string {
	host := "127.0.0.1"
	if fromEnv := os.Getenv("QUOIN_LOOPBACK_HOST"); fromEnv != "" {
		host = fromEnv
	}
	return fmt.Sprintf("http://%s:%d/", host, stack.StelePort)
}
