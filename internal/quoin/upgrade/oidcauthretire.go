package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// This is an unpublished acceptance-build identity, not a released predecessor.
// Its exact captured bytes are pinned in testdata/oidc-auth-retire-predecessor.sql.gz.
const (
	oidcAuthRetirePredecessorSchemaDigest = "83d3021c5242457639d60472061fdd7e5e82c6be0b4d36ac7d74425f616774ae"
	oidcAuthRetireMigrationID             = "20260920_oidc_auth_v1"
)

func migrateOIDCAuthRetireOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	_, report, err := beginReleasedSchemaMigration(ctx, conn, oidcAuthRetireMigrationID, oidcAuthRetirePredecessorSchemaDigest)
	if err != nil {
		return report, err
	}
	if err := verifyUnifiedAuthHistory(ctx, conn, true); err != nil {
		return report, err
	}
	// ADR-0010: pending login flows, undelivered OTP challenges, and delivery
	// settings are transient state of the retired second-factor subsystem — the
	// canonical rebuild drops them wholesale. Every user keeps credentials and
	// sessions verbatim; existing users are local-source by construction, so the
	// new identities table starts empty and no backfill is owed. The new
	// initial_password_expires_at column lands NULL for the same reason.
	if err := rebuildCanonicalSchema(ctx, conn, nil, false); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, oidcAuthRetireMigrationID, migrationDigest(oidcAuthRetireMigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	_, err = conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow())
	return report, err
}
