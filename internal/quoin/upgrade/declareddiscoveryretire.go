package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// 声明发现/观测资源三表退役转换（2026-09）：前一个发布版本（digest 见
// declaredDiscoveryRetirePredecessorSchemaDigest，逐字节捕获于
// testdata/declared-discovery-retire-predecessor.sql.gz）之后，这三张表在生产
// 代码中已零消费——现行资源发现由插件描述符驱动（ADR-0004 来源级观测：
// DiscoverObjects 目录 + enablement/schedule 触发，事实落在
// observation_run_objects 与 config_resource_scopes），声明时代的
// config_discoveries 投影与 observed_resources 正式资源投影只剩历史行。本迁移
// 删除 config_discoveries、observed_resources、observed_resource_identity_labels
// 及其触发器/索引：重建不回拷即丢弃历史行（均为可重建派生/历史投影，非权威源；
// 审计权威是 audit_events，声明事实存续于 declaration_json 与
// config_resource_scopes）。声明 cutover 转换链所需的 legacy 发现事实改为在
// rebuild 前预捕获（captureLegacyDiscoveryFacts）。
const (
	declaredDiscoveryRetirePredecessorSchemaDigest = "3ba319dba474c224f3d7e5af2d242e53bbf289be30f1ea15928c888b393aa2fe"
	declaredDiscoveryRetireMigrationID             = "20260920_retire_declared_discovery_v1"
)

// migrateDeclaredDiscoveryRetireOn drops the unread declared-discovery and
// observed-resource projections with the canonical rebuild; every retained
// business row survives verbatim.
func migrateDeclaredDiscoveryRetireOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, declaredDiscoveryRetireMigrationID, declaredDiscoveryRetirePredecessorSchemaDigest)
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
		declaredDiscoveryRetireMigrationID, migrationDigest(declaredDiscoveryRetireMigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	_, err = conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow())
	return report, err
}
