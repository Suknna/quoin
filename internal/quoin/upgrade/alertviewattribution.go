package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// 告警视图归属（alert view attribution）发布转换：前一个发布版本（digest 见
// alertViewAttributionPredecessorSchemaDigest，逐字节捕获于
// testdata/alert-view-attribution-predecessor.sql.gz）之后，告警归属弃用旧
// business_system 声明、改用业务视图的显式 AM 来源约束与精确标签条件。转换
// 完全是加列/加表：business_views 新增可默认的 alert_source_keys_json，新增
// 独立投影表 alert_occurrence_view_attributions；既有告警/旧归属/视图历史
// 保持原值——重建为 copy-safe by construction。
const (
	alertViewAttributionPredecessorSchemaDigest = "281f533f9ca7bb4a607495a555217b2bd795562da486659adb27fef089157a48"
	alertViewAttributionMigrationID             = "20260916_alert_view_attribution_v1"
)

// verifyAlertViewAttributionPredecessorHistory admits only authentic history
// on the alert-view-attribution predecessor: fresh installs of that release
// carry an empty ledger, and conversions of the inspection-freeze predecessor
// carry exactly its one ledger row.
func verifyAlertViewAttributionPredecessorHistory(ctx context.Context, conn *sql.Conn) error {
	var history int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&history); err != nil {
		return err
	}
	if history == 0 {
		return nil
	}
	var matching int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger WHERE migration_id=? AND digest=?`,
		inspectionFreezeMigrationID, migrationDigest(inspectionFreezeMigrationID)).Scan(&matching); err != nil {
		return err
	}
	if history == 1 && matching == 1 {
		return nil
	}
	return fmt.Errorf("%w: alert view attribution predecessor requires an empty ledger or exactly the inspection freeze ledger", ErrSchemaHistoryPresent)
}

func migrateAlertViewAttributionOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, alertViewAttributionMigrationID, alertViewAttributionPredecessorSchemaDigest)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report.LegacySchemaDigest = stored
	if err := verifyAlertViewAttributionPredecessorHistory(ctx, conn); err != nil {
		return report, err
	}
	if err := rebuildCanonicalSchema(ctx, conn, nil, false); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`,
		alertViewAttributionMigrationID, migrationDigest(alertViewAttributionMigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	_, err = conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow())
	return report, err
}
