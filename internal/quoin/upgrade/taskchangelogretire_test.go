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

// taskChangeLogRetireFixture opens the byte-exact predecessor capture（仍带
// task_change_log 的最后一个发布版本）并写入其钉死的身份。
func taskChangeLogRetireFixture(t *testing.T) *sql.DB {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "task-change-log-retire-predecessor.sql.gz"))
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
	if hex.EncodeToString(digest[:]) != taskChangeLogRetirePredecessorSchemaDigest {
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
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-09-19T00:00:00Z')`, taskChangeLogRetirePredecessorSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedTaskChangeLogState plants predecessor history whose derived log rows the
// conversion must drop while the authoritative rows survive verbatim: an
// analysis (its task_change_log rows exist via the write triggers) plus an
// alert occurrence proving the sibling alert_change_log stays untouched.
func seedTaskChangeLogState(t *testing.T, db *sql.DB) {
	t.Helper()
	const now = "2026-09-19T00:00:00Z"
	statements := []string{
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Administrator','admin',1,1,'fixture-hash',1,'" + now + "','" + now + "')",
		"INSERT INTO alert_sources(id,source_key,protocol,enabled,row_version,created_at) VALUES(1,'am-prod','alertmanager',1,1,'" + now + "')",
		"INSERT INTO alert_occurrences(id,source_id,fingerprint,starts_at,state,row_version,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(1,1,x'0102030405060708','2026-09-19T00:00:00Z','Firing',1,'{}','" + "6464646464646464646464646464646464646464646464646464646464646464" + "','" + now + "','" + now + "')",
		// 一条真实权威对象：插入即在 predecessor 上派生 task_change_log 行。
		"INSERT INTO initial_analyses(id,occurrence_id,state,input_snapshot_digest,row_version,created_by,created_at) VALUES(1,1,'Queued','" + "6565656565656565656565656565656565656565656565656565656565656565" + "',1,1,'" + now + "')",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	var derived int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_change_log WHERE object_type='initial_analysis' AND object_id=1`).Scan(&derived); err != nil || derived == 0 {
		t.Fatalf("predecessor write triggers produced no log rows: %d %v", derived, err)
	}
}

func TestTaskChangeLogRetireDropsUnreadLog(t *testing.T) {
	db := taskChangeLogRetireFixture(t)
	seedTaskChangeLogState(t, db)
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
	// 派生日志整体删除；写触发器不再存在。
	var present int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'trg_task_change_log%' OR name = 'task_change_log'`).Scan(&present); err != nil || present != 0 {
		t.Fatalf("task_change_log objects survived: %d %v", present, err)
	}
	// 权威对象与其 row_version 触发器逐字保留。
	var analysisState string
	if err := db.QueryRow(`SELECT state FROM initial_analyses WHERE id=1`).Scan(&analysisState); err != nil || analysisState != "Queued" {
		t.Fatalf("authoritative history changed: %q %v", analysisState, err)
	}
	if _, err := db.Exec(`UPDATE initial_analyses SET state='Running',row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatalf("row_version concurrency fence lost: %v", err)
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
		taskChangeLogRetireMigrationID, migrationDigest(taskChangeLogRetireMigrationID)).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("authentic task-change-log ledger missing: %d %v", ledger, err)
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

func TestTaskChangeLogRetireRejectsForgedHistory(t *testing.T) {
	db := taskChangeLogRetireFixture(t)
	seedTaskChangeLogState(t, db)
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
	if digest != taskChangeLogRetirePredecessorSchemaDigest {
		t.Fatal("failed migration changed schema")
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_change_log`).Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("failed migration changed data: %d %v", rows, err)
	}
}
