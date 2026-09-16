package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// 巡检分析冻结（inspection analysis freeze）发布转换：前一个发布版本（digest
// 见 inspectionFreezePredecessorSchemaDigest，逐字节捕获于
// testdata/inspection-freeze-predecessor.sql.gz）之后，巡检计划新增可选的分析
// 语义字段（检查说明/单位/初始报告要求），Run 创建时冻结，重分析支持仅本次
// 覆盖。转换完全是加列/加表：新增列全部可空，既有 Run/检查/报告的历史事实
// 保持原值——重建为 copy-safe by construction。
const (
	inspectionFreezePredecessorSchemaDigest = "98ea729cbfb0f88ca5c89c4245eb48cdf7b48c02f60f0ca3df66e2bcfff63abd"
	inspectionFreezeMigrationID             = "20260916_inspection_freeze_v1"
)

// verifyInspectionFreezePredecessorHistory admits only authentic history on
// the inspection-freeze predecessor: fresh installs of that release carry an
// empty ledger, and conversions of the auth-simplification predecessor carry
// exactly its one ledger row.
func verifyInspectionFreezePredecessorHistory(ctx context.Context, conn *sql.Conn) error {
	var history int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&history); err != nil {
		return err
	}
	if history == 0 {
		return nil
	}
	var matching int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger WHERE migration_id=? AND digest=?`,
		authSimplificationMigrationID, migrationDigest(authSimplificationMigrationID)).Scan(&matching); err != nil {
		return err
	}
	if history == 1 && matching == 1 {
		return nil
	}
	return fmt.Errorf("%w: inspection freeze predecessor requires an empty ledger or exactly the auth simplification ledger", ErrSchemaHistoryPresent)
}

func migrateInspectionFreezeOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, inspectionFreezeMigrationID, inspectionFreezePredecessorSchemaDigest)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report.LegacySchemaDigest = stored
	if err := verifyInspectionFreezePredecessorHistory(ctx, conn); err != nil {
		return report, err
	}
	if err := rebuildCanonicalSchema(ctx, conn, nil, false); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`,
		inspectionFreezeMigrationID, migrationDigest(inspectionFreezeMigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	_, err = conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow())
	return report, err
}
