package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/test/support"
)

const admissionPassword = "correct-horse-battery-staple"

// newAdmissionTestServer builds a real apiServer on the applied contract
// schema with an initialized administrator.
func newAdmissionTestServer(t *testing.T) (*apiServer, *auth.Service) {
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
	initializeAdmissionAdmin(t, service, initial)
	application := NewMaintenanceAPIServer(service, database.SQL, config.RootKeyFile)
	if err := application.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	if err := application.configureLoginProviders(nil); err != nil {
		t.Fatal(err)
	}
	return application, service
}

// initializeAdmissionAdmin unlocks the bootstrap administrator through the
// real single-step path (initial random password + forced change).
func initializeAdmissionAdmin(t *testing.T, service *auth.Service, initialPassword string) {
	t.Helper()
	ctx := context.Background()
	result, err := service.LoginWithPassword(ctx, "admin", initialPassword, "Test on Linux")
	if err != nil {
		t.Fatalf("bootstrap login: %v", err)
	}
	session, err := service.Authenticate(ctx, result.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, session, initialPassword, admissionPassword); err != nil {
		t.Fatalf("forced password change: %v", err)
	}
}

// admissionLogin performs the real single-step login and returns the bearer.
func admissionLogin(t *testing.T, service *auth.Service) string {
	t.Helper()
	result, err := service.LoginWithPassword(context.Background(), "admin", admissionPassword, "Mozilla/5.0 Chrome Linux")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return result.Bearer
}

// The tests below exercise the real constructors (NewHandler and
// newMaintenanceHandler, which install the guard and run ValidateSurface at
// construction), so declaration drift fails here exactly as it would in main.

func TestNewHandlerBuildsGuardedSurfaceWithoutPlannedDeclarations(t *testing.T) {
	application, _ := newAdmissionTestServer(t)
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

	// A session operation without a credential is rejected before any handler.
	response, err := server.Client().Get(server.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("absent session status = %d, want 401", response.StatusCode)
	}
}

func TestMaintenanceSurfacesBuildForEveryReason(t *testing.T) {
	application, _ := newAdmissionTestServer(t)
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
	application, service := newAdmissionTestServer(t)
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
	bearer := admissionLogin(t, service)
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
	application, service := newAdmissionTestServer(t)
	handler, err := NewHandler(application, "https://quoin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	bearer := admissionLogin(t, service)

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

func TestPublicConfigRequestIsServedWithoutASession(t *testing.T) {
	application, _ := newAdmissionTestServer(t)
	handler, err := NewHandler(application, "https://quoin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// The public login-channel projection answers anonymous requests; it is
	// the login page's bootstrap endpoint and must never require a session.
	response, err := server.Client().Get(server.URL + "/api/v1/auth/config")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("auth config status = %d, want 200", response.StatusCode)
	}
	var projection struct {
		Local struct {
			Enabled bool `json:"enabled"`
			Visible bool `json:"visible"`
		} `json:"local"`
	}
	if err := json.NewDecoder(response.Body).Decode(&projection); err != nil {
		t.Fatal(err)
	}
	if !projection.Local.Enabled || !projection.Local.Visible {
		t.Fatalf("default local channel projection = %+v", projection)
	}
}
