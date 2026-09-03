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

	"github.com/Suknna/quoin/internal/quoin/config"
	"github.com/Suknna/quoin/internal/contract"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/labelcontract"
	"github.com/Suknna/quoin/internal/quoin/upgrade"
)

func testNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func newFixture(t *testing.T) (*bootstrap.Database, int64) {
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

// seedInspectionRun builds the minimal published inspection chain so a
// run_check child can exist under the frozen scope trigger. The label
// contract activates through the real domain command because its derived
// state is trigger-owned.
func seedInspectionRun(t *testing.T, db *sql.DB, adminID int64) int64 {
	t.Helper()
	now := testNow()
	digest64 := hex.EncodeToString(make([]byte, 32))
	contracts := labelcontract.NewService(db)
	draft, err := contracts.CreateDraft(context.Background(), adminID, "t36-contract-0001", []byte("label_contract:\n  business_system_label: business_system\n"), config.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	currentContractID, stateRowVersion, err := contracts.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.Activate(context.Background(), adminID, "t36-contract-0002", labelcontract.ActivateInput{ContractVersion: draft.Version, ExpectedStateRowVersion: stateRowVersion, ExpectedCurrentContractID: currentContractID, ExpectedTargetRowVersion: draft.RowVersion}); err != nil {
		t.Fatal(err)
	}
	system, err := db.Exec(`INSERT INTO business_systems(key,display_name,enabled,created_at) VALUES('t36-system','T36 System',0,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	systemID, _ := system.LastInsertId()
	version, err := db.Exec(`INSERT INTO business_system_config_versions(business_system_id,system_key,display_name,enabled,timezone,resource_refresh_interval_seconds,version_seq,state,yaml_body,parser_version,schema_version,label_contract_version_id,journey_catalog_digest,journey_catalog_version,digest,created_at) VALUES(?, 't36-system','T36 System',1,'UTC',3600,1,'draft','body','p','v1',?,?,'cat-v1',?,?)`, systemID, draft.Version, digest64, digest64, now)
	if err != nil {
		t.Fatal(err)
	}
	versionID, _ := version.LastInsertId()
	plan, err := db.Exec(`INSERT INTO config_plans(config_version_id,plan_key,display_name) VALUES(?, 'nightly','Nightly')`, versionID)
	if err != nil {
		t.Fatal(err)
	}
	planID, _ := plan.LastInsertId()
	mustExec(t, db, `INSERT INTO config_checks(plan_id,check_key,display_name,analysis_question,kind,query_mode,expression) VALUES(?, 'probe-check','Probe','?','promql','instant','up')`, planID)
	// Moving the current pointer onto the same-system unpublished draft
	// publishes it through the frozen owner trigger; the root projection
	// columns must travel in the same UPDATE.
	mustExec(t, db, `UPDATE business_systems SET enabled=1,current_config_version_id=?,display_name='T36 System',timezone='UTC',resource_refresh_interval_seconds=3600,row_version=row_version+1 WHERE id=?`, versionID, systemID)
	run, err := db.Exec(`INSERT INTO inspection_runs(business_system_id,plan_key,config_version_id,label_contract_version_id,trigger_kind,state,created_at) VALUES(?, 'nightly',?,?, 'manual','Queued',?)`, systemID, versionID, draft.Version, now)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := run.LastInsertId()
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
	// Emulate the real terminal transition so the admission window moves on.
	status := "succeeded"
	if runErr != nil {
		status = "failed"
	}
	_, _ = fake.db.ExecContext(ctx, `UPDATE backups SET status=?,stage=CASE WHEN ?='succeeded' THEN 'completed' ELSE stage END,error_code=CASE WHEN ?='succeeded' THEN error_code ELSE 'storage_failure' END,retryable=0,error_detail=CASE WHEN ?='succeeded' THEN error_detail ELSE 'fixture failure' END,completed_at=?,updated_at=?,row_version=row_version+1 WHERE id=?`, status, status, status, status, testNow(), testNow(), id)
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
	ctx := context.Background()
	database, adminID := newFixture(t)
	defer database.Close()
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
	// Idempotent replay returns the same frozen revision.
	replayed, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0001", ExpectedRowVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.RowVersion != 2 || len(replayed.Items) != len(state.Items) {
		t.Fatalf("replayed=%+v", replayed)
	}
}

func TestPrepareRejectsOperatorsAndForeignMaintenance(t *testing.T) {
	ctx := context.Background()
	database, adminID := newFixture(t)
	defer database.Close()
	if _, err := database.SQL.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES('op','Op','operator',1,'x',1,?,?)`, testNow(), testNow()); err != nil {
		t.Fatal(err)
	}
	var operatorID int64
	if err := database.SQL.QueryRow(`SELECT id FROM users WHERE username='op'`).Scan(&operatorID); err != nil {
		t.Fatal(err)
	}
	service := upgrade.NewService(database.SQL)
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: operatorID, ClientCommandID: "t36_prepare_0002", ExpectedRowVersion: 1}); !errors.Is(err, upgrade.ErrConflict) {
		t.Fatalf("operator error=%v", err)
	}
	if _, err := database.SQL.Exec(`UPDATE maintenance_state SET active=1,reason='Restore',entered_at=?,entered_by_type='system',row_version=row_version+1 WHERE id=1`, testNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0003", ExpectedRowVersion: 2}); !errors.Is(err, upgrade.ErrConflict) {
		t.Fatalf("foreign maintenance error=%v", err)
	}
}

func TestReconcilerDrainsMarksBackupSafeAndProjectsPrepared(t *testing.T) {
	ctx := context.Background()
	database, adminID := newFixture(t)
	defer database.Close()
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
	if prepared, err := reconciler.Reconcile(ctx); err != nil || prepared {
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
	prepared, err := reconciler.Reconcile(ctx)
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
	ctx := context.Background()
	database, adminID := newFixture(t)
	defer database.Close()
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
	ctx := context.Background()
	database, adminID := newFixture(t)
	defer database.Close()
	service := upgrade.NewService(database.SQL)
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0006", ExpectedRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	backups := &fakeBackups{db: database.SQL, runErr: errors.New("storage full")}
	reconciler := upgrade.NewReconciler(database.SQL, backups)
	if _, err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	runs := backups.runs()
	if len(runs) != 1 {
		t.Fatalf("expected one backup run, got %v", runs)
	}
	// The failed run is durable; no automatic retry until a newer prepare.
	if _, err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if runs = backups.runs(); len(runs) != 1 {
		t.Fatalf("auto-retried after failure: %v", runs)
	}
	// The Admin re-arms with a new command id after fixing the cause.
	if _, err := service.Prepare(ctx, upgrade.PrepareRequest{ActorID: adminID, ClientCommandID: "t36_prepare_0007", ExpectedRowVersion: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx); err != nil {
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
