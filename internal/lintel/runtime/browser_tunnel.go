package runtime

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/novnc"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

type browserTunnelBinding struct {
	client  runtimev1.BrowserTunnelClient
	context context.Context
	epoch   uint64
}

// openBrowserTunnel connects Lintel's loopback-only x0vncserver to Quoin's
// authorized relay. BrowserFrameData stays opaque on both sides.
func (channel *Channel) openBrowserTunnel(request *runtimev1.StartBrowserOperation) {
	binding, exists := channel.currentBrowserTunnelBinding()
	if !exists {
		return
	}
	address, exists := channel.browser.VNCAddress(request.GetOperationId())
	if !exists {
		return
	}
	var actor struct {
		ActorUserID    int64 `json:"actorUserId"`
		ActorSessionID int64 `json:"actorSessionId"`
	}
	if json.Unmarshal(request.GetInput().GetCanonicalJson(), &actor) != nil || actor.ActorUserID < 1 || actor.ActorSessionID < 1 {
		return
	}
	ctx, cancel := context.WithCancel(binding.context)
	done := make(chan struct{})
	generation, claimed := channel.claimBrowserTunnelForOperation(request, binding.epoch, cancel, done)
	if !claimed {
		cancel()
		return
	}
	defer func() {
		channel.releaseBrowserTunnel(request.GetOperationId(), generation, done)
		cancel()
	}()
	stream, err := binding.client.Open(ctx)
	if err != nil {
		sharedops.LogEvent("lintel", "warn", "browser_tunnel.open_failed", err.Error())
		return
	}
	if err = stream.Send(&runtimev1.BrowserEnvelope{Msg: &runtimev1.BrowserEnvelope_Open{Open: &runtimev1.BrowserSessionOpen{
		OperationId: request.GetOperationId(), IdentityId: request.GetIdentityId(), Slot: runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL,
		BootId: channel.bootID, ConnectionEpoch: binding.epoch, ActorUserId: actor.ActorUserID, ActorSessionId: actor.ActorSessionID,
		OperationKind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN, AttachmentSeq: generation,
	}}}); err != nil {
		sharedops.LogEvent("lintel", "warn", "browser_tunnel.open_send_failed", err.Error())
		return
	}
	ack, err := stream.Recv()
	if err != nil || ack.GetOpenAck() == nil {
		if err != nil {
			sharedops.LogEvent("lintel", "warn", "browser_tunnel.open_rejected", err.Error())
		}
		return
	}
	if err := novnc.Bridge(ctx, &grpcBrowserTunnel{stream: stream}, address); err != nil && ctx.Err() == nil {
		sharedops.LogEvent("lintel", "warn", "browser_tunnel.closed", err.Error())
	}
	// A WebSocket disconnect closes only this RFB attachment. Keep the
	// operation alive and replace its transient tunnel at a higher sequence so
	// a same-boot grace-period reattach can receive a fresh RFB handshake.
	if ctx.Err() == nil {
		time.AfterFunc(100*time.Millisecond, func() { channel.openBrowserTunnel(request) })
	}
}

// claimBrowserTunnel gives exactly one live or opening attachment ownership of
// an operation. The owner fence stays present through Open/OpenAck so Stop and
// Publish can cancel and join it even before the RFB bridge starts.
// installBrowserTunnelBinding makes a control epoch's gRPC client/context
// immutable for all tunnels it starts. A replacement first cancels and joins
// every prior-generation bridge, so a reconnect cannot mix epochs or leave a
// running operation without a reopened relay.
func (channel *Channel) installBrowserTunnelBinding(binding browserTunnelBinding) {
	// Fence late openers before snapshotting owners: claimBrowserTunnelForOperation
	// verifies this pointer under tunnelMu, so no old-epoch bridge can register
	// after this point and defeat the replacement's reopen.
	channel.tunnelMu.Lock()
	channel.tunnelBinding = nil
	channel.tunnelMu.Unlock()
	channel.cancelAndJoinBrowserTunnels()
	channel.tunnelMu.Lock()
	channel.tunnelBinding = &binding
	channel.tunnelMu.Unlock()
}

func (channel *Channel) removeBrowserTunnelBinding(epoch uint64) {
	channel.tunnelMu.Lock()
	if channel.tunnelBinding == nil || channel.tunnelBinding.epoch != epoch {
		channel.tunnelMu.Unlock()
		return
	}
	channel.tunnelBinding = nil
	channel.tunnelMu.Unlock()
	channel.cancelAndJoinBrowserTunnels()
}

func (channel *Channel) currentBrowserTunnelBinding() (browserTunnelBinding, bool) {
	channel.tunnelMu.Lock()
	defer channel.tunnelMu.Unlock()
	if channel.tunnelBinding == nil {
		return browserTunnelBinding{}, false
	}
	return *channel.tunnelBinding, true
}

func (channel *Channel) cancelAndJoinBrowserTunnels() {
	channel.tunnelMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(channel.tunnelCancels))
	dones := make([]chan struct{}, 0, len(channel.tunnelDones))
	for _, cancel := range channel.tunnelCancels {
		cancels = append(cancels, cancel)
	}
	for _, done := range channel.tunnelDones {
		dones = append(dones, done)
	}
	channel.tunnelMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, done := range dones {
		<-done
	}
}

func (channel *Channel) claimBrowserTunnelForOperation(request *runtimev1.StartBrowserOperation, bindingEpoch uint64, cancel context.CancelFunc, done chan struct{}) (uint64, bool) {
	// Operation terminal transitions use operationMu before inspecting the
	// tunnel fence. Claim under the same order, so Stop/Publish cannot pass an
	// empty fence between an active check and this registration.
	channel.operationMu.Lock()
	defer channel.operationMu.Unlock()
	if channel.started[request.GetOperationId()] != request {
		return 0, false
	}
	channel.tunnelMu.Lock()
	defer channel.tunnelMu.Unlock()
	if channel.tunnelBinding == nil || channel.tunnelBinding.epoch != bindingEpoch {
		return 0, false
	}
	return channel.claimBrowserTunnelLocked(request.GetOperationId(), cancel, done)
}

func (channel *Channel) claimBrowserTunnelLocked(operationID int64, cancel context.CancelFunc, done chan struct{}) (uint64, bool) {
	if channel.tunnelGenerations == nil {
		channel.tunnelGenerations = make(map[int64]uint64)
	}
	if channel.tunnelDones == nil {
		channel.tunnelDones = make(map[int64]chan struct{})
	}
	if channel.tunnelDones[operationID] != nil {
		return 0, false
	}
	channel.tunnelGenerations[operationID]++
	generation := channel.tunnelGenerations[operationID]
	channel.tunnelCancels[operationID] = cancel
	channel.tunnelDones[operationID] = done
	return generation, true
}

func (channel *Channel) releaseBrowserTunnel(operationID int64, generation uint64, done chan struct{}) {
	channel.tunnelMu.Lock()
	if channel.tunnelGenerations[operationID] == generation && channel.tunnelDones[operationID] == done {
		delete(channel.tunnelCancels, operationID)
		delete(channel.tunnelDones, operationID)
	}
	channel.tunnelMu.Unlock()
	close(done)
}

type grpcBrowserTunnel struct {
	stream    runtimev1.BrowserTunnel_OpenClient
	mu        sync.Mutex
	pending   []byte
	closeOnce sync.Once
	closeErr  error
}

func (tunnel *grpcBrowserTunnel) Read(buffer []byte) (int, error) {
	for len(tunnel.pending) == 0 {
		frame, err := tunnel.stream.Recv()
		if err != nil {
			return 0, err
		}
		if data := frame.GetData(); data != nil {
			tunnel.pending = data.GetPayload()
			continue
		}
		if frame.GetClose() != nil {
			return 0, io.EOF
		}
	}
	n := copy(buffer, tunnel.pending)
	tunnel.pending = tunnel.pending[n:]
	return n, nil
}
func (tunnel *grpcBrowserTunnel) Write(data []byte) (int, error) {
	tunnel.mu.Lock()
	defer tunnel.mu.Unlock()
	copy := append([]byte(nil), data...)
	if err := tunnel.stream.Send(&runtimev1.BrowserEnvelope{Msg: &runtimev1.BrowserEnvelope_Data{Data: &runtimev1.BrowserFrameData{Payload: copy}}}); err != nil {
		return 0, err
	}
	return len(data), nil
}
func (tunnel *grpcBrowserTunnel) Close() error {
	tunnel.closeOnce.Do(func() {
		tunnel.mu.Lock()
		defer tunnel.mu.Unlock()
		// Keep stream stable for an in-flight Read. gRPC permits one concurrent
		// Send and Recv, but replacing the pointer while Recv uses it creates an
		// artificial data race at the attachment boundary.
		tunnel.closeErr = tunnel.stream.Send(&runtimev1.BrowserEnvelope{Msg: &runtimev1.BrowserEnvelope_Close{Close: &runtimev1.BrowserSessionClose{Reason: runtimev1.BrowserCloseReason_BROWSER_CLOSE_REASON_OPERATION_TERMINAL}}})
	})
	return tunnel.closeErr
}

var _ io.ReadWriteCloser = (*grpcBrowserTunnel)(nil)

// reopenRunningBrowserTunnels restores the transient RFB relay after a
// same-process control reconnect. Browser Manager retains the operation and
// its private VNC endpoint; Quoin authorizes the higher-epoch attachment.
func (channel *Channel) reopenRunningBrowserTunnels() {
	channel.operationMu.Lock()
	requests := make([]*runtimev1.StartBrowserOperation, 0, len(channel.started))
	for _, request := range channel.started {
		if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN {
			requests = append(requests, request)
		}
	}
	channel.operationMu.Unlock()
	for _, request := range requests {
		go channel.openBrowserTunnel(request)
	}
}
