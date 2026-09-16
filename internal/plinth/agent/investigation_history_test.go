package agent

// Renderer v3 renders Quoin's own recent alert history into the
// investigation conversation, and the fixed contract forbids the
// "instant ALERTS empty => no rules/no alerts" inference.

import (
	"strings"
	"testing"
)

func TestBuildInvestigationMessagesRendersRecentAlertHistory(t *testing.T) {
	input, err := ParseInvestigationInput([]byte(`{
		"messages":[{"role":"user","content":"总结当前最需要处理的告警"}],
		"sources":[],
		"integrations":[{"kind":"metrics","name":"mall-shop-prometheus"}],
		"recentOccurrences":[
			{"id":"2","sourceKey":"mall-shop","startsAt":"2026-09-16T13:37:48.339Z",
			 "labels":{"alertname":"MallShopMiddlewareTargetDown","severity":"critical"}}
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
	// system contract + scope guidance + history block + user turn (the
	// sources block only renders when the conversation carries provenance).
	if len(messages) != 4 {
		t.Fatalf("messages=%d want contract, scope, history, user", len(messages))
	}
	history := messages[2].Content
	for _, required := range []string{"近期告警记录", "MallShopMiddlewareTargetDown", "critical", "已恢复"} {
		if !strings.Contains(history, required) {
			t.Fatalf("history block missing %q: %s", required, history)
		}
	}
	// The fixed contract must block the empty-instant inference and require
	// verbatim evidence numbers.
	for _, required := range []string{"ALERTS 为空", "没有告警规则", "min_over_time", "逐字一致"} {
		if !strings.Contains(InvestigationSystemPrompt, required) {
			t.Fatalf("investigation contract missing %q", required)
		}
	}
	// Without history the block is absent, not empty (renderer v2 shape).
	plain, err := BuildInvestigationMessages(InvestigationInput{
		Messages:      input.Messages,
		ModelContract: input.ModelContract,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 2 {
		t.Fatalf("history-free messages=%d want contract, user", len(plain))
	}
}
