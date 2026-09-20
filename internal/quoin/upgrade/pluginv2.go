package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// 插件体系 v2 执行模式转换（ADR-0011，2026-09-20）：quoin_routed 取代
// supervisor_typed 成为出向工具的执行模式——工具由 Quoin 编排、Stele 网关执行。
// 本迁移只扩展 tool_calls.execution_mode 的 CHECK 词表并重建规范 schema；
// 历史行的 supervisor_typed 是既成事实，保留不回写。
const (
	pluginV2PredecessorSchemaDigest = "9b5662ac8e4f14c9992fc84d833dcf62a51862be46a6f8959153499a27b95e91"
	pluginV2MigrationID             = "20260920_plugin_v2_execution_mode_v1"
)

// migratePluginV2On applies the execution-mode vocabulary extension with the
// canonical rebuild; every retained business row survives verbatim.
func migratePluginV2On(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, pluginV2MigrationID, pluginV2PredecessorSchemaDigest)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report.LegacySchemaDigest = stored
	if err := rebuildCanonicalSchema(ctx, conn, nil, false); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`,
		pluginV2MigrationID, migrationDigest(pluginV2MigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	_, err = conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow())
	return report, err
}
