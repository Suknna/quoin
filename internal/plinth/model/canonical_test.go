package model

// Canonical response digest pin: the supervisor's canonical chat response
// must match the Quoin-side ledger digest byte-for-byte
// (internal/quoin/attempt.CanonicalChatResponseJSON), otherwise every
// CompleteModelCall would be rejected by the digest gate.

import (
	"testing"

	"github.com/Suknna/quoin/internal/plinth/agent"
	"github.com/Suknna/quoin/internal/quoin/attempt"
)

func TestCanonicalResponseDigestMatchesQuoin(t *testing.T) {
	assistantText := "初步诊断：高错误率来自后端超时。"
	proposed := []ProposedTool{
		{ProviderIndex: 0, ProviderToolCallID: "call-1", ToolName: "bash", ArgumentsJSON: []byte(`{"command":"uptime"}`), ArgumentsDigest: "x"},
		{ProviderIndex: 1, ProviderToolCallID: "call-2", ToolName: "read", ArgumentsJSON: []byte(`{"path":"out.txt"}`), ArgumentsDigest: "y"},
	}
	got, err := canonicalResponseDigest(assistantText, proposed)
	if err != nil {
		t.Fatal(err)
	}
	quoinTools := []attempt.ProposedTool{
		{ProviderIndex: 0, ProviderToolCallID: "call-1", ToolName: "bash", ArgumentsJSON: []byte(`{"command":"uptime"}`)},
		{ProviderIndex: 1, ProviderToolCallID: "call-2", ToolName: "read", ArgumentsJSON: []byte(`{"path":"out.txt"}`)},
	}
	_, want, err := attempt.CanonicalChatResponseJSON(assistantText, quoinTools)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("canonical response drift: model=%s attempt=%s", got, want)
	}
}

func TestCanonicalResponseEmptyTools(t *testing.T) {
	got, err := canonicalResponseDigest("结论", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, want, err := attempt.CanonicalChatResponseJSON("结论", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("empty-tool canonical drift: model=%s attempt=%s", got, want)
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
