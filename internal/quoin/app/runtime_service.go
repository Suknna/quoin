package app

// RuntimeControl gRPC service (T06): Register (one-time token bootstrap) and
// Connect (Hello handshake + heartbeat maintenance). The service runs on the
// Runtime TLS listener (:8443) next to SteleRelay.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	appinvestigation "github.com/Suknna/quoin/internal/quoin/app/investigation"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/browser"
	"github.com/Suknna/quoin/internal/quoin/businesssystem"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/investigation"
	"github.com/Suknna/quoin/internal/quoin/knowledge"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// RuntimeService adapts the runtime.Service authority to the gRPC surface.
type RuntimeService struct {
	runtimev1.UnimplementedRuntimeControlServer
	Slots          *qruntime.Service
	ReleaseVersion string
	// Connections owns connection_probe attempts and credential grants
	// (T07); nil keeps the T06 handshake-only behaviour for tests that do
	// not exercise the task slice.
	Connections *connections.Service
	// BusinessSystems owns Config Verification PromQL collection attempts.
	BusinessSystems *businesssystem.Service
	// Analyses owns initial-analysis attempts (T10); nil keeps the
	// handshake-only behaviour for tests that do not exercise it.
	Analyses *analysis.Service
	// Investigations owns investigation attempts (T13); nil keeps the
	// handshake-only behaviour for tests that do not exercise it.
	Investigations *investigation.Service
	// Knowledge owns source-material extraction attempts (T28).
	Knowledge *knowledge.Service
	// InvestigationRuntime carries the investigation runtime slice
	// (dispatch, result adjudication, delta fan-out).
	InvestigationRuntime *appinvestigation.RuntimeSlice
	// Artifacts is the Artifact store the ArtifactService adapts (T10).
	Artifacts *artifact.Store
	// Browsers owns durable Browser Identity/Operation authority (T20).
	Browsers *browser.Service
	// Inspections owns manual Inspection Runs: run_check PromQL children and
	// inspection_analysis report attempts (T24); nil keeps the handshake-only
	// behaviour for tests that do not exercise it.
	Inspections *inspection.Service
	// CatalogDigest is the embedded Journey Catalog digest both Quoin and
	// Lintel must agree on (RUNTIME-CTRL-010); empty means no catalog
	// embedded yet, which keeps lintel handshake-rejected with CATALOG_
	// MISMATCH until a release embeds one.
	CatalogDigest string
	// reconcile carries the pending same-boot ReconcileReport waiter
	// (T12, RUNTIME-TASK-005).
	reconcile reconcileState
	// sendEnvelopeForTest captures outbound control replies in package tests.
	// Production leaves it nil and always routes through the live slot.
	sendEnvelopeForTest func(slot string, envelope *runtimev1.ControlEnvelope) error
	// browserExplorationSlotView is a narrow test seam for exercising the
	// frozen SQLite action triggers without opening a live gRPC stream.
	browserExplorationSlotView func(context.Context) (qruntime.SlotView, error)
}

func (service *RuntimeService) slotName(slot runtimev1.RuntimeSlot) string {
	switch slot {
	case runtimev1.RuntimeSlot_RUNTIME_SLOT_PLINTH:
		return qruntime.SlotPlinth
	case runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL:
		return qruntime.SlotLintel
	default:
		return ""
	}
}

func bearerFromContext(ctx context.Context) string {
	data, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, value := range data["authorization"] {
		if strings.HasPrefix(value, "Bearer ") {
			return strings.TrimPrefix(value, "Bearer ")
		}
	}
	return ""
}

func registerStatus(err error) error {
	var registerErr *qruntime.RegisterError
	if errors.As(err, &registerErr) {
		switch registerErr.Status {
		case "INVALID_ARGUMENT":
			return status.Error(codes.InvalidArgument, registerErr.Detail)
		case "UNAUTHENTICATED":
			return status.Error(codes.Unauthenticated, registerErr.Detail)
		case "FAILED_PRECONDITION":
			return status.Error(codes.FailedPrecondition, registerErr.Detail)
		}
	}
	return status.Error(codes.Internal, "registration failed")
}

// Register exchanges a one-time token for a long-term token
// (RUNTIME-REG-002/003).
func (service *RuntimeService) Register(ctx context.Context, request *runtimev1.RegisterRuntimeRequest) (*runtimev1.RegisterRuntimeResponse, error) {
	slot := service.slotName(request.GetSlot())
	if slot == "" {
		return nil, status.Error(codes.InvalidArgument, "slot must be plinth or lintel")
	}
	token, generation, err := service.Slots.Register(ctx, slot, request.GetOneTimeToken(), int64(request.GetGeneration()), request.GetBootId(), request.GetReleaseVersion(), service.ReleaseVersion)
	if err != nil {
		return nil, registerStatus(err)
	}
	sharedops.LogEvent("quoin", "info", "runtime.registered", "slot="+slot)
	return &runtimev1.RegisterRuntimeResponse{
		Slot:          request.GetSlot(),
		Generation:    uint64(generation),
		LongTermToken: token,
	}, nil
}

// Connect authenticates the bearer, adjudicates the Hello handshake and
// keeps the transient connection projection alive. T06 implements the
// handshake/readiness slice of the control stream; task dispatch arrives
// with later tickets, so after Hello the loop only maintains heartbeats
// until the stream ends.
func (service *RuntimeService) Connect(stream runtimev1.RuntimeControl_ConnectServer) error {
	ctx := stream.Context()
	bearer := bearerFromContext(ctx)
	if bearer == "" {
		return status.Error(codes.Unauthenticated, "authorization bearer required")
	}
	// First frame must be Hello (RUNTIME-CTRL-002).
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.Unauthenticated, "hello frame required")
	}
	hello := first.GetHello()
	if hello == nil || first.GetMessageId() != 1 {
		return status.Error(codes.InvalidArgument, "first frame must be hello")
	}
	slot := service.slotName(hello.GetSlot())
	if slot == "" {
		return status.Error(codes.InvalidArgument, "slot must be plinth or lintel")
	}
	if slot == qruntime.SlotLintel && hello.GetBrowserCapacitySlots() == 0 {
		return status.Error(codes.InvalidArgument, "lintel browser capacity must be positive")
	}
	decision, err := service.Slots.Adjudicate(ctx, bearer, slot, hello.GetBootId(), hello.GetConnectionEpoch(), hello.GetReleaseVersion(), service.ReleaseVersion, service.CatalogDigest, hello.GetJourneyCatalogDigest())
	if err != nil {
		sharedops.LogEvent("quoin", "error", "runtime.hello_failed", err.Error())
		return status.Error(codes.Internal, "handshake failed")
	}
	if !decision.Accepted {
		reason := runtimev1.HelloRejectReason(runtimev1.HelloRejectReason_value[mapRejectReason(decision.Reason)])
		_ = stream.Send(&runtimev1.ControlEnvelope{
			MessageId:       1,
			ConnectionEpoch: hello.GetConnectionEpoch(),
			BootId:          hello.GetBootId(),
			Msg: &runtimev1.ControlEnvelope_HelloAck{
				HelloAck: &runtimev1.HelloAck{
					Accepted:                 false,
					RejectReason:             reason,
					LastConnectionEpoch:      decision.LastConnectionEpoch,
					ProfileReconcileRequired: decision.ProfileReconcileRequired,
				},
			},
		})
		sharedops.LogEvent("quoin", "info", "runtime.hello_rejected", "slot="+slot+" reason="+decision.Reason)
		return status.Error(codes.Unauthenticated, "handshake rejected")
	}
	var outbound sync.Mutex
	sender := func(envelope any) error {
		outbound.Lock()
		defer outbound.Unlock()
		proto, ok := envelope.(*runtimev1.ControlEnvelope)
		if !ok {
			return fmt.Errorf("unsupported envelope type")
		}
		return stream.Send(proto)
	}
	closing := service.Slots.AttachStreamWithSender(slot, hello.GetBootId(), hello.GetConnectionEpoch(), sender)
	if slot == qruntime.SlotLintel {
		if err := service.Slots.SetBrowserCapacity(slot, hello.GetBootId(), hello.GetConnectionEpoch(), uint64(hello.GetBrowserCapacitySlots())); err != nil {
			return status.Error(codes.Internal, "bind lintel browser capacity")
		}
	}
	// Reserve the HelloAck sequence number before concurrent dispatchers use this stream.
	helloAckID, err := service.Slots.NextMessageID(slot)
	if err != nil {
		return status.Error(codes.Internal, "allocate hello acknowledgement id")
	}
	defer func() {
		service.Slots.DetachStream(slot, hello.GetBootId(), hello.GetConnectionEpoch())
		if slot == qruntime.SlotPlinth {
			// The stream ended: Cancelling attempts of this binding converge
			// (RUNTIME-CANCEL-003); Running attempts keep their lease window
			// for a same-boot reconnect (RUNTIME-TASK-005).
			service.onPlinthStreamEnded(context.Background(), hello.GetBootId(), hello.GetConnectionEpoch())
		}
	}()
	ack := &runtimev1.ControlEnvelope{
		MessageId:       helloAckID,
		ConnectionEpoch: hello.GetConnectionEpoch(),
		BootId:          hello.GetBootId(),
		Msg: &runtimev1.ControlEnvelope_HelloAck{
			HelloAck: &runtimev1.HelloAck{
				Accepted:                 true,
				LastConnectionEpoch:      decision.LastConnectionEpoch,
				ProfileReconcileRequired: decision.ProfileReconcileRequired,
			},
		},
	}
	if err := stream.Send(ack); err != nil {
		return err
	}
	if slot == qruntime.SlotLintel {
		// Hello is a physical-operation snapshot, not merely a capacity hint. The
		// handshake acknowledgement must be the first server frame, so reconcile
		// only after it is on the stream.
		service.reconcileLintelPhysicalOperations(ctx, hello.GetBootId(), hello.GetConnectionEpoch(), hello.GetActiveBrowserOperations())
	}
	sharedops.LogEvent("quoin", "info", "runtime.connected", "slot="+slot)
	// Lintel browser work is admission-gated below. A new boot must first
	// reconcile the complete profile inventory before any replayed action or
	// cancellation can touch Chromium.
	// A different Lintel boot cannot own processes started by its predecessor.
	// Record terminal interruption first, but retain the cleanup fence; after
	// inventory, dispatchPendingBrowserStops asks this new boot to remove its
	// startup-cleaned staging and acknowledges that fence.
	if slot == qruntime.SlotLintel && decision.ProfileReconcileRequired {
		if service.Browsers == nil {
			return status.Error(codes.Internal, "browser authority unavailable")
		}
		// Runtime.Service keeps epoch history only in Quoin memory. A reconnect
		// from an already-running Lintel boot with epoch > 1 and no remembered
		// predecessor therefore proves that Quoin restarted. Browser operations
		// are not recoverable across that control-plane restart, even though the
		// Lintel boot ID is unchanged.
		if decision.LastConnectionEpoch == 0 && hello.GetConnectionEpoch() > 1 {
			if _, interruptErr := service.Browsers.InterruptForQuoinRestart(ctx, hello.GetBootId()); interruptErr != nil {
				return status.Error(codes.Internal, "interrupt browser operations after Quoin restart")
			}
		}
		if _, interruptErr := service.Browsers.InterruptOldBootOperations(ctx, hello.GetBootId(), hello.GetConnectionEpoch()); interruptErr != nil {
			return status.Error(codes.Internal, "interrupt prior browser boot")
		}
		// New-boot interruption commits terminal Tool Call rows while Plinth may
		// remain connected. Replay from those durable rows immediately; otherwise
		// the parent model loop can wait forever for a child result that was only
		// written during Lintel's profile-reconcile path.
		go service.replayUndeliveredBrowserToolResults(context.Background())
	}
	// Inventory establishes Lintel's reconciliation fence before any Browser
	// Operation dispatch is allowed on this new stream.
	inventoryID := ""
	if slot == qruntime.SlotLintel && decision.ProfileReconcileRequired {
		items, inventoryErr := service.Browsers.ExpectedInventory(ctx)
		if inventoryErr != nil {
			return status.Error(codes.Internal, "read browser profile inventory")
		}
		profiles := make([]*runtimev1.ExpectedBrowserProfile, 0, len(items))
		for _, item := range items {
			digest, decodeErr := hex.DecodeString(item.ManifestDigest)
			if decodeErr != nil {
				return status.Error(codes.Internal, "invalid stored browser manifest digest")
			}
			profiles = append(profiles, &runtimev1.ExpectedBrowserProfile{IdentityId: item.IdentityID, ProfileGenerationId: item.ProfileGenerationID, Generation: uint64(item.Generation), ChromiumRevision: item.ChromiumRevision, ProfileManifestDigest: digest})
		}
		inventoryID = "inv-" + hello.GetBootId() + "-" + fmt.Sprint(hello.GetConnectionEpoch())
		if err := service.sendEnvelope(slot, &runtimev1.ControlEnvelope{
			ConnectionEpoch: hello.GetConnectionEpoch(), BootId: hello.GetBootId(),
			Msg: &runtimev1.ControlEnvelope_ProfileInventoryRequest{ProfileInventoryRequest: &runtimev1.ProfileInventoryRequest{InventoryId: inventoryID, Profiles: profiles}},
		}); err != nil {
			return err
		}
	}
	if slot == qruntime.SlotPlinth {
		// Reconnect adjudication first (new-boot interrupts, same-boot
		// reconcile), then queued attempts created while the slot was
		// disconnected bind to this live stream and dispatch immediately.
		go service.onPlinthAttached(context.Background(), hello.GetBootId(), hello.GetConnectionEpoch())
		go service.dispatchAllCancellingInspections(context.Background())
		go service.dispatchAllCancellingKnowledgeExtractions(context.Background())
		go service.dispatchQueuedProbes(context.Background())
		go service.dispatchQueuedVerificationAttempts(context.Background())
		go service.dispatchQueuedResourceRefreshAttempts(context.Background())
		go service.dispatchQueuedAnalyses(context.Background())
		go service.dispatchQueuedKnowledgeExtractions(context.Background())
		go service.dispatchQueuedEmbeddings(context.Background())
		go service.dispatchQueuedInvestigations(context.Background())
		go service.dispatchQueuedInspections(context.Background())
	}
	if slot == qruntime.SlotLintel && !decision.ProfileReconcileRequired {
		go service.reconcileJourneyVerificationChildren(context.Background())
		go service.dispatchAllCancellingBrowserExplorations(context.Background())
		go service.replayRunningBrowserExplorationChildren(context.Background())
		go service.dispatchPendingBrowserStops(context.Background())
		go service.dispatchQueuedBrowserOperations(context.Background())
	}
	lastInboundMessageID := first.GetMessageId()
	for {
		var envelope *runtimev1.ControlEnvelope
		// A replace/revoke signal must end the RPC even while Recv blocks:
		// race the receive against the closing channel so gRPC tears the
		// stream down on both ends (RUNTIME-CTRL-001/007).
		received := make(chan error, 1)
		go func() {
			frame, recvErr := stream.Recv()
			envelope = frame
			received <- recvErr
		}()
		select {
		case <-closing:
			_ = stream.Context().Err()
			return status.Error(codes.Canceled, "control stream replaced or revoked")
		case err := <-received:
			if err != nil {
				// Stream ended (Runtime shutdown/replace/revoke): the
				// connection projection is dropped by the deferred DetachStream.
				return nil
			}
		}
		_ = envelope
		if err != nil {
			// Stream ended (Runtime shutdown/replace/revoke): the connection
			// projection is dropped by the deferred DetachStream.
			return nil
		}
		if envelope.GetConnectionEpoch() != hello.GetConnectionEpoch() || envelope.GetBootId() != hello.GetBootId() {
			// Stale-stream fence: drop silently, audit only (RUNTIME-CTRL-009).
			sharedops.LogEvent("quoin", "info", "runtime.envelope_dropped", "slot="+slot)
			continue
		}
		if envelope.GetMessageId() <= lastInboundMessageID {
			// Retries and stale duplicate frames never re-execute side effects.
			sharedops.LogEvent("quoin", "info", "runtime.envelope_duplicate", "slot="+slot)
			continue
		}
		lastInboundMessageID = envelope.GetMessageId()
		switch payload := envelope.Msg.(type) {
		case *runtimev1.ControlEnvelope_Heartbeat:
			service.Slots.Touch(slot)
			if slot == qruntime.SlotLintel {
				service.reconcileLintelPhysicalOperations(ctx, hello.GetBootId(), hello.GetConnectionEpoch(), payload.Heartbeat.GetActiveBrowserOperations())
				// A post-commit cancellation send can fail while this stream stays
				// alive; heartbeat is the durable replay cadence for its fence.
				go service.reconcileJourneyVerificationChildren(context.Background())
			}
			if slot == qruntime.SlotPlinth {
				// Heartbeats renew the live stream's attempt leases
				// (RUNTIME-TASK-007; runtime_slots stays memory-only,
				// RUNTIME-CTRL-005).
				service.renewPlinthLeases(ctx, hello.GetBootId())
				// Lease renewal must not mask a failed initial cancellation send.
				go service.dispatchAllCancellingInspections(context.Background())
				go service.dispatchAllCancellingKnowledgeExtractions(context.Background())
			}
		case *runtimev1.ControlEnvelope_ReconcileReport:
			if slot == qruntime.SlotPlinth {
				service.deliverReconcileReport(slot, payload.ReconcileReport.GetRunningAttemptIds())
			}
		case *runtimev1.ControlEnvelope_AttemptAccept:
			service.handleAttemptAcceptRouted(ctx, envelope, payload.AttemptAccept)
		case *runtimev1.ControlEnvelope_AttemptReject:
			service.handleAttemptRejectRouted(ctx, envelope, payload.AttemptReject)
		case *runtimev1.ControlEnvelope_ResultProposal:
			service.handleResultProposalRouted(ctx, envelope, payload.ResultProposal)
		case *runtimev1.ControlEnvelope_CancelAck:
			service.handleCancelAckRouted(ctx, slot, payload.CancelAck)
		case *runtimev1.ControlEnvelope_BeginModelCall:
			service.handleBeginModelCallRouted(ctx, envelope, payload.BeginModelCall)
		case *runtimev1.ControlEnvelope_CompleteModelCall:
			service.handleCompleteModelCallRouted(ctx, envelope, payload.CompleteModelCall)
		case *runtimev1.ControlEnvelope_BeginToolCall:
			service.handleBeginToolCallRouted(ctx, envelope, payload.BeginToolCall)
		case *runtimev1.ControlEnvelope_CompleteToolCall:
			service.handleCompleteToolCallRouted(ctx, envelope, payload.CompleteToolCall)
		case *runtimev1.ControlEnvelope_RequestBrowserSubExecution:
			if slot == qruntime.SlotPlinth {
				service.handleBrowserSubExecution(ctx, envelope, payload.RequestBrowserSubExecution)
			}
		case *runtimev1.ControlEnvelope_BrowserExplorationActionResult:
			if slot == qruntime.SlotLintel {
				service.handleBrowserExplorationActionResult(ctx, envelope, payload.BrowserExplorationActionResult)
			}
		case *runtimev1.ControlEnvelope_BrowserExplorationTerminalClaim:
			if slot == qruntime.SlotLintel {
				service.handleBrowserExplorationTerminalClaim(ctx, envelope, payload.BrowserExplorationTerminalClaim)
			}
		case *runtimev1.ControlEnvelope_CancelBrowserExplorationActionAck:
			// The action result, not this receipt, is the durable cancellation
			// outcome. Receiving the typed Ack only proves Lintel accepted the fence.
			if slot == qruntime.SlotLintel {
				sharedops.LogEvent("quoin", "info", "browser.cancel_accepted", fmt.Sprintf("child=%d", payload.CancelBrowserExplorationActionAck.GetChildAttemptId()))
			}
		case *runtimev1.ControlEnvelope_ToolResultDeliveryAck:
			if slot == qruntime.SlotPlinth {
				service.handleBrowserToolResultDeliveryAck(envelope, payload.ToolResultDeliveryAck)
			}
		case *runtimev1.ControlEnvelope_ModelTokenDelta:
			// Transient visible deltas fan out to the investigation stream
			// feeds only (RUNTIME-AGENT-004); the analysis slice has no
			// display stream and drops them.
			if service.InvestigationRuntime != nil {
				delta := payload.ModelTokenDelta
				attemptType, lookupErr := service.attemptTypeOf(ctx, delta.GetAttemptId())
				if lookupErr == nil && attemptType == "investigation" {
					service.InvestigationRuntime.HandleDelta(delta.GetAttemptId(), delta.GetModelCallId(), delta.GetDeltaSeq(), delta.GetText())
				}
			}
		case *runtimev1.ControlEnvelope_StartBrowserOperationAck:
			if slot == qruntime.SlotLintel {
				service.handleBrowserStartAck(ctx, envelope, payload.StartBrowserOperationAck)
			}
		case *runtimev1.ControlEnvelope_CompleteBrowserOperation:
			if slot == qruntime.SlotLintel {
				service.handleBrowserCompletion(ctx, envelope, payload.CompleteBrowserOperation)
			}
		case *runtimev1.ControlEnvelope_PublishBrowserProfileResult:
			if slot == qruntime.SlotLintel {
				service.handleBrowserPublishResult(ctx, envelope, payload.PublishBrowserProfileResult)
			}
		case *runtimev1.ControlEnvelope_StopBrowserOperationAck:
			if slot == qruntime.SlotLintel {
				service.handleBrowserStopAck(ctx, envelope, payload.StopBrowserOperationAck)
			}
		case *runtimev1.ControlEnvelope_ProfileInventoryReport:
			if slot != qruntime.SlotLintel || inventoryID == "" || payload.ProfileInventoryReport.GetInventoryId() != inventoryID || !payload.ProfileInventoryReport.GetComplete() {
				sharedops.LogEvent("quoin", "info", "runtime.inventory_incomplete", "slot="+slot)
				continue
			}
			observations := make(map[int64]browser.InventoryObservation, len(payload.ProfileInventoryReport.GetProfiles()))
			for _, observed := range payload.ProfileInventoryReport.GetProfiles() {
				if observed.GetProfileGenerationId() < 1 || observed.GetIdentityId() < 1 {
					observations[0] = browser.InventoryObservation{Status: "invalid"}
					continue
				}
				if _, exists := observations[observed.GetProfileGenerationId()]; exists {
					// A duplicate cannot be a complete set; retain it as an invalid
					// entry so ReconcileInventory rejects the entire report.
					observations[0] = browser.InventoryObservation{Status: "invalid"}
					continue
				}
				statusName := "manifest_invalid"
				switch observed.GetStatus() {
				case runtimev1.ProfileInventoryStatus_PROFILE_INVENTORY_STATUS_COMPATIBLE:
					statusName = "compatible"
				case runtimev1.ProfileInventoryStatus_PROFILE_INVENTORY_STATUS_MISSING:
					statusName = "missing"
				case runtimev1.ProfileInventoryStatus_PROFILE_INVENTORY_STATUS_CHROMIUM_REVISION_MISMATCH:
					statusName = "chromium_revision_mismatch"
				}
				observations[observed.GetProfileGenerationId()] = browser.InventoryObservation{IdentityID: observed.GetIdentityId(), Status: statusName, ChromiumRevision: observed.GetObservedChromiumRevision(), ManifestDigestHex: hex.EncodeToString(observed.GetObservedManifestDigest())}
			}
			if reconcileErr := service.Slots.WithCurrent(qruntime.SlotLintel, envelope.GetBootId(), envelope.GetConnectionEpoch(), func() error {
				return service.Browsers.ReconcileInventory(ctx, envelope.GetBootId(), envelope.GetConnectionEpoch(), observations)
			}); reconcileErr != nil {
				sharedops.LogEvent("quoin", "info", "runtime.inventory_rejected", "slot="+slot)
				continue
			}
			sharedops.LogEvent("quoin", "info", "runtime.inventory_complete", "slot="+slot)
			// This is the sole readiness edge for a reconciled boot. Only after
			// the complete inventory transaction has committed may replayed work
			// reach Lintel.
			go service.reconcileJourneyVerificationChildren(context.Background())
			go service.dispatchAllCancellingBrowserExplorations(context.Background())
			go service.replayRunningBrowserExplorationChildren(context.Background())
			go service.dispatchPendingBrowserStops(context.Background())
			go service.dispatchQueuedBrowserOperations(context.Background())
		default:
			// Task dispatch/results arrive with later tickets; unknown
			// frames are ignored (fail-closed: no partial task authority).
			sharedops.LogEvent("quoin", "info", "runtime.envelope_ignored", "slot="+slot)
		}
	}
}

func mapRejectReason(reason string) string {
	switch reason {
	case "TOKEN_INVALID":
		return "HELLO_REJECT_REASON_TOKEN_INVALID"
	case "SLOT_REVOKED":
		return "HELLO_REJECT_REASON_SLOT_REVOKED"
	case "VERSION_MISMATCH":
		return "HELLO_REJECT_REASON_VERSION_MISMATCH"
	case "EPOCH_STALE":
		return "HELLO_REJECT_REASON_EPOCH_STALE"
	case "CATALOG_MISMATCH":
		return "HELLO_REJECT_REASON_CATALOG_MISMATCH"
	default:
		return "HELLO_REJECT_REASON_UNSPECIFIED"
	}
}

// NewRuntimeControl builds the control-stream service; keep the value so
// the HTTP surface can reuse its task dispatcher.
func NewRuntimeControl(slots *qruntime.Service, releaseVersion, catalogDigest string, taskConnections *connections.Service) *RuntimeService {
	return &RuntimeService{Slots: slots, ReleaseVersion: releaseVersion, CatalogDigest: catalogDigest, Connections: taskConnections}
}

// dispatchQueuedInvestigations binds and dispatches every Queued
// investigation attempt after a Plinth stream attaches (created while
// disconnected).
func (service *RuntimeService) dispatchQueuedInvestigations(ctx context.Context) {
	if service.InvestigationRuntime == nil {
		return
	}
	service.InvestigationRuntime.DispatchQueued(ctx)
}

// RegisterRuntimeControl mounts the service on an existing gRPC server.
func RegisterRuntimeControl(server *grpc.Server, service *RuntimeService) {
	runtimev1.RegisterRuntimeControlServer(server, service)
}
