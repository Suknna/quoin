package app

// The standalone browser identity surface is retired with the browser
// business (受控浏览器退役): its implementation stays in browser_standalone.go
// but no route mounts it, so its former HTTP coverage is gone. This file now
// only owns the surface boot helper shared by tests that exercise the
// remaining public routes (e.g. the runtime slot gate).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
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
		RuntimeClientCAFile:     filepath.Join(secrets, "stele-service-token"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
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

	cookie := loginStandaloneAdmin(t, server, config.PublicOrigin, formalPassword, sender)
	return &standaloneSurface{server: server, cookie: cookie, origin: map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json", "Cookie": cookie}, application: application}
}

func loginStandaloneAdmin(t *testing.T, server *httptest.Server, origin, password string, sender *stubSender) string {
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
	// Step one: the verified password opens the login flow (no session yet).
	response, body := do(http.MethodPost, "/api/v1/auth/login", fmt.Sprintf(`{"username":"admin","password":%q}`, password), map[string]string{"Origin": origin, "Content-Type": "application/json"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", response.StatusCode, body)
	}
	flowCookie := scenarioSetCookie(response.Cookies(), flowCookieName)
	if flowCookie == nil {
		t.Fatalf("no flow cookie: %v", response.Cookies())
	}
	var flow struct {
		Contacts []struct {
			ID string `json:"id"`
		} `json:"contacts"`
	}
	if err := json.Unmarshal([]byte(body), &flow); err != nil {
		t.Fatal(err)
	}
	if len(flow.Contacts) == 0 {
		t.Fatalf("login flow without a deliverable contact: %s", body)
	}
	// Step two: the consumed OTP code issues the session cookie.
	sessionHeaders := map[string]string{"Cookie": flowCookieName + "=" + flowCookie.Value, "Origin": origin, "Content-Type": "application/json"}
	if response, body := do(http.MethodPost, "/api/v1/auth/flow/challenge", `{"contactId":"`+flow.Contacts[0].ID+`"}`, sessionHeaders); response.StatusCode != http.StatusOK {
		t.Fatalf("challenge: %d %s", response.StatusCode, body)
	}
	verifyResponse, verifyBody := do(http.MethodPost, "/api/v1/auth/flow/verify", `{"code":"`+sender.lastCode()+`"}`, sessionHeaders)
	if verifyResponse.StatusCode != http.StatusOK {
		t.Fatalf("verify: %d %s", verifyResponse.StatusCode, verifyBody)
	}
	sessionCookie := scenarioSetCookie(verifyResponse.Cookies(), scenarioSessionCookieName)
	if sessionCookie == nil {
		t.Fatalf("no session cookie after verification: %v", verifyResponse.Cookies())
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
