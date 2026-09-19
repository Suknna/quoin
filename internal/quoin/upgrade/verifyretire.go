package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// Config Verification / Resource Refresh 引擎退役转换（2026-09）：前一个发布版本
// （digest 见 verifyRetirePredecessorSchemaDigest，逐字节捕获于
// testdata/verify-retire-predecessor.sql.gz）之后，验证/刷新引擎的全部入口已消亡
// ——创建/取消端点随 ADR-0004 退役，只读详情与列表随 2026-09 API 精简移除，队列
// 无生产者、扫描恒空转。本迁移把剩余持久面一并退役：DROP config_verification_runs /
// config_verification_discovery_results / config_verification_run_check_results /
// resource_refresh_runs / observed_refresh_log 及其触发器族，收窄
// execution_attempts 的 scope 枚举与派发约束（plan_key 列随引擎退役删除）。
// 死 scope 的存量 attempt 行连同其子行由 rebuildCanonicalSchema 统一清理；其余
// 业务数据逐字保留——重建为 copy-safe by construction。
const (
	verifyRetirePredecessorSchemaDigest = "2fb984fb7f1962f12bed16b762f0246dc4f670146a6d050046c5ecbe89c00c06"
	verifyRetireMigrationID             = "20260919_retire_config_verification_v1"
)

// migrateVerifyRetireOn retires the dead verification/refresh persistence before
// the canonical rebuild so no narrowed constraint can reject the historic copy.
func migrateVerifyRetireOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, verifyRetireMigrationID, verifyRetirePredecessorSchemaDigest)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report.LegacySchemaDigest = stored
	if err := verifyUnifiedAuthHistory(ctx, conn, true); err != nil {
		return report, err
	}
	if err := rebuildCanonicalSchema(ctx, conn, nil, false); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`,
		verifyRetireMigrationID, migrationDigest(verifyRetireMigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	_, err = conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow())
	return report, err
}
