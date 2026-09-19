package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/upgrade"
	"github.com/Suknna/quoin/test/support"
)

// upgradeHTTPFixture builds the real normal-mode surface with the live gate.
// The administrator is initialized through the real flow with the formal
// password, so the fixture login is the real two-step login.

func upgradeHTTPFixture(t *testing.T) (*apiServer, http.Handler, *contract.QuoinConfig, string, *stubSender) {
	t.Helper()
	root := t.TempDir()
	config := &contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), RuntimeClientCAFile: filepath.Join(root, "secrets", "stele")}
	if err := support.GenerateDeploymentSecrets(*config); err != nil {
		t.Fatal(err)
	}
	database, authService, sender := newScenarioAuth(t, config.DataDirectory, config.RootKeyFile)
	t.Cleanup(func() { database.Close() })
	// The initialization flow's password step sets the formal password
	// directly; the old temporary-password forced-change detour is gone.
	const formal = "formal-password-456"
	scenarioInitializeAdmin(t, authService, sender, formal)
	application := NewAPIServer(authService, database.SQL, "")
	if err := application.SetReadOnlyReader(database.Reader); err != nil {
		t.Fatal(err)
	}
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
	return application, gate, config, formal, sender
}

type upgradeTestingBackups struct{}

func (upgradeTestingBackups) RunUpgrade(ctx context.Context, id int64) error { return nil }

func upgradeLogin(t *testing.T, handler http.Handler, config *contract.QuoinConfig, password string, sender *stubSender) *http.Cookie {
	t.Helper()
	return scenarioLoginCookie(t, handler, config.PublicOrigin, "admin", password, sender)
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
	application, handler, config, password, sender := upgradeHTTPFixture(t)
	cookie := upgradeLogin(t, handler, config, password, sender)
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

// upgradeMaintenanceFixture boots the real restart-inside-Upgrade shape the
// production app.Run checks before startUpgradeMaintenanceRuntime: canonical
// secrets, the WAL database with its shared read-only pool, configureReadOnly
// and an active Upgrade maintenance row.
func upgradeMaintenanceFixture(t *testing.T) (context.Context, context.CancelFunc, *apiServer, *bootstrap.Database, contract.QuoinConfig) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), RuntimeClientCAFile: filepath.Join(root, "secrets", "stele")}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.SQL.Exec(`UPDATE maintenance_state SET active=1,reason='Upgrade',entered_at=?,entered_by_type='system',entered_by_id=0,row_version=row_version+1 WHERE id=1`, now); err != nil {
		t.Fatal(err)
	}
	application := NewMaintenanceAPIServer(authService, database.SQL, config.RootKeyFile)
	if err := application.configureReadOnly(database.Reader); err != nil {
		t.Fatal(err)
	}
	return ctx, stop, application, database, config
}

// assertNoWALSidecar closes the write pool and proves the data directory
// carries no live WAL sidecar: the boot must leave the write pool as the
// last SQLite connection so the clean close checkpoints the WAL away.
func assertNoWALSidecar(t *testing.T, database *bootstrap.Database, dataDirectory string) {
	t.Helper()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDirectory, "quoin.db-wal")); !os.IsNotExist(err) {
		t.Fatalf("backup-owned reader leaked past database.Close: WAL stat err=%v", err)
	}
}

// TestUpgradeMaintenanceRuntimeBackupJoinsSharedReader pins the maintenance
// boot ownership: the backup service's public reads move onto the process
// shared read-only pool, the durable reconciler is equipped, and the write
// pool stays the last SQLite connection of the boot. Closing the shared pool
// must take the backup service's reads down with it — a still-attached
// self-opened pool would keep answering.
func TestUpgradeMaintenanceRuntimeBackupJoinsSharedReader(t *testing.T) {
	ctx, stop, application, database, config := upgradeMaintenanceFixture(t)
	opsServer, err := sharedops.New("quoin", ":0", sharedops.Maintenance)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.startUpgradeMaintenanceRuntime(ctx, config, &servers{ops: opsServer}); err != nil {
		t.Fatal(err)
	}
	if application.upgradeReconciler == nil || application.upgradeBackups == nil {
		t.Fatal("upgrade reconciler or backup authority was not equipped")
	}
	stop()
	if err := application.reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := application.upgradeBackups.Settings(context.Background()); err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("backup read after shared-pool close err=%v, want the closed shared pool (an owned pool is still attached)", err)
	}
	assertNoWALSidecar(t, database, config.DataDirectory)
}

// TestUpgradeMaintenanceRuntimeProjectorFailureFailsBootWithoutLeak pins the
// failure path: a projector error fails the boot loudly instead of running
// the reconciler unprojected, and the boot still leaves no backup-owned
// reader pool behind (the shared-pool wiring holds even on this path).
func TestUpgradeMaintenanceRuntimeProjectorFailureFailsBootWithoutLeak(t *testing.T) {
	ctx, stop, application, database, config := upgradeMaintenanceFixture(t)
	// A non-quoin ops catalog has no quoin_upgrade_prepared gauge, so the
	// projector wiring fails after the backup service already exists.
	opsServer, err := sharedops.New("plinth", ":0", sharedops.Maintenance)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.startUpgradeMaintenanceRuntime(ctx, config, &servers{ops: opsServer}); err == nil {
		t.Fatal("projector failure must fail the maintenance boot")
	}
	if application.upgradeBackups == nil {
		t.Fatal("backup authority was not equipped before the projector failure")
	}
	if _, err := application.upgradeBackups.Settings(context.Background()); err != nil {
		t.Fatalf("backup service must serve from the shared pool after the failed boot: %v", err)
	}
	stop()
	assertNoWALSidecar(t, database, config.DataDirectory)
}
