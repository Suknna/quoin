package maintenance_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
)

func TestExitRequiresEveryFrozenRestoreItemSafeAndReplays(t *testing.T) {
	ctx := context.Background()
	database, adminID := maintenanceFixture(t)
	defer database.Close()
	enterRestore(t, database.SQL, adminID)
	service := maintenance.NewService(database.SQL)
	state, err := service.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := maintenance.ExitRequest{ActorID: adminID, ExpectedReason: "Restore", ExpectedRowVersion: state.RowVersion, ClientCommandID: "exit_restore_0001"}
	if _, err := service.Exit(ctx, request); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("blocking exit error=%v, want conflict", err)
	}
	if _, err := database.SQL.Exec(`UPDATE maintenance_items SET safe_state='Safe',detail_code='verified' WHERE maintenance_revision=?`, state.RowVersion); err != nil {
		t.Fatal(err)
	}
	exited, err := service.Exit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if exited.Active {
		t.Fatalf("state after exit=%+v", exited)
	}
	replayed, err := service.Exit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Active {
		t.Fatalf("replayed state=%+v", replayed)
	}
}

func TestMarkAdminPasswordSafeDoesNotAdvanceFrozenMaintenanceRevision(t *testing.T) {
	ctx := context.Background()
	database, adminID := maintenanceFixture(t)
	defer database.Close()
	enterRestore(t, database.SQL, adminID)
	service := maintenance.NewService(database.SQL)
	before, err := service.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.MarkAdminPasswordSafe(ctx, adminID); err != nil {
		t.Fatal(err)
	}
	after, err := service.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.RowVersion != before.RowVersion {
		t.Fatalf("rowVersion changed %d -> %d", before.RowVersion, after.RowVersion)
	}
	for _, item := range after.Items {
		if item.Kind == "AdminPassword" && item.SafeState != "Safe" {
			t.Fatalf("admin item=%+v", item)
		}
	}
}

func maintenanceFixture(t *testing.T) (*bootstrap.Database, int64) {
	t.Helper()
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), SteleServiceTokenFile: filepath.Join(root, "secrets", "stele")}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(database.SQL)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	created, err := service.CreateFirstAdmin(context.Background(), "admin", "Restore Admin", "original-password-123")
	if err != nil || !created {
		database.Close()
		t.Fatalf("create first admin created=%v err=%v", created, err)
	}
	var adminID int64
	if err := database.SQL.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&adminID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	return database, adminID
}

func enterRestore(t *testing.T, database *sql.DB, adminID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Exec(`UPDATE maintenance_state SET active=1,reason='Restore',entered_at=?,entered_by_type='system',entered_by_id=0,row_version=row_version+1 WHERE id=1`, now); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := database.QueryRow(`SELECT row_version FROM maintenance_state WHERE id=1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,?,?,?)`, revision, "AdminPassword", fmt.Sprint(adminID), "Blocking", "temporary_password_change_required", now); err != nil {
		t.Fatal(err)
	}
}
