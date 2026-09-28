package investigation

// 来源级授权的冻结谱系（ADR-0004，原 business_context.go 的存留部分；
// business_system 授权模型已随 ADR-0012 整域退役删除）。渲染与冻结的范围
// 来自声明派生的 scope 计划（attempt.SourceScopes）：新信任插件按声明即
// 进入冻结来源权威，宿主不再维护连接类型表。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// insertSourceLineageItems freezes one input item per enabled source
// integration of the declared scope plan at its current revision (ADR-0004
// source-level authority). The frozen revision is the grant-eligible set:
// later grant-scoped tool calls must match these items exactly, and
// rotations create new revisions so the attempt's authority stays
// reconstructible. The per-connection role comes from the declared plan.
func insertSourceLineageItems(ctx context.Context, tx writer, snapshotID, firstSeq int64, table attempt.SourceScopeTable) (int64, error) {
	if len(table.Types()) == 0 {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT name, type, current_revision_id FROM connections
		WHERE type IN (`+table.Placeholders()+`) AND enabled=1 AND revalidation_required=0
		ORDER BY name, current_revision_id`, typesAsArgs(table.Types())...)
	if err != nil {
		return 0, err
	}
	type frozen struct {
		role       string
		revisionID int64
	}
	var sources []frozen
	for rows.Next() {
		var source frozen
		var name, connectionType string
		if err := rows.Scan(&name, &connectionType, &source.revisionID); err != nil {
			rows.Close()
			return 0, err
		}
		source.role = table.RoleOf(connectionType)
		sources = append(sources, source)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for index, source := range sources {
		digest := sha256.Sum256([]byte("connection-revision:" + strconv.FormatInt(source.revisionID, 10)))
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id)
			VALUES(?,?,?,?,?)`, snapshotID, firstSeq+int64(index), source.role, hex.EncodeToString(digest[:]), source.revisionID); err != nil {
			return 0, err
		}
	}
	return int64(len(sources)), nil
}

// enabledIntegrations renders the model-visible projection of the enabled
// source integrations of the declared scope plan, in the same deterministic
// order as the frozen lineage items. It composes on the runner's guarded
// transaction just like on a plain pool handle (writer).
func enabledIntegrations(ctx context.Context, tx writer, table attempt.SourceScopeTable) ([]RenderedIntegration, error) {
	if len(table.Types()) == 0 {
		return []RenderedIntegration{}, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT name, type FROM connections
		WHERE type IN (`+table.Placeholders()+`) AND enabled=1 AND revalidation_required=0
		ORDER BY name, type`, typesAsArgs(table.Types())...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var integrations []RenderedIntegration
	for rows.Next() {
		var name, connectionType string
		if err := rows.Scan(&name, &connectionType); err != nil {
			return nil, err
		}
		integrations = append(integrations, RenderedIntegration{Kind: table.KindOf(table.RoleOf(connectionType)), Name: name})
	}
	return integrations, rows.Err()
}

// typesAsArgs renders the scope type list as query arguments.
func typesAsArgs(types []string) []any {
	args := make([]any, len(types))
	for index, value := range types {
		args[index] = value
	}
	return args
}
