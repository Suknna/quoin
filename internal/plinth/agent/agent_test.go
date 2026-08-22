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
