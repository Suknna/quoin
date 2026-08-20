package app_test

// HTTP-level coverage for the T05 admin surface over the real Huma handler:
// the authorization matrix (admin/operator/restricted session), the frozen
// problem+json envelope with the conflict block, ledger replay through the
// real transport, and revoked-session rejection on subsequent requests.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func newAdminSurface(t *testing.T) (*httptest.Server, map[string]string) {
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
	t.Cleanup(func() { database.Close() })
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	const temporary = "Correct horse battery staple 2026!"
	if _, err := service.CreateFirstAdmin(ctx, "admin", "Quoin Admin", temporary); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mustHandler(t, service, database.SQL, config.PublicOrigin))
	t.Cleanup(server.Close)
	origin := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"}
	login := mustPost(t, server, origin, `/api/v1/auth/login`, `{"username":"admin","password":"`+temporary+`"}`, http.StatusOK)
	adminCookie := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json", "Cookie": splitCookie(login.headers.Get("Set-Cookie"))}
	// Change the temp password so the admin session is unrestricted.
	mustDo(t, server, http.MethodPut, adminCookie, `/api/v1/auth/password`, `{"currentPassword":"`+temporary+`","newPassword":"A private admin passphrase 2027!"}`, http.StatusNoContent)
	return server, adminCookie
}

func TestAdminUserAuthorizationMatrix(t *testing.T) {
	server, admin := newAdminSurface(t)

	// Operator: can read audit, cannot touch user management.
	mustPost(t, server, admin, `/api/v1/admin/users`, `{"clientCommandId":"authz-create-01","username":"op1","displayName":"Operator One","role":"operator","password":"Operator one passphrase 2026!"}`, http.StatusCreated)
	login := mustPost(t, server, map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json"}, `/api/v1/auth/login`, `{"username":"op1","password":"Operator one passphrase 2026!"}`, http.StatusOK)
	operator := map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json", "Cookie": splitCookie(login.headers.Get("Set-Cookie"))}

	listForbidden := mustRequest(t, server, operator, `/api/v1/admin/users`, http.StatusForbidden)
	if !strings.Contains(listForbidden, "forbidden") {
		t.Fatalf("operator listUsers must be forbidden with code, got %s", listForbidden)
	}
	denied := mustPost(t, server, operator, `/api/v1/admin/users`, `{"clientCommandId":"authz-create-02","username":"op2","displayName":"Two","role":"operator","password":"Another operator passphrase 2027!"}`, http.StatusForbidden)
	if !strings.Contains(denied.body, "需要管理员权限") {
		t.Fatalf("operator write must be forbidden with a human message, got %s", denied)
	}
	// Every logged-in user reads audit events (HTTP-PERM-001).
	audit := mustRequest(t, server, operator, `/api/v1/audit-events?action=user.create`, http.StatusOK)
	if !strings.Contains(audit, `"user.create"`) {
		t.Fatalf("operator must read audit events, got %s", audit)
	}
	// Own sessions visible to the operator with the current marker.
	sessions := mustRequest(t, server, operator, `/api/v1/auth/sessions`, http.StatusOK)
	if !strings.Contains(sessions, `"current":true`) {
		t.Fatalf("own sessions must mark the current session, got %s", sessions)
	}

	// Restricted session: a reset target logs in with the temporary password
	// and every admin/session surface answers 403 password_change_required.
	users := mustRequest(t, server, admin, `/api/v1/admin/users`, http.StatusOK)
	var list struct {
		Items []struct {
			ID         string `json:"id"`
			RowVersion int64  `json:"rowVersion"`
			Username   string `json:"username"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(users), &list); err != nil {
		t.Fatal(err)
	}
	var op1 string
	var op1Row int64
	for _, item := range list.Items {
		if item.Username == "op1" {
			op1, op1Row = item.ID, item.RowVersion
		}
	}
	if op1 == "" {
		t.Fatalf("op1 missing from user list: %s", users)
	}
	reset := mustPost(t, server, admin, `/api/v1/admin/users/`+op1+`/reset-password`, `{"clientCommandId":"authz-reset-01","expectedRowVersion":`+itoa(op1Row)+`,"newPassword":"Replacement passphrase 2027!"}`, http.StatusOK)
	if !strings.Contains(reset.body, `"revokedSessionCount":1`) {
		t.Fatalf("reset must report the revoked session count, got %s", reset)
	}
	relogin := mustPost(t, server, map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json"}, `/api/v1/auth/login`, `{"username":"op1","password":"Replacement passphrase 2027!"}`, http.StatusOK)
	restricted := map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json", "Cookie": splitCookie(relogin.headers.Get("Set-Cookie"))}
	for _, path := range []string{`/api/v1/admin/users`, `/api/v1/auth/sessions`, `/api/v1/audit-events`} {
		body := mustRequest(t, server, restricted, path, http.StatusForbidden)
		if !strings.Contains(body, "password_change_required") {
			t.Fatalf("restricted session must be told to change the password first (%s): %s", path, body)
		}
	}

	// Revoked session: admin revokes the operator's sessions; the very next
	// authenticated request fails with 401.
	stillLoggedIn := mustPost(t, server, map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json"}, `/api/v1/auth/login`, `{"username":"op1","password":"Replacement passphrase 2027!"}`, http.StatusOK)
	// Complete the forced change so the session is unrestricted.
	stillCookie := map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json", "Cookie": splitCookie(stillLoggedIn.headers.Get("Set-Cookie"))}
	mustDo(t, server, http.MethodPut, stillCookie, `/api/v1/auth/password`, `{"currentPassword":"Replacement passphrase 2027!","newPassword":"Final operator passphrase 2028!"}`, http.StatusNoContent)
	usersAfter := mustRequest(t, server, admin, `/api/v1/admin/users`, http.StatusOK)
	if err := json.Unmarshal([]byte(usersAfter), &list); err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if item.Username == "op1" {
			op1, op1Row = item.ID, item.RowVersion
		}
	}
	mustPost(t, server, admin, `/api/v1/admin/users/`+op1+`/revoke-sessions`, `{"clientCommandId":"authz-revoke-01"}`, http.StatusOK)
	rejected := manualGet(t, server, stillCookie, `/api/v1/auth/sessions`)
	if rejected != http.StatusUnauthorized {
		t.Fatalf("revoked session must be rejected with 401, got %d", rejected)
	}
}

func manualGet(t *testing.T, server *httptest.Server, headers map[string]string, path string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func TestAdminUserCommandReplayOverHTTP(t *testing.T) {
	server, admin := newAdminSurface(t)
	command := `{"clientCommandId":"replay-create-01","username":"op9","displayName":"Nine","role":"operator","password":"Operator nine passphrase 2026!"}`
	first := mustPost(t, server, admin, `/api/v1/admin/users`, command, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(first.body), &created); err != nil {
		t.Fatal(err)
	}
	second := mustPost(t, server, admin, `/api/v1/admin/users`, command, http.StatusCreated)
	if first.body != second.body {
		t.Fatalf("replay must return the identical body:\nfirst=%s\nsecond=%s", first.body, second.body)
	}
	if duplicate := mustPost(t, server, admin, `/api/v1/admin/users`, `{"clientCommandId":"replay-create-02","username":"op9","displayName":"Nine","role":"operator","password":"Operator nine passphrase 2026!"}`, http.StatusConflict); !strings.Contains(duplicate.body, "用户名已存在") {
		t.Fatalf("duplicate username must conflict with a human message, got %s", duplicate.body)
	}
	reused := mustPost(t, server, admin, `/api/v1/admin/users`, `{"clientCommandId":"replay-create-01","username":"op9","displayName":"Renamed","role":"operator","password":"Operator nine passphrase 2026!"}`, http.StatusConflict)
	if !strings.Contains(reused.body, `"conflict":{"code":"command_id_reused"}`) && !strings.Contains(reused.body, `"code":"command_id_reused"`) {
		t.Fatalf("same key with a different request must reuse-conflict, got %s", reused.body)
	}
	// Row-version conflict carries the authoritative version in the envelope.
	mustDo(t, server, http.MethodPatch, admin, `/api/v1/admin/users/`+created.ID, `{"clientCommandId":"replay-update-01","expectedRowVersion":1,"displayName":"Nine Renamed"}`, http.StatusOK)
	stale := mustDo(t, server, http.MethodPatch, admin, `/api/v1/admin/users/`+created.ID, `{"clientCommandId":"replay-update-02","expectedRowVersion":1,"displayName":"Stale Name"}`, http.StatusConflict)
	if !strings.Contains(stale.body, `"rowVersion":`) {
		t.Fatalf("row-version conflict must carry the current version, got %s", stale.body)
	}
	// The frozen problem envelope fields are present.
	var problem struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
		Conflict  struct {
			Code       string `json:"code"`
			RowVersion int64  `json:"rowVersion"`
		} `json:"conflict"`
	}
	if err := json.Unmarshal([]byte(stale.body), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "row_version_conflict" || problem.Conflict.Code != "row_version_conflict" || problem.Conflict.RowVersion == 0 {
		t.Fatalf("frozen envelope mismatch: %+v", problem)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
