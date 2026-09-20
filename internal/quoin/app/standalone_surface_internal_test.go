package app

// This file owns the surface boot helper shared by tests that exercise the
// remaining public routes (e.g. the runtime slot gate): the real handler
// graph, real admin initialization and the real two-step login.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/test/support"
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
// The administrator is initialized through the real flow and the session
// comes from the real two-step login.
func newStandaloneSurface(t *testing.T, configured []string) *standaloneSurface {
	t.Helper()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.example.com",
		DataDirectory:             filepath.Join(root, "data"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		RuntimeClientCAFile:       filepath.Join(secrets, "stele-service-token"),
	}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, authService, sender := newScenarioAuth(t, config.DataDirectory, config.RootKeyFile)
	t.Cleanup(func() { _ = database.Close() })
	const formalPassword = "A private admin passphrase 2027!"
	scenarioInitializeAdmin(t, authService, sender, formalPassword)
	application := newAPIServer(authService, database.SQL, config.RootKeyFile)
	if err := application.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
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

	cookie := loginStandaloneAdmin(t, server, config.PublicOrigin, formalPassword)
	return &standaloneSurface{server: server, cookie: cookie, origin: map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json", "Cookie": cookie}, application: application}
}

func loginStandaloneAdmin(t *testing.T, server *httptest.Server, origin, password string) string {
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
	// The single-step login issues the session cookie directly.
	response, body := do(http.MethodPost, "/api/v1/auth/login", fmt.Sprintf(`{"username":"admin","password":%q}`, password), map[string]string{"Origin": origin, "Content-Type": "application/json"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", response.StatusCode, body)
	}
	sessionCookie := scenarioSetCookie(response.Cookies(), scenarioSessionCookieName)
	if sessionCookie == nil {
		t.Fatalf("no session cookie after login: %v", response.Cookies())
	}
	return scenarioSessionCookieName + "=" + sessionCookie.Value
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
