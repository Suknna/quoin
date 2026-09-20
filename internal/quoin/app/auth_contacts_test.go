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

// The profile's contact section reads the masked projection only: a full
// session lists its own receive targets, raw targets never leave the server,
// and anonymous or restricted sessions are rejected before any read.
func TestHTTPListOwnContactsReturnsMaskedProjection(t *testing.T) {
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
	if err := prepareAuthenticationBootstrap(context.Background(), service, dir); err != nil {
		t.Fatal(err)
	}
	initialRaw, err := os.ReadFile(initialPasswordPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	initial := strings.TrimSpace(string(initialRaw))
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

	// Before deployment initialization the bootstrap gate shields every
	// non-flow endpoint with 503; contacts behave like sessions here.
	call("GET", "/api/v1/auth/contacts", nil, 503)

	// Unlock the admin with the initial random password; the restricted
	// session cannot read the full-admission contact list yet.
	password := "correct-horse-battery-staple"
	login := call("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": initial}, 200)
	if login["completed"] != true {
		t.Fatal("bootstrap login must complete")
	}
	// LevelFull stays behind the bootstrap gate until initialization ends.
	call("GET", "/api/v1/auth/contacts", nil, 503)
	call("PUT", "/api/v1/auth/password", map[string]any{"currentPassword": initial, "newPassword": password}, 204)

	// Seed the admin's display contact through the service (the same surface
	// the admin users page drives).
	seed, err := service.LoginWithPassword(context.Background(), "admin", password, "Test on Linux")
	if err != nil {
		t.Fatal(err)
	}
	seedSession, err := service.Authenticate(context.Background(), seed.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.SetUserContacts(context.Background(), seedSession, auth.SetUserContactsInput{
		ClientCommandID: "cmd-seed-contact", Digest: auth.DigestCommand("user.set_contacts", map[string]any{"userId": seedSession.User.ID}),
		UserID: seedSession.User.ID, ExpectedRow: seedSession.User.RowVersion,
		Contacts: []auth.ContactInput{{Channel: "email", Target: "admin@example.test"}},
	}); err != nil {
		t.Fatalf("seed admin contact: %v", err)
	}

	login = call("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": password}, 200)
	if login["completed"] != true {
		t.Fatal("formal login must complete")
	}
	raw := call("GET", "/api/v1/auth/contacts", nil, 200)
	if raw["items"] == nil {
		t.Fatalf("contacts response missing items: %#v", raw)
	}
	if strings.Contains(mustJSON(t, raw), "admin@example.test") {
		t.Fatal("raw contact target leaked in contacts response")
	}
	items := raw["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("contacts len=%d want=1", len(items))
	}
	item := items[0].(map[string]any)
	if item["channel"] != "email" || item["maskedTarget"] != "a***@example.test" {
		t.Fatalf("unexpected masked contact %#v", item)
	}

	// After logout the session is gone: anonymous reads are rejected with 401.
	call("POST", "/api/v1/auth/logout", nil, 204)
	call("GET", "/api/v1/auth/contacts", nil, 401)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
