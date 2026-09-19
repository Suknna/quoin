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

// unifiedMTLSFixture opens the byte-exact unified-mTLS predecessor capture
// （上一发布——注册制时代的最后一个版本——的 canonical schema）并写入其钉死的身份。
func unifiedMTLSFixture(t *testing.T) *sql.DB {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "unified-mtls-predecessor.sql.gz"))
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
	if hex.EncodeToString(digest[:]) != unifiedMTLSPredecessorSchemaDigest {
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
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-09-19T00:00:00Z')`, unifiedMTLSPredecessorSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedRetiredChecklistItems plants the retired-subsystem checklist rows the
// conversion must clean before the narrowed maintenance_items.kind applies.
func seedRetiredChecklistItems(t *testing.T, db *sql.DB, revision int64) {
	t.Helper()
	retired := []struct{ kind, key string }{
		{"RuntimeSlot", "slot/plinth"},
		{"LintelRecoveryFence", "lintel/1"},
	}
	for _, item := range retired {
		if _, err := db.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,'Safe','drained','2026-09-19T00:00:00Z')`,
			revision, item.kind, item.key); err != nil {
			t.Fatal(err)
		}
	}
}

// seedUnifiedMTLSState plants predecessor-shaped history the conversion must
// handle: the seeded runtime slots and business history that must survive
// verbatim.
func seedUnifiedMTLSState(t *testing.T, db *sql.DB, revision int64) {
	t.Helper()
	const now = "2026-09-19T00:00:00Z"
	statements := []string{
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Administrator','admin',1,1,'fixture-hash',1,'" + now + "','" + now + "')",
		"INSERT INTO alert_sources(id,source_key,protocol,enabled,row_version,created_at) VALUES(1,'am-prod','alertmanager',1,1,'" + now + "')",
		// 注册制遗留：bootstrap 播种的 slot 行（迁移后随表删除）。
		"INSERT INTO runtime_slots(slot,state,row_version,created_at) VALUES('plinth','unregistered',1,'" + now + "')",
		"INSERT INTO runtime_slots(slot,state,row_version,created_at) VALUES('lintel','unregistered',1,'" + now + "')",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnifiedMTLSDropsRegistrationAuthority(t *testing.T) {
	for _, converted := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-predecessor", true: "converted-from-attribution"}[converted], func(t *testing.T) {
			db := unifiedMTLSFixture(t)
			seedUnifiedMTLSState(t, db, 0)
			revision := seedAuthAuditUpgradeWindow(t, db, 1)
			seedRetiredChecklistItems(t, db, revision)
			if converted {
				if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`,
					alertViewAttributionMigrationID, migrationDigest(alertViewAttributionMigrationID), migrationNow()); err != nil {
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
			// 注册制与 Lintel 恢复记账被整体删除。
			for _, table := range []string{"runtime_slots", "runtime_credentials", "lintel_recovery_receipts"} {
				var present int
				if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&present); err != nil || present != 0 {
					t.Fatalf("retired table %s still present: %d %v", table, present, err)
				}
			}
			// 退役子系统 checklist 历史行被清理；普通项与业务历史逐字保留。
			var retiredItems int
			if err := db.QueryRow(`SELECT COUNT(*) FROM maintenance_items WHERE kind IN ('RuntimeSlot','LintelRecoveryFence')`).Scan(&retiredItems); err != nil || retiredItems != 0 {
				t.Fatalf("retired checklist items survived: %d %v", retiredItems, err)
			}
			var backupItem int
			if err := db.QueryRow(`SELECT COUNT(*) FROM maintenance_items WHERE kind='BackupPreflight' AND object_key='pre_upgrade_backup'`).Scan(&backupItem); err != nil || backupItem != 1 {
				t.Fatalf("preserved checklist item lost: %d %v", backupItem, err)
			}
			var sourceKey string
			if err := db.QueryRow(`SELECT source_key FROM alert_sources WHERE id=1`).Scan(&sourceKey); err != nil || sourceKey != "am-prod" {
				t.Fatalf("business history changed: %q %v", sourceKey, err)
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
				unifiedMTLSMigrationID, migrationDigest(unifiedMTLSMigrationID)).Scan(&ledger); err != nil || ledger != 1 {
				t.Fatalf("authentic unified-mTLS ledger missing: %d %v", ledger, err)
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

func TestUnifiedMTLSRejectsForgedHistory(t *testing.T) {
	db := unifiedMTLSFixture(t)
	seedUnifiedMTLSState(t, db, 0)
	revision := seedAuthAuditUpgradeWindow(t, db, 1)
	seedRetiredChecklistItems(t, db, revision)
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
	if digest != unifiedMTLSPredecessorSchemaDigest {
		t.Fatal("failed migration changed schema")
	}
	var slots int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runtime_slots`).Scan(&slots); err != nil || slots != 2 {
		t.Fatalf("failed migration changed data: %d %v", slots, err)
	}
}
