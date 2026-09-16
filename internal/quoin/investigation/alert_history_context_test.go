package investigation

// The investigation prompt context must carry Quoin's own recent alert
// history: a free-form conversation asking "summarize current alerts" is
// otherwise blind to resolved occurrences, and instant ALERTS queries are
// empty once an alert recovers (the frozen lineage keeps the history
// reproducible — ARCH-CONTEXT-006).

import (
	"strings"
	"testing"
)

func TestInvestigationInputCarriesRecentAlertHistory(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	principalID := seedUser(t, db)
	ctx := userContext(t, principalID)
	seedProviderChain(t, db)
	occurrenceID := seedOccurrence(t, db, "MallShopMiddlewareTargetDown")

	result, err := service.Create(ctx, principalID, "cmd-history-context", "总结当前最需要处理的告警", nil, nil)
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
		result.AttemptID, occurrenceID).Scan(&historyItems); err != nil {
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
		!strings.Contains(string(canonical), `"MallShopMiddlewareTargetDown"`) {
		t.Fatalf("input lacks Quoin alert history context: %s", canonical)
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
