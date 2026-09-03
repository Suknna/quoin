package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/browser"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"github.com/Suknna/quoin/internal/quoin/verification/deployment"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dispatchBrowserOperation persists the physical-start unknown-outcome fence
// before writing StartBrowserOperation. Send failure leaves Starting intact;
// it must be reconciled rather than reissued as a fresh Queued operation.
func (service *RuntimeService) dispatchBrowserOperation(ctx context.Context, operationID int64) error {
	if service.Browsers == nil {
		return fmt.Errorf("browser authority unavailable")
	}
	view, err := service.Slots.View(ctx, qruntime.SlotLintel)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return qruntime.ErrNotConnected
	}
	capacity, err := service.Slots.BrowserCapacity(qruntime.SlotLintel, view.BootID, *view.ConnectionEpoch)
	if err != nil {
		return err
	}
	input, err := service.Browsers.PrepareDispatchWithCapacity(ctx, operationID, view.BootID, *view.ConnectionEpoch, capacity)
	if err != nil {
		return err
	}
	// A Journey browser operation may not physically start while its owning
	// attempt is still Queued: CancelFenceOn would correctly close Queued work
	// locally, but Chromium would then be an unowned live process. Bind the child
	// before sending Start so a concurrent cancellation becomes Cancelling and is
	// replayed to Lintel if either control frame crosses in flight.
	if input.Kind == "journey" {
		var attemptID int64
		if err := service.Connections.DB().QueryRowContext(ctx, `SELECT owner_attempt_id FROM browser_operations WHERE id=? AND kind='journey'`, operationID).Scan(&attemptID); err != nil {
			return err
		}
		attemptView, err := service.attemptsService().Get(ctx, attemptID)
		if err != nil {
			return err
		}
		if attemptView.State == "Queued" {
			if err := service.attemptsService().BindToSlot(ctx, attemptID, qruntime.SlotLintel, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease); err != nil {
				return err
			}
		} else if attemptView.State != "Assigned" || attemptView.RuntimeSlot == nil || *attemptView.RuntimeSlot != qruntime.SlotLintel || attemptView.BootID == nil || *attemptView.BootID != view.BootID {
			return fmt.Errorf("journey attempt %d is not start-replayable", attemptID)
		}
	}
	requested, err := time.Parse(time.RFC3339Nano, input.RequestedAt)
	if err != nil {
		return err
	}
	kind := runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_UNSPECIFIED
	switch input.Kind {
	case "manual_login":
		kind = runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN
	case "authentication_probe":
		kind = runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_AUTHENTICATION_PROBE
	case "exploration":
		kind = runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION
	case "journey":
		kind = runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY
	case "deployment_verification":
		kind = runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_DEPLOYMENT_VERIFICATION
	default:
		return browser.ErrInvalid
	}
	digest := sha256.Sum256(input.CanonicalJSON)
	return service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: input.BootID, ConnectionEpoch: input.Epoch, CorrelationId: uint64(input.OperationID), Msg: &runtimev1.ControlEnvelope_StartBrowserOperation{StartBrowserOperation: &runtimev1.StartBrowserOperation{OperationId: input.OperationID, Kind: kind, IdentityId: input.IdentityID, IdentityRevisionId: input.RevisionID, ProfileGenerationId: input.ProfileGenerationID, Input: &runtimev1.BrowserOperationInput{SchemaKind: browserOperationSchemaKind(input.Kind), CanonicalJson: input.CanonicalJSON, ContentDigest: digest[:]}, RequestedAt: timestamppb.New(requested), JourneyCatalogDigest: input.CatalogDigest, JourneyCatalogVersion: input.CatalogVersion, VerificationInvocationItemId: input.VerificationInvocationItemID, CloneIdentity: input.CloneIdentity}}})
}

// reconcileLintelPhysicalOperations compares Lintel's boot-scoped physical
// snapshot with durable operation ownership. A reported operation that Quoin has
// already terminalized must receive Stop again; a missing durable active entry is
// not guessed terminal because a Start frame may still be in flight. This keeps
// the unknown-outcome Start fence intact while ensuring capacity reports cannot
// resurrect an ownerless Chromium process.
func (service *RuntimeService) reconcileLintelPhysicalOperations(ctx context.Context, bootID string, epoch uint64, reported []int64) {
	if service.Browsers == nil || bootID == "" || epoch == 0 {
		return
	}
	seen := make(map[int64]struct{}, len(reported))
	for _, id := range reported {
		if id > 0 {
			seen[id] = struct{}{}
		}
	}
	for id := range seen {
		var state string
		var storedBoot string
		var storedEpoch uint64
		err := service.Browsers.DB().QueryRowContext(ctx, `SELECT state,COALESCE(lintel_boot_id,''),COALESCE(lintel_connection_epoch,0) FROM browser_operations WHERE id=?`, id).Scan(&state, &storedBoot, &storedEpoch)
		if err != nil || storedBoot != bootID || storedEpoch > epoch {
			// A runtime-only process has no durable owner. The typed Stop tombstone
			// is idempotent and is the only safe reconciliation action.
			_ = service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: bootID, ConnectionEpoch: epoch, CorrelationId: uint64(id), Msg: &runtimev1.ControlEnvelope_StopBrowserOperation{StopBrowserOperation: &runtimev1.StopBrowserOperation{OperationId: id, Reason: runtimev1.BrowserCloseReason_BROWSER_CLOSE_REASON_OPERATION_TERMINAL, CommittedAt: timestamppb.Now()}}})
			continue
		}
		if state == "Succeeded" || state == "Failed" || state == "Cancelled" || state == "Interrupted" {
			_ = service.dispatchBrowserStop(ctx, id)
		}
	}
	// A Hello/Heartbeat is a complete physical snapshot in both directions.
	// Converge durable active work missing from Lintel before using the same
	// capacity projection to refill FIFO; otherwise stale Running rows consume
	// slots forever and leave both identities and pending parent terminals stuck.
	if missing, err := service.Browsers.InterruptMissingPhysicalOperations(ctx, bootID, epoch, reported); err == nil && len(missing) != 0 {
		service.replayUndeliveredBrowserToolResults(ctx)
		service.reconcilePendingAttemptTerminals(ctx)
	}
	// Only Quoin dispatches work. The full snapshot merely frees capacity that a
	// physical process-loss closure released; it never lets Lintel select work.
	service.dispatchQueuedBrowserOperations(ctx)
}

func browserOperationSchemaKind(kind string) string {
	switch kind {
	case "manual_login":
		return "manual_login_v1"
	case "authentication_probe":
		return "authentication_probe_v1"
	case "exploration":
		return "exploration_v1"
	case "journey":
		return "inspection_collection_v1"
	case "deployment_verification":
		return "deployment_verification_v1"
	default:
		return ""
	}
}

func startRejectReason(reason runtimev1.BrowserOperationStartRejectReason) string {
	switch reason {
	case runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_NO_CAPACITY:
		return "no_capacity"
	case runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_IDENTITY_BUSY:
		return "identity_busy"
	case runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_PROFILE_UNAVAILABLE:
		return "profile_unavailable"
	case runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_AUTHENTICATION_REQUIRED:
		return "authentication_required"
	case runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED:
		return "input_unsupported"
	case runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_RECONCILE_REQUIRED:
		return "reconcile_required"
	case runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_STALE_STREAM:
		return "stale_stream"
	case runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_DOWNLOAD_BLOCKED:
		return "download_blocked"
	default:
		return "internal"
	}
}

func (service *RuntimeService) dispatchBrowserPublish(ctx context.Context, request browser.PublishRequest) error {
	view, err := service.Slots.View(ctx, qruntime.SlotLintel)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return qruntime.ErrNotConnected
	}
	return service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: view.BootID, ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(request.OperationID), Msg: &runtimev1.ControlEnvelope_PublishBrowserProfile{PublishBrowserProfile: &runtimev1.PublishBrowserProfile{OperationId: request.OperationID, IdentityId: request.IdentityID, IdentityRevisionId: request.RevisionID, ExpectedCurrentGenerationId: request.ExpectedGenerationID, NewGeneration: request.NewGeneration, CommandId: request.CommandID}}})
}

func (service *RuntimeService) dispatchBrowserStop(ctx context.Context, operationID int64) error {
	if service.Browsers == nil {
		return fmt.Errorf("browser authority unavailable")
	}
	view, err := service.Slots.View(ctx, qruntime.SlotLintel)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return qruntime.ErrNotConnected
	}
	request, err := service.Browsers.PrepareStopForBoot(ctx, operationID, view.BootID, *view.ConnectionEpoch)
	if err != nil {
		return err
	}
	return service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: request.BootID, ConnectionEpoch: request.Epoch, CorrelationId: uint64(operationID), Msg: &runtimev1.ControlEnvelope_StopBrowserOperation{StopBrowserOperation: &runtimev1.StopBrowserOperation{OperationId: operationID, Reason: runtimev1.BrowserCloseReason_BROWSER_CLOSE_REASON_OPERATION_TERMINAL, CommittedAt: timestamppb.Now()}}})
}

func (service *RuntimeService) handleBrowserStopAck(ctx context.Context, envelope *runtimev1.ControlEnvelope, ack *runtimev1.StopBrowserOperationAck) {
	if service.Browsers == nil || ack == nil || ack.GetStoppedAt() == nil || !ack.GetStoppedAt().IsValid() {
		return
	}
	clean := ack.GetCleanupOutcome() == runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_SUCCEEDED && ack.GetProcessStopped() && ack.GetTunnelClosed() && ack.GetTraceStagingDeleted() && ack.GetTemporaryProfileDeleted() && ack.GetFailureCode() == runtimev1.BrowserCleanupFailureCode_BROWSER_CLEANUP_FAILURE_CODE_UNSPECIFIED
	// Deployment verification owns its stop fence: the typed same-boot
	// cleanup acknowledgment (basis + hash + coupled result) is one
	// transaction in the verification service, not the generic stop path.
	if service.isDeploymentVerificationOperation(ctx, ack.GetOperationId()) && ack.GetCloneIdentity() != "" {
		stopErr := service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
			return service.Browsers.HandleDeploymentStopAck(ctx, ack.GetOperationId(), envelope.GetBootId(), envelope.GetConnectionEpoch(),
				clean, ack.GetStoppedAt().AsTime(), ack.GetCleanupStateHash(), ack.GetOriginalStartBootId(), ack.GetCleanupBootId(),
				ack.GetCleanupConnectionEpoch(), ack.GetStopFenceDigest(), ack.GetCloneIdentity(), deploymentCleanupCounts(ack))
		})
		if stopErr != nil {
			return
		}
		service.afterBrowserStopConvergence(ctx, ack.GetOperationId())
		return
	}
	stopErr := service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
		return service.Browsers.HandleStopAck(ctx, ack.GetOperationId(), envelope.GetBootId(), envelope.GetConnectionEpoch(), clean, ack.GetStoppedAt().AsTime(), ack.GetCleanupStateHash())
	})
	if stopErr == nil {
		// A parent cancellation waits through terminal cleanup so a delayed Start
		// never outlives its only owner. A successful normal close is already
		// terminally committed and must not invoke cancellation convergence: doing
		// so produces a spurious "attempt is not Cancelling" error after a valid
		// close. Re-read the parent state in this StopAck transaction boundary.
		var parentID int64
		var parentState string
		if service.Browsers.DB().QueryRowContext(ctx, `SELECT parent.id,parent.state
			FROM browser_operations operation
			JOIN execution_attempts parent ON parent.id=operation.owner_attempt_id
			WHERE operation.id=? AND operation.kind='exploration'`, ack.GetOperationId()).Scan(&parentID, &parentState) == nil && parentID > 0 && parentState == "Cancelling" {
			go service.finalizeCancellation(context.Background(), parentID, "investigation")
		}
		go service.reconcilePendingAttemptTerminals(context.Background())
		go service.reconcileJourneyVerificationChildren(context.Background())
		go service.dispatchQueuedBrowserOperations(context.Background())
	}
}

func (service *RuntimeService) handleBrowserCompletion(ctx context.Context, envelope *runtimev1.ControlEnvelope, result *runtimev1.CompleteBrowserOperation) {
	if service.Browsers == nil || result == nil || result.GetEndedAt() == nil || !result.GetEndedAt().IsValid() {
		return
	}
	// Exploration actions use CompleteBrowserOperation only for a terminal
	// Runtime-side event (not the normal ActionResult close path). They need to
	// close their parent Tool Call atomically so the frozen trigger closes the
	// child Attempt; Browser.Service handles the other operation kinds.
	if service.isExplorationOperation(ctx, result.GetOperationId()) {
		service.handleExplorationCompletion(ctx, envelope, result)
		return
	}
	probes := make([]browser.ProbeResult, 0, len(result.GetProbeResults()))
	for _, observed := range result.GetProbeResults() {
		if observed == nil || observed.GetObservedAt() == nil || !observed.GetObservedAt().IsValid() {
			return
		}
		phase := ""
		switch observed.GetPhase() {
		case runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_REVISION_CHANGE:
			phase = "revision_change"
		default:
			return
		}
		outcome := ""
		switch observed.GetResult() {
		case runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED:
			outcome = "Authenticated"
		case runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_UNAUTHENTICATED:
			outcome = "Unauthenticated"
		case runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE:
			outcome = "Indeterminate"
		default:
			return
		}
		var reason *string
		if observed.GetReasonCode() != "" {
			value := observed.GetReasonCode()
			reason = &value
		}
		probes = append(probes, browser.ProbeResult{Phase: phase, Result: outcome, JourneyID: observed.GetJourneyId(), JourneyVersion: int64(observed.GetJourneyVersion()), CatalogDigest: observed.GetJourneyCatalogDigest(), CatalogVersion: observed.GetJourneyCatalogVersion(), ReasonCode: reason, ObservedAt: observed.GetObservedAt().AsTime().UTC().Format(time.RFC3339Nano)})
	}
	outcome, terminal := "", ""
	switch result.GetOutcome() {
	case runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_SUCCEEDED:
		outcome = "Succeeded"
	case runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED:
		outcome = "Failed"
	default:
		return
	}
	switch result.GetTerminalReason() {
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_UNSPECIFIED:
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_REQUIRED:
		terminal = "authentication_required"
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE:
		terminal = "authentication_probe_unavailable"
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_PROTOCOL_ERROR:
		terminal = "protocol_error"
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED:
		terminal = "browser_crashed"
	default:
		return
	}
	err := service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
		return service.Browsers.HandleCompletion(ctx, browser.Completion{ID: result.GetOperationId(), BootID: envelope.GetBootId(), Epoch: envelope.GetConnectionEpoch(), Outcome: outcome, TerminalReason: terminal, Digest: result.GetResultDigest(), EndedAt: result.GetEndedAt().AsTime(), Probes: probes})
	})
	ack := &runtimev1.CompleteBrowserOperationAck{OperationId: result.GetOperationId(), ResultDigest: result.GetResultDigest(), Accepted: err == nil}
	if err != nil {
		ack.Detail = "browser completion was not accepted"
	}
	_ = service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: envelope.GetBootId(), ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetMessageId(), Msg: &runtimev1.ControlEnvelope_CompleteBrowserOperationAck{CompleteBrowserOperationAck: ack}})
	if err == nil {
		if service.isDeploymentVerificationOperation(ctx, result.GetOperationId()) {
			service.recordDeploymentVerificationCompletion(ctx, envelope, result)
		}
		go func() { _ = service.dispatchBrowserStop(context.Background(), result.GetOperationId()) }()
	}
}

func (service *RuntimeService) handleBrowserPublishResult(ctx context.Context, envelope *runtimev1.ControlEnvelope, result *runtimev1.PublishBrowserProfileResult) {
	if service.Browsers == nil || result == nil {
		return
	}
	probe := result.GetProbeResult()
	if !result.GetAccepted() {
		// An ordinary unauthenticated publish probe keeps the manual-login
		// operation Running so the same operator can complete login and retry.
		// It is not a terminal authentication fact or a profile publication.
		if result.GetRejectReason() == runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_REQUIRED {
			if probe != nil && probe.GetObservedAt() != nil && probe.GetObservedAt().IsValid() && probe.GetResult() == runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_UNAUTHENTICATED {
				_ = service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
					return service.Browsers.HandlePublishUnauthenticated(ctx, result.GetOperationId(), result.GetCommandId(), envelope.GetBootId(), envelope.GetConnectionEpoch(), browser.ProbeResult{Phase: "publish", Result: "Unauthenticated", JourneyID: probe.GetJourneyId(), JourneyVersion: int64(probe.GetJourneyVersion()), CatalogDigest: probe.GetJourneyCatalogDigest(), CatalogVersion: probe.GetJourneyCatalogVersion(), ObservedAt: probe.GetObservedAt().AsTime().UTC().Format(time.RFC3339Nano)})
				})
			}
			return
		}
		if service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
			return service.Browsers.HandlePublishRejected(ctx, result.GetOperationId(), result.GetCommandId(), envelope.GetBootId(), envelope.GetConnectionEpoch(), result.GetRejectReason() == runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE)
		}) == nil {
			go func() { _ = service.dispatchBrowserStop(context.Background(), result.GetOperationId()) }()
		}
		return
	}
	if probe == nil || probe.GetPhase() != runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_PUBLISH || probe.GetResult() != runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED || probe.GetObservedAt() == nil || !probe.GetObservedAt().IsValid() {
		return
	}
	err := service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
		return service.Browsers.HandlePublishResult(ctx, browser.PublishResult{OperationID: result.GetOperationId(), CommandID: result.GetCommandId(), Generation: result.GetGeneration(), ChromiumRevision: result.GetChromiumRevision(), ManifestDigest: result.GetProfileManifestDigest(), Accepted: result.GetAccepted(), BootID: envelope.GetBootId(), Epoch: envelope.GetConnectionEpoch(), Probe: browser.ProbeResult{Phase: "publish", Result: "Authenticated", JourneyID: probe.GetJourneyId(), JourneyVersion: int64(probe.GetJourneyVersion()), CatalogDigest: probe.GetJourneyCatalogDigest(), CatalogVersion: probe.GetJourneyCatalogVersion(), ObservedAt: probe.GetObservedAt().AsTime().UTC().Format(time.RFC3339Nano)}})
	})
	if err == nil {
		go func() { _ = service.dispatchBrowserStop(context.Background(), result.GetOperationId()) }()
	}
}

func (service *RuntimeService) handleBrowserStartAck(ctx context.Context, envelope *runtimev1.ControlEnvelope, ack *runtimev1.StartBrowserOperationAck) {
	if service.Browsers == nil || ack == nil {
		return
	}
	started := time.Time{}
	if ack.GetStartedAt() != nil && ack.GetStartedAt().IsValid() {
		started = ack.GetStartedAt().AsTime()
	}
	rejectReason := startRejectReason(ack.GetRejectReason())
	err := service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
		return service.Browsers.HandleStartAck(ctx, ack.GetOperationId(), envelope.GetBootId(), envelope.GetConnectionEpoch(), ack.GetAccepted(), rejectReason, started)
	})
	if err == nil && ack.GetAccepted() {
		go service.dispatchPendingExplorationAction(context.Background(), ack.GetOperationId())
		go service.dispatchReadyJourneyAttempts(context.Background())
	} else if err == nil && rejectReason != "no_capacity" {
		// NO_CAPACITY is an explicit non-start: Browser.Service keeps the Operation
		// in WaitingForCapacity at its original FIFO position. It is not a model
		// result and must not close the queued browser child.
		// Start rejection is a terminal physical admission observation. For an
		// Exploration its queued browser child must be closed through the parent
		// Tool Call, otherwise the model waits forever for an action that Lintel
		// never accepted. This is deliberately a schema-valid Tool result, not a
		// transport rejection that loses the model continuation.
		go service.closeRejectedExplorationStart(context.Background(), ack.GetOperationId(), rejectReason, ack.GetDetail())
	}
	if err == nil {
		// A StartAck is a durable physical-admission observation. It can be the
		// missing edge after a natural terminal/cancel won while Start was in
		// flight, including NO_CAPACITY which proves no process exists.
		go service.reconcileTerminalParentExplorations(context.Background())
		go service.reconcilePendingAttemptTerminals(context.Background())
		go service.dispatchAllCancellingBrowserExplorations(context.Background())
		go service.reconcileJourneyVerificationChildren(context.Background())
	}
	// NO_CAPACITY keeps its original FIFO position unless its parent already
	// terminalized; reconciliation above then closes it with the no_capacity
	// cleanup basis rather than leaving a pending parent indefinitely.
}

// dispatchPendingExplorationAction finds the first queued child of a Running
// Exploration after the Start ack made the operation eligible for an action.
func (service *RuntimeService) dispatchPendingExplorationAction(ctx context.Context, operationID int64) {
	if service.Analyses == nil {
		return
	}
	var childID int64
	err := service.Analyses.DB().QueryRowContext(ctx, `SELECT b.child_attempt_id FROM browser_exploration_child_bindings b
		JOIN execution_attempts c ON c.id=b.child_attempt_id
		JOIN browser_operations o ON o.id=b.operation_id
		WHERE b.operation_id=? AND o.kind='exploration' AND o.state='Running' AND c.state='Queued' ORDER BY c.id LIMIT 1`, operationID).Scan(&childID)
	if err == nil {
		_ = service.dispatchBrowserExplorationAction(ctx, childID)
	}
}

func (service *RuntimeService) dispatchPendingBrowserStops(ctx context.Context) {
	if service.Browsers == nil {
		return
	}
	var id int64
	if err := service.Browsers.DB().QueryRowContext(ctx, `SELECT id FROM browser_operations WHERE state IN ('Succeeded','Failed','Cancelled','Interrupted') AND start_dispatched_at IS NOT NULL AND stop_confirmed_at IS NULL ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		return
	}
	// Acknowledgement is asynchronous; do not spin duplicate Stop frames.
	_ = service.dispatchBrowserStop(ctx, id)
}

func (service *RuntimeService) dispatchQueuedBrowserOperations(ctx context.Context) {
	if service.Browsers == nil {
		return
	}
	// A durable Starting row already owns an unknown-outcome Start fence. Replay
	// at most that one first; then atomically claim every FIFO head for which the
	// browser service still observes physical capacity. PrepareDispatch's
	// transaction counts each newly Starting row, so this fills capacity without
	// an unbounded stale-projection burst.
	var starting int64
	if err := service.Browsers.DB().QueryRowContext(ctx, `SELECT id FROM browser_operations WHERE state='Starting' ORDER BY id LIMIT 1`).Scan(&starting); err == nil {
		_ = service.dispatchBrowserOperation(ctx, starting)
	}
	for {
		var id int64
		if err := service.Browsers.DB().QueryRowContext(ctx, `SELECT id FROM browser_operations WHERE state IN ('Queued','WaitingForCapacity') ORDER BY id LIMIT 1`).Scan(&id); err != nil {
			return
		}
		if err := service.dispatchBrowserOperation(ctx, id); err != nil {
			// ErrCapacityUnavailable leaves the head durable in WaitingForCapacity;
			// any other rejection is likewise handled by its existing state machine.
			return
		}
	}
}

// isDeploymentVerificationOperation reports whether the operation owns a
// Deployment Acceptance manifest item.
func (service *RuntimeService) isDeploymentVerificationOperation(ctx context.Context, operationID int64) bool {
	if service.Verifications == nil || service.Browsers == nil || operationID == 0 {
		return false
	}
	var itemID int64
	err := service.Browsers.DB().QueryRowContext(ctx, `SELECT verification_manifest_item_id FROM browser_operations WHERE id=? AND kind='deployment_verification'`, operationID).Scan(&itemID)
	return err == nil && itemID > 0
}

// recordDeploymentVerificationCompletion persists the functional side of a
// deployment verification completion after the generic terminalization.
func (service *RuntimeService) recordDeploymentVerificationCompletion(ctx context.Context, envelope *runtimev1.ControlEnvelope, result *runtimev1.CompleteBrowserOperation) {
	if service.Verifications == nil {
		return
	}
	outcome := ""
	switch result.GetOutcome() {
	case runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_SUCCEEDED:
		outcome = "Succeeded"
	case runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED:
		outcome = "Failed"
	default:
		return
	}
	terminal := ""
	switch result.GetTerminalReason() {
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_REQUIRED:
		terminal = "authentication_required"
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED:
		terminal = "browser_crashed"
	case runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_PROTOCOL_ERROR:
		terminal = "protocol_error"
	}
	probe := ""
	for _, observed := range result.GetProbeResults() {
		if observed == nil || observed.GetObservedAt() == nil || !observed.GetObservedAt().IsValid() {
			continue
		}
		switch observed.GetResult() {
		case runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED:
			probe = "Authenticated"
		case runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_UNAUTHENTICATED:
			probe = "Unauthenticated"
		case runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE:
			probe = "Indeterminate"
		}
	}
	_ = service.Verifications.HandleBrowserCompletion(ctx, deployment.BrowserCompletion{
		OperationID: result.GetOperationId(), Outcome: outcome, TerminalReason: terminal,
		ResultDigest: result.GetResultDigest(), EndedAt: result.GetEndedAt().AsTime(),
		ProbeResult: probe, ProbeObservedAt: probeObservedAt(result),
	})
}

// deploymentCleanupCounts projects the typed resource-zero counts.
func deploymentCleanupCounts(ack *runtimev1.StopBrowserOperationAck) [9]uint64 {
	return [9]uint64{
		ack.GetOperationProcessCount(), ack.GetCgroupProcessCount(), ack.GetChromiumProcessCount(),
		ack.GetX0VncProcessCount(), ack.GetNovncTunnelCount(), ack.GetCloneNamespaceCount(),
		ack.GetTemporaryFileCount(), ack.GetRuntimeHandleCount(), ack.GetSlotLeaseCount(),
	}
}

// afterBrowserStopConvergence runs the shared post-stop reconciliation.
func (service *RuntimeService) afterBrowserStopConvergence(ctx context.Context, operationID int64) {
	go service.reconcilePendingAttemptTerminals(context.Background())
	go service.dispatchQueuedBrowserOperations(context.Background())
}

func probeObservedAt(result *runtimev1.CompleteBrowserOperation) string {
	for _, observed := range result.GetProbeResults() {
		if observed.GetObservedAt() != nil && observed.GetObservedAt().IsValid() {
			return observed.GetObservedAt().AsTime().UTC().Format(time.RFC3339Nano)
		}
	}
	return result.GetEndedAt().AsTime().UTC().Format(time.RFC3339Nano)
}
