package analysis

// Deterministic input rebuild (ARCH-CONTEXT-006): the snapshot row stores
// only the schema kind and digest; the canonical bytes are rebuilt on
// demand from durable occurrence lineage, frozen integration references
// and the chat contract, and must reproduce the frozen digest exactly
// before any dispatch.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// queryer is the minimal query surface the rebuild needs (a pool handle
// outside transactions, the transaction connection inside them — SQLite is
// single-writer and a nested pool fetch would deadlock).
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// occurrenceContextColumns 是 occurrence 语义投影的共享列清单（ADR-0012 归一化
// 层：severity/title/annotations_canonical/resource 均为首观测冻结列，创建与
// 重建读同一批列，不再从交付 body 现算 annotations）。
const occurrenceContextColumns = `state,first_seen_at,last_state_change_at,resolved_at,labels_canonical,
	severity,title,annotations_canonical,resource`

// loadOccurrenceContext 装载一个 occurrence 的完整冻结上下文：统一语义列、
// 首观测富化终值（alert_enrichments 不可变）与视图关联证据
// （alert_occurrence_correlations 不可变快照）。三个来源全部首观测冻结，
// 重建时字节恒定。
func loadOccurrenceContext(ctx context.Context, queries queryer, occurrenceID int64, occurrence *OccurrenceContext) error {
	var resolvedAt sql.NullString
	var labelsJSON, annotationsJSON string
	err := queries.QueryRowContext(ctx, `
		SELECT `+occurrenceContextColumns+`
		FROM alert_occurrences WHERE id=?`, occurrenceID).
		Scan(&occurrence.State, &occurrence.FirstSeenAt, &occurrence.LastStateChange, &resolvedAt,
			&labelsJSON, &occurrence.Severity, &occurrence.Title, &annotationsJSON, &occurrence.Resource)
	if err != nil {
		return err
	}
	occurrence.ID = strconv.FormatInt(occurrenceID, 10)
	if resolvedAt.Valid {
		value := resolvedAt.String
		occurrence.ResolvedAt = &value
	}
	if err := json.Unmarshal([]byte(labelsJSON), &occurrence.Labels); err != nil {
		return err
	}
	if annotationsJSON != "" && annotationsJSON != "{}" {
		if err := json.Unmarshal([]byte(annotationsJSON), &occurrence.Annotations); err != nil {
			return err
		}
	}
	// 首观测富化终值（fields 是叠加结果；rules 溯源留在存储里不进模型输入）。
	var enrichmentJSON sql.NullString
	if err := queries.QueryRowContext(ctx, `
		SELECT enrichment_json FROM alert_enrichments WHERE occurrence_id=?`, occurrenceID).
		Scan(&enrichmentJSON); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if enrichmentJSON.Valid && enrichmentJSON.String != "" {
		var document struct {
			Fields map[string]string `json:"fields"`
		}
		if err := json.Unmarshal([]byte(enrichmentJSON.String), &document); err != nil {
			return fmt.Errorf("decode frozen alert enrichment: %w", err)
		}
		if len(document.Fields) > 0 {
			occurrence.Enrichment = document.Fields
		}
	}
	rows, err := queries.QueryContext(ctx, `
		SELECT view_key, display_name FROM alert_occurrence_correlations
		WHERE occurrence_id=? ORDER BY matched_at, id`, occurrenceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var correlation RenderedCorrelation
		if err := rows.Scan(&correlation.ViewKey, &correlation.DisplayName); err != nil {
			return err
		}
		occurrence.Correlations = append(occurrence.Correlations, correlation)
	}
	return rows.Err()
}

// relatedAlertWindowHours 是相关告警窗口口径（ADR-0012）：同关联视图或同来源、
// 首观测时间前 24 小时内、最近 10 条其它 occurrence。窗口集合在创建时冻结为
// 谱系项，重建按谱系回读，集合不随后续新告警漂移。
const (
	relatedAlertWindowHours = 24
	relatedAlertLimit       = 10
)

// selectRelatedAlerts 查询一个 occurrence 的相关告警窗口（创建路径）：命中
// 视图（该 occurrence 的关联快照 view_key）或同 source 的其它 occurrence，
// first_seen_at 落在 [自身首观测-24h, 自身首观测] 区间，按首观测倒序取前 10。
// severity/title/starts_at 是不可变列；state 与主 occurrence 同为活读（既有
// digest 语义）。
func selectRelatedAlerts(ctx context.Context, queries queryer, occurrence *OccurrenceContext) ([]RenderedRelatedAlert, error) {
	occurrenceID, err := strconv.ParseInt(occurrence.ID, 10, 64)
	if err != nil {
		return nil, err
	}
	firstSeen, err := time.Parse(time.RFC3339Nano, occurrence.FirstSeenAt)
	if err != nil {
		return nil, fmt.Errorf("occurrence first_seen_at unparseable: %w", err)
	}
	windowStart := firstSeen.Add(-relatedAlertWindowHours * time.Hour).Format(time.RFC3339Nano)
	viewKeys := make([]any, 0, len(occurrence.Correlations))
	placeholders := ""
	for index, correlation := range occurrence.Correlations {
		if index > 0 {
			placeholders += ","
		}
		placeholders += "?"
		viewKeys = append(viewKeys, correlation.ViewKey)
	}
	// 关联视图命中（冻结 view_key 精确匹配）或同 source；无关联视图时退化为
	// 仅同 source（与 initial_analysis 输入契约一致）。
	scope := `o.source_id=(SELECT source_id FROM alert_occurrences WHERE id=?)`
	if len(viewKeys) > 0 {
		scope = `EXISTS (
			  SELECT 1 FROM alert_occurrence_correlations rc
			  WHERE rc.occurrence_id=o.id AND rc.view_key IN (`+placeholders+`))
			OR o.source_id=(SELECT source_id FROM alert_occurrences WHERE id=?)`
	}
	rows, err := queries.QueryContext(ctx, `
		SELECT o.id, o.severity, o.title, o.state, o.starts_at
		FROM alert_occurrences o
		WHERE o.id<>?
		  AND o.first_seen_at>=? AND o.first_seen_at<=?
		  AND (`+scope+`)
		ORDER BY o.first_seen_at DESC, o.id DESC
		LIMIT ?`, append([]any{occurrenceID, windowStart, occurrence.FirstSeenAt}, append(viewKeys, occurrenceID, relatedAlertLimit)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var related []RenderedRelatedAlert
	for rows.Next() {
		var id int64
		var item RenderedRelatedAlert
		if err := rows.Scan(&id, &item.Severity, &item.Title, &item.State, &item.StartsAt); err != nil {
			return nil, err
		}
		item.ID = strconv.FormatInt(id, 10)
		related = append(related, item)
	}
	return related, rows.Err()
}

// loadRelatedAlerts 按冻结谱系回读相关告警（重建路径）：item_seq 序即创建时
// 的窗口顺序；字段来自不可变列 + 活读 state，与创建路径同一投影。
func loadRelatedAlerts(ctx context.Context, queries queryer, attemptID int64) ([]RenderedRelatedAlert, error) {
	rows, err := queries.QueryContext(ctx, `
		SELECT o.id, o.severity, o.title, o.state, o.starts_at
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id
			AND item.item_role='related_occurrence' AND item.occurrence_id IS NOT NULL
		JOIN alert_occurrences o ON o.id=item.occurrence_id
		WHERE snapshot.attempt_id=?
		ORDER BY item.item_seq`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var related []RenderedRelatedAlert
	for rows.Next() {
		var id int64
		var item RenderedRelatedAlert
		if err := rows.Scan(&id, &item.Severity, &item.Title, &item.State, &item.StartsAt); err != nil {
			return nil, err
		}
		item.ID = strconv.FormatInt(id, 10)
		related = append(related, item)
	}
	return related, rows.Err()
}

// RebuildInput rebuilds the canonical initial_analysis_v1 input for one
// attempt from its frozen lineage: the occurrence reference, the related
// alert window frozen as lineage items, the frozen integration revisions
// and the frozen chat contract. The dispatch path verifies the digest
// against the snapshot row, so any drift here fails dispatch instead of
// silently diverging. ADR-0012 首发收敛：无历史 renderer 分叉，旧
// business_systems/Label Contract 谱系已随整域退役删除。
func (service *Service) RebuildInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var occurrenceID, probeResultID int64
	err := service.runner.Reader().QueryRowContext(ctx, `
		SELECT occurrence.occurrence_id, grant.qualified_probe_result_id
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items occurrence ON occurrence.snapshot_id=snapshot.id AND occurrence.occurrence_id IS NOT NULL
		JOIN attempt_connection_grants grant ON grant.attempt_id=snapshot.attempt_id AND grant.purpose='chat_model'
		WHERE snapshot.attempt_id=?`, attemptID).Scan(&occurrenceID, &probeResultID)
	if err != nil {
		return nil, fmt.Errorf("attempt %d immutable input lineage missing: %w", attemptID, err)
	}
	// The frozen catalog document is part of the digested input; rebuilding
	// reads the stored column so an enablement change never drifts history.
	catalog, err := attempt.FrozenToolCatalogDoc(ctx, service.runner.Reader(), attemptID)
	if err != nil {
		return nil, err
	}
	var input Input
	if err := loadOccurrenceContext(ctx, service.runner.Reader(), occurrenceID, &input.Occurrence); err != nil {
		return nil, err
	}
	related, err := loadRelatedAlerts(ctx, service.runner.Reader(), attemptID)
	if err != nil {
		return nil, err
	}
	input.Occurrence.RelatedAlerts = related
	integrations, err := frozenIntegrations(ctx, service.runner.Reader(), attemptID)
	if err != nil {
		return nil, err
	}
	input.Integrations = integrations
	input.ToolCatalog = catalog
	if err := service.runner.Reader().QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, probeResultID).
		Scan(&input.ModelContract.ModelID, &input.ModelContract.ContextBudgetTokens, &input.ModelContract.MaxOutputTokens); err != nil {
		return nil, err
	}
	return json.Marshal(input)
}

// frozenIntegrations reconstructs the frozen source-level authority from the
// attempt's item lineage: the exact connections and revisions frozen at
// creation, so later enablement churn cannot re-interpret the snapshot.
func frozenIntegrations(ctx context.Context, queries queryer, attemptID int64) ([]RenderedIntegration, error) {
	rows, err := queries.QueryContext(ctx, `
		SELECT CASE WHEN c.type='kubernetes' THEN 'kubernetes' ELSE 'metrics' END AS kind, c.name
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id
			AND item.item_role IN ('metrics_source','kubernetes_source')
			AND item.connection_revision_id IS NOT NULL
		JOIN connection_revisions r ON r.id=item.connection_revision_id
		JOIN connections c ON c.id=r.connection_id
		WHERE snapshot.attempt_id=?
		ORDER BY c.name, kind`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var integrations []RenderedIntegration
	for rows.Next() {
		var integration RenderedIntegration
		if err := rows.Scan(&integration.Kind, &integration.Name); err != nil {
			return nil, err
		}
		integrations = append(integrations, integration)
	}
	return integrations, rows.Err()
}
