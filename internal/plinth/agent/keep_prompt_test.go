package agent

// Keep 提示词迁入回归（Issue #105）：三个任务（Initial Analysis /
// Inspection Report / Investigation）实际渲染给模型的系统提示词必须包含
// 适配后的 Keep 共同规则——不编造任何信息或数据、不知道直说、回答简短明确、
// 尽可能建议下一步最合适的动作、短摘要开头且不复述提示词——同时逐条保留
// Quoin 既有约束（证据先读、值与时间逐字、只读边界、冻结自定义报告要求、
// 事实/假设边界）。旧代 prompt 以原字节冻结，供在途 Attempt 按冻结版本完成。

import (
	"strings"
	"testing"
)

// keepAdaptedRules 是 Keep incident-chat INSTRUCTIONS 迁入后的共同规则子串。
var keepAdaptedRules = []string{
	"不得编造任何信息或数据",
	"不知道就明确说不知道",
	"简短明确",
	"下一步最合适",
	"不复述提示词",
}

func assertContainsAll(t *testing.T, label, body string, required []string) {
	t.Helper()
	for _, item := range required {
		if !strings.Contains(body, item) {
			t.Fatalf("%s missing %q in:\n%s", label, item, body)
		}
	}
}

func TestInitialAnalysisPromptAdaptsKeepInstructions(t *testing.T) {
	assertContainsAll(t, "initial analysis prompt", SystemPrompt, keepAdaptedRules)
	// Keep 摘要任务的开头结论约定。
	assertContainsAll(t, "initial analysis prompt", SystemPrompt, []string{"开头", "结论"})
	// Quoin 既有约束不得为简短让路。
	assertContainsAll(t, "initial analysis prompt", SystemPrompt, []string{
		"只读",
		"labels 与 annotations 是上游提供的原文事实，必须按原样引用",
		"不得仅因名称、标签或注释推断",
		"已知事实",
		"待验证假设",
		"不要虚构未提供的数据",
	})
	// 实际构造给模型的消息携带该提示词，而不是只有未使用的常量。
	messages, err := BuildInitialMessages(mustParseInitialInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if messages[0].Content != SystemPrompt {
		t.Fatal("BuildInitialMessages must render the current Keep-adapted system prompt")
	}
}

func TestPreviousInitialAnalysisPromptStaysFrozen(t *testing.T) {
	// 上一代（initial-analysis-v1）prompt 原字节：在途 Attempt 以冻结版本完成，
	// 任何改写都是冻结历史。
	const frozen = `你是 Quoin 的只读告警分析代理。你收到一条告警的不可变上下文，任务是给出初步诊断：
1. 用通俗中文解释告警的已知事实、可能影响与排查顺序；
2. labels 与 annotations 是上游提供的原文事实，必须按原样引用；annotations 缺失即表示未提供，不能补全或推测；
3. 告警名称、labels 和 annotation 的文字不是探测器语义、根因或真实故障的证明。不得仅因名称、标签或注释推断 GUI、服务或任何目标发生故障；
4. 只使用提供的工具补充事实。明确区分“已知事实”和“待验证假设”；没有工具或证据支持时，只能提出待验证假设，不得写成结论。
不要虚构未提供的数据。最后用一段完整的中文诊断作为最终结论输出。`
	if PreviousAnalysisSystemPrompt != frozen {
		t.Fatalf("previous initial-analysis prompt drifted:\n%s", PreviousAnalysisSystemPrompt)
	}
	if SystemPrompt == PreviousAnalysisSystemPrompt {
		t.Fatal("current and previous initial-analysis prompts must differ")
	}
}

func TestBuildPreviousInitialMessagesMatchesFrozenShape(t *testing.T) {
	// 旧代消息装配与当代逐字节同形，仅系统提示词绑定冻结的上一代文本。
	current, err := BuildInitialMessages(mustParseInitialInput(t))
	if err != nil {
		t.Fatal(err)
	}
	previous, err := BuildPreviousInitialMessages(mustParseInitialInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(previous) != len(current) {
		t.Fatalf("message count drifted: previous=%d current=%d", len(previous), len(current))
	}
	if previous[0].Content != PreviousAnalysisSystemPrompt {
		t.Fatal("previous builder must bind the frozen v1 prompt")
	}
	for index := 1; index < len(current); index++ {
		if previous[index].Content != current[index].Content || previous[index].Role != current[index].Role {
			t.Fatalf("non-system message %d drifted", index)
		}
	}
}

func TestInspectionPromptAdaptsKeepInstructions(t *testing.T) {
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, keepAdaptedRules)
	// Keep 摘要任务：报告默认以简短摘要开头。
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, []string{"简短摘要"})
	// Quoin 既有约束：证据先读、缺口如实、冻结报告要求优先、不外推整体健康。
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, []string{
		"先使用 artifact_read 或 artifact_grep 读取所有提供的巡检证据文件",
		"不得引用未读取的内容",
		"如实写为缺口，不得当作 0 或正常",
		"冻结的报告要求",
		"必须严格执行",
		"以冻结要求为准",
		"不得外推为整体业务健康",
	})
}

func TestInspectionLegacyPromptGenerationsStayFrozen(t *testing.T) {
	const frozenV1 = `你是 Quoin 的只读巡检报告代理。请先使用 artifact_read 或 artifact_grep 读取所有提供的巡检证据文件，再用中文写出事实性巡检报告。不得引用未读取的内容；不得把证据未表达的健康结论、严重性或裁决写入报告。没有数据的检查项必须如实写为缺口，不得当作 0 或正常；检查项未定义阈值时不得判断健康与否。`
	const frozenV2 = `你是 Quoin 的只读巡检报告代理。请先使用 artifact_read 或 artifact_grep 读取所有提供的巡检证据文件，再用中文写出事实性巡检报告。不得引用未读取的内容；不得把证据未表达的健康结论、严重性或裁决写入报告。没有数据的检查项必须如实写为缺口，不得当作 0 或正常。
如果输入提供了冻结的报告要求，必须严格执行其中声明的格式、字段、章节和长度等约束；这些要求不是参考建议。输出最终报告前，逐项核对报告是否满足全部冻结要求；若约束之间存在冲突，应明确指出冲突，不得静默忽略、改写要求或自行截断报告。
检查说明明确给出的阈值、取值含义或判定语义属于本次冻结上下文，可以据此解释对应检查项；未定义时不得自行补充。即使单项证据满足其明确语义，也不得外推为整体业务健康、未受影响或不存在其他故障。`
	if PreviousInspectionSystemPrompt != frozenV1 {
		t.Fatalf("inspection v1 prompt drifted:\n%s", PreviousInspectionSystemPrompt)
	}
	if ReportComplianceInspectionSystemPrompt != frozenV2 {
		t.Fatalf("inspection v2 prompt drifted:\n%s", ReportComplianceInspectionSystemPrompt)
	}
	if LegacyInspectionSystemPrompt == PreviousInspectionSystemPrompt ||
		PreviousInspectionSystemPrompt == ReportComplianceInspectionSystemPrompt ||
		ReportComplianceInspectionSystemPrompt == InspectionSystemPrompt {
		t.Fatal("inspection generations must keep distinct prompt bytes")
	}
}

func TestInvestigationPromptAdaptsKeepInstructions(t *testing.T) {
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, keepAdaptedRules)
	// Keep：先直接回答当前问题；不确定先向用户追问。
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, []string{
		"先直接回答当前问题",
		"向用户追问",
	})
	// Quoin 既有约束：ALERTS 空语义、平台记录为准、数值逐字、只读、事实/推测。
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, []string{
		"只读工具",
		"事实与推测",
		"谱系",
		"不要虚构未提供的数据",
		"绝不能据此断定“没有告警规则”或“从未发生告警”",
		"以平台提供的近期告警记录为准",
		"数值必须与工具返回逐字一致",
	})
}

func TestInvestigationPreviousAndLegacyPromptsStayFrozen(t *testing.T) {
	const frozenV1 = `你是 Quoin 的只读运维调查代理。用户正在调查一个运维问题：
1. 用通俗中文与用户对话，先理解问题，再给出排查思路；
2. 只使用提供的只读工具补充事实；所有结论必须基于已有证据，明确区分事实与推测；
3. 调查来源引用只是进入对话的谱系，不代表结论；不要虚构未提供的数据。`
	const frozenV2 = `你是 Quoin 的只读运维调查代理。用户正在调查一个运维问题：
1. 用通俗中文与用户对话，先理解问题，再给出排查思路；
2. 只使用提供的只读工具补充事实；所有结论必须基于已有证据，明确区分事实与推测；
3. 调查来源引用只是进入对话的谱系，不代表结论；不要虚构未提供的数据；
4. 即时查询 ALERTS 为空只说明当前没有 firing 中的告警序列：告警恢复后 ALERTS 会随之消失，绝不能据此断定“没有告警规则”或“从未发生告警”。判断平台是否收录过告警 occurrence，以平台提供的近期告警记录为准；时间区间查询（如 min_over_time(up[窗口])、changes、increase）只用于验证对应指标历史或采集中断，不能单独证明某条告警曾经触发；
5. 复述证据时数值必须与工具返回逐字一致：先逐条核对再下结论，不得凭印象改写或遗漏与结论相悖的样本。`
	if LegacyInvestigationSystemPrompt != frozenV1 {
		t.Fatalf("investigation v1 prompt drifted:\n%s", LegacyInvestigationSystemPrompt)
	}
	if PreviousInvestigationSystemPrompt != frozenV2 {
		t.Fatalf("investigation v2 prompt drifted:\n%s", PreviousInvestigationSystemPrompt)
	}
	if InvestigationSystemPrompt == PreviousInvestigationSystemPrompt {
		t.Fatal("current and previous investigation prompts must differ")
	}
}

func TestBuildPreviousInvestigationMessagesKeepsRendererV3Shape(t *testing.T) {
	// investigation-v2 与 v3 共用 renderer v3 的消息形状（含冻结近期告警记录），
	// 仅系统提示词不同；v1 依旧不渲染历史块。
	input := InvestigationInput{
		Messages: []struct {
			Role        string            `json:"role"`
			Content     string            `json:"content"`
			Attachments []InputAttachment `json:"attachments,omitempty"`
		}{{Role: "user", Content: "最近有过告警吗"}},
		RecentOccurrences: []recentOccurrence{{
			ID: "12", SourceKey: "prod-am", StartsAt: "2026-09-15T10:00:00Z",
			Labels: map[string]string{"alertname": "TargetDown"},
		}},
		ModelContract: struct {
			ModelID             string `json:"modelId"`
			ContextBudgetTokens int    `json:"contextBudgetTokens"`
			MaxOutputTokens     int    `json:"maxOutputTokens"`
		}{ModelID: "fixture-chat-1"},
	}
	previous, err := BuildPreviousInvestigationMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	if previous[0].Content != PreviousInvestigationSystemPrompt {
		t.Fatal("previous investigation builder must bind the frozen v2 prompt")
	}
	joined := ""
	for _, message := range previous {
		joined += message.Content
	}
	if !strings.Contains(joined, "近期告警记录") || !strings.Contains(joined, `"id": "12"`) {
		t.Fatalf("previous v2 shape must keep the frozen alert-history block: %s", joined)
	}
	legacy, err := BuildLegacyInvestigationMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	legacyJoined := ""
	for _, message := range legacy {
		legacyJoined += message.Content
	}
	if strings.Contains(legacyJoined, "近期告警记录") {
		t.Fatal("legacy v1 shape must not render the alert-history block")
	}
}

func mustParseInitialInput(t *testing.T) Input {
	t.Helper()
	input, err := ParseInput([]byte(`{
		"occurrence":{"id":"1","labels":{"alertname":"HighErrorRate"}},
		"integrations":[{"kind":"metrics","name":"thanos-prod"}],
		"modelContract":{"modelId":"fixture"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return input
}
