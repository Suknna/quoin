package app_test

// Shared real-authentication fixture for the HTTP tests in this external
// package (ADR-0010 single-step surface). The local channel verifies a
// password once and issues the session cookie directly; the first forced
// password change rides the restricted session over the ordinary
// PUT /api/v1/auth/password. Helpers drive exactly that production path —
// no direct-login bypass exists here.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/test/support"
)

const (
	scenarioAdminEmail    = "admin@quoin.test"
	scenarioAdminPassword = "scenario admin horse battery 2026!"
	scenarioPublicOrigin  = "https://quoin.example.com"
	scenarioSessionCookie = "__Host-quoin-session"
)

// authScenario is the shared fixture: real database, real auth service, the
// seeded bootstrap administrator unlocked through the real single-step path
// (initial random password + forced change), and the real HTTP server the
// tests log in against.
type authScenario struct {
	t             *testing.T
	server        *httptest.Server
	db            *sql.DB
	reader        execution.Reader
	auth          *auth.Service
	adminPassword string
	publicOrigin  string
	rootKeyFile   string
	rootDir       string
	// origin carries the default API headers (Origin + JSON content type).
	origin map[string]string
}

// newAuthScenario boots the fixture with the default handler (mustHandler).
func newAuthScenario(t *testing.T) *authScenario {
	return newAuthScenarioWith(t, nil)
}

// newAuthScenarioWith boots the fixture and builds the served handler with
// build (nil selects the default mustHandler handler). The callback receives
// the fixture before initialization completes so custom wiring (backup
// service, stele URL) can attach to the same database and service.
func newAuthScenarioWith(t *testing.T, build func(scenario *authScenario) http.Handler) *authScenario {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: scenarioPublicOrigin,
		DataDirectory:             filepath.Join(root, "data"),
		BackupDirectory:           filepath.Join(root, "backup"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		RuntimeClientCAFile:       filepath.Join(secrets, "stele-service-token"),
	}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	initial, err := auth.GenerateInitialPassword()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBootstrapAdmin(ctx, initial, time.Now().UTC().Add(auth.InitialPasswordLifetime)); err != nil {
		t.Fatal(err)
	}
	scenario := &authScenario{
		t: t, db: database.SQL, reader: database.Reader, auth: service,
		adminPassword: scenarioAdminPassword,
		publicOrigin:  config.PublicOrigin, rootKeyFile: config.RootKeyFile, rootDir: root,
		origin: map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"},
	}
	// The administrator is unlocked through the real single-step path so the
	// deployment is in its genuine initialized state before any test logs in.
	scenario.initializeAdminServiceSide(initial)
	var handler http.Handler
	if build != nil {
		handler = build(scenario)
	} else {
		handler = mustHandler(t, service, database.SQL, database.Reader, config.PublicOrigin, config.RootKeyFile)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	scenario.server = server
	return scenario
}

// initializeAdminServiceSide unlocks the bootstrap administrator through the
// service (the same steps the restricted session drives over HTTP).
func (s *authScenario) initializeAdminServiceSide(initialPassword string) {
	s.t.Helper()
	ctx := context.Background()
	result, err := s.auth.LoginWithPassword(ctx, "admin", initialPassword, "Test on Linux")
	if err != nil {
		s.t.Fatalf("bootstrap login: %v", err)
	}
	session, err := s.auth.Authenticate(ctx, result.Bearer)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := s.auth.ChangePassword(ctx, session, initialPassword, s.adminPassword); err != nil {
		s.t.Fatalf("forced password change: %v", err)
	}
}

// login performs the real single-step login over HTTP and returns the
// __Host-quoin-session cookie value.
func (s *authScenario) login(t *testing.T, username, password string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	response := mustPost(t, s.server, s.origin, "/api/v1/auth/login", string(body), http.StatusOK)
	session := scenarioCookie(response.headers, scenarioSessionCookie)
	if session == "" {
		t.Fatalf("login issued no session cookie: %v", response.headers)
	}
	return strings.TrimPrefix(session, scenarioSessionCookie+"=")
}

// sessionHeaders returns the API headers carrying the given session cookie
// value on top of the scenario origin headers.
func (s *authScenario) sessionHeaders(session string) map[string]string {
	return merge(s.origin, map[string]string{"Cookie": scenarioSessionCookie + "=" + session})
}

// createOperator creates an operator with a display contact through the real
// admin surface.
func (s *authScenario) createOperator(t *testing.T, adminSession, clientCommandID, username, displayName, tempPassword, contact string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"clientCommandId": clientCommandID, "username": username, "displayName": displayName,
		"role": "operator", "password": tempPassword,
		"contacts": []map[string]string{{"channel": "email", "target": contact}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustPost(t, s.server, s.sessionHeaders(adminSession), "/api/v1/admin/users", string(body), http.StatusCreated)
}

// initializeOperatorSession drives the operator's forced password change over
// HTTP (restricted session + PUT /api/v1/auth/password) and returns the
// operator's formal session cookie value.
func (s *authScenario) initializeOperatorSession(t *testing.T, username, tempPassword, newPassword string) string {
	t.Helper()
	restricted := s.login(t, username, tempPassword)
	changeBody, err := json.Marshal(map[string]string{"currentPassword": tempPassword, "newPassword": newPassword})
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.server, http.MethodPut, s.sessionHeaders(restricted), "/api/v1/auth/password", string(changeBody), http.StatusNoContent)
	return s.login(t, username, newPassword)
}

// scenarioCookie extracts the Name=Value cookie of the given name from a
// response's Set-Cookie header values.
func scenarioCookie(headers http.Header, name string) string {
	for _, value := range headers.Values("Set-Cookie") {
		if strings.HasPrefix(value, name+"=") {
			return strings.Split(value, ";")[0]
		}
	}
	return ""
}
