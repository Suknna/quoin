package app

// The standalone browser identity surface is retired with the browser
// business (受控浏览器退役): its implementation stays in browser_standalone.go
// but no route mounts it, so its former HTTP coverage is gone. This file now
// only owns the surface boot helper shared by tests that exercise the
// remaining public routes (e.g. the runtime slot gate).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

type standaloneSurface struct {
	server *httptest.Server
	cookie string
	origin map[string]string
	// application is the live server instance: runtime-gate tests re-resolve
	// plugin enablement on it to simulate a deployment whose YAML changes
	// between releases.
	application *apiServer
}

// newStandaloneSurface boots the real handler graph with the plugin
// enablement resolved from the given whitelist (nil = descriptor defaults).
func newStandaloneSurface(t *testing.T, configured []string) *standaloneSurface {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.example.com",
		DataDirectory:             filepath.Join(root, "data"),
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
	t.Cleanup(func() { _ = database.Close() })
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	const temporary = "Correct horse battery staple 2026!"
	if _, err := authService.CreateFirstAdmin(ctx, "admin", "Quoin Admin", temporary); err != nil {
		t.Fatal(err)
	}
	application := newAPIServer(authService, database.SQL, config.RootKeyFile)
	// The same boot seam Run() uses: deployment YAML selects the whitelist and
	// the registry resolves it once. This ordering (after handler
	// construction, before serving) is why the capability fact is a
	// request-time read.
	if _, err := application.configurePlugins(configured); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(application, config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// A real admin session: login, then clear the forced password change.
	cookie := loginStandaloneAdmin(t, server, config.PublicOrigin, temporary)
	return &standaloneSurface{server: server, cookie: cookie, origin: map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json", "Cookie": cookie}, application: application}
}

func loginStandaloneAdmin(t *testing.T, server *httptest.Server, origin, temporary string) string {
	t.Helper()
	do := func(method, path, body string, headers map[string]string) (*http.Response, string) {
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		payload, _ := io.ReadAll(response.Body)
		return response, string(payload)
	}
	response, _ := do(http.MethodPost, "/api/v1/auth/login", fmt.Sprintf(`{"username":"admin","password":%q}`, temporary), map[string]string{"Origin": origin, "Content-Type": "application/json"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", response.StatusCode)
	}
	setCookie := response.Header.Get("Set-Cookie")
	if !strings.HasPrefix(setCookie, "__Host-quoin-session=") {
		t.Fatalf("no session cookie: %q", setCookie)
	}
	cookie := strings.Split(strings.Split(setCookie, ";")[0], "=")[1]
	newPassword := "A private admin passphrase 2027!"
	session := map[string]string{"Cookie": "__Host-quoin-session=" + cookie, "Origin": origin, "Content-Type": "application/json"}
	if response, _ := do(http.MethodPut, "/api/v1/auth/password", fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, temporary, newPassword), session); response.StatusCode != http.StatusNoContent {
		t.Fatalf("password change: %d", response.StatusCode)
	}
	return "__Host-quoin-session=" + cookie
}

func (surface *standaloneSurface) request(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, _ := http.NewRequest(method, surface.server.URL+path, reader)
	for key, value := range surface.origin {
		request.Header.Set(key, value)
	}
	response, err := surface.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(payload)
}
