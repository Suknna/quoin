package agent

// 指标查询时间语义分层约束的实证回归（失效形状依据 investigation 会话 10 真实
// 输出的精度残余：把子查询步进时刻 max_over_time(timestamp(...)) 的返回值写成
// 「最晚一次 down 采样」的精确采样时刻，并把近似区间端点写成精确恢复时刻；
// 另有以自造 p=HHMM 人工标签充当时间证据、offset 锚点未按评估时刻减 offset
// 理解的风险。证据见
// .artifacts/acceptance-20260921/quality-temporal-fix21-live-retest.md）：
// 三个 alerts_recent 消费路径（Initial Analysis / Investigation / Inspection）的
// 当前系统提示词的**既有时间字段段**必须扩展携带通用「指标查询时间分层」语义——
// 查询评估时刻、子查询步进时刻与原始采样时刻含义不同，只有来源明确给出原始
// 样本 timestamp 的才能称为实际采样时刻；offset 探测锚点按评估时刻减 offset
// 换算理解，自造人工时刻标签不构成时间证据；近似区间只可近似表述，不得写成
// 精确的恢复或复现时刻。按「复用现有时间字段段」的整合要求，新语义并入既有
// 段落，不新增独立重复块（有紧凑性测试钉住「时间字段各有语义」仍只出现一次）。
// 约束只描述通用义务，不含单次事故的业务名/真实地址/具体指标名/具体时刻值；
// 知识接入之前的各代 prompt 以原字节冻结，不随后续代际演进。

import (
	"strings"
	"testing"
)

// queryTimeSemanticsRules 是「指标查询时间分层」约束在三个当前 prompt 中的
// 共同关键子串。
var queryTimeSemanticsRules = []string{
	"查询评估时刻、子查询步进时刻与原始采样时刻",
	"只有来源明确给出原始样本 timestamp 的才能称为实际采样时刻",
	"评估或步进时刻及其回看窗口不得写成采样时刻",
	"offset 探测的锚点按评估时刻减 offset 换算理解",
	"自造的人工时刻标签不构成时间证据",
	"近似区间只可近似表述，不得写成精确的恢复或复现时刻",
}

func TestPromptsConstrainQueryTimeSemantics(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsAll(t, label, prompt, queryTimeSemanticsRules)
	}
}

// TestQueryTimeSemanticsIntegratedInTimeFieldBlock 新语义并入既有时间字段段，
// 不得另加冗长重复块：「时间字段各有语义」在每个当前 prompt 中仍只出现一次。
func TestQueryTimeSemanticsIntegratedInTimeFieldBlock(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		if n := strings.Count(prompt, "时间字段各有语义"); n != 1 {
			t.Fatalf("%s must carry the time-field segment exactly once, got %d", label, n)
		}
	}
}

// TestKeptPromptsPredateQueryTimeSemantics 在途 Attempt 的冻结 prompt 绝不随
// 本次补修演进。
func TestKeptPromptsPredateQueryTimeSemantics(t *testing.T) {
	for label, prompt := range map[string]string{
		"kept initial analysis": KeptAnalysisSystemPrompt,
		"kept inspection":       KeptInspectionSystemPrompt,
		"kept investigation":    KeptInvestigationSystemPrompt,
	} {
		for _, rule := range queryTimeSemanticsRules {
			if strings.Contains(prompt, rule) {
				t.Fatalf("%s prompt must stay frozen before the query-time-semantics fix but mentions %q", label, rule)
			}
		}
	}
}

// TestQueryTimeSemanticsRulesStayGeneric 约束本身不得携带单次盲测的具体业务名、
// 真实地址、具体指标名或事故时刻值。
func TestQueryTimeSemanticsRulesStayGeneric(t *testing.T) {
	forbidden := []string{
		"Mall", "mall", "Moyen", "Redis", "redis", "10.43.", "Middleware",
		"15:58", "16:07", "16:33", "16:36:30", "16:02:30", "15:57:45", "16:00:50",
	}
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsNone(t, label, prompt, forbidden)
	}
}

// TestEmpiricalAssemblyExposesQueryTimeSemanticsRule 真实 ParseInput +
// BuildInitialMessages 链路：occ3 形状的冻结输入（identifier_fidelity_prompt_test.go
// 共用 fixture）装配出的模型可见系统消息必须携带时间分层约束（装配无遗漏）。
func TestEmpiricalAssemblyExposesQueryTimeSemanticsRule(t *testing.T) {
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
	assertContainsAll(t, "initial analysis system message", messages[0].Content, queryTimeSemanticsRules)
}

// TestInvestigationAssemblyExposesQueryTimeSemanticsRule 真实
// ParseInvestigationInput + BuildInvestigationMessages 链路（失效发生的路径）：
// 系统消息必须携带时间分层约束。
func TestInvestigationAssemblyExposesQueryTimeSemanticsRule(t *testing.T) {
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
	assertContainsAll(t, "investigation system message", messages[0].Content, queryTimeSemanticsRules)
}

// stepBoundaryResultJSON 镜像 timestamp 边界查询的真实形状：value 是
// [评估时刻, "步进时刻"]——第二个元素是子查询步进时刻（带回看窗口的上界），
// 不是原始采样时刻。
const stepBoundaryResultJSON = `{"success":true,"status":"success","resultType":"vector","sampleCount":1,"truncated":false,"output":"{\"status\":\"success\",\"data\":{\"resultType\":\"vector\",\"result\":[{\"metric\":{\"__name__\":\"up\",\"instance\":\"192.0.2.10:9121\",\"job\":\"fixture-cache-exporter\"},\"value\":[1789126900.5,\"1789123500\"]}]}}"}`

// TestToolResultReplayDistinguishesStepFromSampleBytes 工具结果回放路径：
// 边界查询的 [评估时刻, 步进时刻] 配对原字节进入下一轮模型可见消息——区分
// 评估/步进与采样时刻的判断材料在模型上下文中原样存在（数据原值保留）。
func TestToolResultReplayDistinguishesStepFromSampleBytes(t *testing.T) {
	message := ToolResultMessage("call_s", "thanos_query", []byte(stepBoundaryResultJSON))
	for _, fragment := range []string{
		`\"value\":[1789126900.5,\"1789123500\"]`,
		`192.0.2.10:9121`,
	} {
		if !strings.Contains(message.Content, fragment) {
			t.Fatalf("tool result replay must preserve the raw boundary-pair bytes %q:\n%s", fragment, message.Content)
		}
	}
}
