package upgrade

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// inspectionFreezeFixture opens the byte-exact inspection-freeze predecessor
// capture and stamps its pinned identity into schema_state.
func inspectionFreezeFixture(t *testing.T) *sql.DB {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "inspection-freeze-predecessor.sql.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	schema, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	if hex.EncodeToString(digest[:]) != inspectionFreezePredecessorSchemaDigest {
		t.Fatalf("predecessor identity differs: %x", digest)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "predecessor.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-09-16T00:00:00Z')`, inspectionFreezePredecessorSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedInspectionFreezeState plants the predecessor-shaped inspection history
// the conversion must preserve verbatim: one plan, one frozen plan run with a
// check row, and one analysis attempt whose input snapshot has no override row.
func seedInspectionFreezeState(t *testing.T, db *sql.DB) {
	t.Helper()
	const now = "2026-09-16T00:00:00Z"
	statements := []string{
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,auth_revision,row_version,created_at,updated_at) VALUES(1,'admin','Administrator','admin',1,1,'fixture-hash',1,1,'" + now + "','" + now + "')",
		"INSERT INTO connections(id,name,type,enabled,row_version,created_at) VALUES(1,'fixture-metrics','prometheus',1,1,'" + now + "')",
		"INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_at,updated_at) " +
			"VALUES('legacy-plan','历史计划',1,1,'prometheus','promql_instant','1','{\"expression\":\"up\"}','{\"kind\":\"integration\"}','integration',NULL,'UTC',1,'" + now + "','" + now + "')",
		"INSERT INTO inspection_runs(plan_id,plan_key,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,trigger_kind,state,row_version,created_at) " +
			"VALUES(1,'legacy-plan',1,'prometheus','promql_instant','1','{\"expression\":\"up\"}','{\"kind\":\"integration\"}','manual','Queued',1,'" + now + "')",
		"UPDATE inspection_runs SET state='Running',evidence_at='" + now + "',row_version=row_version+1 WHERE id=1 AND state='Queued'",
		"INSERT INTO inspection_run_checks(run_id,check_key,display_name,plugin_id,template_id,template_version,params_json,created_at) " +
			"VALUES(1,'promql_instant','连通巡检','prometheus','promql_instant','1','{\"expression\":\"up\"}','" + now + "')",
		"INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,created_at) " +
			"VALUES('inspection_collection','run_check',1,'promql_instant','Queued','test','" + now + "')",
		"INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) " +
			"VALUES(1,'inspection_plugin_execution_v1','v1','" + hex.EncodeToString(make([]byte, 32)) + "','" + now + "')",
		"UPDATE execution_attempts SET state='Failed',ended_at='" + now + "',row_version=row_version+1 WHERE id=1 AND state='Queued'",
		"INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at) " +
			"VALUES(1,'promql_instant','gap',NULL,1,NULL,'runtime_unavailable','" + now + "')",
		"UPDATE inspection_runs SET state='CompletedWithGaps',row_version=row_version+1 WHERE id=1 AND state='Running'",
		"INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) " +
			"VALUES('inspection_analysis','run',1,'Queued','test','" + now + "')",
		"INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,inspection_report_version,created_at) " +
			"VALUES(2,'inspection_analysis_v1','v1','" + hex.EncodeToString(make([]byte, 32)) + "',1,'" + now + "')",
		"INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,correlation_id,created_at) VALUES('user',1,'retained.history','success','inspection_run',1,'freeze-history','" + now + "')",
		"UPDATE sqlite_sequence SET seq=1000 WHERE name='audit_events'",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInspectionFreezePreservesHistoryAndAddsAnalysisColumns(t *testing.T) {
	for _, converted := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-predecessor", true: "converted-from-simplification"}[converted], func(t *testing.T) {
			db := inspectionFreezeFixture(t)
			seedInspectionFreezeState(t, db)
			seedAuthAuditUpgradeWindow(t, db, 1)
			if converted {
				if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`,
					authSimplificationMigrationID, migrationDigest(authSimplificationMigrationID), migrationNow()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Preflight(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			result, err := Migrate(context.Background(), db)
			if err != nil {
				t.Fatal(err)
			}
			if result.BackupID != 1 || len(result.ManifestSHA256) != 64 {
				t.Fatalf("migration summary lost backup provenance: %+v", result)
			}
			// 历史事实逐字保留：计划/Run/检查行不因加列改变任何既有值。
			var planName, runParams, checkName string
			if err := db.QueryRow(`SELECT display_name FROM inspection_plans WHERE plan_key='legacy-plan'`).Scan(&planName); err != nil || planName != "历史计划" {
				t.Fatalf("plan history changed: %q %v", planName, err)
			}
			if err := db.QueryRow(`SELECT frozen_params_json FROM inspection_runs WHERE id=1`).Scan(&runParams); err != nil || runParams != `{"expression":"up"}` {
				t.Fatalf("run frozen params changed: %q %v", runParams, err)
			}
			if err := db.QueryRow(`SELECT display_name FROM inspection_run_checks WHERE run_id=1 AND check_key='promql_instant'`).Scan(&checkName); err != nil || checkName != "连通巡检" {
				t.Fatalf("check history changed: %q %v", checkName, err)
			}
			// 新列存在且既有行为 NULL；新表存在且旧 Attempt 无行。
			var description, unit, instructions sql.NullString
			if err := db.QueryRow(`SELECT check_description,metric_unit,report_instructions FROM inspection_plans WHERE plan_key='legacy-plan'`).Scan(&description, &unit, &instructions); err != nil {
				t.Fatal(err)
			}
			if description.Valid || unit.Valid || instructions.Valid {
				t.Fatalf("predecessor rows must not grow semantics: %+v %+v %+v", description, unit, instructions)
			}
			var displayName, frozenDescription sql.NullString
			if err := db.QueryRow(`SELECT frozen_display_name,frozen_check_description FROM inspection_runs WHERE id=1`).Scan(&displayName, &frozenDescription); err != nil {
				t.Fatal(err)
			}
			var requirements int
			if err := db.QueryRow(`SELECT COUNT(*) FROM inspection_analysis_requirements WHERE attempt_id=2`).Scan(&requirements); err != nil {
				t.Fatal(err)
			}
			if requirements != 0 {
				t.Fatal("predecessor analysis attempt must not gain a requirements row")
			}
			var stored string
			if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(gen.SchemaSQL))
			if stored != hex.EncodeToString(digest[:]) {
				t.Fatal("target digest not stamped")
			}
			var ledger int
			if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger WHERE migration_id=? AND digest=?`,
				inspectionFreezeMigrationID, migrationDigest(inspectionFreezeMigrationID)).Scan(&ledger); err != nil || ledger != 1 {
				t.Fatalf("authentic freeze ledger missing: %d %v", ledger, err)
			}
			if _, err := Migrate(context.Background(), db); !errors.Is(err, ErrNotUpgradeMaintenance) {
				t.Fatalf("completed migration retry=%v", err)
			}
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := verifySchemaGate(context.Background(), conn, &PreflightResult{}); err != nil {
				t.Fatalf("authentic migrated canonical rejected: %v", err)
			}
		})
	}
}

func TestInspectionFreezeRejectsMissingBackupAndForgedHistory(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-backup", true: "forged-history"}[forged], func(t *testing.T) {
			db := inspectionFreezeFixture(t)
			seedInspectionFreezeState(t, db)
			want := ErrNotUpgradeMaintenance
			if !forged {
				if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
					t.Fatal(err)
				}
			}
			if forged {
				seedAuthAuditUpgradeWindow(t, db, 1)
				if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES('forged',printf('%064d',0),'2026-09-16T00:00:00Z')`); err != nil {
					t.Fatal(err)
				}
				want = ErrSchemaHistoryPresent
			}
			if _, err := Migrate(context.Background(), db); !errors.Is(err, want) {
				t.Fatalf("got=%v want=%v", err, want)
			}
			var digest string
			if err := db.QueryRow(`SELECT schema_digest FROM schema_state`).Scan(&digest); err != nil {
				t.Fatal(err)
			}
			if digest != inspectionFreezePredecessorSchemaDigest {
				t.Fatal("failed migration changed schema")
			}
			var plans int
			if err := db.QueryRow(`SELECT COUNT(*) FROM inspection_plans`).Scan(&plans); err != nil || plans != 1 {
				t.Fatalf("failed migration changed data: %d %v", plans, err)
			}
		})
	}
}
