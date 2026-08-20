package runtime

// Outbound control-stream sender registry (T07 task slice). The runtime
// package stays transport-agnostic: the app layer registers a sender that
// knows the proto envelope type; SendTo only guards single-live-stream
// replacement and per-direction message-id monotonicity (RUNTIME-CTRL-009).

import (
	"errors"
)

// ErrNotConnected reports that the slot has no live stream to dispatch to.
var ErrNotConnected = errors.New("runtime slot not connected")

// StreamSender forwards one outbound envelope on the live control stream.
// It must be safe to call from any goroutine; a failed send ends that
// stream's authority (the reader loop observes the error and detaches).
type StreamSender func(envelope any) error

// AttachStreamWithSender records the accepted stream together with its
// outbound sender; dispatchers may then use SendTo until the stream ends.
func (service *Service) AttachStreamWithSender(slotName, bootID string, epoch uint64, sender StreamSender) <-chan struct{} {
	service.mu.Lock()
	defer service.mu.Unlock()
	key := slotName + "\x00" + bootID
	if service.bootEpochs[key] < epoch {
		service.bootEpochs[key] = epoch
	}
	if old, live := service.conns[slotName]; live {
		old.close()
	}
	fresh := &connection{bootID: bootID, epoch: epoch, updated: service.now(), closing: make(chan struct{}), sender: sender}
	service.conns[slotName] = fresh
	return fresh.closing
}

// NextMessageID allocates the next outbound message id for the slot's live
// connection; ids are monotonic and unique per direction within one
// slot+boot+epoch connection (RUNTIME-CTRL-009).
func (service *Service) NextMessageID(slotName string) (uint64, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	conn, live := service.conns[slotName]
	if !live {
		return 0, ErrNotConnected
	}
	conn.outbound++
	return conn.outbound, nil
}

// SendTo forwards an envelope through the live stream's sender. Replaced or
// ended streams fail with ErrNotConnected.
func (service *Service) SendTo(slotName string, envelope any) error {
	service.mu.Lock()
	conn, live := service.conns[slotName]
	var sender StreamSender
	if live {
		sender = conn.sender
	}
	service.mu.Unlock()
	if !live || sender == nil {
		return ErrNotConnected
	}
	return sender(envelope)
}

// StreamView describes the live binding a dispatcher must fence on.
type StreamView struct {
	BootID string
	Epoch  uint64
}
