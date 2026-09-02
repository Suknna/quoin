package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func TestRootKeyRebindAllowsOwnPasswordChangeWithoutRestoreChecklist(t *testing.T) {
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), SteleServiceTokenFile: filepath.Join(root, "secrets", "stele")}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authService.CreateFirstAdmin(context.Background(), "admin", "Root Key Admin", "original-password-123"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.SQL.Exec(`UPDATE maintenance_state SET active=1,reason='RootKeyRebind',entered_at=?,entered_by_type='system',entered_by_id=0,row_version=row_version+1 WHERE id=1`, now); err != nil {
		t.Fatal(err)
	}
	handler, err := newMaintenanceHandler(NewMaintenanceAPIServer(authService, database.SQL, ""), config.PublicOrigin, "RootKeyRebind")
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"admin","password":"original-password-123"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", config.PublicOrigin)
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	change := httptest.NewRequest(http.MethodPut, "/api/v1/auth/password", bytes.NewBufferString(`{"currentPassword":"original-password-123","newPassword":"root-rebind-password-789"}`))
	change.Header.Set("Content-Type", "application/json")
	change.Header.Set("Origin", config.PublicOrigin)
	change.AddCookie(loginResponse.Result().Cookies()[0])
	changeResponse := httptest.NewRecorder()
	handler.ServeHTTP(changeResponse, change)
	if changeResponse.Code != http.StatusNoContent {
		t.Fatalf("RootKeyRebind password change status=%d body=%s", changeResponse.Code, changeResponse.Body.String())
	}
}

func TestMaintenanceHandlerExposesOnlyRecoverySafeRoutes(t *testing.T) {
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), SteleServiceTokenFile: filepath.Join(root, "secrets", "stele")}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authService.CreateFirstAdmin(context.Background(), "admin", "Restore Admin", "original-password-123"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.SQL.Exec(`UPDATE maintenance_state SET active=1,reason='Restore',entered_at=?,entered_by_type='system',entered_by_id=0,row_version=row_version+1 WHERE id=1`, now); err != nil {
		t.Fatal(err)
	}
	var maintenanceRevision int64
	if err := database.SQL.QueryRow(`SELECT row_version FROM maintenance_state WHERE id=1`).Scan(&maintenanceRevision); err != nil {
		t.Fatal(err)
	}
	var adminID int64
	if err := database.SQL.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?, 'AdminPassword', ?, 'Blocking', 'temporary_password_change_required', ?)`, maintenanceRevision, strconv.FormatInt(adminID, 10), now); err != nil {
		t.Fatal(err)
	}
	handler, err := NewMaintenanceHandler(authService, database.SQL, config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}

	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"admin","password":"original-password-123"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", config.PublicOrigin)
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	cookie := loginResponse.Result().Cookies()[0]
	state := httptest.NewRequest(http.MethodGet, "/api/v1/maintenance", nil)
	state.AddCookie(cookie)
	stateResponse := httptest.NewRecorder()
	handler.ServeHTTP(stateResponse, state)
	if stateResponse.Code != http.StatusOK {
		t.Fatalf("state status=%d body=%s", stateResponse.Code, stateResponse.Body.String())
	}
	var stateBody map[string]any
	if err := json.Unmarshal(stateResponse.Body.Bytes(), &stateBody); err != nil {
		t.Fatal(err)
	}
	if _, present := stateBody["rowVersion"]; !present {
		t.Fatalf("maintenance response lacks OpenAPI rowVersion: %s", stateResponse.Body.String())
	}
	if _, leaked := stateBody["RowVersion"]; leaked {
		t.Fatalf("maintenance response leaked Go field names: %s", stateResponse.Body.String())
	}
	change := httptest.NewRequest(http.MethodPut, "/api/v1/auth/password", bytes.NewBufferString(`{"currentPassword":"original-password-123","newPassword":"replacement-password-456"}`))
	change.Header.Set("Content-Type", "application/json")
	change.Header.Set("Origin", config.PublicOrigin)
	change.AddCookie(cookie)
	changeResponse := httptest.NewRecorder()
	handler.ServeHTTP(changeResponse, change)
	if changeResponse.Code != http.StatusNoContent {
		t.Fatalf("password change status=%d body=%s", changeResponse.Code, changeResponse.Body.String())
	}
	relogin := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"admin","password":"replacement-password-456"}`))
	relogin.Header.Set("Content-Type", "application/json")
	relogin.Header.Set("Origin", config.PublicOrigin)
	reloginResponse := httptest.NewRecorder()
	handler.ServeHTTP(reloginResponse, relogin)
	if reloginResponse.Code != http.StatusOK {
		t.Fatalf("relogin status=%d body=%s", reloginResponse.Code, reloginResponse.Body.String())
	}
	cookie = reloginResponse.Result().Cookies()[0]
	runtimeRequest := httptest.NewRequest(http.MethodGet, "/api/v1/runtime", nil)
	runtimeRequest.AddCookie(cookie)
	runtimeResponse := httptest.NewRecorder()
	handler.ServeHTTP(runtimeResponse, runtimeRequest)
	if runtimeResponse.Code != http.StatusOK {
		t.Fatalf("runtime status=%d body=%s", runtimeResponse.Code, runtimeResponse.Body.String())
	}
	auditRequest := httptest.NewRequest(http.MethodGet, "/api/v1/audit-events", nil)
	auditRequest.AddCookie(cookie)
	auditResponse := httptest.NewRecorder()
	handler.ServeHTTP(auditResponse, auditRequest)
	if auditResponse.Code != http.StatusOK {
		t.Fatalf("audit status=%d body=%s", auditResponse.Code, auditResponse.Body.String())
	}
	unauthenticated := httptest.NewRequest(http.MethodGet, "/api/v1/backups", nil)
	unauthenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized || !strings.Contains(unauthenticatedResponse.Header().Get("Content-Type"), "application/problem+json") || !strings.Contains(unauthenticatedResponse.Body.String(), `"code":"unauthenticated"`) {
		t.Fatalf("unauthenticated maintenance deny status=%d contentType=%q body=%s", unauthenticatedResponse.Code, unauthenticatedResponse.Header().Get("Content-Type"), unauthenticatedResponse.Body.String())
	}
	invalidSession := httptest.NewRequest(http.MethodGet, "/api/v1/backups", nil)
	invalidSession.AddCookie(&http.Cookie{Name: "__Host-quoin-session", Value: "missing-session"})
	invalidSessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidSessionResponse, invalidSession)
	if invalidSessionResponse.Code != http.StatusUnauthorized || !strings.Contains(invalidSessionResponse.Body.String(), `"code":"unauthenticated"`) {
		t.Fatalf("invalid-session maintenance deny status=%d body=%s", invalidSessionResponse.Code, invalidSessionResponse.Body.String())
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/alerts"},
		{http.MethodGet, "/api/v1/backups"},
		{http.MethodGet, "/api/v1/investigations"},
		{http.MethodPost, "/api/v1/connections"},
		{http.MethodPost, "/api/v1/browser-login/example/operations"},
	} {
		request := httptest.NewRequest(route.method, route.path, nil)
		request.Header.Set("Origin", config.PublicOrigin)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Header().Get("Content-Type"), "application/problem+json") || !strings.Contains(response.Body.String(), `"code":"unavailable"`) {
			t.Errorf("%s %s status=%d contentType=%q body=%s, want 503 unavailable problem", route.method, route.path, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/users"},
		{http.MethodGet, "/api/v1/connections"},
		{http.MethodGet, "/api/v1/alert-sources"},
	} {
		request := httptest.NewRequest(route.method, route.path, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Errorf("%s %s status=%d body=%s, want OpenAPI trust-rebuild allow", route.method, route.path, response.Code, response.Body.String())
		}
	}
	rootRebindHandler, err := newMaintenanceHandler(NewMaintenanceAPIServer(authService, database.SQL, ""), config.PublicOrigin, "RootKeyRebind")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/users"},
		{http.MethodGet, "/api/v1/runtime"},
		{http.MethodGet, "/api/v1/alert-sources"},
	} {
		request := httptest.NewRequest(route.method, route.path, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		rootRebindHandler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"unavailable"`) {
			t.Errorf("RootKeyRebind %s %s status=%d body=%s, want 503 unavailable", route.method, route.path, response.Code, response.Body.String())
		}
	}
	rootRebindAudit := httptest.NewRequest(http.MethodGet, "/api/v1/audit-events", nil)
	rootRebindAudit.AddCookie(cookie)
	rootRebindAuditResponse := httptest.NewRecorder()
	rootRebindHandler.ServeHTTP(rootRebindAuditResponse, rootRebindAudit)
	if rootRebindAuditResponse.Code != http.StatusOK {
		t.Errorf("RootKeyRebind audit list status=%d body=%s, want allowed", rootRebindAuditResponse.Code, rootRebindAuditResponse.Body.String())
	}
	rootRebindConnection := httptest.NewRequest(http.MethodGet, "/api/v1/connections", nil)
	rootRebindConnection.AddCookie(cookie)
	rootRebindConnectionResponse := httptest.NewRecorder()
	rootRebindHandler.ServeHTTP(rootRebindConnectionResponse, rootRebindConnection)
	if rootRebindConnectionResponse.Code != http.StatusOK {
		t.Errorf("RootKeyRebind connection list status=%d body=%s, want allowed", rootRebindConnectionResponse.Code, rootRebindConnectionResponse.Body.String())
	}
	missingCredentials := httptest.NewRequest(http.MethodGet, "/api/v1/alert-sources/missing/credentials", nil)
	missingCredentials.AddCookie(cookie)
	missingCredentialsResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingCredentialsResponse, missingCredentials)
	if missingCredentialsResponse.Code != http.StatusNotFound {
		t.Errorf("missing source credentials status=%d body=%s, want 404", missingCredentialsResponse.Code, missingCredentialsResponse.Body.String())
	}
	head := httptest.NewRequest(http.MethodHead, "/api/v1/backups", nil)
	head.AddCookie(cookie)
	headResponse := httptest.NewRecorder()
	handler.ServeHTTP(headResponse, head)
	if headResponse.Code != http.StatusServiceUnavailable {
		t.Errorf("HEAD maintenance deny status=%d, want 503", headResponse.Code)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	failedAuthentication := httptest.NewRequest(http.MethodGet, "/api/v1/backups", nil)
	failedAuthentication.AddCookie(cookie)
	failedAuthenticationResponse := httptest.NewRecorder()
	handler.ServeHTTP(failedAuthenticationResponse, failedAuthentication)
	if failedAuthenticationResponse.Code != http.StatusInternalServerError || !strings.Contains(failedAuthenticationResponse.Body.String(), `"code":"unavailable"`) {
		t.Fatalf("failed-authentication maintenance deny status=%d body=%s", failedAuthenticationResponse.Code, failedAuthenticationResponse.Body.String())
	}
}
