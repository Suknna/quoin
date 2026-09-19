package app

// Reconnect reconciliation and loss convergence for the Plinth control
// stream (T12, RUNTIME-TASK-005/006/007, RUNTIME-CANCEL-003): new-boot
// interruption, same-boot reconcile (ReconcileRequest → ReconcileReport),
// heartbeat lease renewal, Cancelling convergence when a stream ends, the
// periodic lease sweeper and the idempotent re-dispatch of Assigned
// attempts the runtime never accepted. Commit order stays with SQLite.
// Recovery mutations (recovery-loss freeze, unstarted exploration closes)
// run through execution.Execute: the audit commits inside the runner
// transaction under the attempt's restored durable scope, failures roll the
// mutation back, and outbound dispatches happen only after the commit.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// reconcileTimeout bounds the wait for ReconcileReport after a same-boot
// reconnect; a silent runtime leaves its attempts to the lease sweeper.
const reconcileTimeout = 10 * time.Second

// Reconcile recovery writes (ADR-0006): the runtime's recovery mutations are
// declared, registered write operations executed through execution.Execute in
// one runner-owned IMMEDIATE transaction. The audit event commits inside the
// same transaction, so a recovery mutation can never commit without its audit
// record and an audit failure rolls the mutation back — no silent partial
// recovery write. Dispatches stay strictly post-commit: a frame is only sent
// after the durable fact exists. The legacy browser drain-only paths keep
// their existing best-effort send semantics (the durable fence is replayed on
// reconnect); this migration changes transaction ownership, not dispatch.

// reconcileObjectAttempt is the audit domain object type of every reconcile
// mutation: the owning execution attempt.
const reconcileObjectAttempt = "execution_attempt"

// Stable audit action identities of the reconcile family.
const opNameRecoveryLossPending = "runtime.recovery_loss_pending"

// reconcileOpSet holds the canonical registered operation pointers.
type reconcileOpSet struct {
	recoveryLossPending *execution.Operation
}

var (
	reconcileOpsOnce        sync.Once
	reconcileRegistryHandle *execution.Registry
	reconcileOps            reconcileOpSet
)

// reconcileOperations declares the reconcile family's write operations once
// per process. Authorization re-verifies inside the runner transaction that
// the acting principal is the runtime's system authority on a task/internal
// channel — an HTTP caller can never drive recovery. Registration failures
// are declaration conflicts and panic like the compose default.
func reconcileOperations() (*execution.Registry, *reconcileOpSet) {
	reconcileOpsOnce.Do(func() {
		registry := execution.NewRegistry()
		register := func(op execution.Operation) *execution.Operation {
			declared, err := registry.Register(op)
			if err != nil {
				panic("runtime: register " + op.Name + ": " + err.Error())
			}
			return declared
		}
		authorize := func(ctx context.Context, _ *execution.Tx) error {
			meta, err := execution.Require(ctx)
			if err != nil {
				return err
			}
			if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
				return errors.New("runtime: reconcile operation requires the system principal")
			}
			if meta.Source.Kind == execution.SourceHTTP {
				return errors.New("runtime: reconcile operation cannot arrive from the http channel")
			}
			return nil
		}
		declare := func(name string) *execution.Operation {
			return register(execution.Operation{Name: name, Class: execution.ClassWrite, ObjectType: reconcileObjectAttempt, Authorize: authorize})
		}
		reconcileRegistryHandle = registry
		reconcileOps = reconcileOpSet{
			recoveryLossPending: declare(opNameRecoveryLossPending),
		}
	})
	return reconcileRegistryHandle, &reconcileOps
}

// reconcileRunner composes the reconcile family's runner over the product
// database with the shared operation registry; a nil writer falls back to the
// default audit writer. The runner is a value object: building it per call
// keeps the helpers composable without shared mutable runner state.
func reconcileRunner(db *sql.DB, writer *audit.Writer) *execution.Runner {
	registry, _ := reconcileOperations()
	return execution.NewRunner(db, registry, writer)
}

// reconcileScopeContext resolves the durable task scope for one attempt's
// recovery mutation: inherited when the caller already carries execution
// metadata, otherwise restored from the attempt row's persisted association
// (attempt.LoadCorrelation, ADR-0006) with the system actor, the inherited
// original initiator and the attempt's operation correlation. A legacy
// correlation-less row establishes a fresh task identity — a deliberate
// recovery operation, never a fabricated link.
func reconcileScopeContext(ctx context.Context, db audit.Reader, attemptID int64) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	correlation, found, err := attempt.LoadCorrelation(ctx, db, attemptID)
	if err != nil {
		return nil, err
	}
	actor := execution.Principal{Kind: execution.PrincipalSystem, ID: 0}
	source := execution.Source{Kind: execution.SourceTask}
	if found && restorableReconcileCorrelation(correlation) {
		return execution.ReplaceMetadata(ctx, execution.Metadata{
			CorrelationID: correlation.OperationCorrelationID,
			Actor:         actor,
			Initiator: execution.Principal{
				Kind: execution.PrincipalKind(correlation.InitiatorType),
				ID:   correlation.InitiatorID,
			},
			Source: source,
		})
	}
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.ReplaceMetadata(ctx, execution.Metadata{CorrelationID: correlationID, Actor: actor, Source: source})
}

// restorableReconcileCorrelation reports whether a persisted attempt
// correlation is complete enough for recovery to reattach (a NULL legacy
// column set is a no-correlation fact that is never guessed).
func restorableReconcileCorrelation(correlation attempt.Correlation) bool {
	if correlation.OperationCorrelationID == "" {
		return false
	}
	switch execution.PrincipalKind(correlation.InitiatorType) {
	case execution.PrincipalSystem:
		return correlation.InitiatorID == 0
	case execution.PrincipalUser, execution.PrincipalService:
		return correlation.InitiatorID > 0
	default:
		return false
	}
}

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
	if view.AttemptType == "knowledge_extraction" && service.Knowledge != nil {
		if err := service.Knowledge.InterruptExtraction(ctx, view.ID, reason); err != nil {
			sharedops.LogEvent("quoin", "error", "knowledge.interrupt_failed", fmt.Sprintf("attempt=%d %v", view.ID, err))
		}
		return
	}
	if view.AttemptType == "investigation" {
		// Create recovery_loss as the durable closure authority, then drain it:
		// with no browser obligations left, the pending terminal commits at once.
		if err := service.freezeRecoveryLossPending(ctx, view.ID, reason); err != nil {
			sharedops.LogEvent("quoin", "error", "reconcile.interrupt_pending_failed", fmt.Sprintf("attempt=%d %v", view.ID, err))
			return
		}
		service.reconcilePendingAttemptTerminals(ctx)
		return
	}
	// A connection_probe has its own immutable typed result closure. During a
	// restart it must append the interrupted child before the terminal Attempt
	// update; generic interruption would violate that database fence.
	if view.AttemptType == "connection_probe" && service.Connections != nil {
		if err := service.Connections.InterruptProbe(ctx, view.ID, reason); err != nil {
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
	if view.AttemptType == "inspection_collection" && view.ScopeType == "observation_run" && service.Observations != nil {
		// The generic interruption terminalized the attempt row; the
		// observation authority projects the honest gap and converges the Run.
		if err := service.Observations.ConvergeInterruptedChild(ctx, view.ID, "interrupted"); err != nil {
			sharedops.LogEvent("quoin", "error", "reconcile.interrupt_failed", fmt.Sprintf("attempt=%d %v", view.ID, err))
		}
		return
	}
	if view.AttemptType == "inspection_collection" && view.ScopeType == "run_check" && service.Inspections != nil {
		if err := service.Inspections.RecordPromQLTechnicalGap(ctx, view.ID, reason); err != nil {
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
// lost Investigation is allowed to become Interrupted. The insert is one
// audited Execute transaction under the attempt's restored durable scope;
// SQLite serialization makes duplicate reconciliation/reconnect scans
// harmless. The committed pending row is immutable
// (trg_pending_attempt_terminals_immutable), so an already-frozen attempt is
// a proven no-op and is skipped silently — the five-second lease sweep would
// otherwise re-audit the same fact on every tick while browser cleanup is
// still pending.
func (service *RuntimeService) freezeRecoveryLossPending(ctx context.Context, attemptID int64, reason string) error {
	if service.Analyses == nil {
		return fmt.Errorf("analysis service unavailable")
	}
	db := service.writer
	var frozen int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_attempt_terminals WHERE attempt_id=? AND source='recovery_loss')`, attemptID).Scan(&frozen); err != nil {
		return err
	}
	if frozen == 1 {
		return nil
	}
	scope, err := reconcileScopeContext(ctx, db, attemptID)
	if err != nil {
		return err
	}
	type frozenTerminal struct {
		AttemptID int64
	}
	_, ops := reconcileOperations()
	_, err = execution.Execute(scope, reconcileRunner(db, nil), ops.recoveryLossPending, func(tx *execution.Tx) (frozenTerminal, error) {
		// The state guard stays inside the writer transaction: only a Running
		// investigation may freeze, and the check and the insert must share one
		// serialization point with a concurrent terminal transition.
		var state, attemptType string
		if err := tx.QueryRowContext(scope, `SELECT state,attempt_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &attemptType); err != nil {
			return frozenTerminal{}, err
		}
		if state != "Running" || attemptType != "investigation" {
			return frozenTerminal{AttemptID: attemptID}, nil
		}
		if _, err := tx.ExecContext(scope, `INSERT INTO pending_attempt_terminals(attempt_id,source,target_state,terminal_reason,created_at)
			VALUES(?,'recovery_loss','Interrupted',?,?) ON CONFLICT(attempt_id) DO NOTHING`, attemptID, reason, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return frozenTerminal{}, err
		}
		return frozenTerminal{AttemptID: attemptID}, nil
	}, func(value frozenTerminal) int64 { return value.AttemptID })
	return err
}

// finalizeCancellation routes one Cancelling convergence to the owning
// scope aggregate (the runtime confirmed the stop, or the stream ended /
// the attempt was lost with the fence already committed).
func (service *RuntimeService) finalizeCancellation(ctx context.Context, attemptID int64, attemptType string) {
	switch attemptType {
	case "inspection_collection":
		if service.Inspections != nil {
			var scopeType string
			_ = service.Inspections.Reader().QueryRowContext(ctx, `SELECT scope_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeType)
			if scopeType == "run_check" {
				if err := service.Inspections.Attempts().CancelAck(ctx, attemptID); err != nil {
					sharedops.LogEvent("quoin", "error", "inspection.cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, err))
				}
				return
			}
			if scopeType == "observation_run" && service.Observations != nil {
				if err := service.Observations.Attempts().CancelAck(ctx, attemptID); err != nil {
					sharedops.LogEvent("quoin", "error", "source_observation.cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, err))
					return
				}
				if err := service.Observations.ConvergeInterruptedChild(ctx, attemptID, "cancelled"); err != nil {
					sharedops.LogEvent("quoin", "error", "source_observation.cancel_converge", fmt.Sprintf("attempt=%d %v", attemptID, err))
				}
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
			if view.State == "Assigned" && service.Knowledge != nil && view.AttemptType == "knowledge_extraction" {
				// Accept can be lost while the Runtime executes the frozen import.
				if err := service.Knowledge.Attempts().Accept(ctx, view.ID, bootID, 0); err != nil {
					sharedops.LogEvent("quoin", "error", "reconcile.accept_restore", fmt.Sprintf("attempt=%d %v", view.ID, err))
				}
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
				// Source observation children belong to the observation
				// authority: its rebuilder is the only one that can re-seal
				// their frozen input, so the accept must flow through the
				// owning service, never the inspection aggregate.
				if view.AttemptType == "inspection_collection" && view.ScopeType == "observation_run" && service.Observations != nil {
					if err := service.Observations.Attempts().Accept(ctx, view.ID, bootID, 0); err != nil {
						sharedops.LogEvent("quoin", "error", "reconcile.accept_restore", fmt.Sprintf("attempt=%d %v", view.ID, err))
					}
					continue
				}
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
			if view.AttemptType == "inspection_collection" && view.ScopeType == "observation_run" {
				err = service.reDispatchSourceObservationAttempt(ctx, view)
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
	attempts := service.attemptsService()
	if (view.AttemptType == "inspection_collection" || view.AttemptType == "inspection_analysis") && service.Inspections != nil {
		attempts = service.Inspections.Attempts()
	}
	if view.AttemptType == "knowledge_extraction" && service.Knowledge != nil {
		attempts = service.Knowledge.Attempts()
	}
	if attempts == nil {
		return fmt.Errorf("attempt service not wired for %s", view.AttemptType)
	}
	input, err := attempts.DispatchInputFor(ctx, view.ID)
	if err != nil {
		return err
	}
	// Re-dispatch echoes the stored association exactly like the first
	// dispatch: the persisted row stays the single correlation authority
	// (ADR-0006). The database follows the attempts-service selection above.
	var correlationDB audit.Reader
	if service.Analyses != nil {
		correlationDB = service.Analyses.Reader()
	}
	if (view.AttemptType == "inspection_collection" || view.AttemptType == "inspection_analysis") && service.Inspections != nil {
		correlationDB = service.Inspections.Reader()
	}
	if view.AttemptType == "knowledge_extraction" && service.Knowledge != nil {
		correlationDB = service.Knowledge.Reader()
	}
	operationCorrelationID, err := dispatchOperationCorrelation(ctx, correlationDB, view.ID)
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
	} else if view.AttemptType == "knowledge_extraction" {
		attemptWire = runtimev1.AttemptType_ATTEMPT_TYPE_KNOWLEDGE_EXTRACTION
		scopeWire = runtimev1.ScopeType_SCOPE_TYPE_KNOWLEDGE_IMPORT_BATCH
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
			AttemptId:              view.ID,
			AttemptType:            attemptWire,
			ScopeType:              scopeWire,
			ScopeId:                view.ScopeID,
			OperationCorrelationId: operationCorrelationID,
			LeaseDeadline:          timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
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
					view, viewErr := attempts.Get(ctx, item.AttemptID)
					if viewErr != nil {
						sharedops.LogEvent("quoin", "error", "reconcile.sweep_candidate", fmt.Sprintf("attempt=%d %v", item.AttemptID, viewErr))
						continue
					}
					if view.State == "Cancelling" {
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
				case "knowledge_extraction":
					if service.Knowledge != nil {
						if err := service.Knowledge.InterruptExtraction(ctx, item.AttemptID, "lease_expired"); err != nil {
							sharedops.LogEvent("quoin", "error", "knowledge_extraction.sweep_closure", fmt.Sprintf("attempt=%d %v", item.AttemptID, err))
						}
					}
				case "inspection_collection":
					var closeErr error
					switch item.ScopeType {
					case "run_check":
						if service.Inspections != nil {
							closeErr = service.Inspections.RecordPromQLTechnicalGap(ctx, item.AttemptID, "interrupted")
						}
					case "observation_run":
						if service.Observations != nil {
							closeErr = service.Observations.ConvergeInterruptedChild(ctx, item.AttemptID, "interrupted")
						}
					default:
						sharedops.LogEvent("quoin", "info", "reconcile.sweep_scope_unhandled", fmt.Sprintf("attempt=%d scope=%s", item.AttemptID, item.ScopeType))
					}
					if closeErr != nil {
						sharedops.LogEvent("quoin", "error", "inspection_collection.sweep_closure", fmt.Sprintf("attempt=%d %v", item.AttemptID, closeErr))
					}
				}
			}
			// Keep the semantic projection converging between external
			// triggers: drift detection, pending batches and settled
			// generation switches all reconcile on the tick.
			service.dispatchQueuedEmbeddings(ctx)
		}
	}
}

// dispatchAllCancellingInspections replays durable Plinth cancellation fences
// after reconnect. Sending is best effort; the fence stays Cancelling and this
// method retries on every attachment until a CancelAck or loss convergence.
func (service *RuntimeService) dispatchAllCancellingInspections(ctx context.Context) {
	if service.Inspections == nil {
		return
	}
	rows, err := service.Inspections.Reader().QueryContext(ctx, `
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

func (service *RuntimeService) reconcilePendingAttemptTerminals(ctx context.Context) {
	if service.Analyses == nil || service.Investigations == nil {
		return
	}
	rows, err := service.Analyses.Reader().QueryContext(ctx, `SELECT p.attempt_id
		FROM pending_attempt_terminals p`)
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

// dispatchAllCancellingKnowledgeExtractions replays cancellation fences after
// Plinth reconnect; extraction attempts use the common routed cancel protocol.
func (service *RuntimeService) dispatchAllCancellingKnowledgeExtractions(ctx context.Context) {
	if service.Knowledge == nil {
		return
	}
	rows, err := service.Knowledge.Reader().QueryContext(ctx, `SELECT id FROM execution_attempts WHERE state='Cancelling' AND attempt_type='knowledge_extraction' ORDER BY id`)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "knowledge.cancel_replay", err.Error())
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			sharedops.LogEvent("quoin", "error", "knowledge.cancel_replay", err.Error())
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		sharedops.LogEvent("quoin", "error", "knowledge.cancel_replay", err.Error())
		return
	}
	_ = rows.Close()
	for _, id := range ids {
		if err := service.dispatchCancelRouted(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "knowledge.cancel_replay", fmt.Sprintf("attempt=%d %v", id, err))
		}
	}
}
