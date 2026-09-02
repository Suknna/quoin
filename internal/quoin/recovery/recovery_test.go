package recovery_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/recovery"
)

func TestTicket33RestoreReplacesSnapshotAndEntersTrustIsolation(t *testing.T) {
	ctx := context.Background()
	config := testConfig(t.TempDir())
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateFirstAdmin(ctx, "admin", "Restore Admin", "original-password-123"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(config.DataDirectory, "artifacts", "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.DataDirectory, "artifacts", "blobs", "snapshot.blob"), []byte("snapshot artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	backups, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	run, err := backups.RunOffline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(config.DataDirectory, "artifacts", "blobs", "after-snapshot"), []byte("must disappear"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := recovery.Restore(ctx, recovery.Request{
		DataDirectory:     config.DataDirectory,
		BackupDirectory:   config.BackupDirectory,
		BackupID:          run.ID,
		RootKeyFile:       config.RootKeyFile,
		AdminUsername:     "admin",
		TemporaryPassword: "recovered-password-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MaintenanceReason != "Restore" || result.MaintenanceRevision < 2 {
		t.Fatalf("restore result=%+v", result)
	}
	if _, err := os.Stat(filepath.Join(config.DataDirectory, "artifacts", "blobs", "after-snapshot")); !os.IsNotExist(err) {
		t.Fatalf("post-snapshot residue stat err=%v, want not exist", err)
	}

	restored, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	assertRestoreIsolation(t, restored.SQL, "recovered-password-123")
}

func TestTicket33RestoreRejectsMissingCorruptAndForeignBackupWithoutReplacingData(t *testing.T) {
	ctx := context.Background()
	corruptConfig, corruptRun := backupFixture(t)
	corruptOriginal, err := os.ReadFile(filepath.Join(corruptConfig.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptConfig.BackupDirectory, corruptRun.ID, "quoin.db"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	corruptRequest := recovery.Request{DataDirectory: corruptConfig.DataDirectory, BackupDirectory: corruptConfig.BackupDirectory, BackupID: corruptRun.ID, RootKeyFile: corruptConfig.RootKeyFile, AdminUsername: "admin", TemporaryPassword: "recovered-password-123"}
	if _, err := recovery.Restore(ctx, corruptRequest); err == nil {
		t.Fatal("corrupt database was accepted")
	}
	corruptCurrent, err := os.ReadFile(filepath.Join(corruptConfig.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(corruptCurrent) != string(corruptOriginal) {
		t.Fatal("corrupt backup replaced live database")
	}

	wrongKeyConfig, wrongKeyRun := backupFixture(t)
	wrongKeyOriginal, err := os.ReadFile(filepath.Join(wrongKeyConfig.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := os.ReadFile(wrongKeyConfig.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey[0] ^= 0xff
	wrongKeyPath := filepath.Join(t.TempDir(), "wrong-root-key")
	if err := os.WriteFile(wrongKeyPath, wrongKey, 0o600); err != nil {
		t.Fatal(err)
	}
	wrongKeyRequest := recovery.Request{DataDirectory: wrongKeyConfig.DataDirectory, BackupDirectory: wrongKeyConfig.BackupDirectory, BackupID: wrongKeyRun.ID, RootKeyFile: wrongKeyPath, AdminUsername: "admin", TemporaryPassword: "recovered-password-123"}
	if _, err := recovery.Restore(ctx, wrongKeyRequest); err == nil {
		t.Fatal("wrong root key was accepted")
	}
	wrongKeyCurrent, err := os.ReadFile(filepath.Join(wrongKeyConfig.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(wrongKeyCurrent) != string(wrongKeyOriginal) {
		t.Fatal("wrong root key replaced live database")
	}

	sidecarConfig, sidecarRun := backupFixture(t)
	sidecarOriginal, err := os.ReadFile(filepath.Join(sidecarConfig.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecarConfig.DataDirectory, "quoin.db-wal"), []byte("old-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecarRequest := recovery.Request{DataDirectory: sidecarConfig.DataDirectory, BackupDirectory: sidecarConfig.BackupDirectory, BackupID: sidecarRun.ID, RootKeyFile: sidecarConfig.RootKeyFile, AdminUsername: "admin", TemporaryPassword: "recovered-password-123"}
	if _, err := recovery.Restore(ctx, sidecarRequest); err == nil {
		t.Fatal("live WAL sidecar was accepted")
	}
	sidecarCurrent, err := os.ReadFile(filepath.Join(sidecarConfig.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(sidecarCurrent) != string(sidecarOriginal) {
		t.Fatal("sidecar fence replaced live database")
	}

	config, run := backupFixture(t)
	original, err := os.ReadFile(filepath.Join(config.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	request := recovery.Request{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, BackupID: run.ID, RootKeyFile: config.RootKeyFile, AdminUsername: "admin", TemporaryPassword: "recovered-password-123"}
	if _, err := recovery.Restore(ctx, request); err != nil {
		t.Fatalf("valid restore: %v", err)
	}
	restored, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	assertRestoreIsolation(t, restored.SQL, "recovered-password-123")
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.BackupDirectory, run.ID, "foreign"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.Restore(ctx, request); err == nil {
		t.Fatal("foreign backup was accepted")
	}
	if err := os.Remove(filepath.Join(config.BackupDirectory, run.ID, "foreign")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(config.BackupDirectory, run.ID, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.Restore(ctx, request); err == nil {
		t.Fatal("missing manifest was accepted")
	}
	current, err := os.ReadFile(filepath.Join(config.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) == string(original) {
		// The first valid restore changes the database for isolation; subsequent
		// invalid source attempts must not replace that current restored database.
		t.Fatal("fixture did not observe the valid isolation transition")
	}
}

func TestContinuationRequiresPublishedRestoreMaintenanceAndBackupBoundRollback(t *testing.T) {
	ctx := context.Background()
	config, run := backupFixture(t)
	request := recovery.Request{
		DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, BackupID: run.ID,
		RootKeyFile: config.RootKeyFile, AdminUsername: "admin", TemporaryPassword: "temporary-password-456",
		RollbackDirectory: ".restore-rollback-" + run.ID,
	}
	if _, err := recovery.Restore(ctx, request); err != nil {
		t.Fatal(err)
	}
	continued, err := recovery.Continue(ctx, recovery.Request{DataDirectory: config.DataDirectory, BackupID: run.ID, RootKeyFile: config.RootKeyFile, RollbackDirectory: ".restore-rollback-" + run.ID})
	if err != nil || continued.MaintenanceRevision < 1 || filepath.Base(continued.RollbackDirectory) != ".restore-rollback-"+run.ID {
		t.Fatalf("continuation=%+v err=%v", continued, err)
	}
	if err := os.RemoveAll(continued.RollbackDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.Continue(ctx, recovery.Request{DataDirectory: config.DataDirectory, BackupID: run.ID, RootKeyFile: config.RootKeyFile, RollbackDirectory: ".restore-rollback-" + run.ID}); !errors.Is(err, recovery.ErrContinuationFence) {
		t.Fatalf("missing rollback continuation error=%v", err)
	}
}

func TestPreflightVerifiesExactPublishedArchiveBeforeDataLock(t *testing.T) {
	config, run := backupFixture(t)
	result, err := recovery.Preflight(config.BackupDirectory, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.BackupID != run.ID || result.Release == "" || len(result.ManifestSHA256) != 64 {
		t.Fatalf("preflight result=%+v", result)
	}
	if _, err := recovery.Preflight(config.BackupDirectory, "999999999"); err == nil {
		t.Fatal("missing archive was accepted before destructive restore")
	}
	if err := os.WriteFile(filepath.Join(config.BackupDirectory, run.ID, "unexpected"), []byte("tamper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.Preflight(config.BackupDirectory, run.ID); err == nil || !strings.Contains(err.Error(), "verify backup") {
		t.Fatalf("tampered archive preflight error=%v", err)
	}
}

func backupFixture(t *testing.T) (contract.QuoinConfig, backup.Summary) {
	t.Helper()
	ctx := context.Background()
	config := testConfig(t.TempDir())
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateFirstAdmin(ctx, "admin", "Restore Admin", "original-password-123"); err != nil {
		t.Fatal(err)
	}
	createdAt := "2026-01-01T00:00:00Z"
	var adminPHC string
	if err := database.SQL.QueryRowContext(ctx, `SELECT password_phc FROM users WHERE username='admin'`).Scan(&adminPHC); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.ExecContext(ctx, `INSERT INTO users(username,display_name,role,enabled,password_phc,created_at,updated_at) VALUES('disabled-user','Disabled User','operator',0,?,?,?)`, adminPHC, createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.ExecContext(ctx, `INSERT INTO connections(name,type,enabled,created_at) VALUES('restored-thanos','thanos',1,?)`, createdAt); err != nil {
		t.Fatal(err)
	}
	alertSource, err := database.SQL.ExecContext(ctx, `INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES('restored-alerts','alertmanager',1,?)`, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	alertSourceID, err := alertSource.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	alertCredential, err := database.SQL.ExecContext(ctx, `INSERT INTO alert_source_credentials(source_id,digest,state,created_at) VALUES(?,?,'Active',?)`, alertSourceID, make([]byte, 32), createdAt)
	if err != nil {
		t.Fatal(err)
	}
	alertCredentialID, err := alertCredential.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.ExecContext(ctx, `INSERT INTO alert_source_credentials(source_id,digest,state,supersedes_credential_id,created_at) VALUES(?,?,'Active',?,?)`, alertSourceID, bytes.Repeat([]byte{1}, 32), alertCredentialID, createdAt); err != nil {
		t.Fatal(err)
	}
	result, err := database.SQL.ExecContext(ctx, `INSERT INTO runtime_credentials(slot,generation,token_digest,created_at,confirmed_at) VALUES('plinth',1,?,?,?)`, make([]byte, 32), createdAt, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.ExecContext(ctx, `UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot='plinth'`, credentialID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(config.DataDirectory, "artifacts", "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	backups, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	run, err := backups.RunOffline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return config, run
}

func assertRestoreIsolation(t *testing.T, database *sql.DB, temporaryPassword string) {
	t.Helper()
	var active int
	var reason string
	if err := database.QueryRow(`SELECT active,reason FROM maintenance_state WHERE id=1`).Scan(&active, &reason); err != nil {
		t.Fatal(err)
	}
	if active != 1 || reason != "Restore" {
		t.Fatalf("maintenance active=%d reason=%q", active, reason)
	}
	var passwordChange, enabled int
	var passwordPHC string
	if err := database.QueryRow(`SELECT password_change_required,enabled,password_phc FROM users WHERE username='admin'`).Scan(&passwordChange, &enabled, &passwordPHC); err != nil {
		t.Fatal(err)
	}
	if passwordChange != 1 || enabled != 1 || !auth.VerifyPassword(temporaryPassword, passwordPHC) {
		t.Fatalf("recovery admin isolation passwordChange=%d enabled=%d temporaryPasswordMatches=%t", passwordChange, enabled, auth.VerifyPassword(temporaryPassword, passwordPHC))
	}
	var disabledUserEnabled, disabledUserRevision int
	disabledUserErr := database.QueryRow(`SELECT enabled,auth_revision FROM users WHERE username='disabled-user'`).Scan(&disabledUserEnabled, &disabledUserRevision)
	if disabledUserErr != nil && disabledUserErr != sql.ErrNoRows {
		t.Fatal(disabledUserErr)
	}
	if disabledUserErr == nil && (disabledUserEnabled != 0 || disabledUserRevision != 1) {
		t.Fatalf("disabled user enabled=%d auth_revision=%d", disabledUserEnabled, disabledUserRevision)
	}
	var restoredConnectionEnabled int
	connectionErr := database.QueryRow(`SELECT enabled FROM connections WHERE name='restored-thanos'`).Scan(&restoredConnectionEnabled)
	if connectionErr != nil && connectionErr != sql.ErrNoRows {
		t.Fatal(connectionErr)
	}
	if connectionErr == nil && restoredConnectionEnabled != 0 {
		t.Fatal("restored enabled connection was not disabled")
	}
	var acceptedAlertCredentials int
	if err := database.QueryRow(`SELECT COUNT(*) FROM alert_source_credentials WHERE state <> 'Retired'`).Scan(&acceptedAlertCredentials); err != nil {
		t.Fatal(err)
	}
	if acceptedAlertCredentials != 0 {
		t.Fatalf("accepted alert credentials=%d", acceptedAlertCredentials)
	}
	var sessions, registered, activeRuntimeCredentials, uncheckedConnections, blockingItems int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM runtime_slots WHERE state <> 'revoked'`).Scan(&registered); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM runtime_credentials WHERE retired_at IS NULL`).Scan(&activeRuntimeCredentials); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM connections WHERE revalidation_required = 0`).Scan(&uncheckedConnections); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM maintenance_items WHERE maintenance_revision=(SELECT row_version FROM maintenance_state WHERE id=1) AND safe_state = 'Blocking'`).Scan(&blockingItems); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || registered != 0 || activeRuntimeCredentials != 0 || uncheckedConnections != 0 || blockingItems != 1 {
		t.Fatalf("sessions=%d registered=%d activeRuntimeCredentials=%d uncheckedConnections=%d blockingItems=%d", sessions, registered, activeRuntimeCredentials, uncheckedConnections, blockingItems)
	}
	for _, kind := range []string{"RuntimeSlot", "AlertSource", "Connection", "BrowserIdentity"} {
		var total, unsafe int
		if err := database.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN safe_state='Blocking' THEN 1 ELSE 0 END),0) FROM maintenance_items WHERE maintenance_revision=(SELECT row_version FROM maintenance_state WHERE id=1) AND kind=?`, kind).Scan(&total, &unsafe); err != nil {
			t.Fatal(err)
		}
		if total > 0 && unsafe != 0 {
			t.Fatalf("restore containment %s remains blocking", kind)
		}
	}
}

func testConfig(root string) contract.QuoinConfig {
	secrets := filepath.Join(root, "secrets")
	return contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backup"), RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), SteleServiceTokenFile: filepath.Join(secrets, "stele-service-token")}
}
