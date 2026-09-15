package app

// Narrow coverage for the reconcile recovery mutations (runtime_reconcile.go):
// every recovery write commits inside one execution.Execute transaction that
// also carries its audit event under the attempt's restored durable scope
// (attempt.LoadCorrelation: original correlation, inherited initiator, system
// actor), a failure rolls the whole mutation back leaving zero durable trace,
// and outbound dispatches happen only after the durable commit.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/browser"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// newReconcileFixture builds the minimal frozen-schema fixture for the
// reconcile recovery mutations: one Running investigation attempt (id 2) with
// a persisted correlation, the browser identity chain that exploration
// operations require, and one runnable quoin_browser Tool Call. The seed order
// mirrors the browser exploration scenarios so every frozen trigger stays
// enabled.

func newReconcileFixture(t *testing.T) (*sql.DB, *RuntimeService, func() []*runtimev1.ControlEnvelope) {
	t.Helper()
	ctx := browserAdminContext(t)
	db, reader, rootKeyFile := newBrowserExplorationFixture(t, ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	lease := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano)
	d64 := fmt.Sprintf("%064x", 1)
	mustExec(t, db, `INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'x',?,?)`, now, now)
	mustExec(t, db, `INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,?,1,'test',?,?,?,?)`, make([]byte, 32), now, now, "2036-09-15T00:00:00Z", "2036-09-22T00:00:00Z")
	mustExec(t, db, `INSERT INTO source_materials(id,kind,digest,size_bytes,content,created_at) VALUES(1,'knowledge_import',?,0,'fixture',?)`, d64, now)
	// The published manual-login chain gives exploration operations their
	// required profile generation without dispatching any real browser work.
	mustExec(t, db, `INSERT INTO browser_identity_revisions(id,business_system_id,revision,name,start_url,probe_journey_id,probe_journey_version,probe_params_json,journey_catalog_digest,journey_catalog_version,created_at) VALUES(1,NULL,1,'readonly','https://payments.example','authentication.url-prefix.v1',1,'{}',?,'v1',?)`, d64, now)
	mustExec(t, db, `INSERT INTO browser_identities(id,business_system_id,identity_key,current_revision_id,state,created_at) VALUES(1,NULL,'ops-console',1,'AuthenticationRequired',?)`, now)
	mustExec(t, db, `INSERT INTO browser_operations(id,identity_id,identity_revision_id,kind,actor_user_id,actor_session_id,state,journey_catalog_digest,journey_catalog_version,requested_at) VALUES(1,1,1,'manual_login',1,1,'Queued',?,'v1',?)`, d64, now)
	mustExec(t, db, `UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='lintel-boot',lintel_connection_epoch=7,row_version=row_version+1 WHERE id=1`, now)
	mustExec(t, db, `UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=1`, now)
	mustExec(t, db, `INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,observed_at) VALUES(1,1,'publish',1,'authentication.url-prefix.v1',1,?,'v1','Authenticated',?)`, d64, now)
	mustExec(t, db, `INSERT INTO browser_profile_generations(id,identity_id,identity_revision_id,generation,chromium_revision,profile_manifest_digest,probe_journey_id,probe_journey_version,probe_catalog_digest,probe_catalog_version,published_operation_id,published_by,published_at) VALUES(1,1,1,1,'chrome',?,'authentication.url-prefix.v1',1,?,'v1',1,1,?)`, d64, d64, now)
	mustExec(t, db, `UPDATE browser_operations SET stop_confirmed_at=?,stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=1`, now)
	seedQualifiedModelProvider(t, ctx, db, reader, rootKeyFile, now)
	// The Running investigation attempt carries the persisted association the
	// durable scope restore must read back (ADR-0006). The frozen lifecycle is
	// executed in order: created Queued, frozen input + grant, dispatched
	// Assigned, accepted Running.
	mustExec(t, db, `INSERT INTO investigations(id,created_by,created_at) VALUES(9,1,?)`, now)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,operation_correlation_id,initiator_type,initiator_id,created_at)
		VALUES(2,'investigation','investigation',9,'Queued','q','investigation-v1','corr-reconcile-original','user',1,?)`, now)
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(2,2,'investigation_v1','v1',?,?)`, d64, now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,source_material_id) VALUES(2,1,'source',?,1)`, d64)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(3,2,'chat_model',1,1,1,1,?)`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='plinth-boot',connection_epoch=1,lease_until=?,runtime_release_version='p',row_version=row_version+1 WHERE id=2`, lease)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=2`, now, now)
	arguments := []byte(`{"action":"open","identityKey":"ops-console"}`)
	argumentsDigest := fmt.Sprintf("%x", sha256.Sum256(arguments))
	mustExec(t, db, `INSERT INTO model_calls(id,attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at) VALUES(4,2,1,0,'chat','model',3,'p','investigation-v1',?,'t',?,?,?,10,1,0,'running',?)`, d64, d64, d64, d64, now)
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(4,1,'system',?,'system_contract'),(4,2,'system',?,'tool_schema')`, d64, d64)
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(4,3,'system',?,2)`, d64)
	mustExec(t, db, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(4,1,?,?, 'tool_calls',?)`, `{"assistantText":"","finishReason":"tool_calls","tool_calls":[{"id":"provider-open","name":"quoin_browser","arguments":{"action":"open","identityKey":"ops-console"}}]}`, d64, now)
	mustExec(t, db, `UPDATE model_calls SET status='succeeded',ended_at=?,usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}' WHERE id=4`, now)
	mustExec(t, db, `INSERT INTO tool_calls(id,attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at) VALUES(1,2,4,1,0,'provider-open','quoin_browser','1',?,?, 'quoin_browser','return_to_model','pending',?)`, string(arguments), argumentsDigest, now)
	mustExec(t, db, `UPDATE tool_calls SET status='running',started_at=?,row_version=row_version+1 WHERE id=1`, now)

	var sent []*runtimev1.ControlEnvelope
	slots := qruntime.NewService(db)
	if err := slots.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	slots.AttachStream(qruntime.SlotPlinth, "plinth-boot", 1)
	slots.AttachStream(qruntime.SlotLintel, "lintel-boot", 7)
	analyses := analysis.NewService(db)
	if err := analyses.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	browsers := browser.NewService(db)
	browsers.SetReader(reader)
	service := &RuntimeService{
		Slots: slots, Analyses: analyses, Browsers: browsers, writer: db,
		sendEnvelopeForTest: func(_ string, envelope *runtimev1.ControlEnvelope) error {
			sent = append(sent, envelope)
			return nil
		},
	}
	return db, service, func() []*runtimev1.ControlEnvelope { return sent }
}

// reconcileScan reads one multi-column fixture row.
func reconcileScan(t *testing.T, db *sql.DB, query string, targets ...any) {
	t.Helper()
	if err := db.QueryRow(query).Scan(targets...); err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
}

// seedExplorationOperation inserts one Queued exploration operation for the
// parent attempt; promoting it past Queued is the caller's scenario detail.
func seedExplorationOperation(t *testing.T, db *sql.DB, id, parentID int64, now string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO browser_operations(id,identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,state,journey_catalog_digest,journey_catalog_version,requested_at)
		VALUES(?,1,1,1,?,'exploration','Queued',?,'v1',?)`, id, parentID, fmt.Sprintf("%064x", 1), now)
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
	db, service, _ := newReconcileFixture(t)
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
	db, service, _ := newReconcileFixture(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	lease := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano)
	mustExec(t, db, `INSERT INTO investigations(id,created_by,created_at) VALUES(10,1,?)`, now)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES(3,'investigation','investigation',10,'Queued','q','investigation-v1',?)`, now)
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
	db, service, _ := newReconcileFixture(t)
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

// Unstarted exploration cancellation closes the Queued and NO_CAPACITY
// operations as one audited transaction, and only dispatches the same-boot
// Stop tombstone after the durable commit — for the cancelled start fence
// only. An audit failure rolls the close back and dispatches nothing. The
// schema keeps one active exploration per parent, so the fenced scenario runs
// after the first close reached its terminal state.
func TestCancelUnstartedExplorationsDispatchesOnlyAfterDurableCommit(t *testing.T) {
	db, service, sent := newReconcileFixture(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	seedExplorationOperation(t, db, 10, 2, now)

	abortAuditForAction(t, db, opNameExplorationCancelUnstarted)
	if err := service.cancelUnstartedExplorations(context.Background(), 2); err == nil {
		t.Fatal("cancellation survived its own audit failure")
	}
	var openCount int
	mustQuery(t, db, `SELECT COUNT(*) FROM browser_operations WHERE id=10 AND state='Queued'`, &openCount)
	if openCount != 1 {
		t.Fatal("rollback did not restore the queued operation")
	}
	if len(sent()) != 0 {
		t.Fatalf("failed transaction dispatched %d frames", len(sent()))
	}
	assertNoAuditEvent(t, db, opNameExplorationCancelUnstarted)
	dropAuditAbort(t, db)

	if err := service.cancelUnstartedExplorations(context.Background(), 2); err != nil {
		t.Fatalf("cancellation: %v", err)
	}
	var state, basis string
	reconcileScan(t, db, `SELECT state,stop_confirmation_basis FROM browser_operations WHERE id=10`, &state, &basis)
	if state != "Cancelled" || basis != "not_dispatched" {
		t.Fatalf("queued operation closed as %s/%s", state, basis)
	}
	for _, envelope := range sent() {
		if envelope.GetStopBrowserOperation() != nil {
			t.Fatalf("unfenced close dispatched %+v", envelope.GetStopBrowserOperation())
		}
	}

	// A row another writer already cancelled with its start fence but without
	// any physical stop confirmation still owes its same-boot Stop tombstone.
	// (The NO_CAPACITY close above intentionally records stop_confirmed_at:
	// Lintel proved no Chromium exists, so no Stop frame is necessary.) The
	// drain-only dispatch runs strictly after the durable commit.
	mustExec(t, db, `INSERT INTO browser_operations(id,identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,state,journey_catalog_digest,journey_catalog_version,requested_at)
		VALUES(11,1,1,1,2,'exploration','Queued',?,'v1',?)`, fmt.Sprintf("%064x", 1), now)
	mustExec(t, db, `UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='lintel-boot',lintel_connection_epoch=7,row_version=row_version+1 WHERE id=11`, now)
	mustExec(t, db, `UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='runtime_unavailable',row_version=row_version+1 WHERE id=11`, now)
	if err := service.cancelUnstartedExplorations(context.Background(), 2); err != nil {
		t.Fatalf("tombstone drain: %v", err)
	}
	stops := 0
	for _, envelope := range sent() {
		if stop := envelope.GetStopBrowserOperation(); stop != nil {
			stops++
			if stop.GetOperationId() != 11 {
				t.Fatalf("Stop tombstone for %d, want only the unconfirmed operation 11", stop.GetOperationId())
			}
		}
	}
	if stops != 1 {
		t.Fatalf("dispatched %d Stop tombstones, want exactly 1", stops)
	}
	var correlated, total int
	mustQuery(t, db, `SELECT COUNT(*) FROM audit_events WHERE action=? AND correlation_id='corr-reconcile-original'`, &correlated, opNameExplorationCancelUnstarted)
	mustQuery(t, db, `SELECT COUNT(*) FROM audit_events WHERE action=?`, &total, opNameExplorationCancelUnstarted)
	if total != 2 || correlated != 2 {
		t.Fatalf("cancellation audit correlation coverage %d/%d", correlated, total)
	}
}

// A natural pending parent terminal closes the unstarted operation and its
// queued Tool Call in one audited transaction; the Tool Call trigger remains
// the sole child-state writer. An audit failure rolls both closes back.
func TestClosePendingUnstartedExplorationsRollsBackAtomically(t *testing.T) {
	db, service, _ := newReconcileFixture(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// The frozen recovery-loss work item is what makes the close legitimate.
	mustExec(t, db, `INSERT INTO pending_attempt_terminals(attempt_id,source,target_state,terminal_reason,created_at) VALUES(2,'recovery_loss','Interrupted','lease_expired',?)`, now)
	seedExplorationOperation(t, db, 12, 2, now)
	// The pre-start child of Tool Call 1: queued browser exploration attempt
	// linked through the child binding and the requesting Tool Call.
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,requested_by_tool_call_id,quoin_release_version,created_at)
		VALUES(3,'browser_exploration','investigation',9,'Queued',1,'q',?)`, now)
	mustExec(t, db, `INSERT INTO browser_exploration_child_bindings(child_attempt_id,tool_call_id,operation_id,parent_attempt_id,created_at) VALUES(3,1,12,2,?)`, now)

	abortAuditForAction(t, db, opNameExplorationCloseUnstarted)
	if err := service.closePendingUnstartedExplorations(context.Background()); err == nil {
		t.Fatal("pending close survived its own audit failure")
	}
	var openCount int
	mustQuery(t, db, `SELECT COUNT(*) FROM browser_operations WHERE id=12 AND state='Queued'`, &openCount)
	if openCount != 1 {
		t.Fatal("rollback did not restore the queued operation")
	}
	mustQuery(t, db, `SELECT COUNT(*) FROM tool_calls WHERE id=1 AND status='running'`, &openCount)
	if openCount != 1 {
		t.Fatal("rollback did not restore the queued Tool Call")
	}
	assertNoAuditEvent(t, db, opNameExplorationCloseUnstarted)
	dropAuditAbort(t, db)

	if err := service.closePendingUnstartedExplorations(context.Background()); err != nil {
		t.Fatalf("pending close: %v", err)
	}
	var state, reason string
	reconcileScan(t, db, `SELECT state,terminal_reason FROM browser_operations WHERE id=12`, &state, &reason)
	if state != "Cancelled" || reason != "parent_terminal" {
		t.Fatalf("operation closed as %s/%s", state, reason)
	}
	var status, detail string
	reconcileScan(t, db, `SELECT status,error_detail FROM tool_calls WHERE id=1`, &status, &detail)
	if status != "cancelled" || detail != "parent terminal pending browser cleanup" {
		t.Fatalf("tool call closed as %s/%s", status, detail)
	}
	// trg_tool_calls_close_browser_child is the sole child-state writer.
	var childState, childReason string
	reconcileScan(t, db, `SELECT state,termination_reason FROM execution_attempts WHERE id=3`, &childState, &childReason)
	if childState != "Cancelled" || childReason != "cancelled" {
		t.Fatalf("child closed as %s/%s", childState, childReason)
	}
	var correlation string
	reconcileScan(t, db, `SELECT correlation_id FROM audit_events WHERE action='`+opNameExplorationCloseUnstarted+`'`, &correlation)
	if correlation != "corr-reconcile-original" {
		t.Fatalf("close audit correlation=%q", correlation)
	}
}
