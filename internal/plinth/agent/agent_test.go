package agent

// Investigation input assembly tests (T14): the frozen investigation_v1
// canonical bytes (messages with attachment locators, sources, chat
// contract) parse and render deterministically; attachment bodies never
// inline — only the locator block that points the model at the granted
// artifact_read/artifact_grep tools.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestParseInputRequiresBusinessContextOrSources(t *testing.T) {
	// ADR-0004: an attempt without a business declaration must carry the
	// frozen integrations as its source-level authority; neither is invalid.
	_, err := ParseInput([]byte(`{
		"occurrence":{"id":"1","labels":{}},
		"modelContract":{"modelId":"fixture"}
	}`))
	if err == nil || !strings.Contains(err.Error(), "integrations") {
		t.Fatalf("ParseInput error = %v, want missing integrations", err)
	}
}

// TestSourceIntegrationsRenderIntoModelPrompt proves the source names the
// model needs for sourceRef actually reach the rendered prompt (not only
// the canonical JSON) while endpoint/secret facts never do.
func TestSourceIntegrationsRenderIntoModelPrompt(t *testing.T) {
	input, err := ParseInput([]byte(`{
		"occurrence":{"id":"1","labels":{"alertname":"HighErrorRate"}},
		"integrations":[{"kind":"metrics","name":"thanos-prod"},{"kind":"kubernetes","name":"k8s-prod"}],
		"modelContract":{"modelId":"fixture"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInitialMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, message := range messages {
		joined += message.Content
	}
	for _, required := range []string{"thanos-prod", "k8s-prod", "sourceRef", "resourceRef 在此模式不可用", `thanos_query({sourceRef: "thanos-prod", query: "up"})`} {
		if !strings.Contains(joined, required) {
			t.Fatalf("source prompt missing %q in: %s", required, joined)
		}
	}
	for _, secret := range []string{"baseUrl", "http://thanos.test", "password", "bearerToken", "kubeconfig"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("source prompt leaked %q", secret)
		}
	}
}

// TestSourceIntegrationsRenderIntoInvestigationPrompt proves the same
// source-level visibility for direct chat prompts.
func TestSourceIntegrationsRenderIntoInvestigationPrompt(t *testing.T) {
	canonical := []byte(`{
	  "messages": [{"role": "user", "content": "查一下错误率"}],
	  "sources": [],
	  "integrations": [{"kind": "metrics", "name": "thanos-prod"}],
	  "modelContract": {"modelId": "fixture-chat-1", "contextBudgetTokens": 4096, "maxOutputTokens": 1024}
	}`)
	input, err := ParseInvestigationInput(canonical)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInvestigationMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, message := range messages {
		joined += message.Content
	}
	for _, required := range []string{"thanos-prod", "sourceRef", "resourceRef 在此模式不可用"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("investigation source prompt missing %q in: %s", required, joined)
		}
	}
	if strings.Contains(joined, "http://thanos.test") || strings.Contains(joined, "kubeconfig") {
		t.Fatal("investigation source prompt leaked endpoint or credential facts")
	}
}

// TestBuildInitialMessagesIncludesFrozenBusinessContext proves the business
// view still reaches the model as descriptive context while the tool call
// shape stays the source-level one (ADR-0004): no resourceRef guidance may
// return, and scope guidance only renders for frozen integrations.
func TestBuildInitialMessagesIncludesFrozenBusinessContext(t *testing.T) {
	input, err := ParseInput([]byte(`{
		"occurrence":{"id":"1","labels":{"business_system":"payments"}},
		"businessContext":{"systemKey":"payments","configVersionId":"8","resources":[{"name":"pods","displayName":"Pods","allowedMetrics":["up","http_requests_*"]}]},
		"modelContract":{"modelId":"fixture"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInitialMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	// Business context alone: exactly system + user, the declaration renders
	// as context, and the removed resourceRef vocabulary never appears.
	if len(messages) != 2 || !strings.Contains(messages[1].Content, "业务配置上下文") || !strings.Contains(messages[1].Content, "payments") {
		t.Fatalf("messages = %#v", messages)
	}
	joined := ""
	for _, message := range messages {
		joined += message.Content
	}
	if strings.Contains(joined, "resourceRef") {
		t.Fatalf("declared context must not resurrect resourceRef guidance: %s", joined)
	}
}

func TestInitialAnalysisPromptGroundsDetectorNamesAndSeparatesHypotheses(t *testing.T) {
	input, err := ParseInput([]byte(`{
		"occurrence":{"id":"1","labels":{"alertname":"MallGUIAcceptanceProbe"},"annotations":{"summary":"controlled GUI acceptance probe","description":"No true fault; this is a controlled test annotation."}},
		"integrations":[{"kind":"metrics","name":"thanos-prod"}],
		"modelContract":{"modelId":"fixture"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInitialMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"不得仅因名称、标签或注释推断", "已知事实", "待验证假设", "MallGUIAcceptanceProbe", "No true fault; this is a controlled test annotation."} {
		if !strings.Contains(messages[0].Content+messages[2].Content, required) {
			t.Fatalf("initial analysis messages missing %q", required)
		}
	}
}

// TestBuildInvestigationMessagesRendersSourceScopeForDeclaredAttempts proves
// the cutover: even a business-attributed conversation resolves tools by
// sourceRef against the frozen integrations; the declaration's resourceRef
// vocabulary is gone from prompts (ADR-0004).
func TestBuildInvestigationMessagesRendersSourceScopeForDeclaredAttempts(t *testing.T) {
	input, err := ParseInvestigationInput([]byte(`{
		"messages":[{"role":"user","content":"查询 Java、MySQL 和 Redis 状态"}],
		"sources":[],
		"businessContext":{"systemKey":"local-inspection-demo","configVersionId":"17","resources":[{"name":"services","displayName":"Services","allowedMetrics":["up","mysql_up","redis_up"]}]},
		"integrations":[{"kind":"metrics","name":"thanos-demo"},{"kind":"kubernetes","name":"k8s-demo"}],
		"modelContract":{"modelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInvestigationMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages=%d want system, scope, user", len(messages))
	}
	scope := messages[1].Content
	for _, required := range []string{"sourceRef", "thanos-demo", "k8s-demo", "resourceRef 在此模式不可用"} {
		if !strings.Contains(scope, required) {
			t.Fatalf("scope prompt missing %q: %s", required, scope)
		}
	}
	for _, removed := range []string{"resourceRef 和 query", `可用 resourceRef`, `业务系统 "local-inspection-demo"`} {
		if strings.Contains(scope, removed) {
			t.Fatalf("scope prompt resurrects the removed declaration shape %q: %s", removed, scope)
		}
	}
}

func TestInvestigationPromptUsesPlatformHistoryAsAlertOccurrenceAuthority(t *testing.T) {
	for _, required := range []string{
		"以平台提供的近期告警记录为准",
		"时间区间查询",
		"不能单独证明某条告警曾经触发",
		"数值与标识符必须与工具返回逐字一致",
	} {
		if !strings.Contains(InvestigationSystemPrompt, required) {
			t.Fatalf("investigation prompt missing %q: %s", required, InvestigationSystemPrompt)
		}
	}
}

func TestBuildInvestigationMessagesRendersRecentOccurrenceHistory(t *testing.T) {
	input, err := ParseInvestigationInput([]byte(`{
		"messages":[{"role":"user","content":"最近有过告警吗"}],
		"sources":[],
		"recentOccurrences":[
			{"id":"12","sourceKey":"prod-am","startsAt":"2026-09-15T10:00:00Z","labels":{"alertname":"TargetDown","instance":"api-1"}},
			{"id":"11","sourceKey":"prod-am","startsAt":"2026-09-14T10:00:00Z","labels":{"alertname":"HighLatency"}}
		],
		"modelContract":{"modelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}
	}`))
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
	history := messages[1].Content
	for _, required := range []string{"近期告警记录", `"id": "12"`, `"sourceKey": "prod-am"`, `"alertname": "TargetDown"`, `"id": "11"`} {
		if !strings.Contains(history, required) {
			t.Fatalf("history prompt missing %q: %s", required, history)
		}
	}
	if strings.Index(history, `"id": "12"`) > strings.Index(history, `"id": "11"`) {
		t.Fatalf("history order changed: %s", history)
	}
	for _, mutable := range []string{"state", "resolvedAt", "lastStateChangeAt"} {
		if strings.Contains(history, mutable) {
			t.Fatalf("history prompt exposed mutable field %q: %s", mutable, history)
		}
	}
}

func TestParseInvestigationInputAttachments(t *testing.T) {
	canonical := []byte(`{
	  "messages": [
	    {"role": "user", "content": "请阅读附件", "attachments": [
	      {"filename": "logs.txt", "artifactId": "42", "sizeBytes": 2048}
	    ]},
	    {"role": "assistant", "content": "已读取"}
	  ],
	  "sources": [],
	  "modelContract": {"modelId": "fixture-chat-1", "contextBudgetTokens": 4096, "maxOutputTokens": 1024}
	}`)
	input, err := ParseInvestigationInput(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Messages) != 2 {
		t.Fatalf("messages=%d", len(input.Messages))
	}
	if len(input.Messages[0].Attachments) != 1 || input.Messages[0].Attachments[0].ArtifactID != "42" {
		t.Fatalf("attachments wrong: %+v", input.Messages[0].Attachments)
	}
	messages, err := BuildInvestigationMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 { // system + user + assistant
		t.Fatalf("messages=%d", len(messages))
	}
	user := messages[1]
	if user.Role != schema.User || !strings.Contains(user.Content, "请阅读附件") {
		t.Fatalf("user turn wrong: %+v", user)
	}
	if !strings.Contains(user.Content, "[附件 1] logs.txt（artifactId=42，2048 字节）") {
		t.Fatalf("attachment locator block missing: %q", user.Content)
	}
	if !strings.Contains(user.Content, "artifact_read") {
		t.Fatalf("locator block must name the read tools: %q", user.Content)
	}
	// An attachment-free turn renders untouched.
	plain, err := BuildInvestigationMessages(InvestigationInput{
		Messages: []struct {
			Role        string            `json:"role"`
			Content     string            `json:"content"`
			Attachments []InputAttachment `json:"attachments,omitempty"`
		}{{Role: "user", Content: "纯文本"}},
		ModelContract: struct {
			ModelID             string `json:"modelId"`
			ContextBudgetTokens int    `json:"contextBudgetTokens"`
			MaxOutputTokens     int    `json:"maxOutputTokens"`
		}{ModelID: "fixture-chat-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain[1].Content != "纯文本" {
		t.Fatalf("plain turn must stay untouched: %q", plain[1].Content)
	}
	// A round trip keeps the canonical shape stable.
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"artifactId":"42"`) {
		t.Fatalf("round trip lost the locator: %s", encoded)
	}
}
