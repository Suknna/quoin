package app

// Narrow coverage for the reconcile recovery-loss freeze (runtime_reconcile.go):
// the recovery write commits inside one execution.Execute transaction that
// also carries its audit event under the attempt's restored durable scope
// (attempt.LoadCorrelation: original correlation, inherited initiator, system
// actor), a failure rolls the whole mutation back leaving zero durable trace,
// and an already-frozen attempt is a proven no-op.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	_ "modernc.org/sqlite"
)

// newReconcileFixture builds the minimal frozen-schema fixture for the
// recovery-loss freeze: one Running investigation attempt (id 2) with a
// persisted correlation. The frozen lifecycle is executed in order: created
// Queued, frozen input + model grant, dispatched Assigned, accepted Running.
func newReconcileFixture(t *testing.T) (*sql.DB, *RuntimeService) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/reconcile.db?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	lease := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano)
	d64 := fmt.Sprintf("%064x", 1)
	mustExec(t, db, `INSERT INTO source_materials(id,kind,digest,size_bytes,content,created_at) VALUES(1,'knowledge_import',?,0,'fixture',?)`, d64, now)
	seedQualifiedModelProvider(t, db, now, lease)
	mustExec(t, db, `INSERT INTO investigations(id,created_at) VALUES(9,?)`, now)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,operation_correlation_id,initiator_type,initiator_id,created_at)
		VALUES(2,'investigation','investigation',9,'Queued','q','investigation-v3','corr-reconcile-original','user',1,?)`, now)
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(2,2,'investigation_v1','v1',?,?)`, d64, now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,source_material_id) VALUES(2,1,'source',?,1)`, d64)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(3,2,'chat_model',1,1,1,1,?)`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='plinth-boot',connection_epoch=1,lease_until=?,runtime_release_version='p',row_version=row_version+1 WHERE id=2`, lease)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=2`, now, now)
	analyses := analysis.NewService(db)
	service := &RuntimeService{Analyses: analyses, writer: db}
	return db, service
}

// seedQualifiedModelProvider builds the enabled model-provider chain the
// investigation's chat_model grant closure trigger verifies (root key binding
// → connection → revision → credential generation → passed typed probe with
// real chat calls → explicit enable qualification → enable), using the same
// frozen lifecycle fences the product path enforces.
func seedQualifiedModelProvider(t *testing.T, db *sql.DB, now, lease string) {
	t.Helper()
	d64 := fmt.Sprintf("%064x", 1)
	d2 := fmt.Sprintf("%064x", 2)
	mustExec(t, db, `INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'x',?,?)`, now, now)
	mustExec(t, db, `INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now)
	mustExec(t, db, `INSERT INTO connections(id,name,type,enabled,row_version,revalidation_required,created_at) VALUES(1,'fixture-model','model_provider',0,1,0,?)`, now)
	mustExec(t, db, `INSERT INTO connection_revisions(id,connection_id,revision_seq,config_json,created_at) VALUES(1,1,1,'{"type":"model_provider","baseUrl":"https://model.example","chatModelId":"chat","contextBudgetTokens":1024,"maxOutputTokens":256}',?)`, now)
	mustExec(t, db, `INSERT INTO credential_generations(id,connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(1,1,1,1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now)
	mustExec(t, db, `UPDATE connections SET current_revision_id=1,current_credential_generation_id=1,row_version=2 WHERE id=1`)
	// The supervisor probe attempt with its exact model_probe_chat binding.
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES(800,'connection_probe','connection',1,'Queued','q',?)`, now)
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(800,800,'connection_probe_v1','v1',?,?)`, d64, now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(800,1,'connection_revision',?,1)`, d64)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(800,800,'model_probe_chat',1,1,1,?)`, now)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(801,800,'model_probe_embedding',1,1,1,?)`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='probe-boot',connection_epoch=1,lease_until=?,runtime_release_version='p',row_version=row_version+1 WHERE id=800`, lease)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=800`, now, now)
	// The deterministic provider probe observation: one succeeded chat call
	// (complete output plus chat contract input lineage) and one cancelled chat
	// call — the typed probe child's closure triggers demand exactly these.
	mustExec(t, db, `INSERT INTO model_calls(id,attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at)
		VALUES(801,800,1,0,'chat','chat',800,'v1','probe-v1',?,'v1',?,?,?,1024,256,0,'running',?)`, d64, d64, d64, d64, d64, now)
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(801,1,'system',?,'system_contract'),(801,2,'system',?,'tool_schema')`, d64, d64)
	mustExec(t, db, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(801,1,'{"assistantText":"","finishReason":"stop","tool_calls":[]}',?,'stop',?)`, d2, now)
	mustExec(t, db, `UPDATE model_calls SET status='succeeded',ended_at=?,usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}' WHERE id=801`, now)
	mustExec(t, db, `INSERT INTO model_calls(id,attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at)
		VALUES(802,800,2,0,'chat','chat',800,'v1','probe-v1',?,'v1',?,?,?,1024,256,0,'running',?)`, d64, d64, d64, d64, d64, now)
	mustExec(t, db, `UPDATE model_calls SET status='cancelled',ended_at=?,termination_reason='cancelled' WHERE id=802`, now)
	// The immutable passed probe result, its typed model-provider child, the
	// explicit enable qualification and the enable itself.
	mustExec(t, db, `INSERT INTO connection_probe_results(id,attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(1,800,1,'model_provider',1,1,1,'fixture',1,?,'passed',?,?,?,?)`, d2, d2, now, now, now)
	mustExec(t, db, `INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,detail_json) VALUES(1,'chat',1024,256,1,1,1,1,1,1,0,'{}')`)
	mustExec(t, db, `INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(1,3,1,1,?)`, now)
	mustExec(t, db, `UPDATE connections SET enabled=1,row_version=3 WHERE id=1`)
}

// reconcileScan reads one multi-column fixture row.
func reconcileScan(t *testing.T, db *sql.DB, query string, targets ...any) {
	t.Helper()
	if err := db.QueryRow(query).Scan(targets...); err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
}

// abortAuditForAction installs a trigger that fails the automatic audit write
// of one reconcile action inside the runner transaction, so the business
// stage's committed-in-transaction writes hit the audit failure rollback.
func abortAuditForAction(t *testing.T, db *sql.DB, action string) {
	t.Helper()
	mustExec(t, db, fmt.Sprintf(`CREATE TRIGGER reconcile_audit_abort BEFORE INSERT ON audit_events
		WHEN NEW.action='%s' BEGIN SELECT RAISE(ABORT, 'audit aborted by test'); END`, action))
}

func dropAuditAbort(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP TRIGGER IF EXISTS reconcile_audit_abort`)
}

func assertNoAuditEvent(t *testing.T, db *sql.DB, action string) {
	t.Helper()
	var count int
	mustQuery(t, db, `SELECT COUNT(*) FROM audit_events WHERE action=?`, &count, action)
	if count != 0 {
		t.Fatalf("action %s recorded %d audit events, want 0", action, count)
	}
}

// A recovery-loss freeze commits the immutable pending row and its audit as
// one transaction. The audit carries the attempt's ORIGINAL persisted
// correlation with the inherited initiator and the system actor, and the
// attempt row itself is untouched. A repeat freeze is the proven no-op of the
// immutable row: no duplicate row and no duplicate audit event.
func TestFreezeRecoveryLossPendingCommitsOriginalCorrelation(t *testing.T) {
	db, service := newReconcileFixture(t)
	if err := service.freezeRecoveryLossPending(context.Background(), 2, "lease_expired"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	var source, target, reason string
	reconcileScan(t, db, `SELECT source,target_state,terminal_reason FROM pending_attempt_terminals WHERE attempt_id=2`, &source, &target, &reason)
	if source != "recovery_loss" || target != "Interrupted" || reason != "lease_expired" {
		t.Fatalf("pending row=%s/%s/%s", source, target, reason)
	}
	var actorType string
	var actorID int64
	var correlation, initiatorType string
	var initiatorID, refID int64
	var refType, outcome string
	reconcileScan(t, db, `SELECT actor_type,actor_id,correlation_id,initiator_type,initiator_id,domain_ref_type,domain_ref_id,outcome FROM audit_events WHERE action='`+opNameRecoveryLossPending+`'`, &actorType, &actorID, &correlation, &initiatorType, &initiatorID, &refType, &refID, &outcome)
	if actorType != "system" || actorID != 0 || correlation != "corr-reconcile-original" || initiatorType != "user" || initiatorID != 1 || refType != "execution_attempt" || refID != 2 || outcome != "success" {
		t.Fatalf("audit identity actor=%s/%d correlation=%s initiator=%s/%d ref=%s/%d outcome=%s", actorType, actorID, correlation, initiatorType, initiatorID, refType, refID, outcome)
	}
	var targetType string
	var targetID int64
	reconcileScan(t, db, `SELECT t.target_type,t.target_id FROM audit_event_targets t JOIN audit_events e ON e.id=t.audit_event_id WHERE e.action='`+opNameRecoveryLossPending+`'`, &targetType, &targetID)
	if targetType != "execution_attempt" || targetID != 2 {
		t.Fatalf("audit target=%s/%d", targetType, targetID)
	}
	var attemptVersion int
	mustQuery(t, db, `SELECT row_version FROM execution_attempts WHERE id=2`, &attemptVersion)
	if attemptVersion != 3 {
		t.Fatalf("freeze rewrote the attempt row: row_version=%d", attemptVersion)
	}
	// The second call skips silently: the committed pending row is immutable,
	// so re-freezing is a proven no-op and must not re-audit on every sweep.
	if err := service.freezeRecoveryLossPending(context.Background(), 2, "lease_expired"); err != nil {
		t.Fatalf("repeat freeze: %v", err)
	}
	var pendingCount, eventCount int
	mustQuery(t, db, `SELECT COUNT(*) FROM pending_attempt_terminals WHERE attempt_id=2 AND source='recovery_loss'`, &pendingCount)
	mustQuery(t, db, `SELECT COUNT(*) FROM audit_events WHERE action=?`, &eventCount, opNameRecoveryLossPending)
	if pendingCount != 1 || eventCount != 1 {
		t.Fatalf("repeat freeze duplicated durable state: rows=%d events=%d", pendingCount, eventCount)
	}
}

// A legacy correlation-less attempt runs the freeze under its own fresh task
// identity — never with a fabricated or borrowed correlation — while keeping
// the system actor.
func TestFreezeRecoveryLossPendingLegacyAttemptUsesFreshIdentity(t *testing.T) {
	db, service := newReconcileFixture(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	lease := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano)
	mustExec(t, db, `INSERT INTO investigations(id,created_at) VALUES(10,?)`, now)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES(3,'investigation','investigation',10,'Queued','q','investigation-v3',?)`, now)
	// The dispatch fence requires the same frozen input and model grant.
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(3,3,'investigation_v1','v1',?,?)`, fmt.Sprintf("%064x", 2), now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,source_material_id) VALUES(3,1,'source',?,1)`, fmt.Sprintf("%064x", 2))
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(4,3,'chat_model',1,1,1,1,?)`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='plinth-boot',connection_epoch=1,lease_until=?,runtime_release_version='p',row_version=row_version+1 WHERE id=3`, lease)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=3`, now, now)
	if err := service.freezeRecoveryLossPending(context.Background(), 3, "lease_expired"); err != nil {
		t.Fatalf("freeze legacy attempt: %v", err)
	}
	var correlation, initiatorType string
	var initiatorID int64
	reconcileScan(t, db, `SELECT correlation_id,initiator_type,initiator_id FROM audit_events WHERE action='`+opNameRecoveryLossPending+`'`, &correlation, &initiatorType, &initiatorID)
	if correlation == "" || correlation == "corr-reconcile-original" || initiatorType != "system" || initiatorID != 0 {
		t.Fatalf("legacy freeze identity correlation=%q initiator=%s/%d", correlation, initiatorType, initiatorID)
	}
}

// An audit failure inside the Execute transaction rolls the recovery-loss
// freeze back completely: no pending row, no audit record. Removing the
// failure recovers the freeze on the next convergence pass.
func TestFreezeRecoveryLossPendingRollsBackOnAuditFailure(t *testing.T) {
	db, service := newReconcileFixture(t)
	abortAuditForAction(t, db, opNameRecoveryLossPending)
	if err := service.freezeRecoveryLossPending(context.Background(), 2, "lease_expired"); err == nil {
		t.Fatal("freeze survived its own audit failure")
	}
	assertNoAuditEvent(t, db, opNameRecoveryLossPending)
	var pendingCount int
	mustQuery(t, db, `SELECT COUNT(*) FROM pending_attempt_terminals WHERE attempt_id=2`, &pendingCount)
	if pendingCount != 0 {
		t.Fatalf("rollback left %d pending rows", pendingCount)
	}
	dropAuditAbort(t, db)
	if err := service.freezeRecoveryLossPending(context.Background(), 2, "lease_expired"); err != nil {
		t.Fatalf("freeze after failure cleared: %v", err)
	}
	var source string
	mustQuery(t, db, `SELECT source FROM pending_attempt_terminals WHERE attempt_id=2`, &source)
	if source != "recovery_loss" {
		t.Fatalf("recovered freeze row source=%s", source)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

func mustQuery(t *testing.T, db *sql.DB, query string, target any, args ...any) {
	t.Helper()
	if err := db.QueryRow(query, args...).Scan(target); err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
}
