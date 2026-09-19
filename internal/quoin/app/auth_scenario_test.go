package app_test

// Shared real-authentication fixture for the legacy HTTP tests in this
// external package. The two-step login made the old "CreateFirstAdmin +
// one-shot password login" fixtures impossible: a verified password only
// opens a flow, the second factor is an OTP delivered to an assigned
// contact, and only the consumed code issues the session cookie. These
// helpers drive exactly that production path — initialization through the
// real flow steps and login over the real HTTP surface (password start,
// challenge, verify) — with a TEST-ONLY recording auth.Sender capturing the
// OTP codes. No direct-login bypass exists here.

import (
	"github.com/Suknna/quoin/internal/quoin/execution"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// recordingSender is a TEST-ONLY auth.Sender fake that keeps every delivered
// message so tests can read the current OTP code.
type recordingSender struct {
	mu       sync.Mutex
	messages []auth.Message
}

func (s *recordingSender) Send(_ context.Context, message auth.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message)
	return nil
}

func (s *recordingSender) lastCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return ""
	}
	return s.messages[len(s.messages)-1].Variables["code"]
}

const (
	scenarioAdminEmail     = "admin@quoin.test"
	scenarioAdminPassword  = "scenario admin horse battery 2026!"
	scenarioPublicOrigin   = "https://quoin.example.com"
	scenarioSessionCookie  = "__Host-quoin-session"
	scenarioFlowCookieName = "__Host-quoin-flow"
)

// authScenario is the shared fixture: real database, real auth service with
// OTP delivery wired to the recording sender, the seeded pending bootstrap
// administrator fully initialized through the real flow, and the real HTTP
// server the tests log in against.
type authScenario struct {
	t             *testing.T
	server        *httptest.Server
	db            *sql.DB
	reader        execution.Reader
	auth          *auth.Service
	sender        *recordingSender
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
		RuntimeClientCAFile:     filepath.Join(secrets, "stele-service-token"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
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
	sender := &recordingSender{}
	if err := service.ConfigureAuth(auth.AuthConfig{OTPKey: bytes.Repeat([]byte{0x2C}, 32), Sender: sender}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBootstrapAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	scenario := &authScenario{
		t: t, db: database.SQL, reader: database.Reader, auth: service, sender: sender,
		adminPassword: scenarioAdminPassword,
		publicOrigin:  config.PublicOrigin, rootKeyFile: config.RootKeyFile, rootDir: root,
		origin: map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"},
	}
	// The administrator is initialized through the real flow steps (password
	// step plus verified contact) so the deployment is in its genuine
	// initialized state before any test logs in.
	scenario.initializeAdminServiceSide()
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

// initializeAdminServiceSide runs the real admin initialization flow through
// the service (the same steps the HTTP wizard drives).
func (s *authScenario) initializeAdminServiceSide() {
	s.t.Helper()
	ctx := context.Background()
	flow, _, err := s.auth.StartAdminInitialization(ctx, "admin", "admin")
	if err != nil {
		s.t.Fatalf("start admin initialization: %v", err)
	}
	if err := s.auth.SetFlowPassword(ctx, flow.Bearer, s.adminPassword); err != nil {
		s.t.Fatalf("set initialization password: %v", err)
	}
	masked, err := s.auth.RegisterFlowContact(ctx, flow.Bearer, "email", scenarioAdminEmail)
	if err != nil {
		s.t.Fatalf("register initialization contact: %v", err)
	}
	if _, _, err := s.auth.SendFlowChallenge(ctx, flow.Bearer, masked.Locator); err != nil {
		s.t.Fatalf("send initialization challenge: %v", err)
	}
	if err := s.auth.VerifyFlowChallenge(ctx, flow.Bearer, s.sender.lastCode()); err != nil {
		s.t.Fatalf("verify initialization challenge: %v", err)
	}
	if err := s.auth.CompleteAdminInitialization(ctx, flow.Bearer); err != nil {
		s.t.Fatalf("complete admin initialization: %v", err)
	}
}

// login performs the real two-step login over HTTP for an initialized user:
// password start, OTP challenge to the first assigned contact, code verify —
// and returns the __Host-quoin-session cookie value. A pending forced
// password change rides along in the issued session (HTTP-AUTH-006).
func (s *authScenario) login(t *testing.T, username, password string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	start := mustPost(t, s.server, s.origin, "/api/v1/auth/login", string(body), http.StatusOK)
	if strings.Contains(start.body, "$schema") {
		t.Fatalf("flow body must match the frozen OpenAPI schema, got %s", start.body)
	}
	flowCookie := scenarioCookie(start.headers, scenarioFlowCookieName)
	if flowCookie == "" {
		t.Fatalf("login issued no %s cookie: %v", scenarioFlowCookieName, start.headers)
	}
	var flow struct {
		Type     string `json:"type"`
		Contacts []struct {
			ID string `json:"id"`
		} `json:"contacts"`
	}
	if err := json.Unmarshal([]byte(start.body), &flow); err != nil {
		t.Fatal(err)
	}
	if len(flow.Contacts) == 0 {
		t.Fatalf("login flow has no deliverable contact: %s", start.body)
	}
	flowHeaders := merge(s.origin, map[string]string{"Cookie": flowCookie})
	challengeBody, err := json.Marshal(map[string]string{"contactId": flow.Contacts[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	mustPost(t, s.server, flowHeaders, "/api/v1/auth/flow/challenge", string(challengeBody), http.StatusOK)
	verifyBody, err := json.Marshal(map[string]string{"code": s.sender.lastCode()})
	if err != nil {
		t.Fatal(err)
	}
	verify := mustPost(t, s.server, flowHeaders, "/api/v1/auth/flow/verify", string(verifyBody), http.StatusOK)
	session := scenarioCookie(verify.headers, scenarioSessionCookie)
	if session == "" {
		t.Fatalf("verified login issued no session cookie: %v", verify.headers)
	}
	return strings.TrimPrefix(session, scenarioSessionCookie+"=")
}

// sessionHeaders returns the API headers carrying the given session cookie
// value on top of the scenario origin headers.
func (s *authScenario) sessionHeaders(session string) map[string]string {
	return merge(s.origin, map[string]string{"Cookie": scenarioSessionCookie + "=" + session})
}

// createOperator creates an operator with an assigned OTP contact through the
// real admin surface. Contacts are mandatory since the two-step login (the
// user could otherwise never receive a second factor).
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

// initializeOperatorSession drives the real operator initialization flow over
// HTTP (forced password step plus contact verification of the admin-assigned
// target) and then performs the two-step login. Returns the operator's
// session cookie value.
func (s *authScenario) initializeOperatorSession(t *testing.T, username, tempPassword, newPassword string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": tempPassword})
	if err != nil {
		t.Fatal(err)
	}
	start := mustPost(t, s.server, s.origin, "/api/v1/auth/login", string(body), http.StatusOK)
	var flow struct {
		Type     string `json:"type"`
		Contacts []struct {
			ID string `json:"id"`
		} `json:"contacts"`
	}
	if err := json.Unmarshal([]byte(start.body), &flow); err != nil {
		t.Fatal(err)
	}
	if flow.Type != "operator_initialize" || len(flow.Contacts) == 0 {
		t.Fatalf("expected an operator initialization flow with contacts, got %s", start.body)
	}
	flowCookie := scenarioCookie(start.headers, scenarioFlowCookieName)
	flowHeaders := merge(s.origin, map[string]string{"Cookie": flowCookie})
	passwordBody, err := json.Marshal(map[string]string{"newPassword": newPassword})
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.server, http.MethodPut, flowHeaders, "/api/v1/auth/flow/password", string(passwordBody), http.StatusNoContent)
	challengeBody, err := json.Marshal(map[string]string{"contactId": flow.Contacts[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	mustPost(t, s.server, flowHeaders, "/api/v1/auth/flow/challenge", string(challengeBody), http.StatusOK)
	verifyBody, err := json.Marshal(map[string]string{"code": s.sender.lastCode()})
	if err != nil {
		t.Fatal(err)
	}
	mustPost(t, s.server, flowHeaders, "/api/v1/auth/flow/verify", string(verifyBody), http.StatusOK)
	mustPost(t, s.server, flowHeaders, "/api/v1/auth/flow/complete", `{}`, http.StatusNoContent)
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
