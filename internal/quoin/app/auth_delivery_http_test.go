package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// deliveryTestServer boots the real handler over TLS with a seeded pending
// bootstrap administrator that already completed the password step, and
// returns the server plus a JSON request helper bound to its cookie jar.
func deliveryTestServer(t *testing.T) (*httptest.Server, func(method, path string, body any) (*http.Response, map[string]any)) {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{5}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), dir, keyFile)
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
	if _, err := service.EnsureBootstrapAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	application := NewAPIServer(service, database.SQL, keyFile)
	if err := application.configureReadOnly(database.Reader); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	server.StartTLS()
	t.Cleanup(server.Close)
	handler, err := NewHandler(application, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler

	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	do := func(method, path string, body any) (*http.Response, map[string]any) {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(encoded)
		} else {
			reader = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, server.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", server.URL)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var decoded map[string]any
		_ = json.NewDecoder(response.Body).Decode(&decoded)
		return response, decoded
	}
	// The pending administrator enters the unified initialization flow and
	// completes its password step; the OTP steps are irrelevant here. Tests
	// that need an unauthenticated jar skip startAdminFlow.
	return server, do
}

// startAdminFlow opens the unified initialization flow and completes the
// password step, leaving the flow cookie in the jar.
func startAdminFlow(t *testing.T, do func(method, path string, body any) (*http.Response, map[string]any)) {
	t.Helper()
	response, _ := do(http.MethodPost, "/api/v1/auth/login", map[string]any{"username": "admin", "password": "admin"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d", response.StatusCode)
	}
	if response, _ = do(http.MethodPut, "/api/v1/auth/flow/password", map[string]any{"newPassword": "formal-password-16ch"}); response.StatusCode != http.StatusNoContent {
		t.Fatalf("set password status=%d", response.StatusCode)
	}
}

// validDeliveryConfiguration is a minimal configuration that passes the
// server-side validation (one SMTP email channel, no secret references).
func validDeliveryConfiguration() map[string]any {
	return map[string]any{
		"email": map[string]any{
			"kind": "smtp", "host": "smtp.example.com", "port": 587,
			"from": "noreply@quoin.example.com", "tlsMode": "starttls",
		},
	}
}

// Cookies are scoped by host, not port: a stale session cookie left in the
// browser must not lock a valid initialization flow out of the delivery
// settings. The flow credential, whenever present, wins over any session —
// proven with the real authenticated PUT and its subsequent GET.
func TestDeliveryFlowWinsOverStaleSessionCookie(t *testing.T) {
	server, do := deliveryTestServer(t)
	startAdminFlow(t, do)
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.Client().Jar.SetCookies(origin, []*http.Cookie{{Name: "__Host-quoin-session", Value: "stale-token-from-another-port"}})
	response, body := do(http.MethodGet, "/api/v1/auth/flow/delivery", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initial delivery read status=%d body=%v", response.StatusCode, body)
	}
	version, ok := body["rowVersion"].(float64)
	if !ok || version != 0 {
		t.Fatalf("unconfigured deployment must report rowVersion 0, got %v", body)
	}

	// Inject the stale artifact the browser can hold: an invalid session
	// cookie alongside the valid flow cookie. The PUT must go through the
	// flow credential, not fail on the session.
	response, body = do(http.MethodPut, "/api/v1/auth/flow/delivery", map[string]any{
		"configuration":      validDeliveryConfiguration(),
		"expectedRowVersion": 0,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("configure with stale session cookie status=%d body=%v", response.StatusCode, body)
	}
	if configured, _ := body["configured"].(bool); !configured {
		t.Fatalf("the PUT must report the configured state: %v", body)
	}
	if version, _ := body["rowVersion"].(float64); version != 1 {
		t.Fatalf("the PUT must advance the row version, got %v", body)
	}

	// The same stale-cookie combination reads back the persisted state.
	response, body = do(http.MethodGet, "/api/v1/auth/flow/delivery", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delivery read after configure status=%d body=%v", response.StatusCode, body)
	}
	if configured, _ := body["configured"].(bool); !configured {
		t.Fatalf("the GET must observe the persisted configuration: %v", body)
	}
	if version, _ := body["rowVersion"].(float64); version != 1 {
		t.Fatalf("the GET must observe the advanced row version: %v", body)
	}
	if source, _ := body["source"].(string); source != "administrator" {
		t.Fatalf("the flow-configured delivery must be administrator-owned: %v", body)
	}
}

// The in-transaction re-proof binds the credential digest to the acting
// administrator: an unresolvable flow cookie (expired, foreign or garbage)
// fails closed on the write even before the row version matters, and no
// session fallback can rescue it.
func TestDeliveryCredentialIdentityFailClosed(t *testing.T) {
	server, do := deliveryTestServer(t)
	_ = server
	// No login happened: the jar holds no credential at all.
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Garbage flow credential plus no session: denied (the admission guard
	// answers 401 before the pane's 403; both fail closed).
	if response, body := do(http.MethodPut, "/api/v1/auth/flow/delivery", map[string]any{
		"configuration":      validDeliveryConfiguration(),
		"expectedRowVersion": 0,
	}); response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("configure without any credential must be denied, got %d body=%v", response.StatusCode, body)
	}

	// A flow credential that exists for nobody must not configure either.
	server.Client().Jar.SetCookies(origin, []*http.Cookie{{Name: "__Host-quoin-flow", Value: "foreign-flow-bearer-from-another-deployment"}})
	if response, body := do(http.MethodPut, "/api/v1/auth/flow/delivery", map[string]any{
		"configuration":      validDeliveryConfiguration(),
		"expectedRowVersion": 0,
	}); response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("configure with a foreign flow credential must be denied, got %d body=%v", response.StatusCode, body)
	}
	// Reads fail closed on the same terms.
	if response, _ := do(http.MethodGet, "/api/v1/auth/flow/delivery", nil); response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("read with a foreign flow credential must be denied, got %d", response.StatusCode)
	}
}
