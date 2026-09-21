package investigation

// Renderer v5（ADR-0012）：调查输入的近期告警记录按「会话来源告警的关联
// 视图」圈定——即时查询 ALERTS 为空只说明当前没有 firing 序列，会话相关的
// 已恢复告警仍需进入上下文；冻结谱系保证 digest 可复现（ARCH-CONTEXT-006）。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInvestigationInputCarriesCorrelatedAlertHistory(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	principalID := seedUser(t, db)
	ctx := userContext(t, principalID)
	seedProviderChain(t, db)
	sourceID := seedAlertSourceForHistory(t, db, "history-context")
	related := seedHistoryOccurrence(t, db, sourceID, "MallShopPaymentLatency", "2026-09-20T09:00:00Z")
	seedCorrelation(t, db, related, "mall", "商城")
	occurrenceID := seedHistoryOccurrence(t, db, sourceID, "MallShopMiddlewareTargetDown", "2026-09-20T10:00:00Z")
	seedCorrelation(t, db, occurrenceID, "mall", "商城")

	result, err := service.Create(ctx, principalID, "cmd-history-context", "总结当前最需要处理的告警", nil, []SourceInput{{Type: "occurrence", SourceID: occurrenceID}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The frozen lineage records the history occurrence so the digest stays
	// rebuildable from durable references alone.
	var historyItems int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM attempt_input_items i
		JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		WHERE s.attempt_id=? AND i.item_role='history_occurrence' AND i.occurrence_id=?`,
		result.AttemptID, related).Scan(&historyItems); err != nil {
		t.Fatal(err)
	}
	if historyItems != 1 {
		t.Fatalf("history lineage items=%d want 1", historyItems)
	}
	canonical, err := service.RebuildInput(ctx, result.AttemptID)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !strings.Contains(string(canonical), `"recentOccurrences"`) ||
		!strings.Contains(string(canonical), `"MallShopPaymentLatency"`) {
		t.Fatalf("input lacks correlated alert history context: %s", canonical)
	}
	var input Input
	if err := json.Unmarshal(canonical, &input); err != nil {
		t.Fatal(err)
	}
	if len(input.RecentOccurrences) != 1 || input.RecentOccurrences[0].Labels["alertname"] != "MallShopPaymentLatency" {
		t.Fatalf("recent occurrences=%+v want only the correlated neighbour", input.RecentOccurrences)
	}
	// The rebuild must reproduce the frozen bytes exactly (no mutable
	// occurrence state such as state/resolvedAt may leak into the render).
	again, err := service.RebuildInput(ctx, result.AttemptID)
	if err != nil {
		t.Fatalf("rebuild again: %v", err)
	}
	if string(again) != string(canonical) {
		t.Fatal("rebuild drifted from frozen digest bytes")
	}
}
