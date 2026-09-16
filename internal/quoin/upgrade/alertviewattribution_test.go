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

// alertViewAttributionFixture opens the byte-exact alert-view-attribution
// predecessor capture（上一发布的 canonical schema）并写入其钉死的身份。
func alertViewAttributionFixture(t *testing.T) *sql.DB {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "alert-view-attribution-predecessor.sql.gz"))
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
	if hex.EncodeToString(digest[:]) != alertViewAttributionPredecessorSchemaDigest {
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
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-09-16T00:00:00Z')`, alertViewAttributionPredecessorSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedAlertViewAttributionState plants predecessor-shaped alert and business
// view history the conversion must preserve verbatim: one alert source with a
// credential, one delivery with its item and occurrence, one legacy
// attribution row, and one business view without the new source-scope column.
func seedAlertViewAttributionState(t *testing.T, db *sql.DB) {
	t.Helper()
	const now = "2026-09-16T00:00:00Z"
	statements := []string{
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Administrator','admin',1,1,'fixture-hash',1,'" + now + "','" + now + "')",
		"INSERT INTO alert_sources(id,source_key,protocol,enabled,row_version,created_at) VALUES(1,'am-prod','alertmanager',1,1,'" + now + "')",
		"INSERT INTO alert_source_credentials(id,source_id,digest,state,row_version,created_at) VALUES(1,1,zeroblob(32),'Active',1,'" + now + "')",
		"INSERT INTO alert_deliveries(id,relay_id,source_id,credential_id,credential_snapshot_version,protocol,body,body_size_bytes,integrity,status,received_at,committed_at) VALUES(1,'relay-1',1,1,1,'alertmanager',zeroblob(16),16,'complete','processed','" + now + "','" + now + "')",
		"INSERT INTO alert_delivery_items(id,delivery_id,item_index,status,fingerprint,starts_at,labels_canonical) VALUES(1,1,0,'ok',x'0011223344556677','" + now + "','{\"alertname\":\"Latency\"}')",
		"INSERT INTO alert_occurrences(id,source_id,fingerprint,starts_at,state,row_version,labels_canonical,labels_digest,business_system_id,first_seen_at,last_state_change_at) VALUES(1,1,x'0011223344556677','" + now + "','Firing',1,'{\"alertname\":\"Latency\"}','" + hex.EncodeToString(make([]byte, 32)) + "',NULL,'" + now + "','" + now + "')",
		"INSERT INTO alert_occurrence_attributions(occurrence_id,status,candidate_system_ids_json,candidate_config_version_ids_json,reason_json,evaluated_from_delivery_id,evaluated_from_delivery_item_id,created_at) VALUES(1,'unattributed','[]','[]','{\"code\":\"source_mismatch\"}',1,1,'" + now + "')",
		"INSERT INTO business_views(id,view_key,display_name,description,connection_id,label_conditions_json,row_version,created_at,updated_at) VALUES(1,'payments-prod','支付生产','历史视图',NULL,'{\"service\":\"payments\"}',1,'" + now + "','" + now + "')",
		"INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,correlation_id,created_at) VALUES('user',1,'retained.history','success','alert_occurrence',1,'alert-view-history','" + now + "')",
		"UPDATE sqlite_sequence SET seq=1000 WHERE name='audit_events'",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAlertViewAttributionPreservesAlertAndViewHistory(t *testing.T) {
	for _, converted := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-predecessor", true: "converted-from-freeze"}[converted], func(t *testing.T) {
			db := alertViewAttributionFixture(t)
			seedAlertViewAttributionState(t, db)
			seedAuthAuditUpgradeWindow(t, db, 1)
			if converted {
				if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`,
					inspectionFreezeMigrationID, migrationDigest(inspectionFreezeMigrationID), migrationNow()); err != nil {
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
			// 旧告警与视图历史逐字保留：occurrence、旧归属证据、视图行不因加列
			// 改变任何既有值，也绝不伪造旧 business_system 归属。
			var state, labels string
			var businessID sql.NullInt64
			if err := db.QueryRow(`SELECT state,labels_canonical,business_system_id FROM alert_occurrences WHERE id=1`).Scan(&state, &labels, &businessID); err != nil {
				t.Fatal(err)
			}
			if state != "Firing" || labels != `{"alertname":"Latency"}` || businessID.Valid {
				t.Fatalf("occurrence history changed: state=%s labels=%s business=%+v", state, labels, businessID)
			}
			var legacyReason, candidates, candidateConfigs string
			if err := db.QueryRow(`SELECT reason_json,candidate_system_ids_json,candidate_config_version_ids_json FROM alert_occurrence_attributions WHERE occurrence_id=1`).Scan(&legacyReason, &candidates, &candidateConfigs); err != nil {
				t.Fatal(err)
			}
			if legacyReason != `{"code":"source_mismatch"}` || candidates != "[]" || candidateConfigs != "[]" {
				t.Fatalf("legacy attribution evidence changed: %s %s %s", legacyReason, candidates, candidateConfigs)
			}
			var viewName, conditions string
			if err := db.QueryRow(`SELECT display_name,label_conditions_json FROM business_views WHERE view_key='payments-prod'`).Scan(&viewName, &conditions); err != nil || viewName != "支付生产" || conditions != `{"service":"payments"}` {
				t.Fatalf("view history changed: %q %q %v", viewName, conditions, err)
			}
			// 新列存在且既有行为默认空（不参与告警归属）；新投影表存在且为空。
			var sourceKeys string
			if err := db.QueryRow(`SELECT alert_source_keys_json FROM business_views WHERE view_key='payments-prod'`).Scan(&sourceKeys); err != nil || sourceKeys != "[]" {
				t.Fatalf("predecessor view must default to non-participating scope: %q %v", sourceKeys, err)
			}
			var projected int
			if err := db.QueryRow(`SELECT COUNT(*) FROM alert_occurrence_view_attributions`).Scan(&projected); err != nil || projected != 0 {
				t.Fatalf("predecessor occurrences must not gain fabricated view attributions: %d %v", projected, err)
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
				alertViewAttributionMigrationID, migrationDigest(alertViewAttributionMigrationID)).Scan(&ledger); err != nil || ledger != 1 {
				t.Fatalf("authentic attribution ledger missing: %d %v", ledger, err)
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

func TestAlertViewAttributionRejectsMissingBackupAndForgedHistory(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-backup", true: "forged-history"}[forged], func(t *testing.T) {
			db := alertViewAttributionFixture(t)
			seedAlertViewAttributionState(t, db)
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
			if digest != alertViewAttributionPredecessorSchemaDigest {
				t.Fatal("failed migration changed schema")
			}
			var occurrences int
			if err := db.QueryRow(`SELECT COUNT(*) FROM alert_occurrences`).Scan(&occurrences); err != nil || occurrences != 1 {
				t.Fatalf("failed migration changed data: %d %v", occurrences, err)
			}
		})
	}
}
