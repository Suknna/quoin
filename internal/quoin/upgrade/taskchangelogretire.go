package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// task_change_log 退役转换（2026-09）：前一个发布版本（digest 见
// taskChangeLogRetirePredecessorSchemaDigest，逐字节捕获于
// testdata/task-change-log-retire-predecessor.sql.gz）之后，该派生日志的唯一
// 读取方（GET /api/v1/tasks/events SSE 与 tasks/snapshot）已随 2026-09 API 精简
// 移除，19 个写入/保护触发器成为无人消费的持续写入。本迁移删除表、索引与全部
// 触发器：重建不回拷即丢弃历史行（DATA-SCOPE-003 声明该表可丢弃、可重建、非
// 权威源；审计权威是 audit_events）。源表的 row_version 列与递增触发器
// （DATA-ROWVER-001）继续服务乐观并发，不受影响。
const (
	taskChangeLogRetirePredecessorSchemaDigest = "8d64f2fa925f9dac7a2750982a95b31486ceea7b63852b91df4c4b4c84f5bee4"
	taskChangeLogRetireMigrationID             = "20260919_retire_task_change_log_v1"
)

// migrateTaskChangeLogRetireOn drops the unread derived log with the canonical
// rebuild; every retained business row survives verbatim.
func migrateTaskChangeLogRetireOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, taskChangeLogRetireMigrationID, taskChangeLogRetirePredecessorSchemaDigest)
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
		taskChangeLogRetireMigrationID, migrationDigest(taskChangeLogRetireMigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	_, err = conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow())
	return report, err
}
