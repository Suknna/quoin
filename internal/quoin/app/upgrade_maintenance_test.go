package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/upgrade"
)

// upgradeHTTPFixture builds the real normal-mode surface with the live gate.
func upgradeHTTPFixture(t *testing.T) (*apiServer, http.Handler, *contract.QuoinConfig, string) {
	t.Helper()
	root := t.TempDir()
	config := &contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), SteleServiceTokenFile: filepath.Join(root, "secrets", "stele")}
	if _, err := bootstrap.BootstrapSecrets(*config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authService.CreateFirstAdmin(context.Background(), "admin", "Upgrade Admin", "original-password-123"); err != nil {
		t.Fatal(err)
	}
	application := NewAPIServer(authService, database.SQL, "")
	handler, err := NewHandler(application, config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	gate := newUpgradeGate(handler)
	application.upgradeGate = gate
	application.onUpgradeMaintenanceEntered = func() { application.enterUpgradeMaintenance(config.PublicOrigin) }
	application.onUpgradeMaintenanceExit = application.exitUpgradeMaintenance
	application.setReadiness = func(sharedops.Readiness) {}
	reconciler := upgrade.NewReconciler(database.SQL, upgradeTestingBackups{})
	application.upgradeReconciler = reconciler
	// The first login carries a mandatory password change; complete it so
	// the fixture admin acts as a fully-initialized operator.
	firstCookie := upgradeLogin(t, handler, config, "original-password-123")
	formal := upgradeRequest(t, handler, config, firstCookie, http.MethodPut, "/api/v1/auth/password", `{"currentPassword":"original-password-123","newPassword":"formal-password-456"}`)
	if formal.Code != http.StatusNoContent {
		t.Fatalf("password change status=%d body=%s", formal.Code, formal.Body.String())
	}
	return application, gate, config, "formal-password-456"
}

type upgradeTestingBackups struct{}

func (upgradeTestingBackups) RunUpgrade(ctx context.Context, id int64) error { return nil }

func upgradeLogin(t *testing.T, handler http.Handler, config *contract.QuoinConfig, password string) *http.Cookie {
	t.Helper()
	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(fmt.Sprintf(`{"username":"admin","password":%q}`, password)))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", config.PublicOrigin)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, login)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	return recorder.Result().Cookies()[0]
}

func upgradeRequest(t *testing.T, handler http.Handler, config *contract.QuoinConfig, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", config.PublicOrigin)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestUpgradeGateSwapsLiveSurfaceAndDrainsThroughAllowlist proves the whole
// live transition: prepareUpgrade swaps the public surface, ordinary
// operations answer 503 while the deterministic drain cancels stay open, and
// exitMaintenance restores the normal surface.
func TestUpgradeGateSwapsLiveSurfaceAndDrainsThroughAllowlist(t *testing.T) {
	application, handler, config, password := upgradeHTTPFixture(t)
	cookie := upgradeLogin(t, handler, config, password)
	// One active investigation attempt is drainable work.
	investigation, err := application.db.Exec(`INSERT INTO investigations(created_at) VALUES(?)`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	investigationID, _ := investigation.LastInsertId()
	attempt, err := application.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('investigation','investigation',?,'Queued','v1-test',?)`, investigationID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	attemptID, _ := attempt.LastInsertId()

	prepared := upgradeRequest(t, handler, config, cookie, http.MethodPost, "/api/v1/maintenance/upgrade/prepare", `{"clientCommandId":"t36-live-prepare-1","expectedRowVersion":1}`)
	if prepared.Code != http.StatusAccepted {
		t.Fatalf("prepare status=%d body=%s", prepared.Code, prepared.Body.String())
	}
	var state maintenanceStateResponse
	if err := json.Unmarshal(prepared.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Active || state.Reason != "Upgrade" || state.RowVersion != 2 {
		t.Fatalf("prepared state=%+v", state)
	}
	// The checklist carries the drainable attempt with its cancel locator.
	found := false
	for _, item := range state.Items {
		if item.Kind == "ActiveAttempt" && item.ObjectKey == fmt.Sprintf("attempt/%d", attemptID) {
			found = true
			want := fmt.Sprintf("queued|cancel:investigation:%d/%d:1", investigationID, attemptID)
			if item.DetailCode != want {
				t.Fatalf("drain detail=%q want %q", item.DetailCode, want)
			}
		}
	}
	if !found {
		t.Fatalf("checklist missing the drainable attempt: %+v", state.Items)
	}
	// Ordinary product work is denied on the swapped surface...
	denied := upgradeRequest(t, handler, config, cookie, http.MethodGet, "/api/v1/alerts", "")
	if denied.Code != http.StatusServiceUnavailable {
		t.Fatalf("ordinary work status=%d body=%s", denied.Code, denied.Body.String())
	}
	// ...while the frozen drain cancel stays the one open write path.
	cancel := upgradeRequest(t, handler, config, cookie, http.MethodPost, fmt.Sprintf("/api/v1/investigations/%d/attempts/%d/cancel", investigationID, attemptID), `{"clientCommandId":"t36-live-cancel-1","expectedRowVersion":1}`)
	if cancel.Code != http.StatusOK {
		t.Fatalf("drain cancel status=%d body=%s", cancel.Code, cancel.Body.String())
	}
	// The reconciliation observes the terminal attempt.
	if _, err := application.upgradeReconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A prepared operator aborts the upgrade: exit restores the normal
	// surface. The BackupPreflight is still Blocking, so the exit first
	// marks the checklist complete the way the reconciler would after a
	// verified backup.
	if _, err := application.db.Exec(`UPDATE maintenance_items SET safe_state='Safe',detail_code='backup_verified' WHERE maintenance_revision=2`); err != nil {
		t.Fatal(err)
	}
	exited := upgradeRequest(t, handler, config, cookie, http.MethodPost, "/api/v1/maintenance/exit", `{"clientCommandId":"t36-live-exit-1","expectedRowVersion":2,"expectedReason":"Upgrade"}`)
	if exited.Code != http.StatusOK {
		t.Fatalf("exit status=%d body=%s", exited.Code, exited.Body.String())
	}
	restored := upgradeRequest(t, handler, config, cookie, http.MethodGet, "/api/v1/alerts", "")
	if restored.Code == http.StatusServiceUnavailable {
		t.Fatalf("normal surface not restored: %s", restored.Body.String())
	}
}
