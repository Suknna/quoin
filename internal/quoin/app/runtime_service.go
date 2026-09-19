package app

// RuntimeControl gRPC service (T06): Register (one-time token bootstrap) and
// Connect (Hello handshake + heartbeat maintenance). The service runs on the
// Runtime TLS listener (:8443) next to SteleRelay.

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	appinvestigation "github.com/Suknna/quoin/internal/quoin/app/investigation"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/investigation"
	"github.com/Suknna/quoin/internal/quoin/knowledge"
	"github.com/Suknna/quoin/internal/quoin/observation"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RuntimeService adapts the runtime.Service authority to the gRPC surface.
type RuntimeService struct {
	writer *sql.DB
	runtimev1.UnimplementedRuntimeControlServer
	Slots *qruntime.Service
	// PlatformFaults projects runtime connection transitions into the independent
	// unified-alert source; it never creates upstream Delivery or Occurrence data.
	PlatformFaults *alerts.PlatformFaultReporter
	// platformFaultProjectionMu serializes connection-view reads with durable
	// fault projection. Each caller still reads SlotService's fenced authority,
	// but serialization guarantees projectors commit in that observation order.
	platformFaultProjectionMu sync.Mutex
	// ReleaseVersion records Quoin's own build provenance; peer releases
	// come from the live Hello. It never participates in RPC admission.
	ReleaseVersion string
	// Connections owns connection_probe attempts and credential grants
	// (T07); nil keeps the T06 handshake-only behaviour for tests that do
	// not exercise the task slice.
	Connections *connections.Service
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
	// MaintenanceBlocking gates scheduling admission while any maintenance
	// revision is active: due boundaries then record their durable
	// runtime_unavailable outcome instead of creating dispatchable work
	// (OPS-UPGRADE-003). Nil admits normally.
	MaintenanceBlocking func(ctx context.Context) bool
	// Inspections owns manual Inspection Runs: run_check PromQL children and
	// inspection_analysis report attempts (T24); nil keeps the handshake-only
	// behaviour for tests that do not exercise it.
	Inspections *inspection.Service
	// Observations owns source observation children (ADR-0004); nil keeps the
	// handshake-only behaviour for tests that do not exercise it.
	Observations *observation.Service
	// reconcile carries the pending same-boot ReconcileReport waiter
	// (T12, RUNTIME-TASK-005).
	reconcile reconcileState
	// sendEnvelopeForTest captures outbound control replies in package tests.
	// Production leaves it nil and always routes through the live slot.
	sendEnvelopeForTest func(slot string, envelope *runtimev1.ControlEnvelope) error
}

// slotName resolves the proto slot enum onto the runtime authority. Only
// PLINTH maps; anything else is rejected as an unsupported slot before any
// further processing.
func (service *RuntimeService) slotName(slot runtimev1.RuntimeSlot) string {
	switch slot {
	case runtimev1.RuntimeSlot_RUNTIME_SLOT_PLINTH:
		return qruntime.SlotPlinth
	default:
		return ""
	}
}

// Register is retired with the registration era (ADR-0009): component
// identity is the mTLS client certificate, so there is nothing to exchange.

// Connect requires the verified mTLS client identity (CN=plinth), adjudicates
// the Hello handshake and keeps the transient connection projection alive.
// T06 implements the handshake/readiness slice of the control stream; task
// dispatch arrives with later tickets, so after Hello the loop only maintains
// heartbeats until the stream ends.
func (service *RuntimeService) Connect(stream runtimev1.RuntimeControl_ConnectServer) error {
	ctx := stream.Context()
	if !requireComponentIdentity(ctx, qruntime.SlotPlinth) {
		return status.Error(codes.Unauthenticated, "plinth client identity required")
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
		return status.Error(codes.InvalidArgument, "unsupported slot")
	}
	decision, err := service.Slots.Adjudicate(ctx, slot, hello.GetBootId(), hello.GetConnectionEpoch(), hello.GetContractFingerprint(), contract.ProtoAuthorityFingerprint)
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
					Accepted:            false,
					RejectReason:        reason,
					LastConnectionEpoch: decision.LastConnectionEpoch,
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
	closing := service.Slots.AttachStreamWithSenderVersion(slot, hello.GetBootId(), hello.GetConnectionEpoch(), hello.GetReleaseVersion(), sender)
	// Another concurrently admitted Hello may attach first. Its newer epoch
	// remains authoritative; this stale stream must terminate before it can
	// project connection state or send an accepted acknowledgement.
	if closing == nil {
		// Attachment is the second, mutex-protected epoch admission point. Send
		// the normal rejected HelloAck so a concurrently delayed reconnect keeps
		// the established reconnect protocol rather than observing a bare EOF.
		stale, staleErr := service.Slots.Adjudicate(ctx, slot, hello.GetBootId(), hello.GetConnectionEpoch(), hello.GetContractFingerprint(), contract.ProtoAuthorityFingerprint)
		if staleErr != nil {
			return status.Error(codes.Internal, "handshake failed")
		}
		_ = stream.Send(&runtimev1.ControlEnvelope{
			MessageId: 1, ConnectionEpoch: hello.GetConnectionEpoch(), BootId: hello.GetBootId(),
			Msg: &runtimev1.ControlEnvelope_HelloAck{HelloAck: &runtimev1.HelloAck{
				Accepted: false, RejectReason: runtimev1.HelloRejectReason(runtimev1.HelloRejectReason_value[mapRejectReason(stale.Reason)]),
				LastConnectionEpoch: stale.LastConnectionEpoch,
			}},
		})
		return status.Error(codes.Unauthenticated, "handshake rejected")
	}
	// Every return after attachment must release the stream's transient slot
	// ownership. In particular, a projection/capacity/ack setup failure must
	// not strand a phantom connected Runtime.
	defer func() {
		service.Slots.DetachStream(slot, hello.GetBootId(), hello.GetConnectionEpoch())
		if faultErr := service.projectRuntimeConnection(context.Background(), slot); faultErr != nil {
			sharedops.LogEvent("quoin", "error", "platform_fault.project_disconnect_failed", faultErr.Error())
		}
		if slot == qruntime.SlotPlinth {
			// The stream ended: Cancelling attempts of this binding converge
			// (RUNTIME-CANCEL-003); Running attempts keep their lease window
			// for a same-boot reconnect (RUNTIME-TASK-005).
			service.onPlinthStreamEnded(context.Background(), hello.GetBootId(), hello.GetConnectionEpoch())
		}
	}()
	// A successful Hello is the existing authoritative connection fact. The
	// independent projector resolves only its matching platform lifecycle.
	if faultErr := service.projectRuntimeConnection(ctx, slot); faultErr != nil {
		return status.Error(codes.Internal, "project runtime connection")
	}
	// Reserve the HelloAck sequence number before concurrent dispatchers use this stream.
	helloAckID, err := service.Slots.NextMessageID(slot)
	if err != nil {
		return status.Error(codes.Internal, "allocate hello acknowledgement id")
	}
	ack := &runtimev1.ControlEnvelope{
		MessageId:       helloAckID,
		ConnectionEpoch: hello.GetConnectionEpoch(),
		BootId:          hello.GetBootId(),
		Msg: &runtimev1.ControlEnvelope_HelloAck{
			HelloAck: &runtimev1.HelloAck{
				Accepted:            true,
				LastConnectionEpoch: decision.LastConnectionEpoch,
			},
		},
	}
	if err := stream.Send(ack); err != nil {
		return err
	}
	sharedops.LogEvent("quoin", "info", "runtime.connected", "slot="+slot)
	if slot == qruntime.SlotPlinth {
		// Reconnect adjudication first (new-boot interrupts, same-boot
		// reconcile), then queued attempts created while the slot was
		// disconnected bind to this live stream and dispatch immediately.
		go service.onPlinthAttached(context.Background(), hello.GetBootId(), hello.GetConnectionEpoch())
		go service.dispatchAllCancellingInspections(context.Background())
		go service.dispatchAllCancellingKnowledgeExtractions(context.Background())
		go service.dispatchQueuedProbes(context.Background())
		go service.dispatchQueuedSourceObservationAttempts(context.Background())
		go service.dispatchQueuedAnalyses(context.Background())
		go service.dispatchQueuedKnowledgeExtractions(context.Background())
		go service.dispatchQueuedEmbeddings(context.Background())
		go service.dispatchQueuedInvestigations(context.Background())
		go service.dispatchQueuedInspections(context.Background())
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
		default:
			// Task dispatch/results arrive with later tickets; unknown
			// frames are ignored (fail-closed: no partial task authority).
			sharedops.LogEvent("quoin", "info", "runtime.envelope_ignored", "slot="+slot)
		}
	}
}

func mapRejectReason(reason string) string {
	switch reason {
	case "CONTRACT_MISMATCH":
		return "HELLO_REJECT_REASON_CONTRACT_MISMATCH"
	case "EPOCH_STALE":
		return "HELLO_REJECT_REASON_EPOCH_STALE"
	default:
		return "HELLO_REJECT_REASON_UNSPECIFIED"
	}
}

// projectRuntimeConnection makes the slot service's fenced connection view the
// only lifecycle input. Keeping this lookup beside the gRPC boundary prevents
// handler-local connect/disconnect events from racing a successor stream.
func (service *RuntimeService) projectRuntimeConnection(ctx context.Context, slot string) error {
	if service.PlatformFaults == nil {
		return nil
	}
	service.platformFaultProjectionMu.Lock()
	defer service.platformFaultProjectionMu.Unlock()
	view, err := service.Slots.View(ctx, slot)
	if err != nil {
		return err
	}
	return service.PlatformFaults.ObserveRuntimeConnection(ctx, slot, view.Connected)
}

// NewRuntimeControl builds the control-stream service; keep the value so
// the HTTP surface can reuse its task dispatcher. writer is the composition
// writer the runtime families' not-yet-migrated runner compositions run on;
// reads never go through it (they serve the injected read-only pools).
func NewRuntimeControl(slots *qruntime.Service, releaseVersion string, taskConnections *connections.Service, writer *sql.DB) *RuntimeService {
	return &RuntimeService{Slots: slots, ReleaseVersion: releaseVersion, Connections: taskConnections, writer: writer}
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
