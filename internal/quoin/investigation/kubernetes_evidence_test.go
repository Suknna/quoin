package investigation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	plinthagent "github.com/Suknna/quoin/internal/plinth/agent"
	"github.com/Suknna/quoin/internal/quoin/attempt"
)

func TestInvestigationModelCallPersistsModeProvenance(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, principalID, "cmd-investigation-provenance", "调查 Kubernetes", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	toolsDigest, err := attempt.CanonicalToolsDigest(AgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	var snapshotDigest string
	if err := db.QueryRow(`SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, created.AttemptID).Scan(&snapshotDigest); err != nil {
		t.Fatal(err)
	}
	promptSum := sha256.Sum256([]byte(plinthagent.InvestigationSystemPrompt))
	callID, err := service.Attempts().BeginModelCall(ctx, attempt.BeginCall{
		AttemptID: created.AttemptID, CallSeq: 1, ModelID: "fixture-chat-1",
		PromptDigest: fmt.Sprintf("%x", promptSum[:]), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("a", 64), RenderedDigest: strings.Repeat("b", 64),
		InputItems:    []attempt.ModelInputItem{{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("c", 64), Role: "system"}, {Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("d", 64), Role: "system"}, {Sequence: 3, ItemKind: "snapshot", ContentDigest: snapshotDigest, Role: "system"}},
		ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	var renderer, version, storedPrompt, schemaVersion, schemaDigest string
	if err := db.QueryRow(`SELECT prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest FROM model_calls WHERE id=?`, callID).Scan(&renderer, &version, &storedPrompt, &schemaVersion, &schemaDigest); err != nil {
		t.Fatal(err)
	}
	if renderer != RendererVersion || version != AgentVersion || storedPrompt != fmt.Sprintf("%x", promptSum[:]) || schemaVersion != "investigation-tools-v1" || schemaDigest != toolsDigest {
		t.Fatalf("provenance renderer=%q version=%q prompt=%q schema=%q/%q", renderer, version, storedPrompt, schemaVersion, schemaDigest)
	}
}

// Kubernetes remains a stored connection domain, but must never become a model
// capability during the development gate. Rejection happens before tool-call
// persistence, so it cannot create an execution or Evidence side effect.
func TestKubernetesReadProposalIsRejectedWithoutToolCallOrEvidence(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, principalID, "cmd-kubernetes-gated", "调查 Kubernetes", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, registered := attempt.LookupToolForAgentVersion(AgentVersion, "kubernetes_read"); registered {
		t.Fatal("kubernetes_read remains registered for investigations")
	}
	toolsDigest, err := attempt.CanonicalToolsDigest(AgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	var snapshotDigest string
	if err := db.QueryRow(`SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, created.AttemptID).Scan(&snapshotDigest); err != nil {
		t.Fatal(err)
	}
	promptSum := sha256.Sum256([]byte(plinthagent.InvestigationSystemPrompt))
	callID, err := service.Attempts().BeginModelCall(ctx, attempt.BeginCall{
		AttemptID: created.AttemptID, CallSeq: 1, ModelID: "fixture-chat-1",
		PromptDigest: fmt.Sprintf("%x", promptSum[:]), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("a", 64), RenderedDigest: strings.Repeat("b", 64),
		InputItems:    []attempt.ModelInputItem{{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("c", 64), Role: "system"}, {Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("d", 64), Role: "system"}, {Sequence: 3, ItemKind: "snapshot", ContentDigest: snapshotDigest, Role: "system"}},
		ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments := []byte(`{"businessSystem":"payments","operation":"discovery"}`)
	proposed := []attempt.ProposedTool{{ProviderIndex: 0, ProviderToolCallID: "kubernetes-disabled", ToolName: "kubernetes_read", ArgumentsJSON: arguments, ArgumentsDigest: fmt.Sprintf("%x", sha256.Sum256(arguments))}}
	_, responseDigest, err := attempt.CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Attempts().CompleteModelCall(ctx, attempt.CompleteCall{AttemptID: created.AttemptID, CallID: callID, Outcome: "succeeded", FinishReason: "tool_calls", ProposedTools: proposed, ResponseDigest: responseDigest, ResponseComplete: true}); err == nil {
		t.Fatal("Kubernetes tool proposal was accepted")
	}
	var toolCalls, evidence int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE attempt_id=?`, created.AttemptID).Scan(&toolCalls); err != nil || toolCalls != 0 {
		t.Fatalf("tool calls=%d err=%v, want none", toolCalls, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM evidence WHERE attempt_id=?`, created.AttemptID).Scan(&evidence); err != nil || evidence != 0 {
		t.Fatalf("evidence=%d err=%v, want none", evidence, err)
	}
}
