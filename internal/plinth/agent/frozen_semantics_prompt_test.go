package agent

// 盲测补修 3 实证回归（Run2 v2 重分析失效后建立）：不再只对 prompt 常量做
// 子串检查，而是把 Run2 形状的冻结 canonical 输入（完整检查说明/指标单位/
// 单检查项/证据定位）走真实 ParseInspectionInput + BuildInspectionMessages
// 路径，断言模型可见消息确实携带完整冻结语义（装配无遗漏），同时断言系统
// 提示词携带"给定取值语义按原词正向复述"的正向陈述义务——v2 的失效不是
// 部署或装配问题（prompt_digest 与冻结说明均已实证到位），而是只有禁令没
// 有正向义务导致模型回避给定语义、自造混合表述并把结果重新归到采集端点。
// fixture 保持通用：不含具体业务名、真实地址或具体指标语义答案。

import (
	"encoding/json"
	"strings"
	"testing"
)

// run2ShapedCanonical mirrors the real inspection_analysis_v1 canonical input
// shape observed for the Run2 re-analysis attempt (single promql_instant
// check, complete frozen check semantics, no per-run override).
const run2ShapedCanonical = `{
	"schemaKind": "inspection_analysis_v1",
	"attemptId": 56,
	"inspectionRunId": 2,
	"artifactIds": [7],
	"evidenceIds": [93],
	"modelContract": {"modelId": "fixture-chat-1"},
	"plan": {
		"key": "middleware-paas-demo",
		"params": {"expression": "middleware_up{system_id=\"demo\"} or database_up{system_id=\"demo\"}"},
		"scope": {"kind": "integration"},
		"checkDescription": "检查本次采样中两个中间件是否可由各自 exporter 连接。预期各一条序列，1 表示连接成功，0 表示连接失败，缺失表示证据不足。不能等同于业务交易成功。",
		"metricUnit": "1=连接成功，0=连接失败",
		"reportInstructions": "分别列出两个中间件的实际值、证据时间及结论，不将历史恢复与当前状态混淆。"
	},
	"checks": [
		{
			"checkKey": "promql_instant", "displayName": "PromQL 即时查询", "status": "ok",
			"evidenceId": 93, "artifactId": 7,
			"expression": "middleware_up{system_id=\"demo\"} or database_up{system_id=\"demo\"}",
			"observedAt": "2026-09-21T16:27:58.81678522Z"
		}
	]
}`

// TestEmpiricalAssemblyExposesFrozenSemantics 装配无遗漏：完整冻结检查说明、
// 指标单位与逐检查项事实必须逐字进入模型可见的用户消息。
func TestEmpiricalAssemblyExposesFrozenSemantics(t *testing.T) {
	input, err := ParseInspectionInput([]byte(run2ShapedCanonical))
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInspectionMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != InspectionSystemPrompt {
		t.Fatalf("inspection assembly must be [current system prompt, user message], got %d messages", len(messages))
	}
	user := messages[1].Content
	for _, fragment := range []string{
		"检查说明：检查本次采样中两个中间件是否可由各自 exporter 连接。预期各一条序列，1 表示连接成功，0 表示连接失败，缺失表示证据不足。不能等同于业务交易成功。",
		"指标单位：1=连接成功，0=连接失败",
		"evidenceId=93 artifactId=7",
		"表达式=middleware_up",
		"observedAt=2026-09-21T16:27:58.81678522Z",
		"本次报告要求",
	} {
		if !strings.Contains(user, fragment) {
			t.Fatalf("model-visible user message lost frozen semantics %q:\n%s", fragment, user)
		}
	}
}

// positiveRestatementRules 是"给定取值语义按原词正向复述"义务的关键子串。
var positiveRestatementRules = []string{
	"原样正向陈述",
	"用其原词",
	"不得在其前后插入采集类字样形成混合表述",
	"既不是降格也不是升格",
	"只针对超出给定语义的改写",
	"改归他处",
}

// TestInspectionPromptRequiresPositiveRestatement 只有禁令会让模型回避给定
// 语义（Run2 v2 的"采集连接成功"混合表述与"不代表服务端本体状态"改写）；
// 当前提示词必须携带正向陈述义务，且与升降级禁令的边界写明。
func TestInspectionPromptRequiresPositiveRestatement(t *testing.T) {
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, positiveRestatementRules)
}

// TestKeptInspectionPromptPredatePositiveRestatement 在途 Attempt 的冻结
// prompt 绝不随本次补修演进。
func TestKeptInspectionPromptPredatePositiveRestatement(t *testing.T) {
	for label, prompt := range map[string]string{
		"kept inspection": KeptInspectionSystemPrompt,
	} {
		for _, rule := range positiveRestatementRules {
			if strings.Contains(prompt, rule) {
				t.Fatalf("%s prompt must stay frozen before the positive-restatement fix but mentions %q", label, rule)
			}
		}
	}
}

// TestPositiveRestatementRuleStaysGeneric 正向陈述义务本身也不得携带单次
// 盲测的具体指标/业务/地址真值。
func TestPositiveRestatementRuleStaysGeneric(t *testing.T) {
	assertContainsNone(t, "inspection prompt", InspectionSystemPrompt, []string{
		"Redis", "redis", "mysql", "MySQL", "10.43.", "mall", "失配", "selector", "选择器", "_up",
	})
}

// TestRun2ShapedFixtureIsWellFormed 防止 fixture 自身漂移：与真实解析器
// 的结构约定（plan 三态、检查项字段）保持一致。
func TestRun2ShapedFixtureIsWellFormed(t *testing.T) {
	var probe struct {
		Plan   *json.RawMessage  `json:"plan"`
		Checks []json.RawMessage `json:"checks"`
	}
	if err := json.Unmarshal([]byte(run2ShapedCanonical), &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Plan == nil || len(probe.Checks) != 1 {
		t.Fatal("run2-shaped fixture must keep the frozen plan and single-check shape")
	}
	if _, present := (&InspectionInput{ReportInstructionsOverride: nil}).EffectiveReportInstructions(); present {
		t.Fatal("sanity: absent override must not count as present")
	}
}
