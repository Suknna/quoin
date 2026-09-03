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
// zero-history fresh-v1 schema. It is the same-Release offline verification
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
	if err := verifySchemaGate(ctx, conn, &result); err != nil {
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
// image against the stopped stack's data directory. For the first release
// the forward migration set is empty by definition: the gate must find the
// exact fresh-v1 schema already in place. On success the fully-verified
// Upgrade maintenance is exited by the system actor — the commit-order
// "accepts new writes" boundary — and the wizard may start normal-mode
// components (OPS-UPGRADE-002/005).
func Migrate(ctx context.Context, db *sql.DB) (PreflightResult, error) {
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

func timestampNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
