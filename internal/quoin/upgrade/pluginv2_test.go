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
	"strings"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// pluginV2Fixture opens the byte-exact predecessor capture（ADR-0011 前的最后
// 一个验收版本：execution_mode CHECK 仍为 worker_local|supervisor_typed）。
func pluginV2Fixture(t *testing.T) *sql.DB {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "plugin-v2-predecessor.sql.gz"))
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
	if hex.EncodeToString(digest[:]) != pluginV2PredecessorSchemaDigest {
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
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-09-20T00:00:00Z')`, pluginV2PredecessorSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPluginV2ExecutionModeVocabulary(t *testing.T) {
	db := pluginV2Fixture(t)
	const now = "2026-09-20T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Administrator','admin',1,1,'fixture-hash',1,'` + now + `','` + now + `')`); err != nil {
		t.Fatal(err)
	}
	seedAuthAuditUpgradeWindow(t, db, 1)
	if _, err := Preflight(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	// 重建后的 DDL 携带三值词表：quoin_routed 可写，supervisor_typed 保留给
	// 历史行（rebuildCanonicalSchema 的行级保留语义由既有迁移测试覆盖）。
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='tool_calls'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	for _, vocabulary := range []string{"'worker_local'", "'supervisor_typed'", "'quoin_routed'"} {
		if !strings.Contains(ddl, vocabulary) {
			t.Fatalf("tool_calls CHECK lost %s: %s", vocabulary, ddl)
		}
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
		pluginV2MigrationID, migrationDigest(pluginV2MigrationID)).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("plugin v2 ledger missing: %d %v", ledger, err)
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
