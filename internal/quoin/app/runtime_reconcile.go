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
	var sameBoot []attempt.View
	for _, view := range active {
		if view.BootID == nil || *view.BootID != helloBoot {
			service.finalizeLoss(ctx, view, "lease_expired")
			sharedops.LogEvent("quoin", "info", "reconcile.new_boot_interrupt", fmt.Sprintf("attempt=%d oldBoot=%v", view.ID, view.BootID))
			continue
		}
		sameBoot = append(sameBoot, view)
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

// finalizeCancellation routes one Cancelling convergence to the owning
// scope aggregate (the runtime confirmed the stop, or the stream ended /
// the attempt was lost with the fence already committed).
func (service *RuntimeService) finalizeCancellation(ctx context.Context, attemptID int64, attemptType string) {
	if attemptType == "inspection_collection" && service.BusinessSystems != nil {
		view, err := service.attemptsService().Get(ctx, attemptID)
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
			if view.State == "Assigned" && view.AttemptType == "inspection_collection" && service.BusinessSystems != nil {
				if err := service.BusinessSystems.VerificationAttempts().Accept(ctx, view.ID, bootID, 0); err != nil {
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

// onPlinthStreamEnded converges Cancelling attempts bound to the ended
// stream: the cancellation fence already committed, so the attempt must
// not stay in the Cancelling middle state (RUNTIME-CANCEL-003). Running
// attempts keep their lease window for a same-boot reconnect.
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
		service.finalizeCancellation(ctx, view.ID, view.AttemptType)
		sharedops.LogEvent("quoin", "info", "reconcile.stream_end_cancelled", fmt.Sprintf("attempt=%d", view.ID))
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
