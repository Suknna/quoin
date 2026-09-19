package recovery_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/recovery"
	"github.com/Suknna/quoin/test/support"
)

func TestTicket33RestoreReplacesSnapshotAndEntersTrustIsolation(t *testing.T) {
	ctx := context.Background()
	config := testConfig(t.TempDir())
	if err := support.GenerateDeploymentSecrets(config); err != nil {
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
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
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
	// Release the backup service's reader pool before the write pool so the
	// last SQLite connection checkpoints and removes the WAL; Restore refuses
	// a data directory that still carries a sidecar.
	if err := backups.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(config.DataDirectory, "artifacts", "blobs", "after-snapshot"), []byte("must disappear"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := recovery.Restore(ctx, recovery.Request{
		DataDirectory:   config.DataDirectory,
		BackupDirectory: config.BackupDirectory,
		BackupID:        run.ID,
		RootKeyFile:     config.RootKeyFile,
		AdminUsername:   "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MaintenanceReason != "Restore" || result.MaintenanceRevision < 2 {
		t.Fatalf("restore result=%+v", result)
	}
	if result.TemporaryPassword == "" {
		t.Fatal("restore must issue the one-time recovery credential")
	}
	if _, err := os.Stat(filepath.Join(config.DataDirectory, "artifacts", "blobs", "after-snapshot")); !os.IsNotExist(err) {
		t.Fatalf("post-snapshot residue stat err=%v, want not exist", err)
	}

	restored, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	assertRestoreIsolation(t, restored.SQL)
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
	corruptRequest := recovery.Request{DataDirectory: corruptConfig.DataDirectory, BackupDirectory: corruptConfig.BackupDirectory, BackupID: corruptRun.ID, RootKeyFile: corruptConfig.RootKeyFile, AdminUsername: "admin"}
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
	wrongKeyRequest := recovery.Request{DataDirectory: wrongKeyConfig.DataDirectory, BackupDirectory: wrongKeyConfig.BackupDirectory, BackupID: wrongKeyRun.ID, RootKeyFile: wrongKeyPath, AdminUsername: "admin"}
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
	sidecarRequest := recovery.Request{DataDirectory: sidecarConfig.DataDirectory, BackupDirectory: sidecarConfig.BackupDirectory, BackupID: sidecarRun.ID, RootKeyFile: sidecarConfig.RootKeyFile, AdminUsername: "admin"}
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
	request := recovery.Request{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, BackupID: run.ID, RootKeyFile: config.RootKeyFile, AdminUsername: "admin"}
	if _, err := recovery.Restore(ctx, request); err != nil {
		t.Fatalf("valid restore: %v", err)
	}
	restored, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	assertRestoreIsolation(t, restored.SQL)
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
		RootKeyFile: config.RootKeyFile, AdminUsername: "admin",
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
	if err := support.GenerateDeploymentSecrets(config); err != nil {
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
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
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
	// Close the owned reader pool before the fixture's deferred write-pool
	// close so the WAL sidecar is checkpointed away and the restore below
	// sees a cleanly closed data directory.
	if err := backups.Close(); err != nil {
		t.Fatal(err)
	}
	return config, run
}

func testConfig(root string) contract.QuoinConfig {
	secrets := filepath.Join(root, "secrets")
	return contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backup"), RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), RuntimeClientCAFile: filepath.Join(secrets, "stele-service-token")}
}

// recoveryRecordingSender captures the fixture delivery so the test can read
// the issued verification code.
type recoveryRecordingSender struct {
	mu    sync.Mutex
	codes []string
}

func (sender *recoveryRecordingSender) Send(_ context.Context, message auth.Message) error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.codes = append(sender.codes, message.Variables["code"])
	return nil
}

func (sender *recoveryRecordingSender) lastCode() string {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.codes) == 0 {
		return ""
	}
	return sender.codes[len(sender.codes)-1]
}

// TestRestoreEntersDirectRecoveryFlowAndReachesLogin covers the sanctioned
// restore journey end to end: the isolated snapshot carries an initialized
// administrator with live factors, restore burns them and seals the one-time
// recovery credential, and the web recovery flow (new password plus verified
// contact) is the only path back to a normal login. The pre-restore temporary
// password field stays accepted for helper compatibility.
func TestRestoreEntersDirectRecoveryFlowAndReachesLogin(t *testing.T) {
	ctx := context.Background()
	const originalPassword = "original-password-123"
	const restoredPassword = "Restored admin passphrase 2027!"
	config := testConfig(t.TempDir())
	if err := support.GenerateDeploymentSecrets(config); err != nil {
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
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateFirstAdmin(ctx, "admin", "Restore Admin", originalPassword); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Simulate the realistic initialized deployment: clearing the forced-change
	// flag is a security change, so the triggers require the revision advance.
	if _, err := database.SQL.ExecContext(ctx, `UPDATE users SET initialized=1,password_change_required=0,password_change_required_at=NULL,auth_revision=auth_revision+1,row_version=row_version+1 WHERE username='admin'`); err != nil {
		t.Fatal(err)
	}
	var adminID, adminRevision int64
	if err := database.SQL.QueryRowContext(ctx, `SELECT id,auth_revision FROM users WHERE username='admin'`).Scan(&adminID, &adminRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.ExecContext(ctx, `INSERT INTO user_contacts(user_id,channel,target,version,verified_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, adminID, "email", "ops@quoin.test", 1, now, now, now); err != nil {
		t.Fatal(err)
	}
	sessionRaw := make([]byte, 32)
	if _, err := rand.Read(sessionRaw); err != nil {
		t.Fatal(err)
	}
	sessionDigest := sha256.Sum256(sessionRaw)
	if _, err := database.SQL.ExecContext(ctx, `INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		adminID, sessionDigest[:], adminRevision, "pre-restore", now, now, time.Now().UTC().Add(12*time.Hour).Format(time.RFC3339Nano), time.Now().UTC().Add(7*24*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	flowRaw := make([]byte, 32)
	if _, err := rand.Read(flowRaw); err != nil {
		t.Fatal(err)
	}
	flowDigest := sha256.Sum256(flowRaw)
	if _, err := database.SQL.ExecContext(ctx, `INSERT INTO auth_flows(flow_type,user_id,flow_token_digest,correlation_id,auth_revision_at_issue,password_set,client_label,status,created_at,expires_at) VALUES('login',?,?,'pre-restore',?,0,'pre-restore','pending',?,?)`,
		adminID, flowDigest[:], adminRevision, now, time.Now().UTC().Add(15*time.Minute).Format(time.RFC3339Nano)); err != nil {
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
	// Same ownership order as the other restore fixtures: the backup
	// service's reader pool closes first, then the write pool, so SQLite
	// removes the WAL instead of the restore refusing a live sidecar.
	if err := backups.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := recovery.Restore(ctx, recovery.Request{
		DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, BackupID: run.ID,
		RootKeyFile: config.RootKeyFile, AdminUsername: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TemporaryPassword == "" {
		t.Fatal("restore must return the temporary administrator password")
	}

	restored, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	assertRestoreIsolation(t, restored.SQL)
	restoredService, err := auth.NewService(restored.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := restoredService.SetReader(restored.Reader); err != nil {
		t.Fatal(err)
	}
	sender := &recoveryRecordingSender{}
	if err := restoredService.ConfigureAuth(auth.AuthConfig{OTPKey: bytes.Repeat([]byte{0x5A}, 32), Sender: sender}); err != nil {
		t.Fatal(err)
	}

	// The burned password and every pre-restore factor are dead; the printed
	// temporary password is the only way in.
	if _, _, err := restoredService.StartAuthentication(ctx, "admin", originalPassword, "test"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("the pre-restore password must be unverifiable, got %v", err)
	}
	flow, _, err := restoredService.StartAuthentication(ctx, "admin", result.TemporaryPassword, "test")
	if err != nil {
		t.Fatalf("start unified initialization after restore: %v", err)
	}
	if flow.Type != auth.FlowAdminInitialize {
		t.Fatalf("unexpected flow type %q", flow.Type)
	}
	if err := restoredService.SetFlowPassword(ctx, flow.Bearer, restoredPassword); err != nil {
		t.Fatalf("set recovery password: %v", err)
	}
	contact, err := restoredService.RegisterFlowContact(ctx, flow.Bearer, "email", "ops@quoin.test")
	if err != nil {
		t.Fatalf("register recovery contact: %v", err)
	}
	if _, _, err := restoredService.SendFlowChallenge(ctx, flow.Bearer, contact.Locator); err != nil {
		t.Fatalf("send recovery challenge: %v", err)
	}
	if code := sender.lastCode(); len(code) != 6 {
		t.Fatalf("expected a 6-digit code, got %q", code)
	}
	if err := restoredService.VerifyFlowChallenge(ctx, flow.Bearer, sender.lastCode()); err != nil {
		t.Fatalf("verify recovery challenge: %v", err)
	}
	if err := restoredService.CompleteAdminInitialization(ctx, flow.Bearer); err != nil {
		t.Fatalf("complete re-initialization: %v", err)
	}

	var initialized int
	if err := restored.SQL.QueryRowContext(ctx, `SELECT initialized FROM users WHERE username='admin'`).Scan(&initialized); err != nil || initialized != 1 {
		t.Fatalf("recovery completion must re-initialize the administrator: value=%d err=%v", initialized, err)
	}
	login, _, err := restoredService.StartAuthentication(ctx, "admin", restoredPassword, "Mozilla/5.0 test")
	if err != nil {
		t.Fatalf("the restored administrator must reach a normal login: %v", err)
	}
	if login.Type != auth.FlowLogin {
		t.Fatalf("expected the login flow, got %q", login.Type)
	}
	if _, _, err := restoredService.StartAuthentication(ctx, "admin", result.TemporaryPassword, "test"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("the replaced temporary password must be invalid, got %v", err)
	}
	var restoreAudits int
	if err := restored.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action='maintenance.restore.enter' AND actor_type='system' AND correlation_id<>''`).Scan(&restoreAudits); err != nil || restoreAudits != 1 {
		t.Fatalf("restore must audit through the shared writer with explicit correlation: count=%d err=%v", restoreAudits, err)
	}
}

func assertRestoreIsolation(t *testing.T, database *sql.DB) {
	t.Helper()
	var active int
	var reason string
	if err := database.QueryRow(`SELECT active,reason FROM maintenance_state WHERE id=1`).Scan(&active, &reason); err != nil {
		t.Fatal(err)
	}
	if active != 1 || reason != "Restore" {
		t.Fatalf("maintenance active=%d reason=%q", active, reason)
	}
	// The preserved administrator sits exactly at the unified initialization
	// entry: enabled, uninitialized and holding only the printed temporary
	// password (marked for the forced formal change).
	var enabled, initialized, passwordChange int
	if err := database.QueryRow(`SELECT enabled,initialized,password_change_required FROM users WHERE username='admin'`).Scan(&enabled, &initialized, &passwordChange); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || initialized != 0 || passwordChange != 1 {
		t.Fatalf("recovery admin isolation enabled=%d initialized=%d passwordChange=%d", enabled, initialized, passwordChange)
	}
	var pendingFlows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM auth_flows WHERE status='pending'`).Scan(&pendingFlows); err != nil {
		t.Fatal(err)
	}
	if pendingFlows != 0 {
		t.Fatalf("pending flows survived restore isolation: %d", pendingFlows)
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
	var sessions, uncheckedConnections, blockingItems int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM connections WHERE revalidation_required = 0`).Scan(&uncheckedConnections); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM maintenance_items WHERE maintenance_revision=(SELECT row_version FROM maintenance_state WHERE id=1) AND safe_state = 'Blocking'`).Scan(&blockingItems); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || uncheckedConnections != 0 || blockingItems != 1 {
		t.Fatalf("sessions=%d uncheckedConnections=%d blockingItems=%d", sessions, uncheckedConnections, blockingItems)
	}
	for _, kind := range []string{"AlertSource", "Connection"} {
		var total, unsafe int
		if err := database.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN safe_state='Blocking' THEN 1 ELSE 0 END),0) FROM maintenance_items WHERE maintenance_revision=(SELECT row_version FROM maintenance_state WHERE id=1) AND kind=?`, kind).Scan(&total, &unsafe); err != nil {
			t.Fatal(err)
		}
		if total > 0 && unsafe != 0 {
			t.Fatalf("restore containment %s remains blocking", kind)
		}
	}
}
