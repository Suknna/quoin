package app

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/browser"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// browserTunnelHub is transient transport state only. Browser operation and
// attachment authorization remain durable authority in browser.Service.
type browserTunnelHub struct {
	mu             sync.Mutex
	tunnels        map[int64]*browserTunnel
	reservations   map[int64]*browserAttachmentReservation
	lastAttachment map[int64]uint64
}

var errBrowserTunnelUnavailable = errors.New("browser tunnel unavailable")

type browserAttachmentReservation struct{}

type browserTunnel struct {
	operationID  int64
	fromLintel   chan []byte
	toLintel     chan []byte
	attached     bool
	closed       chan struct{}
	ownerClosing <-chan struct{}
	once         sync.Once
}

func newBrowserTunnelHub() *browserTunnelHub {
	return &browserTunnelHub{tunnels: map[int64]*browserTunnel{}, reservations: map[int64]*browserAttachmentReservation{}, lastAttachment: map[int64]uint64{}}
}
func (tunnel *browserTunnel) close() { tunnel.once.Do(func() { close(tunnel.closed) }) }

func (hub *browserTunnelHub) register(id int64, attachmentSeq uint64, ownerClosing <-chan struct{}) (*browserTunnel, error) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if existing, exists := hub.tunnels[id]; exists {
		select {
		case <-existing.ownerClosing:
			delete(hub.tunnels, id)
			existing.close()
		default:
			return nil, errors.New("browser tunnel already exists")
		}
	}
	if attachmentSeq == 0 || attachmentSeq <= hub.lastAttachment[id] {
		return nil, errors.New("browser tunnel attachment sequence is stale")
	}
	tunnel := &browserTunnel{operationID: id, fromLintel: make(chan []byte, 16), toLintel: make(chan []byte, 16), closed: make(chan struct{}), ownerClosing: ownerClosing}
	hub.tunnels[id], hub.lastAttachment[id] = tunnel, attachmentSeq
	return tunnel, nil
}
func (hub *browserTunnelHub) remove(id int64, tunnel *browserTunnel) {
	hub.mu.Lock()
	if hub.tunnels[id] == tunnel {
		delete(hub.tunnels, id)
	}
	hub.mu.Unlock()
	tunnel.close()
}
func (hub *browserTunnelHub) reserveAttachment(id int64) (*browserAttachmentReservation, error) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.reservations[id] != nil {
		return nil, errors.New("browser tunnel already attached")
	}
	if tunnel := hub.tunnels[id]; tunnel != nil && tunnel.attached {
		return nil, errors.New("browser tunnel already attached")
	}
	reservation := &browserAttachmentReservation{}
	hub.reservations[id] = reservation
	return reservation, nil
}

func (hub *browserTunnelHub) releaseReservation(id int64, reservation *browserAttachmentReservation) {
	hub.mu.Lock()
	if hub.reservations[id] == reservation {
		delete(hub.reservations, id)
	}
	hub.mu.Unlock()
}

func (hub *browserTunnelHub) attach(id int64, reservation *browserAttachmentReservation) (*browserTunnel, error) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.reservations[id] != reservation {
		return nil, errors.New("browser tunnel attachment reservation is no longer current")
	}
	tunnel := hub.tunnels[id]
	if tunnel == nil {
		return nil, errBrowserTunnelUnavailable
	}
	select {
	case <-tunnel.closed:
		delete(hub.tunnels, id)
		return nil, errBrowserTunnelUnavailable
	case <-tunnel.ownerClosing:
		delete(hub.tunnels, id)
		tunnel.close()
		return nil, errBrowserTunnelUnavailable
	default:
	}
	if tunnel.attached {
		return nil, errors.New("browser tunnel already attached")
	}
	tunnel.attached = true
	delete(hub.reservations, id)
	return tunnel, nil
}

// attachAwait lets a browser reconnect ride out Lintel's short replacement of
// the previous RFB generation after a WebSocket disconnect. It never waits
// behind another WebSocket: that is a deterministic conflict, not a queue.
func (hub *browserTunnelHub) attachAwait(ctx context.Context, id int64, reservation *browserAttachmentReservation) (*browserTunnel, error) {
	return hub.attachAwaitFor(ctx, id, reservation, 5*time.Second)
}

func (hub *browserTunnelHub) attachAwaitFor(ctx context.Context, id int64, reservation *browserAttachmentReservation, timeout time.Duration) (*browserTunnel, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		tunnel, err := hub.attach(id, reservation)
		if err == nil || !errors.Is(err, errBrowserTunnelUnavailable) {
			return tunnel, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, err
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func (hub *browserTunnelHub) detach(tunnel *browserTunnel) {
	hub.mu.Lock()
	tunnel.attached = false
	hub.mu.Unlock()
}

// closeAttachment ends exactly the RFB transport generation the departing
// WebSocket used. Lintel then opens a fresh VNC/RFB connection for a same-boot
// reattach; it must not close a newer attachment generation by operation ID.
func (hub *browserTunnelHub) closeAttachment(tunnel *browserTunnel) {
	hub.mu.Lock()
	if hub.tunnels[tunnel.operationID] == tunnel {
		// Retire this transport generation while holding the map lock. A
		// reattaching WebSocket can therefore only observe no tunnel (and wait
		// for Lintel's replacement), never a closed stale one.
		delete(hub.tunnels, tunnel.operationID)
	}
	hub.mu.Unlock()
	tunnel.close()
}
func (hub *browserTunnelHub) closeOperation(id int64) {
	hub.mu.Lock()
	tunnel := hub.tunnels[id]
	hub.mu.Unlock()
	if tunnel != nil {
		tunnel.close()
	}
}

// BrowserTunnelService accepts Lintel-originated, authenticated RFB relay
// streams. It deliberately does not parse or persist BrowserFrameData bytes.
type BrowserTunnelService struct {
	runtimev1.UnimplementedBrowserTunnelServer
	Slots    *qruntime.Service
	Browsers *browser.Service
	Hub      *browserTunnelHub
	OnClosed func(operationID int64)
}

func (service *BrowserTunnelService) Open(stream runtimev1.BrowserTunnel_OpenServer) error {
	if service.Hub == nil || service.Browsers == nil || !service.Slots.ValidateBearer(stream.Context(), bearerFromContext(stream.Context()), qruntime.SlotLintel) {
		return status.Error(codes.Unauthenticated, "lintel bearer required")
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "browser session open required")
	}
	open := first.GetOpen()
	if open == nil || open.GetSlot() != runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL || open.GetOperationKind() != runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN {
		return status.Error(codes.InvalidArgument, "invalid browser session open")
	}
	var tunnel *browserTunnel
	err = service.Slots.WithCurrentClosing(qruntime.SlotLintel, open.GetBootId(), open.GetConnectionEpoch(), func(ownerClosing <-chan struct{}) error {
		if !service.Browsers.ValidateTunnel(stream.Context(), open.GetOperationId(), open.GetIdentityId(), open.GetActorUserId(), open.GetActorSessionId(), open.GetBootId(), open.GetConnectionEpoch()) {
			return status.Error(codes.PermissionDenied, "browser session is not authorized")
		}
		registered, registerErr := service.Hub.register(open.GetOperationId(), open.GetAttachmentSeq(), ownerClosing)
		if registerErr != nil {
			return status.Error(codes.AlreadyExists, registerErr.Error())
		}
		tunnel = registered
		// The control stream is the sole authority for this data-plane relay.
		// Remove the mapping as soon as its owner is superseded, instead of
		// waiting for an old gRPC handler to observe that closure.
		go func() {
			<-ownerClosing
			service.Hub.remove(open.GetOperationId(), registered)
		}()
		return nil
	})
	if err != nil {
		if errors.Is(err, qruntime.ErrNotConnected) {
			return status.Error(codes.PermissionDenied, "browser session is not authorized")
		}
		return err
	}
	defer func() {
		service.Hub.remove(open.GetOperationId(), tunnel)
		if service.OnClosed != nil {
			service.OnClosed(open.GetOperationId())
		}
	}()
	if err := stream.Send(&runtimev1.BrowserEnvelope{Msg: &runtimev1.BrowserEnvelope_OpenAck{OpenAck: &runtimev1.BrowserSessionOpenAck{SessionId: "operation-" + itoa(open.GetOperationId()), AttachmentSeq: open.GetAttachmentSeq()}}}); err != nil {
		return err
	}
	received := make(chan error, 1)
	go func() {
		for {
			frame, receiveErr := stream.Recv()
			if receiveErr != nil {
				received <- receiveErr
				return
			}
			if data := frame.GetData(); data != nil {
				copy := append([]byte(nil), data.GetPayload()...)
				select {
				case tunnel.fromLintel <- copy:
				case <-tunnel.closed:
					received <- nil
					return
				case <-stream.Context().Done():
					received <- stream.Context().Err()
					return
				}
				continue
			}
			if frame.GetClose() != nil {
				received <- nil
				return
			}
		}
	}()
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case err := <-received:
			if err == io.EOF {
				return nil
			}
			return err
		case <-tunnel.closed:
			return nil
		case bytes := <-tunnel.toLintel:
			if err := stream.Send(&runtimev1.BrowserEnvelope{Msg: &runtimev1.BrowserEnvelope_Data{Data: &runtimev1.BrowserFrameData{Payload: bytes}}}); err != nil {
				return err
			}
		}
	}
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	out := make([]byte, 0, 20)
	for value > 0 {
		out = append([]byte{byte('0' + value%10)}, out...)
		value /= 10
	}
	return string(out)
}
