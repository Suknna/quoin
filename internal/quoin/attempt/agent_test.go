package attempt

// Agent model-call ledger tests (ARCH-AGENT-006, ARCH-TOOL-001): the exact
// Begin/Complete sequence the Plinth supervisor drives for one agent turn,
// including the tool-call closure the frozen success triggers enforce.

import (
	"context"
	"database/sql"
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
	// Runtime ingress must accept only the result schema fixed by this tool
	// definition, before a terminal completion can be persisted.
	if schema, err := service.ExpectedToolResultSchema(ctx, authorizations[0].ToolCallID); err != nil || schema != "workspace_tool_result_v1" {
		t.Fatalf("expected tool result schema=%q err=%v", schema, err)
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

func TestToolCallPreflightOutcomeIsSingleAssignmentAndImmutable(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	attemptID, _ := seedAttempt(t, db)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=?`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=?`, now, now, attemptID); err != nil {
		t.Fatal(err)
	}
	digest, err := CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	callID, err := service.BeginModelCall(ctx, BeginCall{AttemptID: attemptID, CallSeq: 1, ModelID: "fixture-chat-1", PromptDigest: strings.Repeat("a", 64), ToolSchemaDigest: digest, InputDigest: strings.Repeat("b", 64), RenderedDigest: strings.Repeat("c", 64), InputItems: []ModelInputItem{{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"}, {Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("e", 64), Role: "system"}, {Sequence: 3, ItemKind: "snapshot", ContentDigest: testDigest, Role: "system"}}, ContextBudget: 4096, MaxOutput: 1024})
	if err != nil {
		t.Fatal(err)
	}
	arguments := []byte(`{"command":"true"}`)
	proposed := []ProposedTool{{ProviderIndex: 0, ProviderToolCallID: "preflight-once", ToolName: "bash", ArgumentsJSON: arguments, ArgumentsDigest: sha256HexString(arguments)}}
	_, responseDigest, err := CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	completion := CompleteCall{AttemptID: attemptID, CallID: callID, Outcome: "succeeded", FinishReason: "tool_calls", ProposedTools: proposed, ResponseDigest: responseDigest, ResponseComplete: true}
	authorizations, err := service.CompleteModelCall(ctx, completion)
	if err != nil {
		t.Fatal(err)
	}
	toolCallID := authorizations[0].ToolCallID
	if _, err := db.Exec(`UPDATE tool_calls SET preflight_error_code='no_mapping',preflight_error_detail='没有 Kubernetes 连接',row_version=row_version+1 WHERE id=?`, toolCallID); err != nil {
		t.Fatalf("first pending preflight assignment: %v", err)
	}
	for _, statement := range []string{
		`UPDATE tool_calls SET preflight_error_code='target_not_found',preflight_error_detail='changed',row_version=row_version+1 WHERE id=?`,
		`UPDATE tool_calls SET preflight_error_code=NULL,preflight_error_detail=NULL,row_version=row_version+1 WHERE id=?`,
		`UPDATE tool_calls SET status='running',started_at='2026-01-01T00:00:00Z',preflight_error_code='no_mapping',preflight_error_detail='没有 Kubernetes 连接',row_version=row_version+1 WHERE id=?`,
	} {
		if _, err := db.Exec(statement, toolCallID); err == nil {
			t.Fatalf("immutable preflight mutation succeeded: %s", statement)
		}
	}
	var code, detail string
	if err := db.QueryRow(`SELECT preflight_error_code,preflight_error_detail FROM tool_calls WHERE id=?`, toolCallID).Scan(&code, &detail); err != nil || code != "no_mapping" || detail != "没有 Kubernetes 连接" {
		t.Fatalf("durable preflight after rejected mutations = %q/%q, err=%v", code, detail, err)
	}
}

// TestFrozenSchemaRejectsLegacyKubernetesToolNames loads the generated SQL
// contract and then bypasses Go catalog validation deliberately. This proves
// legacy Kubernetes verbs cannot be resurrected by a direct ledger insert.
func TestFrozenSchemaRejectsLegacyKubernetesToolNames(t *testing.T) {
	legacy := []string{"kubernetes_get", "kubernetes_list", "kubernetes_logs", "kubernetes_events"}
	for _, name := range legacy {
		t.Run(name, func(t *testing.T) {
			db := newTestDB(t)
			defer db.Close()
			attemptID, callID := sealedRawToolProposal(t, db, name)
			if _, err := insertRawToolCall(db, attemptID, callID, name); err == nil {
				t.Fatalf("legacy tool %q was accepted by frozen SQL", name)
			}
		})
	}
	t.Run("kubernetes_read remains legal", func(t *testing.T) {
		db := newTestDB(t)
		defer db.Close()
		attemptID, callID := sealedRawToolProposal(t, db, "kubernetes_read")
		if _, err := insertRawToolCall(db, attemptID, callID, "kubernetes_read"); err != nil {
			t.Fatalf("current Kubernetes tool rejected by frozen SQL: %v", err)
		}
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE attempt_id=? AND tool_name='kubernetes_read' AND status='pending'`, attemptID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("accepted kubernetes_read row count=%d err=%v", count, err)
		}
	})
}

// sealedRawToolProposal creates a correctly closed physical model call whose
// stored provider output names toolName. The test then targets the SQL closure
// directly instead of relying on the Go-side catalog to reject legacy input.
func sealedRawToolProposal(t *testing.T, db *sql.DB, toolName string) (int64, int64) {
	t.Helper()
	service := NewService(db)
	attemptID, _ := seedAttempt(t, db)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='tool-name-proof',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=?`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=?`, now, now, attemptID); err != nil {
		t.Fatal(err)
	}
	digest, err := CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	callID, err := service.BeginModelCall(context.Background(), BeginCall{
		AttemptID: attemptID, CallSeq: 1, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("a", 64), ToolSchemaDigest: digest,
		InputDigest: strings.Repeat("b", 64), RenderedDigest: strings.Repeat("c", 64),
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
	response, err := json.Marshal(map[string]any{
		"assistantText": "", "finishReason": "tool_calls",
		"tool_calls": []any{map[string]any{
			"id": "raw-" + toolName, "name": toolName,
			"arguments": map[string]any{"businessSystem": "payments", "operation": "discovery"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,?,?,?,?)`, callID, string(response), strings.Repeat("f", 64), "tool_calls", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
	return attemptID, callID
}

func insertRawToolCall(db *sql.DB, attemptID, callID int64, toolName string) (sql.Result, error) {
	arguments := `{"businessSystem":"payments","operation":"discovery"}`
	return db.Exec(`INSERT INTO tool_calls(attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,'pending',?)`,
		attemptID, callID, 1, 0, "raw-"+toolName, toolName, "1", arguments, sha256HexString([]byte(arguments)), "supervisor_typed", "return_to_model", time.Now().UTC().Format(time.RFC3339Nano))
}
