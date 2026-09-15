package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func TestHTTPInitializationThenTwoStepLoginAndAudit(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), dir, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
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
	if err := service.ConfigureAuth(auth.AuthConfig{OTPKey: bytes.Repeat([]byte{9}, 32), Sender: sender}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBootstrapAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	application := NewAPIServer(service, database.SQL, keyFile)
	if err := application.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	server.StartTLS()
	defer server.Close()
	handler, err := NewHandler(application, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	call := func(method, path string, body any, status int) map[string]any {
		t.Helper()
		var input io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			input = bytes.NewReader(encoded)
		}
		req, err := http.NewRequest(method, server.URL+path, input)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", server.URL)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if path == "/api/v1/auth/logout" && response.StatusCode == http.StatusNoContent {
			cleared := map[string]bool{}
			for _, cookie := range response.Cookies() {
				if cookie.MaxAge < 0 && cookie.Value == "" {
					cleared[cookie.Name] = true
				}
			}
			if !cleared["__Host-quoin-session"] || !cleared[flowCookieName] {
				t.Fatal("logout must explicitly clear both session and authentication flow cookies")
			}
		}

		encoded, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, path, response.StatusCode, status, encoded)
		}
		var result map[string]any
		if len(encoded) > 0 {
			if err := json.Unmarshal(encoded, &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	flow := call("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "admin"}, 200)
	if flow["type"] != "admin_initialize" {
		t.Fatalf("unexpected flow %#v", flow)
	}
	call("GET", "/api/v1/auth/me", nil, 401)
	password := "correct-horse-battery-staple"
	call("PUT", "/api/v1/auth/flow/password", map[string]any{"newPassword": password}, 204)
	contact := call("POST", "/api/v1/auth/flow/contacts", map[string]any{"channel": "email", "target": "admin@example.test"}, 200)
	call("POST", "/api/v1/auth/flow/challenge", map[string]any{"contactId": contact["id"]}, 200)
	verified := call("POST", "/api/v1/auth/flow/verify", map[string]any{"code": sender.lastCode()}, 200)
	if verified["completed"] != false {
		t.Fatal("initialization unexpectedly logged in")
	}
	call("POST", "/api/v1/auth/flow/complete", map[string]any{}, 204)
	call("GET", "/api/v1/auth/me", nil, 401)
	flow = call("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": password}, 200)
	if flow["type"] != "login" {
		t.Fatal("expected second-factor login")
	}
	call("GET", "/api/v1/auth/me", nil, 401)
	contacts := flow["contacts"].([]any)
	contactID := contacts[0].(map[string]any)["id"]
	call("POST", "/api/v1/auth/flow/challenge", map[string]any{"contactId": contactID}, 200)
	verified = call("POST", "/api/v1/auth/flow/verify", map[string]any{"code": sender.lastCode()}, 200)
	if verified["completed"] != true {
		t.Fatal("login did not complete")
	}
	call("GET", "/api/v1/auth/me", nil, 200)
	events := call("GET", "/api/v1/audit-events?limit=100", nil, 200)
	if len(events["items"].([]any)) == 0 {
		t.Fatal("authentication and access audit missing")
	}
	call("POST", "/api/v1/auth/logout", nil, 204)
	call("GET", "/api/v1/auth/me", nil, 401)
}
