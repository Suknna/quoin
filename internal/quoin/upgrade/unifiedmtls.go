package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// 组件认证统一 mTLS（ADR-0009）发布转换：前一个发布版本（digest 见
// unifiedMTLSPredecessorSchemaDigest，逐字节捕获于
// testdata/unified-mtls-predecessor.sql.gz）之后，Plinth 一次性注册制与 Stele
// service token 退役，组件身份改为部署 CA 签发的 mTLS 客户端证书。转换是纯
// 删除：DROP runtime_slots / runtime_credentials / lintel_recovery_receipts 及其
// 触发器与两个派发 fence 触发器；maintenance 枚举收窄（去掉 RuntimeSlot /
// LintelRecoveryFence kind、LintelRecovery reason 与 deployment_helper actor）。
// 退役子系统的 checklist 历史行先清理再重建，其余业务数据逐字保留——重建为
// copy-safe by construction。
const (
	unifiedMTLSPredecessorSchemaDigest = "f83c2a718ff52eb004bae521ec35adaa82b87be9b5200dbbd088f23268db14ec"
	unifiedMTLSMigrationID             = "20260919_unified_mtls_component_auth_v1"
)

// migrateUnifiedMTLSOn drops the retired registration/token authority and the
// Lintel recovery bookkeeping before the canonical rebuild so no narrowed
// constraint can reject the historic copy.
func migrateUnifiedMTLSOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, unifiedMTLSMigrationID, unifiedMTLSPredecessorSchemaDigest)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report.LegacySchemaDigest = stored
	if err := verifyUnifiedAuthHistory(ctx, conn, true); err != nil {
		return report, err
	}
	// 已退役子系统的 checklist 历史行在重建前清理：其主题对象（slot 凭据、
	// Lintel 恢复围栏）已不存在，保留会违反收窄后的 maintenance_items.kind。
	// 前置版本把 checklist 冻结为不可删历史；本迁移就是权威的 schema 变更
	// 通道，先解除该保护再清理（重建会安装新 schema 的触发器集合）。
	if _, err := conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS trg_maintenance_items_no_delete`); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM maintenance_items WHERE kind IN ('RuntimeSlot','LintelRecoveryFence')`); err != nil {
		return report, err
	}
	if err := rebuildCanonicalSchema(ctx, conn, nil, false); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`,
		unifiedMTLSMigrationID, migrationDigest(unifiedMTLSMigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	_, err = conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow())
	return report, err
}
