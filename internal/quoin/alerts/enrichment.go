// 告警富化（ADR-0012 intake 流水线 Enrich 段）：首观测事务内对启用中的
// enrichment_rules 求值——来源 key 命中（或规则不限来源）且全部精确标签条件
// 命中的规则，按 priority 升序（同序按创建序）叠加 outputs；后命中的规则不
// 覆盖已写字段（priority 优先者胜）。求值结果连同命中规则溯源冻结进
// alert_enrichments；即使无规则命中也写一行（fields 为空对象），用于区分
// "无规则命中"与"未求值"。与旧归属/新关联完全正交：富化不裁决归属，关联不
// 产出字段。
package alerts

import (
	"context"
	"encoding/json"
)

// enrichmentRuleTrace 是一个命中规则的冻结溯源（按求值序）。
type enrichmentRuleTrace struct {
	Key     string            `json:"key"`
	Outputs map[string]string `json:"outputs"`
}

// enrichmentDocument 是 alert_enrichments.enrichment_json 的冻结形状：
// fields 是叠加终值（后命中不覆盖），rules 按求值序记录每个命中规则的
// 完整声明输出（含被更高优先级遮蔽的字段，供审计区分"声明了"与"生效了"）。
type enrichmentDocument struct {
	Fields map[string]string     `json:"fields"`
	Rules  []enrichmentRuleTrace `json:"rules"`
}

// evaluateEnrichment 在首观测事务内求值当前启用的富化规则并渲染冻结文档。
// sourceKey 是交付告警源的稳定 key；labels 是首观测冻结的完整标签集。
// 无规则命中时返回 {"fields":{},"rules":[]}，仍由调用方落库。
func evaluateEnrichment(ctx context.Context, conn txQuerier, sourceKey string, labels map[string]string) (string, error) {
	canonical, err := CanonicalLabels(labels)
	if err != nil {
		return "", err
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT rule_key, outputs_json,
			EXISTS(SELECT 1 FROM json_each(label_conditions_json) conditions
				WHERE NOT EXISTS(SELECT 1 FROM json_each(?) incoming
					WHERE incoming.key=conditions.key AND incoming.value=conditions.value)) AS labels_differ
		FROM enrichment_rules
		WHERE enabled = 1
			AND (NOT EXISTS(SELECT 1 FROM json_each(alert_source_keys_json))
				OR EXISTS(SELECT 1 FROM json_each(alert_source_keys_json) scoped WHERE scoped.value = ?))
		ORDER BY priority ASC, id ASC`, canonical, sourceKey)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	document := enrichmentDocument{Fields: map[string]string{}, Rules: []enrichmentRuleTrace{}}
	for rows.Next() {
		var ruleKey, outputsJSON string
		var labelsDiffer int
		if err := rows.Scan(&ruleKey, &outputsJSON, &labelsDiffer); err != nil {
			return "", err
		}
		if labelsDiffer != 0 {
			continue
		}
		outputs := map[string]string{}
		if err := json.Unmarshal([]byte(outputsJSON), &outputs); err != nil {
			return "", err
		}
		// priority 升序叠加：后命中的规则不覆盖已写字段（首写者胜）。
		for name, value := range outputs {
			if _, exists := document.Fields[name]; !exists {
				document.Fields[name] = value
			}
		}
		document.Rules = append(document.Rules, enrichmentRuleTrace{Key: ruleKey, Outputs: outputs})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// persistEnrichment 与 occurrence 首观测同事务冻结富化文档（即使 fields 为空
// 也写入），写入后由 schema 触发器拒绝任何 UPDATE/DELETE。
func persistEnrichment(ctx context.Context, conn txQuerier, occurrenceID int64, documentJSON, evaluatedAt string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO alert_enrichments(occurrence_id,enrichment_json,evaluated_at) VALUES(?,?,?)`,
		occurrenceID, documentJSON, evaluatedAt)
	return err
}
