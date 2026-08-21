package attempt

// Agent model-call ledger tests (ARCH-AGENT-006, ARCH-TOOL-001): the exact
// Begin/Complete sequence the Plinth supervisor drives for one agent turn,
// including the tool-call closure the frozen success triggers enforce.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAgentModelCallWithToolClosure(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	attemptID, analysisID := seedAttempt(t, db)
	_ = analysisID
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Queued -> Assigned -> Running (the frozen ladder).
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, now, attemptID); err != nil {
		t.Fatal(err)
	}

	toolsDigest, err := CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	callID, err := service.BeginModelCall(ctx, BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0,
		ModelID:          "fixture-chat-1",
		PromptDigest:     strings.Repeat("a", 64),
		ToolSchemaDigest: toolsDigest,
		InputDigest:      strings.Repeat("b", 64), RenderedDigest: strings.Repeat("c", 64),
		InputItems: []ModelInputItem{
			{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"},
			{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("e", 64), Role: "system"},
			{Sequence: 3, ItemKind: "snapshot", ContentDigest: testDigest, Role: "system"},
		},
		ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments := []byte(`{"command":"echo agent-fixture-proof"}`)
	proposed := []ProposedTool{{
		ProviderIndex: 0, ProviderToolCallID: "call-agent-bash",
		ToolName: "bash", ArgumentsJSON: arguments, ArgumentsDigest: sha256HexString(arguments),
	}}
	_, responseDigest, err := CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	authorizations, err := service.CompleteModelCall(ctx, CompleteCall{
		AttemptID: attemptID, CallID: callID,
		Outcome: "succeeded", FinishReason: "tool_calls",
		AssistantText: "", ProposedTools: proposed,
		ResponseDigest: responseDigest, ResponseComplete: true,
		InputTokens: 12, OutputTokens: 8, TotalTokens: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(authorizations) != 1 || authorizations[0].ToolCallID == 0 {
		t.Fatalf("authorizations=%+v", authorizations)
	}
	// The pending tool call rows exist and the sealed output carries the
	// same tool_calls array (trg_execution_attempts_success_result).
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE attempt_id=? AND status='pending' AND tool_name='bash'`, attemptID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	var outputJSON string
	if err := db.QueryRow(`SELECT o.response_json FROM model_call_outputs o WHERE o.model_call_id=?`, callID).Scan(&outputJSON); err != nil {
		t.Fatal(err)
	}
	var output struct {
		ToolCalls []any `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(outputJSON), &output); err != nil || len(output.ToolCalls) != 1 {
		t.Fatalf("output=%s err=%v", outputJSON, err)
	}
	// Execute the tool: BeginToolCall -> CompleteToolCall.
	if err := service.BeginToolCall(ctx, attemptID, authorizations[0].ToolCallID); err != nil {
		t.Fatal(err)
	}
	resultJSON := `{"success":true,"output":"agent-fixture-proof\n"}`
	if _, err := service.CompleteToolCall(ctx, ToolResult{
		AttemptID: attemptID, ToolCallID: authorizations[0].ToolCallID,
		Outcome: "succeeded", ResultJSON: resultJSON,
	}); err != nil {
		t.Fatal(err)
	}
	var status, stored string
	if err := db.QueryRow(`SELECT status,result_json FROM tool_calls WHERE id=?`, authorizations[0].ToolCallID).Scan(&status, &stored); err != nil || status != "succeeded" || stored != resultJSON {
		t.Fatalf("status=%s stored=%s err=%v", status, stored, err)
	}
	// The second agent turn references the prior call and the tool result
	// in its lineage and closes with the text-only canonical output.
	secondCall, err := service.BeginModelCall(ctx, BeginCall{
		AttemptID: attemptID, CallSeq: 2, RetrySeq: 0,
		ModelID:          "fixture-chat-1",
		PromptDigest:     strings.Repeat("a", 64),
		ToolSchemaDigest: toolsDigest,
		InputDigest:      strings.Repeat("b", 64), RenderedDigest: strings.Repeat("c", 64),
		InputItems: []ModelInputItem{
			{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"},
			{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("e", 64), Role: "system"},
			{Sequence: 3, ItemKind: "snapshot", ContentDigest: testDigest, Role: "system"},
			{Sequence: 4, ItemKind: "prior_call", ItemID: callID, ContentDigest: strings.Repeat("f", 64), Role: "assistant"},
			{Sequence: 5, ItemKind: "tool_call", ItemID: authorizations[0].ToolCallID, ContentDigest: strings.Repeat("1", 64), Role: "tool"},
		},
		ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	finalText := "初步诊断：该告警为 agent-fixture-proof 场景的可复现示例。"
	_, finalDigest, err := CanonicalChatResponseJSON(finalText, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteModelCall(ctx, CompleteCall{
		AttemptID: attemptID, CallID: secondCall,
		Outcome: "succeeded", FinishReason: "stop",
		AssistantText: finalText, ResponseDigest: finalDigest, ResponseComplete: true,
		InputTokens: 10, OutputTokens: 4, TotalTokens: 14,
	}); err != nil {
		t.Fatal(err)
	}
	// The whole closure satisfies the succeeded-attempt trigger (the output
	// row is the analysis aggregate's job; the trigger requires it inside
	// the same transaction as the attempt closure).
	if _, err := db.Exec(`INSERT INTO initial_analysis_outputs(analysis_id,attempt_id,model_id,content,created_at) VALUES(?,?,'fixture-chat-1',?,?)`, analysisID, attemptID, finalText, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, attemptID); err != nil {
		t.Fatal(err)
	}
}
