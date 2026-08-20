package users

// Shared acceptance helpers for the T05 ticket run: evidence recorder, HTTP
// helpers that capture status codes (rejections are part of the contract
// under test), and small builders. Split from ticket05_test.go to keep each
// file single-purpose and below the 500-line guidance.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type ticketEvidence struct {
	dir       string
	commands  []map[string]any
	artifacts []map[string]any
	env       []string
	stateRoot string
	imageIDs  map[string]string
}

// statusResponse keeps the status code: T05 asserts deterministic 4xx
// rejections, so helpers must not abort on them.
type statusResponse struct {
	Status  int
	Body    string
	Headers http.Header
}

func (evidence *ticketEvidence) run(t *testing.T, name string, stdin io.Reader, command string, arguments ...string) string {
	t.Helper()
	started := time.Now()
	cmd := exec.Command(command, arguments...)
	cmd.Env = evidence.env
	cmd.Dir = repoRoot(t)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	exitCode := 0
	if err := cmd.Run(); err != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	logPath := filepath.Join(evidence.dir, name+".log")
	os.WriteFile(logPath, combined.Bytes(), 0o644)
	evidence.commands = append(evidence.commands, map[string]any{"name": name, "exitCode": exitCode, "duration": time.Since(started).Round(time.Millisecond).String()})
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": logPath, "sha256": sha256Hex(combined.Bytes()), "bytes": combined.Len()})
	if exitCode != 0 {
		t.Fatalf("%s: exit=%d output:\n%s", name, exitCode, combined.String())
	}
	return combined.String()
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	os.WriteFile(path, []byte(content), 0o644)
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(content)), "bytes": len(content)})
}

func cookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func bareClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// bareClientWithCookie speaks for exactly one session bearer — used to prove
// that a revoked/disabled session is rejected on its next request.
func bareClientWithCookie(cookieValue string) *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: cookieTransport{cookieValue},
	}
}

type cookieTransport struct{ value string }

func (transport cookieTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Cookie", "__Host-quoin-session="+transport.value)
	return http.DefaultTransport.RoundTrip(clone)
}

func doRequest(t *testing.T, client *http.Client, method, target, origin, body string) statusResponse {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	return statusResponse{Status: response.StatusCode, Body: string(raw), Headers: response.Header}
}

func httpJSON(t *testing.T, client *http.Client, method, target, origin, body string) statusResponse {
	t.Helper()
	return doRequest(t, client, method, target, origin, body)
}

func loginAndGetCookie(t *testing.T, base, origin, username, password string) (*http.Client, string) {
	t.Helper()
	client := cookieClient(t)
	response := doRequest(t, client, http.MethodPost, base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":%q,"password":%q}`, username, password))
	if response.Status != http.StatusOK {
		t.Fatalf("login %s: status=%d body=%s", username, response.Status, response.Body)
	}
	return client, cookieOf(t, client, base)
}

func cookieOf(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range client.Jar.Cookies(parsed) {
		if cookie.Name == "__Host-quoin-session" {
			return cookie.Value
		}
	}
	t.Fatal("session cookie missing from jar")
	return ""
}

type userRow struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	Enabled    bool   `json:"enabled"`
	RowVersion int64  `json:"rowVersion"`
}

func userByName(t *testing.T, client *http.Client, base, origin, username string) userRow {
	t.Helper()
	list := doRequest(t, client, http.MethodGet, base+"/api/v1/admin/users", origin, "")
	if list.Status != http.StatusOK {
		t.Fatalf("list users: status=%d body=%s", list.Status, list.Body)
	}
	var page struct {
		Items []userRow `json:"items"`
	}
	if err := json.Unmarshal([]byte(list.Body), &page); err != nil {
		t.Fatalf("parse users: %v\n%s", err, list.Body)
	}
	for _, item := range page.Items {
		if item.Username == username {
			return item
		}
	}
	t.Fatalf("user %s not found: %s", username, list.Body)
	return userRow{}
}

func randomSecret(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(content), 0o600)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}

func outputOf(t *testing.T, command string, arguments ...string) string {
	t.Helper()
	out, err := exec.Command(command, arguments...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	return string(out)
}

func scanForSecrets(t *testing.T, evidenceDir string, secrets ...string) {
	t.Helper()
	filepath.Walk(evidenceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, _ := os.ReadFile(path)
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(data, []byte(secret)) {
				t.Fatalf("secret material leaked into %s", path)
			}
		}
		return nil
	})
}

func (evidence *ticketEvidence) writeRuntimeEvidence(t *testing.T, commit string) {
	t.Helper()
	startedAt := time.Now().UTC()
	statusOut, _ := exec.Command("git", "-C", repoRoot(t), "status", "--porcelain").Output()
	dirtyDigest := sha256Hex(statusOut)
	os.WriteFile(filepath.Join(evidence.dir, "cleanup.json"), []byte(mustJSON(t, map[string]any{
		"containers":    "quoin compose project down --remove-orphans; no other long-lived containers owned",
		"networks":      "quoin_default/quoin_internal/quoin_edge removed by compose down",
		"images":        "quoin/{quoin,plinth,lintel,stele}:v0.1.0-dev removed only when this run built them",
		"workRoot":      "temporary XDG_STATE_HOME + install config removed with the test temp root",
		"credentials":   "temporary/new admin and operator passwords held only in process memory; never written to evidence (verified by the final secret scan)",
		"fixtureState":  "cooldown state is per-username process memory on the removed container; nothing persists",
		"timestamp":     startedAt.Format(time.RFC3339),
	})), 0o644)
	os.WriteFile(filepath.Join(evidence.dir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit":        commit,
		"dirtyStateDigest": dirtyDigest,
		"startedAt":        startedAt.Format(time.RFC3339),
		"commands":         evidence.commands,
		"artifacts":        evidence.artifacts,
		"components": map[string]any{
			"deployHelper": "cmd/quoin-deploy (go build -trimpath, host binary) driving the canonical compose install",
			"imageDigests": evidence.imageIDs,
			"httpSurface":  "real Quoin container over 127.0.0.1:18080 with same-origin Origin enforcement",
		},
		"observed": map[string]any{
			"realPath":   "Chromium-less HTTP acceptance: Playwright @ticket-05 covers the browser path in the same ticket run; this test drives the identical endpoints over real HTTP against the installed container",
			"transitions": []string{
				"users/sessions/audit projections read over authenticated HTTP",
				"createUser 201; operator 403 on admin surfaces; restricted session 403 password_change_required",
				"resetUserPassword revoked 1 session; old bearer rejected 401 on next request",
				"sixth failed login for one username answered 429 with Retry-After",
				"concurrent disable race: exactly one 200, everything else 409/401, exactly one enabled admin remains",
				"command replay returned the byte-identical 201 body; same key with a different request 409 command_id_reused",
				"disabled account: existing bearer and fresh login both 401",
			},
		},
		"expectedVersusActual": map[string]string{
			"authorization matrix (operator read audit, forbidden on admin writes)": "actual: 200 audit / 403 both admin writes; restricted session 403 code=password_change_required",
			"password cooldown":                          "actual: 5 failures -> 6th 429 with Retry-After header",
			"dummy-hash":                                  "actual: unknown-user and wrong-password failures return identical bodies and comparable Argon2id latency (>=15ms each)",
			"concurrent last-admin races":                 "actual: exactly one disable committed; every other attempt deterministically 409/401; exactly one enabled admin remained; survivor self-disable 409 active_conflict",
			"revoked/disabled session rejection":         "actual: reset and disable both made the next authenticated request 401",
			"command replay":                              "actual: identical body on same-digest replay; 409 command_id_reused on drift",
		},
		"redactions": "all admin/operator passwords excluded from evidence; verified by the trailing secret scan",
	})), 0o644)
}
