package upgrade_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/upgrade"
	"github.com/Suknna/quoin/test/support"
)

func testNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func zeroDigest() string { return hex.EncodeToString(make([]byte, 32)) }

// seedVerifiedSession completes the fixture administrator's initialization
// (the bootstrap admin starts pending-init) and issues one real session row,
// then returns a request context carrying the execution metadata — user actor
// plus the session proof reference — that the shared command runner demands.
// auth.VerifyExecutionSession re-checks exactly this state inside the
// command transaction.
func seedVerifiedSession(t *testing.T, db *sql.DB, userID int64) context.Context {
	t.Helper()
	now := time.Now().UTC()
	// password_change_required is a user security column: clearing it requires
	// the paired auth_revision advance in the same UPDATE. A row already
	// session-eligible (fixture operators) must not be rewritten at all — the
	// released trigger forbids a revision advance without a security change.
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

// auditEvents counts committed audit rows for one action, optionally filtered
// by outcome.
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

func newFixture(t *testing.T) (*bootstrap.Database, int64) {
	t.Helper()
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), RuntimeClientCAFile: filepath.Join(root, "secrets", "stele")}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
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
	if _, err := service.CreateFirstAdmin(context.Background(), "admin", "Upgrade Admin", "original-password-123"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	var adminID int64
	if err := database.SQL.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&adminID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	return database, adminID
}

// seedAttempt inserts one execution attempt row; the fixture stays raw SQL
// because every projection input is a frozen table.
func seedAttempt(t *testing.T, db *sql.DB, attemptType, scopeType string, scopeID int64, state string) int64 {
	return seedAttemptWithCheck(t, db, attemptType, scopeType, scopeID, state, "")
}

func seedAttemptWithCheck(t *testing.T, db *sql.DB, attemptType, scopeType string, scopeID int64, state, checkKey string) int64 {
	t.Helper()
	var check any
	if checkKey != "" {
		check = checkKey
	}
	result, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,created_at) VALUES(?,?,?,?,?, 'v1-test', ?)`, attemptType, scopeType, scopeID, check, state, testNow())
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// seedInspectionRun builds the minimal plan-run inspection chain so a
// run_check child can exist under the frozen scope trigger: an enabled plan on
// an enabled metrics connection, its Run freezing the plan binding, and the
// expanded check catalog in inspection_run_checks.
func seedInspectionRun(t *testing.T, db *sql.DB, _ int64) int64 {
	t.Helper()
	now := testNow()
	metricsConnectionID := seedConnection(t, db, "t36-metrics")
	plan, err := db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_kind,scope_json,created_at,updated_at)
		VALUES('nightly','Nightly',1,?, 'builtin','promql_instant','1','{}','integration','{"kind":"integration"}',?,?)`, metricsConnectionID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	planID, _ := plan.LastInsertId()
	run, err := db.Exec(`INSERT INTO inspection_runs(plan_id,plan_key,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,trigger_kind,state,created_at)
		VALUES(?, 'nightly',?, 'builtin','promql_instant','1','{}','{"kind":"integration"}','manual','Queued',?)`, planID, metricsConnectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := run.LastInsertId()
	mustExec(t, db, `INSERT INTO inspection_run_checks(run_id,check_key,display_name,plugin_id,template_id,template_version,params_json,created_at)
		VALUES(?, 'probe-check','Probe','builtin','promql_instant','1','{}',?)`, runID, now)
	// A run_check child requires the Running parent with evidence started.
	mustExec(t, db, `UPDATE inspection_runs SET state='Running',evidence_at=?,row_version=row_version+1 WHERE id=?`, now, runID)
	return runID
}

func mustExec(t *testing.T, db *sql.DB, query string, arguments ...any) {
	t.Helper()
	if _, err := db.Exec(query, arguments...); err != nil {
		t.Fatal(err)
	}
}

func seedConnection(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?, 'thanos', 1, ?)`, name, testNow())
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func maintenanceItem(t *testing.T, db *sql.DB, revision int64, kind, objectKey string) (string, string) {
	t.Helper()
	var safeState, detailCode string
	if err := db.QueryRow(`SELECT safe_state,detail_code FROM maintenance_items WHERE maintenance_revision=? AND kind=? AND object_key=?`, revision, kind, objectKey).Scan(&safeState, &detailCode); err != nil {
		t.Fatalf("item %s/%s: %v", kind, objectKey, err)
	}
	return safeState, detailCode
}

// fakeBackups is the deterministic BackupRunner double: it records the
// executed runs and forces each one's durable outcome.
type fakeBackups struct {
	mu     sync.Mutex
	db     *sql.DB
	runErr error
	ran    []int64
}

func (fake *fakeBackups) RunUpgrade(ctx context.Context, id int64) error {
	fake.mu.Lock()
	fake.ran = append(fake.ran, id)
	runErr := fake.runErr
	fake.mu.Unlock()
	// Emulate the real terminal transitions through the frozen state machine
	// (queued→running→terminal with its stage and publish-field triggers), so
	// the admission window moves on; a partial update would leave the run
	// queued forever.
	if runErr == nil {
		for _, update := range []struct {
			query string
			args  []any
		}{
			{`UPDATE backups SET status='running',stage='preflight',started_at=?,updated_at=?,row_version=row_version+1 WHERE id=?`, []any{testNow(), testNow(), id}},
			{`UPDATE backups SET stage='database_snapshot',updated_at=?,row_version=row_version+1 WHERE id=?`, []any{testNow(), id}},
			{`UPDATE backups SET stage='artifact_copy',updated_at=?,row_version=row_version+1 WHERE id=?`, []any{testNow(), id}},
			{`UPDATE backups SET stage='manifest_publish',updated_at=?,row_version=row_version+1 WHERE id=?`, []any{testNow(), id}},
			{`UPDATE backups SET status='succeeded',stage='completed',db_sha256=?,manifest_sha256=?,artifact_count=0,size_bytes=1234,manifest_path='/backup/manifest.json',completed_at=?,updated_at=?,row_version=row_version+1 WHERE id=?`, []any{zeroDigest(), zeroDigest(), testNow(), testNow(), id}},
		} {
			if _, err := fake.db.ExecContext(ctx, update.query, update.args...); err != nil {
				// A fixture transition that violates the frozen state machine
				// must fail loudly, never strand the run queued forever.
				return err
			}
		}
		return nil
	}
	_, _ = fake.db.ExecContext(ctx, `UPDATE backups SET status='failed',error_code='storage_failure',retryable=0,error_detail='fixture failure',completed_at=?,updated_at=?,row_version=row_version+1 WHERE id=?`, testNow(), testNow(), id)
	return runErr
}

func (fake *fakeBackups) runs() []int64 {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]int64(nil), fake.ran...)
}

// seedSucceededUpgradeBackup inserts the succeeded pre-upgrade run bound to
// the maintenance window. It must satisfy every CHECK of the frozen state
// machine, so it reuses the real publish fields.
func seedSucceededUpgradeBackup(t *testing.T, db *sql.DB, actor int64, enteredAfter string) int64 {
	t.Helper()
	digest64 := hex.EncodeToString(make([]byte, 32))
	result, err := db.Exec(`
		INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,db_sha256,manifest_sha256,artifact_count,size_bytes,manifest_path,row_version,created_at,updated_at,started_at,completed_at,triggered_by)
		VALUES('succeeded','completed','upgrade','online',NULL,?,?,?,?, '/backup/manifest.json',1,?,?,?,?,?)`,
		digest64, digest64, 0, 1234, testNow(), testNow(), testNow(), testNow(), actor)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPrepareEntersUpgradeMaintenanceWithDeterministicChecklist(t *testing.T) {
	database, adminID := newFixture(t)
	defer database.Close()
	ctx := seedVerifiedSession(t, database.SQL, adminID)
	connectionID := seedConnection(t, database.SQL, "prod-thanos")
	probeID := seedAttempt(t, database.SQL, "connection_probe", "connection", connectionID, "Queued")
	service := upgrade.NewService(database.SQL)
	state, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0001", ExpectedRowVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Active || state.Reason != "Upgrade" || state.RowVersion != 2 {
		t.Fatalf("state=%+v", state)
	}
	if safe, _ := maintenanceItem(t, database.SQL, 2, "BackupPreflight", "pre_upgrade_backup"); safe != "Blocking" {
		t.Fatalf("backup preflight safe=%s", safe)
	}
	safe, detail := maintenanceItem(t, database.SQL, 2, "ActiveAttempt", fmt.Sprintf("attempt/%d", probeID))
	if safe != "Blocking" {
		t.Fatalf("attempt item safe=%s", safe)
	}
	// A Queued probe has no user cancel path (the fence requires a Running
	// attempt); the directive must say so instead of fabricating a button.
	if detail != "queued|converge" {
		t.Fatalf("queued probe attempt detail=%q want queued|converge", detail)
	}
	_ = probeID
	// The shared runner recorded exactly one audited command: the user actor
	// with the session-backed correlation from the request metadata.
	if count := auditEvents(t, database.SQL, "upgrade.prepare"); count != 1 {
		t.Fatalf("prepare audit rows=%d", count)
	}
	var actorType string
	var actorID, correlationID int64
	if err := database.SQL.QueryRow(`SELECT actor_type,actor_id,COALESCE(correlation_id IS NOT NULL,0) FROM audit_events WHERE action='upgrade.prepare'`).Scan(&actorType, &actorID, &correlationID); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || actorID != adminID || correlationID != 1 {
		t.Fatalf("prepare audit actor=%s/%d correlated=%d", actorType, actorID, correlationID)
	}
	// Idempotent replay returns the same frozen revision and records nothing new.
	replayed, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0001", ExpectedRowVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.RowVersion != 2 || len(replayed.Items) != len(state.Items) {
		t.Fatalf("replayed=%+v", replayed)
	}
	if count := auditEvents(t, database.SQL, "upgrade.prepare"); count != 1 {
		t.Fatalf("replay produced new audit rows=%d", count)
	}
	if count := ledgerRows(t, database.SQL, "upgrade.prepare"); count != 1 {
		t.Fatalf("ledger rows=%d", count)
	}
}

func TestPrepareFailsClosedWithoutExecutionContext(t *testing.T) {
	database, adminID := newFixture(t)
	defer database.Close()
	service := upgrade.NewService(database.SQL)
	// Missing execution metadata must fail closed before any state changes:
	// no synthetic context and no anonymous actor may mask the gap.
	if _, err := service.Prepare(context.Background(), upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_bare", ExpectedRowVersion: 1}); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("missing context error=%v", err)
	}
	var active int
	if err := database.SQL.QueryRow(`SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("maintenance after bare prepare=%d err=%v", active, err)
	}
	if count := auditEvents(t, database.SQL, "upgrade.prepare"); count != 0 {
		t.Fatalf("bare prepare recorded audit rows=%d", count)
	}
	if count := ledgerRows(t, database.SQL, "upgrade.prepare"); count != 0 {
		t.Fatalf("bare prepare recorded ledger rows=%d", count)
	}
}

func TestPrepareRejectsForeignPrincipalAndUnqualifiedSessions(t *testing.T) {
	database, adminID := newFixture(t)
	defer database.Close()
	ctx := seedVerifiedSession(t, database.SQL, adminID)
	if _, err := database.SQL.Exec(`INSERT INTO users(username,display_name,role,enabled,initialized,auth_revision,password_phc,created_at,updated_at) VALUES('op','Op','operator',1,1,1,'x',?,?)`, testNow(), testNow()); err != nil {
		t.Fatal(err)
	}
	var operatorID int64
	if err := database.SQL.QueryRow(`SELECT id FROM users WHERE username='op'`).Scan(&operatorID); err != nil {
		t.Fatal(err)
	}
	service := upgrade.NewService(database.SQL)
	// The request names an actor the verified session does not carry: the
	// runner rejects the mismatch before anything is recorded.
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: operatorID, ClientCommandID: "t36_prepare_foreign", ExpectedRowVersion: 1}); err == nil || errors.Is(err, upgrade.ErrConflict) {
		t.Fatalf("foreign principal error=%v", err)
	}
	// An operator session fails the in-transaction admin re-verification.
	operatorCtx := seedVerifiedSession(t, database.SQL, operatorID)
	if _, err := service.Prepare(operatorCtx, upgrade.PrepareRequest{ActorID: operatorID, ClientCommandID: "t36_prepare_operator", ExpectedRowVersion: 1}); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("operator error=%v", err)
	}
	var active int
	if err := database.SQL.QueryRow(`SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("maintenance after rejected prepares=%d err=%v", active, err)
	}
	if count := auditEvents(t, database.SQL, "upgrade.prepare"); count != 0 {
		t.Fatalf("rejected prepares recorded audit rows=%d", count)
	}
	// A foreign active maintenance window stays a deterministic business
	// conflict for the verified admin: the runner records the rejection
	// durably while the service maps the public ErrConflict back.
	if _, err := database.SQL.Exec(`UPDATE maintenance_state SET active=1,reason='Restore',entered_at=?,entered_by_type='system',row_version=row_version+1 WHERE id=1`, testNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_restore", ExpectedRowVersion: 2}); !errors.Is(err, upgrade.ErrConflict) {
		t.Fatalf("foreign maintenance error=%v", err)
	}
	if count := rejectedAudits(t, database.SQL, "upgrade.prepare"); count != 1 {
		t.Fatalf("conflicted prepare rejected audits=%d", count)
	}
	if count := rejectedLedgerRows(t, database.SQL, "upgrade.prepare"); count != 1 {
		t.Fatalf("conflicted prepare rejected ledger rows=%d", count)
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

// Ledger rows written by the pre-runner releases carry only their compact
// legacy JSON payload. Replaying such a row must keep the old contract —
// return the live projection, record nothing new.
func TestPrepareReplaysLegacyJSONLedgerRowsWithoutNewRecords(t *testing.T) {
	database, adminID := newFixture(t)
	defer database.Close()
	ctx := seedVerifiedSession(t, database.SQL, adminID)
	service := upgrade.NewService(database.SQL)
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_live", ExpectedRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	digest := auth.DigestCommand("upgrade.prepare", map[string]any{"expectedReason": "Upgrade", "expectedRowVersion": 1})
	if _, err := database.SQL.Exec(`INSERT INTO client_commands(principal_type,principal_id,client_command_id,command_type,request_digest,outcome,result_object_type,result_object_id,result_payload_json,created_at) VALUES('user',?,'t36_prepare_legacy','upgrade.prepare',?,'committed','maintenance',1,'{"reason":"Upgrade"}',?)`, adminID, digest, testNow()); err != nil {
		t.Fatal(err)
	}
	before := auditEvents(t, database.SQL, "upgrade.prepare")
	replayed, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_legacy", ExpectedRowVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The legacy payload cannot reconstruct the frozen checklist; the replay
	// falls back to the live projection exactly like the pre-runner contract.
	if !replayed.Active || replayed.RowVersion != 2 || len(replayed.Items) == 0 {
		t.Fatalf("legacy replay=%+v", replayed)
	}
	if after := auditEvents(t, database.SQL, "upgrade.prepare"); after != before {
		t.Fatalf("legacy replay recorded audit rows %d -> %d", before, after)
	}
	if count := ledgerRows(t, database.SQL, "upgrade.prepare"); count != 2 {
		t.Fatalf("legacy replay created ledger rows=%d", count)
	}
}

func TestReconcilerDrainsMarksBackupSafeAndProjectsPrepared(t *testing.T) {
	database, adminID := newFixture(t)
	defer database.Close()
	ctx := seedVerifiedSession(t, database.SQL, adminID)
	investigation, err := database.SQL.Exec(`INSERT INTO investigations(created_at) VALUES(?)`, testNow())
	if err != nil {
		t.Fatal(err)
	}
	investigationID, _ := investigation.LastInsertId()
	attemptID := seedAttempt(t, database.SQL, "investigation", "investigation", investigationID, "Queued")
	service := upgrade.NewService(database.SQL)
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0004", ExpectedRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	backups := &fakeBackups{db: database.SQL}
	var preparedSeen []bool
	reconciler := upgrade.NewReconciler(database.SQL, backups)
	reconciler.SetPrepared(func(prepared bool) { preparedSeen = append(preparedSeen, prepared) })
	// Work still blocking: no backup may be created.
	if prepared, err := reconciler.Reconcile(context.Background()); err != nil || prepared {
		t.Fatalf("prepared=%v err=%v", prepared, err)
	}
	if runs := backups.runs(); len(runs) != 0 {
		t.Fatalf("backup created while work blocking: %v", runs)
	}
	// The frozen upgrade-drain cancel commits the attempt's terminal state.
	if _, err := database.SQL.Exec(`UPDATE execution_attempts SET state='Cancelled',ended_at=?,termination_reason='cancelled',row_version=row_version+1 WHERE id=?`, testNow(), attemptID); err != nil {
		t.Fatal(err)
	}
	backupID := seedSucceededUpgradeBackup(t, database.SQL, adminID, "")
	prepared, err := reconciler.Reconcile(context.Background())
	if err != nil || !prepared {
		t.Fatalf("prepared=%v err=%v", prepared, err)
	}
	if safe, detail := maintenanceItem(t, database.SQL, 2, "ActiveAttempt", fmt.Sprintf("attempt/%d", attemptID)); safe != "Safe" || detail != "drained" {
		t.Fatalf("drained item=%s/%s", safe, detail)
	}
	if safe, detail := maintenanceItem(t, database.SQL, 2, "BackupPreflight", "pre_upgrade_backup"); safe != "Safe" || detail != "backup_verified" {
		t.Fatalf("backup item=%s/%s", safe, detail)
	}
	if len(preparedSeen) == 0 || !preparedSeen[len(preparedSeen)-1] {
		t.Fatalf("prepared gauge projections=%v", preparedSeen)
	}
	if backupID == 0 {
		t.Fatal("fixture backup id")
	}
}

func TestReconcilerSkipsTombstonedRunCheckAttempts(t *testing.T) {
	database, adminID := newFixture(t)
	defer database.Close()
	ctx := seedVerifiedSession(t, database.SQL, adminID)
	// A missed-schedule tombstone is a terminal (Failed) run_check attempt
	// with its runtime_unavailable gap already recorded; it is durable
	// history, not drainable work.
	runID := seedInspectionRun(t, database.SQL, adminID)
	tombstone := seedAttemptWithCheck(t, database.SQL, "inspection_collection", "run_check", runID, "Queued", "probe-check")
	mustExec(t, database.SQL, `UPDATE execution_attempts SET state='Failed',ended_at=?,row_version=row_version+1 WHERE id=?`, testNow(), tombstone)
	mustExec(t, database.SQL, `INSERT INTO inspection_check_results(run_id,check_key,status,gap_reason,attempt_id,created_at) VALUES(?,'probe-check','gap','runtime_unavailable',?,?)`, runID, tombstone, testNow())
	service := upgrade.NewService(database.SQL)
	state, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0005", ExpectedRowVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Items {
		if item.Kind == "ActiveAttempt" {
			t.Fatalf("tombstone projected as active work: %+v", item)
		}
	}
}

func TestReconcilerCreatesBackupOnlyAfterWorkClearsAndReArmsAfterFailure(t *testing.T) {
	database, adminID := newFixture(t)
	defer database.Close()
	ctx := seedVerifiedSession(t, database.SQL, adminID)
	service := upgrade.NewService(database.SQL)
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0006", ExpectedRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	backups := &fakeBackups{db: database.SQL, runErr: errors.New("storage full")}
	reconciler := upgrade.NewReconciler(database.SQL, backups)
	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs := backups.runs()
	if len(runs) != 1 {
		t.Fatalf("expected one backup run, got %v", runs)
	}
	// The failed run is durable; no automatic retry until a newer prepare.
	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs = backups.runs(); len(runs) != 1 {
		t.Fatalf("auto-retried after failure: %v", runs)
	}
	// The Admin re-arms with a new command id after fixing the cause.
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0007", ExpectedRowVersion: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs = backups.runs(); len(runs) != 2 {
		t.Fatalf("re-armed run missing: %v", runs)
	}
}

func TestSchemaGateRejectsUnsupportedVersions(t *testing.T) {
	ctx := context.Background()
	database, _ := newFixture(t)
	defer database.Close()
	db := database.SQL
	if _, err := upgrade.Preflight(ctx, db); !errors.Is(err, upgrade.ErrNotUpgradeMaintenance) {
		t.Fatalf("no maintenance error=%v", err)
	}
	enterVerified := func(t *testing.T) int64 {
		t.Helper()
		if _, err := db.Exec(`UPDATE maintenance_state SET active=1,reason='Upgrade',entered_at=?,entered_by_type='user',entered_by_id=1,row_version=row_version+1,exited_at=NULL,exited_by_type=NULL,exited_by_id=NULL WHERE id=1 AND active=0`, testNow()); err != nil {
			t.Fatal(err)
		}
		var revision int64
		if err := db.QueryRow(`SELECT row_version FROM maintenance_state WHERE id=1`).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?, 'BackupPreflight','pre_upgrade_backup','Safe','backup_verified',?)`, revision, testNow()); err != nil {
			t.Fatal(err)
		}
		return revision
	}
	revision := enterVerified(t)
	if _, err := upgrade.Preflight(ctx, db); !errors.Is(err, upgrade.ErrNoUpgradeBackup) {
		t.Fatalf("missing backup error=%v", err)
	}
	seedSucceededUpgradeBackup(t, db, 1, "")
	if _, err := db.Exec(`UPDATE schema_state SET schema_version='v0.9.0' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := upgrade.Preflight(ctx, db); !errors.Is(err, upgrade.ErrUnsupportedSchema) {
		t.Fatalf("unsupported version error=%v", err)
	}
	digest := sha256.Sum256([]byte("not the frozen schema"))
	if _, err := db.Exec(`UPDATE schema_state SET schema_version='v1',schema_digest=? WHERE id=1`, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := upgrade.Preflight(ctx, db); !errors.Is(err, upgrade.ErrSchemaDigestMismatch) {
		t.Fatalf("digest mismatch error=%v", err)
	}
	// Restore the exact frozen digest; the gate passes and Migrate exits the
	// fully-verified maintenance as the system actor. Then a second revision
	// with a fabricated migration ledger row proves the zero-history gate.
	realDigest := sha256.Sum256([]byte(gencontracts.SchemaSQL))
	if _, err := db.Exec(`UPDATE schema_state SET schema_digest=? WHERE id=1`, hex.EncodeToString(realDigest[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := upgrade.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var active int
	var reason sql.NullString
	var exitedBy string
	if err := db.QueryRow(`SELECT active,reason,exited_by_type FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &exitedBy); err != nil {
		t.Fatal(err)
	}
	if active != 0 || reason.Valid || exitedBy != "system" {
		t.Fatalf("maintenance after migrate active=%d reason=%v exitedBy=%s", active, reason, exitedBy)
	}
	if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES('legacy-001',?,'2020-01-01T00:00:00Z')`, hex.EncodeToString(make([]byte, 32))); err != nil {
		t.Fatal(err)
	}
	enterVerified(t)
	seedSucceededUpgradeBackup(t, db, 1, "")
	if _, err := upgrade.Preflight(ctx, db); !errors.Is(err, upgrade.ErrSchemaHistoryPresent) {
		t.Fatalf("ledger error=%v", err)
	}
	if revision == 0 {
		t.Fatal("revision")
	}
}

// The background reconcile is itself an audited system execution: a pass that
// can mutate the projection runs through the shared runner as the system
// principal with a scheduler source, while a quiet pass on a fully prepared
// window records nothing.
func TestReconcilerRecordsAuditedSystemPassesOnlyWhenWorkPending(t *testing.T) {
	database, adminID := newFixture(t)
	defer database.Close()
	ctx := seedVerifiedSession(t, database.SQL, adminID)
	investigation, err := database.SQL.Exec(`INSERT INTO investigations(created_at) VALUES(?)`, testNow())
	if err != nil {
		t.Fatal(err)
	}
	investigationID, _ := investigation.LastInsertId()
	seedAttempt(t, database.SQL, "investigation", "investigation", investigationID, "Queued")
	service := upgrade.NewService(database.SQL)
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0008", ExpectedRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	backups := &fakeBackups{db: database.SQL}
	reconciler := upgrade.NewReconciler(database.SQL, backups)
	// Blocking work: the pass can mutate and is audited as the system actor.
	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	var actorType string
	var actorID int64
	var correlation string
	var initiatorType string
	var initiatorID int64
	if err := database.SQL.QueryRow(`SELECT actor_type,actor_id,correlation_id,initiator_type,initiator_id FROM audit_events WHERE action='upgrade.reconcile' ORDER BY id DESC LIMIT 1`).Scan(&actorType, &actorID, &correlation, &initiatorType, &initiatorID); err != nil {
		t.Fatalf("reconcile audit row missing: %v", err)
	}
	if actorType != "system" || actorID != 0 {
		t.Fatalf("reconcile audit actor=%s/%d", actorType, actorID)
	}
	// The pass inherits the originating upgrade.prepare operation: its user
	// correlation and initiator, with the system principal as the acting
	// executor — never a fresh per-pass identity.
	var prepareCorrelation string
	if err := database.SQL.QueryRow(`SELECT correlation_id FROM audit_events WHERE action='upgrade.prepare' AND outcome='success'`).Scan(&prepareCorrelation); err != nil {
		t.Fatal(err)
	}
	if correlation != prepareCorrelation || prepareCorrelation == "" {
		t.Fatalf("reconcile correlation=%q want originating prepare %q", correlation, prepareCorrelation)
	}
	if initiatorType != "user" || initiatorID != adminID {
		t.Fatalf("reconcile initiator=%s/%d want user/%d", initiatorType, initiatorID, adminID)
	}
	// Settle the window completely: drained work plus a succeeded backup.
	if _, err := database.SQL.Exec(`UPDATE execution_attempts SET state='Cancelled',ended_at=?,termination_reason='cancelled',row_version=row_version+1 WHERE attempt_type='investigation'`, testNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The fully prepared window can no longer mutate: quiet passes are not
	// audited, no matter how often the loop ticks.
	for i := 0; i < 3; i++ {
		prepared, err := reconciler.Reconcile(context.Background())
		if err != nil || !prepared {
			t.Fatalf("quiet pass prepared=%v err=%v", prepared, err)
		}
	}
	var reconcileRows, userRows int
	if err := database.SQL.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN actor_type='user' THEN 1 ELSE 0 END),0) FROM audit_events WHERE action='upgrade.reconcile'`).Scan(&reconcileRows, &userRows); err != nil {
		t.Fatal(err)
	}

	if reconcileRows != 3 || userRows != 0 {
		t.Fatalf("reconcile audit rows=%d user-attributed=%d", reconcileRows, userRows)
	}
}
