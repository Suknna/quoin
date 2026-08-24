package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/browser"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
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
	input, err := service.Browsers.PrepareDispatch(ctx, operationID, view.BootID, *view.ConnectionEpoch)
	if err != nil {
		return err
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
	default:
		return browser.ErrInvalid
	}
	digest := sha256.Sum256(input.CanonicalJSON)
	return service.sendEnvelope(qruntime.SlotLintel, &runtimev1.ControlEnvelope{BootId: input.BootID, ConnectionEpoch: input.Epoch, CorrelationId: uint64(input.OperationID), Msg: &runtimev1.ControlEnvelope_StartBrowserOperation{StartBrowserOperation: &runtimev1.StartBrowserOperation{OperationId: input.OperationID, Kind: kind, IdentityId: input.IdentityID, IdentityRevisionId: input.RevisionID, ProfileGenerationId: input.ProfileGenerationID, Input: &runtimev1.BrowserOperationInput{SchemaKind: map[bool]string{true: "manual_login_v1", false: "authentication_probe_v1"}[input.Kind == "manual_login"], CanonicalJson: input.CanonicalJSON, ContentDigest: digest[:]}, RequestedAt: timestamppb.New(requested), JourneyCatalogDigest: input.CatalogDigest, JourneyCatalogVersion: input.CatalogVersion}}})
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
	if service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
		return service.Browsers.HandleStopAck(ctx, ack.GetOperationId(), envelope.GetBootId(), envelope.GetConnectionEpoch(), clean, ack.GetStoppedAt().AsTime(), ack.GetCleanupStateHash())
	}) == nil {
		go service.dispatchQueuedBrowserOperations(context.Background())
	}
}

func (service *RuntimeService) handleBrowserCompletion(ctx context.Context, envelope *runtimev1.ControlEnvelope, result *runtimev1.CompleteBrowserOperation) {
	if service.Browsers == nil || result == nil || result.GetEndedAt() == nil || !result.GetEndedAt().IsValid() {
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
	_ = service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
		return service.Browsers.HandleStartAck(ctx, ack.GetOperationId(), envelope.GetBootId(), envelope.GetConnectionEpoch(), ack.GetAccepted(), startRejectReason(ack.GetRejectReason()), started)
	})
	// NO_CAPACITY keeps its original FIFO position. It is retried only after a
	// later physical-stop acknowledgement (or the next Lintel attachment), not
	// recursively from this stale capacity response.
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
	// Start acknowledgement is asynchronous. Dispatch one FIFO head only, so a
	// stale capacity projection cannot turn an entire queue into an unbounded
	// burst of Starting operations before Lintel has accepted any of them.
	var id int64
	if err := service.Browsers.DB().QueryRowContext(ctx, `SELECT id FROM browser_operations WHERE state IN ('Starting','Queued','WaitingForCapacity') ORDER BY CASE state WHEN 'Starting' THEN 0 ELSE 1 END,id LIMIT 1`).Scan(&id); err != nil {
		return
	}
	_ = service.dispatchBrowserOperation(ctx, id)
}
