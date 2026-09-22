package agent

// 盲测补修 2 回归：三个任务（Initial Analysis / Inspection Report /
// Investigation）的当前系统提示词必须携带“逐指标语义”的通用解读约束——
// 指标含义以其自身定义与随证据/冻结检查说明给出的语义为准（冻结说明优先
// 于模型推断）；instance 等地址标签只标明采集来源，不定义指标含义，不得
// 因地址指向采集端点（如 exporter 地址）就把指标描述的对象与采集端点混同
// （例如把已给定“exporter 连接被监控对象成功”语义的结果改述为“采集/抓取
// 成功”）；给定语义层级不得降格为仅采集或抓取成功，采集或抓取成功也不得
// 升格为被监控对象状态良好或交易正常。约束只描述通用语义：具体指标名、
// 业务名、真实地址、具体根因结论都不得写进提示词；知识接入之前的各代
// prompt 以原字节冻结，不随后续代际演进。

import (
	"strings"
	"testing"
)

// metricSemanticsRules 是逐指标语义约束在三个当前 prompt 中的共同关键子串。
var metricSemanticsRules = []string{
	"只标明采集来源",
	"不定义指标含义",
	"把指标描述的对象与采集端点混同",
	"不得降格改述为仅采集或抓取成功",
	"不得升格为被监控对象状态良好",
}

func TestPromptsConstrainPerMetricSemantics(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsAll(t, label, prompt, metricSemanticsRules)
	}
}

// TestInspectionPromptPrefersFrozenDescriptions 巡检的逐检查项语义以冻结
// 检查说明为最高优先级，且健康外推禁令覆盖交易正常。
func TestInspectionPromptPrefersFrozenDescriptions(t *testing.T) {
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, []string{
		"且优先于你自己的推断",
		"逐检查项按指标自身含义与冻结说明解读",
		"交易正常",
	})
}

// TestMetricSemanticsRulesStayGeneric 禁止把单次盲测的真值（具体指标名、
// 业务名、真实地址段、具体根因结论）写进提示词：约束必须保持可复用的
// 通用语义。
func TestMetricSemanticsRulesStayGeneric(t *testing.T) {
	forbidden := []string{"Redis", "redis", "mysql", "MySQL", "10.43.", "mall", "失配", "selector", "选择器", "_up"}
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsNone(t, label, prompt, forbidden)
	}
}

// TestKeptPromptsPredateMetricSemantics 在途 Attempt 的冻结 prompt 绝不随
// 本次补修演进。
func TestKeptPromptsPredateMetricSemantics(t *testing.T) {
	for label, prompt := range map[string]string{
		"kept initial analysis": KeptAnalysisSystemPrompt,
		"kept inspection":       KeptInspectionSystemPrompt,
		"kept investigation":    KeptInvestigationSystemPrompt,
	} {
		for _, rule := range metricSemanticsRules {
			if strings.Contains(prompt, rule) {
				t.Fatalf("%s prompt must stay frozen before the metric-semantics fix but mentions %q", label, rule)
			}
		}
	}
}

// TestEndpointSemanticsBoundaryIntact 本次补修不得挤掉补修 1 的端点边界
// 约束（地址≠主机、连接拒绝≠进程停止等仍在当前提示词中）。
func TestEndpointSemanticsBoundaryIntact(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsAll(t, label, prompt, endpointSemanticsRules)
	}
}
