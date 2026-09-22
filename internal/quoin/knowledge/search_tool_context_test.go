package knowledge

// search_tool_context_test.go 回归 2026-09-21 实机知识链路问题：quoin_routed
// 工具 knowledge_search 在编排侧以裸 context（无执行元数据）调用 Search 时，
// 语义通道必须照常服务——检索词 embedding 的查询 Attempt 创建不能因为发起
// 方是运行时后台续跑（而非 HTTP 用户会话）而失败；失败被诚实降级为空语义通
// 道时，模型会得出"知识不存在"的错误结论。

import (
	"context"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/testfixture"
)

// TestSearchSemanticChannelServesToolExecutionContext 复刻实机故障形态：
// 索引 ready、查询 dispatcher 可用，唯一差异是调用方 context 与
// executeRoutedToolCall 一致（context.Background 派生、零执行元数据）。
// 语义通道必须返回命中，而不是静默失败为空通道。
func TestSearchSemanticChannelServesToolExecutionContext(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	confirmed := confirmFromSource(t, f, "tool-ctx", false)
	if err := f.service.Embeddings().Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	driveEmbedding(t, f, func(item rebuildItem) []float32 { return unitQuery() })
	f.service.Embeddings().SetDispatcher(wireQueryDispatcher(t, f, unitQuery()))

	// 与 internal/quoin/app.executeRoutedToolCall 的 execCtx 同构：裸
	// context 派生、不携带任何执行元数据。
	toolCtx := context.Background()
	result, _, err := f.service.Search(toolCtx, "耗尽导致", nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SemanticMatches) == 0 {
		t.Fatal("semantic channel is empty under the routed-tool context though the index is ready: the model would conclude the knowledge does not exist")
	}
	if result.SemanticMatches[0].Knowledge.ID != confirmed {
		t.Fatalf("semantic hit = %+v, want the confirmed knowledge %s", result.SemanticMatches, confirmed)
	}
	if result.SemanticMatches[0].IndexState != "ready" {
		t.Fatalf("semantic index state = %q, want ready", result.SemanticMatches[0].IndexState)
	}
}

// TestSearchSemanticChannelServesSystemContinuationContext 覆盖修复后的归属
// 形态：编排侧给工具执行补挂系统主体元数据（后台续跑窗口）时，语义通道同
// 样必须服务。锁住"系统主体可创建查询 Attempt"的授权语义。
func TestSearchSemanticChannelServesSystemContinuationContext(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	confirmFromSource(t, f, "sys-ctx", false)
	if err := f.service.Embeddings().Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	driveEmbedding(t, f, func(item rebuildItem) []float32 { return unitQuery() })

	var mu sync.Mutex
	dispatched := false
	base := wireQueryDispatcher(t, f, unitQuery())
	f.service.Embeddings().SetDispatcher(func(ctx context.Context, attemptID int64) error {
		mu.Lock()
		dispatched = true
		mu.Unlock()
		return base(ctx, attemptID)
	})

	// 与 RebuildSearchDocs 的后台窗口同型的系统主体元数据。
	toolCtx := testfixture.SystemContext(t, "knowledge-tool-search")
	result, _, err := f.service.Search(toolCtx, "耗尽导致", nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SemanticMatches) == 0 {
		t.Fatal("semantic channel is empty under the system-continuation context though the index is ready")
	}
	mu.Lock()
	defer mu.Unlock()
	if !dispatched {
		t.Fatal("query attempt was never dispatched")
	}
}
