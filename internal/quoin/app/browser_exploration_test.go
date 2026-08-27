package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/browser"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/investigation"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBrowserExplorationActionAcceptanceAckBindsExactResult(t *testing.T) {
	var sent *runtimev1.ControlEnvelope
	service := &RuntimeService{sendEnvelopeForTest: func(slot string, envelope *runtimev1.ControlEnvelope) error {
		if slot != qruntime.SlotLintel {
			t.Fatalf("ack slot=%q", slot)
		}
		sent = envelope
		return nil
	}}
	result := &runtimev1.BrowserExplorationActionResult{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, ParentAttemptId: 7, ErrorCode: "Cancelled", ErrorDetail: "parent cancelled", SessionTerminal: true}
	service.acknowledgeBrowserExplorationAction(&runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 8, MessageId: 91}, result, true, "committed")
	ack := sent.GetBrowserExplorationActionResultAck()
	if ack == nil || !ack.GetAccepted() || ack.GetOperationId() != result.GetOperationId() || ack.GetChildAttemptId() != result.GetChildAttemptId() || ack.GetToolCallId() != result.GetToolCallId() || sent.GetBootId() != "lintel-boot" || sent.GetConnectionEpoch() != 8 || sent.GetCorrelationId() != 91 {
		t.Fatalf("invalid action acceptance ack: %#v envelope=%#v", ack, sent)
	}
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(body)
	if !bytes.Equal(ack.GetResultDigest(), want[:]) {
		t.Fatalf("ack digest=%x want=%x", ack.GetResultDigest(), want)
	}
}

func TestStartRejectReasonMapsDownloadBlockedToModelResult(t *testing.T) {
	if got := startRejectReason(runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_DOWNLOAD_BLOCKED); got != "download_blocked" {
		t.Fatalf("DownloadBlocked mapped to %q", got)
	}
	payload := browserAdmissionPayload("DownloadBlocked", nil)
	if err := attempt.ValidateToolResultPayload("browser_tool_result_v1", payload); err != nil || !bytes.Contains(payload, []byte(`"code":"DownloadBlocked"`)) || !bytes.Contains(payload, []byte(`"outcome":"session_closed"`)) || !bytes.Contains(payload, []byte(`"retryableInSession":false`)) {
		t.Fatalf("download admission payload=%s err=%v", payload, err)
	}
}

func TestIdentityBusyAdmissionPayloadCarriesDurableOccupancy(t *testing.T) {
	payload := browserAdmissionPayload("IdentityBusy", &identityBusy{OperationID: 17, Kind: "deployment_verification", OccupiedAt: "2026-03-16T12:00:00Z"})
	if err := attempt.ValidateToolResultPayload("browser_tool_result_v1", payload); err != nil {
		t.Fatalf("IdentityBusy payload violates frozen schema: %v: %s", err, payload)
	}
	var result struct {
		Outcome string `json:"outcome"`
		Error   struct {
			Retryable bool `json:"retryableInSession"`
			Busy      struct {
				OperationID int64  `json:"currentOperationId"`
				Kind        string `json:"operationKind"`
			} `json:"identityBusy"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &result); err != nil || result.Outcome != "recoverable_error" || result.Error.Retryable || result.Error.Busy.OperationID != 17 || result.Error.Busy.Kind != "deployment_verification" {
		t.Fatalf("IdentityBusy admission shape=%s err=%v", payload, err)
	}
}

func TestBrowserSubExecutionRejectsInvalidInputBeforePersistence(t *testing.T) {
	var reply *runtimev1.ControlEnvelope
	service := &RuntimeService{sendEnvelopeForTest: func(slot string, envelope *runtimev1.ControlEnvelope) error {
		if slot != qruntime.SlotPlinth {
			t.Fatalf("slot=%q", slot)
		}
		reply = envelope
		return nil
	}}
	service.handleBrowserSubExecution(context.Background(), &runtimev1.ControlEnvelope{CorrelationId: 7}, &runtimev1.RequestBrowserSubExecution{
		ParentAttemptId: 1, ToolCallId: 2,
		Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: []byte(`{"action":"evaluate","javascript":"alert(1)"}`)},
	})
	if reply == nil || reply.GetBrowserSubExecutionAck() == nil {
		t.Fatal("missing BrowserSubExecutionAck")
	}
	ack := reply.GetBrowserSubExecutionAck()
	if ack.GetAccepted() || ack.GetRejectReason() != runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED {
		t.Fatalf("ack=%+v", ack)
	}
}

func TestBrowserOpenPromotionPersistsFrozenChildInputSnapshot(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "success")
}

// An action ledger is not allowed to invent page/origin facts from Tool-call
// fields. This adversarial result is schema-shaped except for the missing
// current page and must be rejected before it can seal an action audit.
func TestBrowserObservationAuditRejectsMissingCurrentPage(t *testing.T) {
	payload := []byte(`{"action":"read","outcome":"success","sessionId":"1","observation":{"version":7,"url":"https://example.test/","origin":"https://example.test","title":"Example","pages":[],"visibleText":"","accessibilityText":"","elements":[],"events":[],"originalSizeBytes":0,"truncated":false}}`)
	if _, _, _, _, _, err := browserObservationAudit(payload); err == nil {
		t.Fatal("missing current page was accepted as an action audit")
	}
}
func TestBrowserExplorationArtifactCommitFailed(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "artifact")
}

func TestBrowserExplorationFailedClosePersistsCompletionProbe(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "artifact_probe")
}

func TestBrowserExplorationFailedAdmissionPersistsProbe(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "admission_failed")
}
func TestBrowserExplorationCrashFirst(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "crash")
}

func TestBrowserExplorationLateCrashCannotOverwriteCommittedClose(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "close_then_crash")
}

func TestBrowserExplorationIdleCrashArtifactCommitFailureClosesOperation(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "idle_artifact")
}

func TestBrowserOpenAdmissionRejectionClosesChildWithoutOperationOrAction(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "admission")
}

func TestLintelReconnectRedispatchesExactRunningExplorationAction(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "reconnect")
}

// The immutable action dispatch is fenced to its start epoch, but an exactly
// matching result replayed by the same Lintel boot on a successor connection
// must still be accepted. An earlier epoch remains stale.
func TestBrowserExplorationAcceptsSuccessorEpochActionResult(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "successor_epoch")
}

func TestBrowserExplorationRejectsConflictingActionResultReplay(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "conflicting_replay")
}

// Exercises cancellation against the second, already Running action under the
// complete frozen SQLite schema. The table drives duplicate/result/crash-style
// replays through the same idempotent terminal result fence.
func TestBrowserExplorationParentCancellationMatrix(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "cancel")
}

func TestBrowserExplorationCancellationArtifactCommitFailureCloses(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "cancel_artifact")
}

// The parent cancellation is committed before Lintel installs the mandatory
// terminal trace and sends its ActionResult. The fixture therefore proves the
// real transaction order: the later artifact/result closes the browser action,
// while the already-cancelled Tool Call and parent remain authoritative.
func TestBrowserExplorationParentCancelThenTraceCommitThenActionResult(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "cancel")
}

// A complete trace Artifact can commit after a claim while its ActionResult is
// still in flight. A parent cancellation that commits in that interval owns the
// terminal outcome: the immutable bytes remain evidence, but the Operation and
// action must be canonical Cancelled/incomplete rather than Succeeded/complete.
func TestBrowserExplorationParentCancellationWinsCommittedCompleteTrace(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "cancel_complete")
}

func TestBrowserExplorationParentCancellationWaitsForRunningOperationWithoutChild(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "cancel_no_child")
}

func TestBrowserExplorationNaturalParentTerminalClosesIdleSession(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "natural_parent")
}

func TestPendingRecoveryTerminalDoesNotOverrideCommittedCompleteTraceClaim(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "pending_claim_complete")
}

func TestBrowserExplorationCancellationDiscardsPendingNaturalTerminal(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "pending_cancel")
}

func TestBrowserExplorationInterruptedParentClosesIdleSession(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "interrupted_parent")
}

func TestBrowserExplorationBootInterruptionSealsSchemaValidActionClosure(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "boot_interrupted")
}

func TestBrowserExplorationIdleCancellationDoesNotSelfLockSingleConnection(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "cancel_idle_single_connection")
}

func TestBrowserExplorationClosedSessionCallCreatesTerminalChildTombstone(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "closed_session")
}

func TestBrowserExplorationParentCancellationClosesQueuedOperationWithoutOwnerlessStart(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "cancel_queued")
}

func TestBrowserExplorationParentCancellationClosesNoCapacityOperationWithItsStartFence(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "cancel_no_capacity")
}

func TestBrowserExplorationParentCancellationKeepsStartingOperationForTraceClosure(t *testing.T) {
	runBrowserExplorationTerminalScenario(t, "cancel_starting")
}

func runBrowserExplorationTerminalScenario(t *testing.T, scenario string) {
	ctx := context.Background()
	db, rootKeyFile := newBrowserExplorationFixture(t, ctx)
	defer db.Close()
	arguments := []byte(`{"action":"open","businessSystemKey":"payments"}`)
	digest := sha256.Sum256(arguments)
	// The fixture uses the complete frozen schema: no browser, Tool Call, or
	// execution Attempt trigger is removed or bypassed.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	d64 := fmt.Sprintf("%064x", 1)
	// The fixture intentionally executes the frozen schema, including its
	// browser_exploration_actions and row-version triggers. Foreign-key checks
	// are disabled only to avoid unrelated model-provider bootstrap records.
	mustExec(t, db, `INSERT INTO business_systems(id,key,display_name,enabled,created_at) VALUES(1,'payments','Payments',0,?)`, now)
	mustExec(t, db, `UPDATE business_systems SET enabled=1,row_version=row_version+1 WHERE id=1`)
	mustExec(t, db, `INSERT INTO users(id,username,display_name,role,enabled,password_phc,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'x',?,?)`, now, now)
	mustExec(t, db, `INSERT INTO source_materials(id,kind,digest,size_bytes,content,created_at) VALUES(1,'knowledge_import',?,0,'fixture',?)`, d64, now)
	seedQualifiedModelProvider(t, ctx, db, rootKeyFile, now)
	mustExec(t, db, `INSERT INTO browser_identity_revisions(id,business_system_id,revision,name,start_url,probe_journey_id,probe_journey_version,probe_params_json,journey_catalog_digest,journey_catalog_version,created_at) VALUES(1,1,1,'readonly','https://payments.example','authentication.url-prefix.v1',1,'{}',?,'v1',?)`, d64, now)
	mustExec(t, db, `INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,?,1,'test',?,?,?,?)`, make([]byte, 32), now, now, now, now)
	mustExec(t, db, `INSERT INTO browser_identities(id,business_system_id,current_revision_id,state,created_at) VALUES(1,1,1,'AuthenticationRequired',?)`, now)
	mustExec(t, db, `INSERT INTO browser_operations(id,identity_id,identity_revision_id,kind,actor_user_id,actor_session_id,state,journey_catalog_digest,journey_catalog_version,requested_at) VALUES(1,1,1,'manual_login',1,1,'Queued',?,'v1',?)`, d64, now)
	mustExec(t, db, `UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='lintel-boot',lintel_connection_epoch=7,row_version=row_version+1 WHERE id=1`, now)
	mustExec(t, db, `UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=1`, now)
	mustExec(t, db, `INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,observed_at) VALUES(1,1,'publish',1,'authentication.url-prefix.v1',1,?,'v1','Authenticated',?)`, d64, now)
	mustExec(t, db, `INSERT INTO browser_profile_generations(id,identity_id,identity_revision_id,generation,chromium_revision,profile_manifest_digest,probe_journey_id,probe_journey_version,probe_catalog_digest,probe_catalog_version,published_operation_id,published_by,published_at) VALUES(1,1,1,1,'chrome',?,'authentication.url-prefix.v1',1,?,'v1',1,1,?)`, d64, d64, now)
	// A terminal browser Operation continues to reserve its Identity until the
	// same-boot cleanup is durably confirmed. Model the completed manual login
	// faithfully so this fixture does not fabricate an IdentityBusy conflict.
	mustExec(t, db, `UPDATE browser_operations SET stop_confirmed_at=?,stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=1`, now)
	mustExec(t, db, `INSERT INTO investigations(id,created_by,created_at) VALUES(9,1,?)`, now)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at) VALUES(2,'investigation','investigation',9,'Queued','q','investigation-v1',?)`, now)
	mustExec(t, db, `INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,client_command_id,created_at) VALUES(9,2,1,'user','active','finish now','natural-parent-terminal',?)`, now)
	mustExec(t, db, `UPDATE investigations SET current_head_message_id=(SELECT id FROM investigation_messages WHERE attempt_id=2 AND role='user') WHERE id=9`)
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(2,2,'investigation_v1','v1',?,?)`, d64, now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,source_material_id) VALUES(2,1,'source',?,1)`, d64)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(3,2,'chat_model',1,1,1,1,?)`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',connection_epoch=1,boot_id='plinth-boot',lease_until=?,runtime_release_version='p',row_version=row_version+1 WHERE id=2`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=2`, now, now)
	mustExec(t, db, `INSERT INTO model_calls(id,attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at) VALUES(4,2,1,0,'chat','model',3,'p','investigation-v1',?,'t',?,?,?,10,1,0,'running',?)`, d64, d64, d64, d64, now)
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(4,1,'system',?,'system_contract'),(4,2,'system',?,'tool_schema')`, d64, d64)
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(4,3,'system',?,2)`, d64)
	mustExec(t, db, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(4,1,?,?, 'tool_calls',?)`, `{"assistantText":"","finishReason":"tool_calls","tool_calls":[{"id":"provider-open","name":"quoin_browser","arguments":{"action":"open","businessSystemKey":"payments"}}]}`, d64, now)
	mustExec(t, db, `UPDATE model_calls SET status='succeeded',ended_at=?,usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}' WHERE id=4`, now)
	mustExec(t, db, `INSERT INTO tool_calls(id,attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at) VALUES(1,2,4,1,0,'provider-open','quoin_browser','1',?,?, 'quoin_browser','return_to_model','pending',?)`, string(arguments), fmt.Sprintf("%x", digest), now)
	mustExec(t, db, `UPDATE tool_calls SET status='running',started_at=?,row_version=row_version+1 WHERE id=1`, now)

	var sentMu sync.Mutex
	var sent []*runtimev1.ControlEnvelope
	sentSnapshot := func() []*runtimev1.ControlEnvelope {
		sentMu.Lock()
		defer sentMu.Unlock()
		return append([]*runtimev1.ControlEnvelope(nil), sent...)
	}
	resetSent := func() {
		sentMu.Lock()
		sent = nil
		sentMu.Unlock()
	}
	slots := qruntime.NewService(db)
	slots.AttachStream(qruntime.SlotPlinth, "plinth-boot", 1)
	slots.AttachStream(qruntime.SlotLintel, "lintel-boot", 7)
	service := &RuntimeService{
		Slots: slots, Analyses: analysis.NewService(db), Investigations: investigation.NewService(db), Browsers: browser.NewService(db), ReleaseVersion: "q",
		sendEnvelopeForTest: func(_ string, envelope *runtimev1.ControlEnvelope) error {
			sentMu.Lock()
			sent = append(sent, envelope)
			sentMu.Unlock()
			return nil
		},
		browserExplorationSlotView: func(context.Context) (qruntime.SlotView, error) {
			epoch := uint64(7)
			return qruntime.SlotView{Connected: true, BootID: "lintel-boot", ConnectionEpoch: &epoch}, nil
		},
	}
	var identityState string
	mustQuery(t, db, `SELECT state FROM browser_identities WHERE id=1`, &identityState)
	if identityState != "Ready" {
		t.Fatalf("fixture identity state=%s", identityState)
	}
	if err := attempt.ValidateToolArguments(attempt.BrowserTool, arguments); err != nil {
		t.Fatalf("fixture browser args: %v", err)
	}
	request := &runtimev1.RequestBrowserSubExecution{ParentAttemptId: 2, ToolCallId: 1, Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: arguments, ContentDigest: digest[:]}}
	if scenario == "admission" {
		mustExec(t, db, `UPDATE browser_identities SET state='AuthenticationRequired',row_version=row_version+1 WHERE id=1`)
	}
	service.handleBrowserSubExecution(ctx, &runtimev1.ControlEnvelope{BootId: "plinth-boot", ConnectionEpoch: 1, CorrelationId: 1}, request)
	frames := sentSnapshot()
	if len(frames) < 1 || frames[0].GetBrowserSubExecutionAck() == nil || !frames[0].GetBrowserSubExecutionAck().GetAccepted() {
		t.Fatalf("request acknowledgement=%v", frames)
	}
	childID := frames[0].GetBrowserSubExecutionAck().GetChildAttemptId()
	if scenario == "admission" {
		var toolState, childState, resultJSON string
		var actionCount, explorationCount int
		if err := db.QueryRow(`SELECT status,result_json FROM tool_calls WHERE id=1`).Scan(&toolState, &resultJSON); err != nil {
			t.Fatalf("query admission tool result: %v", err)
		}
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &childState, childID)
		mustQuery(t, db, `SELECT COUNT(*) FROM browser_exploration_actions`, &actionCount)
		mustQuery(t, db, `SELECT COUNT(*) FROM browser_operations WHERE kind='exploration'`, &explorationCount)
		if toolState != "succeeded" || childState != "Succeeded" || actionCount != 0 || explorationCount != 0 || attempt.ValidateToolResultPayload("browser_tool_result_v1", []byte(resultJSON)) != nil {
			t.Fatalf("admission closure tool=%s child=%s actions=%d operations=%d result=%s", toolState, childState, actionCount, explorationCount, resultJSON)
		}
		return
	}
	var actionCount, childVersion int
	mustQuery(t, db, `SELECT COUNT(*) FROM browser_exploration_actions`, &actionCount)
	mustQuery(t, db, `SELECT row_version FROM execution_attempts WHERE id=?`, &childVersion, childID)
	if actionCount != 0 || childVersion != 1 {
		t.Fatalf("request must persist only queued child: actions=%d childVersion=%d", actionCount, childVersion)
	}
	if scenario == "cancel_queued" || scenario == "cancel_no_capacity" {
		if scenario == "cancel_no_capacity" {
			// Preserve the immutable first Start fence exactly as a Lintel
			// NO_CAPACITY acknowledgement does; no Chromium process exists.
			mustExec(t, db, `UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='lintel-boot',lintel_connection_epoch=7,row_version=row_version+1 WHERE id=2`, now)
			mustExec(t, db, `UPDATE browser_operations SET state='WaitingForCapacity',row_version=row_version+1 WHERE id=2`)
		}
		if state, err := attempt.NewService(db).CancelFence(ctx, 2); err != nil || state != "Cancelling" {
			t.Fatalf("queued parent cancel state=%q err=%v", state, err)
		}
		service.finalizeCancellation(ctx, 2, "investigation")
		var parent, operation, stopBasis string
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=2`, &parent)
		if err := db.QueryRow(`SELECT state,stop_confirmation_basis FROM browser_operations WHERE id=2`).Scan(&operation, &stopBasis); err != nil {
			t.Fatalf("query queued operation cancellation: %v", err)
		}
		wantBasis := "not_dispatched"
		if scenario == "cancel_no_capacity" {
			wantBasis = "no_capacity"
		}
		if parent != "Cancelled" || operation != "Cancelled" || stopBasis != wantBasis {
			t.Fatalf("unstarted cancellation left possible ownerless start parent=%s operation=%s stop=%s", parent, operation, stopBasis)
		}
		return
	}
	mustExec(t, db, `UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='lintel-boot',lintel_connection_epoch=7,row_version=row_version+1 WHERE id=2`, now)
	if scenario == "cancel_starting" {
		if state, err := attempt.NewService(db).CancelFence(ctx, 2); err != nil || state != "Cancelling" {
			t.Fatalf("starting parent cancel state=%q err=%v", state, err)
		}
		resetSent()
		service.finalizeCancellation(ctx, 2, "investigation")
		frames = sentSnapshot()
		if len(frames) == 0 || frames[0].GetCancelBrowserExplorationAction() == nil {
			t.Fatalf("starting cancellation did not dispatch trace-bearing cancel: %v", frames)
		}
		var operationState string
		mustQuery(t, db, `SELECT state FROM browser_operations WHERE id=2`, &operationState)
		if operationState != "Starting" {
			t.Fatalf("starting cancellation bypassed Lintel trace closure: state=%s", operationState)
		}
		return
	}
	mustExec(t, db, `UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=2`, now)
	// The full frozen dispatch trigger remains enabled: the production path must
	// persist a child snapshot and one immutable inherited lineage item before
	// this Queued → Assigned promotion can succeed.
	if err := service.dispatchBrowserExplorationAction(ctx, childID); err != nil {
		t.Fatalf("frozen dispatch: %v", err)
	}
	var schemaKind, state string
	var itemCount int
	mustQuery(t, db, `SELECT schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, &schemaKind, childID)
	mustQuery(t, db, `SELECT COUNT(*) FROM attempt_input_items i JOIN attempt_input_snapshots s ON s.id=i.snapshot_id WHERE s.attempt_id=?`, &itemCount, childID)
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, childID)
	mustQuery(t, db, `SELECT COUNT(*) FROM browser_exploration_actions WHERE child_attempt_id=?`, &actionCount, childID)
	var boundOperationID, boundParentID, boundToolID int64
	if err := db.QueryRow(`SELECT operation_id,parent_attempt_id,tool_call_id FROM browser_exploration_child_bindings WHERE child_attempt_id=?`, childID).Scan(&boundOperationID, &boundParentID, &boundToolID); err != nil {
		t.Fatalf("load child operation binding: %v", err)
	}
	if schemaKind != "browser_exploration_v1" || itemCount != 1 || state != "Running" || actionCount != 1 || boundOperationID != 2 || boundParentID != 2 || boundToolID != 1 {
		t.Fatalf("child snapshot/binding/dispatch facts schema=%s items=%d state=%s actions=%d binding=%d/%d/%d", schemaKind, itemCount, state, actionCount, boundOperationID, boundParentID, boundToolID)
	}

	// A duplicate request while the child is Running is idempotent: the unique
	// requester key returns the existing child and creates neither an action nor
	// a second dispatch record.
	service.handleBrowserSubExecution(ctx, &runtimev1.ControlEnvelope{BootId: "plinth-boot", ConnectionEpoch: 1, CorrelationId: 2}, request)
	frames = sentSnapshot()
	if ack := frames[len(frames)-1].GetBrowserSubExecutionAck(); ack == nil || !ack.GetAccepted() || ack.GetChildAttemptId() != childID {
		t.Fatalf("replayed request acknowledgement=%v", frames[len(frames)-1])
	}
	mustQuery(t, db, `SELECT COUNT(*) FROM browser_exploration_actions WHERE child_attempt_id=?`, &actionCount, childID)
	if actionCount != 1 {
		t.Fatalf("replayed request created %d actions", actionCount)
	}
	if scenario == "boot_interrupted" {
		ids, err := service.Browsers.InterruptOldBootOperations(ctx, "replacement-lintel", 1)
		if err != nil || len(ids) != 1 || ids[0] != 2 {
			t.Fatalf("boot interruption ids=%v err=%v", ids, err)
		}
		var resultJSON string
		var actionDigest []byte
		mustQuery(t, db, `SELECT result_json FROM tool_calls WHERE id=1`, &resultJSON)
		mustQuery(t, db, `SELECT result_digest FROM browser_exploration_actions WHERE child_attempt_id=?`, &actionDigest, childID)
		if len(actionDigest) != 32 || attempt.ValidateToolResultPayload("browser_tool_result_v1", []byte(resultJSON)) != nil {
			t.Fatalf("interrupted action must have a replay digest and schema-valid result digest=%x payload=%s", actionDigest, resultJSON)
		}
		// Quoin's boot recovery owns the action/child/tool/operation terminal
		// facts. A Lintel terminal result produced before the replacement but
		// delivered afterwards is superseded, not a conflicting result: accepting
		// its exact acknowledgement is what clears Lintel's durable result cache.
		late := &runtimev1.BrowserExplorationActionResult{
			OperationId: 2, ChildAttemptId: childID, ParentAttemptId: 2, ToolCallId: 1,
			SessionTerminal: true, ErrorCode: "BrowserCrashed", ErrorDetail: "late terminal replay",
		}
		resetSent()
		service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, late)
		frames = sentSnapshot()
		if len(frames) != 1 || frames[0].GetBrowserExplorationActionResultAck() == nil || !frames[0].GetBrowserExplorationActionResultAck().GetAccepted() || frames[0].GetBrowserExplorationActionResultAck().GetDetail() != "superseded by durable recovery terminal" {
			t.Fatalf("late recovery-superseded terminal was not accepted: %v", frames)
		}
		var afterDigest []byte
		var actionOutcome, actionError, childState, toolState, operationState string
		if err := db.QueryRow(`SELECT result_digest,outcome,error_code FROM browser_exploration_actions WHERE child_attempt_id=?`, childID).Scan(&afterDigest, &actionOutcome, &actionError); err != nil {
			t.Fatalf("load recovery action authority: %v", err)
		}
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &childState, childID)
		mustQuery(t, db, `SELECT status FROM tool_calls WHERE id=1`, &toolState)
		mustQuery(t, db, `SELECT state FROM browser_operations WHERE id=2`, &operationState)
		if !bytes.Equal(afterDigest, actionDigest) || actionOutcome != "session_closed" || actionError != "ParentTerminated" || childState != "Failed" || toolState != "failed" || operationState != "Interrupted" {
			t.Fatalf("late replay rewrote recovery authority digest=%x/%x action=%s/%s child=%s tool=%s operation=%s", afterDigest, actionDigest, actionOutcome, actionError, childState, toolState, operationState)
		}
		return
	}
	if scenario == "reconnect" {
		// The duplicate RequestBrowserSubExecution above independently replays the
		// same durable action. Let that asynchronous at-least-once send drain before
		// isolating the reconnect reconciliation assertion below.
		time.Sleep(20 * time.Millisecond)
		// Lintel attachment must redispatch the persisted Running child with the
		// exact bound operation, rather than synthesizing a new operation/action.
		resetSent()
		service.replayRunningBrowserExplorationChildren(ctx)
		frames = sentSnapshot()
		if len(frames) != 1 || frames[0].GetExecuteBrowserExplorationAction() == nil {
			t.Fatalf("reconnect replay=%v", frames)
		}
		replay := frames[0].GetExecuteBrowserExplorationAction()
		if replay.GetOperationId() != 2 || replay.GetChildAttemptId() != childID || replay.GetToolCallId() != 1 {
			t.Fatalf("wrong reconnect action operation=%d child=%d tool=%d", replay.GetOperationId(), replay.GetChildAttemptId(), replay.GetToolCallId())
		}
		return
	}

	payloadJSON := browserToolSuccess(t, "open", "2")
	payloadDigest := sha256.Sum256(payloadJSON)
	result := &runtimev1.BrowserExplorationActionResult{
		OperationId: 2, ChildAttemptId: childID, ParentAttemptId: 2, ToolCallId: 1, Success: true,
		Payload:        &runtimev1.ResultPayload{SchemaKind: "browser_tool_result_v1", CanonicalJson: payloadJSON, ContentDigest: payloadDigest[:]},
		AdmissionProbe: authenticatedProbe("admission", d64),
	}
	if scenario == "admission_failed" {
		traceDigest := sha256.Sum256([]byte("failed admission trace"))
		traceID := addBrowserTraceArtifact(t, db, 2, now, hex.EncodeToString(traceDigest[:]))
		result = &runtimev1.BrowserExplorationActionResult{
			OperationId: 2, ChildAttemptId: childID, ParentAttemptId: 2, ToolCallId: 1,
			SessionTerminal: true, ErrorCode: "AuthenticationRequired", ErrorDetail: "admission probe observed an unauthenticated profile",
			TerminalOutcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED,
			TerminalReason:  runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_REQUIRED,
			TraceArtifactId: traceID, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE, TraceDigest: traceDigest[:],
			AdmissionProbe: &runtimev1.AuthenticationProbeObservation{Phase: runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_ADMISSION, JourneyId: "authentication.url-prefix.v1", JourneyVersion: 1, JourneyCatalogDigest: d64, JourneyCatalogVersion: "v1", Result: runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_UNAUTHENTICATED, ObservedAt: timestamppb.Now()},
		}
	}
	if err := slots.WithCurrent(qruntime.SlotLintel, "lintel-boot", 7, func() error { return nil }); err != nil {
		t.Fatalf("lintel fence unavailable: %v", err)
	}
	resultEpoch := uint64(7)
	if scenario == "successor_epoch" {
		resultEpoch = 8
		slots.AttachStream(qruntime.SlotLintel, "lintel-boot", resultEpoch)
	}
	service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: resultEpoch}, result)
	if scenario == "natural_parent" || scenario == "pending_cancel" || scenario == "pending_claim_complete" || scenario == "interrupted_parent" {
		// The child open result has committed and no browser child remains active.
		// A model may now finish, or the parent may be interrupted, without issuing
		// close_session; the post-commit cleanup must send the established
		// operation-level close in both cases.
		mustExec(t, db, `UPDATE execution_attempts SET lease_until=?,row_version=row_version+1 WHERE id=2`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
		if scenario == "natural_parent" || scenario == "pending_cancel" || scenario == "pending_claim_complete" {
			terminalBody := []byte(`"investigation complete"`)
			terminalDigest := sha256.Sum256(terminalBody)
			if err := service.Investigations.CommitResult(ctx, investigation.Result{AttemptID: 2, BootID: "plinth-boot", Epoch: 1, Succeeded: true, SchemaKind: investigation.OutputSchemaKind, Canonical: terminalBody, Digest: terminalDigest[:]}); err != nil {
				t.Fatalf("commit natural parent result: %v", err)
			}
			var parentState string
			var pending int
			mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=2`, &parentState)
			mustQuery(t, db, `SELECT COUNT(*) FROM pending_attempt_terminals WHERE attempt_id=2`, &pending)
			if parentState != "Running" || pending != 1 {
				t.Fatalf("natural result must be precommitted pending parent=%s pending=%d", parentState, pending)
			}
			if scenario == "pending_claim_complete" {
				// Deterministic claim-first interleave: the normal close uploaded its
				// immutable complete trace, then a recovery-loss parent terminal was
				// made pending before Lintel could deliver ActionResult. The operation
				// itself is already physically stopped, so only the claim exclusion can
				// prevent reconciliation from incorrectly committing the recovery result.
				traceDigest := sha256.Sum256([]byte("claim first complete trace"))
				traceID := addCompleteBrowserTraceArtifact(t, db, 2, now, hex.EncodeToString(traceDigest[:]))
				mustExec(t, db, `INSERT INTO browser_exploration_terminal_claims(child_attempt_id,operation_id,tool_call_id,state,trace_artifact_id,trace_digest,created_at,finalized_at) VALUES(?,?,?,?,?,?,?,?)`, childID, 2, 1, "artifact_committed_complete", traceID, traceDigest[:], now, now)
				// Interleave the persisted recovery-loss terminal with a normal close
				// after its trace upload but before Lintel returns its ActionResult.
				// The operation remains physically active here. Reconciliation must see
				// artifact_committed_complete as the close owner's terminal claim and
				// refrain from cancelling it.
				resetSent()
				service.closeTerminalParentExplorations(ctx, 2)
				if frames := sentSnapshot(); len(frames) != 0 {
					t.Fatalf("recovery loss cancelled artifact-committed normal close: %v", frames)
				}
				mustExec(t, db, `INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,reason_code,observed_at)
					SELECT id,COALESCE((SELECT MAX(probe_seq) FROM browser_probe_results WHERE operation_id=browser_operations.id),0)+1,'completion',identity_revision_id,'authentication.url-prefix.v1',1,?,'v1','Authenticated',NULL,? FROM browser_operations WHERE id=2`, d64, now)
				mustExec(t, db, `UPDATE browser_operations SET state='Succeeded',ended_at=?,stop_confirmed_at=?,stop_confirmation_basis='stop_ack',trace_artifact_id=?,trace_integrity='complete',completion_digest=?,row_version=row_version+1 WHERE id=2`, now, now, traceID, traceDigest[:])
				service.reconcilePendingAttemptTerminals(ctx)
				mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=2`, &parentState)
				mustQuery(t, db, `SELECT COUNT(*) FROM pending_attempt_terminals WHERE attempt_id=2`, &pending)
				if parentState != "Running" || pending != 1 {
					t.Fatalf("recovery terminal overrode claim-first complete trace parent=%s pending=%d", parentState, pending)
				}
				return
			}
			if scenario == "pending_cancel" {
				if state, err := attempt.NewService(db).CancelFence(ctx, 2); err != nil || state != "Cancelling" {
					t.Fatalf("cancel pending natural result state=%q err=%v", state, err)
				}
				mustQuery(t, db, `SELECT COUNT(*) FROM pending_attempt_terminals WHERE attempt_id=2`, &pending)
				if pending != 0 {
					t.Fatalf("explicit cancellation retained incompatible pending result count=%d", pending)
				}
				return
			}
		} else if state, err := attempt.NewService(db).Interrupt(ctx, 2, "lease_expired"); err != nil || state != "Interrupted" {
			t.Fatalf("interrupt idle parent state=%q err=%v", state, err)
		}
		resetSent()
		service.closeTerminalParentExplorations(ctx, 2)
		frames = sentSnapshot()
		if len(frames) != 1 || frames[0].GetCancelBrowserExplorationAction() == nil {
			t.Fatalf("natural parent completion did not close idle exploration: %v", frames)
		}
		close := frames[0].GetCancelBrowserExplorationAction()
		if close.GetOperationId() != 2 || close.GetParentAttemptId() != 2 || close.GetChildAttemptId() != 0 || close.GetToolCallId() != 0 {
			t.Fatalf("natural parent close must be operation-level: %#v", close)
		}
		return
	}
	var actionOutcome sql.NullString
	var toolState, childState, operationState string
	mustQuery(t, db, `SELECT outcome FROM browser_exploration_actions WHERE child_attempt_id=?`, &actionOutcome, childID)
	mustQuery(t, db, `SELECT status FROM tool_calls WHERE id=1`, &toolState)
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &childState, childID)
	mustQuery(t, db, `SELECT state FROM browser_operations WHERE id=2`, &operationState)
	if scenario == "admission_failed" {
		var probeResult string
		if err := db.QueryRow(`SELECT result FROM browser_probe_results WHERE operation_id=2 AND phase='admission'`).Scan(&probeResult); err != nil || probeResult != "Unauthenticated" {
			t.Fatalf("failed admission probe was not persisted result=%q err=%v", probeResult, err)
		}
		if !actionOutcome.Valid || actionOutcome.String != "session_closed" || toolState != "failed" || childState != "Failed" || operationState != "Failed" {
			t.Fatalf("failed admission closure action=%v tool=%s child=%s operation=%s", actionOutcome, toolState, childState, operationState)
		}
		return
	}
	if !actionOutcome.Valid || actionOutcome.String != "success" || toolState != "succeeded" || childState != "Succeeded" || operationState != "Running" {
		t.Fatalf("open closure action=%v tool=%s child=%s operation=%s", actionOutcome, toolState, childState, operationState)
	}
	if scenario == "cancel_idle_single_connection" {
		db.SetMaxOpenConns(1)
		done := make(chan struct{})
		go func() {
			if state, err := attempt.NewService(db).CancelFence(ctx, 2); err != nil || state != "Cancelling" {
				t.Errorf("cancel idle parent state=%q err=%v", state, err)
			}
			service.finalizeCancellation(ctx, 2, "investigation")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("idle cancellation self-locked while a SQLite cursor was open")
		}
		return
	}
	if scenario == "conflicting_replay" {
		conflict := proto.Clone(result).(*runtimev1.BrowserExplorationActionResult)
		conflict.Payload = proto.Clone(result.GetPayload()).(*runtimev1.ResultPayload)
		conflict.Payload.CanonicalJson = append([]byte(nil), result.GetPayload().GetCanonicalJson()...)
		conflict.Payload.CanonicalJson = bytes.Replace(conflict.Payload.CanonicalJson, []byte(`"Payments"`), []byte(`"Different"`), 1)
		conflictDigest := sha256.Sum256(conflict.Payload.CanonicalJson)
		conflict.Payload.ContentDigest = conflictDigest[:]
		resetSent()
		service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: resultEpoch}, conflict)
		frames = sentSnapshot()
		for _, frame := range frames {
			if ack := frame.GetBrowserExplorationActionResultAck(); ack != nil && ack.GetAccepted() {
				t.Fatalf("conflicting replay was accepted: %v", frames)
			}
		}
		var storedDigest []byte
		mustQuery(t, db, `SELECT result_digest FROM browser_exploration_actions WHERE child_attempt_id=?`, &storedDigest, childID)
		originalDigest, err := browserExplorationResultDigest(result)
		if err != nil || string(storedDigest) != string(originalDigest) {
			t.Fatalf("conflicting replay changed durable result digest=%x original=%x err=%v", storedDigest, originalDigest, err)
		}
		return
	}
	var observedPageID, observedOrigin, observationDigest string
	var observationVersion, observationSize int
	if err := db.QueryRow(`SELECT observed_page_id,observed_origin,observation_version,observation_digest,observation_size_bytes FROM browser_exploration_actions WHERE child_attempt_id=?`, childID).Scan(&observedPageID, &observedOrigin, &observationVersion, &observationDigest, &observationSize); err != nil {
		t.Fatalf("load actual observation audit: %v", err)
	}
	var resultProjection struct {
		Observation json.RawMessage `json:"observation"`
	}
	if err := json.Unmarshal(payloadJSON, &resultProjection); err != nil {
		t.Fatal(err)
	}
	wantObservationDigest := sha256.Sum256(resultProjection.Observation)
	if observedPageID != "page-2" || observedOrigin != "https://payments.example" || observationVersion != 1 || observationDigest != hex.EncodeToString(wantObservationDigest[:]) || observationSize != len(resultProjection.Observation) {
		t.Fatalf("actual observation audit page=%q origin=%q version=%d digest=%s size=%d", observedPageID, observedOrigin, observationVersion, observationDigest, observationSize)
	}
	if scenario == "successor_epoch" {
		return
	}
	if scenario == "closed_session" {
		// A delayed model Tool Call can target a session that has already reached a
		// durable terminal operation. It must receive the typed terminal result and
		// a child tombstone, rather than an INPUT_UNSUPPORTED transport rejection
		// that strands Plinth's running Tool Call.
		closedTrace := sha256.Sum256([]byte("closed-session trace"))
		closedTraceID := addBrowserTraceArtifact(t, db, 2, now, hex.EncodeToString(closedTrace[:]))
		mustExec(t, db, `UPDATE browser_operations SET state='Failed',ended_at=?,terminal_reason='browser_crashed',trace_artifact_id=?,trace_integrity='incomplete',stop_confirmed_at=?,stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=2`, now, closedTraceID, now)
		closedArguments := []byte(`{"action":"read","sessionId":"2"}`)
		closedToolID := addBrowserToolCall(t, db, 2, closedArguments, "provider-closed-session", now, d64)
		closedDigest := sha256.Sum256(closedArguments)
		resetSent()
		service.handleBrowserSubExecution(ctx, &runtimev1.ControlEnvelope{BootId: "plinth-boot", ConnectionEpoch: 1}, &runtimev1.RequestBrowserSubExecution{ParentAttemptId: 2, ToolCallId: closedToolID, Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: closedArguments, ContentDigest: closedDigest[:]}})
		frames = sentSnapshot()
		if len(frames) < 1 || frames[0].GetBrowserSubExecutionAck() == nil || !frames[0].GetBrowserSubExecutionAck().GetAccepted() {
			t.Fatalf("closed-session request was not accepted: %v", frames)
		}
		closedChildID := frames[0].GetBrowserSubExecutionAck().GetChildAttemptId()
		var closedToolState, closedChildState, closedResult string
		if err := db.QueryRow(`SELECT status,result_json FROM tool_calls WHERE id=?`, closedToolID).Scan(&closedToolState, &closedResult); err != nil {
			t.Fatalf("load closed-session tool result: %v", err)
		}
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &closedChildState, closedChildID)
		if closedToolState != "succeeded" || closedChildState != "Succeeded" || !bytes.Contains([]byte(closedResult), []byte(`"code":"ParentTerminated"`)) || attempt.ValidateToolResultPayload("browser_tool_result_v1", []byte(closedResult)) != nil {
			t.Fatalf("closed-session tombstone tool=%s child=%s result=%s", closedToolState, closedChildState, closedResult)
		}
		// Same Tool Call replay returns the original child, never a second
		// tombstone or an action against the already stopped Chromium session.
		service.handleBrowserSubExecution(ctx, &runtimev1.ControlEnvelope{BootId: "plinth-boot", ConnectionEpoch: 1}, &runtimev1.RequestBrowserSubExecution{ParentAttemptId: 2, ToolCallId: closedToolID, Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: closedArguments, ContentDigest: closedDigest[:]}})
		frames = sentSnapshot()
		if ack := frames[len(frames)-1].GetBrowserSubExecutionAck(); ack == nil || !ack.GetAccepted() || ack.GetChildAttemptId() != closedChildID {
			t.Fatalf("closed-session replay acknowledgement=%v", frames[len(frames)-1])
		}
		return
	}
	if scenario == "idle_artifact" {
		// A trace upload can fail while the admitted session is idle between model
		// turns. There is no child Tool Call to close, but the running operation
		// must still release its identity slot through a typed terminal completion.
		failureDigest := sha256.Sum256([]byte("idle trace upload failure"))
		service.handleBrowserCompletion(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, &runtimev1.CompleteBrowserOperation{OperationId: 2, Outcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED, ResultDigest: failureDigest[:], EndedAt: timestamppb.Now()})
		var operation, terminalReason string
		if err := db.QueryRow(`SELECT state,terminal_reason FROM browser_operations WHERE id=2`).Scan(&operation, &terminalReason); err != nil {
			t.Fatal(err)
		}
		if operation != "Failed" || terminalReason != "artifact_commit_failed" {
			t.Fatalf("idle trace failure did not close operation=%s reason=%s", operation, terminalReason)
		}
		return
	}
	if scenario == "cancel_no_child" {
		// No action child is active after the open result, but Chromium is still
		// running. Offline cancellation must retain the parent fence; once Lintel
		// returns, the operation-level cancellation (zero child/tool IDs) must
		// commit its trace and converge the parent without inventing a child.
		if state, err := attempt.NewService(db).CancelFence(ctx, 2); err != nil || state != "Cancelling" {
			t.Fatalf("parent cancel state=%q err=%v", state, err)
		}
		service.Slots = nil
		service.finalizeCancellation(ctx, 2, "investigation")
		var parent string
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=2`, &parent)
		if parent != "Cancelling" {
			t.Fatalf("offline cancellation acknowledged while operation runs: parent=%s", parent)
		}
		service.Slots = slots
		service.finalizeCancellation(ctx, 2, "investigation")
		frames = sentSnapshot()
		if len(frames) == 0 || frames[len(frames)-1].GetCancelBrowserExplorationAction() == nil {
			t.Fatalf("idle cancellation was not dispatched: %v", frames)
		}
		idleCancel := frames[len(frames)-1].GetCancelBrowserExplorationAction()
		if idleCancel.GetOperationId() != 2 || idleCancel.GetChildAttemptId() != 0 || idleCancel.GetToolCallId() != 0 {
			t.Fatalf("idle cancellation must be operation-level: %+v", idleCancel)
		}
		traceHex := fmt.Sprintf("%064x", 77)
		traceID := addBrowserTraceArtifact(t, db, 2, now, traceHex)
		traceDigest, _ := hex.DecodeString(traceHex)
		completionDigest := sha256.Sum256([]byte("idle cancellation completion"))
		service.handleBrowserCompletion(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, &runtimev1.CompleteBrowserOperation{OperationId: 2, Outcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_CANCELLED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_CANCELLED, ResultDigest: completionDigest[:], TraceDigest: traceDigest, EndedAt: timestamppb.Now(), TraceArtifactId: traceID, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE})
		// Completion records the terminal operation, but cancellation must retain
		// the parent fence through physical StopAck so a delayed Start cannot become
		// ownerless after the model attempt is acknowledged.
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=2`, &parent)
		if parent != "Cancelling" {
			t.Fatalf("parent was acknowledged before StopAck: %s", parent)
		}
		service.handleBrowserStopAck(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, &runtimev1.StopBrowserOperationAck{
			OperationId: 2, StoppedAt: timestamppb.Now(), CleanupOutcome: runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_SUCCEEDED,
			ProcessStopped: true, TunnelClosed: true, TraceStagingDeleted: true, TemporaryProfileDeleted: true,
			CleanupStateHash: make([]byte, sha256.Size),
		})
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=2`, &parent)
			if parent == "Cancelled" {
				break
			}
			time.Sleep(time.Millisecond)
		}
		var operation string
		mustQuery(t, db, `SELECT state FROM browser_operations WHERE id=2`, &operation)
		if parent != "Cancelled" || operation != "Cancelled" {
			t.Fatalf("idle cancellation did not converge after StopAck parent=%s operation=%s", parent, operation)
		}
		return
	}
	// Simulate losing Quoin's process-local pending-delivery map before the
	if scenario != "success" && scenario != "successor_epoch" {
	}
	if scenario != "success" && scenario != "successor_epoch" {
		terminalArguments := []byte(`{"action":"close_session","sessionId":"2"}`)
		terminalToolID := addBrowserToolCall(t, db, 2, terminalArguments, "provider-terminal", now, d64)
		terminalDigest := sha256.Sum256(terminalArguments)
		service.handleBrowserSubExecution(ctx, &runtimev1.ControlEnvelope{BootId: "plinth-boot", ConnectionEpoch: 1}, &runtimev1.RequestBrowserSubExecution{ParentAttemptId: 2, ToolCallId: terminalToolID, Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: terminalArguments, ContentDigest: terminalDigest[:]}})
		frames = sentSnapshot()
		terminalChildID := frames[len(frames)-1].GetBrowserSubExecutionAck().GetChildAttemptId()
		waitForBrowserAction(t, db, terminalChildID)
		if scenario == "close_then_crash" {
			closeTrace := sha256.Sum256([]byte("close trace wins"))
			closeTraceID := addCompleteBrowserTraceArtifact(t, db, 2, now, hex.EncodeToString(closeTrace[:]))
			// Stage the competing crash upload while the child is still Running;
			// the late completion must lose the operation CAS, not be rejected merely
			// because it arrived after physical cleanup.
			crashTrace := sha256.Sum256([]byte("late crash must lose"))
			crashTraceID := addBrowserTraceArtifact(t, db, 2, now, hex.EncodeToString(crashTrace[:]))
			closePayload := browserToolSuccess(t, "close_session", "2")
			closePayloadDigest := sha256.Sum256(closePayload)
			mustExec(t, db, `INSERT INTO browser_exploration_terminal_claims(child_attempt_id,operation_id,tool_call_id,state,created_at) VALUES(?,?,?,'claimed_complete',?)`, terminalChildID, 2, terminalToolID, now)
			closed := &runtimev1.BrowserExplorationActionResult{OperationId: 2, ChildAttemptId: terminalChildID, ParentAttemptId: 2, ToolCallId: terminalToolID, Success: true, Payload: &runtimev1.ResultPayload{SchemaKind: "browser_tool_result_v1", CanonicalJson: closePayload, ContentDigest: closePayloadDigest[:]}, SessionTerminal: true, TerminalOutcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_SUCCEEDED, TraceArtifactId: closeTraceID, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE, TraceDigest: closeTrace[:], CompletionProbe: authenticatedProbe("completion", d64)}
			service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, closed)
			crashDigest := sha256.Sum256([]byte("late crash completion"))
			service.handleBrowserCompletion(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, &runtimev1.CompleteBrowserOperation{OperationId: 2, Outcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED, ResultDigest: crashDigest[:], TraceDigest: crashTrace[:], TraceArtifactId: crashTraceID, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE, EndedAt: timestamppb.Now()})
			var state string
			var traceID int64
			if err := db.QueryRow(`SELECT state,trace_artifact_id FROM browser_operations WHERE id=2`).Scan(&state, &traceID); err != nil {
				t.Fatalf("load terminal operation after late crash: %v", err)
			}
			if state != "Succeeded" || traceID != closeTraceID {
				t.Fatalf("late crash overwrote committed close state=%s trace=%d", state, traceID)
			}
			return
		}
		if scenario == "artifact" || scenario == "artifact_probe" {
			failed := &runtimev1.BrowserExplorationActionResult{OperationId: 2, ChildAttemptId: terminalChildID, ParentAttemptId: 2, ToolCallId: terminalToolID, SessionTerminal: true, ErrorCode: "ArtifactCommitFailed", ErrorDetail: "trace upload failed", TerminalOutcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED}
			if scenario == "artifact_probe" {
				failed.CompletionProbe = &runtimev1.AuthenticationProbeObservation{Phase: runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_COMPLETION, JourneyId: "authentication.url-prefix.v1", JourneyVersion: 1, JourneyCatalogDigest: d64, JourneyCatalogVersion: "v1", Result: runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE, ReasonCode: "url_probe_unavailable", ObservedAt: timestamppb.Now()}
			}
			service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, failed)
			var action, tool, child, op string
			mustQuery(t, db, `SELECT outcome FROM browser_exploration_actions WHERE child_attempt_id=?`, &action, terminalChildID)
			mustQuery(t, db, `SELECT status FROM tool_calls WHERE id=?`, &tool, terminalToolID)
			mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &child, terminalChildID)
			mustQuery(t, db, `SELECT state FROM browser_operations WHERE id=2`, &op)
			if action != "session_closed" || tool != "failed" || child != "Failed" || op != "Failed" {
				t.Fatalf("artifact terminal action=%s tool=%s child=%s op=%s", action, tool, child, op)
			}
			if scenario == "artifact_probe" {
				var probeResult, reason string
				if err := db.QueryRow(`SELECT result,reason_code FROM browser_probe_results WHERE operation_id=2 AND phase='completion'`).Scan(&probeResult, &reason); err != nil {
					t.Fatalf("load failed completion probe: %v", err)
				}
				if probeResult != "Indeterminate" || reason != "url_probe_unavailable" {
					t.Fatalf("failed completion probe was not persisted result=%s reason=%s", probeResult, reason)
				}
			}
			return
		}
		traceHex := fmt.Sprintf("%064x", 10)
		traceID := addBrowserTraceArtifact(t, db, 2, now, traceHex)
		traceDigest, _ := hex.DecodeString(traceHex)
		crashDigest := sha256.Sum256([]byte("crash-first"))
		service.handleBrowserCompletion(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, &runtimev1.CompleteBrowserOperation{OperationId: 2, Outcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED, ResultDigest: crashDigest[:], TraceDigest: traceDigest, EndedAt: timestamppb.Now(), TraceArtifactId: traceID, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE})
		var op, integrity, action string
		mustQuery(t, db, `SELECT state FROM browser_operations WHERE id=2`, &op)
		mustQuery(t, db, `SELECT trace_integrity FROM browser_operations WHERE id=2`, &integrity)
		mustQuery(t, db, `SELECT outcome FROM browser_exploration_actions WHERE child_attempt_id=?`, &action, terminalChildID)
		if op != "Failed" || integrity != "incomplete" || action != "session_closed" {
			t.Fatalf("crash-first op=%s integrity=%s action=%s", op, integrity, action)
		}
		return
	}
	var toolVersion, childVersionAfter int
	mustQuery(t, db, `SELECT row_version FROM tool_calls WHERE id=1`, &toolVersion)
	mustQuery(t, db, `SELECT row_version FROM execution_attempts WHERE id=?`, &childVersionAfter, childID)
	frames = sentSnapshot()
	deliveries := browserToolDeliveryCount(frames)
	// A stale fence and a duplicate final result are both no-ops: neither may
	// mutate durable terminal facts nor emit a second ToolResultDelivery.
	service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "stale", ConnectionEpoch: 8}, result)
	service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, result)
	var gotToolVersion, gotChildVersion int
	mustQuery(t, db, `SELECT row_version FROM tool_calls WHERE id=1`, &gotToolVersion)
	mustQuery(t, db, `SELECT row_version FROM execution_attempts WHERE id=?`, &gotChildVersion, childID)
	frames = sentSnapshot()
	if gotToolVersion != toolVersion || gotChildVersion != childVersionAfter || browserToolDeliveryCount(frames) != deliveries {
		t.Fatalf("stale/duplicate changed terminal facts tool=%d/%d child=%d/%d tool-deliveries=%d/%d", gotToolVersion, toolVersion, gotChildVersion, childVersionAfter, browserToolDeliveryCount(frames), deliveries)
	}

	// A subsequent action can return a model-visible recoverable error without
	// ending the admitted exploration session.
	recoverArguments := []byte(`{"action":"reload","sessionId":"2"}`)
	recoverToolID := addBrowserToolCall(t, db, 2, recoverArguments, "provider-reload", now, d64)
	recoverDigest := sha256.Sum256(recoverArguments)
	recoverRequest := &runtimev1.RequestBrowserSubExecution{ParentAttemptId: 2, ToolCallId: recoverToolID, Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: recoverArguments, ContentDigest: recoverDigest[:]}}
	service.handleBrowserSubExecution(ctx, &runtimev1.ControlEnvelope{BootId: "plinth-boot", ConnectionEpoch: 1, CorrelationId: 3}, recoverRequest)
	frames = sentSnapshot()
	recoverChildID := frames[len(frames)-1].GetBrowserSubExecutionAck().GetChildAttemptId()
	waitForBrowserAction(t, db, recoverChildID)
	if scenario == "cancel" || scenario == "cancel_artifact" || scenario == "cancel_complete" {
		var preCommittedTraceID int64
		var preCommittedTraceDigest []byte
		if scenario == "cancel_complete" {
			traceDigest := sha256.Sum256([]byte("complete trace committed before parent cancellation"))
			preCommittedTraceDigest = traceDigest[:]
			preCommittedTraceID = addCompleteBrowserTraceArtifact(t, db, 2, now, hex.EncodeToString(traceDigest[:]))
			// This is the durable result of Lintel's accepted claim/upload; the
			// following CancelFence intentionally wins before its ActionResult.
			mustExec(t, db, `INSERT INTO browser_exploration_terminal_claims(child_attempt_id,operation_id,tool_call_id,state,trace_artifact_id,trace_digest,created_at,finalized_at) VALUES(?,?,?,?,?,?,?,?)`, recoverChildID, 2, recoverToolID, "artifact_committed_complete", preCommittedTraceID, preCommittedTraceDigest, now, now)
		}
		// Exercise the actual parent fence. Its frozen trigger cancels the running
		// Tool Call and moves this child to Cancelling; no test writes a child state.
		if state, err := attempt.NewService(db).CancelFence(ctx, 2); err != nil || state != "Cancelling" {
			t.Fatalf("parent cancel state=%q err=%v", state, err)
		}
		var parent, tool, child string
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=2`, &parent)
		mustQuery(t, db, `SELECT status FROM tool_calls WHERE id=?`, &tool, recoverToolID)
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &child, recoverChildID)
		if parent != "Cancelling" || tool != "cancelled" || child != "Cancelling" {
			t.Fatalf("parent cancellation fence parent=%s tool=%s child=%s", parent, tool, child)
		}
		service.finalizeCancellation(ctx, 2, "investigation")
		frames = sentSnapshot()
		if len(frames) == 0 || frames[len(frames)-1].GetCancelBrowserExplorationAction() == nil {
			t.Fatalf("cancelling parent did not dispatch browser action cancellation: %v", frames)
		}
		traceDigest := sha256.Sum256([]byte("cancelled continuous trace"))
		traceID := addBrowserTraceArtifact(t, db, 2, now, hex.EncodeToString(traceDigest[:]))
		cancel := &runtimev1.BrowserExplorationActionResult{OperationId: 2, ChildAttemptId: recoverChildID, ParentAttemptId: 2, ToolCallId: recoverToolID, ErrorCode: "Cancelled", ErrorDetail: "parent attempt cancelled", SessionTerminal: true, TerminalOutcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_CANCELLED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_CANCELLED, TraceArtifactId: traceID, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE, TraceDigest: traceDigest[:]}
		if scenario == "cancel_artifact" {
			cancel.ErrorCode, cancel.ErrorDetail = "ArtifactCommitFailed", "trace upload failed during parent cancellation"
			cancel.TerminalOutcome, cancel.TerminalReason = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED
			cancel.TraceArtifactId, cancel.TraceIntegrity, cancel.TraceDigest = 0, runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_UNSPECIFIED, nil
		}
		service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, cancel)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=2`, &parent)
			if parent == "Cancelled" {
				break
			}
			time.Sleep(time.Millisecond)
		}
		var action, op, integrity, claim string
		mustQuery(t, db, `SELECT outcome FROM browser_exploration_actions WHERE child_attempt_id=?`, &action, recoverChildID)
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &child, recoverChildID)
		mustQuery(t, db, `SELECT state,trace_integrity FROM browser_operations WHERE id=2`, &op, &integrity)
		if scenario == "cancel_complete" {
			var historicalArtifactID any
			var historicalDigest []byte
			mustQuery(t, db, `SELECT state,historical_complete_trace_artifact_id,historical_complete_trace_digest FROM browser_exploration_terminal_claims WHERE child_attempt_id=?`, &claim, &historicalArtifactID, &historicalDigest, recoverChildID)
			if historicalArtifactID == nil || historicalDigest == nil || historicalArtifactID.(int64) != preCommittedTraceID || string(historicalDigest) != string(preCommittedTraceDigest) {
				t.Fatalf("downgraded cancellation claim lost immutable complete history artifact=%v digest=%x", historicalArtifactID, historicalDigest)
			}
			var operationTraceID int64
			mustQuery(t, db, `SELECT trace_artifact_id FROM browser_operations WHERE id=2`, &operationTraceID)
			if operationTraceID != traceID || operationTraceID == preCommittedTraceID {
				t.Fatalf("cancellation did not attach distinct incomplete trace got=%d complete=%d incomplete=%d", operationTraceID, preCommittedTraceID, traceID)
			}
		}
		if parent != "Cancelled" || action != "session_closed" || child != "Cancelled" || ((scenario == "cancel" || scenario == "cancel_complete") && op != "Cancelled") || (scenario == "cancel_artifact" && op != "Failed") || (scenario == "cancel_complete" && (integrity != "incomplete" || claim != "downgraded_incomplete")) {
			t.Fatalf("parent cancellation closure parent=%s action=%s child=%s operation=%s integrity=%s claim=%s", parent, action, child, op, integrity, claim)
		}
		return
	}
	recoverPayload := browserToolResult(t, "reload", "recoverable_error", "2")
	recoverPayloadDigest := sha256.Sum256(recoverPayload)
	recoverResult := &runtimev1.BrowserExplorationActionResult{OperationId: 2, ChildAttemptId: recoverChildID, ParentAttemptId: 2, ToolCallId: recoverToolID, Payload: &runtimev1.ResultPayload{SchemaKind: "browser_tool_result_v1", CanonicalJson: recoverPayload, ContentDigest: recoverPayloadDigest[:]}, ErrorCode: "ElementNotFound", ErrorDetail: "target not observed"}
	service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, recoverResult)
	mustQuery(t, db, `SELECT outcome FROM browser_exploration_actions WHERE child_attempt_id=?`, &actionOutcome, recoverChildID)
	mustQuery(t, db, `SELECT status FROM tool_calls WHERE id=?`, &toolState, recoverToolID)
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &childState, recoverChildID)
	if !actionOutcome.Valid || actionOutcome.String != "recoverable_error" || toolState != "succeeded" || childState != "Succeeded" {
		t.Fatalf("recoverable closure action=%v tool=%s child=%s", actionOutcome, toolState, childState)
	}

	// A third model turn can close the same continuous exploration. This is a
	// complete success only when the terminal completion probe and trace facts
	// are supplied before the frozen Tool Call trigger closes its child Attempt.
	closeArguments := []byte(`{"action":"close_session","sessionId":"2"}`)
	closeToolID := addBrowserToolCall(t, db, 3, closeArguments, "provider-close", now, d64)
	closeDigest := sha256.Sum256(closeArguments)
	closeRequest := &runtimev1.RequestBrowserSubExecution{ParentAttemptId: 2, ToolCallId: closeToolID, Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: closeArguments, ContentDigest: closeDigest[:]}}
	service.handleBrowserSubExecution(ctx, &runtimev1.ControlEnvelope{BootId: "plinth-boot", ConnectionEpoch: 1, CorrelationId: 3}, closeRequest)
	frames = sentSnapshot()
	closeChildID := frames[len(frames)-1].GetBrowserSubExecutionAck().GetChildAttemptId()
	waitForBrowserAction(t, db, closeChildID)
	traceDigest := sha256.Sum256([]byte("complete trace"))
	traceID := addCompleteBrowserTraceArtifact(t, db, 2, now, hex.EncodeToString(traceDigest[:]))
	closePayload := browserToolSuccess(t, "close_session", "2")
	closePayloadDigest := sha256.Sum256(closePayload)
	// Terminal trace references are capabilities, not generic browser-owned
	// Artifact IDs: all identity, fence, child, and blob-digest bindings are
	// rechecked in the same transaction that would close the action.
	traceConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = traceConn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name                             string
		artifactID, operationID, childID int64
		boot                             string
		epoch                            uint64
		digest                           []byte
		integrity                        string
		wantErr                          bool
	}{
		{"exact", traceID, 2, closeChildID, "lintel-boot", 7, traceDigest[:], "complete", false},
		{"wrong_blob_digest", traceID, 2, closeChildID, "lintel-boot", 7, make([]byte, sha256.Size), "complete", true},
		// The terminal message must match immutable upload capability metadata;
		// matching owner/digest cannot relabel complete evidence as incomplete.
		{"wrong_trace_integrity", traceID, 2, closeChildID, "lintel-boot", 7, traceDigest[:], "incomplete", true},
		{"wrong_child", traceID, 2, closeChildID + 1, "lintel-boot", 7, traceDigest[:], "complete", true},
		{"wrong_operation", traceID, 3, closeChildID, "lintel-boot", 7, traceDigest[:], "complete", true},
		{"stale_boot", traceID, 2, closeChildID, "old-boot", 7, traceDigest[:], "complete", true},
		// The Start/Upload fence stays immutable at epoch 7; a terminal result
		// replayed through same-boot successor epoch 8 remains lawful.
		{"successor_epoch", traceID, 2, closeChildID, "lintel-boot", 8, traceDigest[:], "complete", false},
	} {
		err := validateExplorationTrace(ctx, traceConn, check.artifactID, check.operationID, check.childID, check.boot, check.epoch, check.digest, check.integrity, false)
		if (err != nil) != check.wantErr {
			t.Fatalf("trace binding %s err=%v wantErr=%v", check.name, err, check.wantErr)
		}
	}
	if _, err = traceConn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	traceConn.Close()
	mustExec(t, db, `INSERT INTO browser_exploration_terminal_claims(child_attempt_id,operation_id,tool_call_id,state,created_at) VALUES(?,?,?,'claimed_complete',?)`, closeChildID, 2, closeToolID, now)
	closeResult := &runtimev1.BrowserExplorationActionResult{
		OperationId: 2, ChildAttemptId: closeChildID, ParentAttemptId: 2, ToolCallId: closeToolID, Success: true, SessionTerminal: true,
		Payload:         &runtimev1.ResultPayload{SchemaKind: "browser_tool_result_v1", CanonicalJson: closePayload, ContentDigest: closePayloadDigest[:]},
		TerminalOutcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_SUCCEEDED,
		TraceArtifactId: traceID, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE, TraceDigest: traceDigest[:],
		CompletionProbe: authenticatedProbe("completion", d64),
	}
	service.handleBrowserExplorationActionResult(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, closeResult)
	var closeActionOutcome, closeToolState, closeChildState, closeOperationState, traceIntegrity string
	var storedTraceID int64
	mustQuery(t, db, `SELECT outcome FROM browser_exploration_actions WHERE child_attempt_id=?`, &closeActionOutcome, closeChildID)
	mustQuery(t, db, `SELECT status FROM tool_calls WHERE id=?`, &closeToolState, closeToolID)
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &closeChildState, closeChildID)
	if err := db.QueryRow(`SELECT state,trace_artifact_id,trace_integrity FROM browser_operations WHERE id=2`).Scan(&closeOperationState, &storedTraceID, &traceIntegrity); err != nil {
		t.Fatal(err)
	}
	if closeActionOutcome != "success" || closeToolState != "succeeded" || closeChildState != "Succeeded" || closeOperationState != "Succeeded" || storedTraceID != traceID || traceIntegrity != "complete" {
		t.Fatalf("complete close action=%s tool=%s child=%s operation=%s trace=%d/%s", closeActionOutcome, closeToolState, closeChildState, closeOperationState, storedTraceID, traceIntegrity)
	}
	// A crash completion that races after a committed successful close is stale.
	// It must not overwrite the complete trace or produce a second Tool result.
	// A stale completion cannot upload a new trace after its operation is
	// terminal. Replay the already committed trace reference to exercise the
	// stale-result acknowledgement without bypassing upload fences.
	crashTraceID := traceID
	crashDigest := sha256.Sum256([]byte("late crash"))
	frames = sentSnapshot()
	deliveriesAfterClose := len(frames)
	service.handleBrowserCompletion(ctx, &runtimev1.ControlEnvelope{BootId: "lintel-boot", ConnectionEpoch: 7}, &runtimev1.CompleteBrowserOperation{OperationId: 2, Outcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED, ResultDigest: crashDigest[:], EndedAt: timestamppb.Now(), TraceArtifactId: crashTraceID, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE})
	if err := db.QueryRow(`SELECT state,trace_artifact_id,trace_integrity FROM browser_operations WHERE id=2`).Scan(&closeOperationState, &storedTraceID, &traceIntegrity); err != nil {
		t.Fatal(err)
	}
	frames = sentSnapshot()
	completionRejected := false
	for _, frame := range frames[deliveriesAfterClose:] {
		ack := frame.GetCompleteBrowserOperationAck()
		if ack != nil && bytes.Equal(ack.GetResultDigest(), crashDigest[:]) && !ack.GetAccepted() {
			completionRejected = true
		}
	}
	if closeOperationState != "Succeeded" || storedTraceID != traceID || traceIntegrity != "complete" || !completionRejected {
		t.Fatalf("late crash overwrote close operation=%s trace=%d/%s completionRejected=%t", closeOperationState, storedTraceID, traceIntegrity, completionRejected)
	}
}

func browserToolSuccess(t *testing.T, action, sessionID string) []byte {
	t.Helper()
	result := map[string]any{"outcome": "success", "action": action, "sessionId": sessionID, "observation": map[string]any{"version": 1, "url": "https://payments.example/", "origin": "https://payments.example", "title": "Payments", "pages": []any{map[string]any{"pageId": "page-2", "url": "https://payments.example/", "origin": "https://payments.example", "title": "Payments", "current": true}}, "visibleText": "", "accessibilityText": "", "elements": []any{}, "events": []any{}, "originalSizeBytes": 0, "truncated": false}}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func authenticatedProbe(phase, digest string) *runtimev1.AuthenticationProbeObservation {
	probePhase := runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_ADMISSION
	if phase == "completion" {
		probePhase = runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_COMPLETION
	}
	return &runtimev1.AuthenticationProbeObservation{Phase: probePhase, Result: runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED, JourneyId: "authentication.url-prefix.v1", JourneyVersion: 1, JourneyCatalogDigest: digest, JourneyCatalogVersion: "v1", ObservedAt: timestamppb.Now()}
}

func addBrowserToolCall(t *testing.T, db *sql.DB, callSeq int, arguments []byte, providerID, now, digest string) int64 {
	t.Helper()
	model := mustExecResult(t, db, `INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at)
		VALUES(2,?,0,'chat','model',3,'p','investigation-v1',?,'t',?,?,?,10,1,0,'running',?)`, callSeq, digest, digest, digest, digest, now)
	modelID, err := model.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, modelID, digest, modelID, digest)
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,3,'system',?,2)`, modelID, digest)
	proposal, err := json.Marshal(map[string]any{"assistantText": "", "finishReason": "tool_calls", "tool_calls": []any{map[string]any{"id": providerID, "name": "quoin_browser", "arguments": json.RawMessage(arguments)}}})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,?,?,'tool_calls',?)`, modelID, string(proposal), digest, now)
	mustExec(t, db, `UPDATE model_calls SET status='succeeded',ended_at=?,usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}' WHERE id=?`, now, modelID)
	argumentDigest := sha256.Sum256(arguments)
	tool := mustExecResult(t, db, `INSERT INTO tool_calls(attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(2,?,?,0,?,'quoin_browser','1',?,?,'quoin_browser','return_to_model','pending',?)`, modelID, callSeq, providerID, string(arguments), fmt.Sprintf("%x", argumentDigest), now)
	toolID, err := tool.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE tool_calls SET status='running',started_at=?,row_version=row_version+1 WHERE id=?`, now, toolID)
	return toolID
}

func waitForBrowserAction(t *testing.T, db *sql.DB, childID int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var state string
		var count int
		mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, childID)
		mustQuery(t, db, `SELECT COUNT(*) FROM browser_exploration_actions WHERE child_attempt_id=?`, &count, childID)
		if state == "Running" && count == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("browser action child %d was not dispatched", childID)
}

func addBrowserTraceArtifact(t *testing.T, db *sql.DB, operationID int64, now, digest string) int64 {
	return addBrowserTraceArtifactWithIntegrity(t, db, operationID, now, digest, "incomplete")
}

func addCompleteBrowserTraceArtifact(t *testing.T, db *sql.DB, operationID int64, now, digest string) int64 {
	return addBrowserTraceArtifactWithIntegrity(t, db, operationID, now, digest, "complete")
}

func addBrowserTraceArtifactWithIntegrity(t *testing.T, db *sql.DB, operationID int64, now, digest, integrity string) int64 {
	t.Helper()
	blob := mustExecResult(t, db, `INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at) VALUES(?,1,?,?)`, digest, "fixture/"+digest, now)
	blobID, err := blob.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	artifact := mustExecResult(t, db, `INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,expires_at,created_at) VALUES(?,'trace','application/json',1,'generated','browser_operation',?, ?,?)`, blobID, operationID, now, now)
	artifactID, err := artifact.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var attemptID sql.NullInt64
	_ = db.QueryRow(`SELECT child.id FROM browser_exploration_actions action JOIN execution_attempts child ON child.id=action.child_attempt_id WHERE action.operation_id=? AND child.state IN ('Running','Cancelling') ORDER BY action.id DESC LIMIT 1`, operationID).Scan(&attemptID)
	uploadID := "trace-" + digest + fmt.Sprint(operationID)
	mustExec(t, db, `INSERT INTO runtime_artifact_uploads(upload_id,attempt_id,boot_id,connection_epoch,owner_type,owner_id,kind,media_type,retention_kind,sensitive,size_bytes,sha256,trace_integrity,state,created_at)
		VALUES(?,?,?,?,? ,?,'trace','application/json','generated',1,1,?,?,'uploading',?)`, uploadID, nullableTestInt(attemptID), "lintel-boot", 7, "browser_operation", operationID, digest, integrity, now)
	mustExec(t, db, `UPDATE runtime_artifact_uploads SET state='committed',artifact_id=?,committed_at=?,row_version=row_version+1 WHERE upload_id=?`, artifactID, now, uploadID)
	return artifactID
}

func nullableTestInt(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
}

func browserToolResult(t *testing.T, action, outcome, sessionID string) []byte {
	t.Helper()
	result := map[string]any{
		"outcome": outcome, "action": action, "sessionId": sessionID,
		"observation": map[string]any{
			"version": 1, "url": "https://payments.example/", "origin": "https://payments.example", "title": "Payments",
			"pages": []any{map[string]any{"pageId": "page-2", "url": "https://payments.example/", "origin": "https://payments.example", "title": "Payments", "current": true}}, "visibleText": "", "accessibilityText": "", "elements": []any{}, "events": []any{}, "originalSizeBytes": 0, "truncated": false,
		},
		"error": map[string]any{"code": "ElementNotFound", "message": "target not observed", "retryableInSession": true},
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func seedQualifiedModelProvider(t *testing.T, ctx context.Context, db *sql.DB, rootKey string, now string) {
	t.Helper()
	connections.ProbeContractSource = func() string { return "fixture-connection-probe-contract-v1" }
	connections.SetReleaseVersion("test")
	d64 := fmt.Sprintf("%064x", 1)
	service := connections.NewService(db, func() ([]byte, error) { return os.ReadFile(rootKey) })
	projection, err := json.Marshal(map[string]any{"type": "model_provider", "baseUrl": "https://model.example", "chatModelId": "chat", "embeddingModelId": "embed", "contextBudgetTokens": 1024, "maxOutputTokens": 256})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(map[string]string{"type": "model_provider", "apiKey": "fixture"})
	summary, err := service.Create(ctx, connections.CreateInput{Name: "fixture-model", Type: connections.TypeModelProvider, NonSecretJSON: projection, Secret: secret, SecretPresent: true}, 1, "fixture-model-create")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT OR IGNORE INTO runtime_slots(slot,state,row_version,created_at) VALUES('plinth','unregistered',1,?)`, now)
	result := mustExecResult(t, db, `INSERT INTO runtime_credentials(slot,generation,token_digest,confirmed_at,created_at) VALUES('plinth',1,?,?,?)`, make([]byte, 32), now, now)
	credentialID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot='plinth'`, credentialID)
	mustExec(t, db, `INSERT OR IGNORE INTO runtime_slots(slot,state,row_version,created_at) VALUES('lintel','unregistered',1,?)`, now)
	lintelCredential := mustExecResult(t, db, `INSERT INTO runtime_credentials(slot,generation,token_digest,confirmed_at,created_at) VALUES('lintel',1,?,?,?)`, make([]byte, 32), now, now)
	lintelCredentialID, err := lintelCredential.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot='lintel'`, lintelCredentialID)
	probeID, err := service.StartProbe(ctx, summary.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='fixture-probe',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=?`, now, probeID)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=?`, now, now, probeID)
	var chatGrant, embeddingGrant int64
	mustQuery(t, db, `SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='model_probe_chat'`, &chatGrant, probeID)
	mustQuery(t, db, `SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='model_probe_embedding'`, &embeddingGrant, probeID)
	for index, state := range []string{"succeeded", "succeeded", "cancelled"} {
		grantID := chatGrant
		if index == 1 {
			grantID = embeddingGrant
		}
		operation := map[int]string{0: "chat", 1: "embedding", 2: "chat"}[index]
		var result sql.Result
		if operation == "chat" {
			result = mustExecResult(t, db, `INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, probeID, index+1, 0, operation, "fixture", grantID, "v1", "probe-v1", d64, "v1", d64, d64, d64, 1024, 256, 0, "running", now)
		} else {
			result = mustExecResult(t, db, `INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, probeID, index+1, 0, operation, "fixture", grantID, d64, d64, 0, "running", now)
		}
		callID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if operation == "chat" {
			mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, d64, callID, d64)
		} else {
			mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?))`, callID, d64, probeID)
		}
		if state == "succeeded" {
			mustExec(t, db, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"","finishReason":"stop","tool_calls":[]}',?,'stop',?)`, callID, d64, now)
		}
		mustExec(t, db, `UPDATE model_calls SET status=?,ended_at=?,termination_reason=?,usage_json=? WHERE id=?`, state, now, map[int]any{0: nil, 1: nil, 2: "cancelled"}[index], `{"input_tokens":1,"output_tokens":1,"total_tokens":2}`, callID)
	}
	// The typed probe closure requires real succeeded chat+embedding records and
	// one cancelled chat record; these rows are the deterministic provider probe observation.
	resultDigest := fmt.Sprintf("%064x", 2)
	child := &connections.TypedChild{ModelProvider: &connections.ModelProviderProbeChild{ChatModelID: "chat", EmbeddingModelID: ptr("embed"), ContextBudgetTokens: 1024, MaxOutputTokens: 256, StreamingSupported: true, NativeToolCallingSupported: true, MultiToolCallSupported: true, CancellationObserved: true, UsageObserved: true, RequestIDObserved: true, EmbeddingSupported: true, EmbeddingVectorDim: 3, DetailJSON: `{}`}}
	if err := service.CommitProbeResult(ctx, probeID, "fixture-probe", 1, connections.TypedProbeResult{Outcome: "passed", Detail: json.RawMessage(`{}`), ResultDigest: resultDigest, StartedAt: now, FinishedAt: now}, child); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,?,?,?,?)`, summary.ID, summary.RowVersion+1, 1, 1, now)
	mustExec(t, db, `UPDATE connections SET enabled=1,row_version=row_version+1 WHERE id=? AND enabled=0`, summary.ID)
}

func ptr(value string) *string { return &value }

func newBrowserExplorationFixture(t *testing.T, ctx context.Context) (*sql.DB, string) {
	t.Helper()
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: root + "/data", BackupDirectory: root + "/backup", RootKeyFile: root + "/secrets/root-key", RuntimeTLSCertificateFile: root + "/secrets/runtime.crt", RuntimeTLSPrivateKeyFile: root + "/secrets/runtime.key", SteleServiceTokenFile: root + "/secrets/stele-token"}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database.SQL, config.RootKeyFile
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}
func mustExecResult(t *testing.T, db *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	result, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
	return result
}
func browserToolDeliveryCount(sent []*runtimev1.ControlEnvelope) int {
	count := 0
	for _, envelope := range sent {
		if envelope.GetToolResultDelivery() != nil {
			count++
		}
	}
	return count
}

func mustQuery(t *testing.T, db *sql.DB, query string, target any, args ...any) {
	t.Helper()
	if err := db.QueryRow(query, args...).Scan(target); err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
}

func TestTerminalReasonForBrowserActionMapsEveryFrozenTerminalCode(t *testing.T) {
	for code, want := range map[string]string{
		"Cancelled": "cancelled", "LeaseExpired": "lease_expired", "ParentTerminated": "parent_terminal",
		"ArtifactCommitFailed": "artifact_commit_failed", "BrowserCrashed": "browser_crashed",
	} {
		got, ok := terminalReasonForBrowserAction(&runtimev1.BrowserExplorationActionResult{ErrorCode: code})
		if !ok || got != want {
			t.Fatalf("terminalReasonForBrowserAction(%q) = (%q, %t), want (%q, true)", code, got, ok, want)
		}
	}
}

type zeroRowsResult struct{}

func (zeroRowsResult) LastInsertId() (int64, error) { return 0, nil }
func (zeroRowsResult) RowsAffected() (int64, error) { return 0, nil }

func TestBrowserOperationTerminalCASLossIsClassifiedForLateDrop(t *testing.T) {
	if err := requireBrowserOperationTerminalCAS(zeroRowsResult{}, nil); !errors.Is(err, errBrowserOperationTerminalCASLost) {
		t.Fatalf("CAS loss error=%v, want late-result sentinel", err)
	}
}

func TestNormalizeBrowserExplorationTerminalRequiresFrozenSessionID(t *testing.T) {
	result := &runtimev1.BrowserExplorationActionResult{ErrorCode: "BrowserCrashed", ErrorDetail: "browser exited", SessionTerminal: true}
	if _, err := normalizeBrowserExplorationToolResult(result, "read", `{}`); err == nil {
		t.Fatal("terminal non-open result without frozen session ID was accepted")
	}
	normalized, err := normalizeBrowserExplorationToolResult(result, "read", `{"action":"read","sessionId":"12"}`)
	if err != nil {
		t.Fatalf("normalize valid terminal result: %v", err)
	}
	if normalized.toolState != "failed" || attempt.ValidateToolResultPayload("browser_tool_result_v1", normalized.payload) != nil || !bytes.Contains(normalized.payload, []byte(`"sessionId":"12"`)) {
		t.Fatalf("normalized terminal result=%+v payload=%s", normalized, normalized.payload)
	}
}

func TestElementNotInteractableIsSchemaValidRecoverableBrowserResult(t *testing.T) {
	payload := []byte(`{"outcome":"recoverable_error","action":"click","sessionId":"1","observation":{"version":1,"url":"https://example.test/","origin":"https://example.test","title":"Example","pages":[{"pageId":"p","current":true,"url":"https://example.test/","origin":"https://example.test","title":"Example"}],"visibleText":"","accessibilityText":"","elements":[],"events":[],"originalSizeBytes":0,"truncated":false},"error":{"code":"ElementNotInteractable","message":"target is covered","retryableInSession":true}}`)
	if err := attempt.ValidateToolResultPayload("browser_tool_result_v1", payload); err != nil {
		t.Fatalf("ElementNotInteractable is not schema-valid: %v", err)
	}
	if got := browserActionFailure("ElementNotInteractable", false); got != "recoverable_error" {
		t.Fatalf("browser action outcome=%q", got)
	}
}
