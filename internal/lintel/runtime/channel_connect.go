package runtime

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"google.golang.org/grpc/metadata"
)

// RunConnect keeps the Lintel control stream alive. It accepts runtime
// operations only after the catalog handshake and profile inventory reconcile
// fence complete.
func (channel *Channel) RunConnect(ctx context.Context, readiness *sharedops.Server) error {
	state, err := channel.loadToken()
	if err != nil {
		return fmt.Errorf("尚未注册（读取状态卷失败）: %w", err)
	}
	connection, err := channel.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	client := runtimev1.NewRuntimeControlClient(connection)
	streamCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+state.LongTermToken))
	stream, err := client.Connect(streamCtx)
	if err != nil {
		return err
	}
	epoch := channel.epoch + 1
	channel.epoch = epoch
	// Control frames from an older epoch cannot be dispatched on this stream.
	// Quoin replays durable Cancelling fences after attach, so cancellation
	// tombstones are scoped to one control epoch rather than a whole long boot.
	channel.operationMu.Lock()
	channel.journeyCancelled = make(map[int64]bool)
	channel.journeyCancelDone = make(map[int64]chan struct{})
	channel.operationMu.Unlock()
	atomic.StoreUint64(&channel.outbound, 1) // Hello consumes the first outbound ID.
	hello := &runtimev1.Hello{
		Slot:                    runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL,
		BootId:                  channel.bootID,
		ConnectionEpoch:         epoch,
		ReleaseVersion:          buildinfo.Release,
		JourneyCatalogDigest:    catalog.Digest(),
		JourneyCatalogVersion:   catalog.Version,
		BrowserCapacitySlots:    channel.Config.BrowserSlots,
		ChromiumRevision:        channel.Config.ChromiumRevision,
		ActiveBrowserOperations: channel.activeBrowserOperationIDs(),
	}
	if err := stream.Send(&runtimev1.ControlEnvelope{MessageId: 1, ConnectionEpoch: epoch, BootId: channel.bootID, Msg: &runtimev1.ControlEnvelope_Hello{Hello: hello}}); err != nil {
		return err
	}
	ack, err := stream.Recv()
	if err != nil {
		return err
	}
	helloAck := ack.GetHelloAck()
	if helloAck == nil || !helloAck.GetAccepted() {
		if readiness != nil {
			readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: false, Reason: sharedops.RuntimeUnregistered})
		}
		return fmt.Errorf("握手被拒绝: %s", helloAck.GetRejectReason())
	}
	if readiness != nil {
		readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: !helloAck.GetProfileReconcileRequired(), Reason: sharedops.Ready})
	}
	channel.installBrowserTunnelBinding(browserTunnelBinding{client: runtimev1.NewBrowserTunnelClient(connection), context: streamCtx, epoch: epoch})
	defer channel.removeBrowserTunnelBinding(epoch)
	channel.installArtifactUploadBinding(artifactUploadBinding{client: runtimev1.NewArtifactServiceClient(connection), context: streamCtx, epoch: epoch})
	defer channel.removeArtifactUploadBinding(epoch)
	sharedops.LogEvent("lintel", "info", "runtime.connected", "quoin="+channel.Config.QuoinEndpoint)

	var outbound sync.Mutex
	send := func(envelope *runtimev1.ControlEnvelope) error {
		outbound.Lock()
		defer outbound.Unlock()
		envelope.MessageId = atomic.AddUint64(&channel.outbound, 1)
		if heartbeat := envelope.GetHeartbeat(); heartbeat != nil {
			heartbeat.Seq = envelope.MessageId
		}
		return stream.Send(envelope)
	}
	channel.controlMu.Lock()
	channel.controlSend = send
	channel.controlMu.Unlock()
	defer func() {
		channel.releaseAllStartAckFences()
		channel.controlMu.Lock()
		channel.controlSend = nil
		channel.controlMu.Unlock()
	}()
	channel.resendPendingCompletions()
	channel.resendPendingJourneyResults()
	channel.resendPendingExplorationClaims()
	channel.resendPendingExplorationResults()
	channel.reopenRunningBrowserTunnels()

	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	heartbeatStop, heartbeatDone := make(chan struct{}), make(chan struct{})
	defer func() { close(heartbeatStop); <-heartbeatDone }()
	go func() {
		defer close(heartbeatDone)
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				active := channel.activeBrowserOperationIDs()
				if err := send(&runtimev1.ControlEnvelope{ConnectionEpoch: epoch, BootId: channel.bootID, Msg: &runtimev1.ControlEnvelope_Heartbeat{Heartbeat: &runtimev1.Heartbeat{ActiveBrowserOperations: active, Capacity: &runtimev1.Capacity{Running: uint32(len(active)), Max: channel.Config.BrowserSlots}}}}); err != nil {
					return
				}
			}
		}
	}()

	seenMessageIDs := map[uint64]struct{}{}
	for {
		envelope, err := stream.Recv()
		if err != nil {
			if readiness != nil {
				readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: false, Reason: sharedops.DependencyUnavailable})
			}
			return fmt.Errorf("控制流结束: %w", err)
		}
		if !isCurrentControlEnvelope(envelope, channel.bootID, epoch, seenMessageIDs) {
			continue
		}
		if channel.handleRuntimeAcks(envelope) {
			continue
		}
		if request := envelope.GetProfileInventoryRequest(); request != nil {
			if err := send(channel.inventoryResponse(envelope, request)); err != nil {
				return err
			}
			if readiness != nil {
				readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: true, Reason: sharedops.Ready})
			}
			continue
		}
		if request := envelope.GetStartBrowserOperation(); request != nil {
			if err := channel.handleStartRequest(send, envelope, request); err != nil {
				return err
			}
			continue
		}
		if request := envelope.GetDispatchAttempt(); request != nil {
			channel.handleJourneyDispatch(envelope, request)
			continue
		}
		if request := envelope.GetCancelAttempt(); request != nil {
			channel.handleJourneyCancel(envelope, request)
			continue
		}
		if request := envelope.GetExecuteBrowserExplorationAction(); request != nil {
			channel.handleExplorationAction(envelope, request)
			continue
		}
		if request := envelope.GetCancelBrowserExplorationAction(); request != nil {
			channel.handleExplorationCancellation(envelope, request)
			continue
		}
		if request := envelope.GetPublishBrowserProfile(); request != nil {
			if err := send(channel.publishResponse(envelope, request)); err != nil {
				return err
			}
			continue
		}
		if request := envelope.GetStopBrowserOperation(); request != nil {
			if err := send(channel.stopResponse(envelope, request)); err != nil {
				return err
			}
			continue
		}
	}
}

func (channel *Channel) handleRuntimeAcks(envelope *runtimev1.ControlEnvelope) bool {
	if ack := envelope.GetCompleteBrowserOperationAck(); ack != nil {
		channel.acknowledgeCompletion(ack)
		return true
	}
	if ack := envelope.GetBrowserExplorationActionResultAck(); ack != nil {
		channel.acknowledgeExplorationResult(ack)
		return true
	}
	if ack := envelope.GetBrowserExplorationTerminalClaimAck(); ack != nil {
		channel.acknowledgeExplorationTerminalClaim(ack)
		return true
	}
	if ack := envelope.GetResultAck(); ack != nil {
		channel.acknowledgeJourneyResult(ack)
		return true
	}
	return false
}

func (channel *Channel) handleStartRequest(send func(*runtimev1.ControlEnvelope) error, envelope *runtimev1.ControlEnvelope, request *runtimev1.StartBrowserOperation) error {
	response := channel.startResponse(envelope, request)
	if err := send(response); err != nil {
		return err
	}
	if !response.GetStartBrowserOperationAck().GetAccepted() {
		return nil
	}
	channel.releaseStartAckFence(request.GetOperationId())
	if failedStart := channel.takeStartupFailure(request.GetOperationId()); failedStart != nil {
		go channel.recordStartupFailure(failedStart)
		return nil
	}
	channel.resendPendingCompletions()
	if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN {
		go channel.openBrowserTunnel(request)
	} else if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_AUTHENTICATION_PROBE {
		go channel.completeRevisionProbe(request)
	}
	return nil
}
