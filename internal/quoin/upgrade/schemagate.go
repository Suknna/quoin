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
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
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

// Options carries the operator decisions of `quoin migrate`. The first
// release has no conversion steps, so there is nothing to decide yet; the
// struct stays as the stable call surface for future migrations.
type Options struct{}

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
// current canonical schema with an authentic (empty) migration ledger. It is
// the authenticated offline verification the deployment helper runs on the
// OLD image after stopping is considered safe to proceed; Migrate
// re-verifies under BEGIN IMMEDIATE.
func Preflight(ctx context.Context, db *sql.DB) (PreflightResult, error) {
	return PreflightWithOptions(ctx, db, Options{})
}

// PreflightWithOptions is Preflight with the operator decisions surface kept
// for the CLI flag contract.
func PreflightWithOptions(ctx context.Context, db *sql.DB, options Options) (PreflightResult, error) {
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
// captured a recovery backup before the exclusive migration transaction
// changes any retained facts.
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

// verifySchemaGate is the release boundary: the only admissible database is
// the exact current canonical schema holding an empty migration ledger — a
// fresh initialize writes no ledger row and this build has no converters, so
// any row means the database came from some other build. Any other schema
// version, divergent digest or ledger row is rejected with a stable code.
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
		return fmt.Errorf("%w: a first-release database carries no migration ledger rows", ErrSchemaHistoryPresent)
	}
	return nil
}

// Migrate is the exclusive forward-migration command run by the NEW Quoin
// image against the stopped stack's data directory. The product has never
// shipped a release, so there are no predecessor conversions: the stored
// schema digest must equal the current canonical schema exactly. On success
// the fully-verified Upgrade maintenance is exited by the system actor — the
// commit-order "accepts new writes" boundary — and the wizard may start
// normal-mode components (OPS-UPGRADE-002/005).
func Migrate(ctx context.Context, db *sql.DB) (PreflightResult, error) {
	return MigrateWithOptions(ctx, db, Options{})
}

// MigrateWithOptions is Migrate with the operator decisions surface kept for
// the CLI flag contract; the first release has no decision left to enforce.
func MigrateWithOptions(ctx context.Context, db *sql.DB, options Options) (PreflightResult, error) {
	if meta, ok := execution.FromContext(ctx); ok {
		if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 || meta.Source.Kind != execution.SourceCLI {
			return PreflightResult{}, errors.New("schema migration requires an offline CLI system scope")
		}
	} else {
		correlation, err := execution.NewCorrelationID()
		if err != nil {
			return PreflightResult{}, err
		}
		ctx, err = execution.WithMetadata(ctx, execution.Metadata{CorrelationID: correlation, Actor: execution.Principal{Kind: execution.PrincipalSystem}, Source: execution.Source{Kind: execution.SourceCLI}})
		if err != nil {
			return PreflightResult{}, err
		}
	}
	// 没有前驱迁移路径：摘要不等于当前 canonical schema 即拒绝。首发前的
	// 任何构建产物都不存在可升级对象，重建数据目录是唯一恢复方式。
	var digest string
	if err := db.QueryRowContext(ctx, `SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil {
		return PreflightResult{}, err
	}
	current := sha256.Sum256([]byte(gen.SchemaSQL))
	if digest != hex.EncodeToString(current[:]) {
		return PreflightResult{}, errors.New("数据库来自未发布的构建版本，没有迁移路径；请重建数据目录后重试")
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
	if err := recordMigrationCompletion(ctx, conn, "maintenance.upgrade.migrate", result.Revision); err != nil {
		return PreflightResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return PreflightResult{}, err
	}
	committed = true
	return result, nil
}

// recordMigrationCompletion writes the migration exit's audit event through
// the canonical sink inside the caller's exclusive transaction.
func recordMigrationCompletion(ctx context.Context, conn *sql.Conn, action string, revision int64) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	_, err = audit.NewWriter().Write(ctx, conn, audit.Record{
		ActorType: string(meta.Actor.Kind), ActorID: meta.Actor.ID,
		InitiatorType: string(meta.Initiator.Kind), InitiatorID: meta.Initiator.ID,
		Action: action, Outcome: audit.OutcomeSuccess, CorrelationID: meta.CorrelationID,
		DomainRefType: "maintenance", DomainRefID: revision,
	})
	return err
}

func timestampNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
