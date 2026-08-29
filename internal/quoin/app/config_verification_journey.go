package app

// Config Verification Journey dispatch and convergence (T23): browser-check
// children dispatch to Lintel only after their journey Browser Operation owns
// a physical slot (RUNTIME-BROWSER-005); technical terminations converge the
// child Attempt, the operation and the parent Run without inventing Evidence.

import (
	"context"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// journeyConvergenceBatchSize bounds one reconciliation turn. Further work is
// scheduled as a later turn so a large durable backlog cannot monopolize the
// runtime loop or control stream.
const journeyConvergenceBatchSize = 32

// dispatchJourneyAttempt sends one Lintel-bound browser child. Browser Start
// binds the child before Chromium can run; this function accepts that already
// Assigned state and only binds bare Queued children for recovery compatibility.
func (service *RuntimeService) dispatchJourneyAttempt(ctx context.Context, attemptID int64) error {
	if service.BusinessSystems == nil {
		return fmt.Errorf("business systems are not wired")
	}
	view, err := service.Slots.View(ctx, qruntime.SlotLintel)
	if err != nil {
		return err
	}
	if !view.Connected || view.ConnectionEpoch == nil {
		return qruntime.ErrNotConnected
	}
	var scopeType string
	if err := service.BusinessSystems.DB().QueryRowContext(ctx, `SELECT scope_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeType); err != nil {
		return err
	}
	attempts := service.BusinessSystems.VerificationAttempts()
	if scopeType == "run_check" {
		if service.Inspections == nil {
			return fmt.Errorf("inspections are not wired")
		}
		attempts = service.Inspections.Attempts()
	}
	attemptView, err := attempts.Get(ctx, attemptID)
	if err != nil {
		return err
	}
	if attemptView.State == "Queued" {
		if err := attempts.BindToSlot(ctx, attemptID, "lintel", view.BootID, *view.ConnectionEpoch, attempt.DispatchLease); err != nil {
			return err
		}
	} else if attemptView.State != "Assigned" || attemptView.BootID == nil || *attemptView.BootID != view.BootID {
		return fmt.Errorf("journey attempt %d is not dispatchable", attemptID)
	}
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		return err
	}
	var scopeID int64
	var planKey, checkKey string
	if err := service.BusinessSystems.DB().QueryRowContext(ctx, `SELECT scope_id,COALESCE(plan_key,''),check_key FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID, &planKey, &checkKey); err != nil {
		return err
	}
	return service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{
		ConnectionEpoch: *view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{
			AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_COLLECTION,
			ScopeType: map[bool]runtimev1.ScopeType{true: runtimev1.ScopeType_SCOPE_TYPE_RUN_CHECK, false: runtimev1.ScopeType_SCOPE_TYPE_CONFIG_VERIFICATION_RUN}[scopeType == "run_check"], ScopeId: scopeID,
			PlanKey: planKey, CheckKey: checkKey,
			LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
			Input:         &runtimev1.AttemptInputSnapshot{SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON, ContentDigest: input.ContentDigest},
		}},
	})
}

// dispatchReadyJourneyAttempts dispatches every browser child whose journey
// operation is already Running (Start acknowledged) and whose Attempt was
// bound before its physical Browser start.
func (service *RuntimeService) dispatchReadyJourneyAttempts(ctx context.Context) bool {
	if service.BusinessSystems == nil {
		return false
	}
	rows, err := service.BusinessSystems.DB().QueryContext(ctx, `
		SELECT a.id FROM execution_attempts a
		JOIN browser_operations o ON o.owner_attempt_id=a.id AND o.kind='journey'
		WHERE a.attempt_type='inspection_collection' AND a.scope_type IN ('config_verification_run','run_check')
		  AND a.state='Assigned' AND o.state='Running' AND o.stop_confirmed_at IS NULL
		ORDER BY a.id LIMIT ?`, journeyConvergenceBatchSize)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "config_verification.journey_scan", err.Error())
		return false
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return false
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		sharedops.LogEvent("quoin", "error", "config_verification.journey_scan", err.Error())
		return false
	}
	for _, id := range ids {
		if err := service.dispatchJourneyAttempt(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "config_verification.journey_dispatch", fmt.Sprintf("attempt=%d error=%s", id, err.Error()))
		}
	}
	return len(ids) == journeyConvergenceBatchSize
}

// dispatchCancellingJourneyChecks forwards the cancellation fence of fenced
// browser children to Lintel so the physical Journey stops.
func (service *RuntimeService) dispatchCancellingJourneyChecks(ctx context.Context) bool {
	if service.BusinessSystems == nil {
		return false
	}
	rows, err := service.BusinessSystems.DB().QueryContext(ctx, `
		SELECT a.id, COALESCE(o.lintel_boot_id,''), COALESCE(o.lintel_connection_epoch,0), o.state, a.scope_type
		FROM execution_attempts a
		JOIN browser_operations o ON o.owner_attempt_id=a.id AND o.kind='journey'
		WHERE a.attempt_type='inspection_collection' AND a.scope_type IN ('config_verification_run','run_check')
		  AND a.state='Cancelling'
		ORDER BY a.id LIMIT ?`, journeyConvergenceBatchSize)
	if err != nil {
		return false
	}
	defer rows.Close()
	type cancelling struct {
		attempt  int64
		boot     string
		epoch    uint64
		scope    string
		terminal bool
	}
	var pending []cancelling
	scanned := 0
	for rows.Next() {
		var item cancelling
		scanned++
		var operationState string
		if err := rows.Scan(&item.attempt, &item.boot, &item.epoch, &operationState, &item.scope); err != nil {
			return false
		}
		// A terminal operation has already released the physical Browser work.
		// Its Cancelling child still requires a durable CancelAck-equivalent
		// convergence if the original Ack was lost with the control stream.
		item.terminal = operationState == "Succeeded" || operationState == "Failed" || operationState == "Cancelled" || operationState == "Interrupted"
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		return false
	}
	view, err := service.Slots.View(ctx, qruntime.SlotLintel)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return false
	}
	for _, item := range pending {
		childAttempts := service.BusinessSystems.VerificationAttempts()
		if item.scope == "run_check" {
			if service.Inspections == nil {
				continue
			}
			childAttempts = service.Inspections.Attempts()
		}
		if item.terminal {
			if err := childAttempts.CancelAck(ctx, item.attempt); err == nil {
				service.recordJourneyTechnicalGap(ctx, item.attempt, "cancelled")
			}
			continue
		}
		if item.boot == "" || item.epoch == 0 {
			// Never dispatched: close the child directly through the fence.
			if err := childAttempts.CancelAck(ctx, item.attempt); err == nil {
				service.recordJourneyTechnicalGap(ctx, item.attempt, "cancelled")
			}
			continue
		}
		_ = service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{
			ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(item.attempt), BootId: view.BootID,
			Msg: &runtimev1.ControlEnvelope_CancelAttempt{CancelAttempt: &runtimev1.CancelAttempt{AttemptId: item.attempt}},
		})
	}
	return scanned == journeyConvergenceBatchSize
}

// reconcileJourneyVerificationChildren converges technical terminations:
// a terminal operation interrupts its child and records the technical gap; a
// terminal child without a settled check result closes its operation and
// records the gap. No whole-run or mid-step retry exists (CFG-JOURNEY-006).
func (service *RuntimeService) reconcileJourneyVerificationChildren(ctx context.Context) {
	if service.BusinessSystems == nil {
		return
	}
	// Identity-serial admission: every released identity admits the next
	// waiting browser child of a running Config Verification Run. One turn is
	// bounded; further durable work is handled by the next reconciliation.
	more := false
	for admittedCount := 0; admittedCount < journeyConvergenceBatchSize; admittedCount++ {
		admitted, err := service.BusinessSystems.AdmitNextJourneyChild(ctx)
		if err != nil {
			sharedops.LogEvent("quoin", "error", "config_verification.journey_admit", err.Error())
			break
		}
		if !admitted {
			break
		}
		if admittedCount == journeyConvergenceBatchSize-1 {
			more = true
		}
	}
	if service.Inspections != nil {
		for admittedCount := 0; admittedCount < journeyConvergenceBatchSize; admittedCount++ {
			admitted, err := service.Inspections.AdmitNextJourneyChild(ctx)
			if err != nil {
				sharedops.LogEvent("quoin", "error", "inspection.journey_admit", err.Error())
				break
			}
			if !admitted {
				break
			}
			if admittedCount == journeyConvergenceBatchSize-1 {
				more = true
			}
		}
	}
	db := service.BusinessSystems.DB()
	// 1) Terminal operation with an active child: interrupt the child first.
	rows, err := db.QueryContext(ctx, `
		SELECT a.id, o.id, o.state, COALESCE(o.terminal_reason,''), a.scope_type
		FROM execution_attempts a
		JOIN browser_operations o ON o.owner_attempt_id=a.id AND o.kind='journey'
		WHERE a.attempt_type='inspection_collection' AND a.scope_type IN ('config_verification_run','run_check')
		  AND a.state IN ('Queued','Assigned','Running')
		  AND o.state IN ('Succeeded','Failed','Cancelled','Interrupted')
		ORDER BY a.id LIMIT ?`, journeyConvergenceBatchSize)
	if err == nil {
		type orphan struct {
			attempt, operation     int64
			operationState, reason string
			scope                  string
		}
		var orphans []orphan
		for rows.Next() {
			var item orphan
			if scanErr := rows.Scan(&item.attempt, &item.operation, &item.operationState, &item.reason, &item.scope); scanErr != nil {
				break
			}
			orphans = append(orphans, item)
		}
		queryErr := rows.Err()
		rows.Close()
		if queryErr != nil {
			sharedops.LogEvent("quoin", "error", "config_verification.journey_orphan_scan", queryErr.Error())
			return
		}
		attempts := service.BusinessSystems.VerificationAttempts()
		if len(orphans) == journeyConvergenceBatchSize {
			more = true
		}
		for _, item := range orphans {
			// A Succeeded operation closed through the journey ledger must have
			// closed its child in the same statement; anything else is technical.
			childAttempts := attempts
			if item.scope == "run_check" {
				if service.Inspections == nil {
					continue
				}
				childAttempts = service.Inspections.Attempts()
			}
			if _, err := childAttempts.Interrupt(ctx, item.attempt, technicalReason(item.operationState, item.reason)); err == nil {
				service.recordJourneyTechnicalGap(ctx, item.attempt, gapReasonForOperation(item.operationState, item.reason))
			}
		}
	}
	// 2) Terminal child whose operation is still active: close the operation
	// through its terminal state and let the Stop fence release the identity.
	rows, err = db.QueryContext(ctx, `
		SELECT a.id, a.state, o.id, o.start_dispatched_at IS NOT NULL, a.scope_type FROM execution_attempts a
		JOIN browser_operations o ON o.owner_attempt_id=a.id AND o.kind='journey'
		WHERE a.attempt_type='inspection_collection' AND a.scope_type IN ('config_verification_run','run_check')
		  AND a.state IN ('Succeeded','Failed','Cancelled','Interrupted')
		  AND o.state IN ('Queued','WaitingForCapacity','Starting','Running')
		ORDER BY a.id LIMIT ?`, journeyConvergenceBatchSize)
	if err != nil {
		return
	}
	type straggler struct {
		attempt, operation int64
		attemptState       string
		dispatched         bool
		scope              string
	}
	var stragglers []straggler
	for rows.Next() {
		var item straggler
		if scanErr := rows.Scan(&item.attempt, &item.attemptState, &item.operation, &item.dispatched, &item.scope); scanErr != nil {
			break
		}
		stragglers = append(stragglers, item)
	}
	queryErr := rows.Err()
	rows.Close()
	if queryErr != nil {
		sharedops.LogEvent("quoin", "error", "config_verification.journey_straggler_scan", queryErr.Error())
		return
	}
	if len(stragglers) == journeyConvergenceBatchSize {
		more = true
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, item := range stragglers {
		operationState, operationReason := "Interrupted", "parent_terminal"
		switch item.attemptState {
		case "Cancelled":
			operationState, operationReason = "Cancelled", "cancelled"
		case "Failed":
			operationState, operationReason = "Failed", "parent_terminal"
		case "Succeeded":
			// The local identity_busy path never creates an operation; a
			// Succeeded child with a live operation can only be a lost ledger
			// race — close it as interrupted rather than fabricating success.
			operationState, operationReason = "Interrupted", "parent_terminal"
		}
		var stopBasis any
		if !item.dispatched {
			stopBasis = "not_dispatched"
		}
		if _, err := db.ExecContext(ctx, `UPDATE browser_operations SET state=?,ended_at=?,terminal_reason=?,stop_confirmed_at=COALESCE(stop_confirmed_at,?),stop_confirmation_basis=COALESCE(stop_confirmation_basis,?),row_version=row_version+1 WHERE id=? AND state IN ('Queued','WaitingForCapacity','Starting','Running')`,
			operationState, now, operationReason, nullableTime(now, stopBasis != nil), stopBasis, item.operation); err != nil {
			sharedops.LogEvent("quoin", "error", "config_verification.journey_operation_close", err.Error())
			continue
		}
		if stopBasis == nil {
			go func() { _ = service.dispatchBrowserStop(context.Background(), item.operation) }()
		}
		if item.scope == "run_check" {
			if service.Inspections != nil {
				if err := service.Inspections.RecordJourneyTechnicalGap(ctx, item.attempt, gapReasonForAttempt(item.attemptState)); err != nil {
					sharedops.LogEvent("quoin", "error", "inspection.journey_gap", fmt.Sprintf("attempt=%d error=%s", item.attempt, err.Error()))
				}
			}
		} else {
			service.recordJourneyTechnicalGap(ctx, item.attempt, gapReasonForAttempt(item.attemptState))
		}
	}
	if service.dispatchCancellingJourneyChecks(ctx) {
		more = true
	}
	if service.dispatchReadyJourneyAttempts(ctx) {
		more = true
	}
	if more {
		time.AfterFunc(20*time.Millisecond, func() { service.reconcileJourneyVerificationChildren(context.Background()) })
	}
}

// recordJourneyTechnicalGap settles one browser check whose Attempt reached a
// technical terminal state without a Journey ResultProposal.
func (service *RuntimeService) recordJourneyTechnicalGap(ctx context.Context, attemptID int64, reason string) {
	if reason == "" {
		reason = "interrupted"
	}
	var scope string
	_ = service.BusinessSystems.DB().QueryRowContext(ctx, `SELECT scope_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&scope)
	if scope == "run_check" {
		if service.Inspections != nil {
			if err := service.Inspections.RecordJourneyTechnicalGap(ctx, attemptID, reason); err != nil {
				sharedops.LogEvent("quoin", "error", "inspection.journey_gap", fmt.Sprintf("attempt=%d error=%s", attemptID, err.Error()))
			}
		}
		return
	}
	if err := service.BusinessSystems.RecordVerificationTechnicalGap(ctx, attemptID, reason); err != nil {
		sharedops.LogEvent("quoin", "error", "config_verification.journey_gap", fmt.Sprintf("attempt=%d error=%s", attemptID, err.Error()))
	}
}

// convergeCancelledJourneyChild settles one cancelled journey child after its
// CancelAck: the operation closes as cancelled and the check records its
// technical gap (the cancellation fence already owns the attempt state).
func (service *RuntimeService) convergeCancelledJourneyChild(ctx context.Context, attemptID int64) {
	if service.BusinessSystems == nil {
		return
	}
	db := service.BusinessSystems.DB()
	var operationID int64
	var operationState string
	err := db.QueryRowContext(ctx, `SELECT o.id,o.state FROM execution_attempts a
		JOIN browser_operations o ON o.owner_attempt_id=a.id AND o.kind='journey'
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type IN ('config_verification_run','run_check')`, attemptID).Scan(&operationID, &operationState)
	if err != nil {
		return // not a journey child
	}
	if operationState != "Succeeded" && operationState != "Failed" && operationState != "Cancelled" && operationState != "Interrupted" {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := db.ExecContext(ctx, `UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='cancelled',row_version=row_version+1 WHERE id=? AND state IN ('Queued','WaitingForCapacity','Starting','Running')`, now, operationID); err != nil {
			sharedops.LogEvent("quoin", "error", "config_verification.journey_cancel_close", err.Error())
			return
		}
		go func() { _ = service.dispatchBrowserStop(context.Background(), operationID) }()
	}
	service.recordJourneyTechnicalGap(ctx, attemptID, "cancelled")
	go service.reconcileJourneyVerificationChildren(context.Background())
}

func technicalReason(operationState, operationReason string) string {
	if operationReason != "" {
		switch operationReason {
		case "cancelled":
			return "cancelled"
		}
	}
	return "lease_expired"
}

func gapReasonForOperation(operationState, operationReason string) string {
	if operationState == "Cancelled" || operationReason == "cancelled" {
		return "cancelled"
	}
	return "interrupted"
}

func gapReasonForAttempt(attemptState string) string {
	if attemptState == "Cancelled" {
		return "cancelled"
	}
	return "interrupted"
}

func nullableTime(now string, set bool) any {
	if set {
		return now
	}
	return nil
}

// handleJourneyResultProposal is the lintel-aware adjudication boundary for
// browser_journey_result_v1 (RUNTIME-TASK-012).
func (service *RuntimeService) handleJourneyResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetCorrelationId(), BootId: envelope.GetBootId(),
		Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted, ack.GetResultAck().Detail = false, reason
		_ = service.sendEnvelope(qruntime.SlotLintel, ack)
		sharedops.LogEvent("quoin", "error", "config_verification.journey_rejected", fmt.Sprintf("attempt=%d reason=%s", proposal.GetAttemptId(), reason))
	}
	if service.BusinessSystems == nil {
		reject("business systems are not wired")
		return
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() != "browser_journey_result_v1" || len(payload.GetCanonicalJson()) == 0 {
		reject("expected browser_journey_result_v1 payload")
		return
	}
	var childScope string
	_ = service.BusinessSystems.DB().QueryRowContext(ctx, `SELECT scope_type FROM execution_attempts WHERE id=?`, proposal.GetAttemptId()).Scan(&childScope)
	var commitErr error
	if childScope == "run_check" {
		if service.Inspections == nil {
			reject("inspections are not wired")
			return
		}
		commitErr = service.Inspections.CommitJourneyProposal(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson())
	} else {
		commitErr = service.BusinessSystems.CommitJourneyProposal(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson())
	}
	if commitErr != nil {
		reject(commitErr.Error())
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotLintel, ack)
	if childScope == "run_check" {
		// The journey ledger may have closed the collection and created the
		// analysis attempt; dispatch it without waiting for another event.
		go service.dispatchQueuedInspections(context.Background())
	}
	// The ledger transaction committed the domain terminal states; the
	// physical Stop fence now releases identity and slot.
	var operationID int64
	if service.BusinessSystems.DB().QueryRowContext(ctx, `SELECT operation_id FROM browser_journey_results WHERE attempt_id=?`, proposal.GetAttemptId()).Scan(&operationID) == nil && operationID > 0 {
		go func() { _ = service.dispatchBrowserStop(context.Background(), operationID) }()
	}
	go service.reconcileJourneyVerificationChildren(context.Background())
}
