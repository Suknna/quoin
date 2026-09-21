package investigation

// 来源级授权的冻结谱系（ADR-0004，原 business_context.go 的存留部分；
// business_system 授权模型已随 ADR-0012 整域退役删除）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// insertSourceLineageItems freezes one input item per enabled observation
// integration at its current revision (ADR-0004 source-level authority).
// The frozen revision is the grant-eligible set: later thanos_query and
// kubernetes_read grants must match these items exactly, and rotations
// create new revisions so the attempt's authority stays reconstructible.
func insertSourceLineageItems(ctx context.Context, tx writer, snapshotID, firstSeq int64) (int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT name, current_revision_id FROM connections
		WHERE type IN ('thanos','prometheus','kubernetes') AND enabled=1 AND revalidation_required=0
		ORDER BY name, current_revision_id`)
	if err != nil {
		return 0, err
	}
	type frozen struct {
		name       string
		revisionID int64
	}
	var sources []frozen
	for rows.Next() {
		var source frozen
		if err := rows.Scan(&source.name, &source.revisionID); err != nil {
			rows.Close()
			return 0, err
		}
		sources = append(sources, source)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for index, source := range sources {
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT type FROM connections WHERE current_revision_id=?`, source.revisionID).Scan(&kind); err != nil {
			return 0, err
		}
		role := "metrics_source"
		if kind == "kubernetes" {
			role = "kubernetes_source"
		}
		digest := sha256.Sum256([]byte("connection-revision:" + strconv.FormatInt(source.revisionID, 10)))
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id)
			VALUES(?,?,?,?,?)`, snapshotID, firstSeq+int64(index), role, hex.EncodeToString(digest[:]), source.revisionID); err != nil {
			return 0, err
		}
	}
	return int64(len(sources)), nil
}

// enabledIntegrations renders the model-visible projection of the enabled
// observation integrations in the same deterministic order as the frozen
// lineage items. It composes on the runner's guarded transaction just like
// on a plain pool handle (writer).
func enabledIntegrations(ctx context.Context, tx writer) ([]RenderedIntegration, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT name, type FROM connections
		WHERE type IN ('thanos','prometheus','kubernetes') AND enabled=1 AND revalidation_required=0
		ORDER BY name, type`)
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
		kind := "metrics"
		if connectionType == "kubernetes" {
			kind = "kubernetes"
		}
		integrations = append(integrations, RenderedIntegration{Kind: kind, Name: name})
	}
	return integrations, rows.Err()
}
