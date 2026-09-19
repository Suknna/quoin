package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/test/support"
)

func TestRootKeyRebindAllowsOwnPasswordChangeWithoutRestoreChecklist(t *testing.T) {
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), RuntimeClientCAFile: filepath.Join(root, "secrets", "stele")}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, authService, sender := newScenarioAuth(t, config.DataDirectory, config.RootKeyFile)
	defer database.Close()
	// Real initialization: the formal password the maintenance login proves is
	// set through the initialization flow's password step.
	scenarioInitializeAdmin(t, authService, sender, "original-password-123")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.SQL.Exec(`UPDATE maintenance_state SET active=1,reason='RootKeyRebind',entered_at=?,entered_by_type='system',entered_by_id=0,row_version=row_version+1 WHERE id=1`, now); err != nil {
		t.Fatal(err)
	}
	maintenanceApplication := NewMaintenanceAPIServer(authService, database.SQL, config.RootKeyFile)
	if err := maintenanceApplication.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	handler, err := newMaintenanceHandler(maintenanceApplication, config.PublicOrigin, "RootKeyRebind")
	if err != nil {
		t.Fatal(err)
	}
	cookie := scenarioLoginCookie(t, handler, config.PublicOrigin, "admin", "original-password-123", sender)
	change := httptest.NewRequest(http.MethodPut, "/api/v1/auth/password", bytes.NewBufferString(`{"currentPassword":"original-password-123","newPassword":"root-rebind-password-789"}`))
	change.Header.Set("Content-Type", "application/json")
	change.Header.Set("Origin", config.PublicOrigin)
	change.AddCookie(cookie)
	changeResponse := httptest.NewRecorder()
	handler.ServeHTTP(changeResponse, change)
	if changeResponse.Code != http.StatusNoContent {
		t.Fatalf("RootKeyRebind password change status=%d body=%s", changeResponse.Code, changeResponse.Body.String())
	}
}

func TestMaintenanceHandlerExposesOnlyRecoverySafeRoutes(t *testing.T) {
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), RuntimeClientCAFile: filepath.Join(root, "secrets", "stele")}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, authService, sender := newScenarioAuth(t, config.DataDirectory, config.RootKeyFile)
	defer database.Close()
	scenarioInitializeAdmin(t, authService, sender, "original-password-123")
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
	stateApplication := NewMaintenanceAPIServer(authService, database.SQL, config.RootKeyFile)
	if err := stateApplication.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	handler, err := newMaintenanceHandler(stateApplication, config.PublicOrigin, "Restore")
	if err != nil {
		t.Fatal(err)
	}

	cookie := scenarioLoginCookie(t, handler, config.PublicOrigin, "admin", "original-password-123", sender)
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
	// The new password again only opens a flow: the second-factor login issues
	// the session used for the remaining route checks.
	cookie = scenarioLoginCookie(t, handler, config.PublicOrigin, "admin", "replacement-password-456", sender)
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
	rootRebindApplication := NewMaintenanceAPIServer(authService, database.SQL, config.RootKeyFile)
	if err := rootRebindApplication.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	rootRebindHandler, err := newMaintenanceHandler(rootRebindApplication, config.PublicOrigin, "RootKeyRebind")
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
	if failedAuthenticationResponse.Code != http.StatusServiceUnavailable || !strings.Contains(failedAuthenticationResponse.Body.String(), `"code":"unavailable"`) {
		t.Fatalf("failed-authentication maintenance deny status=%d body=%s", failedAuthenticationResponse.Code, failedAuthenticationResponse.Body.String())
	}
}
