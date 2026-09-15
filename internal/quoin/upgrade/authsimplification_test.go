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

func authSimplificationFixture(t *testing.T) *sql.DB {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "auth-simplification-predecessor.sql.gz"))
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
	if hex.EncodeToString(digest[:]) != authSimplificationPredecessorDigest {
		t.Fatalf("unpublished predecessor identity differs: %x", digest)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "intermediate.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-09-15T00:00:00Z')`, authSimplificationPredecessorDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedInitializedSimplificationState(t *testing.T, db *sql.DB) {
	t.Helper()
	statements := []string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,auth_revision,row_version,created_at,updated_at) VALUES(1,'admin','Administrator','admin',1,1,'retained-password-hash',3,5,'2026-09-15T00:00:00Z','2026-09-15T00:00:00Z')`,
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,auth_revision,row_version,created_at,updated_at) VALUES(2,'operator','Operator','operator',1,0,'temporary-password-hash',2,4,'2026-09-15T00:00:00Z','2026-09-15T00:00:00Z')`,
		`INSERT INTO user_contacts(id,user_id,channel,target,verified_at,created_at,updated_at) VALUES(1,1,'email','admin@fixture.test','2026-09-15T00:00:00Z','2026-09-15T00:00:00Z','2026-09-15T00:00:00Z')`,
		`INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,zeroblob(32),3,'retained','2026-09-15T00:00:00Z','2026-09-15T00:00:00Z','2026-09-16T00:00:00Z','2026-09-22T00:00:00Z')`,
		`INSERT INTO auth_flows(id,flow_type,user_id,flow_token_digest,correlation_id,auth_revision_at_issue,password_set,verified_contact_id,status,created_at,expires_at) VALUES(1,'admin_initialize',1,randomblob(32),'retained-init-correlation',3,1,1,'completed','2026-09-15T00:00:00Z','2026-09-15T01:00:00Z')`,
		`INSERT INTO auth_flows(id,flow_type,user_id,flow_token_digest,correlation_id,auth_revision_at_issue,status,created_at,expires_at) VALUES(2,'recovery',1,randomblob(32),'obsolete-recovery-correlation',3,'pending','2026-09-15T00:00:00Z','2026-09-15T01:00:00Z')`,
		`INSERT INTO auth_challenges(flow_id,user_id,purpose,contact_id,contact_version,auth_revision_at_issue,code_digest,delivery_id,created_at,expires_at) VALUES(2,1,'contact_verification',1,1,3,zeroblob(32),'obsolete-delivery','2026-09-15T00:00:00Z','2026-09-15T01:00:00Z')`,
		`INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,correlation_id,created_at) VALUES('user',1,'retained.history','success','user',2,'durable-history-correlation','2026-09-15T00:00:00Z')`,
		`UPDATE sqlite_sequence SET seq=1000 WHERE name='audit_events'`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuthSimplificationPreservesInitializedAccountsAndHistory(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-intermediate", true: "migrated-intermediate"}[migrated], func(t *testing.T) {
			db := authSimplificationFixture(t)
			seedInitializedSimplificationState(t, db)
			seedAuthAuditUpgradeWindow(t, db, 1)
			if migrated {
				if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, authAuditMigrationID, migrationDigest(authAuditMigrationID), migrationNow()); err != nil {
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
			if result.RevokedSessionCount != 0 {
				t.Fatal("simplification must not re-run first MFA migration")
			}
			if result.BackupID != 1 || len(result.ManifestSHA256) != 64 {
				t.Fatalf("migration summary lost backup provenance: %+v", result)
			}
			var initialized, revision, version int
			var password string
			if err := db.QueryRow(`SELECT initialized,auth_revision,row_version,password_phc FROM users WHERE id=1`).Scan(&initialized, &revision, &version, &password); err != nil {
				t.Fatal(err)
			}
			if initialized != 1 || revision != 3 || version != 5 || password != "retained-password-hash" {
				t.Fatalf("admin state changed: %d %d %d %q", initialized, revision, version, password)
			}
			var sessions, contacts, flows, oldTables, history int
			if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL),(SELECT COUNT(*) FROM user_contacts WHERE verified_at IS NOT NULL),(SELECT COUNT(*) FROM auth_flows WHERE id=1 AND correlation_id='retained-init-correlation'),(SELECT COUNT(*) FROM sqlite_master WHERE name='install_credentials'),(SELECT COUNT(*) FROM audit_events WHERE action='retained.history' AND correlation_id='durable-history-correlation')`).Scan(&sessions, &contacts, &flows, &oldTables, &history); err != nil {
				t.Fatal(err)
			}
			if sessions != 1 || contacts != 1 || flows != 1 || oldTables != 0 || history != 1 {
				t.Fatalf("preservation mismatch: sessions=%d contacts=%d flows=%d tables=%d history=%d", sessions, contacts, flows, oldTables, history)
			}
			var stored string
			if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(gen.SchemaSQL))
			if stored != hex.EncodeToString(digest[:]) {
				t.Fatal("target digest not stamped")
			}
			var migrationCorrelation string
			if err := db.QueryRow(`SELECT correlation_id FROM audit_events WHERE action='maintenance.upgrade.migrate_legacy'`).Scan(&migrationCorrelation); err != nil || migrationCorrelation == "" {
				t.Fatalf("migration completion lost operation correlation: %q %v", migrationCorrelation, err)
			}
			var eventID int
			if err := db.QueryRow(`SELECT MAX(id) FROM audit_events`).Scan(&eventID); err != nil || eventID <= 1000 {
				t.Fatalf("sequence not retained: %d %v", eventID, err)
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

func TestAuthSimplificationRejectsMissingBackupAndForgedHistory(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-backup", true: "forged-history"}[forged], func(t *testing.T) {
			db := authSimplificationFixture(t)
			seedInitializedSimplificationState(t, db)
			want := ErrNotUpgradeMaintenance
			if !forged {
				if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
					t.Fatal(err)
				}
			}
			if forged {
				seedAuthAuditUpgradeWindow(t, db, 1)
				if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES('forged',printf('%064d',0),'2026-09-15T00:00:00Z')`); err != nil {
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
			if digest != authSimplificationPredecessorDigest {
				t.Fatal("failed migration changed schema")
			}
			var flows int
			if err := db.QueryRow(`SELECT COUNT(*) FROM auth_flows WHERE flow_type='recovery'`).Scan(&flows); err != nil || flows != 1 {
				t.Fatalf("failed migration changed data: %d %v", flows, err)
			}
		})
	}
}
