package agent

// 巡检报告消息组装测试：报告要求只在用户级消息；逐检查项结构化清单（身份、
// 冻结语义、表达式与范围、真实窗口、observedAt、warnings、缺口、Evidence/
// Artifact 对应）完整进入用户消息；缺口不伪装成数据；旧形状输入保持兼容。

import (
	"encoding/json"
	"strings"
	"testing"
)

func inspectionFixture(t *testing.T) (InspectionInput, string) {
	t.Helper()
	raw := `{
		"schemaKind": "inspection_analysis_v1",
		"attemptId": 7,
		"inspectionRunId": 3,
		"artifactIds": [11],
		"evidenceIds": [5],
		"modelContract": {"modelId": "fixture-chat-1"},
		"plan": {
			"key": "mall-plan",
			"params": {"expression": "up"},
			"scope": {"kind": "integration"},
			"checkDescription": "检查 Prometheus 连通性",
			"metricUnit": "1=在线",
			"reportInstructions": "冻结的初始报告要求"
		},
		"reportInstructionsOverride": "仅本次：只列异常项",
		"checks": [
			{
				"checkKey": "promql_instant", "displayName": "PromQL 即时查询", "status": "ok",
				"evidenceId": 5, "artifactId": 11,
				"expression": "up", "observedAt": "2026-09-16T00:05:00Z",
				"windowStartAt": "2026-09-16T00:00:00Z", "windowEndAt": "2026-09-16T00:05:00Z",
				"executedStepSeconds": 60, "warnings": ["collection truncated"]
			},
			{
				"checkKey": "promql_range", "displayName": "PromQL 范围查询", "status": "gap",
				"expression": "sum(rate(errors[5m]))", "rangeSeconds": 300, "stepSeconds": 60,
				"observedAt": "2026-09-16T00:05:30Z", "warnings": ["collection truncated"],
				"gapReason": "no_data"
			}
		]
	}`
	var input InspectionInput
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatal(err)
	}
	return input, raw
}

func TestInspectionMessagesKeepInstructionsUserLevelAndExposeChecklist(t *testing.T) {
	input, _ := inspectionFixture(t)
	messages, err := BuildInspectionMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d", len(messages))
	}
	system, user := messages[0].Content, messages[1].Content
	// 证据优先保留在系统提示；报告要求绝不进入系统提示。
	if !strings.Contains(system, "artifact_read") || !strings.Contains(system, "不得引用未读取的内容") {
		t.Fatalf("system prompt lost the evidence-first contract: %q", system)
	}
	if strings.Contains(system, "报告要求") || strings.Contains(system, "只列异常项") {
		t.Fatalf("report requirements must stay user-level: %q", system)
	}
	// 仅本次覆盖优先且注明只作用于本次。
	if !strings.Contains(user, "本次报告要求（仅本次分析生效）") || !strings.Contains(user, "只列异常项") {
		t.Fatalf("override must surface as this-run-only: %q", user)
	}
	// 冻结语义与检查项清单完整可见。
	for _, fragment := range []string{
		"检查说明：检查 Prometheus 连通性", "指标单位：1=在线",
		"checkKey=promql_instant", "名称=PromQL 即时查询",
		"表达式=up", "实际窗口=", "实际步长=60s", "observedAt=2026-09-16T00:05:00Z",
		"warnings=collection truncated", "evidenceId=5", "artifactId=11",
		"checkKey=promql_range", "缺口原因=no_data（无数据，不是 0）",
		"observedAt=2026-09-16T00:05:30Z", "warnings=collection truncated",
		"范围=300s", "未定义阈值的检查项不得判断健康",
	} {
		if !strings.Contains(user, fragment) {
			t.Fatalf("user message missing %q:\n%s", fragment, user)
		}
	}
	// 缺口检查不得携带执行事实或证据 locator。
	if strings.Contains(user, "checkKey=promql_range") && strings.Contains(strings.Split(user, "checkKey=promql_range")[1][:200], "evidenceId=") {
		t.Fatalf("gap check must not fabricate evidence mapping")
	}
}

func TestInspectionMessagesFallBackToFrozenInstructionsAndStayLegacyCompatible(t *testing.T) {
	// 无覆盖时使用 Run 冻结的初始要求，且不出现"仅本次"标注。
	input, _ := inspectionFixture(t)
	input.ReportInstructionsOverride = nil
	messages, err := BuildInspectionMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	user := messages[1].Content
	if !strings.Contains(user, "【本次报告要求】\n冻结的初始报告要求") {
		t.Fatalf("frozen instructions must be the default: %q", user)
	}
	if strings.Contains(user, "仅本次") {
		t.Fatalf("without an override there is no this-run marker: %q", user)
	}
	// 三态之显式清除：字段存在且为空串时如实声明本次无附加要求，绝不静默
	// 回退到冻结值。
	input.ReportInstructionsOverride = new(string)
	cleared, err := BuildInspectionMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	clearedUser := cleared[1].Content
	if !strings.Contains(clearedUser, "本次分析无附加报告要求") {
		t.Fatalf("explicit clear must be declared verbatim: %q", clearedUser)
	}
	if strings.Contains(clearedUser, "冻结的初始报告要求\n") {
		t.Fatalf("explicit clear must not fall back to the frozen requirement: %q", clearedUser)
	}
	text, present := input.EffectiveReportInstructions()
	if !present || text != "" {
		t.Fatalf("cleared override = (%q, %v), want empty with present=true", text, present)
	}
	input.ReportInstructionsOverride = nil
	inherited, inheritedPresent := input.EffectiveReportInstructions()
	if !inheritedPresent || inherited != "冻结的初始报告要求" {
		t.Fatalf("absent override = (%q, %v), want frozen text with present=true", inherited, inheritedPresent)
	}
	// 旧形状（无 plan/checks）保持原样：只有 evidence/artifact 对照。
	legacy := InspectionInput{SchemaKind: "inspection_analysis_v1", AttemptID: 1, InspectionRunID: 1}
	legacy.ModelContract.ModelID = "fixture-chat-1"
	legacy.EvidenceIDs = []int64{5}
	legacy.ArtifactIDs = []int64{11}
	legacyMessages, err := BuildInspectionMessages(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyUser := legacyMessages[1].Content
	if legacyUser != "请读取以下按 Evidence 顺序冻结的 Artifact，然后基于其内容撰写巡检报告。\nevidenceId=5 artifactId=11\n" {
		t.Fatalf("legacy input shape drifted: %q", legacyUser)
	}
}

// TestLegacyInspectionRendererStaysByteExact 钉住升级兼容：上一代巡检渲染
// （初始共享身份下的 prompt 与消息形状）逐字节保留，供在途旧 Attempt 以其
// 冻结时的原始 prompt digest 完成提交。
func TestLegacyInspectionRendererStaysByteExact(t *testing.T) {
	wantPrompt := "你是 Quoin 的只读巡检报告代理。请先使用 artifact_read 或 artifact_grep 读取所有提供的巡检证据文件，再用中文写出事实性巡检报告。不得引用未读取的内容；不得把证据未表达的健康结论、严重性或裁决写入报告。"
	if LegacyInspectionSystemPrompt != wantPrompt {
		t.Fatalf("legacy prompt drifted: %q", LegacyInspectionSystemPrompt)
	}
	if LegacyInspectionSystemPrompt == InspectionSystemPrompt {
		t.Fatal("legacy prompt must differ from the current generation")
	}
	input := InspectionInput{SchemaKind: "inspection_analysis_v1", AttemptID: 1, InspectionRunID: 1}
	input.ModelContract.ModelID = "fixture-chat-1"
	input.EvidenceIDs = []int64{5}
	input.ArtifactIDs = []int64{11}
	messages, err := BuildLegacyInspectionMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	wantUser := "请读取以下按 Evidence 顺序冻结的 Artifact，然后基于其内容撰写巡检报告。\nevidenceId=5 artifactId=11\n"
	if messages[1].Content != wantUser || messages[0].Content != wantPrompt {
		t.Fatalf("legacy rendering drifted:\nsystem=%q\nuser=%q", messages[0].Content, messages[1].Content)
	}
}
