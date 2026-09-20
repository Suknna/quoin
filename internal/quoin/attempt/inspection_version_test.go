package attempt

// 版本身份回归：报告/分析/调查 prompt 的渲染代次必须有自己的版本身份，绝不
// 在共享身份下静默漂移。Keep 提示词迁入后每类任务新增一代，映射同步扩充。

import "testing"

func TestInspectionAgentVersionHasItsOwnRendererGeneration(t *testing.T) {
	if InspectionAgentVersion == AgentVersion {
		t.Fatal("inspection analysis must not share the initial-analysis executor generation")
	}
	if got := promptRendererVersionFor(InspectionAgentVersion); got != "inspection-analysis-renderer-v3" {
		t.Fatalf("current inspection renderer version = %q", got)
	}
}

func TestInitialAnalysisRendererGenerations(t *testing.T) {
	// Keep 适配代：prompt 纯文本变化即新渲染代次。
	if got := promptRendererVersionFor(AgentVersion); got != "initial-analysis-renderer-v5" {
		t.Fatalf("current initial analysis renderer version = %q", got)
	}
}

func TestInvestigationRendererGenerations(t *testing.T) {
	if got := promptRendererVersionFor("investigation-v3"); got != "investigation-renderer-v4" {
		t.Fatalf("current investigation renderer version = %q", got)
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
