package attempt

// 巡检分析版本身份回归：报告 prompt 的渲染代次必须有自己的版本身份，绝不在
// initial-analysis 的共享身份下静默漂移。

import "testing"

func TestInspectionAgentVersionHasItsOwnRendererGeneration(t *testing.T) {
	if InspectionAgentVersion == AgentVersion {
		t.Fatal("inspection analysis must not share the initial-analysis executor generation")
	}
	if got := promptRendererVersionFor(InspectionAgentVersion); got != "inspection-analysis-renderer-v2" {
		t.Fatalf("current inspection renderer version = %q", got)
	}
	if got := promptRendererVersionFor(PreviousInspectionAgentVersion); got != "inspection-analysis-renderer-v1" {
		t.Fatalf("previous inspection renderer version = %q", got)
	}
	if got := promptRendererVersionFor(AgentVersion); got != "initial-analysis-renderer-v4" {
		t.Fatalf("initial analysis renderer version = %q", got)
	}
	if got := promptRendererVersionFor("investigation-v1"); got != "investigation-renderer-v2" {
		t.Fatalf("legacy investigation renderer version = %q", got)
	}
	if got := promptRendererVersionFor("investigation-v2"); got != "investigation-renderer-v3" {
		t.Fatalf("current investigation renderer version = %q", got)
	}
}
