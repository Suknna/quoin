package maintenance_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
)

func TestExitRequiresEveryFrozenRestoreItemSafeAndReplays(t *testing.T) {
	database, adminID := maintenanceFixture(t)
	defer database.Close()
	ctx := seedVerifiedSession(t, database.SQL, adminID)
	enterRestore(t, database.SQL, adminID)
	service := newMaintenanceService(t, database)
	state, err := service.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := maintenance.ExitRequest{ActorID: adminID, ExpectedReason: "Restore", ExpectedRowVersion: state.RowVersion, ClientCommandID: "exit_restore_0001"}
	if _, err := service.Exit(ctx, request); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("blocking exit error=%v, want conflict", err)
	}
	// The deterministic conflict is recorded durably as a rejected command:
	// the runner surfaces the public ErrConflict while the ledger keeps the
	// evidence-bound rejection (the same command id cannot be retried).
	if count := rejectedAudits(t, database.SQL, "maintenance.exit"); count != 1 {
		t.Fatalf("conflicted exit rejected audits=%d", count)
	}
	if count := rejectedLedgerRows(t, database.SQL, "maintenance.exit"); count != 1 {
		t.Fatalf("conflicted exit rejected ledger rows=%d", count)
	}
	if _, err := database.SQL.Exec(`UPDATE maintenance_items SET safe_state='Safe',detail_code='verified' WHERE maintenance_revision=?`, state.RowVersion); err != nil {
		t.Fatal(err)
	}
	// The burned command id stays rejected; the operator's fix retries under a
	// fresh command id, exactly like the deployment helper contract.
	if _, err := service.Exit(ctx, request); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("burned command id replay err=%v, want conflict", err)
	}
	exited, err := service.Exit(ctx, maintenance.ExitRequest{ActorID: adminID, ExpectedReason: "Restore", ExpectedRowVersion: state.RowVersion, ClientCommandID: "exit_restore_0002"})
	if err != nil {
		t.Fatal(err)
	}
	if exited.Active {
		t.Fatalf("state after exit=%+v", exited)
	}
	// The shared runner recorded exactly one audited successful exit for the
	// verified user session: user actor, session-backed correlation.
	var actorType string
	var actorID, successRows, correlated int64
	if err := database.SQL.QueryRow(`SELECT actor_type,actor_id,COUNT(*),COALESCE(MAX(correlation_id IS NOT NULL),0) FROM audit_events WHERE action='maintenance.exit' AND outcome='success'`).Scan(&actorType, &actorID, &successRows, &correlated); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || actorID != adminID || successRows != 1 || correlated != 1 {
		t.Fatalf("exit audit actor=%s/%d rows=%d correlated=%d", actorType, actorID, successRows, correlated)
	}
	replayed, err := service.Exit(ctx, maintenance.ExitRequest{ActorID: adminID, ExpectedReason: "Restore", ExpectedRowVersion: state.RowVersion, ClientCommandID: "exit_restore_0002"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Active {
		t.Fatalf("replayed state=%+v", replayed)
	}
	if count := auditEvents(t, database.SQL, "maintenance.exit"); count != 2 {
		t.Fatalf("replay produced new audit rows=%d", count)
	}
}

func rejectedAudits(t *testing.T, db *sql.DB, action string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=? AND outcome='rejected'`, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func rejectedLedgerRows(t *testing.T, db *sql.DB, commandType string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE command_type=? AND outcome='rejected_known'`, commandType).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// userContext builds metadata for one user actor without a session proof —
// the shape of the password-safe edge, whose owning session the change
// password command has just rotated.
func userContext(t *testing.T, userID int64) context.Context {
	t.Helper()
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: userID},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: correlation},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestMarkAdminPasswordSafeRecordsAuditedCompletionEdge(t *testing.T) {
	database, adminID := maintenanceFixture(t)
	defer database.Close()
	ctx := userContext(t, adminID)
	enterRestore(t, database.SQL, adminID)
	service := newMaintenanceService(t, database)
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
	// The edge is an audited mutation of the owning user's own operation.
	var actorType string
	var actorID int64
	if err := database.SQL.QueryRow(`SELECT actor_type,actor_id FROM audit_events WHERE action='maintenance.admin_password_safe'`).Scan(&actorType, &actorID); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || actorID != adminID {
		t.Fatalf("safe-edge audit actor=%s/%d", actorType, actorID)
	}
	// A foreign actor cannot mark someone else's edge.
	foreign := userContext(t, 99)
	if err := service.MarkAdminPasswordSafe(foreign, adminID); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("foreign actor err=%v", err)
	}
}

func TestMarkAdminPasswordSafeOutsideRestore(t *testing.T) {
	database, adminID := maintenanceFixture(t)
	defer database.Close()
	ctx := userContext(t, adminID)
	service := maintenance.NewService(database.SQL)
	// A closed window: the edge is a recorded no-op attempt, never an error —
	// the password change itself already succeeded.
	if err := service.MarkAdminPasswordSafe(ctx, adminID); err != nil {
		t.Fatalf("inactive maintenance err=%v", err)
	}
	if count := auditEvents(t, database.SQL, "maintenance.admin_password_safe"); count != 1 {
		t.Fatalf("inactive-edge audit rows=%d", count)
	}
	// A foreign active window is a recorded deterministic conflict.
	if _, err := database.SQL.Exec(`UPDATE maintenance_state SET active=1,reason='Upgrade',entered_at=?,entered_by_type='system',row_version=row_version+1 WHERE id=1`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := service.MarkAdminPasswordSafe(ctx, adminID); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("foreign maintenance err=%v", err)
	}
	if count := rejectedAudits(t, database.SQL, "maintenance.admin_password_safe"); count != 1 {
		t.Fatalf("foreign-edge rejected audits=%d", count)
	}
}

func TestExitFailsClosedWithoutExecutionContext(t *testing.T) {
	database, adminID := maintenanceFixture(t)
	defer database.Close()
	enterRestore(t, database.SQL, adminID)
	service := newMaintenanceService(t, database)
	state, err := service.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request := maintenance.ExitRequest{ActorID: adminID, ExpectedReason: "Restore", ExpectedRowVersion: state.RowVersion, ClientCommandID: "exit_restore_bare"}
	if _, err := service.Exit(context.Background(), request); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("missing context error=%v", err)
	}
	var active int
	if err := database.SQL.QueryRow(`SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("maintenance after bare exit=%d err=%v", active, err)
	}
	if count := auditEvents(t, database.SQL, "maintenance.exit"); count != 0 {
		t.Fatalf("bare exit recorded audit rows=%d", count)
	}
	if count := ledgerRows(t, database.SQL, "maintenance.exit"); count != 0 {
		t.Fatalf("bare exit recorded ledger rows=%d", count)
	}
}

// The public state read serves only the composition read-only pool: an
// unwired service fails closed instead of silently borrowing the write pool.
func TestStateFailsClosedWithoutWiredReader(t *testing.T) {
	database, _ := maintenanceFixture(t)
	defer database.Close()
	service := maintenance.NewService(database.SQL)
	if _, err := service.State(context.Background()); err == nil {
		t.Fatal("unwired service state read unexpectedly succeeded")
	}
}

// Ledger rows written by the pre-runner releases carry only their compact
// legacy JSON payload; replaying one keeps the old contract — return the live
// projection and record nothing new.
func TestExitReplaysLegacyJSONLedgerRowsWithoutNewRecords(t *testing.T) {
	database, adminID := maintenanceFixture(t)
	defer database.Close()
	ctx := seedVerifiedSession(t, database.SQL, adminID)
	enterRestore(t, database.SQL, adminID)
	service := newMaintenanceService(t, database)
	state, err := service.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.Exec(`UPDATE maintenance_items SET safe_state='Safe',detail_code='verified' WHERE maintenance_revision=?`, state.RowVersion); err != nil {
		t.Fatal(err)
	}
	digest := auth.DigestCommand("maintenance.exit", map[string]any{"expectedReason": "Restore", "expectedRowVersion": state.RowVersion})
	// The real exit commits under its own command id; the legacy row replays
	// against the already-exited aggregate like a pre-runner deployment.
	if _, err := service.Exit(ctx, maintenance.ExitRequest{ActorID: adminID, ExpectedReason: "Restore", ExpectedRowVersion: state.RowVersion, ClientCommandID: "exit_restore_live"}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.Exec(`INSERT INTO client_commands(principal_type,principal_id,client_command_id,command_type,request_digest,outcome,result_object_type,result_object_id,result_payload_json,created_at) VALUES('user',?,'exit_restore_legacy','maintenance.exit',?,'committed','maintenance',?,'{"exited":true}',?)`, adminID, digest, state.RowVersion, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Exit(ctx, maintenance.ExitRequest{ActorID: adminID, ExpectedReason: "Restore", ExpectedRowVersion: state.RowVersion, ClientCommandID: "exit_restore_legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Active {
		t.Fatalf("legacy replay state=%+v", replayed)
	}
	if count := auditEvents(t, database.SQL, "maintenance.exit"); count != 1 {
		t.Fatalf("audit rows after legacy replay=%d want only the live exit", count)
	}
}

func TestMarkAdminPasswordSafeDoesNotAdvanceFrozenMaintenanceRevision(t *testing.T) {
	database, adminID := maintenanceFixture(t)
	defer database.Close()
	ctx := userContext(t, adminID)
	enterRestore(t, database.SQL, adminID)
	service := newMaintenanceService(t, database)
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

func seedVerifiedSession(t *testing.T, db *sql.DB, userID int64) context.Context {
	t.Helper()
	now := time.Now().UTC()
	// password_change_required is a user security column: clearing it requires
	// the paired auth_revision advance in the same UPDATE. A row already
	// session-eligible must not be rewritten at all — the released trigger
	// forbids a revision advance without a security change.
	var pending, initialized int
	if err := db.QueryRow(`SELECT password_change_required,initialized FROM users WHERE id=?`, userID).Scan(&pending, &initialized); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || initialized != 1 {
		if _, err := db.Exec(`UPDATE users SET initialized=1,password_change_required=0,password_change_required_at=NULL,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=?`, now.Format(time.RFC3339Nano), userID); err != nil {
			t.Fatal(err)
		}
	}
	var revision int64
	if err := db.QueryRow(`SELECT auth_revision FROM users WHERE id=?`, userID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("fixture-session/%d/%d", userID, now.UnixNano())))
	result, err := db.Exec(`INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, digest[:], revision, "fixture", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano), now.Add(24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	sessionID, _ := result.LastInsertId()
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: userID},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: requestID},
		Session:       execution.SessionRef{ID: sessionID, AuthRevision: revision},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func auditEvents(t *testing.T, db *sql.DB, action string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=?`, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func ledgerRows(t *testing.T, db *sql.DB, commandType string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE command_type=?`, commandType).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
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
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
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

// newMaintenanceService builds the live service over the fixture database and
// wires the bootstrap read-only pool through the same SetReader seam
// production installs before serving (app configureReadOnly). The pool's
// lifetime stays with the fixture: database.Close releases both handles.
func newMaintenanceService(t *testing.T, database *bootstrap.Database) *maintenance.Service {
	t.Helper()
	service := maintenance.NewService(database.SQL)
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	return service
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
