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

func TestParseInputRequiresBusinessContext(t *testing.T) {
	_, err := ParseInput([]byte(`{
		"occurrence":{"id":"1","labels":{}},
		"modelContract":{"modelId":"fixture"}
	}`))
	if err == nil || !strings.Contains(err.Error(), "business context") {
		t.Fatalf("ParseInput error = %v, want missing business context", err)
	}
}

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
	if len(messages) != 3 || !strings.Contains(messages[2].Content, "业务配置上下文") || !strings.Contains(messages[2].Content, "payments") {
		t.Fatalf("messages = %#v", messages)
	}
	for _, required := range []string{`resourceRef`, `pods`, `允许指标：up、http_requests_*`, `query: "up"`} {
		if !strings.Contains(messages[1].Content, required) {
			t.Fatalf("initial scope prompt missing %q: %s", required, messages[1].Content)
		}
	}
}

func TestInitialAnalysisPromptGroundsDetectorNamesAndSeparatesHypotheses(t *testing.T) {
	input, err := ParseInput([]byte(`{
		"occurrence":{"id":"1","labels":{"alertname":"MallGUIAcceptanceProbe"},"annotations":{"summary":"controlled GUI acceptance probe","description":"No true fault; this is a controlled test annotation."}},
		"businessContext":{"systemKey":"mall","configVersionId":"8","resources":[{"name":"pods","displayName":"Pods","allowedMetrics":["up"]}]},
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

func TestBuildInvestigationMessagesRendersMandatoryBusinessSelector(t *testing.T) {
	input, err := ParseInvestigationInput([]byte(`{
		"messages":[{"role":"user","content":"查询 Java、MySQL 和 Redis 状态"}],
		"sources":[],
		"businessContext":{"systemKey":"local-inspection-demo","configVersionId":"17","resources":[{"name":"services","displayName":"Services","allowedMetrics":["up","mysql_up","redis_up"]}]},
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
	for _, required := range []string{`业务系统 "local-inspection-demo"`, `resourceRef`, `services`, `允许指标：up、mysql_up、redis_up`, `query: "up"`} {
		if !strings.Contains(scope, required) {
			t.Fatalf("scope prompt missing %q: %s", required, scope)
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
