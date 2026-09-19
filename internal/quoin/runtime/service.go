// Package runtime owns the Quoin-side Runtime connection authority (T06): the
// in-process control-stream projection and Hello handshake adjudication for
// Plinth. Component identity is the deployment CA-signed mTLS client
// certificate (ADR-0009): there is no registration, slot credential state or
// persistent authority left — online connection state (connected/boot/epoch/
// lastSeen) is memory-only (DATA-RUNTIME-001).
package runtime

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	SlotPlinth = "plinth"
	SlotLintel = "lintel"
)

// SlotView is the RuntimeSlot HTTP projection — a pure transient view of the
// in-memory connection state (no persistent slot fields remain).
type SlotView struct {
	Slot            string  `json:"slot"`
	Connected       bool    `json:"connected"`
	BootID          string  `json:"bootId,omitempty"`
	ConnectionEpoch *uint64 `json:"connectionEpoch,omitempty"`
	LastSeenAt      *string `json:"lastSeenAt,omitempty"`
	ReleaseVersion  string  `json:"releaseVersion,omitempty"`
}

var ErrNotFound = errors.New("runtime slot not found")

// connection is the transient control-stream projection (RUNTIME-CTRL-001).
// closing carries the stop signal for the handler loop: cancelling the
// stream context makes gRPC terminate the RPC on both ends.
type connection struct {
	bootID               string
	epoch                uint64
	releaseVersion       string // Informational peer build provenance, never admission.
	updated              time.Time
	closing              chan struct{}
	once                 sync.Once
	sender               StreamSender
	outbound             uint64
	browserCapacitySlots uint32
}

func (connection *connection) close() {
	if connection.closing != nil {
		connection.once.Do(func() { close(connection.closing) })
	}
}

// Service wires the connection authority together.
type Service struct {
	now func() time.Time

	mu    sync.Mutex
	conns map[string]*connection // slot -> active stream
	// bootEpochs remembers the highest accepted epoch per (slot, boot) so
	// stale reconnects are rejected with EPOCH_STALE (RUNTIME-CTRL-004).
	bootEpochs map[string]uint64
}

// NewService builds the connection authority. It holds no database handles:
// the mTLS client identity is established at the TLS handshake and every
// projection this package serves is memory-only.
func NewService() *Service {
	return &Service{now: time.Now, conns: map[string]*connection{}, bootEpochs: map[string]uint64{}}
}

// SetClock is the process-boundary seam for deterministic tests.
func (service *Service) SetClock(now func() time.Time) {
	if now != nil {
		service.now = now
	}
}

func ValidSlot(slot string) bool { return slot == SlotPlinth || slot == SlotLintel }

// View projects the slot's transient connection state.
func (service *Service) View(_ context.Context, slot string) (SlotView, error) {
	if !ValidSlot(slot) {
		return SlotView{}, ErrNotFound
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	view := SlotView{Slot: slot}
	conn, live := service.conns[slot]
	if !live {
		return view, nil
	}
	view.Connected = true
	view.BootID = conn.bootID
	epoch := conn.epoch
	view.ConnectionEpoch = &epoch
	seen := conn.updated.UTC().Format(time.RFC3339Nano)
	view.LastSeenAt = &seen
	view.ReleaseVersion = conn.releaseVersion
	return view, nil
}
