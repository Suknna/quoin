package model

// Canonical response digest pin: the supervisor's canonical chat response
// shape is the Quoin-side ledger digest contract. Plinth 不得 import
// Quoin 的内部包(ADR-0011 编译级不变量),所以这里用冻结的 golden JSON
// 字节钉住线上形状——任何字段名/结构漂移都会改变摘要而被拒绝。

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/Suknna/quoin/internal/plinth/agent"
)

func TestCanonicalResponseDigestMatchesFrozenShape(t *testing.T) {
	assistantText := "初步诊断：高错误率来自后端超时。"
	proposed := []ProposedTool{
		{ProviderIndex: 0, ProviderToolCallID: "call-1", ToolName: "bash", ArgumentsJSON: []byte(`{"command":"uptime"}`), ArgumentsDigest: "x"},
		{ProviderIndex: 1, ProviderToolCallID: "call-2", ToolName: "read", ArgumentsJSON: []byte(`{"path":"out.txt"}`), ArgumentsDigest: "y"},
	}
	got, err := canonicalResponseDigest(assistantText, proposed)
	if err != nil {
		t.Fatal(err)
	}
	golden := `{"assistantText":"初步诊断：高错误率来自后端超时。","toolCalls":[{"providerToolCallId":"call-1","toolName":"bash","arguments":{"command":"uptime"}},{"providerToolCallId":"call-2","toolName":"read","arguments":{"path":"out.txt"}}]}`
	goldenSum := sha256.Sum256([]byte(golden))
	if got != hex.EncodeToString(goldenSum[:]) {
		t.Fatalf("canonical response drift: model=%s golden=%s", got, hex.EncodeToString(goldenSum[:]))
	}
}

func TestCanonicalResponseEmptyTools(t *testing.T) {
	got, err := canonicalResponseDigest("结论", nil)
	if err != nil {
		t.Fatal(err)
	}
	goldenSum := sha256.Sum256([]byte(`{"assistantText":"结论","toolCalls":[]}`))
	if got != hex.EncodeToString(goldenSum[:]) {
		t.Fatalf("empty-tool canonical drift: model=%s golden=%s", got, hex.EncodeToString(goldenSum[:]))
	}
}

func TestPromptDigestUsesModeSelectedPrompt(t *testing.T) {
	initial := promptDigestFor(Contract{SystemPrompt: agent.SystemPrompt})
	investigation := promptDigestFor(Contract{SystemPrompt: agent.InvestigationSystemPrompt})
	if initial == investigation {
		t.Fatal("initial-analysis and investigation prompts must have distinct persisted digests")
	}
	if got := promptDigestFor(Contract{}); got != initial {
		t.Fatal("empty legacy contract must retain the initial-analysis prompt digest")
	}
}
