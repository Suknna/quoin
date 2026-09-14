package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	plinthagent "github.com/Suknna/quoin/internal/plinth/agent"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/tools/kubernetes"
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
	// The worker renders the attempt's FROZEN catalog (ADR-0004); the test
	// derives the digest exactly as the real worker does.
	frozen, err := service.Attempts().FrozenToolCatalog(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	toolsDigest, err := frozen.Digest()
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
	// The prompt renderer generation is owned by the worker prompt package and
	// must track the input renderer cutover (ADR-0004 source guidance); assert
	// against the worker-pinned constant, not the Quoin-side mapping function.
	if renderer != plinthagent.InvestigationRendererVersion || version != AgentVersion || storedPrompt != fmt.Sprintf("%x", promptSum[:]) || schemaVersion != "investigation-tools-v3" || schemaDigest != toolsDigest {
		t.Fatalf("provenance renderer=%q version=%q prompt=%q schema=%q/%q", renderer, version, storedPrompt, schemaVersion, schemaDigest)
	}
}

// TestKubernetesReadProposalIsRejectedWithoutToolCallOrEvidence proves the
// source-level admission: kubernetes_read is now a legal tool (ADR-0004),
// but a routing miss against an attempt with no frozen kubernetes source is
// only a recoverable preflight — no grant, no execution, no Evidence.
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
	if _, registered := attempt.LookupToolForAgentVersion(AgentVersion, "kubernetes_read"); !registered {
		t.Fatal("kubernetes_read must be offered to investigations (ADR-0004)")
	}
	frozen, err := service.Attempts().FrozenToolCatalog(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	toolsDigest, err := frozen.Digest()
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
	// Seal the anchor call and persist one pending kubernetes_read proposal
	// carrying a businessSystem target that cannot resolve: the attempt froze
	// no kubernetes source, so the routing miss must stay a recoverable
	// preflight with zero grants and zero evidence. (Driven at the resolver
	// seam until the frozen per-attempt catalog ships kubernetes_read.)
	now := testNow()
	response, err := json.Marshal(map[string]any{
		"assistantText": "", "finishReason": "tool_calls",
		"tool_calls": []any{map[string]any{
			"id": "kubernetes-disabled", "name": "kubernetes_read",
			"arguments": map[string]any{"businessSystem": "payments", "operation": "discovery"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,?,?,?,?)`, callID, string(response), fmt.Sprintf("%x", sha256.Sum256(response)), "tool_calls", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
	arguments := []byte(`{"businessSystem":"payments","operation":"discovery"}`)
	if _, err := db.Exec(`INSERT INTO tool_calls(attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,'pending',?)`,
		created.AttemptID, callID, 1, 0, "kubernetes-disabled", "kubernetes_read", "1", string(arguments), fmt.Sprintf("%x", sha256.Sum256(arguments)), "supervisor_typed", "return_to_model", now); err != nil {
		t.Fatal(err)
	}
	var toolCallID int64
	_ = db.QueryRow(`SELECT id FROM tool_calls WHERE attempt_id=?`, created.AttemptID).Scan(&toolCallID)
	conn, connErr := db.Conn(ctx)
	if connErr != nil {
		t.Fatal(connErr)
	}
	resolution, resolveErr := kubernetes.ResolveRead(ctx, conn, created.AttemptID, toolCallID)
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if resolveErr != nil {
		t.Fatalf("routing miss must stay a recoverable preflight: %v", resolveErr)
	}
	if resolution.PreflightCode == "" || len(resolution.Grants) != 0 {
		t.Fatalf("resolution=%+v, want one grantless preflight", resolution)
	}
	var evidence, grants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM evidence WHERE attempt_id=?`, created.AttemptID).Scan(&evidence); err != nil || evidence != 0 {
		t.Fatalf("evidence=%d err=%v, want none", evidence, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants WHERE attempt_id=? AND purpose='kubernetes_read'`, created.AttemptID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("grants=%d err=%v, want none", grants, err)
	}
}
