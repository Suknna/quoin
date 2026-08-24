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

// openBrowserTunnel connects Lintel's loopback-only x0vncserver to Quoin's
// authorized relay. BrowserFrameData stays opaque on both sides.
func (channel *Channel) openBrowserTunnel(request *runtimev1.StartBrowserOperation) {
	if channel.tunnelClient == nil || channel.tunnelContext == nil {
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
	ctx, cancel := context.WithCancel(channel.tunnelContext)
	channel.tunnelMu.Lock()
	if channel.tunnelGenerations == nil {
		channel.tunnelGenerations = make(map[int64]uint64)
	}
	channel.tunnelGenerations[request.GetOperationId()]++
	generation := channel.tunnelGenerations[request.GetOperationId()]
	channel.tunnelCancels[request.GetOperationId()] = cancel
	channel.tunnelMu.Unlock()
	defer func() {
		channel.tunnelMu.Lock()
		if channel.tunnelGenerations[request.GetOperationId()] == generation {
			delete(channel.tunnelCancels, request.GetOperationId())
		}
		channel.tunnelMu.Unlock()
		cancel()
	}()
	stream, err := channel.tunnelClient.Open(ctx)
	if err != nil {
		sharedops.LogEvent("lintel", "warn", "browser_tunnel.open_failed", err.Error())
		return
	}
	if err = stream.Send(&runtimev1.BrowserEnvelope{Msg: &runtimev1.BrowserEnvelope_Open{Open: &runtimev1.BrowserSessionOpen{
		OperationId: request.GetOperationId(), IdentityId: request.GetIdentityId(), Slot: runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL,
		BootId: channel.bootID, ConnectionEpoch: channel.epoch, ActorUserId: actor.ActorUserID, ActorSessionId: actor.ActorSessionID,
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

type grpcBrowserTunnel struct {
	stream  runtimev1.BrowserTunnel_OpenClient
	mu      sync.Mutex
	pending []byte
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
	tunnel.mu.Lock()
	defer tunnel.mu.Unlock()
	if tunnel.stream == nil {
		return nil
	}
	err := tunnel.stream.Send(&runtimev1.BrowserEnvelope{Msg: &runtimev1.BrowserEnvelope_Close{Close: &runtimev1.BrowserSessionClose{Reason: runtimev1.BrowserCloseReason_BROWSER_CLOSE_REASON_OPERATION_TERMINAL}}})
	tunnel.stream = nil
	return err
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
