package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/proto"
)

// handleBrowserSubExecution accepts only an already-authorized quoin_browser
// call. The child attempt and audit row are written before the Lintel dispatch;
// consequently a control-stream retry can rediscover the same durable child
// rather than starting a second browser action.
func (service *RuntimeService) handleBrowserSubExecution(ctx context.Context, envelope *runtimev1.ControlEnvelope, request *runtimev1.RequestBrowserSubExecution) {
	ack := &runtimev1.BrowserSubExecutionAck{ParentAttemptId: request.GetParentAttemptId(), ToolCallId: request.GetToolCallId()}
	reject := func(reason runtimev1.BrowserSubExecutionRejectReason) {
		ack.RejectReason = reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{ConnectionEpoch: envelope.GetConnectionEpoch(), BootId: envelope.GetBootId(), CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_BrowserSubExecutionAck{BrowserSubExecutionAck: ack}})
	}
	if service.Analyses == nil || request == nil || request.GetParentAttemptId() < 1 || request.GetToolCallId() < 1 || request.GetInput() == nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
		return
	}
	input := request.GetInput()
	if input.GetSchemaKind() != "browser_tool_v1" || len(input.GetCanonicalJson()) == 0 || len(input.GetContentDigest()) != sha256.Size {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
		return
	}
	digest := sha256.Sum256(input.GetCanonicalJson())
	if string(digest[:]) != string(input.GetContentDigest()) {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
		return
	}
	if err := attempt.ValidateToolArguments(attempt.BrowserTool, input.GetCanonicalJson()); err != nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
		return
	}
	db := service.Analyses.DB()
	conn, err := db.Conn(ctx)
	if err != nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
		return
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var attemptType, state, arguments, argumentsDigest, toolName, toolStatus string
	var scopeID int64
	err = conn.QueryRowContext(ctx, `SELECT a.attempt_type,a.state,a.scope_id,t.tool_name,t.status,t.arguments_json,t.arguments_digest
		FROM execution_attempts a JOIN tool_calls t ON t.attempt_id=a.id WHERE a.id=? AND t.id=?`, request.GetParentAttemptId(), request.GetToolCallId()).Scan(&attemptType, &state, &scopeID, &toolName, &toolStatus, &arguments, &argumentsDigest)
	if err != nil || attemptType != "investigation" || toolName != "quoin_browser" || (toolStatus != "running" && toolStatus != "succeeded") || arguments != string(input.GetCanonicalJson()) || argumentsDigest != hex.EncodeToString(digest[:]) {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
		return
	}
	// The unique requester index is the durable idempotency key. Replays return
	// the same child before attempting any session or operation lookup.
	var childID, operationID int64
	// The child is the replay key. It is intentionally created before its
	// action row, because the frozen action trigger requires both operation and
	// child to be Running.
	err = conn.QueryRowContext(ctx, `SELECT id FROM execution_attempts WHERE requested_by_tool_call_id=?`, request.GetToolCallId()).Scan(&childID)
	if err == nil {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
			return
		}
		committed = true
		ack.Accepted, ack.ChildAttemptId = true, childID
		_ = service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{ConnectionEpoch: envelope.GetConnectionEpoch(), BootId: envelope.GetBootId(), CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_BrowserSubExecutionAck{BrowserSubExecutionAck: ack}})
		// The previous control-stream send may have been lost after the durable
		// action row committed. Replays must redispatch that exact child instead
		// of merely returning its ID and leaving it Running forever.
		go func() { _ = service.replayBrowserExplorationChild(context.Background(), childID) }()
		return
	}
	if err != sql.ErrNoRows {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
		return
	}
	// A pending terminal freezes further browser admission. This runs under the
	// same BEGIN IMMEDIATE transaction as child/action creation, so no new
	// obligation can appear after a terminal drain has checked the parent.
	var terminalPending int
	if err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_attempt_terminals WHERE attempt_id=?)`, request.GetParentAttemptId()).Scan(&terminalPending); err != nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
		return
	}
	if state != "Running" || toolStatus != "running" || terminalPending != 0 {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
		return
	}
	var action struct {
		Action            string `json:"action"`
		BusinessSystemKey string `json:"businessSystemKey"`
		SessionID         string `json:"sessionId"`
		PageID            string `json:"pageId"`
		URL               string `json:"url"`
	}
	if err = json.Unmarshal(input.GetCanonicalJson(), &action); err != nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var identityID, revisionID, profileID int64
	var catalogDigest, catalogVersion string
	if action.Action == "open" {
		var identityState string
		err = conn.QueryRowContext(ctx, `SELECT i.id,i.current_revision_id,i.current_profile_generation_id,i.state,r.journey_catalog_digest,r.journey_catalog_version
			FROM browser_identities i JOIN browser_identity_revisions r ON r.id=i.current_revision_id JOIN business_systems b ON b.id=i.business_system_id
			WHERE b.key=? AND b.enabled=1`, action.BusinessSystemKey).Scan(&identityID, &revisionID, &profileID, &identityState, &catalogDigest, &catalogVersion)
		if err != nil || identityState != "Ready" || profileID < 1 {
			code := "ProfileUnavailable"
			if err == nil && identityState == "AuthenticationRequired" {
				code = "AuthenticationRequired"
			}
			payload := browserAdmissionPayload(code, nil)
			if err := service.completeBrowserAdmissionRejection(ctx, conn, request, scopeID, payload, now); err != nil {
				reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
				return
			}
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
				return
			}
			committed = true
			// The production pool is deliberately single-connection. Release this
			// completed transaction before resolving the durable child for delivery.
			conn.Close()
			var child int64
			if err := db.QueryRowContext(ctx, `SELECT id FROM execution_attempts WHERE requested_by_tool_call_id=?`, request.GetToolCallId()).Scan(&child); err != nil {
				return
			}
			ack.Accepted, ack.ChildAttemptId = true, child
			_ = service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{ConnectionEpoch: envelope.GetConnectionEpoch(), BootId: envelope.GetBootId(), CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_BrowserSubExecutionAck{BrowserSubExecutionAck: ack}})
			service.deliverBrowserToolResult(request.GetToolCallId(), child, true, payload, "")
			return
		}
		res, insertErr := conn.ExecContext(ctx, `INSERT INTO browser_operations(identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,state,journey_catalog_digest,journey_catalog_version,requested_at)
			VALUES(?,?,?,?, 'exploration','Queued',?,?,?)`, identityID, revisionID, profileID, request.GetParentAttemptId(), catalogDigest, catalogVersion, now)
		if insertErr != nil {
			// The active-identity unique index is an expected admission outcome,
			// not malformed Tool input. Close the Tool Call and its child with a
			// structured operation-null result so the model can choose another
			// system or retry after the existing session releases the Identity.
			busy, busyErr := currentIdentityBusy(ctx, conn, identityID)
			if busyErr != nil {
				reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
				return
			}
			payload := browserAdmissionPayload("IdentityBusy", busy)
			if err := service.completeBrowserAdmissionRejection(ctx, conn, request, scopeID, payload, now); err != nil {
				reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
				return
			}
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
				return
			}
			committed = true
			conn.Close()
			var child int64
			if err := db.QueryRowContext(ctx, `SELECT id FROM execution_attempts WHERE requested_by_tool_call_id=?`, request.GetToolCallId()).Scan(&child); err != nil {
				return
			}
			ack.Accepted, ack.ChildAttemptId = true, child
			_ = service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{ConnectionEpoch: envelope.GetConnectionEpoch(), BootId: envelope.GetBootId(), CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_BrowserSubExecutionAck{BrowserSubExecutionAck: ack}})
			service.deliverBrowserToolResult(request.GetToolCallId(), child, true, payload, "")
			return
		}
		operationID, _ = res.LastInsertId()
	} else {
		operationID, err = strconv.ParseInt(action.SessionID, 10, 64)
		if err != nil || operationID < 1 {
			reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
			return
		}
		var operationState, identityState string
		err = conn.QueryRowContext(ctx, `SELECT o.identity_id,o.identity_revision_id,o.profile_generation_id,o.journey_catalog_digest,o.journey_catalog_version,o.state,i.state
			FROM browser_operations o JOIN browser_identities i ON i.id=o.identity_id
			WHERE o.id=? AND o.owner_attempt_id=? AND o.kind='exploration'`, operationID, request.GetParentAttemptId()).Scan(&identityID, &revisionID, &profileID, &catalogDigest, &catalogVersion, &operationState, &identityState)
		if err != nil {
			reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
			return
		}
		if operationState != "Queued" && operationState != "WaitingForCapacity" && operationState != "Starting" && operationState != "Running" {
			payload := browserClosedSessionPayload(action.Action, action.SessionID)
			childID, closeErr := service.completeClosedSessionCall(ctx, conn, request, scopeID, operationID, identityID, revisionID, profileID, catalogDigest, catalogVersion, input.GetCanonicalJson(), payload, now)
			if closeErr != nil {
				reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
				return
			}
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
				return
			}
			committed = true
			// The runtime slot projection reads through the production one-connection
			// pool; release this completed transaction before synchronous delivery.
			conn.Close()
			ack.Accepted, ack.ChildAttemptId = true, childID
			_ = service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{ConnectionEpoch: envelope.GetConnectionEpoch(), BootId: envelope.GetBootId(), CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_BrowserSubExecutionAck{BrowserSubExecutionAck: ack}})
			service.deliverBrowserToolResult(request.GetToolCallId(), childID, true, payload, "")
			return
		}
		if identityState != "Ready" {
			reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INPUT_UNSUPPORTED)
			return
		}
	}
	res, err := conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,requested_by_tool_call_id,quoin_release_version,agent_version,created_at)
		VALUES('browser_exploration','investigation',?,'Queued',?,?,?,?)`, scopeID, request.GetToolCallId(), service.ReleaseVersion, nil, now)
	if err != nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
		return
	}
	childID, _ = res.LastInsertId()
	if err = freezeBrowserExplorationInput(ctx, conn, childID, request.GetParentAttemptId(), request.GetToolCallId(), operationID, identityID, revisionID, profileID, catalogDigest, catalogVersion, input.GetCanonicalJson(), now); err != nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
		return
	}
	// This immutable row, rather than browser_operations.owner_attempt_id, is
	// the operation binding for every replay, cancellation and reconnect. One
	// parent Investigation may own several historical Exploration sessions.
	if _, err = conn.ExecContext(ctx, `INSERT INTO browser_exploration_child_bindings(child_attempt_id,tool_call_id,operation_id,parent_attempt_id,created_at)
		VALUES(?,?,?,?,?)`, childID, request.GetToolCallId(), operationID, request.GetParentAttemptId(), now); err != nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
		return
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		reject(runtimev1.BrowserSubExecutionRejectReason_BROWSER_SUB_EXECUTION_REJECT_REASON_INTERNAL)
		return
	}
	committed = true
	ack.Accepted, ack.ChildAttemptId = true, childID
	_ = service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{ConnectionEpoch: envelope.GetConnectionEpoch(), BootId: envelope.GetBootId(), CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_BrowserSubExecutionAck{BrowserSubExecutionAck: ack}})
	// open needs Chromium started before its child can become Running; later
	// calls attach to the same already-Running operation.
	if action.Action == "open" && service.Slots != nil {
		go func() { _ = service.dispatchBrowserOperation(context.Background(), operationID) }()
	} else {
		go func() { _ = service.dispatchBrowserExplorationAction(context.Background(), childID) }()
	}
}

// completeBrowserAdmissionRejection records the only operation-less browser child
// terminal path. It deliberately creates no action or operation: the frozen
// SQL trigger admits only the four schema-valid admission rejection shapes.
// completeClosedSessionCall writes a terminal child tombstone when Plinth
// retries a model-approved action after that session has already ended. It is
// deliberately a model-visible result rather than a transport rejection: the
// durable operation is the authority that the supplied session can no longer
// execute, and the child snapshot preserves the tool-call causal history.
func (service *RuntimeService) completeClosedSessionCall(ctx context.Context, conn *sql.Conn, request *runtimev1.RequestBrowserSubExecution, scopeID, operationID, identityID, revisionID, profileID int64, catalogDigest, catalogVersion string, input, payload []byte, now string) (int64, error) {
	result, err := conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,requested_by_tool_call_id,quoin_release_version,created_at)
		VALUES('browser_exploration','investigation',?,'Queued',?,?,?)`, scopeID, request.GetToolCallId(), service.ReleaseVersion, now)
	if err != nil {
		return 0, err
	}
	childID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := freezeBrowserExplorationInput(ctx, conn, childID, request.GetParentAttemptId(), request.GetToolCallId(), operationID, identityID, revisionID, profileID, catalogDigest, catalogVersion, input, now); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO browser_exploration_child_bindings(child_attempt_id,tool_call_id,operation_id,parent_attempt_id,created_at)
		VALUES(?,?,?,?,?)`, childID, request.GetToolCallId(), operationID, request.GetParentAttemptId(), now); err != nil {
		return 0, err
	}
	if err := attempt.ValidateToolResultPayload("browser_tool_result_v1", payload); err != nil {
		return 0, fmt.Errorf("invalid closed-session browser result: %w", err)
	}
	result, err = conn.ExecContext(ctx, `UPDATE tool_calls SET status='succeeded',ended_at=?,result_json=?,row_version=row_version+1
		WHERE id=? AND status='running'`, now, string(payload), request.GetToolCallId())
	if err != nil {
		return 0, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return 0, fmt.Errorf("closed-session browser tool call is no longer running")
	}
	return childID, nil
}

func browserClosedSessionPayload(action, sessionID string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"outcome": "session_closed", "action": action, "sessionId": sessionID,
		"error": map[string]any{"code": "ParentTerminated", "message": "browser session is already closed", "retryableInSession": false},
	})
	return payload
}

func (service *RuntimeService) completeBrowserAdmissionRejection(ctx context.Context, conn *sql.Conn, request *runtimev1.RequestBrowserSubExecution, scopeID int64, payload []byte, now string) error {
	result, err := conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,requested_by_tool_call_id,quoin_release_version,created_at)
		VALUES('browser_exploration','investigation',?,'Queued',?,?,?)`, scopeID, request.GetToolCallId(), service.ReleaseVersion, now)
	if err != nil {
		return err
	}
	childID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if attempt.ValidateToolResultPayload("browser_tool_result_v1", payload) != nil {
		return fmt.Errorf("invalid frozen browser admission result")
	}
	result, err = conn.ExecContext(ctx, `UPDATE tool_calls SET status='succeeded',ended_at=?,result_json=?,row_version=row_version+1
		WHERE id=? AND status='running'`, now, string(payload), request.GetToolCallId())
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("browser admission tool call is no longer running")
	}
	_ = childID // child is atomically terminalized by trg_tool_calls_close_browser_child.
	return nil
}

type identityBusy struct {
	OperationID int64
	Kind        string
	OccupiedAt  string
}

// currentIdentityBusy projects the one durable operation that owns this
// identity. IdentityBusy is not retryable in the current browser session: no
// session has been admitted, and the model needs the concrete occupancy facts
// to decide whether to wait or use another business system.
func currentIdentityBusy(ctx context.Context, conn *sql.Conn, identityID int64) (*identityBusy, error) {
	var busy identityBusy
	err := conn.QueryRowContext(ctx, `SELECT id,kind,COALESCE(started_at,start_dispatched_at,requested_at)
		FROM browser_operations WHERE identity_id=?
		  AND (state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR stop_confirmed_at IS NULL)
		ORDER BY id DESC LIMIT 1`, identityID).Scan(&busy.OperationID, &busy.Kind, &busy.OccupiedAt)
	if err != nil {
		return nil, err
	}
	return &busy, nil
}

func browserAdmissionPayload(code string, busy *identityBusy) []byte {
	// Admission failures happen before a Browser Session exists. They can never
	// be retried in-session; IdentityBusy remains a model-visible recoverable
	// admission outcome because choosing another system or waiting can help.
	retryable := false
	outcome := "session_closed"
	if code == "IdentityBusy" {
		outcome = "recoverable_error"
	}
	errorPayload := map[string]any{"code": code, "message": "browser admission rejected", "retryableInSession": retryable}
	if code == "IdentityBusy" && busy != nil {
		errorPayload["identityBusy"] = map[string]any{"currentOperationId": busy.OperationID, "operationKind": busy.Kind, "occupiedSince": busy.OccupiedAt}
	}
	payload, _ := json.Marshal(map[string]any{"outcome": outcome, "action": "open", "error": errorPayload})
	return payload
}

// closeRejectedExplorationStart turns a rejected physical Exploration start into
// the parent Tool Call's durable, model-visible result. The Tool Call trigger is
// the sole writer of the queued child terminal state.
func (service *RuntimeService) closeRejectedExplorationStart(ctx context.Context, operationID int64, rejectReason, detail string) {
	if service.Analyses == nil || operationID < 1 {
		return
	}
	code := "RuntimeUnavailable"
	admission := true
	switch rejectReason {
	case "identity_busy":
		code = "IdentityBusy"
	case "profile_unavailable":
		code = "ProfileUnavailable"
	case "authentication_required":
		code = "AuthenticationRequired"
	case "download_blocked":
		code = "DownloadBlocked"
	case "input_unsupported", "reconcile_required", "stale_stream":
		// Any Lintel rejection of Quoin's frozen operation envelope is a
		// protocol/schema violation, never an infrastructure outage.
		code, admission = "ProtocolError", false
	default:
		// A post-dispatch protocol/reconcile/internal rejection is not an
		// operation-less admission result. It must close lawfully as a failed
		// Tool Call, otherwise the frozen admission trigger would reject the
		// fabricated RuntimeUnavailable success and strand its queued child.
		admission = false
	}
	payload := browserAdmissionPayload(code, nil)
	conn, err := service.Analyses.DB().Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var toolID, childID int64
	err = conn.QueryRowContext(ctx, `SELECT b.tool_call_id,b.child_attempt_id
		FROM browser_exploration_child_bindings b
		JOIN tool_calls t ON t.id=b.tool_call_id
		JOIN execution_attempts c ON c.id=b.child_attempt_id
		WHERE b.operation_id=? AND t.status='running' AND c.state='Queued'
		ORDER BY b.child_attempt_id LIMIT 1`, operationID).Scan(&toolID, &childID)
	if err != nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	toolState := "succeeded"
	if !admission {
		toolState = "failed"
		payload, _ = json.Marshal(map[string]any{
			"outcome": "session_closed", "action": "open",
			"error": map[string]any{"code": code, "message": "browser operation start rejected", "retryableInSession": false},
		})
	}
	result, err := conn.ExecContext(ctx, `UPDATE tool_calls SET status=?,ended_at=?,result_json=?,error_detail=?,row_version=row_version+1
		WHERE id=? AND status='running'`, toolState, now, string(payload), nullableText(detail), toolID)
	if err != nil {
		return
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return
	}
	committed = true
	conn.Close()
	service.deliverBrowserToolResult(toolID, childID, admission, payload, nullableString(detail))
	// A rejected Start has conclusively released Lintel capacity for this
	// operation. Advance the durable global FIFO now rather than waiting for an
	// unrelated StopAck that will never arrive for a rejected start.
	go service.dispatchQueuedBrowserOperations(context.Background())
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableString(value string) string { return value }

type normalizedBrowserExplorationToolResult struct {
	outcome, toolState     string
	payload                []byte
	errorCode, errorDetail any
}

// normalizeBrowserExplorationToolResult is the sole boundary that converts a
// Lintel action result into the durable, model-visible Browser Tool schema.
// In particular, a terminal result without a payload must still carry the
// immutable session ID for every non-open action; accepting an unvalidated
// synthesized failure would poison the Tool Call ledger with an unverifiable
// result.
func normalizeBrowserExplorationToolResult(result *runtimev1.BrowserExplorationActionResult, actionKind, argumentsJSON string) (normalizedBrowserExplorationToolResult, error) {
	if result == nil || actionKind == "" {
		return normalizedBrowserExplorationToolResult{}, errors.New("browser action result is incomplete")
	}
	if payload := result.GetPayload(); payload != nil {
		if payload.GetSchemaKind() != "browser_tool_result_v1" || len(payload.GetContentDigest()) != sha256.Size {
			return normalizedBrowserExplorationToolResult{}, errors.New("browser result payload envelope is invalid")
		}
		digest := sha256.Sum256(payload.GetCanonicalJson())
		if string(digest[:]) != string(payload.GetContentDigest()) || attempt.ValidateToolResultPayload(payload.GetSchemaKind(), payload.GetCanonicalJson()) != nil {
			return normalizedBrowserExplorationToolResult{}, errors.New("browser result payload is invalid")
		}
		var body struct {
			Action  string `json:"action"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(payload.GetCanonicalJson(), &body); err != nil || body.Action != actionKind {
			return normalizedBrowserExplorationToolResult{}, errors.New("browser result action does not match frozen action")
		}
		if result.GetSuccess() && body.Outcome != "success" {
			return normalizedBrowserExplorationToolResult{}, errors.New("successful browser result has non-success outcome")
		}
		if !result.GetSuccess() && (body.Outcome != "recoverable_error" || result.GetSessionTerminal() || result.GetErrorCode() == "" || result.GetErrorDetail() == "") {
			return normalizedBrowserExplorationToolResult{}, errors.New("recoverable browser result is malformed")
		}
		normalized := normalizedBrowserExplorationToolResult{outcome: body.Outcome, toolState: "succeeded", payload: payload.GetCanonicalJson()}
		if body.Outcome == "recoverable_error" {
			normalized.errorCode = result.GetErrorCode()
		}
		return normalized, nil
	}
	if result.GetSuccess() || !result.GetSessionTerminal() || result.GetErrorCode() == "" || result.GetErrorDetail() == "" {
		return normalizedBrowserExplorationToolResult{}, errors.New("terminal browser result is malformed")
	}
	body := map[string]any{
		"outcome": "session_closed",
		"action":  actionKind,
		"error":   map[string]any{"code": result.GetErrorCode(), "message": result.GetErrorDetail(), "retryableInSession": false},
	}
	if actionKind != "open" {
		var arguments struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal([]byte(argumentsJSON), &arguments); err != nil || strings.TrimSpace(arguments.SessionID) == "" {
			return normalizedBrowserExplorationToolResult{}, errors.New("terminal browser result lacks frozen session id")
		}
		body["sessionId"] = arguments.SessionID
	}
	payload, err := json.Marshal(body)
	if err != nil || attempt.ValidateToolResultPayload("browser_tool_result_v1", payload) != nil {
		return normalizedBrowserExplorationToolResult{}, errors.New("synthesized browser terminal payload is invalid")
	}
	return normalizedBrowserExplorationToolResult{outcome: "session_closed", toolState: "failed", payload: payload, errorCode: result.GetErrorCode(), errorDetail: result.GetErrorDetail()}, nil
}

// freezeBrowserExplorationInput creates the child Attempt's independent,
// immutable input lineage before it is eligible for Lintel dispatch. The
// parent Tool Call remains the authority for the original request; the child
// snapshot binds that frozen request to the exact browser identity/profile and
// catalog facts selected in the same transaction.
func freezeBrowserExplorationInput(ctx context.Context, conn *sql.Conn, childID, parentAttemptID, toolCallID, operationID, identityID, revisionID, profileID int64, catalogDigest, catalogVersion string, request []byte, now string) error {
	var parsed json.RawMessage
	if !json.Valid(request) {
		return fmt.Errorf("browser request is not JSON")
	}
	parsed = append(parsed, request...)
	canonical, err := json.Marshal(struct {
		SchemaKind          string            `json:"schemaKind"`
		OperationID         int64             `json:"operationId"`
		IdentityID          int64             `json:"identityId"`
		IdentityRevisionID  int64             `json:"identityRevisionId"`
		ProfileGenerationID int64             `json:"profileGenerationId"`
		ParentAttemptID     int64             `json:"parentAttemptId"`
		ToolCallID          int64             `json:"toolCallId"`
		Catalog             map[string]string `json:"catalog"`
		Request             json.RawMessage   `json:"request"`
	}{
		SchemaKind: "browser_exploration_v1", OperationID: operationID,
		IdentityID: identityID, IdentityRevisionID: revisionID, ProfileGenerationID: profileID,
		ParentAttemptID: parentAttemptID, ToolCallID: toolCallID,
		Catalog: map[string]string{"digest": catalogDigest, "version": catalogVersion}, Request: parsed,
	})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	result, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?,?,?,?)`, childID, "browser_exploration_v1", "browser_tool_v1", hex.EncodeToString(digest[:]), now)
	if err != nil {
		return err
	}
	snapshotID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	// A child cannot invent a second source of truth. It inherits one existing
	// immutable lineage reference from its already-dispatched parent snapshot;
	// the child-specific canonical digest above carries the Tool Call and browser
	// bindings that make that reference's use unambiguous.
	result, err = conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,
		occurrence_id,initial_analysis_id,investigation_message_id,evidence_id,artifact_id,knowledge_version_id,
		inspection_run_id,inspection_check_result_id,source_material_id,business_system_config_version_id,
		label_contract_version_id,knowledge_import_batch_id,embedding_generation_id,connection_revision_id)
		SELECT ?,1,item_role,source_digest,
		occurrence_id,initial_analysis_id,investigation_message_id,evidence_id,artifact_id,knowledge_version_id,
		inspection_run_id,inspection_check_result_id,source_material_id,business_system_config_version_id,
		label_contract_version_id,knowledge_import_batch_id,embedding_generation_id,connection_revision_id
		FROM attempt_input_items
		WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?)
		ORDER BY item_seq LIMIT 1`, snapshotID, parentAttemptID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("parent attempt %d has no frozen input lineage", parentAttemptID)
	}
	return nil
}

// handleBrowserExplorationTerminalClaim is the durable pre-upload ordering
// point for a normal complete trace. It intentionally has no artifact fields:
// accepting the claim authorizes one later complete upload; cancellation that
// committed first makes the guarded insert impossible.
func (service *RuntimeService) handleBrowserExplorationTerminalClaim(ctx context.Context, envelope *runtimev1.ControlEnvelope, claim *runtimev1.BrowserExplorationTerminalClaim) {
	if service.Analyses == nil || service.Slots == nil || envelope == nil || claim == nil || claim.GetOperationId() < 1 || claim.GetChildAttemptId() < 1 || claim.GetParentAttemptId() < 1 || claim.GetToolCallId() < 1 {
		return
	}
	if service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error { return nil }) != nil {
		return
	}
	ack := &runtimev1.BrowserExplorationTerminalClaimAck{OperationId: claim.GetOperationId(), ChildAttemptId: claim.GetChildAttemptId(), ToolCallId: claim.GetToolCallId()}
	conn, err := service.Analyses.DB().Conn(ctx)
	if err == nil {
		defer conn.Close()
		if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err == nil {
			committed := false
			defer func() {
				if !committed {
					_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
				}
			}()
			var childState, parentState, toolState, operationState string
			var childBoot string
			var childEpoch uint64
			err = conn.QueryRowContext(ctx, `SELECT child.state,parent.state,t.status,o.state,child.boot_id,child.connection_epoch
				FROM browser_exploration_actions action
				JOIN execution_attempts child ON child.id=action.child_attempt_id
				JOIN execution_attempts parent ON parent.id=?
				JOIN tool_calls t ON t.id=action.tool_call_id
				JOIN browser_operations o ON o.id=action.operation_id
				WHERE action.child_attempt_id=? AND action.tool_call_id=? AND action.operation_id=?`, claim.GetParentAttemptId(), claim.GetChildAttemptId(), claim.GetToolCallId(), claim.GetOperationId()).Scan(&childState, &parentState, &toolState, &operationState, &childBoot, &childEpoch)
			// A pending natural parent terminal and a close claim are both durable
			// pre-terminal work items. They must compose: pending-first still allows
			// the one complete trace to seal, and claim-first leaves the parent Running
			// until that same pending result drains. Cancellation is the only losing
			// state for a claim, decided by this transaction's SQLite order.
			if err == nil && childBoot == envelope.GetBootId() && envelope.GetConnectionEpoch() >= childEpoch && childState == "Running" && parentState == "Running" && toolState == "running" && operationState == "Running" {
				_, err = conn.ExecContext(ctx, `INSERT INTO browser_exploration_terminal_claims(child_attempt_id,operation_id,tool_call_id,state,created_at)
					VALUES(?,?,?,'claimed_complete',?) ON CONFLICT(child_attempt_id) DO NOTHING`, claim.GetChildAttemptId(), claim.GetOperationId(), claim.GetToolCallId(), time.Now().UTC().Format(time.RFC3339Nano))
				if err == nil {
					var state string
					err = conn.QueryRowContext(ctx, `SELECT state FROM browser_exploration_terminal_claims WHERE child_attempt_id=? AND operation_id=? AND tool_call_id=?`, claim.GetChildAttemptId(), claim.GetOperationId(), claim.GetToolCallId()).Scan(&state)
					ack.Accepted = err == nil && state == "claimed_complete"
					if !ack.Accepted {
						ack.Detail = "terminal claim is no longer complete"
					}
				}
			} else if err == nil {
				ack.Detail = "parent cancellation or terminal transition already committed"
			}
			if _, commitErr := conn.ExecContext(ctx, "COMMIT"); commitErr == nil {
				committed = true
			} else {
				err = commitErr
			}
		}
	}
	if !ack.Accepted && ack.Detail == "" {
		ack.Detail = "terminal claim could not be committed"
	}
	_ = service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: envelope.GetBootId(), ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetMessageId(), Msg: &runtimev1.ControlEnvelope_BrowserExplorationTerminalClaimAck{BrowserExplorationTerminalClaimAck: ack}})
}

// handleBrowserExplorationActionResult seals the durable child/action first and
// only then returns the model-visible payload to Plinth. Replays reconstruct
// the same delivery from the parent tool-call row.
func (service *RuntimeService) handleBrowserExplorationActionResult(ctx context.Context, envelope *runtimev1.ControlEnvelope, result *runtimev1.BrowserExplorationActionResult) {
	if service.Analyses == nil || service.Slots == nil || result == nil || result.GetChildAttemptId() < 1 || result.GetToolCallId() < 1 {
		return
	}
	if envelope == nil || service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error { return nil }) != nil {
		return
	}
	payloadAction, payloadOutcome := "", ""
	if payload := result.GetPayload(); payload != nil {
		// Parse only enough routing metadata to locate the immutable action row.
		// normalizeBrowserExplorationToolResult is the sole schema/digest validator
		// immediately before any browser Tool result reaches durable state.
		var value struct {
			Action  string `json:"action"`
			Outcome string `json:"outcome"`
		}
		if json.Unmarshal(payload.GetCanonicalJson(), &value) != nil {
			return
		}
		payloadAction, payloadOutcome = value.Action, value.Outcome
	}
	// A recoverable Browser error deliberately carries a schema-valid
	// observation and returns to the model as a *succeeded* Tool Call. Terminal
	// errors instead carry no payload and close the exploration session.
	if result.GetSuccess() {
		if result.GetPayload() == nil || payloadOutcome != "success" {
			return
		}
	} else if result.GetPayload() != nil {
		if payloadOutcome != "recoverable_error" || result.GetSessionTerminal() || result.GetErrorCode() == "" || result.GetErrorDetail() == "" {
			return
		}
	} else if result.GetErrorCode() == "" || result.GetErrorDetail() == "" || !result.GetSessionTerminal() {
		return
	}
	resultDigest, err := browserExplorationResultDigest(result)
	if err != nil {
		return
	}
	db := service.Analyses.DB()
	conn, err := db.Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	// The start fence is immutable audit history. A result may arrive after a
	// same-boot reconnect, but accepting that successor-epoch delivery must not
	// rewrite either the child dispatch or operation-start epoch. The trace upload
	// ledger proves its own authenticated stream separately.
	var actionID, parentID, operationRevisionID int64
	var state, actionKind, toolStatus, actionOutcome, actionErrorCode, argumentsJSON, childBoot, operationState, operationTerminalReason string
	var pendingTerminal int
	var terminalClaim string
	var terminalClaimArtifactID int64
	var storedResultDigest []byte
	var childEpoch uint64
	err = conn.QueryRowContext(ctx, `SELECT x.id,t.attempt_id,o.identity_revision_id,a.state,x.action_kind,t.status,COALESCE(x.outcome,''),COALESCE(x.error_code,''),x.result_digest,t.arguments_json,a.boot_id,a.connection_epoch,o.state,COALESCE(o.terminal_reason,''),
			EXISTS(SELECT 1 FROM pending_attempt_terminals p WHERE p.attempt_id=t.attempt_id),
			COALESCE((SELECT claim.state FROM browser_exploration_terminal_claims claim WHERE claim.child_attempt_id=x.child_attempt_id),''),
			COALESCE((SELECT claim.trace_artifact_id FROM browser_exploration_terminal_claims claim WHERE claim.child_attempt_id=x.child_attempt_id),0)
		FROM browser_exploration_actions x JOIN execution_attempts a ON a.id=x.child_attempt_id JOIN tool_calls t ON t.id=x.tool_call_id JOIN browser_operations o ON o.id=x.operation_id
		WHERE x.child_attempt_id=? AND x.tool_call_id=? AND x.operation_id=?`, result.GetChildAttemptId(), result.GetToolCallId(), result.GetOperationId()).Scan(&actionID, &parentID, &operationRevisionID, &state, &actionKind, &toolStatus, &actionOutcome, &actionErrorCode, &storedResultDigest, &argumentsJSON, &childBoot, &childEpoch, &operationState, &operationTerminalReason, &pendingTerminal, &terminalClaim, &terminalClaimArtifactID)
	// A terminal ActionResult may be replayed on a successor connection of the
	// same Lintel boot. The immutable child dispatch epoch is a lower bound,
	// not an equality requirement; accepting an earlier epoch would let a stale
	// stream manufacture a terminal outcome.
	if err != nil || parentID != result.GetParentAttemptId() || childBoot != envelope.GetBootId() || envelope.GetConnectionEpoch() < childEpoch {
		return
	}
	if result.GetPayload() != nil && payloadAction != actionKind {
		return
	}
	if actionOutcome != "" {
		exactReplay := string(storedResultDigest) == string(resultDigest)
		// Quoin restart/new-Lintel-boot recovery is itself a durable terminal
		// writer: it seals the action, child Tool Call, and operation together
		// with ParentTerminated. A terminal message which Lintel had already
		// produced but could not deliver before that recovery must be acknowledged
		// so the Lintel result cache can clear. It is deliberately not allowed to
		// replace any of those durable facts or create a second ToolResult.
		recoverySupersedes := result.GetSessionTerminal() &&
			actionOutcome == "session_closed" && actionErrorCode == "ParentTerminated" &&
			(state == "Failed" || state == "Interrupted") && toolStatus == "failed" &&
			operationState == "Interrupted" && (operationTerminalReason == "shutdown" || operationTerminalReason == "new_boot")
		if !exactReplay && !recoverySupersedes {
			return
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return
		}
		// The first terminal writer created the one model delivery. A late Action
		// Result is a stale replay, not permission to emit a duplicate ToolResult.
		committed = true
		if recoverySupersedes && !exactReplay {
			service.acknowledgeBrowserExplorationAction(envelope, result, true, "superseded by durable recovery terminal")
		} else {
			service.acknowledgeBrowserExplorationAction(envelope, result, true, "already committed")
		}
		return
	}
	// A crash/new-boot terminalization can win while Lintel is still finishing a
	// nonterminal action. Its stale observation must not resurrect the tool call
	// or overwrite action audit facts; acknowledge the exact replay so Lintel can
	// discard it rather than retry forever.
	if !result.GetSessionTerminal() && operationState != "Running" {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return
		}
		committed = true
		service.acknowledgeBrowserExplorationAction(envelope, result, true, "operation already terminal")
		return
	}
	// Parent cancellation atomically changes its browser Tool Call to cancelled
	// and fences its child to Cancelling. Lintel still owns the action's terminal
	// outcome; after that durable action/operation fact commits, this handler
	// closes the already-fenced child and converges the parent through its one
	// CancelAck. Normal results may only complete a running Tool Call.
	// Parent cancellation owns the Tool Call state. Lintel may either produce its
	// normal Cancelled terminal result or report ArtifactCommitFailed when it
	// cannot persist the mandatory incomplete trace; both must converge the
	// already-Cancelling child instead of leaving it stranded forever.
	// A cancelled parent owns its Tool Call before Lintel has finished the
	// mandatory terminal trace. Any Lintel terminal outcome (including a crash
	// observed while stopping) must therefore close the already-Cancelling child;
	// restricting this to two error codes strands the child when the same cancel
	// race is classified differently by the browser boundary.
	parentCancellation := (state == "Cancelling" && toolStatus == "cancelled" || pendingTerminal != 0 && toolStatus == "cancelled") && result.GetSessionTerminal()
	if !parentCancellation && ((state != "Running" && state != "Cancelling") || toolStatus != "running") {
		return
	}
	// The inbound result may describe a complete trace which Lintel committed
	// before it learned of the parent fence. That Artifact is immutable evidence,
	// not a permission to supersede a cancellation that SQLite committed first.
	// Canonicalize durable action/operation facts to the parent-owned Cancelled
	// outcome while acknowledging the original wire result for Lintel replay.
	// A complete wire result cannot be coerced to incomplete by changing only its
	// enum: its referenced Artifact is immutable complete history. Leave it
	// unacknowledged so Lintel's already-delivered cancellation fence replaces it
	// with a separately uploaded incomplete trace.
	if parentCancellation && result.GetTraceIntegrity() == runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE {
		return
	}
	terminalResult := result
	if parentCancellation && !(result.GetErrorCode() == "ArtifactCommitFailed" && result.GetTraceArtifactId() == 0) {
		terminalResult = proto.Clone(result).(*runtimev1.BrowserExplorationActionResult)
		terminalResult.Success = false
		terminalResult.Payload = nil
		terminalResult.ErrorCode = "Cancelled"
		terminalResult.ErrorDetail = "parent cancellation committed before browser action result"
		terminalResult.TerminalOutcome = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_CANCELLED
		terminalResult.TerminalReason = runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_CANCELLED
		terminalResult.TraceIntegrity = runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	normalized, normalizeErr := normalizeBrowserExplorationToolResult(terminalResult, actionKind, argumentsJSON)
	if normalizeErr != nil {
		return
	}
	outcome, toolState, payload := normalized.outcome, normalized.toolState, normalized.payload
	errorCode, errorDetail := normalized.errorCode, normalized.errorDetail
	// Admission observations are durable diagnostic facts even when the open
	// action fails. Previously only a successful Authenticated probe was stored,
	// making Unauthenticated/Indeterminate admission failures indistinguishable
	// from a Lintel crash before probing.
	if actionKind == "open" {
		probe := result.GetAdmissionProbe()
		if probe == nil {
			if outcome == "success" {
				return
			}
		} else {
			probeResult, probeReason, observed, valid := probeResultForExploration(probe, "admission")
			if !valid || (outcome == "success" && probeResult != "Authenticated") {
				return
			}
			if _, err = conn.ExecContext(ctx, `INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,reason_code,observed_at)
				SELECT ?,COALESCE(MAX(probe_seq),0)+1,'admission',?,?,?,?,?,?,?,? FROM browser_probe_results WHERE operation_id=?`, result.GetOperationId(), operationRevisionID, probe.GetJourneyId(), probe.GetJourneyVersion(), probe.GetJourneyCatalogDigest(), probe.GetJourneyCatalogVersion(), probeResult, nullableText(probeReason), observed.Format(time.RFC3339Nano), result.GetOperationId()); err != nil {
				return
			}
		}
	}
	if terminalResult.GetSessionTerminal() {
		// Complete trace artifacts are legal only after the pre-upload claim won
		// the SQLite ordering race. Conversely a normal claim followed by crash
		// must be finalized as incomplete before the terminal result is accepted.
		if terminalResult.GetTraceIntegrity() == runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE && terminalClaim != "claimed_complete" && terminalClaim != "artifact_committed_complete" {
			return
		}
		if (terminalClaim == "claimed_complete" || terminalClaim == "artifact_committed_complete") && terminalResult.GetTraceIntegrity() != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE {
			// Preserve an already-installed complete Artifact in explicit historical
			// columns. The incoming terminal result must reference a different,
			// actually-incomplete Artifact; do not make the old Artifact appear
			// incomplete by merely changing claim/operation metadata.
			if terminalClaim == "artifact_committed_complete" && (terminalResult.GetTraceArtifactId() < 1 || terminalResult.GetTraceArtifactId() == terminalClaimArtifactID) {
				return
			}
			if _, err = conn.ExecContext(ctx, `UPDATE browser_exploration_terminal_claims
				SET state='downgraded_incomplete',
				    historical_complete_trace_artifact_id=CASE WHEN state='artifact_committed_complete' THEN trace_artifact_id ELSE NULL END,
				    historical_complete_trace_digest=CASE WHEN state='artifact_committed_complete' THEN trace_digest ELSE NULL END,
				    trace_artifact_id=NULL,trace_digest=NULL,finalized_at=?
				WHERE child_attempt_id=? AND state IN ('claimed_complete','artifact_committed_complete')`, now, result.GetChildAttemptId()); err != nil {
				return
			}
		}
		if err = service.persistTerminalExplorationResult(ctx, conn, terminalResult, actionKind, operationRevisionID, envelope.GetBootId(), envelope.GetConnectionEpoch(), now); err != nil {
			if errors.Is(err, errBrowserOperationTerminalCASLost) {
				if _, commitErr := conn.ExecContext(ctx, "COMMIT"); commitErr != nil {
					return
				}
				committed = true
				service.acknowledgeBrowserExplorationAction(envelope, result, true, "operation terminal CAS lost")
			}
			return
		}
		if terminalClaim == "artifact_committed_complete" && terminalResult.GetTraceIntegrity() == runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE {
			if _, err = conn.ExecContext(ctx, `UPDATE browser_exploration_terminal_claims SET state='committed_complete',finalized_at=? WHERE child_attempt_id=? AND state='artifact_committed_complete'`, now, result.GetChildAttemptId()); err != nil {
				return
			}
		}
	}
	observationVersion, observationDigest, observationSize, observedPageID, observedOrigin, observationErr := browserObservationAudit(payload)
	if observationErr != nil {
		// A successful or recoverable browser Tool result always carries the
		// bounded Observation that was actually rendered. Never use the whole
		// Tool payload (or a hard-coded version) as a substitute audit record.
		if !(outcome == "session_closed" && result.GetPayload() == nil) {
			return
		}
		observationVersion, observationDigest, observationSize, observedPageID, observedOrigin = nil, nil, nil, nil, nil
	}
	// Screenshot bytes are uploaded as a browser-operation-owned Artifact by
	// Lintel. Persist only its authoritative ID beside the canonical observation
	// digest; the raw image and page content never enter the action ledger.
	var screenshotArtifactID any
	if actionKind == "screenshot" && outcome == "success" {
		var screenshot struct {
			ScreenshotArtifactID int64 `json:"screenshotArtifactId"`
		}
		if json.Unmarshal(payload, &screenshot) != nil || screenshot.ScreenshotArtifactID < 1 {
			return
		}
		var bound int
		// The screenshot ID in the result is only a reference. Bind it to the
		// exact committed Lintel upload for this child/operation and its immutable
		// start fence before recording it in the action ledger.
		if err = conn.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM artifacts artifact
			JOIN runtime_artifact_uploads upload ON upload.artifact_id=artifact.id
			WHERE artifact.id=? AND artifact.kind='screenshot'
			  AND artifact.owner_type='browser_operation' AND artifact.owner_id=?
			  AND upload.state='committed' AND upload.kind='screenshot'
			  AND upload.owner_type='browser_operation' AND upload.owner_id=?
			  AND upload.attempt_id=? AND upload.boot_id=? AND upload.connection_epoch<=?
		)`, screenshot.ScreenshotArtifactID, result.GetOperationId(), result.GetOperationId(), result.GetChildAttemptId(), childBoot, envelope.GetConnectionEpoch()).Scan(&bound); err != nil || bound != 1 {
			return
		}
		screenshotArtifactID = screenshot.ScreenshotArtifactID
	}
	if _, err = conn.ExecContext(ctx, `UPDATE browser_exploration_actions SET ended_at=?,outcome=?,error_code=?,result_digest=?,observation_version=?,observation_digest=?,observation_size_bytes=?,observed_page_id=?,observed_origin=?,screenshot_artifact_id=? WHERE id=? AND outcome IS NULL`, now, outcome, errorCode, resultDigest, observationVersion, observationDigest, observationSize, observedPageID, observedOrigin, screenshotArtifactID, actionID); err != nil {
		return
	}
	if !parentCancellation {
		if _, err = conn.ExecContext(ctx, `UPDATE tool_calls SET status=?,ended_at=?,result_json=?,error_detail=?,row_version=row_version+1 WHERE id=? AND status='running'`, toolState, now, string(payload), errorDetail, result.GetToolCallId()); err != nil {
			return
		}
	}
	// Parent cancellation already owns the Tool Call. The frozen
	// trg_browser_exploration_action_closes_cancelling_child trigger observes
	// this action's durable session_closed outcome and performs the only child
	// terminal transition in the same transaction.
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return
	}
	committed = true
	// The production pool is intentionally single-connection. Release this
	// transaction connection before projecting the delivery through Slots.View,
	// which must read the current Plinth fence from the same pool.
	conn.Close()
	if parentCancellation {
		// No ToolResult is deliverable after the parent cancelled its model loop.
		// This re-reads every child state before issuing the sole parent CancelAck.
		go service.finalizeCancellation(context.Background(), parentID, "investigation")
	} else {
		// A recoverable browser error is a successful Tool delivery: its typed
		// observation is precisely what lets the parent model choose the next action.
		service.deliverBrowserToolResult(result.GetToolCallId(), result.GetChildAttemptId(), result.GetSuccess() || payloadOutcome == "recoverable_error", payload, result.GetErrorDetail())
	}
	service.acknowledgeBrowserExplorationAction(envelope, result, true, "committed")
	if terminalResult.GetSessionTerminal() {
		go func() { _ = service.dispatchBrowserStop(context.Background(), terminalResult.GetOperationId()) }()
	}
}

func browserExplorationResultDigest(result *runtimev1.BrowserExplorationActionResult) ([]byte, error) {
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(result)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	return sum[:], nil
}

func (service *RuntimeService) acknowledgeBrowserExplorationAction(envelope *runtimev1.ControlEnvelope, result *runtimev1.BrowserExplorationActionResult, accepted bool, detail string) {
	if envelope == nil || result == nil {
		return
	}
	ack := &runtimev1.BrowserExplorationActionResultAck{OperationId: result.GetOperationId(), ChildAttemptId: result.GetChildAttemptId(), ToolCallId: result.GetToolCallId(), Accepted: accepted, Detail: detail}
	if digest, err := browserExplorationResultDigest(result); err == nil {
		ack.ResultDigest = digest
	} else {
		// A result that cannot be deterministically marshalled cannot be safely
		// acknowledged because Lintel would be unable to bind the replay.
		return
	}
	_ = service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: envelope.GetBootId(), ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetMessageId(), Msg: &runtimev1.ControlEnvelope_BrowserExplorationActionResultAck{BrowserExplorationActionResultAck: ack}})
}

// replayUndeliveredBrowserToolResults reconstructs ToolResultDelivery from the
// terminal Tool Call row. A delivery is considered durably consumed only once
// a subsequent model-call context item references it; an in-memory Ack cannot
// be the authority because Quoin and Plinth can both restart between Ack and
// the next model call.
func (service *RuntimeService) replayUndeliveredBrowserToolResults(ctx context.Context) {
	if service.Analyses == nil {
		return
	}
	rows, err := service.Analyses.DB().QueryContext(ctx, `
		SELECT t.id, child.id, t.status, COALESCE(t.result_json,''), COALESCE(t.error_detail,'')
		FROM tool_calls t
		JOIN execution_attempts child ON child.requested_by_tool_call_id=t.id
		WHERE t.execution_mode='quoin_browser'
		  AND t.status IN ('succeeded','failed')
		  AND child.attempt_type='browser_exploration'
		  AND child.state IN ('Succeeded','Failed')
		  AND NOT EXISTS (
			SELECT 1 FROM model_call_input_items item WHERE item.tool_call_id=t.id
		  )
		ORDER BY t.id`)
	if err != nil {
		return
	}
	type pending struct {
		toolCallID, childAttemptID int64
		status, payload, detail    string
	}
	var pendingDeliveries []pending
	for rows.Next() {
		var item pending
		if rows.Scan(&item.toolCallID, &item.childAttemptID, &item.status, &item.payload, &item.detail) == nil {
			pendingDeliveries = append(pendingDeliveries, item)
		}
	}
	// Quoin's production pool is deliberately single-connection. Close the
	// durable scan before Slot.View reads the current Plinth fence.
	rows.Close()
	for _, item := range pendingDeliveries {
		service.deliverBrowserToolResult(item.toolCallID, item.childAttemptID, item.status == "succeeded", []byte(item.payload), item.detail)
	}
}

func (service *RuntimeService) deliverBrowserToolResult(toolCallID, childAttemptID int64, success bool, payload []byte, detail string) {
	if service.Slots == nil {
		return
	}
	view, err := service.Slots.View(context.Background(), qruntime.SlotPlinth)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return
	}
	result := &runtimev1.ToolResultDelivery{ToolCallId: toolCallID, ChildAttemptId: childAttemptID, Success: success, ErrorDetail: detail}
	if len(payload) > 0 {
		digest := sha256.Sum256(payload)
		result.Payload = &runtimev1.ResultPayload{SchemaKind: "browser_tool_result_v1", CanonicalJson: payload, ContentDigest: digest[:]}
	}
	envelope := &runtimev1.ControlEnvelope{BootId: view.BootID, ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(toolCallID), Msg: &runtimev1.ControlEnvelope_ToolResultDelivery{ToolResultDelivery: result}}
	// Delivery is reconstructible from the terminal Tool Call row. Do not retain
	// an unbounded in-memory mirror; replayUndeliveredBrowserToolResults is the
	// sole recovery path across disconnects and restarts.
	_ = service.sendEnvelope(qruntime.SlotPlinth, envelope)
}

// browserObservationAudit derives immutable completion audit facts from the
// canonical Observation projection itself. The Tool-result envelope also holds
// action/error metadata and must never be digested as the Observation.
func browserObservationAudit(payload []byte) (version, digest, size, pageID, origin any, err error) {
	var result struct {
		Observation json.RawMessage `json:"observation"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &result) != nil || len(result.Observation) == 0 {
		return nil, nil, nil, nil, nil, fmt.Errorf("browser result has no observation")
	}
	var observation struct {
		Version int64  `json:"version"`
		Origin  string `json:"origin"`
		Pages   []struct {
			PageID  string `json:"pageId"`
			Current bool   `json:"current"`
		} `json:"pages"`
	}
	if json.Unmarshal(result.Observation, &observation) != nil || observation.Version < 1 {
		return nil, nil, nil, nil, nil, fmt.Errorf("browser result has invalid observation")
	}
	for _, page := range observation.Pages {
		if page.Current {
			if page.PageID == "" {
				return nil, nil, nil, nil, nil, fmt.Errorf("browser result has invalid current page")
			}
			pageID = page.PageID
			break
		}
	}
	// Lintel's live executor always reports a current page. Requiring it here
	// makes it impossible for a synthetic payload to claim an action audit while
	// omitting the actual page that produced the Observation.
	if pageID == nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("browser result has no current page")
	}
	checksum := sha256.Sum256(result.Observation)
	return observation.Version, hex.EncodeToString(checksum[:]), len(result.Observation), pageID, observation.Origin, nil
}

func browserActionFailure(code string, terminal bool) string {
	if !terminal {
		switch code {
		case "ElementNotFound", "ElementNotUnique", "ElementNotInteractable", "ActionTimeout", "NavigationFailed", "DialogBlocked", "DownloadBlocked", "ElementReferenceStale":
			return "recoverable_error"
		}
	}
	return "session_closed"
}

func probeResultForExploration(probe *runtimev1.AuthenticationProbeObservation, phase string) (result, reason string, observed time.Time, ok bool) {
	if probe == nil || probe.GetObservedAt() == nil || !probe.GetObservedAt().IsValid() || probe.GetPhase().String() != "AUTHENTICATION_PROBE_PHASE_"+strings.ToUpper(phase) {
		return "", "", time.Time{}, false
	}
	switch probe.GetResult() {
	case runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED:
		result = "Authenticated"
	case runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_UNAUTHENTICATED:
		result = "Unauthenticated"
	case runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE:
		result, reason = "Indeterminate", probe.GetReasonCode()
		if reason == "" {
			return "", "", time.Time{}, false
		}
	default:
		return "", "", time.Time{}, false
	}
	return result, reason, probe.GetObservedAt().AsTime().UTC(), true
}

func terminalReasonForBrowserAction(result *runtimev1.BrowserExplorationActionResult) (string, bool) {
	switch result.GetTerminalReason() {
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_REQUIRED:
		return "authentication_required", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE:
		return "authentication_probe_unavailable", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED:
		return "artifact_commit_failed", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_CANCELLED:
		return "cancelled", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_RUNTIME_UNAVAILABLE:
		return "runtime_unavailable", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED:
		return "browser_crashed", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_PROTOCOL_ERROR:
		return "protocol_error", true
	}
	switch result.GetErrorCode() {
	case "AuthenticationRequired":
		return "authentication_required", true
	case "AuthenticationProbeUnavailable":
		return "authentication_probe_unavailable", true
	case "ArtifactCommitFailed":
		return "artifact_commit_failed", true
	case "Cancelled":
		return "cancelled", true
	case "LeaseExpired":
		return "lease_expired", true
	case "ParentTerminated":
		return "parent_terminal", true
	case "RuntimeUnavailable":
		return "runtime_unavailable", true
	case "BrowserCrashed":
		return "browser_crashed", true
	default:
		return "protocol_error", true
	}
}

// persistTerminalExplorationResult records terminal Operation facts before the
// action and parent Tool Call close the child Attempt through frozen SQL.
func (service *RuntimeService) persistTerminalExplorationResult(ctx context.Context, conn *sql.Conn, result *runtimev1.BrowserExplorationActionResult, actionKind string, revisionID int64, bootID string, epoch uint64, now string) error {
	if result.GetTerminalOutcome() == runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_SUCCEEDED {
		if actionKind != "close_session" || result.GetTraceArtifactId() < 1 || result.GetTraceIntegrity() != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE || len(result.GetTraceDigest()) != sha256.Size {
			return fmt.Errorf("successful exploration close lacks complete terminal facts")
		}
		if err := validateExplorationTrace(ctx, conn, result.GetTraceArtifactId(), result.GetOperationId(), result.GetChildAttemptId(), bootID, epoch, result.GetTraceDigest(), traceIntegrityText(result.GetTraceIntegrity()), false); err != nil {
			return err
		}
		probeResult, probeReason, observed, ok := probeResultForExploration(result.GetCompletionProbe(), "completion")
		if !ok || probeResult != "Authenticated" {
			return fmt.Errorf("successful exploration close lacks authenticated completion probe")
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,reason_code,observed_at)
			SELECT ?,COALESCE(MAX(probe_seq),0)+1,'completion',?,?,?,?,?,?,?,? FROM browser_probe_results WHERE operation_id=?`, result.GetOperationId(), revisionID, result.GetCompletionProbe().GetJourneyId(), result.GetCompletionProbe().GetJourneyVersion(), result.GetCompletionProbe().GetJourneyCatalogDigest(), result.GetCompletionProbe().GetJourneyCatalogVersion(), probeResult, nullableText(probeReason), observed.Format(time.RFC3339Nano), result.GetOperationId()); err != nil {
			return err
		}
		updated, err := conn.ExecContext(ctx, `UPDATE browser_operations SET state='Succeeded',ended_at=?,trace_artifact_id=?,trace_integrity='complete',completion_digest=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, result.GetTraceArtifactId(), result.GetTraceDigest(), result.GetOperationId())
		return requireBrowserOperationTerminalCAS(updated, err)
	}
	if !result.GetSessionTerminal() {
		return nil
	}
	reason, ok := terminalReasonForBrowserAction(result)
	if !ok {
		return fmt.Errorf("terminal browser result has no reason")
	}
	traceID, integrity := any(nil), any(nil)
	if result.GetTraceArtifactId() != 0 {
		if len(result.GetTraceDigest()) != sha256.Size || result.GetTraceIntegrity() == runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_UNSPECIFIED {
			return fmt.Errorf("terminal trace facts malformed")
		}
		traceID, integrity = result.GetTraceArtifactId(), strings.ToLower(strings.TrimPrefix(result.GetTraceIntegrity().String(), "BROWSER_TRACE_INTEGRITY_"))
	}
	// Only a technical trace commit failure is permitted to close without a
	// trace. Other terminal outcomes must carry the diagnostic trace mandated by
	// the frozen operation trigger.
	if traceID == nil && reason != "artifact_commit_failed" && reason != "runtime_unavailable" {
		return fmt.Errorf("terminal exploration result lacks required trace")
	}
	if traceID != nil {
		if err := validateExplorationTrace(ctx, conn, result.GetTraceArtifactId(), result.GetOperationId(), result.GetChildAttemptId(), bootID, epoch, result.GetTraceDigest(), traceIntegrityText(result.GetTraceIntegrity()), false); err != nil {
			return err
		}
	}
	// A failed close can still have a valid completion probe: explicit
	// unauthenticated and indeterminate observations are durable diagnostic facts,
	// not disposable fields just because the operation is not Succeeded.
	if probe := result.GetCompletionProbe(); probe != nil {
		probeResult, probeReason, observed, valid := probeResultForExploration(probe, "completion")
		if !valid {
			return fmt.Errorf("terminal exploration completion probe is malformed")
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,reason_code,observed_at)
			SELECT ?,COALESCE(MAX(probe_seq),0)+1,'completion',?,?,?,?,?,?,?,? FROM browser_probe_results WHERE operation_id=?`, result.GetOperationId(), revisionID, probe.GetJourneyId(), probe.GetJourneyVersion(), probe.GetJourneyCatalogDigest(), probe.GetJourneyCatalogVersion(), probeResult, nullableText(probeReason), observed.Format(time.RFC3339Nano), result.GetOperationId()); err != nil {
			return err
		}
	}
	operationState := "Failed"
	if result.GetTerminalOutcome() == runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_CANCELLED && reason == "cancelled" {
		operationState = "Cancelled"
	}
	updated, err := conn.ExecContext(ctx, `UPDATE browser_operations SET state=?,ended_at=?,terminal_reason=?,trace_artifact_id=?,trace_integrity=?,completion_digest=?,row_version=row_version+1 WHERE id=? AND state IN ('Starting','Running')`, operationState, now, reason, traceID, integrity, nullableBytes(result.GetTraceDigest()), result.GetOperationId())
	return requireBrowserOperationTerminalCAS(updated, err)
}

// requireBrowserOperationTerminalCAS makes terminal proposals compare-and-swap
// operations. A stale second terminal writer must be rejected before it can
// close a different child or overwrite trace facts.
var errBrowserOperationTerminalCASLost = errors.New("browser operation terminal CAS lost")

func requireBrowserOperationTerminalCAS(updated sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errBrowserOperationTerminalCASLost
	}
	return nil
}

// validateExplorationTrace accepts only the exact committed trace upload that
// Lintel made for this terminal result. It binds the logical Artifact, its
// content-addressed blob, upload ledger, operation owner, and current Runtime
// fence in the same SQLite transaction. An operation-level trace is permitted
// only for a completion observed with no active action child.
func traceIntegrityText(integrity runtimev1.BrowserTraceIntegrity) string {
	switch integrity {
	case runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE:
		return "complete"
	case runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE:
		return "incomplete"
	default:
		return ""
	}
}

func validateExplorationTrace(ctx context.Context, conn *sql.Conn, artifactID, operationID, childID int64, bootID string, epoch uint64, digest []byte, expectedIntegrity string, allowOperationTrace bool) error {
	if artifactID < 1 || operationID < 1 || len(digest) != 0 && len(digest) != sha256.Size || bootID == "" || epoch == 0 || (expectedIntegrity != "complete" && expectedIntegrity != "incomplete") {
		return fmt.Errorf("terminal trace facts malformed")
	}
	var valid int
	query := `SELECT EXISTS(
		SELECT 1 FROM artifacts artifact
		JOIN artifact_blobs blob ON blob.id=artifact.blob_id
		JOIN runtime_artifact_uploads upload ON upload.artifact_id=artifact.id
		WHERE artifact.id=? AND artifact.kind='trace' AND artifact.owner_type='browser_operation'
		  AND artifact.owner_id=? AND artifact.sensitive=1 AND artifact.body_expired=0
		  AND upload.state='committed' AND upload.kind='trace' AND upload.trace_integrity=?
		  AND upload.owner_type='browser_operation' AND upload.owner_id=?
		  -- The browser-start epoch is immutable audit history. A trace may have
		  -- been committed on an earlier authenticated same-boot stream and its
		  -- terminal result replayed on this successor epoch, never vice versa.
		  AND upload.boot_id=? AND upload.connection_epoch<=?`
	args := []any{artifactID, operationID, expectedIntegrity, operationID, bootID, epoch}
	if len(digest) == sha256.Size {
		query += ` AND blob.sha256=?`
		args = append(args, hex.EncodeToString(digest))
	}
	if allowOperationTrace {
		query += ` AND upload.attempt_id IS NULL)`
	} else {
		if childID < 1 {
			return fmt.Errorf("terminal action trace lacks child attempt")
		}
		query += ` AND upload.attempt_id=?)`
		args = append(args, childID)
	}
	if err := conn.QueryRowContext(ctx, query, args...).Scan(&valid); err != nil || valid != 1 {
		return fmt.Errorf("terminal trace is not the exact committed Lintel upload")
	}
	return nil
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

// dispatchBrowserExplorationAction is the second durable phase. It promotes the
// queued child under the live Lintel fence and creates the action only after
// the frozen prerequisites are committed.
// isExplorationOperation routes every Exploration completion, including an
// at-least-once replay after Quoin already persisted terminal state, through the
// Exploration ledger. Generic Browser.Service cannot validate its trace fence.
func (service *RuntimeService) isExplorationOperation(ctx context.Context, operationID int64) bool {
	if service.Analyses == nil || operationID < 1 {
		return false
	}
	var count int
	return service.Analyses.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations WHERE id=? AND kind='exploration'`, operationID).Scan(&count) == nil && count == 1
}

func (service *RuntimeService) explorationCompletionAlreadyCommitted(ctx context.Context, operationID int64, digest []byte) bool {
	if service.Analyses == nil || operationID < 1 || len(digest) != sha256.Size {
		return false
	}
	var count int
	return service.Analyses.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations
		WHERE id=? AND kind='exploration' AND state IN ('Succeeded','Failed','Cancelled','Interrupted')
		  AND completion_digest=?`, operationID, digest).Scan(&count) == nil && count == 1
}

// handleExplorationCompletion closes a Runtime-crashed (or otherwise terminal)
// exploration action. The parent Tool Call is the only application-level
// terminal update: trg_tool_calls_close_browser_child then closes the child
// Attempt in the same SQLite statement.
func (service *RuntimeService) handleExplorationCompletion(ctx context.Context, envelope *runtimev1.ControlEnvelope, result *runtimev1.CompleteBrowserOperation) {
	accepted := false
	replayed := false
	defer func() {
		if envelope == nil || result == nil {
			return
		}
		// A replay after the first transaction committed must get accepted again;
		// otherwise Lintel keeps the same completion cache indefinitely.
		if !accepted {
			accepted = service.explorationCompletionAlreadyCommitted(context.Background(), result.GetOperationId(), result.GetResultDigest())
			replayed = accepted
		}
		ack := &runtimev1.CompleteBrowserOperationAck{OperationId: result.GetOperationId(), ResultDigest: result.GetResultDigest(), Accepted: accepted}
		if !accepted {
			ack.Detail = "exploration completion was not accepted"
		}
		_ = service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: envelope.GetBootId(), ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetMessageId(), Msg: &runtimev1.ControlEnvelope_CompleteBrowserOperationAck{CompleteBrowserOperationAck: ack}})
		if accepted && !replayed {
			go func() { _ = service.dispatchBrowserStop(context.Background(), result.GetOperationId()) }()
		}
	}()
	if service.Analyses == nil || service.Slots == nil || envelope == nil || result == nil {
		return
	}
	if service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error { return nil }) != nil {
		return
	}
	reason, ok := completionTerminalReason(result.GetTerminalReason())
	if !ok || (result.GetOutcome() != runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED && result.GetOutcome() != runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_CANCELLED) || len(result.GetResultDigest()) != sha256.Size {
		return
	}
	if result.GetOutcome() == runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_CANCELLED && reason != "cancelled" {
		return
	}
	traceID, integrity := any(nil), any(nil)
	if result.GetTraceArtifactId() > 0 {
		if len(result.GetTraceDigest()) != sha256.Size || (result.GetTraceIntegrity() != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE && result.GetTraceIntegrity() != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE) {
			return
		}
		traceID, integrity = result.GetTraceArtifactId(), strings.ToLower(strings.TrimPrefix(result.GetTraceIntegrity().String(), "BROWSER_TRACE_INTEGRITY_"))
	} else if reason != "artifact_commit_failed" && reason != "runtime_unavailable" {
		return
	}
	conn, err := service.Analyses.DB().Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var actionID, childID, toolCallID int64
	var actionKind, toolStatus, actionOutcome, state string
	err = conn.QueryRowContext(ctx, `SELECT a.id,a.child_attempt_id,a.tool_call_id,a.action_kind,t.status,COALESCE(a.outcome,''),o.state
		FROM browser_operations o
		JOIN browser_exploration_actions a ON a.operation_id=o.id
		JOIN tool_calls t ON t.id=a.tool_call_id
		WHERE o.id=? AND o.kind='exploration' AND a.outcome IS NULL
		ORDER BY a.action_seq DESC LIMIT 1`, result.GetOperationId()).Scan(&actionID, &childID, &toolCallID, &actionKind, &toolStatus, &actionOutcome, &state)
	if err == sql.ErrNoRows {
		// A parent may cancel after StartAck or between actions. There is then no
		// active child or running Tool Call to close; the operation-level completion
		// is the sole terminal fact and unblocks the parent's CancelAck.
		if result.GetOutcome() == runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_CANCELLED && reason == "cancelled" {
			var parentID int64
			if traceID == nil || validateExplorationTrace(ctx, conn, result.GetTraceArtifactId(), result.GetOperationId(), 0, envelope.GetBootId(), envelope.GetConnectionEpoch(), result.GetTraceDigest(), traceIntegrityText(result.GetTraceIntegrity()), true) != nil {
				return
			}
			now := result.GetEndedAt().AsTime().UTC().Format(time.RFC3339Nano)
			if err = conn.QueryRowContext(ctx, `SELECT owner_attempt_id FROM browser_operations WHERE id=? AND kind='exploration' AND state IN ('Starting','Running')`, result.GetOperationId()).Scan(&parentID); err != nil {
				return
			}
			updated, updateErr := conn.ExecContext(ctx, `UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='cancelled',trace_artifact_id=?,trace_integrity=?,completion_digest=?,row_version=row_version+1 WHERE id=? AND state IN ('Starting','Running')`, now, traceID, integrity, result.GetResultDigest(), result.GetOperationId())
			if requireBrowserOperationTerminalCAS(updated, updateErr) != nil {
				return
			}
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return
			}
			committed, accepted = true, true
			conn.Close()
			go service.finalizeCancellation(context.Background(), parentID, "investigation")
			return
		}
		// After a terminal action completes, an idle Exploration can crash before
		// a next Tool Call supplies a child fence. If its trace upload fails, the
		// failure itself is still a terminal fact; do not reject it as stale and
		// leak the running operation/identity slot.
		if result.GetOutcome() == runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED && reason == "artifact_commit_failed" && traceID == nil {
			var parentID int64
			now := result.GetEndedAt().AsTime().UTC().Format(time.RFC3339Nano)
			if err = conn.QueryRowContext(ctx, `SELECT owner_attempt_id FROM browser_operations WHERE id=? AND kind='exploration' AND state IN ('Starting','Running')`, result.GetOperationId()).Scan(&parentID); err != nil {
				return
			}
			updated, updateErr := conn.ExecContext(ctx, `UPDATE browser_operations
				SET state='Failed',ended_at=?,terminal_reason='artifact_commit_failed',completion_digest=?,row_version=row_version+1
				WHERE id=? AND kind='exploration' AND state='Running'`, now, result.GetResultDigest(), result.GetOperationId())
			if requireBrowserOperationTerminalCAS(updated, updateErr) != nil {
				return
			}
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return
			}
			committed, accepted = true, true
			conn.Close()
			go service.finalizeCancellation(context.Background(), parentID, "investigation")
			return
		}
		// Crash can win after StartAck but before the first action audit row, or
		// between two child actions. Locate the outstanding Tool Call directly from
		// the operation owner; its frozen AFTER trigger remains the sole child
		// Attempt terminal writer.
		err = conn.QueryRowContext(ctx, `SELECT b.child_attempt_id,b.tool_call_id,COALESCE(json_extract(t.arguments_json,'$.action'),'open'),t.status,o.state
			FROM browser_exploration_child_bindings b
			JOIN tool_calls t ON t.id=b.tool_call_id AND t.execution_mode='quoin_browser'
			JOIN execution_attempts child ON child.id=b.child_attempt_id
			JOIN browser_operations o ON o.id=b.operation_id
			WHERE b.operation_id=? AND o.kind='exploration' AND child.state IN ('Queued','Assigned','Running','Cancelling')
			  AND t.status='running' ORDER BY child.id DESC LIMIT 1`, result.GetOperationId()).Scan(&childID, &toolCallID, &actionKind, &toolStatus, &state)
		if err == sql.ErrNoRows {
			// Chromium can crash after a previously committed action result and before
			// Quoin has dispatched another Tool Call. There is no child Attempt left
			// to close in that interval, but the Running operation still owns the
			// session and must durably record its incomplete operation-level trace.
			// Only a trace fenced to this operation and stream can establish that fact.
			if result.GetOutcome() == runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED && traceID != nil &&
				validateExplorationTrace(ctx, conn, result.GetTraceArtifactId(), result.GetOperationId(), 0, envelope.GetBootId(), envelope.GetConnectionEpoch(), result.GetTraceDigest(), traceIntegrityText(result.GetTraceIntegrity()), true) == nil {
				now := result.GetEndedAt().AsTime().UTC().Format(time.RFC3339Nano)
				if updated, updateErr := conn.ExecContext(ctx, `UPDATE browser_operations
					SET state='Failed',ended_at=?,terminal_reason=?,trace_artifact_id=?,trace_integrity=?,completion_digest=?,row_version=row_version+1
					WHERE id=? AND kind='exploration' AND state='Running'`, now, reason, traceID, integrity, result.GetResultDigest(), result.GetOperationId()); requireBrowserOperationTerminalCAS(updated, updateErr) == nil {
					if _, commitErr := conn.ExecContext(ctx, "COMMIT"); commitErr == nil {
						committed, accepted = true, true
						return
					}
				}
				return
			}
			// A committed terminal operation completion has precedence. This is a
			// duplicate/stale replay and must not alter durable history.
			_, _ = conn.ExecContext(ctx, "COMMIT")
			committed = true
			return
		}
		if err != nil || (state != "Starting" && state != "Running") || toolStatus != "running" {
			return
		}
		actionID = 0
	}
	if err != nil || (state != "Starting" && state != "Running") || actionOutcome != "" || toolStatus != "running" {
		return
	}
	now := result.GetEndedAt().AsTime().UTC().Format(time.RFC3339Nano)
	// The trace must be the exact committed Lintel upload for this operation
	// and current stream fence. A crash with no active child uses the explicitly
	// bounded operation-level trace exception; it still cannot attach an arbitrary
	// browser-owned Artifact or a blob whose digest differs from this result.
	if traceID != nil {
		if err = validateExplorationTrace(ctx, conn, result.GetTraceArtifactId(), result.GetOperationId(), childID, envelope.GetBootId(), envelope.GetConnectionEpoch(), result.GetTraceDigest(), traceIntegrityText(result.GetTraceIntegrity()), actionID == 0); err != nil {
			return
		}
	}
	updated, updateErr := conn.ExecContext(ctx, `UPDATE browser_operations SET state='Failed',ended_at=?,terminal_reason=?,trace_artifact_id=?,trace_integrity=?,completion_digest=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, reason, traceID, integrity, result.GetResultDigest(), result.GetOperationId())
	if requireBrowserOperationTerminalCAS(updated, updateErr) != nil {
		return
	}
	failure := map[string]any{"outcome": "session_closed", "action": actionKind, "error": map[string]any{"code": terminalErrorCode(reason), "message": "browser exploration session ended", "retryableInSession": false}}
	payload, _ := json.Marshal(failure)
	if actionID != 0 {
		if _, err = conn.ExecContext(ctx, `UPDATE browser_exploration_actions SET ended_at=?,outcome='session_closed',error_code=?,result_digest=?,observation_version=NULL,observation_digest=NULL,observation_size_bytes=NULL WHERE id=? AND outcome IS NULL`, now, terminalErrorCode(reason), result.GetResultDigest(), actionID); err != nil {
			return
		}
	}
	if _, err = conn.ExecContext(ctx, `UPDATE tool_calls SET status='failed',ended_at=?,result_json=?,error_detail=?,row_version=row_version+1 WHERE id=? AND status='running'`, now, string(payload), "browser exploration session ended", toolCallID); err != nil {
		return
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return
	}
	committed = true
	accepted = true
	// See handleBrowserExplorationActionResult: delivery re-reads slot state
	// and must not wait on this completed transaction's pool connection.
	conn.Close()
	// The immediate delivery must be byte-identical to the durable replay.
	service.deliverBrowserToolResult(toolCallID, childID, false, payload, "browser exploration session ended")
}

func completionTerminalReason(reason runtimev1.BrowserOperationTerminalReason) (string, bool) {
	switch reason {
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED:
		return "browser_crashed", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED:
		return "artifact_commit_failed", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_RUNTIME_UNAVAILABLE:
		return "runtime_unavailable", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_CANCELLED:
		return "cancelled", true
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_PROTOCOL_ERROR:
		return "protocol_error", true
	default:
		return "", false
	}
}

func terminalErrorCode(reason string) string {
	if reason == "browser_crashed" {
		return "BrowserCrashed"
	}
	if reason == "artifact_commit_failed" {
		return "ArtifactCommitFailed"
	}
	return "RuntimeUnavailable"
}

func (service *RuntimeService) dispatchBrowserExplorationAction(ctx context.Context, childID int64) error {
	if service.Analyses == nil || (service.Slots == nil && service.browserExplorationSlotView == nil) {
		return fmt.Errorf("browser action authority unavailable")
	}
	var view qruntime.SlotView
	var err error
	if service.browserExplorationSlotView != nil {
		view, err = service.browserExplorationSlotView(ctx)
	} else {
		view, err = service.Slots.View(ctx, qruntime.SlotLintel)
	}
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return qruntime.ErrNotConnected
	}
	conn, err := service.Analyses.DB().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var operationID, parentID, toolCallID int64
	var childState, toolState, actionJSON, operationState, parentState string
	err = conn.QueryRowContext(ctx, `SELECT b.operation_id,b.parent_attempt_id,b.tool_call_id,c.state,t.status,t.arguments_json,o.state,p.state
		FROM browser_exploration_child_bindings b
		JOIN execution_attempts c ON c.id=b.child_attempt_id
		JOIN tool_calls t ON t.id=b.tool_call_id
		JOIN browser_operations o ON o.id=b.operation_id
		JOIN execution_attempts p ON p.id=b.parent_attempt_id WHERE b.child_attempt_id=?`, childID).Scan(&operationID, &parentID, &toolCallID, &childState, &toolState, &actionJSON, &operationState, &parentState)
	if err != nil {
		return err
	}
	if childState != "Queued" || toolState != "running" || operationState != "Running" || parentState != "Running" {
		return nil
	}
	var action struct {
		Action string `json:"action"`
		PageID string `json:"pageId"`
	}
	if err = json.Unmarshal([]byte(actionJSON), &action); err != nil {
		return err
	}
	now := time.Now().UTC()
	lease := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	// The frozen attempt state machine requires Queued → Assigned → Running.
	// Assign and accept within this one durable preparation transaction; the
	// action is still not sent until its audit row has committed.
	result, err := conn.ExecContext(ctx, `UPDATE execution_attempts
		SET state='Assigned',runtime_slot='lintel',boot_id=?,connection_epoch=?,lease_until=?,runtime_release_version=?,row_version=row_version+1
		WHERE id=? AND state='Queued'`, view.BootID, *view.ConnectionEpoch, lease, service.ReleaseVersion, childID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil
	}
	result, err = conn.ExecContext(ctx, `UPDATE execution_attempts
		SET state='Running',accepted_at=?,row_version=row_version+1
		WHERE id=? AND state='Assigned'`, now.Format(time.RFC3339Nano), childID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("browser action child %d was not promoted to Running", childID)
	}
	var actionSeq int64
	if err = conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(action_seq),0)+1 FROM browser_exploration_actions WHERE operation_id=?`, operationID).Scan(&actionSeq); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO browser_exploration_actions(operation_id,action_seq,child_attempt_id,tool_call_id,action_kind,page_id,target_description,started_at)
		VALUES(?,?,?,?,?,?,?,?)`, operationID, actionSeq, childID, toolCallID, action.Action, nullableText(action.PageID), action.Action, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	digest := sha256.Sum256([]byte(actionJSON))
	return service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{
		BootId: view.BootID, ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(childID),
		Msg: &runtimev1.ControlEnvelope_ExecuteBrowserExplorationAction{ExecuteBrowserExplorationAction: &runtimev1.ExecuteBrowserExplorationAction{
			OperationId: operationID, ChildAttemptId: childID, ParentAttemptId: parentID, ToolCallId: toolCallID,
			Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: []byte(actionJSON), ContentDigest: digest[:]}}},
	})
}

// handleBrowserToolResultDeliveryAck deliberately has no process-local cleanup.
// Acknowledgements are transport facts only: model_call_input_items is the durable
// consumption authority used by replayUndeliveredBrowserToolResults.
func (service *RuntimeService) handleBrowserToolResultDeliveryAck(_ *runtimev1.ControlEnvelope, _ *runtimev1.ToolResultDeliveryAck) {
}

// replayBrowserExplorationChild re-emits a durably prepared action after an
// unknown-outcome send. Existing child/action IDs are the idempotency key.
func (service *RuntimeService) replayBrowserExplorationChild(ctx context.Context, childID int64) error {
	if service.Analyses == nil || service.Slots == nil {
		return nil
	}
	view, err := service.Slots.View(ctx, qruntime.SlotLintel)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return qruntime.ErrNotConnected
	}
	var operationID, parentID, toolID int64
	var actionJSON, state, operationState string
	err = service.Analyses.DB().QueryRowContext(ctx, `SELECT b.operation_id,b.parent_attempt_id,b.tool_call_id,t.arguments_json,c.state,o.state
		FROM browser_exploration_child_bindings b
		JOIN execution_attempts c ON c.id=b.child_attempt_id
		JOIN tool_calls t ON t.id=b.tool_call_id
		JOIN browser_operations o ON o.id=b.operation_id
		WHERE b.child_attempt_id=?`, childID).Scan(&operationID, &parentID, &toolID, &actionJSON, &state, &operationState)
	if err != nil {
		return err
	}
	if state == "Queued" && operationState == "Running" {
		return service.dispatchBrowserExplorationAction(ctx, childID)
	}
	if state != "Running" && state != "Cancelling" {
		return nil
	}
	var exists int
	if err := service.Analyses.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM browser_exploration_actions WHERE child_attempt_id=? AND outcome IS NULL)`, childID).Scan(&exists); err != nil || exists != 1 {
		return err
	}
	digest := sha256.Sum256([]byte(actionJSON))
	return service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: view.BootID, ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(childID), Msg: &runtimev1.ControlEnvelope_ExecuteBrowserExplorationAction{ExecuteBrowserExplorationAction: &runtimev1.ExecuteBrowserExplorationAction{OperationId: operationID, ChildAttemptId: childID, ParentAttemptId: parentID, ToolCallId: toolID, Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: []byte(actionJSON), ContentDigest: digest[:]}}}})
}
