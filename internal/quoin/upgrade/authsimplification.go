package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// This is an unpublished acceptance-build identity, not a released predecessor.
// Its exact captured bytes are pinned in testdata/auth-simplification-predecessor.sql.gz.
const (
	authSimplificationPredecessorDigest = "7bf6090749699d897896221c44e973f57f6be8eb4b23466b11705cf440f29244"
	authSimplificationMigrationID       = "20260915_auth_simplification_v1"
)

func verifyUnifiedAuthHistory(ctx context.Context, conn *sql.Conn, allowSimplification bool) error {
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
		authAuditMigrationID:                  true,
		authSimplificationMigrationID:         allowSimplification,
		inspectionFreezeMigrationID:           allowSimplification,
		alertViewAttributionMigrationID:       allowSimplification,
		unifiedMTLSMigrationID:               allowSimplification,
	}
	for rows.Next() {
		var id, digest string
		if err := rows.Scan(&id, &digest); err != nil {
			return err
		}
		if !known[id] || digest != migrationDigest(id) {
			return fmt.Errorf("%w: inauthentic unified-auth migration %q", ErrSchemaHistoryPresent, id)
		}
	}
	return rows.Err()
}

func migrateAuthSimplificationOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	_, report, err := beginReleasedSchemaMigration(ctx, conn, authSimplificationMigrationID, authSimplificationPredecessorDigest)
	if err != nil {
		return report, err
	}
	if err := verifyUnifiedAuthHistory(ctx, conn, false); err != nil {
		return report, err
	}
	// Recovery flow credentials were transient, never workbench sessions. The
	// removed redemption protocol cannot remain resumable; its historical audit
	// events are retained, as are every user's credentials and verified targets.
	if _, err := conn.ExecContext(ctx, `DELETE FROM auth_challenges WHERE flow_id IN (SELECT id FROM auth_flows WHERE flow_type='recovery')`); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM auth_flows WHERE flow_type='recovery'`); err != nil {
		return report, err
	}
	if err := rebuildCanonicalSchema(ctx, conn, nil, false); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, authSimplificationMigrationID, migrationDigest(authSimplificationMigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	_, err = conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow())
	return report, err
}
