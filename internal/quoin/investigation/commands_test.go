package investigation

// Create/Send command tests over the real frozen schema: the first-message
// atomicity closure, replay idempotency (HTTP-COMMAND-003), the head fence
// and the single-active-attempt invariant (DATA-INVEST-001/003).

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCreateAtomicFirstTurn(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	result, err := service.Create(ctx, principalID, "cmd-create-0001", "请分析这个告警", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The create transaction closed every row of the first turn at once.
	var head sql.NullInt64
	if err := db.QueryRow(`SELECT current_head_message_id FROM investigations WHERE id=?`, result.InvestigationID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if !head.Valid || head.Int64 != result.MessageID {
		t.Fatalf("head=%v want message %d", head, result.MessageID)
	}
	var role, status, content string
	var attemptRow int64
	var seq int64
	if err := db.QueryRow(`SELECT role,status,content,attempt_id,seq FROM investigation_messages WHERE id=?`, result.MessageID).Scan(&role, &status, &content, &attemptRow, &seq); err != nil {
		t.Fatal(err)
	}
	if role != "user" || status != "active" || content != "请分析这个告警" || attemptRow != result.AttemptID || seq != 1 {
		t.Fatalf("message row wrong: role=%s status=%s seq=%d", role, status, seq)
	}
	var attemptType, scopeType string
	var attemptState string
	if err := db.QueryRow(`SELECT attempt_type,scope_type,state FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&attemptType, &scopeType, &attemptState); err != nil {
		t.Fatal(err)
	}
	if attemptType != "investigation" || scopeType != "investigation" || attemptState != "Queued" {
		t.Fatalf("attempt row wrong: %s/%s/%s", attemptType, scopeType, attemptState)
	}
	var schemaKind, agentVersion string
	if err := db.QueryRow(`SELECT schema_kind, agent_version FROM attempt_input_snapshots s JOIN execution_attempts a ON a.id=s.attempt_id WHERE s.attempt_id=?`, result.AttemptID).Scan(&schemaKind, &agentVersion); err != nil {
		t.Fatal(err)
	}
	if schemaKind != SchemaKind || agentVersion != AgentVersion {
		t.Fatalf("snapshot wrong: %s/%s", schemaKind, agentVersion)
	}
	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items i JOIN attempt_input_snapshots s ON s.id=i.snapshot_id WHERE s.attempt_id=?`, result.AttemptID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 1 {
		t.Fatalf("lineage items=%d want 1", items)
	}
	var grants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'`, result.AttemptID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 1 {
		t.Fatalf("chat_model grants=%d want 1", grants)
	}
	// The frozen digest must be reproducible from the durable lineage.
	if _, err := service.RebuildInput(ctx, result.AttemptID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
}

func TestCreateReplayAndDigestConflict(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	first, err := service.Create(ctx, principalID, "cmd-create-replay", "第一条消息", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Same command id + same digest replays the original result without
	// creating a second investigation (HTTP-COMMAND-003).
	replay, err := service.Create(ctx, principalID, "cmd-create-replay", "第一条消息", nil)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.InvestigationID != first.InvestigationID || replay.MessageID != first.MessageID || replay.AttemptID != first.AttemptID {
		t.Fatalf("replay diverged: %+v vs %+v", replay, first)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("investigations=%d want 1", count)
	}
	// Same command id + different digest is a deterministic conflict.
	if _, err := service.Create(ctx, principalID, "cmd-create-replay", "另一条消息", nil); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("digest conflict err=%v want ErrCommandReused", err)
	}
}

func TestCreateSourceValidation(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	occurrenceID := seedOccurrence(t, db, "T13Probe")

	result, err := service.Create(ctx, principalID, "cmd-create-source", "结合告警排查", []SourceInput{{Type: "occurrence", SourceID: occurrenceID}})
	if err != nil {
		t.Fatalf("create with source: %v", err)
	}
	var links int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigation_source_links WHERE investigation_id=?`, result.InvestigationID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Fatalf("source links=%d want 1", links)
	}
	// The rendered input carries the frozen provenance reference.
	canonical, err := service.RebuildInput(ctx, result.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"type":"occurrence"`) || !strings.Contains(string(canonical), `"alertname":"T13Probe"`) {
		t.Fatalf("input lacks occurrence provenance: %s", canonical)
	}
	// Unknown sources fail deterministically (no partial investigation).
	if _, err := service.Create(ctx, principalID, "cmd-create-bad-source", "x", []SourceInput{{Type: "occurrence", SourceID: 999999}}); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("unknown source err=%v want ErrSourceNotFound", err)
	}
	if _, err := service.Create(ctx, principalID, "cmd-create-bad-type", "x", []SourceInput{{Type: "nonsense", SourceID: 1}}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("bad type err=%v want ErrInvalidSource", err)
	}
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 1 {
		t.Fatalf("failed creates left %d investigations, want 1", after)
	}
}

func TestCreateRequiresContent(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	if _, err := service.Create(ctx, principalID, "cmd-create-blank", "   ", nil); !errors.Is(err, ErrMessageInvalid) {
		t.Fatalf("blank content err=%v want ErrMessageInvalid", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("blank message persisted %d investigations", count)
	}
}

func TestSendHeadFenceAndActiveAttempt(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	first, err := service.Create(ctx, principalID, "cmd-send-1", "第一条", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A stale head conflicts (DATA-INVEST-001).
	_, err = service.Send(ctx, principalID, "cmd-send-2", first.InvestigationID, nil, "第二条")
	var headConflict *HeadConflictError
	if !errors.As(err, &headConflict) {
		t.Fatalf("stale head err=%v want HeadConflictError", err)
	}
	if headConflict.CurrentHead == nil || *headConflict.CurrentHead != first.MessageID {
		t.Fatalf("conflict carries head %v want %d", headConflict.CurrentHead, first.MessageID)
	}
	// The active attempt blocks concurrent sends (DATA-INVEST-003).
	_, err = service.Send(ctx, principalID, "cmd-send-3", first.InvestigationID, int64Ptr(first.MessageID), "第二条")
	if !errors.Is(err, ErrActiveAttempt) {
		t.Fatalf("active attempt err=%v want ErrActiveAttempt", err)
	}
	// After the attempt resolves, the send with the correct head appends
	// the second turn in order.
	if err := closeAttemptSucceeded(t, db, service, first.AttemptID, first.MessageID); err != nil {
		t.Fatal(err)
	}
	expectedHead := secondHead(t, db, first.InvestigationID)
	second, err := service.Send(ctx, principalID, "cmd-send-4", first.InvestigationID, int64Ptr(expectedHead), "第二条")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var seq int64
	if err := db.QueryRow(`SELECT seq FROM investigation_messages WHERE id=?`, second.MessageID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 3 {
		t.Fatalf("second user seq=%d want 3", seq)
	}
	// The second attempt's input freezes both prior turns plus the new one.
	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items i JOIN attempt_input_snapshots s ON s.id=i.snapshot_id WHERE s.attempt_id=?`, second.AttemptID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 3 {
		t.Fatalf("second lineage items=%d want 3", items)
	}
	// Replaying the same send returns the original message.
	replay, err := service.Send(ctx, principalID, "cmd-send-4", first.InvestigationID, int64Ptr(expectedHead), "第二条")
	if err != nil {
		t.Fatalf("send replay: %v", err)
	}
	if replay.MessageID != second.MessageID || replay.AttemptID != second.AttemptID {
		t.Fatalf("send replay diverged: %+v vs %+v", replay, second)
	}
}

func secondHead(t *testing.T, db *sql.DB, investigationID int64) int64 {
	t.Helper()
	var head int64
	if err := db.QueryRow(`SELECT current_head_message_id FROM investigations WHERE id=?`, investigationID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	return head
}

func int64Ptr(value int64) *int64 { return &value }

// closeAttemptSucceeded drives one attempt to the committed terminal state
// through the domain service (assistant message + head move), the same
// closure the runtime result proposal exercises.
func closeAttemptSucceeded(t *testing.T, db *sql.DB, service *Service, attemptID, userMessageID int64) error {
	t.Helper()
	if err := bindRunning(t, db, attemptID); err != nil {
		return err
	}
	if err := seedModelCall(t, db, attemptID); err != nil {
		return err
	}
	content := `"已回复"`
	digest := sha256Sum([]byte(content))
	return service.CommitResult(context.Background(), Result{
		AttemptID: attemptID, BootID: "boot-t", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: digest[:],
	})
}

func bindRunning(t *testing.T, db *sql.DB, attemptID int64) error {
	t.Helper()
	now := testNow()
	lease := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot-t',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, lease, attemptID); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, attemptID); err != nil {
		return err
	}
	return nil
}

// seedModelCall inserts one succeeded chat call with the frozen closure
// (input lineage + sealed response) for an investigation attempt — the
// result seal requires it.
func seedModelCall(t *testing.T, db *sql.DB, attemptID int64) error {
	t.Helper()
	now := testNow()
	var grantID int64
	if err := db.QueryRow(`SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'`, attemptID).Scan(&grantID); err != nil {
		return err
	}
	digest := strings.Repeat("2", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,'1','0','chat','fixture-chat-1',?,?,?,?,?,?,?,?,4096,1024,0,0,'running',?)`,
		attemptID, grantID, RendererVersion, AgentVersion, digest, digest, digest, digest, digest, now)
	if err != nil {
		return err
	}
	callID, _ := call.LastInsertId()
	var snapshotID int64
	if err := db.QueryRow(`SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&snapshotID); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,3,'system',?,?)`, callID, digest, snapshotID); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"已回复","finishReason":"stop","tool_calls":[]}',?,'stop',?)`, callID, digest, now); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":4,"output_tokens":2,"total_tokens":6}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		return err
	}
	return nil
}
