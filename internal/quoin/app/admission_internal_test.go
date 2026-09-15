package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// stubSender captures OTP deliveries so tests can complete real flows.
type stubSender struct {
	mu       sync.Mutex
	messages []auth.Message
}

func (s *stubSender) Send(_ context.Context, message auth.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message)
	return nil
}

func (s *stubSender) lastCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return ""
	}
	return s.messages[len(s.messages)-1].Variables["code"]
}

const (
	admissionAdminEmail = "admin@quoin.test"
	admissionPassword   = "correct-horse-battery-staple"
)

// newAdmissionTestServer builds a real apiServer on the applied contract
// schema with an initialized administrator.
func newAdmissionTestServer(t *testing.T) (*apiServer, *auth.Service, *stubSender) {
	t.Helper()
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
	sender := &stubSender{}
	if err := service.ConfigureAuth(auth.AuthConfig{OTPKey: bytes.Repeat([]byte{0x5A}, 32), Sender: sender}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBootstrapAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	initializeAdmissionAdmin(t, service, sender)
	application := NewMaintenanceAPIServer(service, database.SQL, config.RootKeyFile)
	if err := application.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	return application, service, sender
}

// initializeAdmissionAdmin drives the real admin initialization flow.
func initializeAdmissionAdmin(t *testing.T, service *auth.Service, sender *stubSender) {
	t.Helper()
	ctx := context.Background()
	flow, _, err := service.StartAdminInitialization(ctx, "admin", "admin")
	if err != nil {
		t.Fatalf("start admin initialization: %v", err)
	}
	if err := service.SetFlowPassword(ctx, flow.Bearer, admissionPassword); err != nil {
		t.Fatalf("set flow password: %v", err)
	}
	masked, err := service.RegisterFlowContact(ctx, flow.Bearer, "email", admissionAdminEmail)
	if err != nil {
		t.Fatalf("register flow contact: %v", err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, masked.Locator); err != nil {
		t.Fatalf("send flow challenge: %v", err)
	}
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, sender.lastCode()); err != nil {
		t.Fatalf("verify flow challenge: %v", err)
	}
	if err := service.CompleteAdminInitialization(ctx, flow.Bearer); err != nil {
		t.Fatalf("complete admin initialization: %v", err)
	}
}

// admissionLogin performs the real two-step login and returns the session bearer.
func admissionLogin(t *testing.T, service *auth.Service, sender *stubSender) string {
	t.Helper()
	ctx := context.Background()
	flow, _, err := service.StartAuthentication(ctx, "admin", admissionPassword, "Mozilla/5.0 Chrome Linux")
	if err != nil {
		t.Fatalf("start authentication: %v", err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, flow.Contacts[0].Locator); err != nil {
		t.Fatalf("send login challenge: %v", err)
	}
	result, err := service.CompleteLogin(ctx, flow.Bearer, sender.lastCode())
	if err != nil {
		t.Fatalf("complete login: %v", err)
	}
	return result.Bearer
}

// The tests below exercise the real constructors (NewHandler and
// newMaintenanceHandler, which install the guard and run ValidateSurface at
// construction), so declaration drift fails here exactly as it would in main.

func TestNewHandlerBuildsGuardedSurfaceWithoutPlannedDeclarations(t *testing.T) {
	application, _, _ := newAdmissionTestServer(t)
	registry, err := NormalAccessRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if remaining := registry.RemainingPlanned(); len(remaining) != 0 {
		t.Fatalf("RemainingPlanned = %v, want empty: adoption completes with an exact inventory", remaining)
	}
	handler, err := NewHandler(application, "https://quoin.example.com")
	if err != nil {
		t.Fatalf("NewHandler construction must validate the live surface: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// A flow operation without a credential is rejected before any handler.
	response, err := server.Client().Get(server.URL + "/api/v1/auth/flow")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("absent flow status = %d, want 401", response.StatusCode)
	}
}

func TestMaintenanceSurfacesBuildForEveryReason(t *testing.T) {
	application, _, _ := newAdmissionTestServer(t)
	for _, reason := range []string{"RootKeyRebind", "Restore", "Upgrade"} {
		registry, err := MaintenanceAccessRegistry(reason)
		if err != nil {
			t.Fatal(err)
		}
		if remaining := registry.RemainingPlanned(); len(remaining) != 0 {
			t.Fatalf("%s: RemainingPlanned = %v, want empty", reason, remaining)
		}
		if _, err := newMaintenanceHandler(application, "https://quoin.example.com", reason); err != nil {
			t.Fatalf("%s maintenance surface: %v", reason, err)
		}
	}
}

func TestGuardRecordsAccessFactThroughAuditSink(t *testing.T) {
	application, service, sender := newAdmissionTestServer(t)
	handler, err := NewHandler(application, "https://quoin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// Anonymous request: rejected by the guard, no audit row.
	response, err := server.Client().Get(server.URL + "/api/v1/maintenance")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", response.StatusCode)
	}

	// Admin session via the real two-step login: admitted, handler runs, the
	// default sink persists the phase=access record.
	bearer := admissionLogin(t, service, sender)
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/maintenance", nil)
	request.Header.Set("Cookie", "__Host-quoin-session="+bearer)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d, want 200", response.StatusCode)
	}

	var phase, outcome, action, initiatorType string
	var actorID int64
	if err := application.db.QueryRow(`SELECT phase,outcome,actor_id,action,initiator_type FROM audit_events WHERE phase='access' ORDER BY id DESC LIMIT 1`).Scan(&phase, &outcome, &actorID, &action, &initiatorType); err != nil {
		t.Fatalf("access fact missing from audit_events: %v", err)
	}
	if phase != "access" || outcome != "success" || actorID <= 0 || action != "getMaintenanceState" || initiatorType != "user" {
		t.Fatalf("access record = phase %q outcome %q actor %d action %q initiator %q", phase, outcome, actorID, action, initiatorType)
	}
	var correlation, requestID string
	if err := application.db.QueryRow(`SELECT correlation_id,request_id FROM audit_events WHERE phase='access' ORDER BY id DESC LIMIT 1`).Scan(&correlation, &requestID); err != nil || correlation == "" || requestID == "" {
		t.Fatalf("access record correlation missing: %v %q %q", err, correlation, requestID)
	}
}

func TestGuardEnforcesLevelsOnRealSurface(t *testing.T) {
	application, service, sender := newAdmissionTestServer(t)
	handler, err := NewHandler(application, "https://quoin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	bearer := admissionLogin(t, service, sender)

	// Session-level admits any valid session.
	response, err := getWithCookie(server, "/api/v1/auth/me", bearer)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/auth/me status = %d, want 200", response.StatusCode)
	}

	// The single seeded admin is an admin, so the role-denial path is
	// asserted through an anonymous request instead: admin operations must
	// answer 401 from the guard, never a leaked handler response.
	response, err = server.Client().Get(server.URL + "/api/v1/admin/about")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous /admin/about status = %d, want 401", response.StatusCode)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "unauthenticated" {
		t.Fatalf("problem code = %q, want unauthenticated", problem.Code)
	}
}

func getWithCookie(server *httptest.Server, path, bearer string) (*http.Response, error) {
	request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
	request.Header.Set("Cookie", "__Host-quoin-session="+bearer)
	return server.Client().Do(request)
}

func TestFlowRequestRecordsAccessRow(t *testing.T) {
	application, service, _ := newAdmissionTestServer(t)
	handler, err := NewHandler(application, "https://quoin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// An incomplete login flow still claims the administrator identity: the
	// guard records the access fact with the flow's bound user and its stored
	// correlation, regardless of what the handler does.
	flow, _, err := service.StartAuthentication(context.Background(), "admin", admissionPassword, "Mozilla/5.0 Chrome Linux")
	if err != nil {
		t.Fatal(err)
	}
	flowRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/flow", nil)
	flowRequest.Header.Set("Cookie", "__Host-quoin-flow="+flow.Bearer)
	response, err := server.Client().Do(flowRequest)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("flow status = %d, want 200", response.StatusCode)
	}
	var action, correlation string
	var actorID int64
	if err := application.db.QueryRow(`SELECT action,actor_id,correlation_id FROM audit_events WHERE phase='access' AND action='readAuthenticationFlow' ORDER BY id DESC LIMIT 1`).Scan(&action, &actorID, &correlation); err != nil {
		t.Fatalf("flow access row missing: %v", err)
	}
	if actorID <= 0 || correlation == "" {
		t.Fatalf("flow access row = actor %d correlation %q", actorID, correlation)
	}
}
