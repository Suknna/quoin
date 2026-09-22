package agent

// 首发实机盲测补修回归：三个任务（Initial Analysis / Inspection Report /
// Investigation）的当前系统提示词必须携带“地址与连接错误”的通用解读约束——
// 指标标签/错误信息中的地址只是抓取或连接的目标地址（可能是服务虚拟地址、
// 负载均衡、代理或容器地址），未经确认不得当作主机、不得建议登录该地址执行
// 主机级命令；connection refused 只证明该地址:端口当时未接受连接，不能据此
// 断定进程已停止；端点类型/服务后端（Service Endpoints）/路由/进程状态列为
// 需分别验证的分支，不预设是哪一个；下一步建议优先只读核实，无证据不建议
// 重启等变更操作。约束只描述通用语义：任何具体故障真值（业务名、真实地址、
// 具体根因结论、精确 selector）都不得写进提示词；知识接入之前的各代 prompt
// 以原字节冻结，不随后续代际演进。

import (
	"strings"
	"testing"
)

// endpointSemanticsRules 是地址/连接错误解读约束在三个当前 prompt 中的共同
// 关键子串。
var endpointSemanticsRules = []string{
	"服务虚拟地址",
	"负载均衡",
	"代理或容器地址",
	"只证明该地址:端口当时未接受连接",
	"据此断定进程已停止",
	"端点类型",
	"Service Endpoints",
	"进程状态",
	"无证据时不建议重启",
}

func assertContainsNone(t *testing.T, label, body string, forbidden []string) {
	t.Helper()
	for _, item := range forbidden {
		if strings.Contains(body, item) {
			t.Fatalf("%s must not carry incident-specific content %q in:\n%s", label, item, body)
		}
	}
}

func TestPromptsConstrainAddressAndConnectionSemantics(t *testing.T) {
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsAll(t, label, prompt, endpointSemanticsRules)
	}
}

func TestDialogPromptsForbidTreatingTargetAddressAsHost(t *testing.T) {
	// 会话型任务（告警分析/调查）会直接给用户排查建议：必须禁止把目标地址
	// 当作可登录主机并下达主机级命令。
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsAll(t, label, prompt, []string{
			"未经确认不得称其为“主机/机器”",
			"不得建议登录该地址执行",
		})
	}
	// F2 失效形态的具体锚点：调查代理不得建议对指标地址执行主机级命令。
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, []string{
		"systemctl、ss、dmesg",
		"不要预设是哪一个分支",
	})
	// 巡检报告只解读证据：地址不得在报告中称为主机，分支列为待验证项。
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, []string{
		"报告中不得称为或暗示为主机",
		"待验证项",
	})
}

// TestPromptsStayGenericAboutIncident 禁止把单次盲测的真值写进提示词：业务
// 名、真实地址段、具体根因结论与精确 selector 要求都不得出现——约束必须
// 保持为可复用的通用语义。
func TestPromptsStayGenericAboutIncident(t *testing.T) {
	forbidden := []string{"10.43.", "mall", "Redis", "redis", "失配", "selector", "选择器"}
	for label, prompt := range map[string]string{
		"initial analysis prompt": SystemPrompt,
		"inspection prompt":       InspectionSystemPrompt,
		"investigation prompt":    InvestigationSystemPrompt,
	} {
		assertContainsNone(t, label, prompt, forbidden)
	}
}

// TestKeptPromptsPredateEndpointSemantics 在途 Attempt 的冻结 prompt 绝不随
// 本次补修演进。
func TestKeptPromptsPredateEndpointSemantics(t *testing.T) {
	for label, prompt := range map[string]string{
		"kept initial analysis": KeptAnalysisSystemPrompt,
		"kept inspection":       KeptInspectionSystemPrompt,
		"kept investigation":    KeptInvestigationSystemPrompt,
	} {
		for _, rule := range endpointSemanticsRules {
			if strings.Contains(prompt, rule) {
				t.Fatalf("%s prompt must stay frozen before the endpoint-semantics fix but mentions %q", label, rule)
			}
		}
	}
}

// TestBuildMessagesBindEndpointSemanticsPrompt 实际构造给模型的消息必须携带
// 补修后的当前提示词，而不是只有未使用的常量。
func TestBuildMessagesBindEndpointSemanticsPrompt(t *testing.T) {
	initial, err := BuildInitialMessages(mustParseInitialInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if initial[0].Content != SystemPrompt || !strings.Contains(initial[0].Content, "服务虚拟地址") {
		t.Fatal("BuildInitialMessages must render the endpoint-semantics system prompt")
	}

	investigation, err := BuildInvestigationMessages(InvestigationInput{
		Messages: []struct {
			Role        string            `json:"role"`
			Content     string            `json:"content"`
			Attachments []InputAttachment `json:"attachments,omitempty"`
		}{{Role: "user", Content: "Redis exporter 的 up 是 0，帮我看下"}},
		ModelContract: struct {
			ModelID             string `json:"modelId"`
			ContextBudgetTokens int    `json:"contextBudgetTokens"`
			MaxOutputTokens     int    `json:"maxOutputTokens"`
		}{ModelID: "fixture-chat-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if investigation[0].Content != InvestigationSystemPrompt || !strings.Contains(investigation[0].Content, "connection refused") {
		t.Fatal("BuildInvestigationMessages must render the endpoint-semantics system prompt")
	}

	inspection, err := BuildInspectionMessages(InspectionInput{
		SchemaKind: "inspection_analysis_v1", AttemptID: 1, InspectionRunID: 1,
		ArtifactIDs: []int64{10}, EvidenceIDs: []int64{20},
		ModelContract: struct {
			ModelID string `json:"modelId"`
		}{ModelID: "fixture-chat-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspection[0].Content != InspectionSystemPrompt || !strings.Contains(inspection[0].Content, "服务虚拟地址") {
		t.Fatal("BuildInspectionMessages must render the endpoint-semantics system prompt")
	}
}
