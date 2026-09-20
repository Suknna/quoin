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
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func TestHTTPBootstrapUnlockThenSingleStepLoginAndAudit(t *testing.T) {
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
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	if err := prepareAuthenticationBootstrap(context.Background(), service, dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(initialPasswordPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	initial := strings.TrimSpace(string(raw))
	application := NewAPIServer(service, database.SQL, keyFile)
	if err := application.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	if err := application.configureLoginProviders(nil); err != nil {
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
			if !cleared["__Host-quoin-session"] {
				t.Fatal("logout must explicitly clear the session cookie")
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

	// The public login-channel projection answers before any session exists.
	config := call("GET", "/api/v1/auth/config", nil, 200)
	if local := config["local"].(map[string]any); !local["enabled"].(bool) || !local["visible"].(bool) {
		t.Fatalf("default local projection = %#v", config)
	}

	// The initial random password logs into the restricted session directly.
	login := call("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": initial}, 200)
	if login["completed"] != true {
		t.Fatal("single-step login must complete")
	}
	user := login["user"].(map[string]any)
	if user["passwordChangeRequired"] != true {
		t.Fatalf("bootstrap session must stay restricted: %#v", user)
	}
	me := call("GET", "/api/v1/auth/me", nil, 200)
	if me["passwordChangeRequired"] != true || me["authSource"] != "local" {
		t.Fatalf("restricted me projection = %#v", me)
	}
	// Before the deployment completes initialization even the authenticated
	// bootstrap administrator cannot reach managed routes: the gate answers
	// with the initialization envelope.
	call("GET", "/api/v1/admin/users", nil, 503)

	password := "correct-horse-battery-staple"
	call("PUT", "/api/v1/auth/password", map[string]any{"currentPassword": initial, "newPassword": password}, 204)
	me = call("GET", "/api/v1/auth/me", nil, 200)
	if me["passwordChangeRequired"] != false {
		t.Fatal("the forced change must unlock the session")
	}

	// Logout, then the formal password logs in with a normal session.
	call("POST", "/api/v1/auth/logout", nil, 204)
	call("GET", "/api/v1/auth/me", nil, 401)
	login = call("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": password}, 200)
	if login["completed"] != true {
		t.Fatal("formal login must complete")
	}
	call("GET", "/api/v1/auth/me", nil, 200)

	// Wrong passwords answer 401 and the attempts land in the audit log.
	call("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "wrong password value!"}, 401)
	events := call("GET", "/api/v1/audit-events?limit=100", nil, 200)
	if len(events["items"].([]any)) == 0 {
		t.Fatal("authentication and access audit missing")
	}
	call("POST", "/api/v1/auth/logout", nil, 204)
	call("GET", "/api/v1/auth/me", nil, 401)
}
