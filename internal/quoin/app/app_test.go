package app_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// TestAuthEndpointsOverRealServer drives the real Huma surface end to end,
// including the PUT password path, over an in-process server with real SQLite.
func TestAuthEndpointsOverRealServer(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.example.com",
		DataDirectory:             filepath.Join(root, "data"),
		BackupDirectory:           filepath.Join(root, "backup"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		SteleServiceTokenFile:     filepath.Join(secrets, "stele-service-token"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	const temporary = "Correct horse battery staple 2026!"
	if _, err := service.CreateFirstAdmin(ctx, "admin", "Quoin Admin", temporary); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(mustHandler(t, service, database.SQL, config.PublicOrigin, config.RootKeyFile))
	defer server.Close()

	origin := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"}

	login := mustPost(t, server, origin, `/api/v1/auth/login`, `{"username":"admin","password":"Correct horse battery staple 2026!"}`, http.StatusOK)
	if strings.Contains(login.body, "$schema") {
		t.Fatalf("response body must match the frozen OpenAPI schema, got %s", login.body)
	}
	cookie := login.headers.Get("Set-Cookie")
	if !strings.HasPrefix(cookie, "__Host-quoin-session=") {
		t.Fatalf("expected session cookie, got %q", cookie)
	}
	var session struct {
		PasswordChangeRequired bool `json:"passwordChangeRequired"`
		AuthRevision           int  `json:"authRevision"`
	}
	if err := json.Unmarshal([]byte(login.body), &session); err != nil {
		t.Fatal(err)
	}
	if !session.PasswordChangeRequired {
		t.Fatal("first login must report passwordChangeRequired")
	}

	// Huma validates request structure before a handler executes. It must still
	// serialize the project-wide frozen ErrorModel and must not echo submitted
	// values (which may be secrets on other endpoints).
	invalid := mustPost(t, server, origin, `/api/v1/auth/login`, `{"username":"admin","password":"wrong","unexpected":true}`, http.StatusUnprocessableEntity)
	if contentType := invalid.headers.Get("Content-Type"); !strings.HasPrefix(contentType, "application/problem+json") {
		t.Fatalf("framework validation content type=%q, want application/problem+json", contentType)
	}
	assertFrozenProblem(t, invalid, "validation_failed")

	malformed := mustPost(t, server, origin, `/api/v1/auth/login`, `{"username":`, http.StatusBadRequest)
	assertFrozenProblem(t, malformed, "malformed_request")

	unsupportedMedia := mustPost(t, server, merge(origin, map[string]string{"Content-Type": "text/plain"}), `/api/v1/auth/login`, `{"username":"admin","password":"irrelevant"}`, http.StatusUnsupportedMediaType)
	assertFrozenProblem(t, unsupportedMedia, "unsupported_media")

	me := mustRequest(t, server, map[string]string{"Cookie": splitCookie(cookie)}, `/api/v1/auth/me`, http.StatusOK)
	t.Logf("me: %s", me)

	change := mustDo(t, server, http.MethodPut, merge(origin, map[string]string{"Cookie": splitCookie(cookie)}),
		`/api/v1/auth/password`, `{"currentPassword":"Correct horse battery staple 2026!","newPassword":"A better personal passphrase 2027!"}`, http.StatusNoContent)
	t.Logf("change headers: %v", change.headers)

	after := mustRequest(t, server, map[string]string{"Cookie": splitCookie(cookie)}, `/api/v1/auth/me`, http.StatusOK)
	var updated struct {
		PasswordChangeRequired bool `json:"passwordChangeRequired"`
		AuthRevision           int  `json:"authRevision"`
	}
	if err := json.Unmarshal([]byte(after), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.PasswordChangeRequired || updated.AuthRevision != 2 {
		t.Fatalf("password change did not take effect: %s", after)
	}
}

func assertFrozenProblem(t *testing.T, result httpResult, wantCode string) {
	t.Helper()
	var body struct {
		Code        string `json:"code"`
		Message     string `json:"message"`
		Retryable   *bool  `json:"retryable"`
		FieldErrors []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"fieldErrors"`
	}
	if err := json.Unmarshal([]byte(result.body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != wantCode || body.Message == "" || body.Retryable == nil || len(body.FieldErrors) == 0 || strings.Contains(result.body, `"value"`) {
		t.Fatalf("framework validation must use frozen redacted ErrorModel (want code %q): %s", wantCode, result.body)
	}
}

func splitCookie(setCookie string) string {
	return strings.Split(setCookie, ";")[0]
}

func mustHandler(t *testing.T, service *auth.Service, db *sql.DB, origin string, rootKeyFile string) http.Handler {
	t.Helper()
	handler, err := app.NewHandler(app.NewAPIServer(service, db, rootKeyFile), origin)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

type httpResult struct {
	body    string
	headers http.Header
}

func mustPost(t *testing.T, server *httptest.Server, headers map[string]string, path, body string, want int) httpResult {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return perform(t, server.Client(), request, want)
}

func mustDo(t *testing.T, server *httptest.Server, method string, headers map[string]string, path, body string, want int) httpResult {
	t.Helper()
	request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return perform(t, server.Client(), request, want)
}

func mustRequest(t *testing.T, server *httptest.Server, headers map[string]string, path string, want int) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return perform(t, server.Client(), request, want).body
}

func perform(t *testing.T, client *http.Client, request *http.Request, want int) httpResult {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw := &strings.Builder{}
	_, _ = io.Copy(raw, response.Body)
	if response.StatusCode != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", request.Method, request.URL.Path, response.StatusCode, want, raw.String())
	}
	return httpResult{body: raw.String(), headers: response.Header}
}

func merge(base, extra map[string]string) map[string]string {
	joined := map[string]string{}
	for key, value := range base {
		joined[key] = value
	}
	for key, value := range extra {
		joined[key] = value
	}
	return joined
}

// TestAlertSourceRevealLifecycleOverRealServer drives the frozen reveal
// lifecycle over the real Huma surface: create → replay (same handle) →
// reveal once → second reveal 410 → replay after consume reports
// revealAvailable=false; Operator role is forbidden from both commands.
func TestAlertSourceRevealLifecycleOverRealServer(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.example.com",
		DataDirectory:             filepath.Join(root, "data"),
		BackupDirectory:           filepath.Join(root, "backup"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		SteleServiceTokenFile:     filepath.Join(secrets, "stele-service-token"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	const temporary = "Correct horse battery staple 2026!"
	if _, err := service.CreateFirstAdmin(ctx, "admin", "Quoin Admin", temporary); err != nil {
		t.Fatal(err)
	}
	// A second non-admin user (Operator) exercises the 403 paths; user
	// management lands in a later ticket, so the operator row is inserted
	// directly with the real Argon2id hash.
	operatorPHC, err := auth.HashPassword("Operator passphrase 2026!")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.ExecContext(ctx, `INSERT INTO users(username,display_name,role,enabled,password_phc,password_change_required,created_at,updated_at) VALUES(?,'Ops Operator','operator',1,?,0,?,?)`,
		"operator", operatorPHC, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(mustHandler(t, service, database.SQL, config.PublicOrigin, config.RootKeyFile))
	defer server.Close()
	origin := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"}

	adminCookie := loginCookie(t, server, origin, "admin", temporary, true)
	operatorCookie := loginCookie(t, server, origin, "operator", "Operator passphrase 2026!", false)

	create := mustPost(t, server, merge(origin, map[string]string{"Cookie": adminCookie}),
		`/api/v1/alert-sources`, `{"key":"prod-am","protocol":"alertmanager","clientCommandId":"cmd-0001"}`, http.StatusCreated)
	var created struct {
		SourceKey       string `json:"sourceKey"`
		CredentialID    string `json:"credentialId"`
		RevealAvailable bool   `json:"revealAvailable"`
		RevealHandle    string `json:"revealHandle"`
	}
	if err := json.Unmarshal([]byte(create.body), &created); err != nil {
		t.Fatal(err)
	}
	if !created.RevealAvailable || created.RevealHandle == "" || created.SourceKey != "prod-am" {
		t.Fatalf("create response malformed: %s", create.body)
	}

	// Replay with the same clientCommandId returns the original source and the
	// same still-valid handle (HTTP-COMMAND-003 / SEC-REVEAL-003).
	replay := mustPost(t, server, merge(origin, map[string]string{"Cookie": adminCookie}),
		`/api/v1/alert-sources`, `{"key":"prod-am","protocol":"alertmanager","clientCommandId":"cmd-0001"}`, http.StatusCreated)
	var replayed struct {
		SourceKey       string `json:"sourceKey"`
		RevealHandle    string `json:"revealHandle"`
		RevealAvailable bool   `json:"revealAvailable"`
	}
	if err := json.Unmarshal([]byte(replay.body), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.SourceKey != "prod-am" || replayed.RevealHandle != created.RevealHandle || !replayed.RevealAvailable {
		t.Fatalf("replay must return the original handle: %s", replay.body)
	}

	// Replaying with a DIFFERENT payload under the same command id conflicts.
	mustPost(t, server, merge(origin, map[string]string{"Cookie": adminCookie}),
		`/api/v1/alert-sources`, `{"key":"other-am","protocol":"alertmanager","clientCommandId":"cmd-0001"}`, http.StatusConflict)

	// Operator is forbidden from both commands.
	mustPost(t, server, merge(origin, map[string]string{"Cookie": operatorCookie}),
		`/api/v1/alert-sources`, `{"key":"ops-am","protocol":"alertmanager","clientCommandId":"cmd-0002"}`, http.StatusForbidden)
	mustPost(t, server, merge(origin, map[string]string{"Cookie": operatorCookie}),
		`/api/v1/alert-sources/credentials/reveal`, `{"revealHandle":"`+created.RevealHandle+`"}`, http.StatusForbidden)

	// Reveal succeeds exactly once; the second consume of the same handle is 410.
	reveal := mustPost(t, server, merge(origin, map[string]string{"Cookie": adminCookie}),
		`/api/v1/alert-sources/credentials/reveal`, `{"revealHandle":"`+created.RevealHandle+`"}`, http.StatusOK)
	var revealed struct {
		CredentialID string `json:"credentialId"`
		BearerToken  string `json:"bearerToken"`
	}
	if err := json.Unmarshal([]byte(reveal.body), &revealed); err != nil {
		t.Fatal(err)
	}
	if len(revealed.BearerToken) != 43 {
		t.Fatalf("bearer shape wrong: %q", revealed.BearerToken)
	}
	mustPost(t, server, merge(origin, map[string]string{"Cookie": adminCookie}),
		`/api/v1/alert-sources/credentials/reveal`, `{"revealHandle":"`+created.RevealHandle+`"}`, http.StatusGone)

	// After consume, a replay of the same command reports revealAvailable=false
	// and never re-creates the credential (SEC-REVEAL-*).
	after := mustPost(t, server, merge(origin, map[string]string{"Cookie": adminCookie}),
		`/api/v1/alert-sources`, `{"key":"prod-am","protocol":"alertmanager","clientCommandId":"cmd-0001"}`, http.StatusCreated)
	var afterConsume struct {
		RevealAvailable bool `json:"revealAvailable"`
	}
	if err := json.Unmarshal([]byte(after.body), &afterConsume); err != nil {
		t.Fatal(err)
	}
	if afterConsume.RevealAvailable {
		t.Fatalf("replay after consume must not offer a handle: %s", after.body)
	}
}

func loginCookie(t *testing.T, server *httptest.Server, origin map[string]string, username, password string, changePassword bool) string {
	t.Helper()
	login := mustPost(t, server, origin, `/api/v1/auth/login`,
		`{"username":"`+username+`","password":"`+password+`"}`, http.StatusOK)
	cookie := splitCookie(login.headers.Get("Set-Cookie"))
	if changePassword {
		mustDo(t, server, http.MethodPut, merge(origin, map[string]string{"Cookie": cookie}),
			`/api/v1/auth/password`,
			`{"currentPassword":"`+password+`","newPassword":"Fresh personal passphrase 2027!"}`, http.StatusNoContent)
	}
	return cookie
}
