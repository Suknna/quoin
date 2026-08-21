package analysis

// T11 tool-authorization and evidence-closure tests over the real frozen
// schema and the production service wiring: the thanos_query grant freezes
// inside the CompleteModelCall transaction, execution authorization
// re-checks the connection state, and the deterministic Evidence commits
// in the same transaction as the Tool Call terminal state
// (ARCH-INPUT-003, DATA-CONN-002, ARCH-TOOL-003, DATA-EVIDENCE-001).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// seedThanosChain inserts one enabled thanos connection with a
// root-binding-matching credential generation (no probe qualification is
// required for thanos: the enable command only fences the partial index).
func seedThanosChain(t *testing.T, db *sql.DB) (connectionID, revisionID, generationID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, []byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 16)), now); err != nil {
		t.Fatal(err)
	}
	var existingConnectionID, existingRevisionID, existingGenerationID int64
	err := db.QueryRow(`SELECT c.id, c.current_revision_id, c.current_credential_generation_id FROM connections c WHERE c.type='thanos' AND c.enabled=1`).Scan(&existingConnectionID, &existingRevisionID, &existingGenerationID)
	if err == nil {
		return existingConnectionID, existingRevisionID, existingGenerationID
	}
	if err != sql.ErrNoRows {
		t.Fatalf("existing thanos lookup: %v", err)
	}
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?,'thanos',0,?)`, fmt.Sprintf("thanos-%d", seedCounter), now)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ = connection.LastInsertId()
	revision, err := db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,'{"baseUrl":"http://thanos.test"}',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, _ = revision.LastInsertId()
	nonce := make([]byte, 12)
	for i := range nonce {
		nonce[i] = byte(seedCounter*17 + i)
	}
	generation, err := db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, nonce, []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ = generation.LastInsertId()
	if _, err := db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=1, row_version=row_version+1 WHERE id=? AND enabled=0`, connectionID); err != nil {
		t.Fatal(err)
	}
	return connectionID, revisionID, generationID
}

// runThanosAttempt creates one analysis through the production path and
// drives its attempt to Running, opening the first chat call.
func runThanosAttempt(t *testing.T, db *sql.DB, service *Service, occurrenceID int64, commandID string) (attemptID, callID int64) {
	t.Helper()
	created, err := service.Create(context.Background(), occurrenceID, 1, commandID)
	if err != nil {
		t.Fatal(err)
	}
	attemptID = created.AttemptID
	if err := service.Attempts().BindToStream(context.Background(), attemptID, "boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().Accept(context.Background(), attemptID, "boot", 1); err != nil {
		t.Fatal(err)
	}
	snapshotDigest, err := service.Attempts().InputSnapshotDigest(context.Background(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	toolsDigest, err := attempt.CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	callID, err = service.Attempts().BeginModelCall(context.Background(), attempt.BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0,
		ModelID:          "fixture-chat-1",
		PromptDigest:     strings.Repeat("a", 64),
		ToolSchemaDigest: toolsDigest,
		InputDigest:      strings.Repeat("b", 64), RenderedDigest: strings.Repeat("c", 64),
		InputItems: []attempt.ModelInputItem{
			{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"},
			{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("e", 64), Role: "system"},
			{Sequence: 3, ItemKind: "snapshot", ContentDigest: snapshotDigest, Role: "system"},
		},
		ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return attemptID, callID
}

// completeThanosProposal seals the model call carrying one proposed
// thanos_query and returns the durable authorization.
func completeThanosProposal(t *testing.T, service *Service, attemptID, callID int64) attempt.ToolAuthorization {
	t.Helper()
	arguments := []byte(`{"query":"up"}`)
	proposed := []attempt.ProposedTool{{
		ProviderIndex: 0, ProviderToolCallID: "call-agent-thanos",
		ToolName: "thanos_query", ArgumentsJSON: arguments, ArgumentsDigest: sha256Hex(string(arguments)),
	}}
	_, responseDigest, err := attempt.CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	authorizations, err := service.Attempts().CompleteModelCall(context.Background(), attempt.CompleteCall{
		AttemptID: attemptID, CallID: callID,
		Outcome: "succeeded", FinishReason: "tool_calls",
		AssistantText: "", ProposedTools: proposed,
		ResponseDigest: responseDigest, ResponseComplete: true,
		InputTokens: 12, OutputTokens: 8, TotalTokens: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(authorizations) != 1 {
		t.Fatalf("authorizations=%+v", authorizations)
	}
	return authorizations[0]
}

// TestThanosGrantFreezesInToolCallTransaction proves the production wiring:
// the grant resolves inside the pending-row transaction and travels in the
// authorization (ARCH-INPUT-003).
func TestThanosGrantFreezesInToolCallTransaction(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-grant")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	if len(authorization.Grants) != 1 || authorization.Grants[0].Purpose != "thanos_query" {
		t.Fatalf("authorization=%+v", authorization)
	}
	var bindings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_call_connection_grants WHERE tool_call_id=? AND connection_grant_id=?`, authorization.ToolCallID, authorization.Grants[0].GrantID).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("bindings=%d err=%v", bindings, err)
	}
}

// TestThanosToolRejectedWithoutAuthorizationTarget proves the
// tool-before-authorization rejection: without an enabled Thanos
// connection the whole model call is refused and no tool call rows exist
// (RUNTIME-AGENT-005: an unresolvable tool route is invalid_response).
func TestThanosToolRejectedWithoutAuthorizationTarget(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-reject")
	arguments := []byte(`{"query":"up"}`)
	proposed := []attempt.ProposedTool{{
		ProviderIndex: 0, ProviderToolCallID: "call-agent-thanos",
		ToolName: "thanos_query", ArgumentsJSON: arguments, ArgumentsDigest: sha256Hex(string(arguments)),
	}}
	_, responseDigest, err := attempt.CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Attempts().CompleteModelCall(context.Background(), attempt.CompleteCall{
		AttemptID: attemptID, CallID: callID,
		Outcome: "succeeded", FinishReason: "tool_calls",
		AssistantText: "", ProposedTools: proposed,
		ResponseDigest: responseDigest, ResponseComplete: true,
		InputTokens: 12, OutputTokens: 8, TotalTokens: 20,
	}); err == nil {
		t.Fatal("complete must reject a thanos_query without an enabled connection")
	}
	var toolRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE attempt_id=?`, attemptID).Scan(&toolRows); err != nil || toolRows != 0 {
		t.Fatalf("toolRows=%d err=%v", toolRows, err)
	}
}

// TestThanosBeginToolCallExecutionFence proves the execution authorization
// re-reads the connection state (DATA-CONN-002): a disable committed after
// the grant refuses BeginToolCall and the tool call stays pending.
func TestThanosBeginToolCallExecutionFence(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	connectionID, _, _ := seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-fence")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	if _, err := db.Exec(`UPDATE connections SET enabled=0, row_version=row_version+1 WHERE id=?`, connectionID); err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BeginToolCall(context.Background(), attemptID, authorization.ToolCallID); err == nil {
		t.Fatal("begin must refuse a disabled connection")
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, authorization.ToolCallID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=1, row_version=row_version+1 WHERE id=?`, connectionID); err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BeginToolCall(context.Background(), attemptID, authorization.ToolCallID); err != nil {
		t.Fatalf("begin after re-enable: %v", err)
	}
}

// TestThanosEvidenceCommitsWithToolCallTerminalState proves the success
// transaction: tool call terminal state, deterministic Evidence and the
// grant binding commit atomically, and the Evidence detail projects the
// producer and connection facts (ARCH-TOOL-003, DATA-EVIDENCE-001).
func TestThanosEvidenceCommitsWithToolCallTerminalState(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-evidence")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	if err := service.Attempts().BeginToolCall(context.Background(), attemptID, authorization.ToolCallID); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(map[string]any{
		"success": true, "status": "success", "resultType": "vector", "sampleCount": 1,
		"startedAt": started, "finishedAt": started,
		"truncated": false, "totalBytes": 32, "totalLines": 1, "output": `{"status":"success"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceIDs, err := service.Attempts().CompleteToolCall(context.Background(), attempt.ToolResult{
		AttemptID: attemptID, ToolCallID: authorization.ToolCallID,
		Outcome: "succeeded", ResultJSON: string(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidenceIDs) != 1 {
		t.Fatalf("evidence ids=%v", evidenceIDs)
	}
	var toolStatus string
	if err := db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, authorization.ToolCallID).Scan(&toolStatus); err != nil || toolStatus != "succeeded" {
		t.Fatalf("toolStatus=%q err=%v", toolStatus, err)
	}
	detail, err := service.Evidence().Get(context.Background(), evidenceIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	producer, ok := detail.Producer.(map[string]any)
	if !ok || producer["toolName"] != "thanos_query" {
		t.Fatalf("producer=%v", detail.Producer)
	}
	if len(detail.Connections) != 1 || detail.Connections[0].Type != "thanos" {
		t.Fatalf("connections=%v", detail.Connections)
	}
}

// TestThanosSpilledResultBindsEvidenceToArtifact proves the long-output
// closure: the evidence body is exactly the committed tool_result Artifact
// (DATA-EVIDENCE-001: 正文位置恰好一个).
func TestThanosSpilledResultBindsEvidenceToArtifact(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-spill")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	if err := service.Attempts().BeginToolCall(context.Background(), attemptID, authorization.ToolCallID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	blob, err := db.Exec(`INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at) VALUES(?,?,?,?)`, strings.Repeat("a", 64), 70000, "blobs/test", now)
	if err != nil {
		t.Fatal(err)
	}
	blobID, _ := blob.LastInsertId()
	artifact, err := db.Exec(`INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,expires_at,created_at) VALUES(?,'tool_result','application/json',0,'generated','tool_call',?,?,?)`, blobID, authorization.ToolCallID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	artifactID, _ := artifact.LastInsertId()
	payload, err := json.Marshal(map[string]any{
		"success": true, "status": "success", "resultType": "matrix", "sampleCount": 900,
		"startedAt": now, "finishedAt": now,
		"truncated": true, "totalBytes": 70000, "totalLines": 3600, "output": "…（完整输出已存入 Artifact）\nhead",
		"artifact": map[string]any{"id": fmt.Sprint(artifactID), "mediaType": "application/json", "sha256": strings.Repeat("a", 64), "sizeBytes": 70000, "totalLines": 3600},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceIDs, err := service.Attempts().CompleteToolCall(context.Background(), attempt.ToolResult{
		AttemptID: attemptID, ToolCallID: authorization.ToolCallID,
		Outcome: "succeeded", ResultJSON: string(payload), ArtifactID: artifactID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var bodyArtifactID int64
	var bodyJSON sql.NullString
	if err := db.QueryRow(`SELECT artifact_id,result_json FROM evidence WHERE id=?`, evidenceIDs[0]).Scan(&bodyArtifactID, &bodyJSON); err != nil {
		t.Fatal(err)
	}
	if bodyArtifactID != artifactID || bodyJSON.Valid {
		t.Fatalf("evidence body wrong: artifact=%d json=%v", bodyArtifactID, bodyJSON.Valid)
	}
	var toolArtifactID int64
	if err := db.QueryRow(`SELECT result_artifact_id FROM tool_calls WHERE id=?`, authorization.ToolCallID).Scan(&toolArtifactID); err != nil || toolArtifactID != artifactID {
		t.Fatalf("toolArtifact=%d err=%v", toolArtifactID, err)
	}
}

// TestThanosResultPayloadShapeRejected proves the frozen result schema
// validation (RUNTIME-AGENT-008): a malformed thanos_query payload refuses
// the tool completion and leaves the call running (the runtime then seals
// the technical failure, never a fake observation).
func TestThanosResultPayloadShapeRejected(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-shape")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	if err := service.Attempts().BeginToolCall(context.Background(), attemptID, authorization.ToolCallID); err != nil {
		t.Fatal(err)
	}
	// Truncated=true without an artifact locator violates the frozen shape.
	if _, err := service.Attempts().CompleteToolCall(context.Background(), attempt.ToolResult{
		AttemptID: attemptID, ToolCallID: authorization.ToolCallID,
		Outcome:    "succeeded",
		ResultJSON: `{"success":true,"status":"success","resultType":"matrix","sampleCount":1,"startedAt":"2026-01-01T00:00:00Z","finishedAt":"2026-01-01T00:00:00Z","truncated":true,"totalBytes":10,"totalLines":1,"output":"x"}`,
	}); err == nil {
		t.Fatal("complete must reject a malformed thanos result")
	}
	var toolStatus string
	if err := db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, authorization.ToolCallID).Scan(&toolStatus); err != nil || toolStatus != "running" {
		t.Fatalf("toolStatus=%q err=%v", toolStatus, err)
	}
}
