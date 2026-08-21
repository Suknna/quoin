package attempt

// State machine and fence tests over the real frozen schema (WAL SQLite,
// single writer): dispatch binding, acceptance, result-vs-cancel commit
// ordering and the model-call ledger fencing.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"fmt"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	_ "modernc.org/sqlite"
)

func sha256HexString(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

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

// seedAttempt inserts one minimal Queued initial-analysis attempt with an
// input snapshot, input items and a chat_model grant, returning the
// attempt id and the analysis id.
var seedCounter int

func seedAttempt(t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()
	seedCounter++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	source, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`, fmt.Sprintf("test-source-%d-%d", seedCounter, time.Now().UnixNano()), now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	occurrence, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(?,?,?,'Firing','{}',?,?,?)`,
		sourceID, []byte{0, 0, 0, 0, 0, 0, 0, 1}, now, strings.Repeat("c", 64), now, now)
	if err != nil {
		t.Fatal(err)
	}
	occurrenceID, _ := occurrence.LastInsertId()
	analysis, err := db.Exec(`INSERT INTO initial_analyses(occurrence_id,state,input_snapshot_digest,created_by,created_at) VALUES(?,'Queued',?,NULL,?)`, occurrenceID, strings.Repeat("a", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	analysisID, _ := analysis.LastInsertId()
	attempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at) VALUES('initial_analysis','analysis',?,'Queued','test','initial-analysis-v1',?)`, analysisID, now)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, _ := attempt.LastInsertId()
	snapshot, err := db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'initial_analysis_v1','initial-analysis-renderer-v1',?,?)`, attemptID, testDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, _ := snapshot.LastInsertId()
	if _, err := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,occurrence_id) VALUES(?,1,'user',?,?)`, snapshotID, strings.Repeat("d", 64), occurrenceID); err != nil {
		t.Fatal(err)
	}
	connectionID, revisionID, generationID, probeResultID := seedProviderChain(t, db)
	if _, err := db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(?,'chat_model',?,?,?,?,?)`, attemptID, connectionID, revisionID, generationID, probeResultID, now); err != nil {
		t.Fatal(err)
	}
	return attemptID, analysisID
}

// testDigest is the frozen snapshot digest for the test input
// (sha256(testInputJSON)); the rebuilder must reproduce the same bytes.
var testDigest = sha256HexString([]byte(`{"occurrence":{"id":"1","state":"Firing","firstSeenAt":"2026-01-01T00:00:00Z","lastStateChangeAt":"2026-01-01T00:00:00Z","labels":{"alertname":"TestAlert"}},"modelContract":{"modelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}}`))

// seedProviderChain inserts one enabled qualified model provider.
func seedProviderChain(t *testing.T, db *sql.DB) (connectionID, revisionID, generationID, probeResultID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
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
	// Only one enabled model provider may exist (frozen partial index);
	// reuse the first one when a later seed runs in the same database.
	var existingConnectionID, existingRevisionID, existingGenerationID, existingProbeID int64
	if err := db.QueryRow(`SELECT c.id, c.current_revision_id, c.current_credential_generation_id, (SELECT probe_result_id FROM connection_enable_qualifications q WHERE q.connection_id=c.id ORDER BY q.id DESC LIMIT 1) FROM connections c WHERE c.type='model_provider' AND c.enabled=1`).Scan(&existingConnectionID, &existingRevisionID, &existingGenerationID, &existingProbeID); err == nil {
		return existingConnectionID, existingRevisionID, existingGenerationID, existingProbeID
	}
	// Model providers are created disabled and flip enabled only after the
	// real capability probe (frozen trigger).
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?,'model_provider',0,?)`, fmt.Sprintf("test-provider-%d-%d", seedCounter, time.Now().UnixNano()), now)
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
	probeAttempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued','test',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	probeAttemptID, _ := probeAttempt.LastInsertId()
	// The frozen probe ladder: Queued -> Assigned -> Running -> Succeeded,
	// with the dispatch-ready input snapshot and probe grants first. The
	// dispatch gate also requires a registered plinth slot (created once
	// per database: the registration window only exists on an empty slot).
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
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	// The probe result closes the attempt: insert while Running, then
	// Succeeded (frozen closure trigger).
	probe, err := db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,1,'model-provider-capabilities',1,?,?,?,?,?,?)`,
		probeAttemptID, connectionID, "model_provider", revisionID, generationID, strings.Repeat("0", 64), "passed", strings.Repeat("1", 64), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	probeResultID, _ = probe.LastInsertId()
	// The frozen probe child closure also requires the real call evidence:
	// one succeeded chat call, one cancelled chat call and one succeeded
	// embedding call (the probe action set drives all three).
	embeddingCall, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,'6','0','embedding','fixture-embed-1',?,?,?,0,'running',?)`,
		probeAttemptID, probeEmbeddingGrantID, strings.Repeat("1", 64), strings.Repeat("1", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	embeddingCallID, _ := embeddingCall.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, embeddingCallID, strings.Repeat("1", 64), probeSnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,created_at) VALUES(?,1,'{}',?,?)`, embeddingCallID, strings.Repeat("1", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":0,"total_tokens":1}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, embeddingCallID); err != nil {
		t.Fatal(err)
	}
	// The frozen probe child closure also requires the real call evidence:
	// one succeeded chat call and one cancelled chat call (the probe action
	// set drives both).
	succeededCall, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,'1','0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		probeAttemptID, probeChatGrantID, strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	succeededCallID, _ := succeededCall.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, succeededCallID, strings.Repeat("1", 64), succeededCallID, strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?, 'stop',?)`, succeededCallID, strings.Repeat("1", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, succeededCallID); err != nil {
		t.Fatal(err)
	}
	cancelledCall, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,'4','0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		probeAttemptID, probeChatGrantID, strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	cancelledCallID, _ := cancelledCall.LastInsertId()
	if _, err := db.Exec(`UPDATE model_calls SET status='cancelled',termination_reason='cancelled',ended_at=? WHERE id=? AND status='running'`, now, cancelledCallID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,'fixture-chat-1','fixture-embed-1',4096,1024,1,1,1,1,1,1,1,16,'{}')`, probeResultID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	// Enable with the explicit qualification event (row_version+1 fence).
	if _, err := db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,3,?,?,?)`, connectionID, probeResultID, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=2`, connectionID); err != nil {
		t.Fatal(err)
	}
	return connectionID, revisionID, generationID, probeResultID
}

func TestBindAcceptResultLifecycle(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	attemptID, analysisID := seedAttempt(t, db)
	ctx := context.Background()

	if err := service.BindToStream(ctx, attemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	view, err := service.Get(ctx, attemptID)
	if err != nil || view.State != "Assigned" || view.BootID == nil || *view.BootID != "boot-1" {
		t.Fatalf("state=%v view=%+v err=%v", view.State, view, err)
	}
	// The analysis stays Queued until acceptance.
	var analysisState string
	if err := db.QueryRow(`SELECT state FROM initial_analyses WHERE id=?`, analysisID).Scan(&analysisState); err != nil || analysisState != "Queued" {
		t.Fatalf("analysis state=%q err=%v", analysisState, err)
	}
	// Wrong boot/epoch must refuse acceptance.
	if err := service.Accept(ctx, attemptID, "boot-other", 1); err == nil {
		t.Fatal("acceptance with mismatched boot must fail")
	}
	if err := service.Accept(ctx, attemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	view, _ = service.Get(ctx, attemptID)
	if view.State != "Running" || view.StartedAt == nil {
		t.Fatalf("state=%v", view.State)
	}
	// The failure closure commits at the attempt level (the succeeded
	// closure with model-call evidence belongs to the analysis aggregate).
	if err := service.CommitResult(ctx, attemptID, "boot-1", 1, false, "timeout"); err != nil {
		t.Fatal(err)
	}
	view, _ = service.Get(ctx, attemptID)
	if view.State != "Failed" || view.EndedAt == nil || view.TerminationReason == nil || *view.TerminationReason != "timeout" {
		t.Fatalf("state=%v reason=%v", view.State, view.TerminationReason)
	}
	// Terminal rows are immutable for the commit path.
	if err := service.CommitResult(ctx, attemptID, "boot-1", 1, false, "timeout"); !errors.Is(err, ErrLateResult) {
		t.Fatalf("second commit must be late: %v", err)
	}
}

func TestCancelFenceCommitOrder(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()

	// Cancel-before-result: the fence closes to Cancelling; the late
	// result loses (DATA-TX-005).
	attemptID, _ := seedAttempt(t, db)
	if err := service.BindToStream(ctx, attemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.Accept(ctx, attemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	state, err := service.CancelFence(ctx, attemptID)
	if err != nil || state != "Cancelling" {
		t.Fatalf("fence state=%q err=%v", state, err)
	}
	if err := service.CommitResult(ctx, attemptID, "boot-1", 1, true, ""); !errors.Is(err, ErrLateResult) {
		t.Fatalf("result after fence must be late: %v", err)
	}
	if err := service.CancelAck(ctx, attemptID); err != nil {
		t.Fatal(err)
	}
	view, _ := service.Get(ctx, attemptID)
	if view.State != "Cancelled" || view.TerminationReason == nil || *view.TerminationReason != "cancelled" {
		t.Fatalf("state=%v reason=%v", view.State, view.TerminationReason)
	}
	// The fence is idempotent on terminal rows.
	state, err = service.CancelFence(ctx, attemptID)
	if err != nil || state != "Cancelled" {
		t.Fatalf("idempotent fence state=%q err=%v", state, err)
	}

	// Result-before-cancel: success closes first and the fence answers
	// the completed state.
	attemptID, _ = seedAttempt(t, db)
	if err := service.BindToStream(ctx, attemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.Accept(ctx, attemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitResult(ctx, attemptID, "boot-1", 1, false, "timeout"); err != nil {
		t.Fatal(err)
	}
	state, err = service.CancelFence(ctx, attemptID)
	if err != nil || state != "Failed" {
		t.Fatalf("fence after terminal state=%q err=%v", state, err)
	}

	// Queued attempts cancel directly (no runtime involved).
	attemptID, _ = seedAttempt(t, db)
	state, err = service.CancelFence(ctx, attemptID)
	if err != nil || state != "Cancelled" {
		t.Fatalf("queued fence state=%q err=%v", state, err)
	}
}

func TestQueuedAgentAttemptsAndDispatchInput(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	attemptID, _ := seedAttempt(t, db)
	ids, err := service.QueuedAgentAttempts(ctx, "initial_analysis")
	if err != nil || len(ids) != 1 || ids[0] != attemptID {
		t.Fatalf("queued=%v err=%v", ids, err)
	}
	// The dispatch input rebuild requires a wired rebuilder and must
	// verify the frozen digest.
	service.SnapshotRebuilder = func(ctx context.Context, id int64) ([]byte, error) {
		if id != attemptID {
			t.Fatalf("rebuild called for %d", id)
		}
		return []byte(`{"tampered":true}`), nil
	}
	if _, err := service.DispatchInputFor(ctx, attemptID); err == nil {
		t.Fatal("digest mismatch must fail dispatch")
	}
	service.SnapshotRebuilder = func(ctx context.Context, id int64) ([]byte, error) {
		// Rebuild must reproduce the exact frozen digest (testDigest).
		return []byte(`{"occurrence":{"id":"1","state":"Firing","firstSeenAt":"2026-01-01T00:00:00Z","lastStateChangeAt":"2026-01-01T00:00:00Z","labels":{"alertname":"TestAlert"}},"modelContract":{"modelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}}`), nil
	}
	input, err := service.DispatchInputFor(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if input.SchemaKind != "initial_analysis_v1" || len(input.ContentDigest) != 32 {
		t.Fatalf("input=%+v", input)
	}
}
