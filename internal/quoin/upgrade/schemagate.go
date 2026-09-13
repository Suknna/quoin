package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// Stable error codes surfaced by `quoin migrate`; the deployment helper
// records them verbatim in its report.
var (
	ErrNotUpgradeMaintenance = errors.New("upgrade_maintenance_not_active")
	ErrChecklistBlocking     = errors.New("upgrade_checklist_blocking")
	ErrNoUpgradeBackup       = errors.New("upgrade_backup_missing")
	ErrUnsupportedSchema     = errors.New("unsupported_schema_version")
	ErrSchemaDigestMismatch  = errors.New("schema_digest_mismatch")
	ErrSchemaHistoryPresent  = errors.New("schema_history_present")
)

// PreflightResult is the offline verification summary printed by
// `quoin migrate preflight` and re-verified under the exclusive migration
// transaction.
type PreflightResult struct {
	Revision         int64  `json:"maintenanceRevision"`
	BackupID         int64  `json:"backupId"`
	ManifestSHA256   string `json:"manifestSha256"`
	DBSHA256         string `json:"dbSha256"`
	ArtifactCount    int64  `json:"artifactCount"`
	SchemaVersion    string `json:"schemaVersion"`
	MigrationHistory int64  `json:"migrationHistory"`
}

// Preflight re-reads every gate read-only: the Upgrade maintenance must be
// active with every checklist item Safe, the window's upgrade backup must
// have succeeded with a manifest digest, and the database must be an exact
// fresh-v1 schema or the exact declaration predecessor with its pinned ledger.
// It is the authenticated offline verification
// the deployment helper runs on the OLD image before stopping is considered
// safe to proceed; Migrate re-verifies under BEGIN IMMEDIATE.
func Preflight(ctx context.Context, db *sql.DB) (PreflightResult, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return PreflightResult{}, err
	}
	defer conn.Close()
	return preflightOn(ctx, conn)
}

func preflightOn(ctx context.Context, conn *sql.Conn) (PreflightResult, error) {
	result, err := preflightOperationalOn(ctx, conn)
	if err != nil {
		return PreflightResult{}, err
	}
	if err := verifySchemaGate(ctx, conn, &result); err != nil {
		return PreflightResult{}, err
	}
	return result, nil
}

// preflightOperationalOn verifies that the operator drained runtime work and
// captured a recovery backup. Legacy migration calls it under its one write
// transaction before it changes any retained facts.
func preflightOperationalOn(ctx context.Context, conn *sql.Conn) (PreflightResult, error) {
	var result PreflightResult
	var active int
	var reason string
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &result.Revision); err != nil {
		return PreflightResult{}, err
	}
	if active != 1 || reason != Reason {
		return PreflightResult{}, ErrNotUpgradeMaintenance
	}
	var total, blocking int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN safe_state='Blocking' THEN 1 ELSE 0 END),0) FROM maintenance_items WHERE maintenance_revision=?`, result.Revision).Scan(&total, &blocking); err != nil {
		return PreflightResult{}, err
	}
	if total == 0 || blocking != 0 {
		return PreflightResult{}, fmt.Errorf("%w: %d of %d items blocking", ErrChecklistBlocking, blocking, total)
	}
	var enteredAt string
	if err := conn.QueryRowContext(ctx, `SELECT entered_at FROM maintenance_state WHERE id=1`).Scan(&enteredAt); err != nil {
		return PreflightResult{}, err
	}
	err := conn.QueryRowContext(ctx, `SELECT id,manifest_sha256,db_sha256,artifact_count FROM backups WHERE trigger_kind='upgrade' AND status='succeeded' AND created_at>=? ORDER BY id DESC LIMIT 1`, enteredAt).
		Scan(&result.BackupID, &result.ManifestSHA256, &result.DBSHA256, &result.ArtifactCount)
	if errors.Is(err, sql.ErrNoRows) {
		return PreflightResult{}, ErrNoUpgradeBackup
	}
	if err != nil {
		return PreflightResult{}, err
	}
	return result, nil
}

// verifySchemaGate enforces the first-release boundary: the only migratable
// database is a zero-history exact fresh v1 schema. Any other schema version,
// divergent digest or pre-existing migration ledger row is rejected with a
// stable code; no N-1 migration evidence is ever fabricated.
func verifySchemaGate(ctx context.Context, conn *sql.Conn, result *PreflightResult) error {
	var stored string
	if err := conn.QueryRowContext(ctx, `SELECT schema_version,schema_digest FROM schema_state WHERE id=1`).Scan(&result.SchemaVersion, &stored); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	if result.SchemaVersion != "v1" {
		return fmt.Errorf("%w: found %q", ErrUnsupportedSchema, result.SchemaVersion)
	}
	if stored == declarationCutoverSchemaDigest {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
			return err
		}
		var matching int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger WHERE migration_id=? AND digest=?`, directInvestigationMetricsMigrationID, migrationDigest(directInvestigationMetricsMigrationID)).Scan(&matching); err != nil {
			return err
		}
		if result.MigrationHistory != 1 || matching != 1 {
			return fmt.Errorf("%w: predecessor requires its exact direct-investigation ledger", ErrSchemaHistoryPresent)
		}
		return nil
	}
	if stored != hex.EncodeToString(digest[:]) {
		return ErrSchemaDigestMismatch
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
		return err
	}
	if result.MigrationHistory != 0 {
		return fmt.Errorf("%w: %d ledger rows", ErrSchemaHistoryPresent, result.MigrationHistory)
	}
	return nil
}

// Migrate is the exclusive forward-migration command run by the NEW Quoin
// image against the stopped stack's data directory. It accepts the exact
// current canonical schema plus only named released predecessor digests; each
// predecessor has a one-shot canonical rebuild and durable migration ledger
// record. On success the fully-verified Upgrade maintenance is exited by the
// system actor — the commit-order "accepts new writes" boundary — and the
// wizard may start normal-mode components (OPS-UPGRADE-002/005).
func Migrate(ctx context.Context, db *sql.DB) (PreflightResult, error) {
	// A released v1 database has one known prior digest. Route it to the
	// explicit converter instead of treating the digest mismatch as a generic
	// compatibility error. No other historical digest is accepted.
	var digest string
	if err := db.QueryRowContext(ctx, `SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil {
		return PreflightResult{}, err
	}
	if digest == legacyMetricsBusinessSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, migrateLegacyMetricsBusinessOn)
	}
	if digest == directInvestigationMetricsSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, migrateDirectInvestigationMetricsOn)
	}
	if digest == declarationCutoverSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, migrateDeclarationCutoverOn)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return PreflightResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return PreflightResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	result, err := preflightOn(ctx, conn)
	if err != nil {
		return PreflightResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE maintenance_state SET active=0,reason=NULL,entered_at=NULL,entered_by_type=NULL,entered_by_id=NULL,exited_at=?,exited_by_type='system',exited_by_id=0,row_version=row_version+1 WHERE id=1 AND active=1 AND row_version=?`, timestampNow(), result.Revision); err != nil {
		return PreflightResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('system',0,'maintenance.upgrade.migrate','success','maintenance',?,?)`, result.Revision, timestampNow()); err != nil {
		return PreflightResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return PreflightResult{}, err
	}
	committed = true
	return result, nil
}

// migrateReleasedSchemaAndFinish commits a released-schema conversion and the
// Upgrade maintenance exit together. A crash or failure before COMMIT rolls
// back both, leaving the known predecessor digest eligible for a safe retry.
func migrateReleasedSchemaAndFinish(ctx context.Context, db *sql.DB, migrate func(context.Context, *sql.Conn) (LegacyMigrationReport, error)) (PreflightResult, error) {
	var result PreflightResult
	_, err := migrateReleasedSchemaTransaction(ctx, db, migrate, func(ctx context.Context, conn *sql.Conn, report LegacyMigrationReport) error {
		var completionErr error
		result, completionErr = finishReleasedMigrationOn(ctx, conn, report)
		return completionErr
	})
	if err != nil {
		return PreflightResult{}, err
	}
	return result, nil
}

// finishReleasedMigrationOn closes the one already-open migration transaction.
// It intentionally does not re-run the operational gate: the same exclusive
// transaction already read it before rebuilding the schema.
func finishReleasedMigrationOn(ctx context.Context, conn *sql.Conn, report LegacyMigrationReport) (PreflightResult, error) {
	var result PreflightResult
	if err := conn.QueryRowContext(ctx, `SELECT row_version FROM maintenance_state WHERE id=1 AND active=1 AND reason=?`, Reason).Scan(&result.Revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PreflightResult{}, ErrNotUpgradeMaintenance
		}
		return PreflightResult{}, err
	}
	result.SchemaVersion = "v1"
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
		return PreflightResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE maintenance_state SET active=0,reason=NULL,entered_at=NULL,entered_by_type=NULL,entered_by_id=NULL,exited_at=?,exited_by_type='system',exited_by_id=0,row_version=row_version+1 WHERE id=1 AND active=1 AND row_version=?`, timestampNow(), result.Revision); err != nil {
		return PreflightResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('system',0,'maintenance.upgrade.migrate_legacy','success','maintenance',?,?)`, result.Revision, timestampNow()); err != nil {
		return PreflightResult{}, err
	}
	_ = report // The durable ledger and audit event are the authoritative record.
	return result, nil
}

func timestampNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
