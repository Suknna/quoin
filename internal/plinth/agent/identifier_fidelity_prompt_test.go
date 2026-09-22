package agent

// 盲测补修 4 实证回归（occ3 analysis2 最终诊断段标识符漂移后建立）：三个
// alerts_recent 消费路径（Initial Analysis / Investigation / Inspection）的当前
// 系统提示词必须携带「标识符逐字」约束——工具结果与告警上下文中已出现过的标识符
// （告警名称、其他告警的标题、指标名、标签值、来源名）复述或再次引用时必须与
// 原文逐字一致，最终结论/收尾段再次引用前先回查原文，不得凭印象改写、缩略、拼接
// 或另造近似名称。失效形态：模型在同一篇报告的证据段正确引用 alerts_recent 返回的
// 关联告警标题，又在最终诊断段把它改写成未在任何输入中出现过的近似拼写。约束只
// 描述通用义务，不含单次事故的业务名/真实地址/具体告警名；知识接入之前的各代
// prompt 以原字节冻结，不随后续代际演进。

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// identifierFidelityRules 是「标识符逐字」约束在三个当前 prompt 中的共同关键子串。
var identifierFidelityRules = []string{
	"告警名称、其他告警的标题、指标名、标签值、来源名",
	"先回查原文再落笔",
	"不得凭印象改写、缩略、拼接或另造近似的名称",
}

func TestPromptsConstrainIdentifierFidelity(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsAll(t, label, prompt, identifierFidelityRules)
	}
	// 会话/报告两个上下文各自的逐字锚点。
	assertContainsAll(t, "initial analysis prompt", SystemPrompt, []string{
		"数值与标识符必须与工具返回逐字一致",
	})
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, []string{
		"数值与标识符必须与工具返回逐字一致",
	})
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, []string{
		"与工具返回及冻结证据原文逐字一致",
	})
}

// TestKeptPromptsPredateIdentifierFidelity 在途 Attempt 的冻结 prompt 绝不随本次
// 补修演进。
func TestKeptPromptsPredateIdentifierFidelity(t *testing.T) {
	for label, prompt := range map[string]string{
		"kept initial analysis": KeptAnalysisSystemPrompt,
		"kept inspection":       KeptInspectionSystemPrompt,
		"kept investigation":    KeptInvestigationSystemPrompt,
	} {
		for _, rule := range identifierFidelityRules {
			if strings.Contains(prompt, rule) {
				t.Fatalf("%s prompt must stay frozen before the identifier-fidelity fix but mentions %q", label, rule)
			}
		}
	}
}

// TestIdentifierFidelityRulesStayGeneric 约束本身不得携带单次盲测的具体业务名、
// 真实地址或具体告警名。
func TestIdentifierFidelityRulesStayGeneric(t *testing.T) {
	assertContainsNone(t, "initial analysis prompt", SystemPrompt, []string{
		"Mall", "mall", "Moyen", "Redis", "redis", "10.43.", "Middleware",
	})
	assertContainsNone(t, "inspection prompt", InspectionSystemPrompt, []string{
		"Mall", "mall", "Moyen", "Redis", "redis", "10.43.", "Middleware",
	})
	assertContainsNone(t, "investigation prompt", InvestigationSystemPrompt, []string{
		"Mall", "mall", "Moyen", "Redis", "redis", "10.43.", "Middleware",
	})
}

// occ3ShapedCanonical 镜像真实 initial_analysis_v1 冻结输入形状（来源：
// occ3 分析 attempt 的输入结构 + db-snapshot 中 alerts_recent 冻结目录条目），
// 业务名/地址全部替换为通用 fixture 值。
const occ3ShapedCanonical = `{
	"occurrence": {
		"id": "3",
		"state": "Resolved",
		"firstSeenAt": "2026-09-21T16:07:14.883Z",
		"lastStateChangeAt": "2026-09-21T16:12:14.884Z",
		"resolvedAt": "2026-09-21T16:12:14.884Z",
		"labels": {
			"alertname": "FixtureShopCacheUnavailable",
			"instance": "192.0.2.10:9121",
			"job": "fixture-cache-exporter",
			"severity": "critical",
			"system_id": "fixture-shop"
		},
		"annotations": {
			"description": "cache_up=0 observed by exporter; the service itself may still be healthy",
			"summary": "fixture-shop cache not serving commands"
		}
	},
	"integrations": [{"kind": "metrics", "name": "fixture-shop-prometheus"}],
	"modelContract": {"modelId": "fixture-chat-1", "contextBudgetTokens": 32000, "maxOutputTokens": 4096},
	"toolCatalog": {
		"tools": [
			{
				"name": "alerts_recent",
				"version": "1",
				"executionMode": "quoin_routed",
				"failureMode": "return_to_model",
				"resultSchemaKind": "alerts_recent_result_v1",
				"description": "查询 Quoin 告警库中的近期告警（归一化语义）：可按业务视图、最低 severity、时间窗过滤，返回 id/severity/title/state/startsAt/labels 摘要。用于分析时获取相关告警上下文。",
				"parameters": {
					"type": "object",
					"properties": {
						"hours": {"type": "number", "minimum": 1, "maximum": 168, "description": "时间窗（小时），按告警 startsAt 回看；缺省 24，上限 168。"},
						"limit": {"type": "number", "minimum": 1, "maximum": 50, "description": "返回条数上限；缺省 10，上限 50。"}
					}
				}
			}
		]
	}
}`

// alertsRecentFixtureJSON 镜像 alerts_recent 真实结果形状（db-snapshot attempt 51
// tool_calls id=158），含本 fixture 世界的“关联告警标题”——模型复述的就是这个
// 字段的字节。
const alertsRecentFixtureJSON = `{"alerts":[{"occurrenceId":"4","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T16:33:48.339Z","state":"Resolved","title":"FixtureShopCacheUnavailable","viewKeys":["fixture-shop"]},{"occurrenceId":"2","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T15:58:33.339Z","state":"Resolved","title":"FixtureShopMiddlewareTargetDown","viewKeys":["fixture-shop"]}],"total":2,"window":"2026-09-20T17:44:00Z/2026-09-21T17:44:00Z"}`

// driftedFinalAnswerFixture 镜像失效报告形状：证据段正确引用工具返回的关联告警
// 标题，最终诊断段把它改写成未出现在任何输入中的近似拼写（前缀段被替换）。
const driftedFinalAnswerFixture = "## 结论\n\n该告警已恢复。近 8 小时内 count_over_time((up==0)[8h:1m]) = 5，时间范围 15:58:00Z–16:02:00Z，与告警列表中的 #2 FixtureShopMiddlewareTargetDown（15:58:33Z，同一实例，已 Resolved）时间吻合。\n\n**最终诊断**：这是一条已恢复的 critical 告警，并伴随一段更早的抓取目标不可达（对应 FixienMiddlewareTargetDown 告警）。建议按只读方式核对端点配置。"

// TestEmpiricalAssemblyExposesIdentifierFidelityRule 真实 ParseInput +
// BuildInitialMessages 链路：occ3 形状的冻结输入装配出的模型可见系统消息必须携带
// 标识符逐字约束（装配无遗漏），用户消息必须携带冻结的告警上下文。
func TestEmpiricalAssemblyExposesIdentifierFidelityRule(t *testing.T) {
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
	assertContainsAll(t, "initial analysis system message", messages[0].Content, identifierFidelityRules)
	user := messages[2].Content
	for _, fragment := range []string{
		"FixtureShopCacheUnavailable",
		"请分析以下告警",
	} {
		if !strings.Contains(user, fragment) {
			t.Fatalf("model-visible user message lost frozen occurrence context %q:\n%s", fragment, user)
		}
	}
}

// TestToolResultReplayCarriesExactIdentifierBytes 工具结果回放路径
// （ToolResultMessage）：alerts_recent 返回的关联告警标题以原字节进入下一轮模型
// 可见消息——改写发生时正确拼写确实在模型上下文中。
func TestToolResultReplayCarriesExactIdentifierBytes(t *testing.T) {
	message := ToolResultMessage("call_1", "alerts_recent", []byte(alertsRecentFixtureJSON))
	if message.Role != schema.Tool || message.ToolCallID != "call_1" {
		t.Fatalf("tool result message must bind role=tool and the provider tool call id, got %q/%q", message.Role, message.ToolCallID)
	}
	if !strings.Contains(message.Content, `"title":"FixtureShopMiddlewareTargetDown"`) {
		t.Fatalf("tool result replay must carry the exact identifier bytes:\n%s", message.Content)
	}
}
