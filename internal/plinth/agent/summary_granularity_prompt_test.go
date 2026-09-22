package agent

// 总结粒度正向约束的实证回归（失效形状依据 investigation 会话 8 真实输出：
// 正文表格与叙述已正确区分三条独立 occurrence，总结段却用「一次…采集中断…
// 共 3 条 critical」「唯一遗留的不确定」总括覆盖独立项，并以仅查过的 up 指标
// 历史代表未查询的 redis 侧告警；证据摘录见
// .artifacts/acceptance-20260921/quality-temporal-fix18-live-retest.md）：
// 三个 alerts_recent 消费路径（Initial Analysis / Investigation / Inspection）的
// 当前系统提示词必须携带「总结粒度」通用正向要求——摘要/总结/结论段保留正文
// 已区分的逐异常对象/指标语义与证据限制；单一指标的历史或区间结果只解释该
// 指标自身，不得用来代表其他告警或其他指标；涉及未查询的指标要么先核实要么
// 明确写明未验证；禁止用一次事件/同一问题/唯一遗留等总括表述覆盖正文已分别
// 列出的独立项，总结粒度不得粗于正文证据已支持的粒度。约束只描述通用义务，
// 不含单次事故的业务名/真实地址/具体告警名/具体指标名/具体时间值；知识接入
// 之前的各代 prompt 以原字节冻结，不随后续代际演进。

import (
	"strings"
	"testing"
)

// summaryGranularityRules 是「总结粒度」约束在三个当前 prompt 中的共同关键子串。
var summaryGranularityRules = []string{
	"必须保留正文已区分的逐异常粒度",
	"只解释该指标自身",
	"不得用来代表其他告警",
	"明确写明未验证",
	"禁止用一次事件、同一问题、唯一遗留等总括表述覆盖正文已分别列出的独立项",
	"总结粒度不得粗于正文证据已支持的粒度",
}

func TestPromptsConstrainSummaryGranularity(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsAll(t, label, prompt, summaryGranularityRules)
	}
	// 会话路径的正向动作要求：要么先调用对应工具核实，要么明确写明未验证。
	assertContainsAll(t, "initial analysis prompt", SystemPrompt, []string{
		"要么先调用对应工具核实，要么明确写明未验证",
	})
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, []string{
		"要么先调用对应工具核实，要么明确写明未验证",
	})
	// 巡检路径对应取证口径：尚未取证的检查项如实写明未验证。
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, []string{
		"要么先取证核实，要么明确写明未验证",
	})
}

// TestKeptPromptsPredateSummaryGranularity 在途 Attempt 的冻结 prompt 绝不随
// 本次补修演进。
func TestKeptPromptsPredateSummaryGranularity(t *testing.T) {
	for label, prompt := range map[string]string{
		"kept initial analysis": KeptAnalysisSystemPrompt,
		"kept inspection":       KeptInspectionSystemPrompt,
		"kept investigation":    KeptInvestigationSystemPrompt,
	} {
		for _, rule := range summaryGranularityRules {
			if strings.Contains(prompt, rule) {
				t.Fatalf("%s prompt must stay frozen before the summary-granularity fix but mentions %q", label, rule)
			}
		}
	}
}

// TestSummaryGranularityRulesStayGeneric 约束本身不得携带单次盲测的具体业务名、
// 真实地址、具体告警名、具体指标名或事故时间值。
func TestSummaryGranularityRulesStayGeneric(t *testing.T) {
	forbidden := []string{
		"Mall", "mall", "Moyen", "Redis", "redis", "10.43.", "Middleware",
		"15:58", "16:07", "16:33", "up==0", "redis_up",
	}
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsNone(t, label, prompt, forbidden)
	}
}

// TestEmpiricalAssemblyExposesSummaryGranularityRule 真实 ParseInput +
// BuildInitialMessages 链路：occ3 形状的冻结输入（identifier_fidelity_prompt_test.go
// 共用 fixture）装配出的模型可见系统消息必须携带总结粒度约束（装配无遗漏）。
func TestEmpiricalAssemblyExposesSummaryGranularityRule(t *testing.T) {
	input, err := ParseInput([]byte(occ3ShapedCanonical))
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInitialMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Content != SystemPrompt {
		t.Fatalf("initial analysis assembly must be [system prompt, scope guidance, user message], got %d messages", len(messages))
	}
	assertContainsAll(t, "initial analysis system message", messages[0].Content, summaryGranularityRules)
}

// TestInvestigationAssemblyExposesSummaryGranularityRule 真实
// ParseInvestigationInput + BuildInvestigationMessages 链路（失效发生的路径）：
// 系统消息必须携带总结粒度约束。
func TestInvestigationAssemblyExposesSummaryGranularityRule(t *testing.T) {
	input, err := ParseInvestigationInput([]byte(investigationTemporalShapedCanonical))
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInvestigationMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages=%d want system, history, user", len(messages))
	}
	assertContainsAll(t, "investigation system message", messages[0].Content, summaryGranularityRules)
}
