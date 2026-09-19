package app

// admission_bootstrap_test.go verifies the deployment bootstrap gate: while
// no administrator completed initialization, every normal managed endpoint is
// blocked and only the minimal declared authentication-flow surface passes;
// a crafted legacy database with an initialized operator never opens it; and
// probe failures fail closed without leaking detail.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// newUninitializedAdmissionServer boots the applied contract schema without
// seeding or initializing any user.
func newUninitializedAdmissionServer(t *testing.T) (*apiServer, *auth.Service, *bootstrap.Database) {
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
	application := NewMaintenanceAPIServer(service, database.SQL, config.RootKeyFile)
	if err := application.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	return application, service, database
}

// newGatedSurface builds the real NewHandler surface, whose outermost layer
// is the production bootstrap gate.
func newGatedSurface(t *testing.T, application *apiServer) *httptest.Server {
	t.Helper()
	handler, err := NewHandler(application, "https://quoin.example.com")
	if err != nil {
		t.Fatalf("NewHandler must construct with the bootstrap gate: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func getBody(t *testing.T, server *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	response, err := server.Client().Get(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("%s: undecodable body: %v", path, err)
	}
	return response.StatusCode, body
}

func TestBootstrapGateBlocksUninitializedSystem(t *testing.T) {
	application, _, _ := newUninitializedAdmissionServer(t)
	server := newGatedSurface(t, application)

	// Normal managed endpoints are blocked with the initialization envelope,
	// regardless of authentication state (no session can exist anyway).
	for _, path := range []string{
		"/api/v1/maintenance",
		"/api/v1/admin/about",
		"/api/v1/users-not-a-route",
	} {
		status, body := getBody(t, server, path)
		if status != http.StatusServiceUnavailable || body["code"] != "initialization_required" {
			t.Fatalf("GET %s = %d %v, want 503 initialization_required", path, status, body["code"])
		}
	}

	status, body := getBody(t, server, "/api/v1/auth/me")
	if status != http.StatusUnauthorized || body["code"] != "initialization_required" {
		t.Fatalf("session discovery must show authentication UI without granting access: %d %v", status, body)
	}

	// The minimal declared authentication surface passes the gate: the
	// flow-start route reaches the API (its response is a handler/validation
	// answer, never the gate's initialization_required envelope).
	flowStart, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/auth/login", nil)
	flowResponse, err := server.Client().Do(flowStart)
	if err != nil {
		t.Fatal(err)
	}
	flowResponse.Body.Close()
	if flowResponse.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("POST /api/v1/auth/login = %d, the gate must pass declared flow routes", flowResponse.StatusCode)
	}

	// A flow step without a credential passes the gate and is answered by the
	// guard (401), proving the route itself is reachable.
	status, body = getBody(t, server, "/api/v1/auth/flow")
	if status != http.StatusUnauthorized || body["code"] != "unauthenticated" {
		t.Fatalf("GET /api/v1/auth/flow = %d %v, want guard 401 through the open gate", status, body["code"])
	}
}

func TestBootstrapGateStaysClosedForInitializedOperatorOnly(t *testing.T) {
	application, _, _ := newUninitializedAdmissionServer(t)
	// Crafted legacy database: an initialized operator exists, but the
	// deployment-level administrator never completed initialization. Only the
	// admin closes the bootstrap window (auth.IsDeploymentInitialized).
	now := "2026-09-15T00:00:00.000000000Z"
	if _, err := application.db.Exec(`INSERT INTO users(username,display_name,role,enabled,auth_revision,initialized,password_phc,password_change_required,row_version,created_at,updated_at) VALUES('legacy-op','Legacy Operator','operator',1,1,1,'$argon2id$crafted',0,1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	server := newGatedSurface(t, application)

	status, body := getBody(t, server, "/api/v1/maintenance")
	if status != http.StatusServiceUnavailable || body["code"] != "initialization_required" {
		t.Fatalf("initialized operator must not open the gate: %d %v", status, body["code"])
	}
	// The flow surface stays reachable for the recovery path.
	flowStatus, flowBody := getBody(t, server, "/api/v1/auth/flow")
	if flowStatus != http.StatusUnauthorized || flowBody["code"] != "unauthenticated" {
		t.Fatalf("flow surface must stay reachable: %d %v", flowStatus, flowBody["code"])
	}
}

func TestBootstrapGateOpensAfterInitialization(t *testing.T) {
	application, service, sender := newAdmissionTestServer(t)
	server := newGatedSurface(t, application)

	// With initialization complete the gate passes everything to the guard:
	// an authenticated request reaches its handler, an anonymous one is the
	// guard's 401 — never the initialization envelope.
	bearer := admissionLogin(t, service, sender)
	response, err := getWithCookie(server, "/api/v1/maintenance", bearer)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initialized system /maintenance = %d, want 200", response.StatusCode)
	}
	status, body := getBody(t, server, "/api/v1/admin/about")
	if status != http.StatusUnauthorized || body["code"] != "unauthenticated" {
		t.Fatalf("initialized system anonymous /admin/about = %d %v, want guard 401", status, body["code"])
	}
}

func TestBootstrapGateFailsClosedOnProbeError(t *testing.T) {
	application, _, database := newUninitializedAdmissionServer(t)
	server := newGatedSurface(t, application)

	// Break the probe's storage — BOTH pools (writer and read-only), exactly
	// like a storage outage in production: every request — managed or flow —
	// fails closed with the generic envelope; no database detail leaks.
	if err := application.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/maintenance", "/api/v1/auth/flow", "/api/v1/auth/login"} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if path == "/api/v1/auth/login" {
			request.Method = http.MethodPost
		}
		response, err := server.Client().Do(request)
		if err != nil {
			continue // transport error from the closed store is also fail-closed
		}
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatalf("%s: undecodable body: %v", path, err)
		}
		if response.StatusCode != http.StatusServiceUnavailable || body["code"] != "unavailable" {
			t.Fatalf("%s = %d %v, want probe-failure 503 unavailable", path, response.StatusCode, body["code"])
		}
	}
}
