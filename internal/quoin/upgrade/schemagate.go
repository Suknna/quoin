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

// PreflightResult is the offline verification summary printed by
// `quoin migrate preflight` and re-verified under the exclusive migration
// transaction. The administrator-consolidation fields are observed by the
// shared pre-rebuild normalization and recorded on the migrate summary.
type PreflightResult struct {
	Revision         int64  `json:"maintenanceRevision"`
	BackupID         int64  `json:"backupId"`
	ManifestSHA256   string `json:"manifestSha256"`
	DBSHA256         string `json:"dbSha256"`
	ArtifactCount    int64  `json:"artifactCount"`
	SchemaVersion    string `json:"schemaVersion"`
	MigrationHistory int64  `json:"migrationHistory"`

	RetainedAdminID     int64   `json:"retainedAdminId,omitempty"`
	DemotedAdminIDs     []int64 `json:"demotedAdminIds,omitempty"`
	RevokedSessionCount int64   `json:"revokedSessionCount,omitempty"`
}

// Preflight re-reads every gate read-only: the Upgrade maintenance must be
// active with every checklist item Safe, the window's upgrade backup must
// have succeeded with a manifest digest, and the database must be an exact
// fresh-v1 schema or an exact released predecessor with an authentic ledger.
// It is the authenticated offline verification
// the deployment helper runs on the OLD image before stopping is considered
// safe to proceed; Migrate re-verifies under BEGIN IMMEDIATE.
func Preflight(ctx context.Context, db *sql.DB) (PreflightResult, error) {
	return PreflightWithOptions(ctx, db, Options{})
}

// PreflightWithOptions is Preflight plus the read-only administrator-topology
// validation for predecessor databases: a missing or non-admin retention
// selection is reported here, on the OLD image, before the exclusive forward
// step is attempted.
func PreflightWithOptions(ctx context.Context, db *sql.DB, options Options) (PreflightResult, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return PreflightResult{}, err
	}
	defer conn.Close()
	result, err := preflightOn(ctx, conn)
	if err != nil {
		return PreflightResult{}, err
	}
	var digest string
	if err := conn.QueryRowContext(ctx, `SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil {
		return PreflightResult{}, err
	}
	if isReleasedPredecessorDigest(digest) {
		if _, err := planAdminNormalization(ctx, conn, options); err != nil {
			return PreflightResult{}, err
		}
	}
	return result, nil
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

// verifySchemaGate is the release boundary: the only migratable databases are
// the exact fresh v1 canonical schema and exact released predecessor digests,
// each holding only its authentic migration history. Any other schema version,
// divergent digest or inauthentic ledger row is rejected with a stable code; no
// N-1 migration evidence is ever fabricated.
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
	// The ADR-0004 predecessor is reached both by fresh installs of that release
	// (empty ledger) and by declaration-cutover migrations (exactly one cutover
	// ledger row). No other history is admissible.
	if stored == pluginRegistrySchemaDigest {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
			return err
		}
		if result.MigrationHistory == 0 {
			return nil
		}
		var matching int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger WHERE migration_id=? AND digest=?`, declarationCutoverMigrationID, migrationDigest(declarationCutoverMigrationID)).Scan(&matching); err != nil {
			return err
		}
		if result.MigrationHistory == 1 && matching == 1 {
			return nil
		}
		return fmt.Errorf("%w: plugin registry predecessor requires an empty ledger or exactly the declaration cutover ledger", ErrSchemaHistoryPresent)
	}
	// The auth-audit predecessor is reached by fresh installs of that release
	// and by conversions of every earlier released predecessor, so only its
	// ledger authenticity is fixed, not one exact shape.
	if stored == authAuditPredecessorSchemaDigest {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
			return err
		}
		return verifyAuthAuditPredecessorHistory(ctx, conn)
	}
	if stored == authSimplificationPredecessorDigest {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
			return err
		}
		return verifyUnifiedAuthHistory(ctx, conn, false)
	}
	// The inspection-freeze predecessor is reached by fresh installs of that
	// release (empty ledger) and by auth-simplification conversions (exactly
	// its one ledger row). No other history is admissible.
	if stored == inspectionFreezePredecessorSchemaDigest {
		return verifyInspectionFreezePredecessorHistory(ctx, conn)
	}
	// The alert-view-attribution predecessor is reached by fresh installs of
	// that release (empty ledger) and by inspection-freeze conversions (exactly
	// its one ledger row). No other history is admissible.
	if stored == alertViewAttributionPredecessorSchemaDigest {
		return verifyAlertViewAttributionPredecessorHistory(ctx, conn)
	}
	// The unified-mTLS predecessor is the last registration-era release; every
	// history its own schema gate admitted stays authentic here.
	if stored == unifiedMTLSPredecessorSchemaDigest {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
			return err
		}
		return verifyUnifiedAuthHistory(ctx, conn, true)
	}
	if stored == verifyRetirePredecessorSchemaDigest {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
			return err
		}
		return verifyUnifiedAuthHistory(ctx, conn, true)
	}
	if stored == taskChangeLogRetirePredecessorSchemaDigest {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
			return err
		}
		return verifyUnifiedAuthHistory(ctx, conn, true)
	}
	if stored == declaredDiscoveryRetirePredecessorSchemaDigest {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
			return err
		}
		return verifyUnifiedAuthHistory(ctx, conn, true)
	}
	if stored == oidcAuthRetirePredecessorSchemaDigest {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
			return err
		}
		return verifyUnifiedAuthHistory(ctx, conn, true)
	}
	if stored != hex.EncodeToString(digest[:]) {
		return ErrSchemaDigestMismatch
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
		return err
	}
	return verifyUnifiedAuthHistory(ctx, conn, true)
}

// verifyAuthAuditPredecessorHistory admits only authentic migration history on
// the auth-audit predecessor. That release is reached both by fresh installs
// and by conversions of every earlier released predecessor, so several
// authentic ledger shapes exist; every row must still be a known migration id
// carrying its exact derived digest, and the auth-audit row itself must be
// absent (its presence would mean the conversion already ran).
func verifyAuthAuditPredecessorHistory(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `SELECT migration_id,digest FROM migration_ledger`)
	if err != nil {
		return err
	}
	defer rows.Close()
	known := map[string]bool{
		legacyMetricsBusinessMigrationID:      true,
		directInvestigationMetricsMigrationID: true,
		declarationCutoverMigrationID:         true,
		pluginRegistryMigrationID:             true,
	}
	for rows.Next() {
		var migrationID, digest string
		if err := rows.Scan(&migrationID, &digest); err != nil {
			return err
		}
		if !known[migrationID] || migrationDigest(migrationID) != digest {
			return fmt.Errorf("%w: auth-audit predecessor carries inauthentic ledger row %q", ErrSchemaHistoryPresent, migrationID)
		}
	}
	if err := rows.Err(); err != nil {
		return err
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
	return MigrateWithOptions(ctx, db, Options{})
}

// MigrateWithOptions is Migrate with the operator decisions (the retained
// administrator selection) required by predecessors holding several enabled
// administrators. The selection is enforced again inside the exclusive
// transaction, so a stale decision cannot commit.
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
	// A released v1 database has known prior digests. Route each to its
	// explicit converter instead of treating the digest mismatch as a generic
	// compatibility error. No other historical digest is accepted.
	var digest string
	if err := db.QueryRowContext(ctx, `SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil {
		return PreflightResult{}, err
	}
	if digest == legacyMetricsBusinessSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateLegacyMetricsBusinessOn)
	}
	if digest == directInvestigationMetricsSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateDirectInvestigationMetricsOn)
	}
	if digest == declarationCutoverSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateDeclarationCutoverOn)
	}
	if digest == pluginRegistrySchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migratePluginRegistryOn)
	}
	if digest == authAuditPredecessorSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateAuthAuditOn)
	}
	if digest == authSimplificationPredecessorDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateAuthSimplificationOn)
	}
	if digest == inspectionFreezePredecessorSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateInspectionFreezeOn)
	}
	if digest == alertViewAttributionPredecessorSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateAlertViewAttributionOn)
	}
	if digest == unifiedMTLSPredecessorSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateUnifiedMTLSOn)
	}
	if digest == verifyRetirePredecessorSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateVerifyRetireOn)
	}
	if digest == taskChangeLogRetirePredecessorSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateTaskChangeLogRetireOn)
	}
	if digest == declaredDiscoveryRetirePredecessorSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateDeclaredDiscoveryRetireOn)
	}
	if digest == oidcAuthRetirePredecessorSchemaDigest {
		return migrateReleasedSchemaAndFinish(ctx, db, options, migrateOIDCAuthRetireOn)
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

// migrateReleasedSchemaAndFinish commits a released-schema conversion and the
// Upgrade maintenance exit together. A crash or failure before COMMIT rolls
// back both, leaving the known predecessor digest eligible for a safe retry.
func migrateReleasedSchemaAndFinish(ctx context.Context, db *sql.DB, options Options, migrate func(context.Context, *sql.Conn) (LegacyMigrationReport, error)) (PreflightResult, error) {
	var result PreflightResult
	_, err := migrateReleasedSchemaTransaction(ctx, db, options, migrate, func(ctx context.Context, conn *sql.Conn, report LegacyMigrationReport) error {
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
	if err := conn.QueryRowContext(ctx, `SELECT id,manifest_sha256,db_sha256,artifact_count FROM backups WHERE trigger_kind='upgrade' AND status='succeeded' AND created_at>=(SELECT entered_at FROM maintenance_state WHERE id=1) ORDER BY id DESC LIMIT 1`).Scan(&result.BackupID, &result.ManifestSHA256, &result.DBSHA256, &result.ArtifactCount); err != nil {
		return PreflightResult{}, err
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&result.MigrationHistory); err != nil {
		return PreflightResult{}, err
	}
	// The operator-facing summary carries the consolidation observations; the
	// durable ledger and audit event remain the authoritative record.
	result.RetainedAdminID = report.RetainedAdminID
	result.DemotedAdminIDs = report.DemotedAdminIDs
	result.RevokedSessionCount = report.RevokedSessionCount
	if _, err := conn.ExecContext(ctx, `UPDATE maintenance_state SET active=0,reason=NULL,entered_at=NULL,entered_by_type=NULL,entered_by_id=NULL,exited_at=?,exited_by_type='system',exited_by_id=0,row_version=row_version+1 WHERE id=1 AND active=1 AND row_version=?`, timestampNow(), result.Revision); err != nil {
		return PreflightResult{}, err
	}
	if err := recordMigrationCompletion(ctx, conn, "maintenance.upgrade.migrate_legacy", result.Revision); err != nil {
		return PreflightResult{}, err
	}
	// The durable ledger and audit event are the authoritative record; the
	// report's consolidation fields were already copied onto the result.
	return result, nil
}

// DDL rebuilds require the migration authority's raw connection. Completion
// still uses the canonical sink in that same transaction, never a second audit.
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
