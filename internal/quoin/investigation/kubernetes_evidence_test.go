package investigation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

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

// TestKubernetesReadCompleteToolCallCreatesEvidenceAtomically runs the real
// investigation Attempt service and registered EvidenceWriter. A rejected
// projection must roll back the terminal Tool Call together with Evidence.
func TestKubernetesReadCompleteToolCallCreatesEvidenceAtomically(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, principalID, "cmd-kubernetes-evidence", "调查 Kubernetes", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	var grantID, snapshotID int64
	if err := db.QueryRow(`SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'`, created.AttemptID).Scan(&grantID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, created.AttemptID).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,1,0,'chat','fixture-chat-1',?,?,?, ?,?,?,?, ?,4096,1024,0,0,'running',?)`, created.AttemptID, grantID, RendererVersion, AgentVersion, strings.Repeat("2", 64), strings.Repeat("2", 64), strings.Repeat("2", 64), strings.Repeat("2", 64), strings.Repeat("2", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	modelCallID, err := call.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, modelCallID, strings.Repeat("2", 64), modelCallID, strings.Repeat("2", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,3,'system',?,?)`, modelCallID, strings.Repeat("2", 64), snapshotID); err != nil {
		t.Fatal(err)
	}
	response := `{"assistantText":"","finishReason":"tool_calls","tool_calls":[{"id":"kubernetes-evidence-valid","name":"kubernetes_read"},{"id":"kubernetes-evidence-invalid","name":"kubernetes_read"},{"id":"kubernetes-evidence-partial","name":"kubernetes_read"},{"id":"kubernetes-evidence-malformed","name":"kubernetes_read"}]}`
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,?,?, 'tool_calls', ?)`, modelCallID, response, strings.Repeat("2", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, modelCallID); err != nil {
		t.Fatal(err)
	}
	insertCall := func(index int, providerID string) int64 {
		t.Helper()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		result, err := db.Exec(`INSERT INTO tool_calls(attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
			VALUES(?,?,?,?,?,'kubernetes_read','investigation-tools-v1',?,?, 'supervisor_typed','return_to_model','pending',?)`,
			created.AttemptID, modelCallID, 1, index, providerID,
			`{"businessSystem":"payments","operation":"discovery"}`, strings.Repeat("a", 64), now)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	validID := insertCall(0, "kubernetes-evidence-valid")
	if err := service.Attempts().BeginToolCall(ctx, created.AttemptID, validID); err != nil {
		t.Fatal(err)
	}
	valid := `{"success":true,"operation":"discovery","observedAt":"2026-08-24T00:00:00Z","totalBytes":2,"totalLines":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","results":[{"success":true,"output":"{}","truncated":false}]}`
	ids, err := service.Attempts().CompleteToolCall(ctx, attempt.ToolResult{AttemptID: created.AttemptID, ToolCallID: validID, Outcome: "succeeded", ResultJSON: valid})
	if err != nil {
		t.Fatalf("complete valid Kubernetes tool: %v", err)
	}
	if len(ids) != 1 || ids[0] == 0 {
		t.Fatalf("evidence ids=%v", ids)
	}
	var status string
	var evidenceCount int
	if err := db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, validID).Scan(&status); err != nil || status != "succeeded" {
		t.Fatalf("valid tool status=%q err=%v", status, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM evidence WHERE tool_call_id=?`, validID).Scan(&evidenceCount); err != nil || evidenceCount != 1 {
		t.Fatalf("valid evidence count=%d err=%v", evidenceCount, err)
	}

	invalidID := insertCall(1, "kubernetes-evidence-invalid")
	if err := service.Attempts().BeginToolCall(ctx, created.AttemptID, invalidID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Attempts().CompleteToolCall(ctx, attempt.ToolResult{AttemptID: created.AttemptID, ToolCallID: invalidID, Outcome: "succeeded", ResultJSON: `{"success":false}`}); err == nil {
		t.Fatal("invalid projector payload completed")
	}
	if err := db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, invalidID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("invalid tool status=%q err=%v, want running rollback", status, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM evidence WHERE tool_call_id=?`, invalidID).Scan(&evidenceCount); err != nil || evidenceCount != 0 {
		t.Fatalf("invalid evidence count=%d err=%v, want zero rollback", evidenceCount, err)
	}
	// Close the deliberately rejected call before executing its next sibling;
	// the frozen ledger permits only sequential Tool Calls.
	if _, err := service.Attempts().CompleteToolCall(ctx, attempt.ToolResult{AttemptID: created.AttemptID, ToolCallID: invalidID, Outcome: "cancelled", ErrorCode: "projection_rejected", ErrorDetail: "test cleanup"}); err != nil {
		t.Fatalf("cancel rejected tool call: %v", err)
	}

	// A partial result retains the raw aggregate solely in an Artifact, but
	// still creates incomplete Evidence from its successful mapping facts.
	partialID := insertCall(2, "kubernetes-evidence-partial")
	if err := service.Attempts().BeginToolCall(ctx, created.AttemptID, partialID); err != nil {
		t.Fatal(err)
	}
	artifactBody := []byte(`{"raw":"partial Kubernetes aggregate"}`)
	sum := sha256.Sum256(artifactBody)
	sha := fmt.Sprintf("%x", sum[:])
	blob, err := db.Exec(`INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at) VALUES(?,?,?,?)`, sha, len(artifactBody), "test/"+sha, now)
	if err != nil {
		t.Fatal(err)
	}
	blobID, err := blob.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := db.Exec(`INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,expires_at,created_at) VALUES(?,'tool_result','application/json',0,'generated','tool_call',?,?,?)`, blobID, partialID, "2099-01-01T00:00:00Z", now)
	if err != nil {
		t.Fatal(err)
	}
	artifactID, err := artifact.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	partial := `{"success":false,"operation":"discovery","observedAt":"2026-08-24T00:00:00Z","totalBytes":34,"totalLines":1,"sha256":"` + sha + `","errorCode":"partial_failure","errorDetail":"one mapping unavailable","results":[{"success":true,"output":"{}","truncated":false},{"success":false,"errorCode":"grant_missing","errorDetail":"connection unavailable"}]}`
	ids, err = service.Attempts().CompleteToolCall(ctx, attempt.ToolResult{AttemptID: created.AttemptID, ToolCallID: partialID, Outcome: "succeeded", ResultJSON: partial, ArtifactID: artifactID})
	if err != nil {
		t.Fatalf("complete artifact-backed partial Kubernetes tool: %v", err)
	}
	if len(ids) != 1 || ids[0] == 0 {
		t.Fatalf("partial evidence ids=%v", ids)
	}
	var integrity string
	var evidenceArtifactID int64
	if err := db.QueryRow(`SELECT t.status,e.integrity,e.artifact_id FROM tool_calls t JOIN evidence e ON e.tool_call_id=t.id WHERE t.id=?`, partialID).Scan(&status, &integrity, &evidenceArtifactID); err != nil || status != "succeeded" || integrity != "incomplete" || evidenceArtifactID != artifactID {
		t.Fatalf("partial status=%q integrity=%q artifact=%d err=%v", status, integrity, evidenceArtifactID, err)
	}

	malformedID := insertCall(3, "kubernetes-evidence-malformed")
	if err := service.Attempts().BeginToolCall(ctx, created.AttemptID, malformedID); err != nil {
		t.Fatal(err)
	}
	malformed := `{"success":false,"operation":"discovery","observedAt":"2026-08-24T00:00:00Z","totalBytes":1,"totalLines":1,"sha256":"` + sha + `","errorCode":"partial_failure","errorDetail":"all mappings failed","results":[{"success":false,"errorCode":"grant_missing","errorDetail":"connection unavailable"}]}`
	if _, err := service.Attempts().CompleteToolCall(ctx, attempt.ToolResult{AttemptID: created.AttemptID, ToolCallID: malformedID, Outcome: "succeeded", ResultJSON: malformed, ArtifactID: artifactID}); err == nil {
		t.Fatal("all-failure partial Kubernetes evidence completed")
	}
	if err := db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, malformedID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("malformed partial status=%q err=%v, want running rollback", status, err)
	}
}
