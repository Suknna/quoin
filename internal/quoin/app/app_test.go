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

	server := httptest.NewServer(mustHandler(t, service, database.SQL, config.PublicOrigin))
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

func splitCookie(setCookie string) string {
	return strings.Split(setCookie, ";")[0]
}

func mustHandler(t *testing.T, service *auth.Service, db *sql.DB, origin string) http.Handler {
	t.Helper()
	handler, err := app.NewHandler(app.NewAPIServer(service, db), origin)
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
