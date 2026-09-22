package agent

// 点查配对与认识论措辞约束的实证回归（失效形状依据 investigation 会话 9 真实
// 输出：正文引用的 @探测值与原始工具返回不符——@16:00:50 引用为 1 而原始为 0、
// @16:35 引用为 1 而原始为 0、引用了从未查询过的 @16:10:50、redis_uptime 的值
// 被错配到 5 分钟前的时刻；另有「只有一类 critical」归类歧义与「互不相关」
// 无据断言无关。证据摘录见
// .artifacts/acceptance-20260921/quality-temporal-fix19-live-retest.md）：
// 三个 alerts_recent 消费路径（Initial Analysis / Investigation / Inspection）的
// 当前系统提示词必须携带通用「点查配对」约束——点查事实是 指标+完整标签+查询
// 时刻+返回值 的不可拆分配对，落笔前回读原始对应工具结果逐点核对，优先保留经
// 回读核准的必要点，不凭记忆铺列时刻或值，未实际查询过的时刻不得补造；返回值
// 为 0 不得写成已恢复，边界未确认如实写未知；没有关联证据不等于确定无关；同
// 组件/同资源归类不得说成同一种故障。约束只描述通用义务，不含单次事故的业务
// 名/真实地址/具体指标名/具体时刻或值；知识接入之前的各代 prompt 以原字节
// 冻结，不随后续代际演进。

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// probeFidelityRules 是「点查配对」约束在三个当前 prompt 中的共同关键子串。
// 健康极性保持未知：是否恢复按指标已确认的取值语义判断，不固定「0=异常/
// 1=健康」方向（error_count=0 等指标 0 即健康）。
var probeFidelityRules = []string{
	"点查事实是不可拆分的配对",
	"逐点核对",
	"优先保留经回读核准的必要点",
	"不凭记忆铺列时刻或值",
	"未实际查询过的时刻不得补造",
	"不得改写原始值",
	"按该指标已确认的取值语义判断",
	"不能仅凭数值 0 或 1 断定",
	"相反的健康方向",
	"边界未确认就如实写未知",
	"没有关联证据不等于确定无关",
	"不得据此说成同一种故障或同一问题",
}

func TestPromptsConstrainProbeFidelity(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsAll(t, label, prompt, probeFidelityRules)
	}
	// 会话路径落笔前回读原始工具结果；巡检路径回读冻结证据或原始工具结果。
	assertContainsAll(t, "initial analysis prompt", SystemPrompt, []string{
		"落笔前回读原始对应的工具结果逐点核对",
	})
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, []string{
		"落笔前回读原始对应的工具结果逐点核对",
	})
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, []string{
		"落笔前回读冻结证据或原始工具结果逐点核对",
	})
}

// TestKeptPromptsPredateProbeFidelity 在途 Attempt 的冻结 prompt 绝不随本次
// 补修演进。
func TestKeptPromptsPredateProbeFidelity(t *testing.T) {
	for label, prompt := range map[string]string{
		"kept initial analysis": KeptAnalysisSystemPrompt,
		"kept inspection":       KeptInspectionSystemPrompt,
		"kept investigation":    KeptInvestigationSystemPrompt,
	} {
		for _, rule := range probeFidelityRules {
			if strings.Contains(prompt, rule) {
				t.Fatalf("%s prompt must stay frozen before the probe-fidelity fix but mentions %q", label, rule)
			}
		}
	}
}

// TestProbeFidelityKeepsUnknownHealthPolarity 修复 fix20 引入的通用化回归：
// 健康极性未知的约束不得固定「0 不得写成恢复」——error_count=0 等指标 0 即
// 健康，固定 0 禁令与既有「逐指标语义」约束冲突。恢复与否只能按该指标已
// 确认的取值语义判断，不能仅凭数值 0 或 1。
func TestProbeFidelityKeepsUnknownHealthPolarity(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsNone(t, label, prompt, []string{
			"返回值为 0 不得写成已恢复",
			"值为 0 不得写成",
		})
		assertContainsAll(t, label, prompt, []string{
			"不能仅凭数值 0 或 1 断定",
			"相反的健康方向",
		})
	}
}

// TestProbeFidelityRulesStayGeneric 约束本身不得携带单次盲测的具体业务名、真实
// 地址、具体指标名、事故时刻或事故点查值。
func TestProbeFidelityRulesStayGeneric(t *testing.T) {
	forbidden := []string{
		"Mall", "mall", "Moyen", "Redis", "redis", "10.43.", "Middleware",
		"15:58", "16:07", "16:33", "16:00:50", "16:10:50", "16:35", "481532",
		"redis_up", "up==0",
	}
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsNone(t, label, prompt, forbidden)
	}
}

// TestProbeFidelityBlockStaysCompact 约束以单块紧凑并入，不得随迭代无限追加
// 重复句：同一关键子串在每个当前 prompt 中只出现一次。
func TestProbeFidelityBlockStaysCompact(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		for _, rule := range probeFidelityRules {
			if n := strings.Count(prompt, rule); n != 1 {
				t.Fatalf("%s must carry rule %q exactly once, got %d", label, rule, n)
			}
		}
	}
}

// TestEmpiricalAssemblyExposesProbeFidelityRule 真实 ParseInput +
// BuildInitialMessages 链路：occ3 形状的冻结输入（identifier_fidelity_prompt_test.go
// 共用 fixture）装配出的模型可见系统消息必须携带点查配对约束（装配无遗漏）。
func TestEmpiricalAssemblyExposesProbeFidelityRule(t *testing.T) {
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
	assertContainsAll(t, "initial analysis system message", messages[0].Content, probeFidelityRules)
}

// TestInvestigationAssemblyExposesProbeFidelityRule 真实
// ParseInvestigationInput + BuildInvestigationMessages 链路（失效发生的路径）：
// 系统消息必须携带点查配对约束。
func TestInvestigationAssemblyExposesProbeFidelityRule(t *testing.T) {
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
	assertContainsAll(t, "investigation system message", messages[0].Content, probeFidelityRules)
}

// probePointResultJSON 镜像 thanos_query 点查结果的真实形状：output 内的
// data.result[0].value 是 [查询求值时刻, "返回值"] 配对字节（fixture 值；不含
// 事故时刻/值）。
const probePointResultJSON = `{"success":true,"status":"success","resultType":"vector","sampleCount":1,"truncated":false,"output":"{\"status\":\"success\",\"data\":{\"resultType\":\"vector\",\"result\":[{\"metric\":{\"__name__\":\"up\",\"instance\":\"192.0.2.10:9121\",\"job\":\"fixture-cache-exporter\"},\"value\":[1789123450.5,\"0\"]}]}}"}`

// TestToolResultReplayPreservesProbeValuePairBytes 工具结果回放路径
// （ToolResultMessage）：点查返回的 [时刻, 值] 配对以原字节进入下一轮模型可见
// 消息——「落笔前回读原始对应工具结果」的对象真实存在且未被改写（数据原值
// 保留）。
func TestToolResultReplayPreservesProbeValuePairBytes(t *testing.T) {
	message := ToolResultMessage("call_p", "thanos_query", []byte(probePointResultJSON))
	if message.Role != schema.Tool || message.ToolCallID != "call_p" {
		t.Fatalf("tool result message must bind role=tool and the provider tool call id, got %q/%q", message.Role, message.ToolCallID)
	}
	for _, fragment := range []string{
		`\"value\":[1789123450.5,\"0\"]`,
		`192.0.2.10:9121`,
		`fixture-cache-exporter`,
	} {
		if !strings.Contains(message.Content, fragment) {
			t.Fatalf("tool result replay must preserve the raw probe pair bytes %q:\n%s", fragment, message.Content)
		}
	}
}
