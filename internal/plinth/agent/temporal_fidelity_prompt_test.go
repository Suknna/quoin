package agent

// 时间序与独立 occurrence 保真约束的实证回归（失效形状依据 investigation
// 会话 7 真实输出，证据摘录见
// .artifacts/acceptance-20260921/quality-temporal-fix18-evidence.md）：
//  1. 「不确定之处」段叙述「2 条 X 夹着 1 条 Y」与自己表格的 startedAt 先后矛盾
//     （真实顺序是 Y 最先，不存在夹着排列）；
//  2. 总结段「只有一轮」把两类相互独立的指标异常粗合并为单一轮次，与同一输出
//     证据节已区分的粒度矛盾。
// 三个 alerts_recent 消费路径（Initial Analysis / Investigation / Inspection）的当前
// 系统提示词必须携带「时间序与独立 occurrence 保真」通用约束：时间字段按语义区分
// （来源侧开始/平台首次见到/状态变化与恢复/采证时间），比较先后分清字段并按真实
// 时间值排序；先后/排列叙述不得描述数据中不存在的顺序或区间，正文与表格/列表不得
// 互相矛盾；没有关联证据不得把多条 occurrence 或多种独立异常合并成一个事件、一轮
// 波动或单一根因，同一资源的多条记录是多次独立 occurrence、可分别描述。约束只描述
// 通用义务，不含单次事故的业务名/真实地址/具体告警名/具体时间值；知识接入之前的
// 各代 prompt 以原字节冻结，不随后续代际演进。

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// temporalFidelityRules 是「时间序与独立 occurrence 保真」约束在三个当前 prompt
// 中的共同关键子串。
var temporalFidelityRules = []string{
	"比较先后时必须分清所引字段并按其真实时间值排序",
	"不得描述数据中不存在的顺序或区间",
	"正文叙述与表格/列表的先后不得互相矛盾",
	"没有关联证据时不得合并成一个事件、一轮波动或单一根因",
	"同一资源的多条记录是多次独立 occurrence",
}

func TestPromptsConstrainTemporalOccurrenceFidelity(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsAll(t, label, prompt, temporalFidelityRules)
	}
	// 时间字段按语义区分：告警分析/调查点名告警上下文的字段语义锚点（开始时间
	// 与首次见到时间是不同时刻）；巡检额外覆盖检查项采证时间与其不混用。
	assertContainsAll(t, "initial analysis prompt", SystemPrompt, []string{
		"时间字段各有语义", "startsAt", "firstSeenAt",
	})
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, []string{
		"时间字段各有语义", "startsAt", "firstSeenAt",
	})
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, []string{
		"时间字段各有语义", "采证时间",
	})
}

// TestKeptPromptsPredateTemporalFidelity 在途 Attempt 的冻结 prompt 绝不随本次
// 补修演进。
func TestKeptPromptsPredateTemporalFidelity(t *testing.T) {
	for label, prompt := range map[string]string{
		"kept initial analysis": KeptAnalysisSystemPrompt,
		"kept inspection":       KeptInspectionSystemPrompt,
		"kept investigation":    KeptInvestigationSystemPrompt,
	} {
		for _, rule := range temporalFidelityRules {
			if strings.Contains(prompt, rule) {
				t.Fatalf("%s prompt must stay frozen before the temporal-fidelity fix but mentions %q", label, rule)
			}
		}
		if strings.Contains(prompt, "时间字段各有语义") {
			t.Fatalf("%s prompt must stay frozen before the temporal-fidelity fix but mentions field-semantics rule", label)
		}
	}
}

// TestTemporalFidelityRulesStayGeneric 约束本身不得携带单次盲测的具体业务名、
// 真实地址、具体告警名或事故时间值。
func TestTemporalFidelityRulesStayGeneric(t *testing.T) {
	forbidden := []string{
		"Mall", "mall", "Moyen", "Redis", "redis", "10.43.", "Middleware",
		"15:58", "16:07", "16:33",
	}
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsNone(t, label, prompt, forbidden)
	}
}

// temporalAlertsRecentFixtureJSON 镜像 alerts_recent 真实结果形状：返回顺序为新在
// 前（与平台实际投递一致），三条 occurrence 中最早的是与另两条不同标题的告警——
// 与会话 7 失效形状同构，但业务名/地址/时间全部为通用 fixture 值。
const temporalAlertsRecentFixtureJSON = `{"alerts":[{"occurrenceId":"9","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T10:23:19.123Z","state":"Resolved","title":"FixtureShopCacheUnavailable","viewKeys":["fixture-shop"]},{"occurrenceId":"8","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T09:47:52.123Z","state":"Resolved","title":"FixtureShopCacheUnavailable","viewKeys":["fixture-shop"]},{"occurrenceId":"7","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T09:11:07.123Z","state":"Resolved","title":"FixtureShopMiddlewareTargetDown","viewKeys":["fixture-shop"]}],"total":3,"window":"2026-09-20T10:30:00Z/2026-09-21T10:30:00Z"}`

// TestEmpiricalAssemblyExposesTemporalFidelityRule 真实 ParseInput +
// BuildInitialMessages 链路：occ3 形状的冻结输入（identifier_fidelity_prompt_test.go
// 共用 fixture）装配出的模型可见系统消息必须携带时间序与独立 occurrence 约束
// （装配无遗漏），用户消息必须携带冻结的 firstSeenAt 时间字段原文。
func TestEmpiricalAssemblyExposesTemporalFidelityRule(t *testing.T) {
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
	assertContainsAll(t, "initial analysis system message", messages[0].Content, temporalFidelityRules)
	user := messages[2].Content
	for _, fragment := range []string{"firstSeenAt", "FixtureShopCacheUnavailable"} {
		if !strings.Contains(user, fragment) {
			t.Fatalf("model-visible user message lost frozen occurrence context %q:\n%s", fragment, user)
		}
	}
}

// investigationTemporalShapedCanonical 镜像真实 investigation_v1 冻结输入形状
// （会话 7 失效路径）：recentOccurrences 按平台投递顺序（新在前）携带三条记录，
// 最早一条与另两条标题不同。
const investigationTemporalShapedCanonical = `{
	"messages": [{"role": "user", "content": "过去24小时有哪些告警，分别是什么时候开始的？"}],
	"sources": [],
	"recentOccurrences": [
		{"id": "9", "sourceKey": "fixture-am", "startsAt": "2026-09-21T10:23:19.123Z", "labels": {"alertname": "FixtureShopCacheUnavailable", "severity": "critical"}},
		{"id": "8", "sourceKey": "fixture-am", "startsAt": "2026-09-21T09:47:52.123Z", "labels": {"alertname": "FixtureShopCacheUnavailable", "severity": "critical"}},
		{"id": "7", "sourceKey": "fixture-am", "startsAt": "2026-09-21T09:11:07.123Z", "labels": {"alertname": "FixtureShopMiddlewareTargetDown", "severity": "critical"}}
	],
	"modelContract": {"modelId": "fixture-chat-1", "contextBudgetTokens": 32000, "maxOutputTokens": 4096}
}`

// TestInvestigationAssemblyExposesTemporalFidelityRule 真实
// ParseInvestigationInput + BuildInvestigationMessages 链路（失效发生的路径）：
// 系统消息必须携带约束，近期告警记录块必须携带 startsAt 原字节——正确的先后
// 事实在模型上下文中。
func TestInvestigationAssemblyExposesTemporalFidelityRule(t *testing.T) {
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
	assertContainsAll(t, "investigation system message", messages[0].Content, temporalFidelityRules)
	history := messages[1].Content
	for _, fragment := range []string{
		`"startsAt": "2026-09-21T09:11:07.123Z"`,
		`"startsAt": "2026-09-21T09:47:52.123Z"`,
		`"startsAt": "2026-09-21T10:23:19.123Z"`,
	} {
		if !strings.Contains(history, fragment) {
			t.Fatalf("model-visible history block lost the frozen occurrence time bytes %q:\n%s", fragment, history)
		}
	}
}

// TestToolResultReplayCarriesExactOccurrenceTimes 工具结果回放路径
// （ToolResultMessage）：alerts_recent 返回的 per-occurrence startedAt 以原字节
// 进入下一轮模型可见消息——叙述先后时正确的时间事实确实在其上下文中。
func TestToolResultReplayCarriesExactOccurrenceTimes(t *testing.T) {
	message := ToolResultMessage("call_1", "alerts_recent", []byte(temporalAlertsRecentFixtureJSON))
	if message.Role != schema.Tool || message.ToolCallID != "call_1" {
		t.Fatalf("tool result message must bind role=tool and the provider tool call id, got %q/%q", message.Role, message.ToolCallID)
	}
	for _, fragment := range []string{
		`"startedAt":"2026-09-21T09:11:07.123Z"`,
		`"startedAt":"2026-09-21T09:47:52.123Z"`,
		`"startedAt":"2026-09-21T10:23:19.123Z"`,
	} {
		if !strings.Contains(message.Content, fragment) {
			t.Fatalf("tool result replay must carry the exact occurrence time bytes %q:\n%s", fragment, message.Content)
		}
	}
}
