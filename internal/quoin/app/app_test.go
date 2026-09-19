package app_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// TestPublicHandlerOnlyServesBackendRoutes guards the split deployment boundary:
// the frontend service owns pages and SPA fallback, while Quoin only serves its
// API, streaming, and WebSocket routes. This ensures an absent frontend image
// can never turn an unknown backend endpoint into an apparently successful page.
func TestPublicHandlerOnlyServesBackendRoutes(t *testing.T) {
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
		RuntimeClientCAFile:       filepath.Join(secrets, "stele-service-token"),
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
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(mustHandler(t, service, database.SQL, database.Reader, config.PublicOrigin, config.RootKeyFile))
	defer server.Close()
	for _, endpoint := range []string{"/", "/investigations/example", "/api/v1/not-a-route"} {
		response, err := server.Client().Get(server.URL + endpoint)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("uninitialized GET %s: status=%d, want %d", endpoint, response.StatusCode, http.StatusServiceUnavailable)
		}
		if response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: backend security headers missing: %v", endpoint, response.Header)
		}
	}
}

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
		RuntimeClientCAFile:       filepath.Join(secrets, "stele-service-token"),
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
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	if err := service.ConfigureAuth(auth.AuthConfig{OTPKey: bytes.Repeat([]byte{0x7E}, 32), Sender: sender}); err != nil {
		t.Fatal(err)
	}
	// The deployment seeds only a pending bootstrap administrator whose initial
	// password is the public default; the unified initialization wizard opens
	// directly with it (the deployment network owns the access boundary).
	if _, err := service.EnsureBootstrapAdmin(ctx); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(mustHandler(t, service, database.SQL, database.Reader, config.PublicOrigin, config.RootKeyFile))
	defer server.Close()

	origin := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"}

	login := mustPost(t, server, origin, `/api/v1/auth/login`, `{"username":"admin","password":"admin"}`, http.StatusOK)
	if strings.Contains(login.body, "$schema") {
		t.Fatalf("response body must match the frozen OpenAPI schema, got %s", login.body)
	}
	if cookie := login.headers.Get("Set-Cookie"); !strings.HasPrefix(cookie, "__Host-quoin-flow=") {
		t.Fatalf("expected flow cookie, got %q", cookie)
	}
	var flow struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(login.body), &flow); err != nil {
		t.Fatal(err)
	}
	if flow.Type != "admin_initialize" {
		t.Fatalf("expected the admin initialization flow, got %s", login.body)
	}
	flowCookie := scenarioCookie(login.headers, "__Host-quoin-flow")
	flowOrigin := merge(origin, map[string]string{"Cookie": flowCookie})

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

	// The wizard: formal password, then a verified OTP contact. The forced
	// first-password change of the old direct login now lives in this flow's
	// password step.
	password := "Correct horse battery staple 2026!"
	mustDo(t, server, http.MethodPut, flowOrigin, `/api/v1/auth/flow/password`, `{"newPassword":"`+password+`"}`, http.StatusNoContent)
	contact := mustPost(t, server, flowOrigin, `/api/v1/auth/flow/contacts`, `{"channel":"email","target":"admin@example.test"}`, http.StatusOK)
	var masked struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(contact.body), &masked); err != nil {
		t.Fatal(err)
	}
	mustPost(t, server, flowOrigin, `/api/v1/auth/flow/challenge`, `{"contactId":"`+masked.ID+`"}`, http.StatusOK)
	initialVerify := mustPost(t, server, flowOrigin, `/api/v1/auth/flow/verify`, `{"code":"`+sender.lastCode()+`"}`, http.StatusOK)
	var verification struct {
		Completed bool `json:"completed"`
	}
	if err := json.Unmarshal([]byte(initialVerify.body), &verification); err != nil {
		t.Fatal(err)
	}
	if verification.Completed {
		t.Fatalf("initialization verification must not create a session: %s", initialVerify.body)
	}
	mustPost(t, server, flowOrigin, `/api/v1/auth/flow/complete`, `{}`, http.StatusNoContent)
	// A flow bearer can never authenticate: the flow cookie answers 401 on me.
	mustRequest(t, server, map[string]string{"Cookie": flowCookie}, `/api/v1/auth/me`, http.StatusUnauthorized)

	// Second-factor login: the password starts a login flow, the consumed OTP
	// code issues the session cookie.
	flowLogin := mustPost(t, server, origin, `/api/v1/auth/login`, `{"username":"admin","password":"`+password+`"}`, http.StatusOK)
	var secondFactor struct {
		Type     string `json:"type"`
		Contacts []struct {
			ID string `json:"id"`
		} `json:"contacts"`
	}
	if err := json.Unmarshal([]byte(flowLogin.body), &secondFactor); err != nil {
		t.Fatal(err)
	}
	if secondFactor.Type != "login" || len(secondFactor.Contacts) == 0 {
		t.Fatalf("expected the second-factor login flow, got %s", flowLogin.body)
	}
	loginOrigin := merge(origin, map[string]string{"Cookie": scenarioCookie(flowLogin.headers, "__Host-quoin-flow")})
	mustPost(t, server, loginOrigin, `/api/v1/auth/flow/challenge`, `{"contactId":"`+secondFactor.Contacts[0].ID+`"}`, http.StatusOK)
	verified := mustPost(t, server, loginOrigin, `/api/v1/auth/flow/verify`, `{"code":"`+sender.lastCode()+`"}`, http.StatusOK)
	cookie := scenarioCookie(verified.headers, "__Host-quoin-session")
	if !strings.HasPrefix(cookie, "__Host-quoin-session=") {
		t.Fatalf("expected session cookie, got %q", verified.headers.Values("Set-Cookie"))
	}

	me := mustRequest(t, server, map[string]string{"Cookie": cookie}, `/api/v1/auth/me`, http.StatusOK)
	t.Logf("me: %s", me)

	change := mustDo(t, server, http.MethodPut, merge(origin, map[string]string{"Cookie": cookie}),
		`/api/v1/auth/password`, `{"currentPassword":"`+password+`","newPassword":"A better personal passphrase 2027!"}`, http.StatusNoContent)
	t.Logf("change headers: %v", change.headers)

	after := mustRequest(t, server, map[string]string{"Cookie": cookie}, `/api/v1/auth/me`, http.StatusOK)
	var updated struct {
		PasswordChangeRequired bool `json:"passwordChangeRequired"`
		AuthRevision           int  `json:"authRevision"`
	}
	if err := json.Unmarshal([]byte(after), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.PasswordChangeRequired || updated.AuthRevision != 3 {
		// Revision chain: seed 1, initialization password step 2, own change 3.
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

func mustHandler(t *testing.T, service *auth.Service, db *sql.DB, reader execution.Reader, origin string, rootKeyFile string) http.Handler {
	t.Helper()
	application := app.NewAPIServer(service, db, rootKeyFile)
	if err := application.SetReadOnlyReader(reader); err != nil {
		t.Fatal(err)
	}
	// Tests use a fixed deployment value rather than httptest's Host so receiver
	// configuration cannot accidentally begin trusting request-controlled hosts.
	application.SetStelePublicURL("https://alerts.example.com/stele/alerts")
	handler, err := app.NewHandler(application, origin)
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
	scenario := newAuthScenario(t)
	server := scenario.server

	adminSession := scenario.login(t, "admin", scenario.adminPassword)
	admin := scenario.sessionHeaders(adminSession)
	// A second non-admin user (Operator) exercises the 403 paths: created with
	// an assigned OTP contact through the real admin surface, initialized via
	// the real operator flow, then logged in with the two-step login.
	const operatorTemp = "Operator initial passphrase 2026!"
	const operatorFormal = "Operator passphrase 2027!"
	scenario.createOperator(t, adminSession, "reveal-create-0001", "operator", "Ops Operator", operatorTemp, "operator@example.test")
	operator := scenario.sessionHeaders(scenario.initializeOperatorSession(t, "operator", operatorTemp, operatorFormal))

	create := mustPost(t, server, admin,
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
	replay := mustPost(t, server, admin,
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
	mustPost(t, server, admin,
		`/api/v1/alert-sources`, `{"key":"other-am","protocol":"alertmanager","clientCommandId":"cmd-0001"}`, http.StatusConflict)

	// Operator is forbidden from both commands.
	mustPost(t, server, operator,
		`/api/v1/alert-sources`, `{"key":"ops-am","protocol":"alertmanager","clientCommandId":"cmd-0002"}`, http.StatusForbidden)
	mustPost(t, server, operator,
		`/api/v1/alert-sources/credentials/reveal`, `{"revealHandle":"`+created.RevealHandle+`"}`, http.StatusForbidden)

	// Reveal succeeds exactly once; the second consume of the same handle is 410.
	reveal := mustPost(t, server, admin,
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
	mustPost(t, server, admin,
		`/api/v1/alert-sources/credentials/reveal`, `{"revealHandle":"`+created.RevealHandle+`"}`, http.StatusGone)

	// After consume, a replay of the same command reports revealAvailable=false
	// and never re-creates the credential (SEC-REVEAL-*).
	after := mustPost(t, server, admin,
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
