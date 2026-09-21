// 告警视图关联（ADR-0012 intake 流水线 Correlate 段）：首观测创建 occurrence
// 时一次性求值并冻结"全部命中视图"。匹配权威沿用 ADR-0008 的视图范围语义——
// 视图必须在其 alert_source_keys_json 显式声明交付的 Alertmanager 告警源，
// 且非空精确标签条件全部命中；空标签条件绝不构成兜底匹配，Alertmanager 来源
// 身份绝不与 Prometheus connection 身份互换。与旧归属状态机（attributed/
// ambiguous/unattributed、candidates/reason）不同：多命中不再裁决唯一归属，
// 而是每命中视图冻结一行（alert_occurrence_correlations），视图改名/退役后
// 快照不漂移，后续视图编辑绝不改写已冻结证据。
package alerts

import (
	"context"
)

// correlatedView 是一个命中视图在关联行中冻结的最小身份快照。
type correlatedView struct {
	ViewID      int64
	ViewKey     string
	DisplayName string
}

// correlateViews 在首观测事务内求值当前业务视图。返回的每一项都是显式声明
// 了交付告警源（alert_source_keys_json 包含该源的 source_key）、声明了至少
// 一个标签条件且全部条件被交付标签精确满足的视图；按视图 id 稳定排序。
func correlateViews(ctx context.Context, conn txQuerier, sourceID int64, labels map[string]string) ([]correlatedView, error) {
	canonical, err := CanonicalLabels(labels)
	if err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT v.id, v.view_key, v.display_name,
			(SELECT COUNT(*) FROM json_each(v.label_conditions_json)) AS condition_count,
			EXISTS(SELECT 1 FROM json_each(v.label_conditions_json) conditions
				WHERE NOT EXISTS(SELECT 1 FROM json_each(?) incoming
					WHERE incoming.key=conditions.key AND incoming.value=conditions.value)) AS labels_differ
		FROM business_views v
		WHERE EXISTS(SELECT 1 FROM json_each(v.alert_source_keys_json) scoped
			WHERE scoped.value = (SELECT source_key FROM alert_sources WHERE id=?))
		ORDER BY v.id`, canonical, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	matched := []correlatedView{}
	for rows.Next() {
		var view correlatedView
		var conditionCount, labelsDiffer int
		if err := rows.Scan(&view.ViewID, &view.ViewKey, &view.DisplayName, &conditionCount, &labelsDiffer); err != nil {
			return nil, err
		}
		// 空标签条件绝不构成吞掉一切的兜底匹配：无条件视图即使声明了来源也
		// 不构成关联（沿用 ADR-0008 语义）。
		if conditionCount == 0 || labelsDiffer != 0 {
			continue
		}
		matched = append(matched, view)
	}
	return matched, rows.Err()
}

// persistCorrelations 把每个命中视图冻结成一行关联证据，与 occurrence 首观测
// 同事务写入；写入后由 schema 触发器拒绝任何 UPDATE/DELETE。
func persistCorrelations(ctx context.Context, conn txQuerier, occurrenceID int64, matched []correlatedView, matchedAt string) error {
	for _, view := range matched {
		if _, err := conn.ExecContext(ctx, `INSERT INTO alert_occurrence_correlations(occurrence_id,view_id,view_key,display_name,matched_at) VALUES(?,?,?,?,?)`,
			occurrenceID, view.ViewID, view.ViewKey, view.DisplayName, matchedAt); err != nil {
			return err
		}
	}
	return nil
}
