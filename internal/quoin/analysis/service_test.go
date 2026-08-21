package analysis

// Analysis aggregate tests over the real frozen schema: create (one-active
// invariant, command replay), dispatch/accept, result seal with the model
// call closure, retry and the cancel-vs-success commit-order races
// (DATA-ANALYSIS-001/002, DATA-TX-005).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

var seedCounter int

// seedOccurrence inserts one firing alert occurrence.
func seedOccurrence(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	seedCounter++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	source, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`, fmt.Sprintf("source-%d", seedCounter), now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"HighErrorRate","severity":"critical"}`
	occurrence, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(?,?,?,'Firing',?,?,?,?)`,
		sourceID, []byte{0, 0, 0, 0, 0, 0, byte(seedCounter % 8), 1}, now, labels, sha256Hex(labels), now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := occurrence.LastInsertId()
	return id
}

// seedProviderChain inserts one enabled qualified model provider.
func seedProviderChain(t *testing.T, db *sql.DB) (connectionID, revisionID, generationID, probeResultID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var existingConnectionID, existingRevisionID, existingGenerationID, existingProbeID int64
	if err := db.QueryRow(`SELECT c.id, c.current_revision_id, c.current_credential_generation_id, (SELECT probe_result_id FROM connection_enable_qualifications q WHERE q.connection_id=c.id ORDER BY q.id DESC LIMIT 1) FROM connections c WHERE c.type='model_provider' AND c.enabled=1`).Scan(&existingConnectionID, &existingRevisionID, &existingGenerationID, &existingProbeID); err == nil {
		return existingConnectionID, existingRevisionID, existingGenerationID, existingProbeID
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, []byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 16)), now); err != nil {
		t.Fatal(err)
	}
	var actorID int64
	err := db.QueryRow(`SELECT id FROM users LIMIT 1`).Scan(&actorID)
	if err != nil {
		user, err := db.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES('test-admin','Test Admin','admin',1,'x',1,?,?)`, now, now)
		if err != nil {
			t.Fatal(err)
		}
		actorID, _ = user.LastInsertId()
	}
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?,'model_provider',0,?)`, fmt.Sprintf("provider-%d", seedCounter), now)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ = connection.LastInsertId()
	revision, err := db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,?,?)`, connectionID, `{"baseUrl":"https://provider.test","chatModelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}`, now)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, _ = revision.LastInsertId()
	nonce := make([]byte, 12)
	for i := range nonce {
		nonce[i] = byte(seedCounter*31 + i)
	}
	generation, err := db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, nonce, []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ = generation.LastInsertId()
	if _, err := db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		t.Fatal(err)
	}
	var slotState string
	if err := db.QueryRow(`SELECT state FROM runtime_slots WHERE slot='plinth'`).Scan(&slotState); err != nil {
		if _, err := db.Exec(`INSERT INTO runtime_slots(slot,state,created_at) VALUES('plinth','unregistered',?)`, now); err != nil {
			t.Fatal(err)
		}
		slotState = "unregistered"
	}
	if slotState == "unregistered" {
		runtimeCredential, err := db.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,created_at,confirmed_at) VALUES('plinth',1,?,?,?)`, []byte(strings.Repeat("0", 32)), now, now)
		if err != nil {
			t.Fatal(err)
		}
		runtimeCredentialID, _ := runtimeCredential.LastInsertId()
		if _, err := db.Exec(`UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot='plinth' AND state='unregistered'`, runtimeCredentialID); err != nil {
			t.Fatal(err)
		}
	}
	probeAttempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued','test',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	probeAttemptID, _ := probeAttempt.LastInsertId()
	probeSnapshot, err := db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'connection_probe_v1','connection-probe-v1',?,?)`, probeAttemptID, strings.Repeat("0", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	probeSnapshotID, _ := probeSnapshot.LastInsertId()
	if _, err := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'user',?,?)`, probeSnapshotID, strings.Repeat("0", 64), revisionID); err != nil {
		t.Fatal(err)
	}
	var probeChatGrantID, probeEmbeddingGrantID int64
	for _, purpose := range []string{"model_probe_chat", "model_probe_embedding"} {
		grant, err := db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, probeAttemptID, purpose, connectionID, revisionID, generationID, now)
		if err != nil {
			t.Fatal(err)
		}
		if purpose == "model_probe_chat" {
			probeChatGrantID, _ = grant.LastInsertId()
		} else {
			probeEmbeddingGrantID, _ = grant.LastInsertId()
		}
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	probe, err := db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,1,'model-provider-capabilities',1,?,?,?,?,?,?)`,
		probeAttemptID, connectionID, "model_provider", revisionID, generationID, strings.Repeat("0", 64), "passed", strings.Repeat("1", 64), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	probeResultID, _ = probe.LastInsertId()
	seedModelCall(t, db, probeAttemptID, probeChatGrantID, 1, "fixture-chat-1", now, true)
	seedModelCall(t, db, probeAttemptID, probeChatGrantID, 4, "fixture-chat-1", now, false)
	seedEmbeddingCall(t, db, probeAttemptID, probeEmbeddingGrantID, probeSnapshotID, now)
	if _, err := db.Exec(`INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,'fixture-chat-1','fixture-embed-1',4096,1024,1,1,1,1,1,1,1,16,'{}')`, probeResultID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,3,?,?,?)`, connectionID, probeResultID, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=2`, connectionID); err != nil {
		t.Fatal(err)
	}
	return connectionID, revisionID, generationID, probeResultID
}

func seedModelCall(t *testing.T, db *sql.DB, attemptID, grantID int64, callSeq int, modelID, now string, succeeded bool) int64 {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,?,'0','chat',?,?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		attemptID, callSeq, modelID, grantID, digest, digest, digest, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
		t.Fatal(err)
	}
	if succeeded {
		if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?, 'stop',?)`, callID, digest, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := db.Exec(`UPDATE model_calls SET status='cancelled',termination_reason='cancelled',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
			t.Fatal(err)
		}
	}
	return callID
}

func seedEmbeddingCall(t *testing.T, db *sql.DB, attemptID, grantID, snapshotID int64, now string) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,'6','0','embedding','fixture-embed-1',?,?,?,0,'running',?)`,
		attemptID, grantID, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, callID, digest, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,created_at) VALUES(?,1,'{}',?,?)`, callID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":0,"total_tokens":1}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// sealAgentCall inserts one succeeded chat call with tool-call closure for
// an agent attempt (the succeeded-attempt trigger requires it).
func sealAgentCall(t *testing.T, db *sql.DB, attemptID int64, modelID string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var grantID int64
	if err := db.QueryRow(`SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'`, attemptID).Scan(&grantID); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("2", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,'1','0','chat',?,?,'initial-analysis-renderer-v1','initial-analysis-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		attemptID, modelID, grantID, digest, digest, digest, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	var snapshotID int64
	if err := db.QueryRow(`SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,3,'system',?,?)`, callID, digest, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"分析结论","finishReason":"stop","tool_calls":[]}',?, 'stop',?)`, callID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":10,"output_tokens":4,"total_tokens":14}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
	return callID
}

func TestCreateDispatchAcceptSeal(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)

	created, err := service.Create(ctx, occurrenceID, 1, "cmd-create-1")
	if err != nil {
		t.Fatal(err)
	}
	// The one-active invariant: a second create returns the same record
	// (DATA-ANALYSIS-001).
	replayed, err := service.Create(ctx, occurrenceID, 1, "cmd-create-2")
	if err != nil || replayed.AnalysisID != created.AnalysisID {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	detail, err := service.Get(ctx, created.AnalysisID)
	if err != nil || detail.State != "Queued" || detail.AttemptCount != 1 {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	// Rebuild reproduces the frozen digest and dispatch binds.
	if _, err := service.RebuildInput(ctx, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	input, err := service.Attempts().DispatchInputFor(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if input.SchemaKind != "initial_analysis_v1" {
		t.Fatalf("schema=%q", input.SchemaKind)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, created.AnalysisID)
	if detail.State != "Running" {
		t.Fatalf("analysis state=%q", detail.State)
	}
	// The succeeded closure requires a succeeded model call.
	sealAgentCall(t, db, created.AttemptID, "fixture-chat-1")
	content, _ := json.Marshal("这是初步分析结论。")
	digest := sha256.Sum256(content)
	err = service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, created.AnalysisID)
	if detail.State != "Succeeded" || detail.Output == nil || detail.Output.Content != "这是初步分析结论。" {
		t.Fatalf("detail=%+v", detail)
	}
	// An identical replay of the sealed result is idempotent (T12,
	// RUNTIME-TASK-008: the runtime retries its terminal proposal until
	// an ack survives; the retry observes the original verdict).
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); err != nil {
		t.Fatalf("identical replay rejected: %v", err)
	}
	// A divergent late result after success still loses (DATA-TX-005).
	otherContent, _ := json.Marshal("另一个迟到的结论")
	otherDigest := sha256.Sum256(otherContent)
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: otherContent, Digest: otherDigest[:]}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("late result=%v", err)
	}
	// Re-analysis creates a NEW analysis after success.
	again, err := service.Create(ctx, occurrenceID, 1, "cmd-create-3")
	if err != nil || again.AnalysisID == created.AnalysisID {
		t.Fatalf("re-analysis=%+v err=%v", again, err)
	}
}

func TestRetryAfterFailure(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, occurrenceID, 1, "cmd-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: false, Termination: "timeout"}); err != nil {
		t.Fatal(err)
	}
	detail, _ := service.Get(ctx, created.AnalysisID)
	if detail.State != "Failed" {
		t.Fatalf("state=%q", detail.State)
	}
	// The frozen schema closes Failed as terminal (see the T10 amendment
	// comment): same-analysis retry answers the deterministic conflict;
	// the retry reopen arrives with T12's contract amendment.
	if _, err := service.Retry(ctx, created.AnalysisID, 1, "cmd-retry-1"); !errors.Is(err, ErrActiveConflict) {
		t.Fatalf("retry after terminal failure=%v", err)
	}
}

func TestCancelVsSuccessCommitOrder(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, occurrenceID, 1, "cmd-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	// Cancel-before-result: the fence commits, the late result loses.
	detail, _ := service.Get(ctx, created.AnalysisID)
	outcome, err := service.Cancel(ctx, created.AnalysisID, 1, detail.RowVersion, "cmd-cancel-1")
	if err != nil || outcome.State != "Running" {
		t.Fatalf("cancel outcome=%+v err=%v", outcome, err)
	}
	if !outcome.DispatchRequired || outcome.AttemptID != created.AttemptID {
		t.Fatalf("running cancel must require a runtime dispatch: %+v", outcome)
	}
	content, _ := json.Marshal("迟到结果")
	digest := sha256.Sum256(content)
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("late success=%v", err)
	}
	if err := service.CancelAck(ctx, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, created.AnalysisID)
	if detail.State != "Cancelled" {
		t.Fatalf("state=%q", detail.State)
	}

	// Success-before-cancel: the seal wins and the fence answers the
	// completed object (HTTP-COMMAND-005).
	again, err := service.Create(ctx, occurrenceID, 1, "cmd-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, again.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, again.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	sealAgentCall(t, db, again.AttemptID, "fixture-chat-1")
	content, _ = json.Marshal("成功结果")
	digest = sha256.Sum256(content)
	if err := service.CommitResult(ctx, Result{AttemptID: again.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, again.AnalysisID)
	outcome, err = service.Cancel(ctx, again.AnalysisID, 1, detail.RowVersion, "cmd-cancel-2")
	if err != nil || outcome.State != "Succeeded" {
		t.Fatalf("cancel after success=%+v err=%v", outcome, err)
	}
	// A stale row version conflicts (a fresh command id with the outdated
	// version); a current version answers the completed object instead.
	if _, err := service.Cancel(ctx, again.AnalysisID, 1, detail.RowVersion-1, "cmd-cancel-3"); err == nil {
		t.Fatal("stale row version must conflict")
	}

	// A Queued attempt closes as Cancelled directly: no runtime dispatch
	// (DATA-ATTEMPT-003).
	queued, err := service.Create(ctx, occurrenceID, 1, "cmd-queued")
	if err != nil {
		t.Fatal(err)
	}
	queuedDetail, _ := service.Get(ctx, queued.AnalysisID)
	outcome, err = service.Cancel(ctx, queued.AnalysisID, 1, queuedDetail.RowVersion, "cmd-cancel-queued")
	if err != nil || outcome.State != "Cancelled" || outcome.DispatchRequired {
		t.Fatalf("queued cancel=%+v err=%v", outcome, err)
	}
	if _, err := service.Get(ctx, queued.AnalysisID); err != nil || outcome.AttemptID != queued.AttemptID {
		t.Fatalf("queued cancel attempt=%d err=%v", outcome.AttemptID, err)
	}
}

var _ = attempt.ErrLateResult
