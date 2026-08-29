package app

// Reconnect reconciliation and loss convergence for the Plinth control
// stream (T12, RUNTIME-TASK-005/006/007, RUNTIME-CANCEL-003): new-boot
// interruption, same-boot reconcile (ReconcileRequest → ReconcileReport),
// heartbeat lease renewal, Cancelling convergence when a stream ends, the
// periodic lease sweeper and the idempotent re-dispatch of Assigned
// attempts the runtime never accepted. Commit order stays with SQLite.

import (
	"context"
	"fmt"
	"sync"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// reconcileTimeout bounds the wait for ReconcileReport after a same-boot
// reconnect; a silent runtime leaves its attempts to the lease sweeper.
const reconcileTimeout = 10 * time.Second

// reconcileWaiters carries one pending ReconcileReport waiter per slot.
type reconcileState struct {
	mu      sync.Mutex
	waiters map[string]chan []int64
}

func (service *RuntimeService) reconcileWaiter(slot string) chan []int64 {
	service.reconcile.mu.Lock()
	defer service.reconcile.mu.Unlock()
	if service.reconcile.waiters == nil {
		service.reconcile.waiters = map[string]chan []int64{}
	}
	waiter, live := service.reconcile.waiters[slot]
	if !live {
		waiter = make(chan []int64, 1)
		service.reconcile.waiters[slot] = waiter
	}
	return waiter
}

// deliverReconcileReport hands one report to the pending waiter (no waiter:
// audit only — a stale or duplicate report is dropped).
func (service *RuntimeService) deliverReconcileReport(slot string, running []int64) {
	service.reconcile.mu.Lock()
	waiter, live := service.reconcile.waiters[slot]
	if live {
		delete(service.reconcile.waiters, slot)
	}
	service.reconcile.mu.Unlock()
	if live {
		waiter <- running
	}
}

// attemptsService returns the shared attempt state machine (the analysis
// service owns the same product database).
func (service *RuntimeService) attemptsService() *attempt.Service {
	if service.Analyses != nil {
		return service.Analyses.Attempts()
	}
	return nil
}

// onPlinthAttached adjudicates every active plinth attempt after an
// accepted Hello: attempts bound to a different boot interrupt immediately
// (RUNTIME-TASK-006); same-boot attempts reconcile without re-dispatch
// (RUNTIME-TASK-005).
func (service *RuntimeService) onPlinthAttached(ctx context.Context, helloBoot string, helloEpoch uint64) {
	attempts := service.attemptsService()
	if attempts == nil {
		return
	}
	active, err := attempts.ActiveOfSlot(ctx, qruntime.SlotPlinth)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "reconcile.scan_failed", err.Error())
		return
	}
	var sameBoot, replacedBoot []attempt.View
	for _, view := range active {
		if view.BootID == nil || *view.BootID != helloBoot {
			replacedBoot = append(replacedBoot, view)
			continue
		}
		sameBoot = append(sameBoot, view)
	}
	// ToolResultDelivery is an at-least-once projection of the durable
	// tool-call ledger, not an in-memory queue. Replay it before interruption
	// convergence: a replacement Plinth must see an already-durable browser
	// result before its predecessor's scope is finalized.
	service.replayUndeliveredBrowserToolResults(ctx)
	for _, view := range replacedBoot {
		service.finalizeLoss(ctx, view, "lease_expired")
		sharedops.LogEvent("quoin", "info", "reconcile.new_boot_interrupt", fmt.Sprintf("attempt=%d oldBoot=%v", view.ID, view.BootID))
	}
	if len(sameBoot) == 0 {
		// Even with nothing bound, a Queued investigation created while the
		// slot was disconnected must dispatch now that the stream is live
		// (RUNTIME-TASK-005: the send command is the trigger; the stream is
		// the carrier).
		if service.InvestigationRuntime != nil {
			service.InvestigationRuntime.DispatchQueued(ctx)
		}
		return
	}
	service.reconcileSameBoot(ctx, helloBoot, helloEpoch, sameBoot)
	if service.InvestigationRuntime != nil {
		service.InvestigationRuntime.DispatchQueued(ctx)
	}
}

// finalizeLoss routes one loss convergence to the owning scope aggregate.
func (service *RuntimeService) finalizeLoss(ctx context.Context, view attempt.View, reason string) {
	if view.AttemptType == "initial_analysis" && service.Analyses != nil {
		if err := service.Analyses.CommitInterruption(ctx, view.ID, reason); err != nil {
			sharedops.LogEvent("quoin", "error", "reconcile.interrupt_failed", fmt.Sprintf("attempt=%d %v", view.ID, err))
		}
		return
	}
	if view.AttemptType == "investigation" {
		// Create recovery_loss before *any* browser activity observation. The
		// same BEGIN IMMEDIATE ordering point owns both the no-browser fast path
		// and a concurrent late Exploration start, so loss can never terminalize
		// the parent in the gap between an activity check and the insert.
		if err := service.freezeRecoveryLossPending(ctx, view.ID, reason); err != nil {
			sharedops.LogEvent("quoin", "error", "reconcile.interrupt_pending_failed", fmt.Sprintf("attempt=%d %v", view.ID, err))
			return
		}
		// The pending work item drains immediately if there are no obligations;
		// otherwise it is the durable parent closure authority until StopAck.
		service.reconcileTerminalParentExplorations(ctx)
		return
	}
	attempts := service.attemptsService()
	if attempts == nil {
		return
	}
	if _, err := attempts.Interrupt(ctx, view.ID, reason); err != nil {
		sharedops.LogEvent("quoin", "error", "reconcile.interrupt_failed", fmt.Sprintf("attempt=%d %v", view.ID, err))
		return
	}
	if view.AttemptType == "inspection_collection" && service.BusinessSystems != nil {
		var err error
		switch view.ScopeType {
		case "config_verification_run":
			err = service.BusinessSystems.RecordVerificationTechnicalGap(ctx, view.ID, reason)
		case "resource_refresh_run":
			err = service.BusinessSystems.RecordResourceRefreshTechnicalGap(ctx, view.ID, reason)
		case "run_check":
			if service.Inspections != nil {
				err = service.Inspections.RecordPromQLTechnicalGap(ctx, view.ID, reason)
			}
		}
		if err != nil {
			sharedops.LogEvent("quoin", "error", "inspection_collection.interrupt_failed", fmt.Sprintf("attempt=%d %v", view.ID, err))
		}
	}
	if view.AttemptType == "investigation" && service.Investigations != nil {
		// Close the attached stream with the interruption terminal view
		// (HTTP-STREAM-006: detach/loss never leaves the observer hanging).
		service.Investigations.NotifyTerminal(ctx, view.ID)
	}
}

// freezeRecoveryLossPending creates the no-model persistent closure before a
// lost Investigation is allowed to become Interrupted. SQLite serialization
// makes duplicate reconciliation/reconnect scans harmless.
func (service *RuntimeService) freezeRecoveryLossPending(ctx context.Context, attemptID int64, reason string) error {
	if service.Analyses == nil {
		return fmt.Errorf("analysis service unavailable")
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
	var state, attemptType string
	if err = conn.QueryRowContext(ctx, `SELECT state,attempt_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &attemptType); err != nil {
		return err
	}
	if state != "Running" || attemptType != "investigation" {
		return nil
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO pending_attempt_terminals(attempt_id,source,target_state,terminal_reason,created_at)
		VALUES(?,'recovery_loss','Interrupted',?,?) ON CONFLICT(attempt_id) DO NOTHING`, attemptID, reason, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// finalizeCancellation routes one Cancelling convergence to the owning
// scope aggregate (the runtime confirmed the stop, or the stream ended /
// the attempt was lost with the fence already committed).
func (service *RuntimeService) finalizeCancellation(ctx context.Context, attemptID int64, attemptType string) {
	if attemptType == "investigation" {
		// Cancel every pre-start operation before acknowledging its parent. A
		// queued/Waiting operation has no physical process and is closed with the
		// explicit not-dispatched cleanup fact. A Starting operation is terminalized
		// first and then receives Stop, which creates a Lintel tombstone even if the
		// delayed Start reaches Lintel afterwards.
		service.cancelUnstartedExplorations(ctx, attemptID)
		if service.hasRunningExploration(ctx, attemptID) {
			// The browser operation, rather than a currently active child, is the
			// authoritative cancellation fence. This includes Starting, Running and
			// terminal-but-not-yet-cleaned operations; otherwise an ownerless late
			// Start can survive after the parent cancellation is acknowledged.
			service.dispatchCancellingBrowserExplorations(ctx, attemptID)
			return
		}
	}
	if attemptType == "inspection_collection" && service.BusinessSystems != nil {
		view, err := service.attemptsService().Get(ctx, attemptID)
		if err == nil && view.ScopeType == "config_verification_run" {
			if cancelErr := service.BusinessSystems.VerificationAttempts().CancelAck(ctx, attemptID); cancelErr != nil {
				sharedops.LogEvent("quoin", "error", "config_verification.cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, cancelErr))
			}
			return
		}
		if err == nil && view.ScopeType == "resource_refresh_run" {
			if err := service.attemptsService().CancelAck(ctx, attemptID); err != nil {
				sharedops.LogEvent("quoin", "error", "resource_refresh.cancel_ack", fmt.Sprintf("attempt=%d %v", attemptID, err))
				return
			}
			if err := service.BusinessSystems.RecordResourceRefreshTechnicalGap(ctx, attemptID, "cancelled"); err != nil {
				sharedops.LogEvent("quoin", "error", "resource_refresh.cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, err))
			}
			return
		}
	}
	switch attemptType {
	case "inspection_collection":
		if service.Inspections != nil {
			var scopeType string
			_ = service.Inspections.DB().QueryRowContext(ctx, `SELECT scope_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeType)
			if scopeType == "run_check" {
				if err := service.Inspections.Attempts().CancelAck(ctx, attemptID); err != nil {
					sharedops.LogEvent("quoin", "error", "inspection.cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, err))
					return
				}
				service.convergeCancelledJourneyChild(ctx, attemptID)
				return
			}
		}
	case "inspection_analysis":
		if service.Inspections != nil {
			if err := service.Inspections.Attempts().CancelAck(ctx, attemptID); err != nil {
				sharedops.LogEvent("quoin", "error", "inspection.analysis_cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, err))
			}
		}
	case "initial_analysis":
		if service.Analyses != nil {
			if err := service.Analyses.CancelAck(ctx, attemptID); err != nil {
				sharedops.LogEvent("quoin", "error", "reconcile.cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, err))
			}
		}
	case "investigation":
		if service.Investigations != nil {
			if err := service.Investigations.CancelAck(ctx, attemptID); err != nil {
				sharedops.LogEvent("quoin", "error", "reconcile.cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, err))
			}
		}
	case "connection_probe":
		if service.Connections != nil {
			if err := service.Connections.RecordCancelAck(ctx, attemptID); err != nil {
				sharedops.LogEvent("quoin", "error", "reconcile.cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, err))
			}
		}
	}
}

// reconcileSameBoot aligns the slot's same-boot active set with the
// runtime's report (RUNTIME-TASK-005): reported attempts renew their lease
// (and an accept the stream lost is recorded), unreported Assigned attempts
// re-dispatch idempotently with their frozen binding, an unreported Running
// attempt is a crash observation (interrupt without resuming worker
// memory) and an unreported Cancelling attempt converges to Cancelled.
// hasRunningExploration prevents parent cancellation from completing while an
// Exploration can still create or retain a physical Chromium process. It is not
// limited to Running: Starting can race a delayed Start frame, and a terminal
// operation still owns cleanup until StopAck records stop_confirmed_at.
func (service *RuntimeService) hasRunningExploration(ctx context.Context, parentID int64) bool {
	if service.Analyses == nil || parentID < 1 {
		return false
	}
	var exists int
	err := service.Analyses.DB().QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM browser_operations
		WHERE owner_attempt_id=? AND kind='exploration'
		  AND (state IN ('Queued','WaitingForCapacity','Starting','Running')
		       OR stop_confirmed_at IS NULL)
	)`, parentID).Scan(&exists)
	return err == nil && exists == 1
}

// cancelUnstartedExplorations closes only operations for which Lintel proved no
// Chromium can exist. Starting is deliberately left for the operation-level
// cancellation protocol: Lintel may already have created Chromium, so it must
// seal its incomplete trace before Quoin records Stop confirmation. It does not
// write browser child attempt states: the Tool Call trigger owns their terminal
// transition.
func (service *RuntimeService) cancelUnstartedExplorations(ctx context.Context, parentID int64) {
	if service.Analyses == nil || parentID < 1 {
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Queued has never sent Start. WaitingForCapacity is different: it may retain
	// a sent Start fence after Lintel explicitly returned NO_CAPACITY. That Ack
	// proves no Chromium was created, but the historical start fence must remain
	// immutable, so it needs its own cleanup basis rather than not_dispatched.
	if _, err = conn.ExecContext(ctx, `UPDATE browser_operations
		SET state='Cancelled', ended_at=?, terminal_reason='cancelled',
		    stop_confirmed_at=?, stop_confirmation_basis='not_dispatched',
		    row_version=row_version+1
		WHERE owner_attempt_id=? AND kind='exploration' AND state='Queued'`, now, now, parentID); err != nil {
		return
	}
	if _, err = conn.ExecContext(ctx, `UPDATE browser_operations
		SET state='Cancelled', ended_at=?, terminal_reason='cancelled',
		    stop_confirmed_at=?, stop_confirmation_basis='no_capacity',
		    row_version=row_version+1
		WHERE owner_attempt_id=? AND kind='exploration' AND state='WaitingForCapacity'`, now, now, parentID); err != nil {
		return
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return
	}
	committed = true
	conn.Close()
	// Already-terminal dispatched operations still need a same-boot Stop
	// tombstone. Starting operations are intentionally not transitioned here:
	// dispatchCancellingBrowserExplorations sends their trace-bearing cancel
	// first, then Stop fences a delayed Start.
	rows, err := service.Analyses.DB().QueryContext(ctx, `SELECT id FROM browser_operations
		WHERE owner_attempt_id=? AND kind='exploration' AND state='Cancelled'
		  AND start_dispatched_at IS NOT NULL AND stop_confirmed_at IS NULL`, parentID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		_ = service.dispatchBrowserStop(context.Background(), id)
	}
}

// dispatchCancellingBrowserExplorations asks Lintel to fence each child which
// SQLite placed in Cancelling when its parent Tool Call was cancelled. It never
// writes a child terminal state: the existing Tool Call terminal trigger owns
// that transition.
func (service *RuntimeService) dispatchCancellingBrowserExplorations(ctx context.Context, parentID int64) bool {
	if service.Analyses == nil || service.Slots == nil {
		return false
	}
	view, err := service.Slots.View(ctx, qruntime.SlotLintel)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return false
	}
	rows, err := service.Analyses.DB().QueryContext(ctx, `SELECT b.operation_id,b.child_attempt_id,b.tool_call_id
		FROM browser_exploration_child_bindings b
		JOIN execution_attempts c ON c.id=b.child_attempt_id
		JOIN browser_operations o ON o.id=b.operation_id
		WHERE b.parent_attempt_id=? AND o.kind='exploration' AND c.state='Cancelling'`, parentID)
	if err != nil {
		return false
	}
	defer rows.Close()
	dispatched := false
	for rows.Next() {
		var operationID, childID, toolID int64
		if rows.Scan(&operationID, &childID, &toolID) == nil {
			dispatched = true
			_ = service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: view.BootID, ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(childID), Msg: &runtimev1.ControlEnvelope_CancelBrowserExplorationAction{CancelBrowserExplorationAction: &runtimev1.CancelBrowserExplorationAction{OperationId: operationID, ChildAttemptId: childID, ParentAttemptId: parentID, ToolCallId: toolID}}})
		}
	}
	// The database uses a single connection in production; close the first
	// cursor before querying idle operations below.
	if err := rows.Close(); err != nil {
		return dispatched
	}
	// An Exploration can be idle immediately after StartAck or between two
	// actions. In both intervals there is no Cancelling child to carry the
	// cancellation fence, but Chromium is still owned by the parent Attempt.
	// Dispatch an operation-level cancellation (zero child/tool IDs) so Lintel
	// commits its incomplete trace and sends CompleteBrowserOperation; do not
	// acknowledge the parent until that durable operation terminal fact arrives.
	idleRows, err := service.Analyses.DB().QueryContext(ctx, `SELECT o.id,o.state
		FROM browser_operations o
		WHERE o.owner_attempt_id=? AND o.kind='exploration' AND o.state IN ('Starting','Running')
		  AND NOT EXISTS (
			SELECT 1 FROM browser_exploration_child_bindings b
			JOIN execution_attempts c ON c.id=b.child_attempt_id
			WHERE b.operation_id=o.id AND c.state='Cancelling'
		  )`, parentID)
	if err != nil {
		return dispatched
	}
	type idleOperation struct {
		id    int64
		state string
	}
	var idle []idleOperation
	for idleRows.Next() {
		var operation idleOperation
		if idleRows.Scan(&operation.id, &operation.state) != nil {
			_ = idleRows.Close()
			return dispatched
		}
		idle = append(idle, operation)
	}
	// dispatchBrowserStop re-enters the Browser authority. Close this cursor
	// first: production intentionally permits a single SQLite connection, and
	// keeping a read cursor open here self-locks the cancellation path.
	if idleRows.Err() != nil || idleRows.Close() != nil {
		return dispatched
	}
	for _, operation := range idle {
		dispatched = true
		// Zero child/tool IDs preserve the operation-level cancellation trace
		// when StartAck already made Chromium live. If this frame races a
		// delayed Start, the parallel Stop tombstone below prevents a late
		// Chromium process from becoming ownerless.
		_ = service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: view.BootID, ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(operation.id), Msg: &runtimev1.ControlEnvelope_CancelBrowserExplorationAction{CancelBrowserExplorationAction: &runtimev1.CancelBrowserExplorationAction{OperationId: operation.id, ParentAttemptId: parentID}}})
		if operation.state == "Starting" {
			_ = service.dispatchBrowserStop(context.Background(), operation.id)
		}
	}
	return dispatched
}

func (service *RuntimeService) reconcileSameBoot(ctx context.Context, bootID string, epoch uint64, active []attempt.View) {
	attempts := service.attemptsService()
	if attempts == nil {
		return
	}
	ids := make([]int64, 0, len(active))
	for _, view := range active {
		ids = append(ids, view.ID)
	}
	if err := service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: epoch,
		BootId:          bootID,
		Msg:             &runtimev1.ControlEnvelope_ReconcileRequest{ReconcileRequest: &runtimev1.ReconcileRequest{ActiveAttemptIds: ids}},
	}); err != nil {
		sharedops.LogEvent("quoin", "error", "reconcile.request_send", err.Error())
		return
	}
	waiter := service.reconcileWaiter(qruntime.SlotPlinth)
	select {
	case running := <-waiter:
		service.alignReconcileReport(ctx, bootID, active, running)
	case <-time.After(reconcileTimeout):
		// Drop the stale waiter so a later report cannot consume a future
		// reconcile round; the lease sweeper owns the unspeaked attempts.
		service.reconcile.mu.Lock()
		delete(service.reconcile.waiters, qruntime.SlotPlinth)
		service.reconcile.mu.Unlock()
		sharedops.LogEvent("quoin", "info", "reconcile.report_timeout", fmt.Sprintf("attempts=%d lease sweeper owns them", len(active)))
	case <-ctx.Done():
	}
}

// alignReconcileReport applies the report alignment for one reconnect.
func (service *RuntimeService) alignReconcileReport(ctx context.Context, bootID string, active []attempt.View, running []int64) {
	attempts := service.attemptsService()
	reported := map[int64]bool{}
	for _, id := range running {
		reported[id] = true
	}
	renew := false
	for _, view := range active {
		if reported[view.ID] {
			renew = true
			if view.State == "Cancelling" && (view.AttemptType == "inspection_collection" || view.AttemptType == "inspection_analysis") {
				// The Runtime is still executing a fence whose first send may have
				// been lost. Re-send rather than treating its active report as proof
				// that the cancellation converged.
				if err := service.dispatchInspectionCancellation(ctx, view.ID); err != nil {
					sharedops.LogEvent("quoin", "error", "reconcile.cancel_replay", fmt.Sprintf("attempt=%d %v", view.ID, err))
				}
				continue
			}
			if view.State == "Assigned" && service.Analyses != nil && view.AttemptType == "initial_analysis" {
				// The accept was lost with the stream; the runtime is
				// already executing the attempt.
				if err := service.Analyses.AcceptAttempt(ctx, view.ID, bootID, 0); err != nil {
					sharedops.LogEvent("quoin", "error", "reconcile.accept_restore", fmt.Sprintf("attempt=%d %v", view.ID, err))
				}
			}
			if view.State == "Assigned" && view.AttemptType == "investigation" && service.Investigations != nil {
				// The investigation aggregate has no separate state row;
				// the attempt row is the authority.
				if err := service.Investigations.AcceptAttempt(ctx, view.ID, bootID, 0); err != nil {
					sharedops.LogEvent("quoin", "error", "reconcile.accept_restore", fmt.Sprintf("attempt=%d %v", view.ID, err))
				}
			}
			if view.State == "Assigned" && (view.AttemptType == "inspection_collection" || view.AttemptType == "inspection_analysis") && service.Inspections != nil {
				if err := service.Inspections.Attempts().Accept(ctx, view.ID, bootID, 0); err != nil {
					sharedops.LogEvent("quoin", "error", "reconcile.accept_restore", fmt.Sprintf("attempt=%d %v", view.ID, err))
				}
			}
			continue
		}
		switch view.State {
		case "Assigned":
			// Never accepted by the runtime: idempotent re-dispatch with
			// the frozen binding (RUNTIME-TASK-005).
			var err error
			if view.AttemptType == "inspection_collection" && view.ScopeType == "config_verification_run" {
				err = service.dispatchVerificationAttempt(ctx, view.ID)
			} else if view.AttemptType == "inspection_collection" && view.ScopeType == "resource_refresh_run" {
				err = service.dispatchResourceRefreshAttempt(ctx, view.ID)
			} else {
				err = service.reDispatchAgentAttempt(ctx, view)
			}
			if err != nil {
				sharedops.LogEvent("quoin", "error", "reconcile.redispatch", fmt.Sprintf("attempt=%d %v", view.ID, err))
			} else {
				sharedops.LogEvent("quoin", "info", "reconcile.redispatched", fmt.Sprintf("attempt=%d", view.ID))
			}
		case "Running":
			// The runtime lost the attempt (worker/supervisor task gone):
			// frozen Interrupted semantics, no worker-memory resume.
			service.finalizeLoss(ctx, view, "lease_expired")
			sharedops.LogEvent("quoin", "info", "reconcile.lost_running", fmt.Sprintf("attempt=%d", view.ID))
		case "Cancelling":
			service.finalizeCancellation(ctx, view.ID, view.AttemptType)
			sharedops.LogEvent("quoin", "info", "reconcile.cancel_converged", fmt.Sprintf("attempt=%d", view.ID))
		}
	}
	if renew {
		if err := attempts.RenewLeaseForBoot(ctx, qruntime.SlotPlinth, bootID, attempt.DispatchLease); err != nil {
			sharedops.LogEvent("quoin", "error", "reconcile.lease_renew", err.Error())
		}
	}
}

// reDispatchAgentAttempt re-sends the DispatchAttempt frame for one Assigned
// agent attempt with its frozen binding (the schema forbids rebinding;
// the accept fence matches the boot, RUNTIME-TASK-005).
func (service *RuntimeService) reDispatchAgentAttempt(ctx context.Context, view attempt.View) error {
	if service.Analyses == nil && service.Investigations == nil {
		return fmt.Errorf("agent services not wired")
	}
	attempts := service.attemptsService()
	if (view.AttemptType == "inspection_collection" || view.AttemptType == "inspection_analysis") && service.Inspections != nil {
		attempts = service.Inspections.Attempts()
	}
	input, err := attempts.DispatchInputFor(ctx, view.ID)
	if err != nil {
		return err
	}
	var artifactRefs []*runtimev1.ArtifactRef
	for _, ref := range input.ArtifactRefs {
		artifactRefs = append(artifactRefs, &runtimev1.ArtifactRef{
			ArtifactId: ref.ArtifactID, Role: ref.Role, MediaType: ref.MediaType,
			SizeBytes: uint64(ref.SizeBytes), Sha256: ref.SHA256, BodyExpired: ref.BodyExpired,
		})
	}
	var grants []*runtimev1.ConnectionGrant
	for _, grant := range input.Grants {
		grants = append(grants, &runtimev1.ConnectionGrant{
			GrantId: grant.GrantID, ConnectionRevisionId: grant.ConnectionRevisionID,
			CredentialGenerationId: grant.CredentialGenerationID, Purpose: grant.Purpose,
			ConnectionProbeResultId: grant.ConnectionProbeResultID,
		})
	}
	bindingEpoch := uint64(0)
	if view.ConnectionEpoch != nil {
		bindingEpoch = uint64(*view.ConnectionEpoch)
	}
	bindingBoot := ""
	if view.BootID != nil {
		bindingBoot = *view.BootID
	}
	attemptWire := runtimev1.AttemptType_ATTEMPT_TYPE_INITIAL_ANALYSIS
	scopeWire := runtimev1.ScopeType_SCOPE_TYPE_ANALYSIS
	if view.AttemptType == "investigation" {
		attemptWire = runtimev1.AttemptType_ATTEMPT_TYPE_INVESTIGATION
		scopeWire = runtimev1.ScopeType_SCOPE_TYPE_INVESTIGATION
	} else if view.AttemptType == "inspection_collection" && view.ScopeType == "run_check" {
		attemptWire = runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_COLLECTION
		scopeWire = runtimev1.ScopeType_SCOPE_TYPE_RUN_CHECK
	} else if view.AttemptType == "inspection_analysis" && view.ScopeType == "run" {
		attemptWire = runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_ANALYSIS
		scopeWire = runtimev1.ScopeType_SCOPE_TYPE_RUN
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: bindingEpoch,
		CorrelationId:   uint64(view.ID),
		BootId:          bindingBoot,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{
			AttemptId:     view.ID,
			AttemptType:   attemptWire,
			ScopeType:     scopeWire,
			ScopeId:       view.ScopeID,
			LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
			Input: &runtimev1.AttemptInputSnapshot{
				SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON,
				ContentDigest: input.ContentDigest, ArtifactRefs: artifactRefs,
				ConnectionGrants: grants, AgentVersion: input.AgentVersion,
			},
		}},
	})
}

// onPlinthStreamEnded preserves Cancelling attempts bound to the ended stream.
// A detached same-boot stream is not proof that its worker stopped, so turning
// the fence into Cancelled here could lie about physical execution. Reconnect
// replay re-sends CancelAttempt; a new boot or an expired lease converges loss.
func (service *RuntimeService) onPlinthStreamEnded(ctx context.Context, bootID string, epoch uint64) {
	attempts := service.attemptsService()
	if attempts == nil {
		return
	}
	active, err := attempts.ActiveOfSlot(ctx, qruntime.SlotPlinth)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "reconcile.stream_end_scan", err.Error())
		return
	}
	for _, view := range active {
		if view.State != "Cancelling" || view.BootID == nil || *view.BootID != bootID {
			continue
		}
		if view.ConnectionEpoch != nil && uint64(*view.ConnectionEpoch) != epoch {
			continue
		}
		sharedops.LogEvent("quoin", "info", "reconcile.stream_end_cancel_pending", fmt.Sprintf("attempt=%d", view.ID))
	}
}

// renewPlinthLeases extends the lease of the live stream's active attempts
// on every heartbeat (RUNTIME-TASK-007; the heartbeat itself never writes
// runtime_slots, RUNTIME-CTRL-005).
func (service *RuntimeService) renewPlinthLeases(ctx context.Context, bootID string) {
	attempts := service.attemptsService()
	if attempts == nil {
		return
	}
	if err := attempts.RenewLeaseForBoot(ctx, qruntime.SlotPlinth, bootID, attempt.DispatchLease); err != nil {
		sharedops.LogEvent("quoin", "error", "reconcile.heartbeat_renew", err.Error())
	}
}

// RunLeaseSweeper loops the periodic lease convergence until the context
// ends; each sweep routes its outcomes to the owning scope aggregates.
func (service *RuntimeService) RunLeaseSweeper(ctx context.Context) {
	attempts := service.attemptsService()
	if attempts == nil {
		return
	}
	ticker := time.NewTicker(attempt.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			swept, err := attempts.SweepExpired(ctx)
			if err != nil {
				sharedops.LogEvent("quoin", "error", "reconcile.sweep_failed", err.Error())
				continue
			}
			for _, item := range swept {
				if item.DeferredLoss {
					// A browser child lease is routed through its parent because the
					// parent owns the one trace and recovery-loss state machine.
					lossID := item.AttemptID
					if item.BrowserParentAttemptID != 0 {
						lossID = item.BrowserParentAttemptID
					}
					view, viewErr := attempts.Get(ctx, lossID)
					if viewErr != nil {
						sharedops.LogEvent("quoin", "error", "reconcile.sweep_candidate", fmt.Sprintf("attempt=%d %v", lossID, viewErr))
						continue
					}
					if view.State == "Cancelling" {
						// A cancellation fence does not erase a live browser obligation.
						// The typed trace/Stop closure is the sole terminal writer.
						service.finalizeCancellation(ctx, view.ID, view.AttemptType)
					} else {
						service.finalizeLoss(ctx, view, "lease_expired")
					}
					continue
				}
				sharedops.LogEvent("quoin", "info", "reconcile.swept", fmt.Sprintf("attempt=%d final=%s", item.AttemptID, item.Final))
				switch item.Type {
				case "initial_analysis":
					if service.Analyses != nil {
						if err := service.Analyses.CommitInterruption(ctx, item.AttemptID, "lease_expired"); err != nil {
							sharedops.LogEvent("quoin", "error", "reconcile.sweep_closure", fmt.Sprintf("attempt=%d %v", item.AttemptID, err))
						}
					}
				case "investigation":
					if service.Investigations != nil {
						// The sweep converged the attempt row; close any
						// attached stream with the interruption terminal view.
						service.Investigations.NotifyTerminal(ctx, item.AttemptID)
					}
				case "inspection_collection":
					if service.BusinessSystems != nil {
						var closeErr error
						switch item.ScopeType {
						case "run_check":
							if service.Inspections != nil {
								closeErr = service.Inspections.RecordPromQLTechnicalGap(ctx, item.AttemptID, "interrupted")
							}
						case "config_verification_run":
							closeErr = service.BusinessSystems.RecordVerificationTechnicalGap(ctx, item.AttemptID, "interrupted")
						case "resource_refresh_run":
							closeErr = service.BusinessSystems.RecordResourceRefreshTechnicalGap(ctx, item.AttemptID, "interrupted")
						default:
							sharedops.LogEvent("quoin", "info", "reconcile.sweep_scope_unhandled", fmt.Sprintf("attempt=%d scope=%s", item.AttemptID, item.ScopeType))
						}
						if closeErr != nil {
							sharedops.LogEvent("quoin", "error", "inspection_collection.sweep_closure", fmt.Sprintf("attempt=%d %v", item.AttemptID, closeErr))
						}
					}
				}
			}
		}
	}
}

// dispatchAllCancellingBrowserExplorations replays cancellation after a Lintel
// reconnect. The database remains the authority; a lost send changes nothing.
// dispatchAllCancellingInspections replays durable Plinth cancellation fences
// after reconnect. Sending is best effort; the fence stays Cancelling and this
// method retries on every attachment until a CancelAck or loss convergence.
func (service *RuntimeService) dispatchAllCancellingInspections(ctx context.Context) {
	if service.Inspections == nil {
		return
	}
	rows, err := service.Inspections.DB().QueryContext(ctx, `
		SELECT a.id FROM execution_attempts a
		WHERE a.state='Cancelling' AND a.attempt_type IN ('inspection_collection','inspection_analysis')
		AND NOT EXISTS (SELECT 1 FROM browser_operations b WHERE b.owner_attempt_id=a.id AND b.kind='journey')
		ORDER BY a.id`)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "inspection.cancel_replay", err.Error())
		return
	}
	// SQLite uses a single connection. Materialize every durable fence and
	// release this cursor before dispatchInspectionCancellation re-reads the
	// Attempt and the active Runtime slot.
	var attemptIDs []int64
	for rows.Next() {
		var attemptID int64
		if err := rows.Scan(&attemptID); err != nil {
			_ = rows.Close()
			sharedops.LogEvent("quoin", "error", "inspection.cancel_replay", err.Error())
			return
		}
		attemptIDs = append(attemptIDs, attemptID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		sharedops.LogEvent("quoin", "error", "inspection.cancel_replay", err.Error())
		return
	}
	if err := rows.Close(); err != nil {
		sharedops.LogEvent("quoin", "error", "inspection.cancel_replay", err.Error())
		return
	}
	for _, attemptID := range attemptIDs {
		if err := service.dispatchInspectionCancellation(ctx, attemptID); err != nil {
			sharedops.LogEvent("quoin", "error", "inspection.cancel_replay", fmt.Sprintf("attempt=%d %v", attemptID, err))
		}
	}
}

func (service *RuntimeService) dispatchAllCancellingBrowserExplorations(ctx context.Context) {
	if service.Analyses == nil {
		return
	}
	rows, err := service.Analyses.DB().QueryContext(ctx, `SELECT DISTINCT parent.id
		FROM execution_attempts parent
		JOIN browser_operations o ON o.owner_attempt_id=parent.id AND o.kind='exploration'
		WHERE parent.state='Cancelling'
		  AND o.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect')`)
	if err != nil {
		return
	}
	// The SQLite pool is single-connection. Materialize IDs and release the
	// cursor before finalizeCancellation opens its own transaction; reconnect
	// must not self-lock precisely when a Cancelling parent needs recovery.
	var parentIDs []int64
	for rows.Next() {
		var parentID int64
		if rows.Scan(&parentID) == nil {
			parentIDs = append(parentIDs, parentID)
		}
	}
	if rows.Close() != nil {
		return
	}
	for _, parentID := range parentIDs {
		// Re-run the complete cancellation materialization, not merely the
		// child-action send: a reconnect may occur while the parent is
		// Cancelling before any child/action row was promoted.
		service.finalizeCancellation(ctx, parentID, "investigation")
	}
}

// replayRunningBrowserExplorationChildren re-emits each action that was
// durably promoted before a Lintel control-stream disconnect. The persisted
// child binding, rather than a process-local pending map, is the authority for
// the exact operation and child to replay.
func (service *RuntimeService) replayRunningBrowserExplorationChildren(ctx context.Context) {
	if service.Analyses == nil {
		return
	}
	service.reconcileTerminalParentExplorations(ctx)
	rows, err := service.Analyses.DB().QueryContext(ctx, `SELECT b.child_attempt_id
		FROM browser_exploration_child_bindings b
		JOIN execution_attempts c ON c.id=b.child_attempt_id
		JOIN browser_operations o ON o.id=b.operation_id
		JOIN execution_attempts parent ON parent.id=b.parent_attempt_id
		JOIN browser_exploration_actions a ON a.child_attempt_id=b.child_attempt_id
		WHERE c.state='Running' AND parent.state='Running' AND o.kind='exploration' AND o.state='Running' AND a.outcome IS NULL`)
	if err != nil {
		return
	}
	// The SQLite pool is deliberately one connection. Read every durable ID and
	// release the rows before replayBrowserExplorationChild re-reads the active
	// Lintel slot, otherwise reconnect reconciliation self-deadlocks.
	var childIDs []int64
	for rows.Next() {
		var childID int64
		if rows.Scan(&childID) == nil {
			childIDs = append(childIDs, childID)
		}
	}
	if err := rows.Close(); err != nil {
		return
	}
	for _, childID := range childIDs {
		if err := service.replayBrowserExplorationChild(ctx, childID); err != nil {
			sharedops.LogEvent("quoin", "warn", "browser.exploration_action_replay_failed", fmt.Sprintf("child=%d %v", childID, err))
		}
	}
}

// reconcileTerminalParentExplorations restores the post-commit close dispatch
// after Quoin restarted or Lintel reconnected between the parent transaction and
// its outbound frame. Parent terminal state is the durable work item; duplicate
// sends are harmless under Lintel's operation-level terminal CAS.
// reconcilePendingAttemptTerminals drains frozen natural results only after
// every Exploration owned by the Attempt is terminal and any dispatched
// browser process has supplied its StopAck. The pending row, not a post-commit
// callback, is the crash-safe precommit authority.
func (service *RuntimeService) reconcilePendingAttemptTerminals(ctx context.Context) {
	if service.Analyses == nil || service.Investigations == nil {
		return
	}
	// Queued and NO_CAPACITY operations have a proven/no-dispatch physical
	// outcome. Close them before checking terminal readiness: a natural parent
	// result must not wait forever for a browser action that cannot exist.
	service.closePendingUnstartedExplorations(ctx)
	rows, err := service.Analyses.DB().QueryContext(ctx, `SELECT p.attempt_id
		FROM pending_attempt_terminals p
		WHERE NOT EXISTS (
			SELECT 1 FROM browser_operations o WHERE o.owner_attempt_id=p.attempt_id AND o.kind='exploration'
			  AND (o.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect')
			       OR (o.start_dispatched_at IS NOT NULL AND o.stop_confirmed_at IS NULL))
		)
		-- An immutable complete trace was accepted under its terminal claim before
		-- the parent loss proposal. It is a still-deliverable normal close, not a
		-- browser obligation recovery may silently turn into cancellation. Wait for
		-- the matching ActionResult/replay to finalize the claim.
		AND NOT EXISTS (
			SELECT 1
			FROM browser_exploration_terminal_claims claim
			JOIN browser_exploration_child_bindings binding ON binding.child_attempt_id=claim.child_attempt_id
			WHERE binding.parent_attempt_id=p.attempt_id
			  AND claim.state='artifact_committed_complete'
		)`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	if rows.Close() != nil {
		return
	}
	for _, id := range ids {
		result, committed, err := service.Investigations.CommitPendingTerminal(ctx, id)
		if err != nil || !committed {
			continue
		}
		// ResultAck is a control-stream receipt, not part of the frozen result
		// proposal. A same-boot reconnect advances the stream epoch while Plinth
		// legitimately retains the proposal for replay. Sending to its old epoch
		// makes the one durable terminal commit unacknowledgeable forever.
		view, viewErr := service.Slots.View(ctx, qruntime.SlotPlinth)
		if viewErr != nil || !view.Connected || view.ConnectionEpoch == nil || view.BootID != result.BootID {
			continue
		}
		_ = service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{BootId: view.BootID, ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(result.AttemptID), Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: result.AttemptID, Accepted: true}}})
	}
}

// closePendingUnstartedExplorations materializes the no-process branch of a
// natural pending parent terminal. WaitingForCapacity retains its historical
// Start fence but NO_CAPACITY has already proved that Lintel did not create a
// process, so no Stop frame is necessary.
func (service *RuntimeService) closePendingUnstartedExplorations(ctx context.Context) {
	if service.Analyses == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
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
	if _, err = conn.ExecContext(ctx, `UPDATE browser_operations
		SET state='Cancelled',ended_at=?,terminal_reason='parent_terminal',
		    stop_confirmed_at=?,stop_confirmation_basis=CASE state
		      WHEN 'WaitingForCapacity' THEN 'no_capacity' ELSE 'not_dispatched' END,
		    row_version=row_version+1
		WHERE kind='exploration' AND state IN ('Queued','WaitingForCapacity')
		  AND EXISTS(SELECT 1 FROM pending_attempt_terminals p WHERE p.attempt_id=browser_operations.owner_attempt_id)`, now, now); err != nil {
		return
	}
	// The parent is still Running while its immutable terminal proposal waits.
	// Closing only BrowserOperation leaves its queued Tool Call and child Attempt
	// live, which then makes the parent terminal transaction permanently fail its
	// own closed-calls invariant. Cancel the Tool Call in this same transaction;
	// trg_tool_calls_close_browser_child is the sole child-state writer.
	if _, err = conn.ExecContext(ctx, `UPDATE tool_calls
		SET status='cancelled',ended_at=?,error_detail='parent terminal pending browser cleanup',row_version=row_version+1
		WHERE status='running' AND execution_mode='quoin_browser' AND id IN (
			SELECT b.tool_call_id FROM browser_exploration_child_bindings b
			JOIN browser_operations o ON o.id=b.operation_id
			JOIN pending_attempt_terminals p ON p.attempt_id=b.parent_attempt_id
			WHERE o.kind='exploration' AND o.state='Cancelled'
			  AND o.terminal_reason='parent_terminal' AND o.ended_at=?
		)`, now, now); err != nil {
		return
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return
	}
	committed = true
}

func (service *RuntimeService) reconcileTerminalParentExplorations(ctx context.Context) {
	if service.Analyses == nil {
		return
	}
	rows, err := service.Analyses.DB().QueryContext(ctx, `SELECT DISTINCT o.owner_attempt_id
		FROM browser_operations o
		JOIN execution_attempts parent ON parent.id=o.owner_attempt_id
		WHERE o.kind='exploration' AND o.state IN ('Starting','Running')
		  AND (parent.state IN ('Succeeded','Failed','Cancelled','Interrupted')
		       OR EXISTS(SELECT 1 FROM pending_attempt_terminals p WHERE p.attempt_id=parent.id))`)
	if err != nil {
		return
	}
	var parentIDs []int64
	for rows.Next() {
		var parentID int64
		if rows.Scan(&parentID) == nil {
			parentIDs = append(parentIDs, parentID)
		}
	}
	if rows.Close() != nil {
		return
	}
	for _, parentID := range parentIDs {
		service.closeTerminalParentExplorations(ctx, parentID)
	}
	service.reconcilePendingAttemptTerminals(ctx)
}

// closeTerminalParentExplorations releases an Exploration after every durable
// parent terminal state. It covers both the idle interval and a child that was
// durably queued/running when a natural failure, interruption, or cancellation
// won elsewhere. It never writes the parent ledger; the terminal parent fact was
// committed before this best-effort typed close dispatch.
func (service *RuntimeService) closeTerminalParentExplorations(ctx context.Context, parentID int64) {
	if service.Analyses == nil || service.Slots == nil || parentID < 1 {
		return
	}
	view, err := service.Slots.View(ctx, qruntime.SlotLintel)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return
	}
	rows, err := service.Analyses.DB().QueryContext(ctx, `SELECT o.id,o.state,
		COALESCE((SELECT b.child_attempt_id FROM browser_exploration_child_bindings b
			JOIN execution_attempts child ON child.id=b.child_attempt_id
			WHERE b.operation_id=o.id AND child.state IN ('Queued','Assigned','Running','Cancelling')
			ORDER BY b.child_attempt_id DESC LIMIT 1),0),
		COALESCE((SELECT b.tool_call_id FROM browser_exploration_child_bindings b
			JOIN execution_attempts child ON child.id=b.child_attempt_id
			WHERE b.operation_id=o.id AND child.state IN ('Queued','Assigned','Running','Cancelling')
			ORDER BY b.child_attempt_id DESC LIMIT 1),0),
		EXISTS(SELECT 1 FROM pending_attempt_terminals p WHERE p.attempt_id=parent.id),
		COALESCE((SELECT claim.state FROM browser_exploration_terminal_claims claim
			JOIN browser_exploration_child_bindings binding ON binding.child_attempt_id=claim.child_attempt_id
			WHERE binding.operation_id=o.id AND claim.state IN ('claimed_complete','artifact_committed_complete','committed_complete')
			ORDER BY claim.child_attempt_id DESC LIMIT 1),'')
		FROM browser_operations o
		JOIN execution_attempts parent ON parent.id=o.owner_attempt_id
		WHERE o.owner_attempt_id=? AND o.kind='exploration'
		  AND o.state IN ('Starting','Running')
		  AND (parent.state IN ('Succeeded','Failed','Cancelled','Interrupted')
		       OR EXISTS(SELECT 1 FROM pending_attempt_terminals p WHERE p.attempt_id=parent.id))`, parentID)
	if err != nil {
		return
	}
	type closure struct {
		operationID, childID, toolCallID int64
		state                            string
		pending                          bool
	}
	var closures []closure
	for rows.Next() {
		var item closure
		var pending int
		var claimState string
		if rows.Scan(&item.operationID, &item.state, &item.childID, &item.toolCallID, &pending, &claimState) == nil {
			item.pending = pending != 0
			// A normal close that already won terminal arbitration owns the
			// immutable complete trace. The parent natural result waits for it;
			// it must not cancel the child that will make the pending row drain.
			if !(item.pending && claimState != "") {
				closures = append(closures, item)
			}
		}
	}
	if rows.Close() != nil {
		return
	}
	for _, item := range closures {
		// A pending natural terminal must turn an active child into Cancelling
		// before Lintel receives its typed action cancel. This materializes the
		// durable child ownership first; retries cannot fabricate an ownerless
		// terminal result. Terminal parents already own this transition.
		if item.pending && item.toolCallID != 0 {
			_, _ = service.Analyses.DB().ExecContext(ctx, `UPDATE tool_calls
				SET status='cancelled',ended_at=?,error_detail='parent terminal pending browser cleanup',row_version=row_version+1
				WHERE id=? AND status='running'`, time.Now().UTC().Format(time.RFC3339Nano), item.toolCallID)
		}
		_ = service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{
			BootId: view.BootID, ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(item.operationID),
			Msg: &runtimev1.ControlEnvelope_CancelBrowserExplorationAction{CancelBrowserExplorationAction: &runtimev1.CancelBrowserExplorationAction{
				OperationId: item.operationID, ParentAttemptId: parentID, ChildAttemptId: item.childID, ToolCallId: item.toolCallID,
			}},
		})
		if item.state == "Starting" {
			// Start is an unknown outcome: install Stop after the durable cancel
			// materialization even if Lintel never sent StartAck.
			_ = service.dispatchBrowserStop(context.Background(), item.operationID)
		}
	}
}
