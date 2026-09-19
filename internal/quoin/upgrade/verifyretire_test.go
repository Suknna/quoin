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

// verifyRetireFixture opens the byte-exact predecessor capture（验证/刷新引擎仍
// 在 schema 中的最后一个发布版本）并写入其钉死的身份。
func verifyRetireFixture(t *testing.T) *sql.DB {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "verify-retire-predecessor.sql.gz"))
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
	if hex.EncodeToString(digest[:]) != verifyRetirePredecessorSchemaDigest {
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
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-09-19T00:00:00Z')`, verifyRetirePredecessorSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedVerifyRetireState plants predecessor-shaped engine history the conversion
// must retire: one PromQL config verification run with its child attempt and
// one resource refresh run with its discovery attempt, built through the
// predecessor's own closure triggers (draft target, published refresh source).
func seedVerifyRetireState(t *testing.T, db *sql.DB) {
	t.Helper()
	const now = "2026-09-19T00:00:00Z"
	statements := []string{
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Administrator','admin',1,1,'fixture-hash',1,'" + now + "','" + now + "')",
		"INSERT INTO connections(id,name,type,enabled,row_version,created_at) VALUES(1,'metrics','thanos',1,1,'" + now + "')",
		"INSERT INTO business_systems(id,key,display_name,enabled,row_version,created_at) VALUES(1,'mall','Mall',0,1,'" + now + "')",
		// v1：验证目标草稿（plan/check 先落，run 后建——parent-frozen 触发器要求）。
		"INSERT INTO business_system_config_versions(id,business_system_id,version_seq,state,yaml_body,parser_version,schema_version,digest,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(1,1,1,'draft','yaml','p1','s1','3636363636363636363636363636363636363636363636363636363636363636','" + now + "','mall','Mall',1,1,'UTC')",
		"INSERT INTO config_plans(id,config_version_id,plan_key,display_name) VALUES(1,1,'p1','Plan')",
		"INSERT INTO config_checks(id,plan_id,check_key,display_name,analysis_question,kind,query_mode,expression) VALUES(1,1,'c1','Check','healthy?','promql','instant','up')",
		"INSERT INTO config_verification_runs(id,purpose,business_system_id,config_version_id,state,row_version,created_by,created_at) VALUES(1,'prepublish',1,1,'Queued',1,1,'" + now + "')",
		"UPDATE config_verification_runs SET state='Running',evidence_at='" + now + "',row_version=row_version+1 WHERE id=1",
		"INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,plan_key,check_key,state,quoin_release_version,created_at) VALUES(10,'inspection_collection','config_verification_run',1,'p1','c1','Queued','v1','" + now + "')",
		// v2：发布后作为 refresh 源。
		"INSERT INTO business_system_config_versions(id,business_system_id,version_seq,state,yaml_body,parser_version,schema_version,digest,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(2,1,2,'draft','yaml2','p1','s1','3535353535353535353535353535353535353535353535353535353535353535','" + now + "','mall','Mall v2',1,1,'UTC')",
		"INSERT INTO config_discoveries(id,config_version_id,discovery_key,display_name,selector,identity_labels_json) VALUES(1,2,'pods','Pods','up','[]')",
		"UPDATE business_systems SET current_config_version_id=2,display_name='Mall v2',enabled=1,timezone='UTC',row_version=row_version+1 WHERE id=1",
		"INSERT INTO resource_refresh_runs(id,business_system_id,config_version_id,trigger_kind,state,row_version,created_by,created_at) VALUES(1,1,2,'manual','Queued',1,1,'" + now + "')",
		"INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,discovery_key,state,quoin_release_version,created_at) VALUES(11,'inspection_collection','resource_refresh_run',1,'pods','Queued','v1','" + now + "')",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func TestVerifyRetireDropsEnginePersistence(t *testing.T) {
	db := verifyRetireFixture(t)
	seedVerifyRetireState(t, db)
	seedAuthAuditUpgradeWindow(t, db, 1)
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
	// 引擎持久面整体删除。
	for _, table := range []string{"config_verification_runs", "config_verification_discovery_results", "config_verification_run_check_results", "resource_refresh_runs", "observed_refresh_log"} {
		var present int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&present); err != nil || present != 0 {
			t.Fatalf("retired table %s still present: %d %v", table, present, err)
		}
	}
	// 死 scope 的存量 attempt 行被清理；业务历史逐字保留。
	var deadAttempts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_type IN ('config_verification_run','resource_refresh_run')`).Scan(&deadAttempts); err != nil || deadAttempts != 0 {
		t.Fatalf("dead-scope attempts survived: %d %v", deadAttempts, err)
	}
	var versionStates string
	if err := db.QueryRow(`SELECT group_concat(state, ',') FROM (SELECT state FROM business_system_config_versions ORDER BY id)`).Scan(&versionStates); err != nil || versionStates != "draft,published" {
		t.Fatalf("config version history changed: %q %v", versionStates, err)
	}
	var systemKey string
	if err := db.QueryRow(`SELECT key FROM business_systems WHERE id=1`).Scan(&systemKey); err != nil || systemKey != "mall" {
		t.Fatalf("business history changed: %q %v", systemKey, err)
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
		verifyRetireMigrationID, migrationDigest(verifyRetireMigrationID)).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("authentic verify-retire ledger missing: %d %v", ledger, err)
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
}

func TestVerifyRetireRejectsForgedHistory(t *testing.T) {
	db := verifyRetireFixture(t)
	seedVerifyRetireState(t, db)
	seedAuthAuditUpgradeWindow(t, db, 1)
	if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES('forged',printf('%064d',0),'2026-09-19T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(context.Background(), db); !errors.Is(err, ErrSchemaHistoryPresent) {
		t.Fatalf("got=%v want=%v", err, ErrSchemaHistoryPresent)
	}
	var digest string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state`).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != verifyRetirePredecessorSchemaDigest {
		t.Fatal("failed migration changed schema")
	}
	var attempts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_type IN ('config_verification_run','resource_refresh_run')`).Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("failed migration changed data: %d %v", attempts, err)
	}
}
