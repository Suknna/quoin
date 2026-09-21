package attempt

// 版本身份回归：报告/分析/调查 prompt 的渲染代次必须有自己的版本身份，绝不
// 在共享身份下静默漂移。Keep 提示词迁入后每类任务新增一代，知识接入代再各
// 新增一代，映射同步扩充。

import "testing"

func TestInspectionAgentVersionHasItsOwnRendererGeneration(t *testing.T) {
	if InspectionAgentVersion == AgentVersion {
		t.Fatal("inspection analysis must not share the initial-analysis executor generation")
	}
	if got := promptRendererVersionFor(InspectionAgentVersion); got != "inspection-analysis-renderer-v4" {
		t.Fatalf("current inspection renderer version = %q", got)
	}
	// 旧代保持各自冻结身份，供在途/历史 Attempt 溯源解读。
	if got := promptRendererVersionFor("inspection-analysis-v3"); got != "inspection-analysis-renderer-v3" {
		t.Fatalf("kept inspection renderer version = %q", got)
	}
}

func TestInitialAnalysisRendererGenerations(t *testing.T) {
	// 知识接入代：prompt 纯文本变化即新渲染代次。
	if got := promptRendererVersionFor(AgentVersion); got != "initial-analysis-renderer-v6" {
		t.Fatalf("current initial analysis renderer version = %q", got)
	}
	if got := promptRendererVersionFor("initial-analysis-v2"); got != "initial-analysis-renderer-v5" {
		t.Fatalf("kept initial analysis renderer version = %q", got)
	}
}

func TestInvestigationRendererGenerations(t *testing.T) {
	// 知识接入调查代跳过 v5（ADR-0012 输入形状 renderer 已占用），自 v6 对齐。
	if got := promptRendererVersionFor("investigation-v4"); got != "investigation-renderer-v6" {
		t.Fatalf("current investigation renderer version = %q", got)
	}
	if got := promptRendererVersionFor("investigation-v3"); got != "investigation-renderer-v4" {
		t.Fatalf("kept investigation renderer version = %q", got)
	}
}

func TestKnowledgeAgentVersionStaysOnOriginalSharedIdentity(t *testing.T) {
	// knowledge 抽取的 prompt 从未演进：显式固定在原共享身份，不随 analysis
	// prompt 代际升级，保证其 model_calls 溯源与输出契约不变。
	if KnowledgeAgentVersion != "initial-analysis-v1" {
		t.Fatalf("knowledge agent version = %q, want the original shared identity", KnowledgeAgentVersion)
	}
	if got := promptRendererVersionFor(KnowledgeAgentVersion); got != "initial-analysis-renderer-v4" {
		t.Fatalf("knowledge renderer version = %q", got)
	}
}
