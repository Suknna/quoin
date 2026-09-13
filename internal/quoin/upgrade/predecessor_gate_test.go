package upgrade

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// The stopped live deployment's recorded predecessor identity. These two byte
// strings are pinned verbatim: sha256("20260910_direct_investigation_metrics_v1")
// was verified to equal the immutable migration_ledger digest on the production
// database, and the schema digest is the released predecessor image's schema.sql
// identity. If either constant or the digest function drifts, this test fails
// before an operator is told a foreign database is the known predecessor.
func TestReleasedPredecessorIdentityIsPinnedToLiveValues(t *testing.T) {
	if declarationCutoverSchemaDigest != "3a95baf7b2ecab6a5f81fe084334fe953a5c23a3a71db3de1cc1d8c0ee107eb2" {
		t.Fatalf("declaration predecessor digest drifted: %s", declarationCutoverSchemaDigest)
	}
	if migrationDigest(directInvestigationMetricsMigrationID) != "39255c7776f4318773e3fe288b84b95a25bd91f4c9885cd816a040993e84375c" {
		t.Fatalf("prior migration ledger digest drifted: %s", migrationDigest(directInvestigationMetricsMigrationID))
	}
}

// openPredecessorSchemaWithoutLedger loads the exact released predecessor DDL
// with schema_state admitted but no migration_ledger row. The ledger table is
// append-only in the released schema (no UPDATE/DELETE triggers pass), so
// adversarial ledger states must be built at insert time rather than mutated
// from declarationCutoverFixture.
func openPredecessorSchemaWithoutLedger(t *testing.T) *sql.DB {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "declaration-cutover-predecessor.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "predecessor-gate.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-01-01T00:00:00Z')`, declarationCutoverSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

// gateError runs verifySchemaGate under SQLite's query-only mode: any write the
// gate attempted would abort, proving the predecessor admission is read-only.
// The pragma is per underlying connection and database/sql pools connections,
// so it must be restored before the pooled connection is reused by later
// fixture writes.
func gateError(t *testing.T, db *sql.DB) error {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	var result PreflightResult
	gateErr := verifySchemaGate(context.Background(), conn, &result)
	if _, err := conn.ExecContext(context.Background(), `PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	return gateErr
}

func TestPredecessorGateRejectsTamperedPriorLedgerDigest(t *testing.T) {
	db := openPredecessorSchemaWithoutLedger(t)
	// Right migration identity, fabricated digest: readiness must never pass on
	// the migration_id alone.
	if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,'2026-01-01T00:00:00Z')`, directInvestigationMetricsMigrationID, digest64()); err != nil {
		t.Fatal(err)
	}
	if err := gateError(t, db); !errors.Is(err, ErrSchemaHistoryPresent) {
		t.Fatalf("tampered prior ledger accepted: %v", err)
	}
}

func TestPredecessorGateRejectsMissingPriorLedgerRow(t *testing.T) {
	db := openPredecessorSchemaWithoutLedger(t)
	if err := gateError(t, db); !errors.Is(err, ErrSchemaHistoryPresent) {
		t.Fatalf("predecessor without its direct ledger accepted: %v", err)
	}
}

func TestPredecessorGateRejectsUnknownSchemaIdentity(t *testing.T) {
	db := openPredecessorSchemaWithoutLedger(t)
	if _, err := db.Exec(`UPDATE schema_state SET schema_digest=? WHERE id=1`, hex.EncodeToString(make([]byte, 32))); err != nil {
		t.Fatal(err)
	}
	if err := gateError(t, db); !errors.Is(err, ErrSchemaDigestMismatch) {
		t.Fatalf("unknown predecessor digest accepted: %v", err)
	}
	if _, err := db.Exec(`UPDATE schema_state SET schema_version='v0',schema_digest=? WHERE id=1`, declarationCutoverSchemaDigest); err != nil {
		t.Fatal(err)
	}
	if err := gateError(t, db); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("unknown predecessor version accepted: %v", err)
	}
}

// seedVerifiedUpgradeWindow reproduces the operator state the operational gate
// demands: an active Upgrade maintenance whose only checklist item is Safe and
// a succeeded upgrade backup inside the window. Every insert satisfies the
// released schema's CHECK constraints and foreign keys.
func seedVerifiedUpgradeWindow(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	const enteredAt = "2026-01-01T00:00:00Z"
	const backupAt = "2026-01-02T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES('upgrade-admin','Upgrade Admin','admin',1,'x',1,?,?)`, enteredAt, enteredAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE maintenance_state SET active=1,reason='Upgrade',entered_at=?,entered_by_type='user',entered_by_id=1,row_version=row_version+1,exited_at=NULL,exited_by_type=NULL,exited_by_id=NULL WHERE id=1 AND active=0`, enteredAt); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := db.QueryRow(`SELECT row_version FROM maintenance_state WHERE id=1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,'BackupPreflight','pre_upgrade_backup','Safe','backup_verified',?)`, revision, enteredAt); err != nil {
		t.Fatal(err)
	}
	digest := hex.EncodeToString(make([]byte, 32))
	if _, err := db.Exec(`INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,db_sha256,manifest_sha256,artifact_count,size_bytes,manifest_path,row_version,created_at,updated_at,started_at,completed_at,triggered_by) VALUES('succeeded','completed','upgrade','online',NULL,?,?,0,1234,'/backup/manifest.json',1,?,?,?,?,1)`, digest, digest, backupAt, backupAt, backupAt, backupAt); err != nil {
		t.Fatal(err)
	}
	return revision
}

// TestPreflightAcceptsReleasedPredecessorEndToEnd runs the full read-only
// Preflight — operational gate plus predecessor schema gate — against the real
// physical predecessor with a valid maintenance window, then proves nothing
// changed: no ledger growth, no schema rewrite, no maintenance exit, no audit
// event. Preflight may never mutate the database it blesses.
func TestPreflightAcceptsReleasedPredecessorEndToEnd(t *testing.T) {
	db := declarationCutoverFixture(t)
	revision := seedVerifiedUpgradeWindow(t, db)
	type snapshot struct {
		ledgerRows  int
		schemaState string
		active      int
		backups     int
		auditEvents int
	}
	snap := func() snapshot {
		t.Helper()
		var s snapshot
		if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger`).Scan(&s.ledgerRows); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT schema_version||'/'||schema_digest FROM schema_state WHERE id=1`).Scan(&s.schemaState); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT active FROM maintenance_state WHERE id=1`).Scan(&s.active); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM backups`).Scan(&s.backups); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&s.auditEvents); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snap()
	result, err := Preflight(context.Background(), db)
	if err != nil {
		t.Fatalf("released predecessor preflight rejected: %v", err)
	}
	if result.Revision != revision || result.BackupID == 0 || result.SchemaVersion != "v1" || result.MigrationHistory != 1 || result.ManifestSHA256 == "" {
		t.Fatalf("preflight result=%+v revision=%d", result, revision)
	}
	if after := snap(); after != before {
		t.Fatalf("preflight mutated the database: before=%+v after=%+v", before, after)
	}
}
